package orchestrator

import (
	"context"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/events"
)

// modelSelectedKind records on the run transcript which model a phase ran on,
// what the selector was asked for, and what the chosen model was worth. The
// operator's capable default and the lowest tier bar routinely resolve to the
// same slug, so the model name alone cannot separate a laddered pick from an
// off-ladder fallback; source is what separates them.
//
// events.Kind is a defined string type, so a repo-local kind needs no harness
// change. The consumer is the durable transcript (the JSON-lines file a local
// run writes, and the container stdout the agent host captures verbatim per
// card), not the live SSE stream: the shared log bridge skips kinds no arm
// claims, and a selection line per panel seat does not belong in an operator's
// live feed.
const modelSelectedKind = "model_selected"

// emitModelSelection writes one selection to the run transcript. subtaskID is
// empty for card-level phases. A nil emitter is a no-op, which is how the
// orchestrator is wired in tests that do not read the transcript.
//
// The field names and values are a wire contract: a transcript consumer parses
// them to answer, per phase, which model ran. Every phase emits where its model
// runs, so a selection line is always a record of use.
func emitModelSelection(emit *events.Emitter, phase, subtaskID string, p registry.Pick) {
	if emit == nil {
		return
	}

	emit.Emit(events.Kind(modelSelectedKind), map[string]any{
		"phase":   phase,
		"subtask": subtaskID,
		"model":   p.Model,
		// Where the model came from: a rung of the ladder, an operator
		// favorite, an operator pin, or the off-ladder capable default.
		"source": p.Source.String(),
		// The bar the phase asked for, which is what makes an unexercised
		// tier visible across runs.
		"tier_requested": string(p.RequestedTier),
		// The strictest bar this model's own prior actually clears, "" when
		// it has no prior. Without it a met request and a silently degraded
		// one are the same line.
		"met_tier": string(p.MetTier),
	})
}

// walkedDown reports that the LADDER produced this pick at a lower rung than
// the phase asked for. It is the condition an outcome report suppresses a
// `failed` row on, and it is deliberately narrower than Pick.BelowBar().
//
// The harm being avoided is a ratchet, and the ratchet needs a rung to empty.
// When the requested rung's pool is dry the selector hands the work to a model
// rated for less; a loss row then lowers the very prior that put that model on
// its OWN rung, so that rung empties too and the next selection walks down
// further. The row would be measuring the selector's shortfall as the model's.
//
// So the question is not "did this pick clear the bar" but "did the ladder
// walk it down", and only a ladder pick can be walked down. The two OFF-LADDER
// sources are excluded by that definition rather than exempted from it:
//
//   - A pin is operator intent. The selector never looked, so there was no
//     walk and no shortfall of the search. (The pins this package synthesizes
//     also carry no measured prior at all, so every one of them reports
//     BelowBar - suppressing on BelowBar alone would silently stop recording
//     outcomes for every pinned run, denying the operator the one piece of
//     feedback they have on their own choice. Same reading the authoritative
//     fix gate and panelBelowBar already take.)
//   - The capable default is reached only when NO rung had anything, so it
//     clears no configured bar and sits in no rung. Nothing can empty beneath
//     it, and its rows are the only evidence that exists about the operator's
//     fallback model.
//
// This gates the FAILURE only. A win from a walked-down pick is still
// recorded: succeeding above your rung is real evidence, and dropping it would
// leave the rung above permanently unclimbable.
func walkedDown(p registry.Pick) bool {
	switch p.Source {
	case registry.SourceAuto, registry.SourceFavorite:
		return p.BelowBar()
	default: // pinned, capable default: off the ladder, so never walked down it
		return false
	}
}

// offLadderPick describes a model chosen without consulting the tier ladder:
// an operator pin, or the operator's configured default that a floor fell back
// to. It carries the tier the phase asked for and no measured prior, because
// nothing scored this slug against a bar - which is what keeps it out of the
// shortfall advisory while still recording the choice on the transcript.
func offLadderPick(reg *registry.Registry, model string, role registry.Role, tier registry.Tier, src registry.PickSource) registry.Pick {
	return registry.Pick{
		ModelSpec: registry.ModelSpec{
			Model:         model,
			ContextWindow: reg.ContextWindow(model),
		},
		Role:          role,
		RequestedTier: tier,
		Source:        src,
		OK:            true,
	}
}

// emitOrchestratorModelSelection resolves the orchestrator model (same
// precedence as resolveOrchestratorModel) and emits a model_selected event for
// the given phase. When reg is nil the emit is skipped entirely: the
// orchestrator's decision phases already guard on this, and there is no ladder
// to prove the model came from. The returned model string is the resolved slug,
// unchanged from what resolveOrchestratorModel returns, so call sites that only
// need the string can assign from the return.
//
// Provenance comes directly from resolveOrchestratorModel, not from a second
// precedence tree, so the event source is always in sync with what the resolver
// decided.
func emitOrchestratorModelSelection(
	ctx context.Context,
	reg *registry.Registry,
	emit *events.Emitter,
	ops Ops,
	cardID, pinned, payload, fallback string,
	phase string,
) string {
	model, src := resolveOrchestratorModel(ctx, reg, emit, ops, cardID, pinned, payload, fallback)

	if reg != nil {
		p := offLadderPick(reg, model, registry.RoleReviewer, decisionTier, src)
		emitModelSelection(emit, phase, "", p)
	}

	return model
}

// emitOrchestratorModel is a thin wrapper on *run so that every bare
// resolveOrchestratorModel call site in the orchestrator phases gets both
// the resolution and the model_selected event in one call. Future call sites
// that need the orchestrator model for a phase should use this method rather
// than calling resolveOrchestratorModel directly.
func (o *run) emitOrchestratorModel(ctx context.Context, phase string) string {
	d := o.d

	return emitOrchestratorModelSelection(ctx, d.Registry, d.Emit, d.Ops, d.Cfg.CardID,
		o.tc.ModelOrchestrator, d.Cfg.PayloadModel, d.Cfg.DefaultModel, phase)
}
