# Model priors refresh

`internal/registry/data/model-priors.json` carries the external quality signal
the model selector pairs with measured capabilities. Each model entry holds a
`coder` and/or `reviewer` score in `[0,1]` plus its `source` and `retrieved`
date. The selector uses a prior as the tier bar for its role; a measured
capability still has to clear the calibrated floor. A missing role (`null`)
means "no prior" — the selector falls back to the measured score, so never emit
a `0` to stand in for an absent signal.

The priors file is **embedded into the binary at build time**. Updating it
requires a rebuild, not just a file edit.

## Field mapping

Priors are derived from the Artificial Analysis v2 models endpoint
(`https://artificialanalysis.ai/api/v2/data/llms/models`):

| Prior role | Source field |
| ---------- | ------------ |
| `coder`    | `data[].evaluations.artificial_analysis_coding_index` |
| `reviewer` | `data[].evaluations.artificial_analysis_intelligence_index` |

Both indices are on a roughly 0–100 scale and are individually nullable. A null
index produces **no** prior for that role.

## Normalization

Each index is normalized **per role** against the maximum observed in the same
response, then clamped to `[0,1]`:

```
coder    = coding_index       / max(coding_index across the response)
reviewer = intelligence_index / max(intelligence_index across the response)
```

The maxima are role-local: the coder denominator is the highest coding index in
the response, the reviewer denominator the highest intelligence index. Because
the scale is relative to the current frontier, re-running after a new frontier
model lands can shift every normalized score down — re-check the tier bars (see
below) when that happens.

## Automated refresh (with an API key)

1. Set the key in the environment (helper-only credential; never committed,
   never passed to workers):

   ```bash
   export CMX_ARTIFICIAL_ANALYSIS_API_KEY=...   # or serve.yaml: artificial_analysis_api_key
   ```

2. Run the proposal command. It writes a **proposal**, never the live file:

   ```bash
   contextmatrix-agent priors-refresh
   # default --out internal/registry/data/model-priors.json.proposed
   # default --gap-threshold 0.85, --date today (UTC)
   ```

   The command fetches indices for every model in
   `internal/eval/fixtures/candidates.txt`, maps each candidate's OpenRouter
   slug to its Artificial Analysis slug via the table in
   `internal/cli/priors_refresh.go`, normalizes, and writes the proposed
   document plus a summary:
   - **priors written** — candidates that got an entry.
   - **unmatched candidates** — candidates with no Artificial Analysis data or
     no slug mapping (left without a prior, never silently dropped).
   - **gap report** — Artificial Analysis models whose normalized coding index
     clears `--gap-threshold` but are absent from `candidates.txt`. These are
     **suggestions only** and are never auto-added.

3. Review the proposal. Eyeball [llm-stats](https://llm-stats.com) (or another
   independent benchmark aggregator) as a manual cross-check that the relative
   ordering is sane.

4. If the gap report flags a model worth tracking, add its OpenRouter slug to
   `internal/eval/fixtures/candidates.txt` and its Artificial Analysis-slug →
   OpenRouter-slug pair to `aaSlugToOpenRouter` in
   `internal/cli/priors_refresh.go`, then re-run. Refresh sessions extend that
   table as the candidate list evolves.

5. If the normalization scale shifted (a new frontier model pushed the maxima
   up), update `meta.tier_bars` in the proposal so the simple/moderate/complex
   bars still line up with the new normalized values.

6. Rename the proposal over the live file, rebuild to re-embed, and commit:

   ```bash
   mv internal/registry/data/model-priors.json.proposed \
      internal/registry/data/model-priors.json
   make build   # re-embeds the updated priors
   git add internal/registry/data/model-priors.json
   git commit
   ```

## Manual refresh (no API key)

When no Artificial Analysis key is available, `priors-refresh` exits with an
error pointing here and makes **no** network call. Transcribe by hand:

1. Open the Artificial Analysis models page and read, for each model in
   `candidates.txt`, its coding index and intelligence index.
2. Find the maximum coding index and the maximum intelligence index across the
   models you are recording (the response-wide maxima).
3. For each candidate, compute `coding_index / max_coding` and
   `intelligence_index / max_intelligence`, clamp to `[0,1]`, and round to two
   decimals. Omit a role whose index is unavailable — do not write `0`.
4. Edit `internal/registry/data/model-priors.json` directly: set each entry's
   `coder`/`reviewer`, `source: "artificialanalysis"`, and `retrieved` to the
   date you read the values. Update `meta.updated` and, if the scale shifted,
   `meta.tier_bars`.
5. Rebuild (`make build`) to re-embed and commit.
