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
// prefix. Other values panic to catch misuse.
func (o *run) warnUnresolvablePin(ctx context.Context, pinType, requested string) {
	o.selMu.Lock()

	var guard *bool

	switch pinType {
	case "coder":
		guard = &o.coderPinWarned
	case "reviewer":
		guard = &o.reviewerPinWarned
	default:
		panic(fmt.Sprintf("warnUnresolvablePin: unknown pinType %q", pinType))
	}

	if *guard {
		o.selMu.Unlock()

		return
	}

	*guard = true
	o.selMu.Unlock()

	slog.Warn("model pin not in catalog, falling back to tier selection",
		"card_id", o.d.Cfg.CardID, "requested", requested)

	if o.d.Emit != nil {
		o.d.Emit.Emit(events.StateChange, map[string]any{
			"warning":   fmt.Sprintf("%s model pin not in catalog, falling back to tier selection", pinType),
			"requested": requested,
		})
	}

	o.d.logCard(ctx, "%s model pin %q not in catalog - falling back to tier selection", pinType, requested)
}
