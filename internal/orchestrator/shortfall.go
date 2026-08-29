package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/events"
)

// noteShortfall records what one selection was actually worth. Every pick is
// traced to the process log and emitted to the run transcript; a pick that did
// not clear the tier bar it asked for also reaches the operator, as a warning,
// a state-change event and one entry on the card's activity log.
//
// rep is the competing-pool report the registry returned for this pick, empty
// for the paths that never consulted a rung (pins, off-ladder capable-default
// picks). A non-empty report earns exactly one "selector: pool" line beside
// the pick line, so every pick correlates with the field it was chosen from.
//
// It is the single wiring point for the transcript event, so a phase that
// selects a model on a tier records that selection by construction. subtaskID
// is empty for the card-level phases.
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
func (o *run) noteShortfall(ctx context.Context, phase, subtaskID string, p registry.Pick, rep registry.SelectionReport) {
	// Unconditional, and above every gate below: the transcript records what
	// ran, not only what fell short, and the shortfall dedupe must never
	// swallow a selection.
	emitModelSelection(o.d.Emit, phase, subtaskID, p)

	slog.Info("selector: pick",
		"card_id", o.d.Cfg.CardID, "phase", phase, "model", p.Model,
		"requested_tier", string(p.RequestedTier), "met_tier", string(p.MetTier),
		"bar", p.RequestedBar, "prior", p.Prior, "has_prior", p.HasPrior,
		"source", p.Source.String())

	o.logSelectionPool(phase, subtaskID, p, rep)

	if p.AtBar() {
		return
	}

	// An operator pin is a choice, not a shortfall of the search, so it never
	// gets the "no model clears the bar" wording: the selector never looked.
	// A pin the registry MEASURED still has a true fact to report, that its
	// own prior does not clear the bar the phase asked for, and gets its own
	// wording below. The pins this package synthesizes carry no prior at all,
	// so there is nothing to say about them beyond the trace above; whether
	// such a pin resolves is a separate advisory.
	pinned := p.Source == registry.SourcePinned
	if pinned && !p.HasPrior {
		return
	}

	if !o.firstNote(fmt.Sprintf("%s/%s/%s->%s", phase, p.Role, p.RequestedTier, p.MetTier)) {
		return
	}

	met := metTierLabel(p)

	warning := "no model clears the requested tier bar"

	line := fmt.Sprintf("%s: no model clears the %s bar (%.2f) for role %s - selected %s at %s (%s)",
		phase, p.RequestedTier, p.RequestedBar, p.Role, p.Model, met, priorClause(p))

	if pinned {
		warning = "the pinned model does not clear the requested tier bar"
		line = fmt.Sprintf("%s: the pinned model %s does not clear the %s bar (%.2f) - it met %s (%s)",
			phase, p.Model, p.RequestedTier, p.RequestedBar, met, priorClause(p))
	}

	slog.Warn("tier bar unreachable; selection fell short",
		"card_id", o.d.Cfg.CardID, "phase", phase, "role", string(p.Role),
		"requested", string(p.RequestedTier), "met", met, "model", p.Model,
		"prior", p.Prior, "has_prior", p.HasPrior, "source", p.Source.String())

	if o.d.Emit != nil {
		o.d.Emit.Emit(events.StateChange, map[string]any{
			"warning":   warning,
			"phase":     phase,
			"role":      string(p.Role),
			"requested": string(p.RequestedTier),
			"met":       met,
			"model":     p.Model,
		})
	}

	o.d.logCard(ctx, "%s", line)
}

// logSelectionPool emits the competing-pool line beside the pick line: every
// candidate that reached the rung the pick was made on, with its prior, price
// and outcome, plus a compact reason-aggregated summary of the catalog models
// that never got there. Exactly one line per pick, and only when the pick
// came off a rung - a pin, an off-ladder capable default, or a discarded
// proposal never consulted a pool and reports none.
func (o *run) logSelectionPool(phase, subtaskID string, p registry.Pick, rep registry.SelectionReport) {
	if rep.Rung == "" && len(rep.Pool) == 0 && len(rep.FilteredOut) == 0 {
		return
	}

	attrs := []any{
		"card_id", o.d.Cfg.CardID, "phase", phase, "subtask_id", subtaskID,
		"model", p.Model, "requested_tier", string(p.RequestedTier),
		"rung", string(rep.Rung), "rung_bar", rep.Bar, "role", string(p.Role),
	}

	for _, e := range rep.Pool {
		attrs = append(attrs,
			"pool_"+e.Model,
			strings.Join([]string{
				"prior=" + strconv.FormatFloat(e.Prior, 'f', -1, 64),
				"price=" + strconv.FormatFloat(e.Price, 'g', -1, 64),
				"outcome=" + string(e.Outcome),
			}, " "))
	}

	for _, f := range rep.FilteredOut {
		attrs = append(attrs, "filtered_"+string(f.Reason), strings.Join(f.Models, ","))
	}

	slog.Info("selector: pool", attrs...)
}

// priorClause renders a pick's prior, or says there is none. HasPrior exists
// to separate a measured zero from no measurement at all, and this is where
// that distinction reaches a human. The capable default routinely has no
// prior - it is typically an operator-configured slug the live catalog does
// not carry, so the priors built from that catalog cannot score it - and
// printing 0.00 there would tell an operator their own chosen fallback rated
// worst possible.
func priorClause(p registry.Pick) string {
	if !p.HasPrior {
		return "no measured prior"
	}

	return fmt.Sprintf("prior %.2f", p.Prior)
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
