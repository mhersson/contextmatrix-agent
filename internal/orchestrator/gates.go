package orchestrator

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// gatesRoundsCap bounds how many times each gate may push a fix round before it
// parks. It is per-gate and per-card, and survives a park/re-trigger because the
// counters live on the card (see gatesState).
const gatesRoundsCap = 3

// Effective-knob defaults, applied when the corresponding Config field is zero.
// A Deps built directly (tests, standalone runs) carries no knobs, so the gates
// must never derive a zero poll interval or a zero wait from it.
const (
	defaultGatesPollInterval       = 30 * time.Second
	defaultGatesCIWaitTimeout      = 45 * time.Minute
	defaultGatesCopilotWaitTimeout = 10 * time.Minute
)

// gatesSectionHeading is the card-body section the pr_gates phase owns.
const gatesSectionHeading = "PR Gates"

// GatesParkedError parks the card in review from the pr_gates phase. Reason
// is a short human phrase already recorded on the card.
type GatesParkedError struct{ Reason string }

func (e *GatesParkedError) Error() string {
	return "pr gates parked: " + e.Reason
}

// gatesState is the durable pr_gates progress, persisted as the "## PR Gates"
// card section so the round caps survive park/re-trigger.
type gatesState struct {
	CopilotRounds int
	CIRounds      int
	Status        string
	Detail        string // human-facing lines: failing checks, park reasons
}

// runPRGates is the pr_gates phase: after integrate has pushed and (optionally)
// opened the PR, it holds the card in review until the enabled gates pass -
// the Copilot review gate first, the CI gate last - then transitions the card
// to done. With no gate flags (or no PR intended) it is a pass-through. A
// gated card whose PR creation failed parks instead of completing
// (fail-closed): there is nothing to gate on.
func runPRGates(ctx context.Context, o *run) error {
	d := o.d
	cfg := d.Cfg

	gated := o.tc.AwaitCI || o.tc.AwaitCopilotReview

	// A resumed run starts in a fresh container with no in-memory URL, so it
	// falls back to the PR the earlier run reported through report_push.
	prURL := o.prURL
	if prURL == "" {
		prURL = o.tc.PRUrl
	}

	if gated && o.tc.CreatePR && prURL == "" {
		reason := "PR creation failed - nothing to gate on; open the PR manually and re-trigger"
		o.recordGates(ctx, gatesState{Status: "parked: " + reason})

		return &GatesParkedError{Reason: reason}
	}

	if gated && prURL != "" {
		st := o.loadGatesState()

		if o.tc.AwaitCopilotReview {
			if err := o.copilotGate(ctx, prURL, &st); err != nil {
				return err
			}
		}

		if o.tc.AwaitCI {
			if err := o.ciGate(ctx, prURL, &st); err != nil {
				return err
			}
		}

		st.Status = "passed"
		o.recordGates(ctx, st)
	}

	if err := d.Ops.TransitionCard(ctx, cfg.CardID, "done"); err != nil {
		return fmt.Errorf("transition parent to done: %w", err)
	}

	return nil
}

// copilotGate holds the card until the Copilot review on the PR is addressed.
// PLACEHOLDER: the Copilot-gate task replaces this body wholesale.
func (o *run) copilotGate(_ context.Context, _ string, _ *gatesState) error {
	return nil
}

// ciGate holds the card until the PR's CI checks are green.
// PLACEHOLDER: the CI-gate task replaces this body wholesale. It polls the
// checks once (ignoring the result) so the phase's URL plumbing is exercised
// end to end while the real waiting loop is still to come.
func (o *run) ciGate(ctx context.Context, prURL string, _ *gatesState) error {
	_, _ = o.d.PRGates.Checks(ctx, prURL)

	return nil
}

// recordGates writes the run's gate progress to the "## PR Gates" card section,
// so the counters survive a park and a human reading the card sees why it is
// waiting (or why it parked).
func (o *run) recordGates(ctx context.Context, st gatesState) {
	var b strings.Builder

	b.WriteString("## " + gatesSectionHeading + "\n\n")
	fmt.Fprintf(&b, "- Copilot rounds used: %d/%d\n", st.CopilotRounds, gatesRoundsCap)
	fmt.Fprintf(&b, "- CI rounds used: %d/%d\n", st.CIRounds, gatesRoundsCap)
	fmt.Fprintf(&b, "- Status: %s\n", st.Status)

	if detail := strings.TrimSpace(st.Detail); detail != "" {
		b.WriteString("\n" + detail + "\n")
	}

	o.recordSection(ctx, gatesSectionHeading, b.String())
}

// gatesRoundsRe matches the two "rounds used" counters recordGates writes, so a
// resumed run recovers them from the card body. Keep in sync with recordGates.
var (
	copilotRoundsRe = regexp.MustCompile(`(?m)^- Copilot rounds used: (\d+)/`)
	ciRoundsRe      = regexp.MustCompile(`(?m)^- CI rounds used: (\d+)/`)
)

// loadGatesState reads the persisted round counters back out of the card body.
// An absent (or unparsable) section yields the zero state - a run that has not
// used any round yet.
func (o *run) loadGatesState() gatesState {
	section := extractSection(o.body, gatesSectionHeading)
	if section == "" {
		return gatesState{}
	}

	return gatesState{
		CopilotRounds: firstSubmatchInt(copilotRoundsRe, section),
		CIRounds:      firstSubmatchInt(ciRoundsRe, section),
	}
}

// firstSubmatchInt returns re's first capture group in s as an int, or 0 when
// the pattern does not match or the capture is not a number.
func firstSubmatchInt(re *regexp.Regexp, s string) int {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 0
	}

	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}

	return n
}

// gatesPoll is how often the gates re-check the PR.
func (o *run) gatesPoll() time.Duration {
	if o.d.Cfg.GatesPollInterval > 0 {
		return o.d.Cfg.GatesPollInterval
	}

	return defaultGatesPollInterval
}

// ciWait bounds how long the CI gate waits for checks to settle.
func (o *run) ciWait() time.Duration {
	if o.d.Cfg.GatesCIWaitTimeout > 0 {
		return o.d.Cfg.GatesCIWaitTimeout
	}

	return defaultGatesCIWaitTimeout
}

// copilotWait bounds how long the Copilot gate waits for a review to land.
func (o *run) copilotWait() time.Duration {
	if o.d.Cfg.GatesCopilotWaitTimeout > 0 {
		return o.d.Cfg.GatesCopilotWaitTimeout
	}

	return defaultGatesCopilotWaitTimeout
}

// gateDeadline is when a gate waiting base from now must give up: the earlier of
// that and the container's own deadline, so a gate never waits past the point
// where the container is killed mid-wait. A zero Cfg.Deadline (serve did not
// tell the worker its timeout) leaves the gate bounded only by base.
func (o *run) gateDeadline(base time.Duration) time.Time {
	until := time.Now().Add(base)

	if dl := o.d.Cfg.Deadline; !dl.IsZero() && dl.Before(until) {
		return dl
	}

	return until
}
