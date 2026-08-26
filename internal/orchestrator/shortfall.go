package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/events"
)

// noteShortfall records what one selection was actually worth. Every pick is
// traced to the process log; a pick that did not clear the tier bar it asked
// for also reaches the operator, as a warning, a state-change event and one
// entry on the card's activity log.
//
// The card entry is deduped per (phase, role, requested -> met): the
// fixed-tier callers ask for the same tier on every call and the activity log
// is capped, so an undeduped advisory would evict the card's real history. A
// different requested bar, or a different bar actually met, is a different
// fact and earns its own line.
//
// It takes o.shortfallMu, never o.selMu: the Best-of-N candidate resolver
// holds selMu across a selection, so an advisory reaching for selMu would
// deadlock the fan-out.
func (o *run) noteShortfall(ctx context.Context, phase string, p registry.Pick) {
	slog.Info("selector: pick",
		"card_id", o.d.Cfg.CardID, "phase", phase, "model", p.Model,
		"requested_tier", string(p.RequestedTier), "met_tier", string(p.MetTier),
		"bar", p.RequestedBar, "prior", p.Prior, "source", p.Source.String())

	if p.AtBar() {
		return
	}

	// An operator pin is a choice, not a shortfall of the search: the selector
	// never looked, so "no model clears the bar" would be false, and the pins
	// this package synthesizes carry no measured prior to report either. The
	// trace above still records the pin and whatever the registry measured.
	// Whether a pin resolves at all is a separate advisory.
	if p.Source == registry.SourcePinned {
		return
	}

	if !o.firstNote(fmt.Sprintf("%s/%s/%s->%s", phase, p.Role, p.RequestedTier, p.MetTier)) {
		return
	}

	met := metTierLabel(p)

	slog.Warn("tier bar unreachable; selection fell short",
		"card_id", o.d.Cfg.CardID, "phase", phase, "role", string(p.Role),
		"requested", string(p.RequestedTier), "met", met, "model", p.Model, "prior", p.Prior)

	if o.d.Emit != nil {
		o.d.Emit.Emit(events.StateChange, map[string]any{
			"warning":   "no model clears the requested tier bar",
			"phase":     phase,
			"role":      string(p.Role),
			"requested": string(p.RequestedTier),
			"met":       met,
			"model":     p.Model,
		})
	}

	o.d.logCard(ctx, "%s: no model clears the %s bar (%.2f) for role %s - selected %s at %s (prior %.2f)",
		phase, p.RequestedTier, p.RequestedBar, p.Role, p.Model, met, p.Prior)
}

// metTierLabel names the strictest bar a pick actually cleared. A pick with no
// prior for the role clears nothing, and saying so is more useful than an
// empty field.
func metTierLabel(p registry.Pick) string {
	if p.MetTier == "" {
		return "no configured bar"
	}

	return string(p.MetTier)
}

// firstNote reports whether key has not been noted yet on this run, marking it
// noted. Safe from concurrent goroutines: the Best-of-N fan-out advises from
// parallel candidates.
func (o *run) firstNote(key string) bool {
	o.shortfallMu.Lock()
	defer o.shortfallMu.Unlock()

	if o.shortfallWarned == nil {
		o.shortfallWarned = map[string]bool{}
	}

	if o.shortfallWarned[key] {
		return false
	}

	o.shortfallWarned[key] = true

	return true
}
