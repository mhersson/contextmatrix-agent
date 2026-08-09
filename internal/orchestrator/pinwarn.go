package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mhersson/contextmatrix-harness/events"
)

// warnUnresolvablePin emits a once-per-run advisory (slog warning, state-change
// event, card log entry) when a non-empty pin fails resolvePin on a selector
// path. It is safe to call from concurrent goroutines (the Best-of-N fan-out
// drives coder selection in parallel). Repeated calls for the same pinType
// produce exactly one set of outputs; the guard is reset only with the run.
//
// pinType must be "coder" or "reviewer" and selects the guard field and the log
// prefix. Other values produce a generic fallback.
func (o *run) warnUnresolvablePin(ctx context.Context, pinType, requested string) {
	o.selMu.Lock()

	var guard *bool
	prefix := pinType
	switch pinType {
	case "coder":
		guard = &o.coderPinWarned
		prefix = "coder"
	case "reviewer":
		guard = &o.reviewerPinWarned
		prefix = "reviewer"
	default:
		guard = new(bool) // unreachable in practice; compile-time safety for a generic arm
	}

	if *guard {
		o.selMu.Unlock()
		return
	}

	*guard = true
	o.selMu.Unlock()

	// Resolve the fallback target, mirroring resolveOrchestratorModel's
	// precedence: payload model first, then serve-config default.
	target := o.d.Cfg.PayloadModel
	if target == "" {
		target = o.d.Cfg.DefaultModel
	}

	slog.Warn("model pin not in catalog, falling back",
		"card_id", o.d.Cfg.CardID, "requested", requested, "using", target)

	if o.d.Emit != nil {
		o.d.Emit.Emit(events.StateChange, map[string]any{
			"warning":   fmt.Sprintf("%s model pin not in catalog, using fallback", prefix),
			"requested": requested,
			"using":     target,
		})
	}

	o.d.logCard(ctx, "%s model pin %q not in catalog - using %q", prefix, requested, target)
}
