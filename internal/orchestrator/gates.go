package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// Park reasons for a gate that ran out of the resources its fix rounds need.
// Both park the card in review rather than failing the run: the work is already
// pushed, so there is nothing to WIP-push and the PR stands as the human finds it.
const (
	gatesBudgetParkReason  = "budget exhausted during CI fixes"
	gatesTurnCapParkReason = "CI fix run hit its turn cap"
)

// gatesNoChecksGrace is how long the CI gate waits for the first check to appear
// before concluding the repo has no CI. A var so tests can shrink it.
var gatesNoChecksGrace = 3 * time.Minute

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
		st := o.loadGatesState()

		return o.parkGates(ctx, &st, "PR creation failed - nothing to gate on; open the PR manually and re-trigger")
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

// ciBuckets is one checks poll classified into the three outcomes the CI gate
// acts on.
type ciBuckets struct {
	failed  []CheckResult
	pending int
	passing int
}

// classifyChecks splits a checks poll by gh's statusCheckRollup bucket. A
// skipped check counts as passing (a filtered workflow is not a failure), and
// any bucket we do not recognize counts as pending: gh grows new states over
// time, and waiting on an unknown one is safer than reading it as a pass (ships
// past a real failure) or as a failure (burns a fix round on nothing).
func classifyChecks(checks []CheckResult) ciBuckets {
	var b ciBuckets

	for _, c := range checks {
		switch c.Bucket {
		case "fail", "cancel":
			b.failed = append(b.failed, c)
		case "pass", "skipping":
			b.passing++
		default:
			b.pending++
		}
	}

	return b
}

// ciGate holds the card until the PR's CI checks are green: it polls the rollup,
// spends up to gatesRoundsCap fix rounds on failures, and parks the card in
// review when the checks stay red, never settle, or the budget runs out.
func (o *run) ciGate(ctx context.Context, prURL string, st *gatesState) error {
	deadline := o.gateDeadline(o.ciWait())
	start := time.Now()

	for {
		checks, err := o.d.PRGates.Checks(ctx, prURL)
		if err != nil {
			// A gh hiccup is not a verdict. Keep waiting - the deadline still
			// bounds the gate - rather than failing a run whose work is pushed.
			slog.Warn("pr_gates: CI checks poll failed; retrying",
				"card_id", o.d.Cfg.CardID, "pr_url", prURL, "error", err)
		}

		b := classifyChecks(checks)

		// One line per poll: the serve-side idle watchdog reads this as proof the
		// container is alive while the gate waits.
		slog.Info("pr_gates: CI checks polled", "card_id", o.d.Cfg.CardID, "pr_url", prURL,
			"passing", b.passing, "pending", b.pending, "failed", len(b.failed))

		switch {
		case err == nil && len(checks) == 0:
			// No check has appeared at all. Past the grace window that means the
			// repo has no CI, not that CI is slow to register.
			if time.Since(start) >= gatesNoChecksGrace {
				st.Detail = ""

				o.d.logCard(ctx, "pr_gates: no CI checks on the PR; gate passes")

				return nil
			}

		case len(b.failed) > 0:
			if ferr := o.ciFixRound(ctx, prURL, st, b.failed); ferr != nil {
				return ferr
			}

			// The fix pushed a new head; re-poll immediately so the next checks
			// call reads that head's run rather than the one just superseded.
			continue

		case b.pending == 0 && b.passing > 0:
			// Every check settled and none failed. The passing>0 guard keeps a
			// failed poll (nil checks) out of this arm.
			st.Detail = ""

			o.d.logCard(ctx, "pr_gates: CI green")

			return nil
		}

		if time.Now().After(deadline) {
			return o.parkGates(ctx, st, "CI still pending at the wait deadline")
		}

		if werr := o.sleepPoll(ctx); werr != nil {
			return werr
		}
	}
}

