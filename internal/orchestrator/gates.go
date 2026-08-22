package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-harness/events"
)

// gatesRoundsCap bounds how many times each gate may push a fix round before it
// parks. It is per-gate and per-card, and survives a park/re-trigger because the
// counters live on the card (see gatesState).
const gatesRoundsCap = 3

// Effective-knob defaults, applied when the corresponding Config field is zero.
// A Deps built directly (tests, standalone runs) carries no knobs, so the gates
// must never derive a zero poll interval or a zero wait from it.
const (
	defaultGatesPollInterval       = 60 * time.Second
	defaultGatesCIWaitTimeout      = 45 * time.Minute
	defaultGatesCopilotWaitTimeout = 10 * time.Minute
)

// gatesSectionHeading is the card-body section the pr_gates phase owns.
const gatesSectionHeading = "PR Gates"

// copilotSectionHeading is the card-body section one Copilot triage round owns,
// numbered "(Round N)" from round 2 on. copilotCommentsHeading labels the
// verbatim comment list inside it.
const (
	copilotSectionHeading  = "Copilot Review"
	copilotCommentsHeading = "Comments triaged"
)

// copilotKeyBodyChars is how much of a comment body identifies it for dedupe:
// enough to tell two comments on one file apart, short enough that a re-post
// whose tail moved (line numbers shift as the branch grows) still matches.
const copilotKeyBodyChars = 80

// copilotCommentLineRe matches the "- <path>: <digest>" lines recordCopilotRound
// writes under its comments heading. Keep the two in sync: they are the write and
// read halves of the dedupe key.
var copilotCommentLineRe = regexp.MustCompile(`^- (.+?): (.*)$`)

// Park reasons for a gate that ran out of the resources its fix rounds need.
// Both park the card in review rather than failing the run: the work is already
// pushed, so there is nothing to WIP-push and the PR stands as the human finds it.
const (
	gatesBudgetParkReason  = "budget exhausted during CI fixes"
	gatesTurnCapParkReason = "CI fix run hit its turn cap"

	gatesCopilotTriageBudgetParkReason = "budget exhausted during Copilot triage"
	gatesCopilotFixBudgetParkReason    = "budget exhausted during Copilot fixes"
	gatesCopilotTurnCapParkReason      = "Copilot fix run hit its turn cap"
)

// gatesNoChecksGrace is how long the CI gate waits for the first check to appear
// before concluding the repo has no CI. A var so tests can shrink it.
var gatesNoChecksGrace = 3 * time.Minute

// gatesFixRoundReserve is the wait a CI fix round must have left on the clock to
// be worth starting: the coder run, its push, and a fresh CI cycle all have to
// fit before the gate gives up, or the round only burns tokens on the way to a
// park. A var so tests can shrink it.
var gatesFixRoundReserve = 5 * time.Minute

// gatesCopilotRecheck is the pause between requesting a Copilot review and
// re-checking that the reviewer actually appeared (the request API can silently
// no-op on an account without Copilot review access). A var so tests can shrink it.
var gatesCopilotRecheck = 10 * time.Second

// gateProgressKind is the event a gate emits once per poll. It is not a
// harness kind - the log bridge picks it up through the agent's MapExtra hook,
// the same way mob discussion events are carried.
const gateProgressKind = "gate_progress"

// gateProgressHeartbeat is how long an unchanging gate may stay off the
// transcript before it shows its status again, so a long quiet wait still
// reads as alive rather than hung. A var so tests can shrink it.
var gateProgressHeartbeat = 5 * time.Minute

// gatePoller renders one gate's per-poll progress.
//
// EVERY poll emits, and that is load-bearing: the worker's output is what the
// serve-side idle watchdog reads as proof the container is alive, and a gate
// can legitimately wait tens of minutes without its status changing. What
// varies is only whether the poll is worth SHOWING. An unchanged status inside
// the heartbeat window is marked repeat=true and the log bridge drops it, so a
// waiting gate stays quiet on screen while the watchdog keeps seeing output
// and the durable run log keeps every poll.
type gatePoller struct {
	gate  string
	last  string
	shown time.Time
}

// poll emits one poll's status, marking it a repeat unless the status changed
// or the heartbeat came due. fields carries the gate's structured counts for
// the run log; they never affect the repeat decision, which is made on the
// rendered status alone.
func (p *gatePoller) poll(emit *events.Emitter, status string, fields map[string]any) {
	now := time.Now()
	repeat := status == p.last && now.Sub(p.shown) < gateProgressHeartbeat

	if !repeat {
		p.last = status
		p.shown = now
	}

	data := map[string]any{"gate": p.gate, "status": status, "repeat": repeat}
	maps.Copy(data, fields)

	emit.Emit(events.Kind(gateProgressKind), data)
}

