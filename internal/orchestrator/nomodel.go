package orchestrator

import (
	"fmt"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
)

// NoModelError marks the model-selection park: no catalogued model clears any
// configured tier bar for the role AND the operator's capable default is
// itself barred this run, so there is no model to do the work with. The worker
// maps it like the toolchain park - push WIP, transition the card to blocked,
// release, fail - so a human sees an environmental park rather than a run that
// silently coded on a model nobody chose.
type NoModelError struct {
	Role          registry.Role
	Tier          registry.Tier
	LowestBar     float64 // the bottom of the ladder that was walked
	RequestedBar  float64
	Excluded      int
	WindowLimited bool // the pool emptied on window fit, not on quality
}

func (e *NoModelError) Error() string {
	if e.WindowLimited {
		return fmt.Sprintf("no model with a large enough context window is available for role %s (%d excluded this run)",
			e.Role, e.Excluded)
	}

	return fmt.Sprintf("no model clears any configured bar (%s %.2f down to %.2f) for role %s (%d excluded this run)",
		e.Tier, e.RequestedBar, e.LowestBar, e.Role, e.Excluded)
}

// noModelError classifies a refusal from the refused pick, which already
// carries the whole ladder that was walked - the requested bar is the one
// number in the message that is not the problem.
//
// Re-probing without the window estimate separates "no model is good enough"
// from "no model is big enough": the ladder cannot buy a bigger context
// window, so a card log naming priors when the cause is an oversized prompt
// sends the operator the wrong way.
func (o *run) noModelError(in registry.SelectInput, p registry.Pick) *NoModelError {
	wide := in
	wide.EstTokens = 0

	return &NoModelError{
		Role:          in.Role,
		Tier:          in.Tier,
		RequestedBar:  p.RequestedBar,
		LowestBar:     p.LowestBar,
		Excluded:      len(in.Exclude),
		WindowLimited: in.EstTokens > 0 && o.d.Registry.SelectByComplexity(wide).OK,
	}
}

// noModelLogMessage is the canonical card-log line for a model-selection park.
// The window-limited wording names a different remedy because the tier ladder
// cannot reach a bigger context window.
func noModelLogMessage(nme *NoModelError) string {
	if nme.WindowLimited {
		return fmt.Sprintf("no model has a context window large enough for the %s work (%d model(s) excluded this run); parking card as blocked - narrow the card or add a larger-window model",
			nme.Role, nme.Excluded)
	}

	return fmt.Sprintf("no model clears any configured tier bar for role %s (asked for %s at %.2f, walked down to %.2f; %d model(s) excluded this run); parking card as blocked",
		nme.Role, nme.Tier, nme.RequestedBar, nme.LowestBar, nme.Excluded)
}
