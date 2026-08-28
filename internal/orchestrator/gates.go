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
	defaultGatesPollInterval  = 60 * time.Second
	defaultGatesCIWaitTimeout = 45 * time.Minute
	// Copilot typically posts 5-10 minutes after the request on a mid-size PR;
	// 10 minutes left reviews at the top of that range unread.
	defaultGatesCopilotWaitTimeout = 20 * time.Minute
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

// copilotCommentLineRe matches the "- [VALID|INVALID ]<path>: <digest>" lines
// recordCopilotRound writes under its comments heading. The verdict group is
// optional so a line written before verdicts were recorded still parses. Keep
// the two in sync: they are the write and read halves of the dedupe key and,
// with the verdict, of the triage record.
var copilotCommentLineRe = regexp.MustCompile(`^- (?:(VALID|INVALID) )?(.+?): (.*)$`)

// Park reasons for a gate that ran out of the resources its fix rounds need.
// Both park the card in review rather than failing the run: the work is already
// pushed, so there is nothing to WIP-push and the PR stands as the human finds it.
const (
	gatesBudgetParkReason  = "budget exhausted during CI fixes"
	gatesTurnCapParkReason = "CI fix run hit its turn cap"

	gatesCopilotTriageBudgetParkReason = "budget exhausted during Copilot triage"
	gatesCopilotFixBudgetParkReason    = "budget exhausted during Copilot fixes"
	gatesCopilotTurnCapParkReason      = "Copilot fix run hit its turn cap"

	gatesCopilotFixNoChangeParkReason = "Copilot fix produced no change"
	gatesCIFixNoChangeParkReason      = "CI fix produced no change"

	// gatesFixParkFallbackReason names a fix-round park that arrived carrying no
	// reason of its own. Every park path sets one, so this guards an invariant
	// rather than describing a case: an empty return from gateResourcePark reads
	// as "not a park at all" and puts the gate back on the hard-fail path that
	// ends a run whose work is already pushed.
	gatesFixParkFallbackReason = "the fix round could not run"
)

// gatesNoChecksGrace is how long the CI gate waits for the first check to appear
// before concluding the repo has no CI. A var so tests can shrink it.
var gatesNoChecksGrace = 3 * time.Minute

// gatesFixRoundReserve is the least wait a CI fix round must have left on the
// clock to be worth starting: the coder run, its push, and a fresh CI cycle all
// have to fit before the gate gives up, or the round only burns tokens on the
// way to a park. Once the gate has watched one CI cycle settle, the reserve
// grows to that cycle plus gatesCoderAllowance. A var so tests can shrink it.
var gatesFixRoundReserve = 5 * time.Minute

// gatesCoderAllowance is the time a fix round's coder run and push are allowed
// on top of the observed CI cycle when sizing the fix-round reserve.
const gatesCoderAllowance = 2 * time.Minute

// gatesCopilotRecheck is the pause between requesting a Copilot review and
// re-checking that the reviewer actually appeared (the request API can silently
// no-op on an account without Copilot review access). A var so tests can shrink it.
var gatesCopilotRecheck = 10 * time.Second

// gatesCopilotGraceWait bounds the wait after a review request that did not
// take: the POST was accepted but the reviewer never appeared. A ruleset that
// reviews new PRs both lists the reviewer and delivers within minutes of PR
// creation, so a short window catches it; the full copilot_wait is reserved
// for a request known to be in flight. A var so tests can shrink it.
var gatesCopilotGraceWait = 5 * time.Minute

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

	data := make(map[string]any, len(fields)+3)
	maps.Copy(data, fields)

	data["gate"] = p.gate
	data["status"] = status
	data["repeat"] = repeat

	emit.Emit(events.Kind(gateProgressKind), data)
}