// copilotFinding is one triaged Copilot comment: the orchestrator model's
// judgement on whether it names a real defect worth a fix round.
type copilotFinding struct {
	File   string `json:"file"`
	Issue  string `json:"issue"`
	Valid  bool   `json:"valid"`
	Reason string `json:"reason"`
}

// copilotTriage is the strict-JSON verdict the triage call returns.
type copilotTriage struct {
	Findings []copilotFinding `json:"findings"`
}

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

	// CopilotSatisfied records that a Copilot review was obtained and
	// addressed in this or an earlier run; a resumed gate skips straight to
	// CI instead of requesting (and paying for) another review. Skip-path
	// outcomes (Copilot unavailable, wait timeout) deliberately do NOT set
	// it - a re-trigger retries those.
	CopilotSatisfied bool

	Status string
	Detail string // human-facing lines: failing checks, park reasons
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

		// The recorded creation failed, but a PR may exist anyway - opened by
		// an earlier run whose report_push never landed, or left behind when
		// gh pr create failed with "a pull request already exists". Probe the
		// branch before giving up.
		found, ferr := d.PRGates.FindPRURL(ctx)
		if ferr != nil {
			slog.Warn("pr_gates: PR probe failed", "error", ferr)
		}

		if found == "" {
			return o.parkGates(ctx, &st, "PR creation failed - nothing to gate on; disable the PR-gate flags and re-trigger to complete without gating")
		}

		prURL = found

		o.d.logCard(ctx, "pr_gates: recovered the branch's existing PR: %s", prURL)

		// Make the recovery durable: record the URL the way integrate's
		// report_push would have, so a later park/resume reads it from the
		// task context instead of re-probing. Best-effort.
		if rerr := d.Ops.ReportPush(ctx, cfg.CardID, cfg.Branch, prURL); rerr != nil {
			slog.Warn("pr_gates: could not report the recovered PR", "error", rerr)
		}
	}

	if gated && prURL != "" {
		st := o.loadGatesState()

		if o.tc.AwaitCopilotReview {
			if err := o.copilotGate(ctx, prURL, &st); err != nil {
				return err
			}

			// Persist here, before the CI gate runs: a context cancellation
			// during ciGate's poll wait is routine container teardown (see
			// sleepGate's doc comment), and it returns straight out of
			// runPRGates without ever reaching the "passed" write below. A
			// CopilotSatisfied set only in memory by copilotGate's pass exits
			// would then be lost, and the next resume would re-request a paid
			// Copilot review this run already got and addressed. Harmless on
			// the already-satisfied and skip-path exits too: recordSection is
			// an idempotent upsert, and the skip paths leave the marker false.
			o.recordGates(ctx, st)
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

// copilotGate holds the card until the Copilot review on the PR is addressed: it
// takes a review already on the PR's current head when there is one, otherwise
// makes sure a review is actually requested and waits for one on that head, then
// triages its comments with one model call and spends up to gatesRoundsCap fix
// rounds on the findings it judges real.
//
// Copilot being unavailable NEVER parks the card. A proven "cannot review"
// response - a 422 "Copilot isn't available for this repository" - records the
// verbatim reason on the card and passes the gate. A generic request failure
// (e.g. a GraphQL login-resolution error) is NOT treated as unavailability: the
// gate still enters the wait loop, because a repo-automated Copilot review may
// arrive regardless, and a review that never arrives is recorded and passed at
// the wait deadline. A request that succeeds without the bot showing up in the
// pending reviewer list is recorded and waited through, not skipped - rulesets
// add Copilot asynchronously and gh cannot be trusted to list bots. The gate
// parks only on findings it could not get fixed, or on running out of budget or
// turns while fixing them.
func (o *run) copilotGate(ctx context.Context, prURL string, st *gatesState) error {
	if st.CopilotSatisfied {
		o.d.logCard(ctx, "pr_gates: Copilot review was addressed in an earlier run; gate already satisfied")

		return nil
	}

	// Seeded from the card body so the dedupe survives a park/re-trigger, then
	// grown in memory as this run triages further rounds.
	seen := copilotSeenComments(o.body)

	// A review may already be on the head - a re-trigger after a park, or a
	// ruleset review that landed while integrate was finishing. Reading it is
	// two gh calls; requesting another is a paid duplicate and a full wait.
	if review := o.copilotReviewOnHead(ctx, prURL); review != nil {
		o.d.logCard(ctx, "pr_gates: Copilot review already on the PR head; triaging it")

		satisfied, err := o.copilotReviewCycle(ctx, prURL, st, seen, review)
		if err != nil || satisfied {
			return err
		}
	} else {
		reason, unavailable, err := o.ensureCopilotReviewer(ctx, prURL)
		if err != nil {
			return err
		}

		if reason != "" {
			if unavailable {
				return o.skipCopilot(ctx, st, "pr_gates: Copilot review unavailable: "+reason+"; gate skipped")
			}

			// Not proof Copilot cannot review - the repo may assign the reviewer
			// itself. Record the verbatim reason and wait; the pass exits clear
			// it, and a pass-by-timeout records its own line.
			st.Detail = "- pr_gates: Copilot review could not be confirmed (" + reason + "); waiting for the review\n"
			o.recordGates(ctx, *st)
			o.d.logCard(ctx, "pr_gates: Copilot review could not be confirmed (%s); waiting for the review", reason)
		}
	}

	for {
		review, werr := o.awaitCopilotReview(ctx, prURL)
		if werr != nil {
			return werr
		}

		if review == nil {
			return o.skipCopilot(ctx, st, "pr_gates: Copilot review did not arrive in time; proceeding")
		}

		satisfied, err := o.copilotReviewCycle(ctx, prURL, st, seen, review)
		if err != nil || satisfied {
			return err
		}
	}
}

// copilotReviewOnHead returns the PR's latest Copilot review when it is a review
// of the CURRENT head, nil otherwise. Read failures are logged and read as
// "none": this is a probe, and the request/wait path behind it re-reads anyway.
func (o *run) copilotReviewOnHead(ctx context.Context, prURL string) *CopilotReview {
	review, err := o.d.PRGates.CopilotReview(ctx, prURL)
	if err != nil {
		slog.Warn("pr_gates: Copilot review probe failed", "card_id", o.d.Cfg.CardID, "pr_url", prURL, "error", err)

		return nil
	}

	if review == nil {
		return nil
	}

	head, err := o.d.PRGates.HeadSHA(ctx, prURL)
	if err != nil {
		slog.Warn("pr_gates: PR head SHA unreadable during review probe",
			"card_id", o.d.Cfg.CardID, "pr_url", prURL, "error", err)

		return nil
	}

	if head == "" || review.CommitID != head {
		return nil
	}

	return review
}

// copilotReviewCycle handles one arrived review: dedupes against what earlier
// rounds triaged, triages the fresh comments, and either passes the gate
// (satisfied=true) or spends a fix round and re-requests the review
// (satisfied=false - the caller waits for the re-review of the new head).
func (o *run) copilotReviewCycle(
	ctx context.Context, prURL string, st *gatesState, seen map[string]bool, review *CopilotReview,
) (bool, error) {
	fresh := unseenComments(review.Comments, seen)

	// Copilot re-posts the comments it already made on every re-review. A
	// round that carries nothing new is nothing to fix - re-triaging it would
	// spend a round undoing work an earlier round already did.
	if len(review.Comments) > 0 && len(fresh) == 0 {
		st.Detail = ""
		st.CopilotSatisfied = true

		o.d.logCard(ctx, "pr_gates: Copilot repeated only comments already triaged; gate passes")

		return true, nil
	}

	findings, terr := o.triageCopilot(ctx, st, review, fresh)
	if terr != nil {
		return false, terr
	}

	for _, c := range fresh {
		seen[copilotCommentKey(c.Path, c.Body)] = true
	}

	valid := validCopilotFindings(findings)
	if len(valid) == 0 {
		st.Detail = ""
		st.CopilotSatisfied = true

		o.d.logCard(ctx, "pr_gates: Copilot review addressed")

		return true, nil
	}

	if ferr := o.copilotFixRound(ctx, st, valid); ferr != nil {
		return false, ferr
	}

	if rerr := o.d.PRGates.RequestCopilotReview(ctx, prURL); rerr != nil {
		if strings.Contains(rerr.Error(), "Copilot isn't available for this repository") {
			return true, o.skipCopilot(ctx, st, "pr_gates: Copilot re-review unavailable: "+
				rerr.Error()+"; gate passes with the fixes already pushed")
		}

		// Same rule as the first request: a generic failure does not prove
		// Copilot will not review the new head (rulesets re-review every push).
		st.Detail = "- pr_gates: Copilot re-review could not be requested (" + rerr.Error() + "); waiting for the review of the fixed head\n"
		o.recordGates(ctx, *st)
		o.d.logCard(ctx, "pr_gates: Copilot re-review could not be requested (%s); waiting for the review of the fixed head", rerr.Error())
	}

	return false, nil
}

// ensureCopilotReviewer makes sure Copilot is on the PR as a reviewer, requesting
// it when it is not. It reports how a failure to put it there should be read:
// unavailable=true means Copilot is proven unable to review this repo - the
// request failed with a 422 "Copilot isn't available for this repository" - and
// the gate may skip. unavailable=false with a non-empty reason covers every other
// outcome: a check failure, a generic request failure (e.g. a GraphQL
// login-resolution error), or a request that succeeded without the reviewer
// showing up on re-check. None of those prove Copilot cannot review, so the
// caller records the reason and still waits for a repo-automated review. A
// non-nil error is the run context ending, and is fatal. The reason, when set, is
// the gh error VERBATIM - where Copilot cannot run, the line it puts on the card
// is the only diagnostic anyone gets.
func (o *run) ensureCopilotReviewer(ctx context.Context, prURL string) (reason string, unavailable bool, err error) {
	requested, err := o.d.PRGates.CopilotRequested(ctx, prURL)
	if err != nil {
		// A check failure is not proof of unavailability - the caller should
		// still wait for a repo-automated review.
		return err.Error(), false, nil
	}

	if requested {
		return "", false, nil
	}

	if err := o.d.PRGates.RequestCopilotReview(ctx, prURL); err != nil {
		// A 422 "Copilot isn't available for this repository" is proven
		// unavailability - skip the gate. Any other error is a generic request
		// failure and does not prove Copilot cannot review: a bare 422 is also
		// generic, since it can mean the request itself was malformed (e.g. the
		// requested_reviewers payload shape), and the repo's automatic Copilot
		// review may still arrive.
		errText := err.Error()

		if strings.Contains(errText, "Copilot isn't available for this repository") {
			return errText, true, nil
		}

		// Generic request failure - return the error as a non-fatal signal so
		// the caller can log it and still enter the wait loop.
		return errText, false, nil
	}

	// Re-check instead of trusting the request: a reviewer that was never added
	// would otherwise burn the gate's entire wait on a review nobody is writing.
	if err := o.sleepGate(ctx, gatesCopilotRecheck); err != nil {
		return "", false, err
	}

	requested, err = o.d.PRGates.CopilotRequested(ctx, prURL)
	if err != nil {
		// A re-check failure is not proof of unavailability.
		return err.Error(), false, nil
	}

	if !requested {
		// Not proof of unavailability either: a ruleset adds the reviewer
		// asynchronously, the listing can lag the request, and the API is known
		// to accept the request without listing the bot. Record the observation
		// and let the caller wait - a review that never arrives passes at the
		// wait deadline; a silent skip here loses a review that does arrive.
		return "the request succeeded but Copilot is not listed as a reviewer yet", false, nil
	}

	return "", false, nil
}

// awaitCopilotReview waits for a completed Copilot review of the PR's CURRENT
// head: a review of a superseded head was written against code the fix round that
// superseded it already changed. It returns a nil review (never an error) once
// the wait deadline passes - this gate proceeds on a missing review, it never
// parks on one. The deadline is per wait, so every re-requested review gets a
// full one, and gateDeadline still clamps each to the container's own deadline.
func (o *run) awaitCopilotReview(ctx context.Context, prURL string) (*CopilotReview, error) {
	deadline := o.gateDeadline(o.copilotWait())
	poller := &gatePoller{gate: "copilot"}

	for {
		review, reviewErr := o.d.PRGates.CopilotReview(ctx, prURL)
		if reviewErr != nil {
			// A gh hiccup is not a verdict. Keep waiting - the deadline still
			// bounds the gate - rather than skipping a review one poll away.
			slog.Warn("pr_gates: Copilot review poll failed; retrying",
				"card_id", o.d.Cfg.CardID, "pr_url", prURL, "error", reviewErr)
		}

		var (
			head    string
			headErr error
		)

		if review != nil && reviewErr == nil {
			// Only read the head when there is a review to match against it: most
			// polls have no review, and a gh call each time buys rate limit for
			// nothing.
			head, headErr = o.d.PRGates.HeadSHA(ctx, prURL)
			if headErr != nil {
				slog.Warn("pr_gates: PR head SHA unreadable; retrying",
					"card_id", o.d.Cfg.CardID, "pr_url", prURL, "error", headErr)
			}
		}

		onHead := review != nil && reviewErr == nil && headErr == nil &&
			head != "" && review.CommitID == head

		poller.poll(o.d.Emit, copilotPollStatus(review != nil, onHead),
			map[string]any{"have_review": review != nil, "on_head": onHead, "head_sha": head})

		if onHead {
			return review, nil
		}

		if time.Now().After(deadline) {
			return nil, nil
		}

		if werr := o.sleepPoll(ctx); werr != nil {
			return nil, werr
		}
	}
}

// copilotPollStatus renders one Copilot poll for the transcript. A review that
// is not on the current head reads differently from no review at all: the
// former is a review of code a fix round has already superseded, and a human
// watching the gate wait needs to see which of the two it is waiting through.
func copilotPollStatus(haveReview, onHead bool) string {
	switch {
	case onHead:
		return "Copilot review: received"
	case haveReview:
		return "Copilot review: waiting - the review on file is for an older commit"
	default:
		return "Copilot review: waiting"
	}
}

// triageCopilot judges which of the review's comments name real defects: ONE
// orchestrator-model call returning a strict JSON verdict. It records the round
// on the card - every comment with its verdict and the reason for it - so a human
// can see what the agent chose to ignore, and why.
func (o *run) triageCopilot(
	ctx context.Context, st *gatesState, review *CopilotReview, comments []ReviewComment,
) ([]copilotFinding, error) {
	d := o.d
	cfg := d.Cfg
	round := st.CopilotRounds + 1

	if err := o.ledger.Check(); err != nil {
		return nil, o.parkGates(ctx, st, gatesCopilotTriageBudgetParkReason)
	}

	model := resolveOrchestratorModel(ctx, d.Registry, d.Emit, d.Ops, cfg.CardID,
		o.tc.ModelOrchestrator, cfg.PayloadModel, cfg.DefaultModel)

	task := fmt.Sprintf(copilotTriagePrompt, o.grounding, o.tc.Title, o.taskDescription,
		copilotReviewSummary(review), formatCopilotComments(comments))

	res, dur, err := o.runModel(ctx, d.ReadTools, task, model)

	o.spendAndReport(ctx, o.ledger, cfg.CardID, "pr_gates: report Copilot triage usage failed",
		res, model, "main", dur)

	if err != nil {
		if reason := gateResourcePark(err, gatesCopilotTriageBudgetParkReason, gatesCopilotTurnCapParkReason); reason != "" {
			return nil, o.parkGates(ctx, st, reason)
		}

		return nil, fmt.Errorf("copilot triage run: %w", err)
	}

	findings, perr := parseCopilotTriage(res.Output)

	// A body-only review (zero line comments) with an unreadable verdict leaves
	// findings empty for a reason distinct from a genuinely clean, readable
	// round - recordCopilotRound needs to know which one happened so it never
	// claims the reviewer raised nothing when nothing was actually judged.
	unreadableWithNoComments := perr != nil && len(comments) == 0

	// A comment the triage did not actually judge must never ship past the gate:
	// take it at face value instead. A fix round spent on a nit costs less than a
	// real defect waved through, and the card records which of the two happened.
	switch {
	case perr != nil:
		if len(comments) == 0 {
			o.d.logCard(ctx, "pr_gates: Copilot triage verdict could not be read (%s); the review left no line comments to take at face value",
				perr.Error())
		} else {
			o.d.logCard(ctx, "pr_gates: Copilot triage verdict could not be read (%s); treating all %d comment(s) as findings",
				perr.Error(), len(comments))
		}

		findings = commentsAsFindings(comments, copilotUnreadableReason)

	case len(comments) > 0 && len(findings) == 0:
		// The verdict parsed but returned nothing. Silence is not a verdict of
		// "no defects" - the prompt asks for one entry per comment, invalid ones
		// included - so the comments stand until something judges them.
		o.d.logCard(ctx, "pr_gates: Copilot triage judged none of the %d comment(s); treating them as findings",
			len(comments))

		findings = commentsAsFindings(comments, copilotUnjudgedReason)
	}

	o.recordCopilotRound(ctx, round, comments, findings, unreadableWithNoComments)

	return findings, nil
}

// copilotFixRound spends one Copilot fix round on the findings triaged as valid.
// The round is counted and persisted BEFORE any work, so a crash mid-fix cannot
// buy a free retry on resume. It returns a park error when the rounds cap is
// spent or the fix runs out of budget or turns, and nil once the fix is pushed.
func (o *run) copilotFixRound(ctx context.Context, st *gatesState, findings []copilotFinding) error {
	st.Detail = copilotFindingLines(findings)

	if st.CopilotRounds >= gatesRoundsCap {
		return o.parkGates(ctx, st,
			fmt.Sprintf("Copilot findings still open after %d rounds", gatesRoundsCap))
	}

	st.CopilotRounds++
	st.Status = fmt.Sprintf("fixing Copilot findings (round %d/%d)", st.CopilotRounds, gatesRoundsCap)
	o.recordGates(ctx, *st)

	o.d.logCard(ctx, "pr_gates: Copilot review - fix round %d/%d on %d finding(s)",
		st.CopilotRounds, gatesRoundsCap, len(findings))

	if err := o.runFix(ctx, copilotFixFindings(findings), st.CopilotRounds, "", false); err != nil {
		if reason := gateResourcePark(err, gatesCopilotFixBudgetParkReason, gatesCopilotTurnCapParkReason); reason != "" {
			return o.parkGates(ctx, st, reason)
		}

		return fmt.Errorf("copilot gate fix round %d: %w", st.CopilotRounds, err)
	}

	return nil
}

// skipCopilot records why the Copilot gate is not holding the card and passes it.
// Every branch that could not get a review lands here: the line is card-logged
// verbatim and kept on the gates section, because it is the whole diagnostic
// channel for a Copilot setup the agent cannot see into.
func (o *run) skipCopilot(ctx context.Context, st *gatesState, line string) error {
	st.Detail = "- " + line + "\n"
	o.recordGates(ctx, *st)
	o.d.logCard(ctx, "%s", line)

	return nil
}

// recordCopilotRound records one triage round on the parent card body, matching
// recordReview's convention: round 1 uses the bare "## Copilot Review" heading,
// later rounds "## Copilot Review (Round N)". The comment list under it is
// written BY CODE from the review itself - it is both the human's record of what
// Copilot actually said and the dedupe key the next round reads back, so a
// re-triggered run never fixes the same comment twice.
func (o *run) recordCopilotRound(
	ctx context.Context, round int, comments []ReviewComment, findings []copilotFinding, triageUnreadable bool,
) {
	heading := copilotSectionHeading
	if round > 1 {
		heading = fmt.Sprintf("%s (Round %d)", copilotSectionHeading, round)
	}

	var b strings.Builder

	b.WriteString("## " + heading + "\n\n")

	if len(findings) == 0 {
		if triageUnreadable {
			b.WriteString("The triage verdict could not be read and the review left no line " +
				"comments to take at face value; nothing was judged.\n")
		} else {
			b.WriteString("The reviewer raised nothing to address.\n")
		}
	}

	for _, f := range findings {
		verdict := "INVALID"
		if f.Valid {
			verdict = "VALID"
		}

		fmt.Fprintf(&b, "- %s %s: %s\n", verdict, f.file(), f.Issue)

		if reason := strings.TrimSpace(f.Reason); reason != "" {
			fmt.Fprintf(&b, "  - %s\n", reason)
		}
	}

	if len(comments) > 0 {
		b.WriteString("\n### " + copilotCommentsHeading + "\n\n")

		for _, c := range comments {
			fmt.Fprintf(&b, "- %s: %s\n", c.Path, copilotCommentDigest(c.Body))
		}
	}

	o.recordSection(ctx, heading, b.String())
}

// parseCopilotTriage extracts the triage verdict JSON with the same extractor the
// review synthesis verdict uses, so a fenced or prose-wrapped answer still parses.
func parseCopilotTriage(s string) ([]copilotFinding, error) {
	raw, ok := extractJSON(s)
	if !ok {
		return nil, errors.New("no JSON object found in the triage output")
	}

	var t copilotTriage
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return nil, fmt.Errorf("unmarshal triage JSON: %w", err)
	}

	return t.Findings, nil
}

// Why a comment stands unjudged, recorded as its finding's reason so the card
// never presents a face-value fix as a triage decision.
const (
	copilotUnreadableReason = "the triage verdict could not be read - the comment is taken at face value"
	copilotUnjudgedReason   = "the triage returned no verdict for this comment - it is taken at face value"
)

// commentsAsFindings is the conservative reading of a review the triage did not
// judge: every comment stands as a finding, labelled with why nothing judged it.
func commentsAsFindings(comments []ReviewComment, reason string) []copilotFinding {
	findings := make([]copilotFinding, 0, len(comments))

	for _, c := range comments {
		findings = append(findings, copilotFinding{
			File:   c.Path,
			Issue:  flattenComment(c.Body),
			Valid:  true,
			Reason: reason,
		})
	}

	return findings
}

// validCopilotFindings keeps the findings the triage judged real - the only ones
// that fund a fix round.
func validCopilotFindings(findings []copilotFinding) []copilotFinding {
	valid := make([]copilotFinding, 0, len(findings))

	for _, f := range findings {
		if f.Valid {
			valid = append(valid, f)
		}
	}

	return valid
}

// copilotFindingLines renders findings in the "- <file>: <issue>" line shape
// fixFiles parses the paths back out of for fixup targeting (formatFixes'
// contract - keep the three in sync).
func copilotFindingLines(findings []copilotFinding) string {
	var b strings.Builder

	for _, f := range findings {
		fmt.Fprintf(&b, "- %s: %s", f.file(), f.Issue)

		if reason := strings.TrimSpace(f.Reason); reason != "" {
			fmt.Fprintf(&b, " - %s", reason)
		}

		b.WriteByte('\n')
	}

	return b.String()
}

// copilotFixFindings frames the valid findings as the fix coder's brief. Only
// valid ones are here: an invalid finding reaching the coder would have it
// "fixing" something the triage just judged to be no defect at all.
func copilotFixFindings(findings []copilotFinding) string {
	return "The Copilot reviewer raised these findings on the pull request, and they were triaged as real:\n" +
		copilotFindingLines(findings)
}

// copilotReviewSummary is the reviewer's own summary for the triage prompt.
func copilotReviewSummary(review *CopilotReview) string {
	if body := strings.TrimSpace(review.Body); body != "" {
		return body
	}

	return "(the reviewer left no summary)"
}

// formatCopilotComments lays the review's line comments out for the triage
// prompt, numbered so a verdict entry can be matched back to the comment it
// judged.
func formatCopilotComments(comments []ReviewComment) string {
	if len(comments) == 0 {
		return "(no line comments - judge the review summary alone)"
	}

	var b strings.Builder

	for i, c := range comments {
		fmt.Fprintf(&b, "%d. %s\n%s\n\n", i+1, c.Path, strings.TrimSpace(c.Body))
	}

	return b.String()
}

// copilotSeenComments rebuilds the set of comments already triaged from every
// Copilot round recorded on the card body. It is what makes the dedupe survive a
// park/re-trigger: a fresh container re-reads the rounds an earlier one wrote.
func copilotSeenComments(body string) map[string]bool {
	seen := make(map[string]bool)
	collecting := false

	for line := range strings.SplitSeq(sectionsWithPrefix(body, copilotSectionHeading), "\n") {
		if strings.HasPrefix(line, "#") {
			collecting = strings.TrimSpace(line) == "### "+copilotCommentsHeading

			continue
		}

		if !collecting {
			continue
		}

		if m := copilotCommentLineRe.FindStringSubmatch(line); m != nil {
			seen[copilotCommentKey(m[1], m[2])] = true
		}
	}

	return seen
}

// unseenComments drops the comments a previous round already triaged.
func unseenComments(comments []ReviewComment, seen map[string]bool) []ReviewComment {
	fresh := make([]ReviewComment, 0, len(comments))

	for _, c := range comments {
		if seen[copilotCommentKey(c.Path, c.Body)] {
			continue
		}

		fresh = append(fresh, c)
	}

	return fresh
}

// copilotCommentKey identifies a review comment for dedupe: its path plus the
// bounded, single-line head of its body. The recorded line and the live comment
// go through the same digest, so what a human reads on the card is exactly what
// the next round compares against.
func copilotCommentKey(path, body string) string {
	return path + "\x00" + copilotCommentDigest(body)
}

// copilotCommentDigest is the single-line, bounded form of a comment body: one
// card line per comment, and the dedupe key.
func copilotCommentDigest(body string) string {
	return truncateRunes(flattenComment(body), copilotKeyBodyChars)
}

// flattenComment collapses a comment body's whitespace onto one line, so a
// multi-line comment cannot break the recorded line shape it is read back from.
func flattenComment(body string) string {
	return strings.Join(strings.Fields(body), " ")
}

// truncateRunes caps s at n characters without splitting a rune.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}

	return string(r[:n])
}

