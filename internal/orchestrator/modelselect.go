package orchestrator

import (
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