// gateNote records one gate decision on every channel a reader might hold: the
// worker's slog (durable run log), a gate_progress event with repeat=false (the
// serve transcript and the run log's JSONL), and the card activity log. The
// poll heartbeat covers waiting; this covers what the gate decided and why.
// The event's gate, status, repeat and decision keys are reserved: fields adds
// context around them and can never overwrite them.
func (o *run) gateNote(ctx context.Context, gate, line string, fields map[string]any) {
	slog.Info(line, "card_id", o.d.Cfg.CardID, "gate", gate)

	if o.d.Emit != nil {
		data := make(map[string]any, len(fields)+4)
		maps.Copy(data, fields)

		data["gate"] = gate
		data["status"] = line
		data["repeat"] = false
		data["decision"] = true

		o.d.Emit.Emit(events.Kind(gateProgressKind), data)
	}

	o.d.logCard(ctx, "%s", line)
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

	// CopilotDetail is the Copilot gate's recorded outcome (skip reason,
	// unconfirmed request, pass note). Kept apart from Detail so the CI gate's
	// writes never erase it.
	CopilotDetail string
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

	// One line naming what this phase is about to hold the card on: without it a
	// run log shows only the polls, and a gate that never ran is indistinguishable
	// from one that passed instantly.
	if gated {
		st := o.loadGatesState()
		o.gateNote(ctx, "pr_gates", fmt.Sprintf(
			"pr_gates: entering - await_ci=%t await_copilot_review=%t create_pr=%t pr_url=%s copilot_wait=%s ci_wait=%s poll=%s copilot_satisfied=%t",
			o.tc.AwaitCI, o.tc.AwaitCopilotReview, o.tc.CreatePR, prURL,
			o.copilotWait(), o.ciWait(), o.gatesPoll(), st.CopilotSatisfied,
		),
			map[string]any{"await_ci": o.tc.AwaitCI, "await_copilot_review": o.tc.AwaitCopilotReview, "pr_url": prURL})
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

		for {
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

			if !o.tc.AwaitCopilotReview || st.CopilotSatisfied {
				break
			}

			// The late probe runs after the enabled gates: one probe before
			// completing, so a review on the head is never left unread. A fix
			// round normally pushes a new head, so both gates run again on it.
			// A repo that refused the review (a proven 422) cannot have one on
			// the head, so the probe is skipped there.
			if strings.Contains(st.CopilotDetail, "Copilot isn't available for this repository") {
				break
			}

			pushed, err := o.copilotLateCheck(ctx, prURL, &st)
			if err != nil {
				return err
			}

			if !pushed {
				break
			}
		}

		st.Status = "passed"
		o.recordGates(ctx, st)
		o.gateNote(ctx, "pr_gates", "pr_gates: passed", nil)
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
// the wait deadline. A 2xx on the request is not trusted either - GitHub
// silently discards the request in some setups - so it is confirmed against
// the POST's response body and one delayed re-read of the pending reviewer
// list; a request confirmed by neither waits only a short grace window for a
// repo-automated review (a pending request appearing during grace upgrades to
// the full wait) and then passes with a note naming the dropped request. The
// gate parks only on findings it could not get fixed, or on running out of
// budget or turns while fixing them.
func (o *run) copilotGate(ctx context.Context, prURL string, st *gatesState) error {
	if st.CopilotSatisfied {
		o.gateNote(ctx, "copilot", "pr_gates: Copilot review was addressed in an earlier run; gate already satisfied", nil)

		return nil
	}

	// Seeded from the card body so the dedupe survives a park/re-trigger, then
	// grown in memory as this run triages further rounds.
	triaged := copilotTriagedComments(o.body)

	// pending carries a review already in hand into the wait loop below, so
	// the head probe and the grace wait feed the same triage path without a
	// duplicate cycle call site.
	var pending *CopilotReview

	// A review may already be on the head - a re-trigger after a park, or a
	// ruleset review that landed while integrate was finishing. Reading it is
	// two gh calls; requesting another is a paid duplicate and a full wait.
	if review := o.copilotReviewOnHead(ctx, prURL); review != nil {
		o.gateNote(ctx, "copilot", "pr_gates: Copilot review already on the PR head; triaging it", nil)

		pending = review
	} else {
		reason, outcome, err := o.ensureCopilotReviewer(ctx, prURL)
		if err != nil {
			return err
		}

		switch outcome {
		case copilotUnavailable:
			return o.skipCopilot(ctx, st, "pr_gates: Copilot review unavailable: "+reason+"; gate skipped")

		case copilotUnconfirmed:
			// The request affirmatively did not take. A full wait would stall
			// the card ~20 minutes for a review nothing suggests is coming, so
			// wait only a grace window - long enough to catch a ruleset that
			// reviews new PRs on its own, or a request the API listed late.
			st.CopilotDetail = "- pr_gates: Copilot review request did not take (" + reason + "); waiting briefly for a repo-automated review\n"
			o.recordGates(ctx, *st)
			o.gateNote(ctx, "copilot",
				fmt.Sprintf("pr_gates: Copilot review request did not take (%s); waiting briefly for a repo-automated review", reason), nil)

			review, requested, gerr := o.awaitCopilotGrace(ctx, prURL)
			if gerr != nil {
				return gerr
			}

			if review == nil && !requested {
				return o.skipCopilot(ctx, st,
					"pr_gates: Copilot review request did not take - the reviewer was never added and no review arrived; proceeding without one")
			}

			// requested=true falls through with pending nil: the reviewer is
			// on the PR after all, so the full wait below covers it.
			if requested {
				o.noteCopilotGraceUpgrade(ctx, st)
			}

			pending = review

		case copilotIndeterminate:
			// Not proof Copilot cannot review - the repo may assign the reviewer
			// itself. Record the verbatim reason and wait; a pass exit or a
			// pass-by-timeout overwrites CopilotDetail with its own line.
			st.CopilotDetail = "- pr_gates: Copilot review could not be confirmed (" + reason + "); waiting for the review\n"
			o.recordGates(ctx, *st)
			o.gateNote(ctx, "copilot",
				fmt.Sprintf("pr_gates: Copilot review could not be confirmed (%s); waiting for the review", reason), nil)

		case copilotReviewerPresent:
		}
	}

	for {
		review := pending
		pending = nil

		if review == nil {
			var werr error

			review, werr = o.awaitCopilotReview(ctx, prURL)
			if werr != nil {
				return werr
			}
		}

		if review == nil {
			return o.skipCopilot(ctx, st, "pr_gates: Copilot review did not arrive in time; proceeding")
		}

		outcome, err := o.copilotReviewCycle(ctx, prURL, st, triaged, review)
		if err != nil || outcome == cycleSatisfied {
			return err
		}

		if outcome == cycleAwaitGrace {
			found, requested, gerr := o.awaitCopilotGrace(ctx, prURL)
			if gerr != nil {
				return gerr
			}

			if found == nil && !requested {
				return o.skipCopilot(ctx, st,
					"pr_gates: fix pushed but the Copilot re-review request did not take; proceeding without a re-review of the fixed head")
			}

			// requested=true leaves pending nil: the reviewer is on the PR
			// after all, so the full wait at the top of the loop covers it.
			if requested {
				o.noteCopilotGraceUpgrade(ctx, st)
			}

			pending = found
		}
	}
}

// copilotLateCheck probes once for a Copilot review on the current head after
// the other gates ran. It reports whether the triage spent a fix round (which
// normally pushes a new head, and the caller then re-runs the gates) and
// returns the review-cycle errors verbatim.
//
// The loop it feeds is bounded: every pushed=true answer came out of
// copilotFixRound, which increments the persisted round counter and parks the
// card once it reaches gatesRoundsCap.
func (o *run) copilotLateCheck(ctx context.Context, prURL string, st *gatesState) (bool, error) {
	review := o.copilotReviewOnHead(ctx, prURL)
	if review == nil {
		return false, nil
	}

	o.gateNote(ctx, "copilot", "pr_gates: late Copilot review found on the PR head; triaging it", nil)

	outcome, err := o.copilotReviewCycle(ctx, prURL, st, copilotTriagedComments(o.body), review)
	if err != nil {
		return false, err
	}

	o.recordGates(ctx, *st)

	// cycleAwaitGrace gets no wait of its own here: pushed=true re-runs the
	// enabled gates, and the Copilot gate probes and requests again on the
	// new head.
	return outcome != cycleSatisfied, nil
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

// copilotCycleOutcome says what the gate should do after one triage/fix cycle.
type copilotCycleOutcome int

const (
	// cycleSatisfied: the gate passes; nothing left to wait for.
	cycleSatisfied copilotCycleOutcome = iota
	// cycleAwaitReview: a re-review is (or may be) in flight; wait the full
	// deadline for it.
	cycleAwaitReview
	// cycleAwaitGrace: the re-request affirmatively did not take; wait only
	// the grace window for a repo-automated review of the fixed head.
	cycleAwaitGrace
)

// copilotReviewCycle handles one arrived review: dedupes against what earlier
// rounds triaged, triages the fresh comments, and either passes the gate
// (cycleSatisfied) or spends fix rounds until one commits, then re-requests
// the review - the caller waits for the re-review of the new head, for the
// full deadline when the re-request confirmably took (cycleAwaitReview) and
// for only the grace window when it was accepted but dropped
// (cycleAwaitGrace). A fix round that committed nothing earns the
// gateNoChangeRetry-funded retry, and HEAD has not moved since the review
// that produced the findings - re-requesting would buy a
// guaranteed-identical review - so the retry is spent on the same findings
// instead. The loop is self-bounding: the second no-change round finds the
// retry already spent and parks. A comment an earlier round triaged VALID
// counts as still open when Copilot repeats it verbatim - the fix round did
// not actually resolve it - and it funds a fix round of its own, folded in
// with anything freshly triaged VALID this round.
func (o *run) copilotReviewCycle(
	ctx context.Context, prURL string, st *gatesState, triaged map[string]bool, review *CopilotReview,
) (copilotCycleOutcome, error) {
	fresh := unseenComments(review.Comments, triaged)
	reopened := repeatedValidComments(review.Comments, triaged)

	// Copilot re-posts the comments it already made on every re-review. A
	// round that carries nothing new AND nothing still open is nothing to
	// act on - re-triaging it would spend a round undoing work an earlier
	// round already did.
	if len(review.Comments) > 0 && len(fresh) == 0 && len(reopened) == 0 {
		st.CopilotDetail = "- pr_gates: Copilot repeated only comments already triaged as invalid; gate passes\n"
		st.CopilotSatisfied = true

		o.gateNote(ctx, "copilot", "pr_gates: Copilot repeated only comments already triaged as invalid; gate passes", nil)

		return cycleSatisfied, nil
	}

	var findings []copilotFinding

	// A round with nothing fresh to judge - only a comment a previous round
	// already triaged VALID, still open - has nothing left for the triage
	// model to say, so it skips the call and goes straight to the reopened
	// finding below. A body-only review (no line comments at all) never sets
	// skipTriage, since reopened is empty then too, and still triages as usual.
	skipTriage := len(fresh) == 0 && len(reopened) > 0

	if !skipTriage {
		var terr error

		findings, terr = o.triageCopilot(ctx, st, review, fresh)
		if terr != nil {
			return cycleAwaitReview, terr
		}

		verdicts := copilotCommentVerdicts(fresh, findings)
		for i, c := range fresh {
			triaged[copilotCommentKey(c.Path, c.Body)] = verdicts[i]
		}
	}

	stillOpen := commentsAsFindings(reopened, copilotRepeatedReason)
	stillOpen = append(stillOpen, validCopilotFindings(findings)...)

	if len(stillOpen) == 0 {
		st.CopilotDetail = "- pr_gates: Copilot review addressed\n"
		st.CopilotSatisfied = true

		o.gateNote(ctx, "copilot", "pr_gates: Copilot review addressed", nil)

		return cycleSatisfied, nil
	}

	if len(reopened) > 0 {
		o.gateNote(ctx, "copilot", fmt.Sprintf(
			"pr_gates: Copilot repeated %d finding(s) an earlier fix round did not resolve; fixing again", len(reopened),
		),
			map[string]any{"reopened": len(reopened)})
	}

	for {
		committed, ferr := o.copilotFixRound(ctx, st, stillOpen)
		if ferr != nil {
			return cycleAwaitReview, ferr
		}

		if committed {
			break
		}

		// gateNoChangeRetry funded a retry and HEAD is unchanged - a
		// re-request would return the same findings verbatim. Work the
		// same already-fetched findings with the stronger fixer the bar
		// raise buys. The loop is self-bounding: a second no-change
		// round finds the retry already spent and parks, and a round
		// that commits falls through to the re-request below.
	}

	confirmed, rerr := o.d.PRGates.RequestCopilotReview(ctx, prURL)
	if rerr != nil {
		if strings.Contains(rerr.Error(), "Copilot isn't available for this repository") {
			return cycleSatisfied, o.skipCopilot(ctx, st, "pr_gates: Copilot re-review unavailable: "+
				rerr.Error()+"; gate passes with the fixes already pushed")
		}

		// Same rule as the first request: a generic failure does not prove
		// Copilot will not review the new head (rulesets re-review every push).
		st.CopilotDetail = "- pr_gates: Copilot re-review could not be requested (" + rerr.Error() + "); waiting for the review of the fixed head\n"
		o.recordGates(ctx, *st)
		o.gateNote(ctx, "copilot", fmt.Sprintf(
			"pr_gates: Copilot re-review could not be requested (%s); waiting for the review of the fixed head", rerr.Error(),
		), nil)

		return cycleAwaitReview, nil
	}

	if !confirmed {
		// Give the listing one recheck's worth of lag before concluding the
		// re-request was dropped - the same allowance the first request gets.
		if serr := o.sleepGate(ctx, gatesCopilotRecheck); serr != nil {
			return cycleAwaitReview, serr
		}

		requested, qerr := o.d.PRGates.CopilotRequested(ctx, prURL)
		if qerr == nil && !requested {
			st.CopilotDetail = "- pr_gates: Copilot re-review request did not take; waiting briefly for a repo-automated review of the fixed head\n"
			o.recordGates(ctx, *st)
			o.gateNote(ctx, "copilot",
				"pr_gates: Copilot re-review request did not take; waiting briefly for a repo-automated review of the fixed head", nil)

			return cycleAwaitGrace, nil
		}
	}

	return cycleAwaitReview, nil
}

// copilotRequestOutcome says how the gate should read ensureCopilotReviewer's
// result.
type copilotRequestOutcome int

const (
	// copilotReviewerPresent: Copilot is confirmed pending; wait the full
	// deadline.
	copilotReviewerPresent copilotRequestOutcome = iota
	// copilotUnavailable: a proven 422 "Copilot isn't available for this
	// repository"; the gate may skip.
	copilotUnavailable
	// copilotUnconfirmed: the request was accepted but the reviewer is
	// affirmatively absent - the POST response body and a delayed re-check
	// both came up empty. Nothing suggests a review is coming, so the caller
	// waits only a short grace window.
	copilotUnconfirmed
	// copilotIndeterminate: a check or request errored; nothing proves
	// Copilot will not review (a repo-automated review may arrive), so the
	// caller records the reason and waits the full deadline.
	copilotIndeterminate
)

// ensureCopilotReviewer makes sure Copilot is on the PR as a reviewer,
// requesting it when it is not, and reports how the result should be read via
// copilotRequestOutcome. A non-nil error is the run context ending, and is
// fatal. The reason, when set, is the gh error VERBATIM - where Copilot cannot
// run, the line it puts on the card is the only diagnostic anyone gets.
func (o *run) ensureCopilotReviewer(ctx context.Context, prURL string) (reason string, outcome copilotRequestOutcome, err error) {
	requested, err := o.d.PRGates.CopilotRequested(ctx, prURL)
	if err != nil {
		slog.Info("pr_gates: Copilot reviewer check failed", "card_id", o.d.Cfg.CardID, "pr_url", prURL, "reason", err.Error())

		// A check failure is not proof of unavailability - the caller should
		// still wait for a repo-automated review.
		return err.Error(), copilotIndeterminate, nil
	}

	if requested {
		slog.Info("pr_gates: Copilot is already a requested reviewer", "card_id", o.d.Cfg.CardID, "pr_url", prURL)

		return "", copilotReviewerPresent, nil
	}

	confirmed, rerr := o.d.PRGates.RequestCopilotReview(ctx, prURL)
	if rerr != nil {
		// A 422 "Copilot isn't available for this repository" is proven
		// unavailability - skip the gate. Any other error is a generic request
		// failure and does not prove Copilot cannot review: a bare 422 is also
		// generic, since it can mean the request itself was malformed (e.g. the
		// requested_reviewers payload shape), and the repo's automatic Copilot
		// review may still arrive.
		errText := rerr.Error()

		if strings.Contains(errText, "Copilot isn't available for this repository") {
			slog.Info("pr_gates: Copilot is unavailable for this repository",
				"card_id", o.d.Cfg.CardID, "pr_url", prURL, "reason", errText)

			return errText, copilotUnavailable, nil
		}

		slog.Info("pr_gates: Copilot review request failed",
			"card_id", o.d.Cfg.CardID, "pr_url", prURL, "reason", errText)

		// Generic request failure - return the error as a non-fatal signal so
		// the caller can log it and still enter the wait loop.
		return errText, copilotIndeterminate, nil
	}

	if confirmed {
		// The POST's response body listed the bot as a pending reviewer -
		// authoritative, no re-check needed.
		slog.Info("pr_gates: Copilot review requested and confirmed", "card_id", o.d.Cfg.CardID, "pr_url", prURL)

		return "", copilotReviewerPresent, nil
	}

	slog.Info("pr_gates: Copilot review requested but not confirmed by the response",
		"card_id", o.d.Cfg.CardID, "pr_url", prURL)

	// Re-check instead of trusting the request: a reviewer that was never added
	// would otherwise burn the gate's entire wait on a review nobody is writing.
	if err := o.sleepGate(ctx, gatesCopilotRecheck); err != nil {
		return "", copilotIndeterminate, err
	}

	requested, err = o.d.PRGates.CopilotRequested(ctx, prURL)
	if err != nil {
		slog.Info("pr_gates: Copilot reviewer re-check failed",
			"card_id", o.d.Cfg.CardID, "pr_url", prURL, "reason", err.Error())

		// A re-check failure is not proof of unavailability.
		return err.Error(), copilotIndeterminate, nil
	}

	if !requested {
		slog.Info("pr_gates: Copilot is not listed as a reviewer after the request",
			"card_id", o.d.Cfg.CardID, "pr_url", prURL)

		// The API accepted the request, but neither the response body nor a
		// delayed listing shows the reviewer: the request was dropped. GitHub
		// does this instead of erroring when the requesting identity cannot
		// use Copilot. Not proof a review will never arrive - a ruleset may
		// deliver one on its own - so the caller waits a grace window, not
		// the full deadline.
		return "the request was accepted but Copilot was never added as a reviewer", copilotUnconfirmed, nil
	}

	slog.Info("pr_gates: Copilot is on the PR as a reviewer", "card_id", o.d.Cfg.CardID, "pr_url", prURL)

	return "", copilotReviewerPresent, nil
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

// noteCopilotGraceUpgrade records that a pending request appeared during the
// grace window, so the card note no longer claims a brief wait while the gate
// waits the full one.
func (o *run) noteCopilotGraceUpgrade(ctx context.Context, st *gatesState) {
	const line = "pr_gates: a pending Copilot review request appeared during the grace window; waiting the full window"

	st.CopilotDetail = "- " + line + "\n"
	o.recordGates(ctx, *st)
	o.gateNote(ctx, "copilot", line, nil)
}

// awaitCopilotGrace waits briefly for evidence that anyone will review: a
// review landing on the current head, or Copilot appearing as a pending
// reviewer (a ruleset request, or a request the API listed late) - the latter
// returned as requested=true so the caller can upgrade to the full wait. It
// exists for requests that did not confirmably take: burning the full
// copilot_wait there stalls the card for a review nothing suggests is coming.
// Like awaitCopilotReview, it returns nil results (never an error) at the
// deadline - the gate proceeds on a missing review, it never parks on one.
func (o *run) awaitCopilotGrace(ctx context.Context, prURL string) (review *CopilotReview, requested bool, err error) {
	deadline := o.gateDeadline(gatesCopilotGraceWait)
	poller := &gatePoller{gate: "copilot"}

	for {
		pending, reqErr := o.d.PRGates.CopilotRequested(ctx, prURL)
		if reqErr == nil && pending {
			return nil, true, nil
		}

		found := o.copilotReviewOnHead(ctx, prURL)

		poller.poll(o.d.Emit, copilotPollStatus(found != nil, found != nil),
			map[string]any{"have_review": found != nil, "unconfirmed_grace": true})

		if found != nil {
			return found, false, nil
		}

		if time.Now().After(deadline) {
			return nil, false, nil
		}

		if werr := o.sleepPoll(ctx); werr != nil {
			return nil, false, werr
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
// spent or the fix runs out of budget or turns, and reports whether the fix
// committed a change: nil with committed=false means the gateNoChangeRetry-
// funded retry was granted, so HEAD has not moved and the caller must not
// re-request the review.
func (o *run) copilotFixRound(ctx context.Context, st *gatesState, findings []copilotFinding) (bool, error) {
	st.CopilotDetail = copilotFindingLines(findings)

	if st.CopilotRounds >= gatesRoundsCap {
		return false, o.parkGates(ctx, st,
			fmt.Sprintf("Copilot findings still open after %d rounds", gatesRoundsCap))
	}

	st.CopilotRounds++
	st.Status = fmt.Sprintf("fixing Copilot findings (round %d/%d)", st.CopilotRounds, gatesRoundsCap)
	o.recordGates(ctx, *st)

	o.gateNote(ctx, "copilot", fmt.Sprintf("pr_gates: Copilot review - fix round %d/%d on %d finding(s)",
		st.CopilotRounds, gatesRoundsCap, len(findings)),
		map[string]any{"round": st.CopilotRounds, "cap": gatesRoundsCap, "findings": len(findings)})

	// FixTier is deliberately left empty: a gate finding is not scoped to one
	// review round, so the card bar is the right fallback.
	committed, err := o.runFix(ctx, fixRequest{Findings: copilotFixFindings(findings), Round: st.CopilotRounds})
	if err != nil {
		if reason := gateResourcePark(err, gatesCopilotFixBudgetParkReason, gatesCopilotTurnCapParkReason); reason != "" {
			return false, o.parkGates(ctx, st, reason)
		}

		return false, fmt.Errorf("copilot gate fix round %d: %w", st.CopilotRounds, err)
	}

	if !committed {
		if o.gateNoChangeRetry(ctx, "copilot", "Copilot", st.CopilotRounds) {
			return false, nil
		}

		return false, o.parkGates(ctx, st, gatesCopilotFixNoChangeParkReason)
	}

	return true, nil
}

// skipCopilot records why the Copilot gate is not holding the card and passes it.
// Every branch that could not get a review lands here: the line is card-logged
// verbatim and kept on the gates section, because it is the whole diagnostic
// channel for a Copilot setup the agent cannot see into.
func (o *run) skipCopilot(ctx context.Context, st *gatesState, line string) error {
	st.CopilotDetail = "- " + line + "\n"
	o.recordGates(ctx, *st)
	o.gateNote(ctx, "copilot", line, nil)

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

		verdicts := copilotCommentVerdicts(comments, findings)
		for i, c := range comments {
			verdict := "INVALID"
			if verdicts[i] {
				verdict = "VALID"
			}

			fmt.Fprintf(&b, "- %s %s: %s\n", verdict, flattenComment(c.Path), copilotCommentDigest(c.Body))
		}
	}

	o.recordSection(ctx, heading, b.String())
}

// copilotCommentVerdicts pairs each comment with the triage verdict it
// received, for recordCopilotRound to write and copilotTriagedComments to read
// back. The triage prompt asks for one finding entry per comment, in the order
// given, so when the counts match the pairing is by index. A triage response
// that omits or duplicates an entry breaks that alignment, so the fallback ORs
// the Valid flag over every finding naming the comment's path instead; a
// comment no finding names at all was never judged and reads VALID, since an
// unjudged comment must never be recorded as safe to ignore.
func copilotCommentVerdicts(comments []ReviewComment, findings []copilotFinding) []bool {
	verdicts := make([]bool, len(comments))

	if len(findings) == len(comments) {
		for i, f := range findings {
			verdicts[i] = f.Valid
		}

		return verdicts
	}

	for i, c := range comments {
		var (
			matched bool
			valid   bool
		)

		for _, f := range findings {
			if f.File == c.Path {
				matched = true
				valid = valid || f.Valid
			}
		}

		verdicts[i] = valid || !matched
	}

	return verdicts
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

// Why a comment stands as a finding without this round's triage judging it
// fresh: unreadable or missing verdict, or already judged real and still open
// after a fix round. Recorded as the finding's reason so the card never
// presents any of these as a fresh triage decision.
const (
	copilotUnreadableReason = "the triage verdict could not be read - the comment is taken at face value"
	copilotUnjudgedReason   = "the triage returned no verdict for this comment - it is taken at face value"
	copilotRepeatedReason   = "Copilot repeated this finding after a fix round - it was triaged as a real defect and is still open"
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

// copilotTriagedComments rebuilds the comments already triaged from every
// Copilot round recorded on the card body, keyed by copilotCommentKey with the
// recorded verdict as the value (true = triaged VALID). It is what makes the
// dedupe survive a park/re-trigger: a fresh container re-reads the rounds an
// earlier one wrote. A line recorded before verdicts were tracked carries none
// and reads as VALID - the conservative reading. Read the map with the comma-ok
// form, never a bare index: an INVALID verdict stores false, so a bare read
// cannot tell "triaged INVALID" from "never triaged".
func copilotTriagedComments(body string) map[string]bool {
	triaged := make(map[string]bool)
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
			verdict, path, digest := m[1], m[2], m[3]
			triaged[copilotCommentKey(path, digest)] = verdict != "INVALID"
		}
	}

	return triaged
}

// unseenComments drops the comments a previous round already triaged. It is a
// membership test, not a value test: an INVALID verdict stores false in
// triaged, and that comment must still be filtered as already triaged rather
// than read as "not seen".
func unseenComments(comments []ReviewComment, triaged map[string]bool) []ReviewComment {
	fresh := make([]ReviewComment, 0, len(comments))

	for _, c := range comments {
		if _, ok := triaged[copilotCommentKey(c.Path, c.Body)]; ok {
			continue
		}

		fresh = append(fresh, c)
	}

	return fresh
}

// repeatedValidComments returns the review comments Copilot re-posted that an
// earlier round triaged VALID: the fix round spent on them did not resolve the
// finding, so it is still open and must not pass the gate as already triaged.
func repeatedValidComments(comments []ReviewComment, triaged map[string]bool) []ReviewComment {
	reopened := make([]ReviewComment, 0, len(comments))

	for _, c := range comments {
		if valid, ok := triaged[copilotCommentKey(c.Path, c.Body)]; ok && valid {
			reopened = append(reopened, c)
		}
	}

	return reopened
}

// copilotCommentKey identifies a review comment for dedupe: its flattened path
// plus the bounded, single-line head of its body. The recorded line and the
// live comment go through the same flattening and digest, so what a human reads
// on the card is exactly what the next round compares against - and a path
// carrying a newline cannot break the recorded line shape any more than a body
// can.
func copilotCommentKey(path, body string) string {
	return flattenComment(path) + "\x00" + copilotCommentDigest(body)
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
	// has no CI". It also spans re-entries within this run: a Copilot fix push
	// sends the gate round again, and the new head's run has not registered yet.
	sawChecks := st.CIRounds > 0 || o.ciSawChecks
	polls := 0

	for {
		checks, pollErr := o.d.PRGates.Checks(ctx, prURL)
		polls++

		if pollErr != nil {
			var permanent *PermanentPollError
			if errors.As(pollErr, &permanent) {
				// The seam says this failure repeats on every poll - looping to
				// the deadline would only park later and blinder.
				st.Detail = "- " + permanent.Err + "\n"

				return o.parkGates(ctx, st, "CI status could not be read (permanent gh failure, see detail)")
			}

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
			o.ciSawChecks = true
		}

		b := classifyChecks(checks)

		// The first settled poll after the gate's own first read is one CI cycle
		// watched end to end; the fix-round reserve grows to fit it. The first
		// poll is skipped: it reflects state from before the gate started.
		if o.ciObservedSettle == 0 && polls > 1 && len(checks) > 0 && b.pending == 0 {
			o.ciObservedSettle = time.Since(start)
		}

		poller.poll(o.d.Emit,
			fmt.Sprintf("CI checks: %d passed, %d pending, %d failed", b.passing, b.pending, len(b.failed)),
			map[string]any{"passing": b.passing, "pending": b.pending, "failed": len(b.failed)})

		switch {
		case pollErr == nil && len(checks) == 0 && !sawChecks:
			// No check has ever appeared on this PR. Past the grace window that
			// means the repo has no CI, not that CI is slow to register.
			if time.Since(start) >= gatesNoChecksGrace {
				st.Detail = ""

				o.gateNote(ctx, "ci", "pr_gates: no CI checks on the PR; gate passes", nil)

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

			reserve := gatesFixRoundReserve
			if o.ciObservedSettle > 0 {
				reserve = max(reserve, o.ciObservedSettle+gatesCoderAllowance)
			}

			if time.Now().Add(reserve).After(deadline) {
				// Not enough wait left for a coder run, a push, and a fresh CI
				// cycle. Park on the red checks instead of spending a round whose
				// result the gate would never see.
				st.Detail = failedChecksDetail(b.failed)

				return o.parkGates(ctx, st, "CI red with too little gate wait left for another fix round")
			}

			if ferr := o.ciFixRound(ctx, prURL, st, b.failed); ferr != nil {
				return ferr
			}

			// When the fix committed, wait one poll interval - GitHub needs a
			// moment to register the new run - then go straight to a fresh poll:
			// the deadline verdict below must never read the buckets this round
			// was started from, or a park would list the failures it just fixed.
			if werr := o.sleepPoll(ctx); werr != nil {
				return werr
			}

			continue

		case b.pending == 0 && b.passing > 0:
			// Every check settled and none failed. The passing>0 guard keeps a
			// failed poll (nil checks) out of this arm.
			st.Detail = ""

			o.gateNote(ctx, "ci", "pr_gates: CI green", nil)

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
// parked, so the note never re-lists failures an earlier fix round addressed; a
// failed last poll puts its error text on the card, the only diagnostic there is.
func (o *run) ciDeadlinePark(ctx context.Context, st *gatesState, b ciBuckets, pollErr error) error {
	switch {
	case pollErr != nil:
		st.Detail = "- " + pollErr.Error() + "\n"

		return o.parkGates(ctx, st, "CI status could not be read before the wait deadline")

	case len(b.failed) > 0:
		st.Detail = failedChecksDetail(b.failed)

		return o.parkGates(ctx, st, "CI still red at the wait deadline")

	case b.pending == 0 && b.passing == 0:
		// Nothing on the head - after a fix round, the new run had not
		// registered before the wait ran out. Say that, not "0 pending".
		st.Detail = "- at the deadline: no checks had registered on the current head\n"

		return o.parkGates(ctx, st, "no checks had registered on the current head at the wait deadline")

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

	o.gateNote(ctx, "ci", fmt.Sprintf("pr_gates: CI red - fix round %d/%d on %d failing check(s)",
		st.CIRounds, gatesRoundsCap, len(failed)),
		map[string]any{"round": st.CIRounds, "cap": gatesRoundsCap, "failed": len(failed)})

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

	// FixTier is deliberately left empty: a gate finding is not scoped to one
	// review round, so the card bar is the right fallback.
	req := fixRequest{Findings: ciFixFindings(digest), Round: st.CIRounds}
	committed, err := o.runFix(ctx, req)

	var mte *MaxTurnsError

	// The budget term is keyed on the WIDTH this round actually ran at, not on
	// the step counter: the budget is clamped at the top rung, so a card whose
	// bar already seeds it there would keep buying rounds of identical width
	// while the note claimed each was wider.
	if errors.As(err, &mte) && committed && o.fixSizing(req).Budget < maxBudgetStep && st.CIRounds < gatesRoundsCap {
		// A capped round that PUSHED earns another CI cycle: the poll loop will
		// see a real new head. One that pushed nothing does not - it would
		// re-bucket the identical settled-red checks and burn the remaining
		// rounds back to back with no CI feedback between them.
		o.markFixCapped()
		o.gateNote(ctx, "ci", fmt.Sprintf("pr_gates: CI fix round %d hit its turn cap after pushing - retrying wider",
			st.CIRounds), map[string]any{"round": st.CIRounds})

		return nil
	}

	if err != nil {
		// A fix that ran out of budget or turns takes the same park arm as the
		// ledger check above - only the reason on the card differs.
		if reason := gateResourcePark(err, gatesBudgetParkReason, gatesTurnCapParkReason); reason != "" {
			return o.parkGates(ctx, st, reason)
		}

		return fmt.Errorf("ci gate fix round %d: %w", st.CIRounds, err)
	}

	if !committed {
		if o.gateNoChangeRetry(ctx, "ci", "CI", st.CIRounds) {
			return nil
		}

		return o.parkGates(ctx, st, gatesCIFixNoChangeParkReason)
	}

	return nil
}

// gateNoChangeRetry funds the ONE retry a no-change fix round earns before a
// gate parks: a round that changed nothing is quality evidence, and another
// round is already funded, so the gate raises the shared bar once and spends
// it on a stronger fixer rather than parking without ever having tried one.
// Shared by the CI and Copilot arms so neither carries its own copy of the
// rule. Keyed on the bar counter, which the review loop also writes: on a
// card whose rounds already escalated elsewhere, a stronger fixer has been
// tried and failed, so the caller parks on the first no-op round exactly as
// before. Reports whether it funded the retry; false tells the caller to park.
func (o *run) gateNoChangeRetry(ctx context.Context, gate, label string, rounds int) bool {
	if o.fixBarSteps != 0 || rounds >= gatesRoundsCap {
		return false
	}

	o.markFixFailed("produced no change")
	o.gateNote(ctx, gate, fmt.Sprintf("pr_gates: %s fix round %d produced no change - retrying with a stronger fixer",
		label, rounds), map[string]any{"round": rounds})

	return true
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
		parked   *ReviewParkedError
	)

	switch {
	case errors.As(err, &budget):
		return budgetReason
	case errors.As(err, &maxTurns):
		return turnCapReason
	case errors.As(err, &parked):
		// The shared fix path parks for more than one reason - no fixer left
		// after the failed rounds, and nothing selectable at all, which can
		// happen on a gate's FIRST round. The card carries the error's own
		// reason rather than a cause the gate guessed at.
		if parked.Reason != "" {
			return parked.Reason
		}

		return gatesFixParkFallbackReason
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
	o.gateNote(ctx, "pr_gates", "pr_gates: parked - "+reason, map[string]any{"reason": reason})

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

	if detail := strings.TrimSpace(st.CopilotDetail); detail != "" {
		b.WriteString("\n" + detail + "\n")
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
