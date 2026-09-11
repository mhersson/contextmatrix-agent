# Model selection

Selection is **priors-only**. During planning, a fixed capable model emits a
complexity tier per subtask - simple / moderate / complex / critical - and
deterministic code maps the tier to a cost-optimal model per role. The LLM
never names a model, and there is no measured-capability gate.

## Inputs

The selector's inputs arrive at run start from CM's `SelectionContext` payload
(`registry.FromSelection`): the candidate set, per-role quality priors,
operator favorites, the blacklist, and the per-role tier ladders
(`tier_bars`) and the price headroom (`price_headroom`). Nothing is embedded
in the binary, and the only host-side selection setting is `default_model`;
the Artificial-Analysis sourcing, normalization, the ladders and the headroom
live on the ContextMatrix side. The selection rule itself is the `selection` package of
`contextmatrix-protocol`, shared with CM so its admin preview and the agent's
real pick are one implementation; `internal/registry` is the agent's adapter
over it.

## Eligibility and choice

A candidate must not be blacklisted, must fit the context window, and must
carry a per-role quality prior clearing the tier bar of that role's ladder.
Every candidate CM ships is tool-capable, so the selector has no tool gate.
Among eligible candidates, an operator favorite wins outright; otherwise the
selector picks the most capable candidate within a price headroom of the
cheapest. The headroom is the operator's value from the payload
(`price_headroom`, set on the ContextMatrix Model selection admin page);
absent or below 1 it is the built-in 1.5x.

## Tier ladders

There is one ladder per role, coder and reviewer, because the two priors come
from different indices with different shapes. The operator edits both on
ContextMatrix's admin page (Model selection); CM sends them with each run as
`SelectionContext.TierBars`, role name to tier name to bar. A role CM sends
nothing for uses the built-in bars: simple 0.65, moderate 0.76, complex 0.82,
critical 0.90. A partial ladder merges over the built-in bars.

The agent validates each role's ladder with the shared rule (known tier
names, every bar in [0,1], non-decreasing from simple to critical). A ladder
that fails falls back to the built-in bars for that role only, the other
role's ladder still applies, the run never fails on it, and the card gets
one log line per failed role:

```text
selector: reviewer tier ladder from the payload did not validate (tier bars: ladder must not decrease: moderate 0.76 is below simple 0.9) - using the built-in bars
```

The ladder is not configurable on the agent host. A `serve.yaml` that still
carries the old `selector_tier_bars` key fails startup with a message naming
the admin page.

## Vendor diversity on multi-seat picks

Review panels, mob discussions, and Best-of-N add a soft vendor-diversity
preference: each seat first considers only vendors not yet seated, with the
price band re-anchored on that subset - so a diverse seat may cost more than
the vendor-blind pick. When no unseated-vendor candidate qualifies, the seat
is picked vendor-blind. Favorites bypass the preference.

## max_capability

A per-card `max_capability` flag (trigger payload, the `maxCapability` argument of
`registry.FromSelection`) overrides both favorites and the price
band: every pick chooses the most capable candidate in the tier regardless of
price. It keeps the tier bar, blacklist, in-run exclude set, window fit, and
vendor-diversity preference intact; equal quality still tie-breaks to the
cheaper model.

## Fallback and pins

When no candidate survives - nothing clears the tier bar, the pool is empty,
or no `SelectionContext` catalog arrives - the selector returns the capable
default, resolved with precedence: payload (the trigger's `default_model`)
first, then the serve-config default (`CMX_DEFAULT_MODEL`), then the
compiled-in `config.DefaultCapableModel`.

Pins are consulted separately in the orchestrator and always override the
catalog path; the fallback precedence is card pin → payload default →
serve-config default.

## Reading a pick in the logs

Every rung selection writes two `slog` INFO lines with the same identifying
fields (`card_id`, `phase`, `model`, `requested_tier`), so a pick and its
field correlate in the log. The existing `selector: pick` line reports the
winner; the new `selector: pool` line explains why it won.

```text
selector: pick card_id=... phase=coder model=high/one \
  requested_tier=complex met_tier=complex bar=0.82 prior=0.85 has_prior=true source=auto
selector: pool card_id=... phase=coder model=high/one \
  requested_tier=complex rung=complex rung_bar=0.82 role=coder \
  pool_top/one="prior=0.93 price=1.5e-05 outcome=out-of-band" \
  pool_high/one="prior=0.85 price=3e-06 outcome=selected" \
  pool_high/two="prior=0.83 price=3.3e-06 outcome=in-band" \
  filtered_prior-below-bar="mid/one,mid/two,low/one"
```

The pool line lists every candidate that reached the rung the pick was made
on (not the tier asked for - a clamped pick describes the rung it clamped to)
with its prior, per-token price, and outcome:

- `selected` - the pick.
- `in-band` - inside the price band but lower quality (or a quality tie lost
  to a cheaper model).
- `out-of-band` - priced above the cheapest candidate times the headroom.
  With `max_capability` the band is unbounded and nothing is out of band.

Catalog models that never reached the pool are aggregated by reason
(`filtered_<reason>="slug1,slug2"`), in catalog order: `prior-below-bar`,
`no-prior-for-role`, `excluded`, `blacklisted`,
`vendor-excluded`, `window-too-small`. A growing in-run exclude set shows up
as one growing `filtered_excluded` entry, so a panel seat's pool reflects the
seats already seated. Exactly one pool line per pick; a pin or an off-ladder
capable-default pick consulted no rung and produces no pool line.

The line is emitted alongside every pick that went through the rung ladder:
coder, fix coder, judge, mob discussion seats, mob moderator, plan and review
decision floors, review panel seats, Best-of-N candidates, and verify-propose.
The one ladder consult whose pool is not logged is the decision floor's
below-bar proposal: the operator's configured model keeps the seat, so logging
the discarded proposal's pool beside its pick line would misattribute the pool
to a model that never ran.

## Off-ladder phases on the transcript

`model_selected` events on the run transcript (the JSON-lines file a local run
writes with `--transcript`, and the container stdout the host captures per
card) record, per phase, which model ran. Ladder phases reach the transcript
through `noteShortfall` alongside their `selector: pick` and `selector: pool`
lines. Three phases resolve the orchestrator model by precedence alone - a
catalog-resolvable card pin, else the payload `default_model`, else the
serve-config default - and never consult a rung: the document phase
(`document`), the integrate PR-body call (`integrate`), and the Copilot triage
call in `pr_gates`. They record their choice with the same event shape:

```json
{"seq":48,"kind":"model_selected","time":"2026-02-14T10:00:00Z","data":{"phase":"integrate","subtask":"","model":"anthropic/claude-sonnet","source":"capable-default","tier_requested":"complex","met_tier":""}}
```

`source` is `pinned` when a catalog-resolvable card pin chose the model, and
`capable-default` otherwise (payload default or serve-config fallback -
`PickSource` does not separate the two). `tier_requested` is `complex`, the
shared decision tier; `subtask` is empty for these card-level phases and
`met_tier` is empty because nothing scored the model against a bar. These
picks consult no rung, so they also produce no `selector: pick` or
`selector: pool` line. The event is observability only - resolution behavior
and the pin-fallback warning path are unchanged.
