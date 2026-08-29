# Model selection

Selection is **priors-only**. During planning, a fixed capable model emits a
complexity tier per subtask - simple / moderate / complex / critical - and
deterministic code maps the tier to a cost-optimal model per role. The LLM
never names a model, and there is no measured-capability gate.

## Inputs

The selector's inputs arrive at run start from CM's `SelectionContext` payload
(`registry.FromSelection`): the candidate set, per-role quality priors,
operator favorites, and the blacklist. Nothing is embedded in the binary; the
Artificial-Analysis sourcing, normalization, and tier-bar tuning live on the
ContextMatrix side.

## Eligibility and choice

A candidate must be tool-capable, not blacklisted, fit the context window, and
carry a per-role quality prior clearing the tier bar (`DefaultTierBars`:
simple 0.65, moderate 0.76, complex 0.82, critical 0.90). Among eligible
candidates, an operator favorite wins outright; otherwise the selector picks
the most capable candidate within a price headroom (default 1.5x) of the
cheapest.

## Vendor diversity on multi-seat picks

Review panels, mob discussions, and Best-of-N add a soft vendor-diversity
preference: each seat first considers only vendors not yet seated, with the
price band re-anchored on that subset - so a diverse seat may cost more than
the vendor-blind pick. When no unseated-vendor candidate qualifies, the seat
is picked vendor-blind. Favorites bypass the preference.

## max_capability

A per-card `max_capability` flag (trigger payload, exposed as
`registry.Selection.MaxCapability`) overrides both favorites and the price
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
`no-prior-for-role`, `not-tools-capable`, `excluded`, `blacklisted`,
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