// file is the finding's path, or a placeholder when the triage model omitted it,
// so the recorded and fix-run lines keep their "- <file>: <issue>" shape.
func (f copilotFinding) file() string {
	if strings.TrimSpace(f.File) == "" {
		return "(file not named)"
	}

	return f.File
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
	poller := &gatePoller{gate: "ci"}

	// The checks-were-seen memory spans containers: a gate whose persisted
	// section already records fix rounds ran CI in an earlier run, so its first
	// empty poll here is the resumed head's run registering - never "this repo
	// has no CI".
	sawChecks := st.CIRounds > 0

	for {
		checks, pollErr := o.d.PRGates.Checks(ctx, prURL)
		if pollErr != nil {
			// A gh hiccup is not a verdict. Keep waiting - the deadline still
			// bounds the gate - rather than failing a run whose work is pushed.
			// A CI-less repo does NOT land here: the worker seam translates gh's
			// "no checks reported" exit into an empty result with a nil error.
			slog.Warn("pr_gates: CI checks poll failed; retrying",
				"card_id", o.d.Cfg.CardID, "pr_url", prURL, "error", pollErr)
		}

		if len(checks) > 0 {
			// Whether this PR has CI at all is settled by the first check ever
			// seen. After that an empty rollup means the current head's run has
			// not registered yet, never "this repo has no CI". Every push the gate
			// makes follows an observed failure, so this also covers the empty
			// re-poll right after a fix round - which would otherwise pass a PR
			// that was red one poll earlier.
			sawChecks = true
		}

		b := classifyChecks(checks)

		poller.poll(o.d.Emit,
			fmt.Sprintf("CI checks: %d passed, %d pending, %d failed", b.passing, b.pending, len(b.failed)),
			map[string]any{"passing": b.passing, "pending": b.pending, "failed": len(b.failed)})

		switch {
		case pollErr == nil && len(checks) == 0 && !sawChecks:
			// No check has ever appeared on this PR. Past the grace window that
			// means the repo has no CI, not that CI is slow to register.
			if time.Since(start) >= gatesNoChecksGrace {
				st.Detail = ""

				o.d.logCard(ctx, "pr_gates: no CI checks on the PR; gate passes")

				return nil
			}

		case len(b.failed) > 0:
			if b.pending > 0 {
				// Red, but the run is still going. Wait for it to finish, however
				// long that takes. gh reads a check's failure log from the RUN's
				// log archive, which GitHub publishes only once EVERY job in the
				// run has finished, so a fix round started here gets a digest with
				// no failure output in it at all. There is no deadline worth
				// cutting the wait short for either: a run that never finishes is
				// a run whose log never becomes readable, so the fix round the
				// gate would buy by giving up early is precisely the blind one
				// this wait exists to prevent - better to let the gate's own
				// deadline park the card for a human.
				//
				// Waiting also means a sibling that fails a minute from now is
				// covered by the same pass, instead of costing a second round out
				// of three to fix what this one could have fixed.
				break
			}

			if time.Now().Add(gatesFixRoundReserve).After(deadline) {
				// Not enough wait left for a coder run, a push, and a fresh CI
				// cycle. Park on the red checks instead of spending a round whose
				// result the gate would never see.
				st.Detail = failedChecksDetail(b.failed)

				return o.parkGates(ctx, st, "CI red with too little gate wait left for another fix round")
			}

			if ferr := o.ciFixRound(ctx, prURL, st, b.failed); ferr != nil {
				return ferr
			}

			// The fix pushed a new head. Fall through to the poll-interval wait:
			// GitHub needs a moment to register the new run, and an immediate
			// re-poll reads the superseded one (or an empty rollup).

		case b.pending == 0 && b.passing > 0:
			// Every check settled and none failed. The passing>0 guard keeps a
			// failed poll (nil checks) out of this arm.
			st.Detail = ""

			o.d.logCard(ctx, "pr_gates: CI green")

			return nil
		}

		if time.Now().After(deadline) {
			return o.ciDeadlinePark(ctx, st, b, pollErr)
		}

		if werr := o.sleepPoll(ctx); werr != nil {
			return werr
		}
	}
}