// ciFixRound spends one CI fix round on the failing checks. The round is counted
// and persisted BEFORE any work, so a crash mid-fix cannot buy a free retry on
// resume. It returns a park error when the rounds cap is spent or the budget runs
// out, and nil once the fix has been committed and pushed.
func (o *run) ciFixRound(ctx context.Context, prURL string, st *gatesState, failed []CheckResult) error {
	st.Detail = failedChecksDetail(failed)

	if st.CIRounds >= gatesRoundsCap {
		return o.parkGates(ctx, st, fmt.Sprintf("CI still red after %d fix rounds", gatesRoundsCap))
	}

	st.CIRounds++
	st.Status = fmt.Sprintf("fixing CI failures (round %d/%d)", st.CIRounds, gatesRoundsCap)
	o.recordGates(ctx, *st)

	o.d.logCard(ctx, "pr_gates: CI red - fix round %d/%d on %d failing check(s)",
		st.CIRounds, gatesRoundsCap, len(failed))

	digest, err := o.d.PRGates.FailureLogs(ctx, prURL, failed)
	if err != nil {
		// Never block a round on the log fetch: gh's own per-check descriptions
		// are a thinner but still actionable finding.
		slog.Warn("pr_gates: CI failure logs unavailable; using the check descriptions",
			"card_id", o.d.Cfg.CardID, "error", err)

		digest = failedChecksSummary(failed)
	}

	// Budget exhaustion inside a gate parks in review instead of returning the
	// budget error: the work is already pushed, so there is nothing to WIP-push,
	// and the PR a human picks up is the one the gate was waiting on.
	if err := o.ledger.Check(); err != nil {
		return o.parkGates(ctx, st, gatesBudgetParkReason)
	}

	if err := o.runFix(ctx, ciFixFindings(digest), st.CIRounds, "", false); err != nil {
		// A fix that ran out of budget or turns takes the same park arm as the
		// ledger check above - only the reason on the card differs.
		var (
			budget   *BudgetExceededError
			maxTurns *MaxTurnsError
		)

		switch {
		case errors.As(err, &budget):
			return o.parkGates(ctx, st, gatesBudgetParkReason)
		case errors.As(err, &maxTurns):
			return o.parkGates(ctx, st, gatesTurnCapParkReason)
		}

		return fmt.Errorf("ci gate fix round %d: %w", st.CIRounds, err)
	}

	return nil
}

// ciFixFindings frames the failure digest as findings for the fix coder.
func ciFixFindings(digest string) string {
	return "CI checks failed on the PR. Failure digest:\n" + digest
}

// failedChecksDetail lists the failing checks for the card section: the name a
// human recognizes plus the link they can open.
func failedChecksDetail(failed []CheckResult) string {
	var b strings.Builder

	for _, c := range failed {
		fmt.Fprintf(&b, "- %s: %s\n", c.Name, c.Link)
	}

	return b.String()
}

// failedChecksSummary is the fix-round finding when the failure logs could not be
// fetched: gh's own one-line description per failing check.
func failedChecksSummary(failed []CheckResult) string {
	var b strings.Builder

	for _, c := range failed {
		fmt.Fprintf(&b, "- %s: %s\n", c.Name, c.Description)
	}

	return b.String()
}

// parkGates parks the card in review from a gate: it carries the in-hand state
// (never a zero value - the round counters must survive the park), records the
// reason on the card body and in the activity log, and returns the park error.
func (o *run) parkGates(ctx context.Context, st *gatesState, reason string) error {
	st.Status = "parked: " + reason
	o.recordGates(ctx, *st)
	o.d.logCard(ctx, "pr_gates: parked - %s", reason)

	return &GatesParkedError{Reason: reason}
}

// sleepPoll waits one poll interval before the next gate poll, aborting early
// when the run's context is canceled so a container teardown is never delayed by
// a sleeping gate.
func (o *run) sleepPoll(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(o.gatesPoll()):
		return nil
	}
}

// recordGates writes the run's gate progress to the "## PR Gates" card section,
// so the counters survive a park and a human reading the card sees why it is
// waiting (or why it parked).
func (o *run) recordGates(ctx context.Context, st gatesState) {
	var b strings.Builder

	b.WriteString("## " + gatesSectionHeading + "\n\n")
	fmt.Fprintf(&b, "- Copilot rounds used: %d/%d\n", st.CopilotRounds, gatesRoundsCap)
	fmt.Fprintf(&b, "- CI rounds used: %d/%d\n", st.CIRounds, gatesRoundsCap)

	if st.Status != "" {
		fmt.Fprintf(&b, "- Status: %s\n", st.Status)
	}

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
