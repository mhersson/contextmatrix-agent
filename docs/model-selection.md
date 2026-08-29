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

## Self-learning blacklist

A model that proves harness-incapable mid-run is reported back
(`report_incapable_model`), excluded, and a replacement re-selected.