// ciDeadlinePark parks a gate that used up its wait, naming what it was actually
// waiting on - unread checks read differently from checks that never settled, and
// the card must not claim the wrong one. The detail is rebuilt from the poll that
// parked, so the note never re-lists failures an earlier fix round addressed.
func (o *run) ciDeadlinePark(ctx context.Context, st *gatesState, b ciBuckets, pollErr error) error {
	switch {
	case pollErr != nil:
		st.Detail = "- the PR's check status could not be read on the last poll\n"

		return o.parkGates(ctx, st, "CI status could not be read before the wait deadline")

	case len(b.failed) > 0:
		st.Detail = failedChecksDetail(b.failed)

		return o.parkGates(ctx, st, "CI still red at the wait deadline")

	default:
		st.Detail = fmt.Sprintf("- at the deadline: %d check(s) still pending, %d passing\n",
			b.pending, b.passing)

		return o.parkGates(ctx, st, "CI still pending at the wait deadline")
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
		if reason := gateResourcePark(err, gatesBudgetParkReason, gatesTurnCapParkReason); reason != "" {
			return o.parkGates(ctx, st, reason)
		}

		return fmt.Errorf("ci gate fix round %d: %w", st.CIRounds, err)
	}

	return nil
}

// gateResourcePark maps a model-run failure to the park reason a gate records
// for it, or "" when the failure is not a resource the gate parks on. Budget and
// turn-cap exhaustion inside a gate park in review rather than failing the run:
// the work is already pushed, so there is nothing to WIP-push and the PR a human
// picks up is the one the gate was waiting on.
func gateResourcePark(err error, budgetReason, turnCapReason string) string {
	var (
		budget   *BudgetExceededError
		maxTurns *MaxTurnsError
	)

	switch {
	case errors.As(err, &budget):
		return budgetReason
	case errors.As(err, &maxTurns):
		return turnCapReason
	}

	return ""
}

// ciFixFindings frames the failure digest as findings for the fix coder, led by
// ciFailureNote so a coder whose verify command passes does not read that as
// the failure being gone.
func ciFixFindings(digest string) string {
	return ciFailureNote + "\n\nCI checks failed on the PR. Failure digest:\n" + digest
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

// sleepPoll waits one poll interval before the next gate poll.
func (o *run) sleepPoll(ctx context.Context) error {
	return o.sleepGate(ctx, o.gatesPoll())
}

// sleepGate waits d inside a gate, aborting early when the run's context is
// canceled so a container teardown is never delayed by a sleeping gate.
func (o *run) sleepGate(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
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

	if st.CopilotSatisfied {
		b.WriteString("- Copilot gate: satisfied\n")
	}

	if st.Status != "" {
		fmt.Fprintf(&b, "- Status: %s\n", st.Status)
	}

	if detail := strings.TrimSpace(st.Detail); detail != "" {
		b.WriteString("\n" + detail + "\n")
	}

	o.recordSection(ctx, gatesSectionHeading, b.String())
}

// copilotRoundsRe, ciRoundsRe and copilotSatisfiedRe match the "rounds used"
// counters and the satisfied marker recordGates writes, so a resumed run
// recovers them from the card body. Keep in sync with recordGates.
var (
	copilotRoundsRe    = regexp.MustCompile(`(?m)^- Copilot rounds used: (\d+)/`)
	ciRoundsRe         = regexp.MustCompile(`(?m)^- CI rounds used: (\d+)/`)
	copilotSatisfiedRe = regexp.MustCompile(`(?m)^- Copilot gate: satisfied$`)
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
		CopilotRounds:    firstSubmatchInt(copilotRoundsRe, section),
		CIRounds:         firstSubmatchInt(ciRoundsRe, section),
		CopilotSatisfied: copilotSatisfiedRe.MatchString(section),
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
