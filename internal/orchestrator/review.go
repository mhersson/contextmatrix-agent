package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/mhersson/contextmatrix-agent/internal/mob"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// reviewPanelSize is the fixed number of review specialists fanned out per
// round: Correctness, Design & Maintainability, Security & Performance.
const reviewPanelSize = 3

// hardReviewIterationCap is a defensive ceiling on the review loop independent
// of the configured attempts cap, so a misbehaving IncrementReviewAttempts (or
// a zero cap) can never loop forever.
const hardReviewIterationCap = 50

// verifyOutputTail caps the verify-command output carried into findings, so a
// noisy failing suite does not swamp the fix prompt. It is a TAIL: a build
// tool's diagnostics are concentrated in its last bytes, and res.Output is
// already bounded at 64 KiB by verifyexec, so nothing upstream is lost.
const verifyOutputTail = 4000

// verifyFailedPrefix marks findings that came from a failed verify gate rather
// than the specialist panel, so runFix can route them to verifyFixPrompt.
const verifyFailedPrefix = "verify command failed: "

// verifyFailedFindings is the finding text a failed verify gate hands its fix
// run: the command that failed, then the redacted excerpt of what it printed.
// The "(tail)" label matches judge.go's identical evidence, so the coder knows
// the block starts mid-output rather than at the command's start. Shared by
// both gates that can fail - the review round and the pre-commit gate - so the
// coder sees one framing whichever one caught the failure.
func verifyFailedFindings(plan verifyPlan, output string) string {
	return verifyFailedPrefix + plan.Display + "\n\nVerify output (tail):\n\n" + verifyFailureExcerpt(output)
}

// cleanupDiscardPrefix opens the card-log line the post-approval cleanup pass
// writes when its fixup is reset away. A constant because the discard is the
// one event on that path with no other trace: the fixup is gone from the
// branch, the summary reverts to open findings, and nothing else on the card
// says a cleanup pass ran at all.
const cleanupDiscardPrefix = "cleanup fix discarded: "

// reviewRoundReserve is the least container time a verify run, a specialist
// panel, and a fix round need to complete without being killed mid-work. A var
// so tests can shrink it.
var reviewRoundReserve = 20 * time.Minute

// verdict is the synthesis model's structured decision: approve outright or
// return a concrete fix list for the coder.
type verdict struct {
	Approved bool   `json:"approved"`
	Summary  string `json:"summary"`
	FixTier  string `json:"fix_tier"`
	Fixes    []fix  `json:"fixes"`
}

// fix is one actionable finding the coder must address on the next round.
type fix struct {
	File       string `json:"file"`
	Issue      string `json:"issue"`
	Suggestion string `json:"suggestion"`
	Severity   string `json:"severity"`
}

// ReviewParkedError marks the review phase stopping and leaving the card for a
// human. The worker maps it to the park path: exit 0, completed callback, card
// left in review. Parked is not failed - a human picks the card up from review.
//
// Reason is the true cause. Several paths reach this sentinel and most write
// an accurate line of their own before returning it, but the cleanup fix pass
// renders Error() straight onto the card, so a fixed string would state a
// cause that is false there. The zero value keeps the attempts-cap wording,
// which is what a bare construction has always meant.
type ReviewParkedError struct {
	Reason string
}

func (e *ReviewParkedError) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = reviewParkedAttemptsCap
	}

	return "review parked: " + reason
}

// The causes a review park can have. Each is the phrase that follows
// "review parked: " on the card.
const (
	reviewParkedAttemptsCap  = "attempts cap exhausted without approval"
	reviewParkedServerCap    = "the board's review attempts cap was reached"
	reviewParkedNoTime       = "less than one round of container time left"
	reviewParkedNoReviewer   = "no reviewer model is selectable"
	reviewParkedNoFixModel   = "no fix model is selectable"
	reviewParkedFixExhausted = "no fix model other than the ones that already failed"
)

// reviewTimeLeft parks the review when less than reviewRoundReserve remains on
// the container deadline, before a verify run, panel, or fix round can be
// started that has no chance of finishing. round names the round a re-trigger
// resumes at in the card log.
func (o *run) reviewTimeLeft(ctx context.Context, round int) error {
	d := o.d
	dl := d.Cfg.Deadline

	if dl.IsZero() {
		return nil
	}

	remaining := time.Until(dl)
	if remaining >= reviewRoundReserve {
		return nil
	}

	// A deadline already behind us reads as "0s left", not a negative span.
	remaining = max(remaining, 0)

	d.logCard(ctx, "review parked: %s of container time left, less than the %s a review round needs - a re-trigger resumes at round %d",
		remaining, reviewRoundReserve, round)

	return &ReviewParkedError{Reason: reviewParkedNoTime}
}

// runReview is the review phase. The parent enters review (idempotent on
// resume), then hands off to reviewLoop (autonomous) or runReviewHITL: cheap
// incremental rounds where a detected verify gate runs first and
// short-circuits to the fix run on failure; otherwise three read-only
// specialists fan out on diverse models and one synthesis call decides
// approve-or-fix.
func runReview(ctx context.Context, o *run) error {
	d := o.d
	cfg := d.Cfg

	// Idempotent on resume: only transition into review when not already there.
	if o.tc.State != "review" {
		if err := d.Ops.StartReview(ctx, cfg.CardID); err != nil {
			return fmt.Errorf("start review: %w", err)
		}
	}

	// Resolve the effective attempts cap. When the persisted counter already
	// meets or exceeds the cap AND the card is entering review from a non-review
	// state (e.g., moved back to todo), the card was parked on a prior run with
	// the counter left at the cap. Reset it to zero so round numbering in the
	// fresh run restarts at 1 - a local reset only; the server's review_attempts
	// counter is monotonic and unaffected. Cards still in review (crash-resume
	// from an interrupted authoritative pass) keep their counter so round
	// numbering continues from where it left off.
	attemptsCap := cfg.ReviewAttemptsCap
	if attemptsCap <= 0 {
		attemptsCap = config.DefaultReviewAttemptsCap
	}

	if o.tc.ReviewAttempts >= attemptsCap && o.tc.State != "review" {
		o.tc.ReviewAttempts = 0
	}

	plan, err := o.ensureVerify(ctx)
	if err != nil {
		return err
	}

	if cfg.Interactive {
		return o.runReviewHITL(ctx, plan)
	}

	return o.reviewLoop(ctx, plan, 0)
}

// reviewLoop is the autonomous review loop. Approval exits nil; each
// non-approval increments the card's review attempts and runs a fix. At the
// cliff (the round that would otherwise park) the gated authoritative pass
// takes over instead of parking on a cheap verdict - it is the sole park gate,
// except when the server's own ceiling refuses a cheap round's increment.
// The budget ledger is checked before every model-bearing step.
//
// consumed is the number of review rounds already run in-process this run: 0
// when this loop owns the whole phase, >0 when runReviewHITL hands over after
// a mid-run promotion. o.tc is a run-start snapshot; every in-process round
// that did not approve also incremented the server counter, so
// o.tc.ReviewAttempts + consumed mirrors the persisted count and round
// numbering stays resume-stable.
func (o *run) reviewLoop(ctx context.Context, plan verifyPlan, consumed int) error {
	d := o.d
	cfg := d.Cfg

	// Guard a mis-wired worker: a zero or negative cap would make the cliff trip
	// on the FIRST round and park (via the authoritative pass) every card
	// immediately. Fall back to CM's convention instead.
	attemptsCap := cfg.ReviewAttemptsCap
	if attemptsCap <= 0 {
		attemptsCap = config.DefaultReviewAttemptsCap
	}

	// fixRan is whether the IMMEDIATELY PRECEDING fix round both ran to
	// completion and committed: a capped round clears it and a round that
	// landed nothing never sets it, because a red verify is charged to the
	// round behind it only when that round has not already been judged.
	fixRan := false

	for iter := range hardReviewIterationCap {
		// Round number continues across resumes: review_attempts persists the
		// count of prior rounds, so round N is stable for the body record.
		round := o.tc.ReviewAttempts + consumed + iter + 1

		if err := o.reviewTimeLeft(ctx, round); err != nil {
			return err
		}

		// At the cliff (the round that would otherwise park), run the gated
		// authoritative pass instead of another cheap round - never park on a cheap
		// verdict here, except when the server's own ceiling refuses this round's
		// increment. It is terminal: returns nil (finished) or parks.
		if round >= attemptsCap {
			return o.authoritativeReview(ctx, plan, round)
		}

		findings, fixTier, approved, vres, fixes, err := o.reviewRound(ctx, plan, round, false)
		if err != nil {
			return err
		}

		// Record this round on the parent card body for the complete review
		// history (CM's review-task workflow skill writes ## Review Findings the same way).
		o.recordReview(ctx, round, findings, approved, vres)

		if approved {
			o.reviewSummary = findings // synthesis verdict summary (plus any surviving fixes), for the PR body

			// Findings survived the critique round despite approval: run exactly
			// one non-escalating cleanup pass and finish either way. This is not
			// another review round - it never increments review attempts, never
			// loops back into the panel, and a transport error or a no-op commit
			// can never un-approve a verdict that already cleared review. A PARK
			// is the exception: budget, context limit, turn cap and missing
			// toolchain are not opinions about the change, they are the points at
			// which the worker learns the run must stop and push its WIP, so they
			// propagate instead of being swallowed here.
			if len(fixes) > 0 {
				// Findings reach the PR body framed as open until a pass lands
				// them: an approval carrying raw findings text lets the PR model
				// narrate them as fixed (see approvedDespiteFindings).
				o.reviewSummary = approvedWithOpenFindings(findings)

				if !worthCleanupPass(fixes) {
					d.logCard(ctx, "review: approved with %d surviving nit-only finding(s) - reported, no cleanup pass", len(fixes))

					return nil
				}

				// The commit the cleanup pass starts from, read BEFORE it runs:
				// a fixup its own verify cannot prove is reset back to here, and
				// once the fixup exists this commit is no longer what HEAD reads.
				// An unreadable HEAD is not fatal - the pass still runs, and the
				// gate below reports the result it can no longer discard.
				pre, herr := d.Git.Head(ctx)
				if herr != nil {
					slog.Warn("review: could not record the pre-cleanup head",
						"card_id", cfg.CardID, "error", herr)

					pre = ""
				}

				committed, ferr := o.runFix(ctx, fixRequest{Findings: findings, Round: round, FixTier: fixTier, NoEscalate: true})
				if ferr != nil {
					if isParkError(ferr) {
						d.logCard(ctx, "review: approved with %d surviving finding(s), but the cleanup fix pass parked - stopping: %s",
							len(fixes), ferr)

						return ferr
					}

					d.logCard(ctx, "review: approved with %d surviving finding(s), but the cleanup fix pass failed - proceeding approved: %s",
						len(fixes), ferr)

					// The failed pass may have left partial edits uncommitted (a
					// transport error mid-run, before runFix ever reaches its own
					// commit). Best-effort, matching the sibling coder-run-failure
					// discards elsewhere: untracked files survive a hard reset, but
					// a dirty tracked file left behind here would otherwise carry
					// into integrate's autosquash rebase and kill an approved run.
					if herr := d.Git.HardReset(ctx, "HEAD"); herr != nil {
						slog.Warn("review: failed to discard the swallowed cleanup pass's partial edits",
							"card_id", cfg.CardID, "error", herr)
					}

					return nil
				}

				if committed {
					o.reviewSummary = approvedWithFixesApplied(findings)

					d.logCard(ctx, "review: approved with %d surviving finding(s) - applied a non-escalating cleanup fix pass", len(fixes))

					// The fixup landed AFTER this round's gate ran, so nothing
					// has checked it: prove it or drop it.
					o.verifyCleanupFixup(ctx, plan, round, pre, findings)
				} else {
					d.logCard(ctx, "review: approved with %d surviving finding(s), but the cleanup fix pass produced no change", len(fixes))
				}
			}

			return nil
		}

		// A fix round that committed but left this round's verify red failed as
		// surely as one that landed nothing: the next fix escalates. Round 1's
		// red verify has no fix behind it and escalates nothing, and neither
		// does a round WE truncated - a cap clears fixRan, because a round that
		// ran out of turns mid-work is the likeliest of all to leave the verify
		// red, and that event is already charged on the budget axis. Charging it
		// on the bar axis too would blame a model for turns it never got, and
		// shrink the fix pool on evidence about volume.
		if fixRan && vres.Status == verifyFailed {
			o.markFixFailed("left the verify red")
		}

		// Carry this round's findings into the next round so the panel verifies
		// their resolution without importing new scope (cross-round memory).
		o.lastFindings = findings

		if _, err := o.incrementReviewAttempt(ctx, findings); err != nil {
			return err
		}

		req := fixRequest{Findings: findings, Round: round, FixTier: fixTier}
		committed, err := o.runFix(ctx, req)

		var mte *MaxTurnsError

		// committed gates the retry the same way it gates the CI gate's capped
		// arm (see gates.go's ciFixRound): a round that pushed earns another
		// panel, because the next round has a real new diff to critique. One
		// that pushed nothing leaves HEAD exactly where round 1 left it, so a
		// retry would spend a full panel re-critiquing the same diff round 1
		// already judged - and it falls through to the park below instead.
		//
		// Keyed on the WIDTH this round actually ran at, not on the step counter:
		// the budget is clamped at the top rung, so a card whose bar already
		// seeds it there would keep buying rounds of identical width while the
		// log claimed each was wider.
		if errors.As(err, &mte) && committed && o.fixSizing(req).Budget < maxBudgetStep {
			// The round is already counted against attemptsCap, so the loop stays
			// bounded; the cap is volume evidence and the next round runs wider on
			// the same pool. Parking the whole run here would spend a round and
			// learn nothing from it.
			o.markFixCapped()
			d.logCard(ctx, "review: fix round %d hit its turn cap - retrying wider", round)

			// Cleared, not merely left alone: an earlier COMPLETED round would
			// otherwise leave this true and hand the next round's red verify to
			// the bar axis, blaming the model this cap just excused.
			fixRan = false

			continue
		}

		if err != nil {
			return err
		}

		// Only a round that COMMITTED is handed forward. A round that landed
		// nothing is judged on the spot, and its HEAD is unchanged, so the next
		// round's verify is the same subprocess against the same tree and
		// carries no new information - charging it again would jump the bar two
		// rungs on the evidence of one round.
		fixRan = committed

		if !committed {
			o.markFixFailed("produced no change")
		}
	}

	return fmt.Errorf("review exceeded the hard iteration cap of %d", hardReviewIterationCap)
}

// runReviewHITL is the HITL review loop: each round produces specialist findings
// (verify gate + 3 specialists + synthesis), records them, and presents them to
// the human, who decides. Approve -> proceed to integrate; adjust -> apply the
// findings plus the human's feedback as a fix, then re-review. The human is the
// decision-maker, so there is no authoritative pass or auto-park; the hard
// iteration cap is only a runaway guard. On a mid-run promotion the loop falls
// back to autonomous decision semantics instead: the promoted round's verdict
// decides, and a rejection lands one fix and hands the remainder to reviewLoop,
// so the attempts cap, authoritative cliff, and park-not-approve semantics all
// apply.
func (o *run) runReviewHITL(ctx context.Context, plan verifyPlan) error {
	d := o.d
	cfg := d.Cfg

	// The gate model resolves on the first classification, not up front: a card
	// promoted mid-run closes the inbox, so every gate returns without a model
	// call, and the phase would otherwise record a selection for a model that
	// never runs. Resolved once and reused across rounds; the gates run
	// sequentially from this loop, so the captured slug needs no lock.
	resolved := ""
	resolveGateModel := func(ctx context.Context) string {
		if resolved == "" {
			resolved = resolveDecisionModel(ctx, d.Registry, d.Emit, d.Ops, cfg.CardID,
				o.tc.ModelOrchestrator, cfg.PayloadModel, cfg.DefaultModel, o.excludedModels(), "review gate")
		}

		return resolved
	}

	for iter := range hardReviewIterationCap {
		round := o.tc.ReviewAttempts + iter + 1

		findings, fixTier, autoApproved, vres, fixes, err := o.reviewRound(ctx, plan, round, false)
		if err != nil {
			return err
		}

		o.recordReview(ctx, round, findings, autoApproved, vres)

		outcome, fb, gerr := o.gate(ctx, gateReviewDecision, resolveGateModel, presentFindings(findings, autoApproved))
		if gerr != nil {
			return gerr
		}

		switch outcome {
		case gateApprove:
			o.reviewSummary = findings
			// An approving verdict can now carry surviving findings, and nothing
			// on this path fixes them: the human saw them and approved anyway,
			// which is what the helper says.
			if !autoApproved || len(fixes) > 0 {
				o.reviewSummary = approvedDespiteFindings(findings)
			}

			return nil

		case gatePromoted:
			// No human decided anything - fall back to the autonomous decision
			// for the verdict already in hand (never re-run the round).
			if autoApproved {
				o.reviewSummary = findings
				if len(fixes) > 0 {
					o.reviewSummary = approvedWithOpenFindings(findings)
				}

				return nil
			}

			o.lastFindings = findings

			if _, err := d.Ops.IncrementReviewAttempts(ctx, cfg.CardID); err != nil {
				return fmt.Errorf("increment review attempts: %w", err)
			}

			if _, err := o.runFix(ctx, fixRequest{Findings: findings, Round: round, FixTier: fixTier}); err != nil {
				return err
			}

			d.logCard(ctx, "review: promoted mid-run with outstanding findings - continuing autonomously from round %d", round+1)

			// The inbox stays closed, so every later gate would return
			// gatePromoted instantly: hand the remainder to the autonomous loop
			// (attempts cap, authoritative cliff, park) and never re-enter this
			// one. iter+1 rounds ran in-process, each counted server-side.
			return o.reviewLoop(ctx, plan, iter+1)
		}

		o.lastFindings = findings

		if _, err := d.Ops.IncrementReviewAttempts(ctx, cfg.CardID); err != nil {
			return fmt.Errorf("increment review attempts: %w", err)
		}

		if _, err := o.runFix(ctx, fixRequest{Findings: mergeFeedback(findings, fb), Round: round, FixTier: fixTier}); err != nil {
			return err
		}
	}

	return fmt.Errorf("HITL review exceeded the hard iteration cap of %d", hardReviewIterationCap)
}

// presentFindings is the chat message for the review-decision gate: the
// synthesized findings plus the automated recommendation (advisory; the human
// decides).
func presentFindings(findings string, autoApproved bool) string {
	rec := "revise"
	if autoApproved {
		rec = "approve"
	}

	return "Review findings (automated recommendation: " + rec + "):\n\n" + findings +
		"\n\nApprove to integrate, or tell me what you'd like changed."
}

// approvedDespiteFindings frames the review summary for the PR body when the
// human approved integration while the automated verdict still had open
// findings, so the PR model cannot narrate them as fixed. Any path that lets an
// approval coexist with open findings must frame the summary through this
// helper or one of its two autonomous siblings below - never assign the raw
// findings text.
func approvedDespiteFindings(findings string) string {
	return "The human reviewer approved integration despite these outstanding review findings (they were presented to the reviewer but not fixed):\n\n" + findings
}

// approvedWithOpenFindings is the autonomous sibling of approvedDespiteFindings:
// the verdict approved the change while findings survived the critique round
// and nothing fixed them.
func approvedWithOpenFindings(findings string) string {
	return "The review approved the change, but these findings survived the critique round and were not fixed:\n\n" + findings
}

// approvedWithFixesApplied frames the summary when the cleanup pass landed the
// surviving findings as a fixup, so the PR model reads them as addressed rather
// than open.
func approvedWithFixesApplied(findings string) string {
	return "The review approved the change. These findings survived the critique round and were applied in a follow-up cleanup pass:\n\n" + findings
}

// cleanupVerifyContext names the gate for logVerifyGate: the run's third gate
// position, after the review round's and the pre-commit one's.
const cleanupVerifyContext = "cleanup fix pass, after the fixup"

// cleanupVerifyCorrection is the clause appended to a recorded round's verify
// line when the cleanup fixup outlives its own gate: the line the round wrote
// describes a tree the branch no longer holds.
const cleanupVerifyCorrection = "the cleanup fix pass that followed this round is still on the branch, and its own verify did not pass"

// verifyCleanupFixup re-runs the resolved verify plan over a committed cleanup
// fixup and either keeps it or resets it away. The fixup lands after the
// approving round's gate has already run, so without this the run would carry
// that round's PASSED into the PR body and the completion note while describing
// a tree the cleanup could have broken.
//
// pre is the commit the pass started from. A red gate resets the branch to it
// and reverts the summary to open findings: the cleanup pass is optional by
// construction, so dropping it restores a tree the approving round already
// verified - which is why a failure here needs no fix round, no retry and no
// park. A gate that could not report at all sits in the same position (the
// fixup is unproven) and takes the same exit. The pushed fixup outlives the
// reset on the remote until integrate's lease push overwrites the branch from
// here, which is the same overwrite every rebased run performs.
//
// Every other outcome updates the run-level result, because it describes the
// tree that will ship: a pass proves the fixup, and an inconclusive run (a
// timeout, a missing tool) proves nothing about it - both are facts about the
// fixup rather than about the tree the approving round measured. So is a red
// gate whose discard could not be performed, which is the one path that reports
// a failure it could not undo. Whenever a kept fixup did not pass, the round's
// recorded section is corrected too: the PR trailer and the card body must not
// contradict each other about the same tree.
func (o *run) verifyCleanupFixup(ctx context.Context, plan verifyPlan, round int, pre, findings string) {
	if len(plan.Argv) == 0 {
		return
	}

	vres, err := o.runVerifyPlan(ctx, o.d.Cfg.Workspace, plan)
	if err != nil {
		if o.discardCleanupFixup(ctx, pre, findings, "verify could not run - "+err.Error()) {
			return
		}

		// Kept but unproven: the note is the only evidence left of why, and it
		// reaches the PR body through the NOT VERIFIED trailer.
		vres = verifyResult{Status: verifySkipped, Note: "the cleanup fix pass could not be verified: " + err.Error()}
	} else {
		o.logVerifyGate(ctx, vres, cleanupVerifyContext)

		if vres.Status == verifyFailed && o.discardCleanupFixup(ctx, pre, findings, "verify failed") {
			return
		}
	}

	o.lastVerify = vres

	if vres.Status != verifyPassed {
		o.recordReviewVerifyCorrection(ctx, round, findings, vres)
	}
}

// discardCleanupFixup resets the branch to pre - the commit the cleanup pass
// started from - and reverts the review summary to open findings, so no
// surface narrates fixes the branch no longer carries. It reports whether the
// fixup is actually gone: an unrecorded pre-cleanup commit or a failed reset
// leaves it in place, and the caller then has to report the gate result it
// would otherwise have discarded.
func (o *run) discardCleanupFixup(ctx context.Context, pre, findings, reason string) bool {
	if pre == "" {
		o.d.logCard(ctx, "review: the cleanup fix pass could not be discarded (%s) - the pre-cleanup commit was never recorded", reason)

		return false
	}

	if err := o.d.Git.HardReset(ctx, pre); err != nil {
		o.d.logCard(ctx, "review: the cleanup fix pass could not be discarded (%s): %s", reason, err)

		return false
	}

	o.reviewSummary = approvedWithOpenFindings(findings)

	o.d.logCard(ctx, "review: %s%s - the branch is back at %s", cleanupDiscardPrefix, reason, pre)

	return true
}

// mergeFeedback folds the human's adjust feedback into the synthesized findings
// fed to the fix coder, so the fix run addresses both.
func mergeFeedback(findings, feedback string) string {
	if strings.TrimSpace(feedback) == "" {
		return findings
	}

	return findings + "\n\nADDITIONAL HUMAN FEEDBACK:\n" + feedback
}

// authoritativeReview is the gated strong pass run at the review cliff instead of
// parking on a cheap verdict: a strong, full-scope review; if it approves the
// card finishes; if it confirms real issues, ONE strong full-scope fix and one
// strong re-review; still failing → park with the strong findings. It never loops.
func (o *run) authoritativeReview(ctx context.Context, plan verifyPlan, round int) error {
	d := o.d

	// reviewLoop checked this at the top of the iteration that reached the
	// cliff; checked again here so authoritativeReview keeps its own entry
	// contract for any caller other than that cliff.
	if err := o.reviewTimeLeft(ctx, round); err != nil {
		return err
	}

	findings, fixTier, approved, vres, fixes, err := o.reviewRound(ctx, plan, round, true)
	if err != nil {
		return err
	}

	o.recordReview(ctx, round, findings, approved, vres)

	if approved {
		// Deliberately no cleanup pass here, unlike reviewLoop's approval branch:
		// this IS the cliff, so any surviving findings are simply surfaced
		// through reviewSummary (and the PR body) rather than spending another
		// strong-tier fix run right before a park. They still reach the PR body
		// framed as open, never as raw findings text.
		o.reviewSummary = findings
		if len(fixes) > 0 {
			o.reviewSummary = approvedWithOpenFindings(findings)
		}

		return nil
	}

	o.lastFindings = findings

	if _, err := o.incrementReviewAttempt(ctx, findings); err != nil {
		return err
	}

	// Gated strong fix - runs only because the authoritative review confirmed
	// real issues.
	if _, err := o.runFix(ctx, fixRequest{Findings: findings, Round: round, FixTier: fixTier, Authoritative: true}); err != nil {
		return err
	}

	// One strong re-review of the full change.
	round2 := round + 1

	findings2, _, approved2, vres2, fixes2, err := o.reviewRound(ctx, plan, round2, true)
	if err != nil {
		return err
	}

	o.recordReview(ctx, round2, findings2, approved2, vres2)

	if approved2 {
		o.reviewSummary = findings2
		if len(fixes2) > 0 {
			o.reviewSummary = approvedWithOpenFindings(findings2)
		}

		return nil
	}

	o.lastFindings = findings2

	n, err := o.incrementReviewAttempt(ctx, findings2)
	if err != nil {
		return err
	}

	// Park with the strong findings. n is the persisted counter after both
	// increments, and is the card's only visible record of how many rounds the
	// configured cap actually bought.
	d.logCard(ctx, "review parked after %d attempts (authoritative pass) - outstanding findings:\n%s", n, findings2)

	return &ReviewParkedError{Reason: reviewParkedAttemptsCap}
}

// incrementReviewAttempt calls IncrementReviewAttempts and treats
// ContextMatrix's ceiling rejection as an implicit park (ReviewParkedError)
// instead of a hard failure, so a card whose persisted counter already sits at
// the ceiling parks gracefully rather than erroring out mid-review. CM's counter
// is monotonic for the card's lifetime, so every increment site can hit it, not
// just the authoritative pass. Returns the new running total on success.
func (o *run) incrementReviewAttempt(ctx context.Context, findings string) (int, error) {
	n, err := o.d.Ops.IncrementReviewAttempts(ctx, o.d.Cfg.CardID)
	if err == nil {
		return n, nil
	}

	if errors.Is(err, cmclient.ErrReviewAttemptsCapped) {
		o.d.logCard(ctx, "review parked at server cap - outstanding findings:\n%s", findings)

		return 0, &ReviewParkedError{Reason: reviewParkedServerCap}
	}

	return 0, fmt.Errorf("increment review attempts: %w", err)
}

// reviewRound runs one review pass and returns the outstanding findings text,
// whether the work is approved, the round's verify result, the verdict's raw
// fix list, and any fatal error (budget park, transport). fixes is nil when
// no verdict ran (the verify-failed short-circuit) and carries v.Fixes -
// possibly non-empty even when approved - from whichever verdict path
// produced it otherwise; it lets the caller act on findings that survived
// approval without re-parsing the rendered findings text. The tri-state
// verify gate runs first: FAILED short-circuits to the fix loop (redacted
// output tail as the finding) WITHOUT spending reviewer tokens; SKIPPED logs
// loudly and proceeds to the specialists WITHOUT the fix loop (a
// missing/timed-out gate is not a defect to fix); PASSED (or no command)
// proceeds. On any gate outcome that reaches them, the three specialists fan
// out and the synthesis verdict decides.
func (o *run) reviewRound(ctx context.Context, plan verifyPlan, round int, authoritative bool) (findings string, fixTier string, approved bool, vres verifyResult, fixes []fix, err error) {
	// Budget gate before the verify subprocess too - the gate may be cheap, but
	// the fix run it can trigger is not, and we park before doing any work.
	if err := o.ledger.Check(); err != nil {
		return "", "", false, verifyResult{}, nil, err
	}

	if len(plan.Argv) > 0 {
		res, verr := o.runVerifyPlan(ctx, o.d.Cfg.Workspace, plan)
		if verr != nil {
			return "", "", false, verifyResult{}, nil, verr
		}

		vres = res

		o.logVerifyRound(ctx, res, round)

		switch res.Status {
		case verifyFailed:
			// Gate failure goes STRAIGHT to the fix loop without burning reviewer
			// tokens. The redacted output tail is the finding the coder fixes.
			// No verdict ran, so the fix run falls back to the card tier (empty fixTier).
			return verifyFailedFindings(plan, res.Output), "", false, vres, nil, nil
		case verifySkipped:
			// A missing or timed-out gate is inconclusive, not a defect: proceed to
			// the specialists without a fix loop. logVerifyRound already said so.
		case verifyPassed:
			// Proceed to the specialist panel.
		}
	}

	// Gate passed, skipped, or absent - the gate is a cheap pre-filter, not a
	// substitute for review, so specialists always run. With mob session
	// review on, a panel discussion replaces the specialist pass for
	// non-authoritative rounds (the authoritative pass keeps the proven solo
	// machinery); a failed discussion degrades to the fan-out below.
	if o.d.Cfg.Mob.enabled() && o.d.Cfg.Mob.Review && !authoritative {
		if v, ok := o.mobReviewVerdict(ctx); ok {
			if v.Approved {
				return strings.TrimSpace(formatFixes(v)), v.FixTier, true, vres, v.Fixes, nil
			}

			return strings.TrimSpace(formatFixes(v)), v.FixTier, false, vres, v.Fixes, nil
		}
	}

	specialistFindings, err := o.runSpecialists(ctx, authoritative)
	if err != nil {
		return "", "", false, vres, nil, err
	}

	if err := o.ledger.Check(); err != nil {
		return "", "", false, vres, nil, err
	}

	v, err := o.synthesize(ctx, specialistFindings, authoritative)
	if err != nil {
		return "", "", false, vres, nil, err
	}

	if v.Approved {
		return strings.TrimSpace(formatFixes(v)), v.FixTier, true, vres, v.Fixes, nil
	}

	return strings.TrimSpace(formatFixes(v)), v.FixTier, false, vres, v.Fixes, nil
}

// runSpecialists fans the three review lenses out as parallel read-only child
// agents over the branch diff and returns their concatenated findings. Each
// child's spend is recorded on the ledger and reported per result.
func (o *run) runSpecialists(ctx context.Context, authoritative bool) (string, error) {
	d := o.d
	cfg := d.Cfg

	// The authoritative pass is FULL scope even when a delta snapshot exists: it
	// re-widens to the base branch so the strong panel reviews the whole change,
	// not just the latest increment.
	base := o.lastReviewBase
	if base == "" || authoritative {
		base = cfg.BaseBranch
	}

	diff, err := d.Git.Diff(ctx, base)
	if err != nil {
		return "", fmt.Errorf("review diff: %w", err)
	}

	panel := o.reviewPanel(ctx, estimateTokens(diff), authoritative)
	if len(panel) == 0 {
		d.logCard(ctx, "review: no model is selectable for the reviewer role - parking the card in review for a human")

		return "", &ReviewParkedError{Reason: reviewParkedNoReviewer}
	}

	d.logCard(ctx, "review panel: %s", panelSummary(panel))

	if distinct := registry.DistinctModels(panel); distinct < 2 &&
		o.firstNote(fmt.Sprintf("panel-collapsed/%s/%d", panel[0].RequestedTier, distinct)) {
		d.logCard(ctx, "review panel collapsed to %d distinct model(s) - an approval from it is single-model evidence", distinct)
	}

	// The authoritative pass is the last thing before parking the card for a
	// human. When no seat met the bar it asked for, the pass still RUNS -
	// refusing would spend a whole run to say nothing - but the card records
	// that the verdict came from an under-powered panel.
	if authoritative && panelBelowBar(panel) && o.firstNote("authoritative-below-bar") {
		d.logCard(ctx, "authoritative review ran below its bar: no seat cleared %s, and the panel held %d distinct model(s) - the verdict stands, weigh it accordingly",
			panel[0].RequestedTier, registry.DistinctModels(panel))
	}

	lenses := []struct{ role, prompt string }{
		{"correctness", correctnessPrompt},
		{"design", designPrompt},
		{"security", securityPrompt},
	}

	// Prior findings are constant across the three lenses: the same previous-round
	// context goes to every specialist (cross-round memory). The authoritative pass
	// gets the FULL recorded history, not just the last round.
	priorText := o.lastFindings
	if authoritative {
		priorText = reviewFindingsHistory(o.body)
	}

	prior := priorFindingsBlock(priorText)

	specs := make([]harness.SubagentSpec, len(lenses))
	for i, l := range lenses {
		specs[i] = harness.SubagentSpec{
			Role: l.role,
			Prompt: fmt.Sprintf(specialistPrompt, o.skillEngage(), o.grounding, readRootsBlock(d.ReadRoots),
				l.prompt, o.tc.Title, o.taskDescription, diff, prior),
			Model:         panel[i].Model,
			MaxTurns:      cfg.MaxTurns,
			ContextWindow: panel[i].ContextWindow,
		}
	}

	// Children inherit the parent phase-run routing (harness v0.7.x
	// SubagentOpts.Provider/Reasoning): both fields derive from the same
	// builder every parent model call uses, so parent and children can never
	// drift. Only Provider/Reasoning are read from parentCfg.
	parentCfg := o.harnessConfig(cfg.DefaultModel)

	results, err := harness.SpawnSubagents(ctx, d.Client, cfg.Workspace, d.Emit, specs,
		harness.SubagentOpts{
			DefaultModel:       cfg.DefaultModel,
			ToolOutputMaxBytes: cfg.ToolOutputMax,
			RedactToolOutput:   d.Redact,
			ExtraReadOnlyTools: reviewSubagentTools(cfg.Workspace, d.ReadRoots, d.SkillTool, d.ReadRootsLog),
			Provider:           parentCfg.Provider,
			Reasoning:          parentCfg.Reasoning,
		})
	if err != nil {
		return "", fmt.Errorf("spawn review specialists: %w", err)
	}

	var b strings.Builder

	for i, res := range results {
		// The read-only specialist panel runs via harness.SpawnSubagents, which
		// exposes no per-subagent wall time, so duration is 0 (omitted on the
		// wire); phase and step still ride the report.
		o.spendAndReport(ctx, o.ledger, cfg.CardID, "review: report specialist usage failed",
			res.Result, specs[i].Model, "main", 0, "role", res.Role)

		b.WriteString("## ")
		b.WriteString(res.Role)
		b.WriteString(" findings\n")

		if res.Err != nil {
			slog.Warn("review: specialist run failed", "card_id", cfg.CardID, "role", res.Role, "error", res.Err)
			b.WriteString("(specialist run failed: " + res.Err.Error() + ")\n")
		} else {
			b.WriteString(res.Output)
			b.WriteString("\n")
		}
	}

	// Capture the reviewed head as the next round's delta base (mirrors CM's
	// review-task workflow skill, which records review_completed head=<sha>), so
	// rounds 2+ review only the change
	// since this review. Best-effort: the activity-log line is for the audit trail;
	// on crash-resume lastReviewBase starts empty and the next round re-runs full.
	if sha, herr := d.Git.Head(ctx); herr == nil && sha != "" {
		o.lastReviewBase = sha
		d.logCard(ctx, "review snapshot %s", sha)
	}

	return b.String(), nil
}

// reviewPanel returns the three specialist seats. An explicit,
// catalog-resolvable reviewer pin overrides the entire panel: all three run on
// the pinned model, and every seat after the first is flagged as a repeat so
// one model's opinion cannot read as three agreeing ones. Otherwise the
// registry selects a diverse panel for the card tier, excluding every model
// that coded a subtask on this run, and any seat that could not be served at
// that tier reports its shortfall.
func (o *run) reviewPanel(ctx context.Context, estTokens int, authoritative bool) []registry.Pick {
	// The authoritative pass escalates the panel to the complex tier so the
	// strongest models judge the change before parking.
	tier := o.cardSizing.Bar
	if authoritative {
		tier = registry.TierComplex
	}

	if resolvePin(o.d.Registry, o.tc.ModelReviewer) {
		seat := offLadderPick(o.d.Registry, o.tc.ModelReviewer, registry.RoleReviewer, tier, registry.SourcePinned)

		panel := make([]registry.Pick, reviewPanelSize)
		for i := range panel {
			panel[i] = seat
			// Every seat after the first repeats seat 1. Flagged, so the card
			// log cannot read one model's opinion as three agreeing ones.
			panel[i].Duplicate = i > 0

			// A pinned panel is still a per-seat selection, so the transcript
			// carries a line per seat exactly as the selected panel does.
			o.noteShortfall(ctx, "review panel", "", panel[i])
		}

		return panel
	}

	if o.tc.ModelReviewer != "" {
		o.warnUnresolvablePin(ctx, "reviewer", o.tc.ModelReviewer)
	}

	panel := o.d.Registry.SelectReviewPanel(registry.SelectInput{
		Role:      registry.RoleReviewer,
		Tier:      tier,
		EstTokens: estTokens,
		// Exclude both the models that coded this run (a model must not review its
		// own work) and any model proven harness-incapable this run (recoverIncapable
		// records it). Merged so neither set masks the other.
		Exclude: o.reviewExclusions(),
	}, reviewPanelSize)

	// One label for the whole panel: seats that fell the same distance are one
	// fact, and the panel summary below carries the per-seat detail.
	for _, p := range panel {
		o.noteShortfall(ctx, "review panel", "", p)
	}

	return panel
}

// reviewExclusions is the union of the coder models (a model must not review its
// own code) and the per-card incapable set (models that could not drive the tool
// loop). Both feed the review panel's diversity Exclude so neither is re-picked.
func (o *run) reviewExclusions() map[string]bool {
	excl := make(map[string]bool, len(o.coderModels)+len(o.excluded))
	for m := range o.coderModels {
		excl[m] = true
	}

	for m := range o.excluded {
		excl[m] = true
	}

	return excl
}

// mobFallback records a mob-review degradation and returns the no-verdict
// result that sends the round to the solo fan-out. It logs to the card, not
// just the transcript: a review that quietly changed shape otherwise looks
// identical to one that did not.
func (o *run) mobFallback(ctx context.Context, reason string, err error) (verdict, bool) {
	if err != nil {
		slog.Warn("mob review: "+reason+"; solo fallback", "card_id", o.d.Cfg.CardID, "error", err)
	} else {
		slog.Warn("mob review: "+reason+"; solo fallback", "card_id", o.d.Cfg.CardID)
	}

	o.d.logCard(ctx, "review: mob discussion unavailable (%s) - falling back to the solo specialist panel", reason)

	return verdict{}, false
}

// mobReviewVerdict convenes the review discussion and parses its synthesis
// into the existing verdict shape, with ONE moderator repair on a parse
// failure (mirroring synthesize's repair turn). ok=false on any failure -
// the caller falls back to the specialist fan-out (see mobFallback). On
// success it records the review snapshot head, exactly like runSpecialists -
// the mob briefing itself never uses it, but a later round that falls back
// to the solo fan-out does.
func (o *run) mobReviewVerdict(ctx context.Context) (verdict, bool) {
	briefing, err := o.mobReviewBriefing(ctx)
	if err != nil {
		return o.mobFallback(ctx, "briefing failed", err)
	}

	seats := min(o.d.Cfg.Mob.Participants, len(reviewLenses))

	t := mob.Topic{
		Kind:     "review",
		Briefing: briefing,
		Lenses:   reviewLenses[:seats],
		Rounds:   1,
		Blind:    true,
		SynthesisPrompt: fmt.Sprintf(reviewSynthesisPrompt,
			o.grounding, o.tc.Title, o.taskDescription),
	}

	out, ok := o.mobDiscuss(ctx, t)
	if !ok {
		return o.mobFallback(ctx, "discussion failed", nil)
	}

	v, perr := parseVerdict(out.Synthesis)
	if perr != nil {
		repaired, rerr := o.mobResynthesize(ctx, t, out, perr.Error())
		if rerr != nil {
			return o.mobFallback(ctx, "repair synthesis failed", rerr)
		}

		v, perr = parseVerdict(repaired)
		if perr != nil {
			return o.mobFallback(ctx, "verdict parse failed after repair", perr)
		}
	}

	if sha, herr := o.d.Git.Head(ctx); herr == nil && sha != "" {
		o.lastReviewBase = sha
		o.d.logCard(ctx, "review snapshot %s", sha)
	}

	return v, true
}

// mobReviewBriefing assembles the discussion briefing: the branch diff against
// the base branch plus the prior round's findings.
//
// The diff is never scoped to a review snapshot. A fix can land code outside
// the delta it targeted, and every mob round after the first follows a fix, so
// a snapshot-scoped briefing would hide exactly the code the round exists to
// re-examine - a seat reported this at runtime, saying the findings cited
// symbols absent from its diff. The solo fan-out still scopes to the snapshot
// on those rounds (it re-widens only on the authoritative pass and after a
// no-op fix), so it carries the same gap; closing it there is separate work.
func (o *run) mobReviewBriefing(ctx context.Context) (string, error) {
	diff, err := o.d.Git.Diff(ctx, o.d.Cfg.BaseBranch)
	if err != nil {
		return "", fmt.Errorf("review diff: %w", err)
	}

	prior := priorFindingsBlock(o.lastFindings)

	return fmt.Sprintf(reviewBriefing, o.grounding, readRootsBlock(o.d.ReadRoots),
		o.tc.Title, o.taskDescription, fencedDiff(diff), prior), nil
}

// synthesize runs ONE orchestrator-model call that reads the three specialists'
// findings and emits the structured verdict. The verdict JSON is parsed with the
// same extractJSON + one repair turn the planner uses.
func (o *run) synthesize(ctx context.Context, findings string, authoritative bool) (verdict, error) {
	d := o.d
	cfg := d.Cfg

	model := resolveDecisionModel(ctx, d.Registry, d.Emit, d.Ops, cfg.CardID,
		o.tc.ModelOrchestrator, cfg.PayloadModel, cfg.DefaultModel, o.excludedModels(), "review synthesis")

	var (
		v       verdict
		lastErr error
	)

	for attempt := range 2 {
		if err := o.ledger.Check(); err != nil {
			return verdict{}, err
		}

		repair := ""
		if attempt > 0 {
			repair = repairBlock(lastErr.Error())
		}

		priorText := o.lastFindings
		if authoritative {
			priorText = reviewFindingsHistory(o.body)
		}

		prior := priorFindingsBlock(priorText)

		task := fmt.Sprintf(synthesisPrompt, o.grounding, o.tc.Title, o.taskDescription, prior, findings, repair)

		res, dur, err := o.runModel(ctx, d.ReadTools, task, model)

		o.spendAndReport(ctx, o.ledger, cfg.CardID, "review: report synthesis usage failed", res, model, "main", dur)

		if err != nil {
			return verdict{}, fmt.Errorf("synthesis run: %w", err)
		}

		v, lastErr = parseVerdict(res.Output)
		if lastErr == nil {
			return v, nil
		}

		slog.Warn("review: verdict parse failed", "card_id", cfg.CardID, "attempt", attempt, "error", lastErr)
	}

	return verdict{}, fmt.Errorf("verdict parse failed after repair: %w", lastErr)
}

// runFixModel runs the fix coder harness with the same in-run incapable recovery
// as the subtask coder: it resolves the fix model (skipping the per-card exclude
// set), logs the pick, runs, and accounts for spend each attempt. An incapable
// model is blacklisted/excluded via recoverIncapable and the next-best fix model
// re-selected for the SAME round; the cap (shared with the coder path via
// o.reselects) parks the run when exhausted. A non-incapable run error returns
// immediately. The successful run's output is consumed inside the harness loop
// (the fixup targets files parsed from the findings, not the model output), so
// it returns only the model that ran and the error.
func (o *run) runFixModel(ctx context.Context, prompt string, req fixRequest) (string, error) {
	d := o.d
	cfg := d.Cfg
	fs := o.fixSizing(req)

	for attempt := 0; attempt <= reselectCap; attempt++ {
		p, rerr := o.resolveFixModel(ctx, req)
		if rerr != nil {
			return "", rerr
		}

		model := p.Model

		o.noteShortfall(ctx, "fix coder", "", p)

		switch {
		case o.fixBarSteps > 0:
			d.logCard(ctx, "fix coder %s selected for round %d fixes (bar=%s, turns=%s) - escalated after a fix round that %s",
				model, req.Round, fs.Bar, budgetLabel(fs.Budget), o.fixFailReason)
		case o.fixBudgetSteps > 0:
			d.logCard(ctx, "fix coder %s selected for round %d fixes (bar=%s, turns=%s) - widened after a fix round that %s",
				model, req.Round, fs.Bar, budgetLabel(fs.Budget), o.fixCapReason)
		default:
			d.logCard(ctx, "fix coder %s selected for round %d fixes (bar=%s, turns=%s)",
				model, req.Round, fs.Bar, budgetLabel(fs.Budget))
		}

		// The escalated round exists to reach a STRONGER model after a failure.
		// When the climb does not clear the bar it climbed to, it bought
		// nothing; the round still runs on a pick that is at least fresh, but
		// the no-op is no longer invisible.
		if o.fixBarSteps > 0 && !p.AtBar() && p.Source != registry.SourcePinned &&
			o.firstNote("fix-escalation-noop/"+string(fs.Bar)+"/"+model) {
			d.logCard(ctx, "fix escalation to %s bought nothing - %s does not clear it (%s); running anyway",
				fs.Bar, model, priorClause(p))
		}

		res, dur, err := o.runModelCoder(ctx, d.WriteTools, prompt, model, fixWrapUpMessage, fs.Budget)

		o.spendAndReport(ctx, o.ledger, cfg.CardID, "review: report fix usage failed", res, model, "main", dur)

		// One row per ATTEMPT, on the same footing as the coder loop's. Three
		// call paths reach here: a review round and a gate round fix findings
		// across the card and leave the unit unset, while the pre-commit verify
		// fix is scoped to one subtask and names it. The estimate is chosen by
		// that SCOPE rather than by an empty request field, so a subtask whose
		// planner word could not be recovered reports no estimate instead of
		// borrowing the card's - the pair only means anything when both halves
		// describe the same unit.
		plannerBar := o.cardPlannerBar
		if req.Subtask != "" {
			plannerBar = req.PlannerBar
		}

		maxTurns, wrapUp := coderTurnCfg(cfg.MaxTurns, fs.Budget)

		o.emitSizingObs(sizingObs{
			Phase: o.curPhase, Solver: "fix", Subtask: req.Subtask, Reselect: attempt,
			Model: model, Bar: string(fs.Bar), BudgetStep: fs.Budget,
			PlannerBar: plannerBar, MaxTurns: maxTurns, WrapUpTurns: wrapUp,
			Turns: res.Turns, Outcome: sizingOutcome(err, res.Turns, maxTurns), DurationMS: dur.Milliseconds(),
		})

		var ie *IncapableError
		if errors.As(err, &ie) {
			if rerr := o.recoverIncapable(ctx, ie); rerr != nil {
				return "", rerr
			}

			continue
		}

		if err != nil {
			// The slug rides out with the error: the caller attributes the round
			// to the model that ran it, and a failed round that reported no model
			// leaves the exclusion set empty forever.
			return model, fmt.Errorf("fix run: %w", err)
		}

		return model, nil
	}

	// Unreachable: recoverIncapable errors at the cap before the loop exhausts.
	return "", fmt.Errorf("review fix (card=%s): re-selection loop exhausted", o.d.Cfg.CardID)
}

// runFix runs one coder fix pass against the outstanding findings, lands the
// changes as a fixup onto the commit that last touched the fixed files (HEAD
// fallback), and pushes. Budget is checked before the model call. It reports
// whether the fix landed a commit - a round that landed nothing is a failed
// round to the review loop - and returns a turn cap ALONGSIDE that verdict, so
// the caller learns the round was truncated without the round's edits being
// thrown away. After a failed round it parks instead of running when no fix
// model other than the ones that failed is available; a request that says
// NoEscalate is exempt from that park.
func (o *run) runFix(ctx context.Context, req fixRequest) (bool, error) {
	d := o.d
	cfg := d.Cfg
	findings := req.Findings

	// Keyed on the BAR counter alone: a widened budget is not a reason to refuse
	// to re-run the same model - running it wider is precisely the correction.
	if !req.NoEscalate && o.fixBarSteps > 0 {
		p, rerr := o.resolveFixModel(ctx, req)
		if rerr != nil || o.fixFailed[p.Model] {
			d.logCard(ctx, "review parked: the previous fix round %s and no other fix model is available (tried: %s) - outstanding findings:\n%s",
				o.fixFailReason, strings.Join(sortedKeys(o.fixFailed), ", "), findings)

			if rerr != nil {
				return false, fmt.Errorf("resolve fix model: %w", rerr)
			}

			return false, &ReviewParkedError{Reason: reviewParkedFixExhausted}
		}
	}

	if err := o.ledger.Check(); err != nil {
		return false, err
	}

	var prompt string
	if strings.HasPrefix(findings, verifyFailedPrefix) {
		// No reviewer ran: the finding is the one failing command, not a critique
		// of the whole card, so the parent card goes in title-only with an
		// explicit scope block instead of the full description.
		prompt = fmt.Sprintf(verifyFixPrompt, o.skillEngage(), o.grounding, cfg.Workspace,
			fixVerifyLine(o.resolvedVerifyPlan()), o.tc.Title, findings)
	} else {
		prompt = fmt.Sprintf(fixPrompt, o.skillEngage(), o.grounding, cfg.Workspace,
			fixVerifyLine(o.resolvedVerifyPlan()), o.tc.Title, o.taskDescription, findings)
	}

	model, ferr := o.runFixModel(ctx, prompt, req)

	// A cap is not a reason to throw the round's edits away. The model ran out of
	// room, but what it wrote is the only evidence the round produced, and
	// leaving it uncommitted in the tree makes any retry the caller attempts
	// unsound - the next round would start from a dirty workspace it did not
	// write. Fall through to the fixup and the push, and return the cap
	// alongside whether anything landed.
	var mte *MaxTurnsError

	if ferr != nil && !errors.As(ferr, &mte) {
		return false, ferr
	}

	// The slug is only ever recorded, never cleared: the next markFixFailed keys
	// the exclusion set off it, and a value wiped to empty means the failed model
	// is never excluded and the exhausted-fixers park can never trip. Guarded
	// here rather than trusting every runFixModel return path, because that
	// coupling is what produced the defect.
	if model != "" {
		o.lastFixModel = model
	}

	// Target the commit that last touched the fixed files so the fixup autosquashes
	// onto the right change; HEAD is the fallback when the path lookup yields
	// nothing (untracked files, or no path match).
	target, lerr := d.Git.LastCommitTouching(ctx, fixFiles(findings))
	if lerr != nil || target == "" {
		target = "HEAD"
	}

	committed, err := d.Git.CommitFixup(ctx, target)
	if err != nil {
		return false, fmt.Errorf("commit review fixup: %w", err)
	}

	if !committed {
		// The fix produced no commit: HEAD is unchanged, so the reviewed-head
		// snapshot captured this round would make the NEXT round diff HEAD...HEAD
		// (an empty delta). An empty-delta panel sees nothing to critique and can
		// approve, integrating the card with the unresolved finding and bypassing
		// the authoritative pass. Drop the snapshot so the next round re-widens to
		// the full base-branch diff and actually re-examines the outstanding work.
		o.lastReviewBase = ""

		return false, ferr
	}

	if err := d.Git.Push(ctx, cfg.Branch); err != nil {
		return false, fmt.Errorf("push review fixup: %w", err)
	}

	return true, ferr
}

// markFixFailed records that the most recent fix round failed for the given
// reason: its model is excluded from every later fix pick, and the next pick
// climbs one bar rung.
func (o *run) markFixFailed(reason string) {
	if o.fixFailed == nil {
		o.fixFailed = map[string]bool{}
	}

	if o.lastFixModel != "" {
		o.fixFailed[o.lastFixModel] = true
	}

	o.fixBarSteps++
	o.fixFailReason = reason
}

// fixExclusions is the fix pick's exclusion set: models proven incapable this
// run plus models whose fix round failed.
func (o *run) fixExclusions() map[string]bool {
	out := make(map[string]bool, len(o.excluded)+len(o.fixFailed))
	maps.Copy(out, o.excluded)
	maps.Copy(out, o.fixFailed)

	return out
}

// fixFailedVendors is the set of vendors behind the failed fix models, for the
// escalated pick's vendor preference.
func (o *run) fixFailedVendors() map[string]bool {
	out := map[string]bool{}

	for model := range o.fixFailed {
		if v := o.d.Registry.Vendor(model); v != "" {
			out[v] = true
		}
	}

	return out
}

// sortedKeys lists a set's members in a stable order for log lines.
func sortedKeys(set map[string]bool) []string {
	keys := slices.Collect(maps.Keys(set))
	slices.Sort(keys)

	return keys
}

// resolveFixModel picks the coder model for the fix run: the card's coder pin
// when catalog-resolvable, else the best-value coder selection at this round's
// fix bar (see fixSizing). Once the bar has climbed, the pick also prefers a
// vendor that has not failed this card - unless the request declines to
// escalate, which opts a call site out of the whole correction, the vendor
// preference included.
func (o *run) resolveFixModel(ctx context.Context, req fixRequest) (registry.Pick, error) {
	tier := o.fixSizing(req).Bar

	if resolvePin(o.d.Registry, o.tc.ModelCoder) {
		// A pinned model is returned even if it is in o.excluded: we never override
		// an explicit operator pin with an auto-selected substitute. A pinned model
		// that is harness-incapable therefore keeps being re-selected, exhausts the
		// re-selection cap, and parks - the blacklist still records it.
		return offLadderPick(o.d.Registry, o.tc.ModelCoder, registry.RoleCoder, tier, registry.SourcePinned), nil
	}

	if o.tc.ModelCoder != "" {
		o.warnUnresolvablePin(ctx, "coder", o.tc.ModelCoder)
	}

	in := registry.SelectInput{
		Role:    registry.RoleCoder,
		Tier:    tier,
		Exclude: o.fixExclusions(),
	}

	if req.NoEscalate || o.fixBarSteps == 0 {
		return o.employableFixPick(o.d.Registry.SelectByComplexity(in))
	}

	// Escalating after a failed round: prefer a vendor that has not failed this
	// card, and fall back to any vendor when that leaves only a failed model.
	in.ExcludeVendors = o.fixFailedVendors()
	if p := o.d.Registry.SelectByComplexity(in); p.OK && !o.fixFailed[p.Model] {
		return p, nil
	}

	in.ExcludeVendors = nil

	return o.employableFixPick(o.d.Registry.SelectByComplexity(in))
}

// employableFixPick converts the selector's refusal into a review park. The
// asymmetry with the coder path is deliberate: no coder means there is no work
// at all, while the code a fix round would touch is already written and
// pushed, so a human taking the card FROM REVIEW is the right destination.
func (o *run) employableFixPick(p registry.Pick) (registry.Pick, error) {
	if !p.OK {
		return registry.Pick{}, &ReviewParkedError{Reason: reviewParkedNoFixModel}
	}

	return p, nil
}

// fixRequest is one fix round's inputs. A struct rather than four positional
// arguments because two of them are booleans, and Round is meaningful to three
// different callers who count different things.
type fixRequest struct {
	Findings string
	Round    int
	// FixTier is the synthesizer's fix_tier for this round. Empty falls back to
	// the card bar, so the fixer is never under-sized.
	FixTier string
	// Subtask and PlannerBar name the unit a SUBTASK-SCOPED round runs on: the
	// pre-commit verify fix is sized on one subtask's bar, not the card's, so
	// its measurement row has to report that subtask's estimate rather than the
	// card's. Both are empty on a card-scoped round (a review round or a gate
	// round), which is what makes the round's scope readable off the row.
	Subtask       string
	PlannerBar    string
	Authoritative bool
	// NoEscalate marks a call site that must neither climb the counters nor be
	// refused by the exhausted-fixers park: the post-approval cleanup pass runs
	// at the un-escalated sizing on a card that has already been approved, and
	// must still run on a card whose earlier rounds failed.
	NoEscalate bool
}

// fixBarBase is the bar this round starts from, before any correction. The
// authoritative pass is floored at complex: it is the last chance before a
// park, and a stated policy constant is legitimate on a JUDGMENT-shaped call
// where nothing in the run measures the right answer.
func (o *run) fixBarBase(req fixRequest) registry.Tier {
	if req.Authoritative {
		return registry.TierComplex
	}

	if req.FixTier == "" {
		return o.cardSizing.Bar
	}

	return tierFromString(req.FixTier)
}

// fixSizing is the bar and budget this fix round runs at: the base bar climbed
// once per failed round, and a budget seeded from the bar the round ACTUALLY
// RUNS AT, then widened once per capped round.
//
// The budget is seeded from the fix bar rather than from the card's, which
// reproduces what the pre-split code did. Taking it from the card would drop the
// authoritative pass - floored at complex, and the one run where exhausting the
// turns ends the run rather than deferring it - back to the base on every
// moderate card.
//
// The two axes stay separate in the direction that matters: a turn cap widens
// the budget and never touches the bar, so running out of room can never buy a
// more expensive model. The reverse direction is this system's own seeding rule
// - seedSizing derives the budget from the bar, which is why a complex card
// opens on a wider window in the first place - so a climbed bar carries its
// window with it.
func (o *run) fixSizing(req fixRequest) sizing {
	base := o.fixBarBase(req)
	s := sizing{Bar: base, Budget: seedBudgetStep(base)}

	if req.NoEscalate {
		return s
	}

	for range o.fixBarSteps {
		s = s.raiseBar()
	}

	// Re-seed from the climbed bar. raiseBar deliberately leaves the budget
	// alone - other callers depend on that - so without this a failed round
	// would hand the harder bar the base window and ask it to do the harder
	// work inside it. Widening only, and applied BEFORE the cap steps so those
	// land on top of the seed rather than instead of it.
	s.Budget = max(s.Budget, seedBudgetStep(s.Bar))

	for range o.fixBudgetSteps {
		s = s.raiseBudget()
	}

	return s
}

// markFixCapped records that the most recent fix round ran out of turns,
// without adding the model to fixFailed - it ran out of room, not shown to be
// too weak. fixCapReason (kept separate from fixFailReason) keeps the card
// log from reporting a cap as a bar escalation or an exhausted fix pool.
func (o *run) markFixCapped() {
	o.fixBudgetSteps++
	o.fixCapReason = "hit its turn cap"
}

// reviewFindingsHistory returns every "## Review Findings" section recorded on
// the parent body, concatenated - the full prior-findings context for the
// authoritative pass. Empty when none have been recorded yet.
func reviewFindingsHistory(body string) string {
	return strings.TrimSpace(sectionsWithPrefix(body, "Review Findings"))
}

// severityNit is the one severity that does not earn a cleanup fix pass.
const severityNit = "nit"

// validSeverities is the vocabulary the synthesis prompts ask for (see
// specialistPrompt and synthesisPrompt/reviewSynthesisPrompt in prompts.go).
var validSeverities = map[string]bool{
	"critical":  true,
	"important": true,
	"minor":     true,
	severityNit: true,
}

// worthCleanupPass reports whether a surviving fix list earns a fix-coder run,
// a fixup commit and a push on a change that already cleared review. A verdict
// carrying nothing but nits does not - they are reported in the summary and
// left alone. An unlabelled finding counts as actionable: a model that omits
// severity must not have its findings silently demoted into the discard this
// field exists to end.
func worthCleanupPass(fixes []fix) bool {
	for _, f := range fixes {
		if f.Severity != severityNit {
			return true
		}
	}

	return false
}

// normalizeSeverity lower-cases and trims s, returning "" for anything outside
// validSeverities, so the rendered tag is always one of four known words and
// worthCleanupPass can decide on it.
func normalizeSeverity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !validSeverities[s] {
		return ""
	}

	return s
}

// collapseLines folds newlines in model-authored fix text into spaces. Every
// field on a fix reaches findings text that formatFixes renders one line per
// finding and fixFiles re-parses line-by-line, cutting at the first colon: an
// embedded newline in any of them would inject a synthetic fix line naming a
// file no reviewer raised, which the fix run would then target.
func collapseLines(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s))
}

// parseVerdict extracts the synthesis verdict JSON (tolerating prose / code
// fences) and unmarshals it. A missing object or malformed JSON is an error so
// the synthesis caller can take its single repair turn. It is the single choke
// point for the verdict type - mobReviewVerdict and synthesize both route
// through it - so normalizing Severity here covers both the mob and solo
// review paths with one call site. parseCheckpointVerdict (checkpoint.go) is
// deliberately NOT covered: its fixes reach neither fixFiles nor the PR body,
// and its File/Issue/Suggestion are equally unvalidated, so validating only
// its severity would be inconsistent.
func parseVerdict(s string) (verdict, error) {
	raw, ok := extractJSON(s)
	if !ok {
		return verdict{}, fmt.Errorf("no JSON object found in synthesis output")
	}

	var v verdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return verdict{}, fmt.Errorf("unmarshal verdict JSON: %w", err)
	}

	for i := range v.Fixes {
		v.Fixes[i].Severity = normalizeSeverity(v.Fixes[i].Severity)
		v.Fixes[i].File = collapseLines(v.Fixes[i].File)
		v.Fixes[i].Issue = collapseLines(v.Fixes[i].Issue)
		v.Fixes[i].Suggestion = collapseLines(v.Fixes[i].Suggestion)
	}

	return v, nil
}

// formatFixes renders a verdict's fix list as the findings text carried into the
// fix run and (on cap exhaustion) the activity log. The
// "- <file>: [<severity>] <issue> - <suggestion>" line shape - severity's
// bracket is omitted entirely when empty - is a contract with fixFiles, which
// parses the file path back out for fixup targeting - keep the two in sync.
func formatFixes(v verdict) string {
	var b strings.Builder

	if v.Summary != "" {
		b.WriteString(v.Summary)
		b.WriteString("\n")
	}

	for _, f := range v.Fixes {
		b.WriteString("- ")
		b.WriteString(f.File)
		b.WriteString(": ")

		if f.Severity != "" {
			b.WriteString("[")
			b.WriteString(f.Severity)
			b.WriteString("] ")
		}

		b.WriteString(f.Issue)

		if f.Suggestion != "" {
			b.WriteString(" - ")
			b.WriteString(f.Suggestion)
		}

		b.WriteString("\n")
	}

	return b.String()
}

// fixFiles extracts the file paths referenced in the findings text so the fixup
// can target the commit that last touched them. It parses the "- <file>: ..."
// line shape formatFixes emits (mirror - keep the two in sync); lines without a
// leading path are ignored.
func fixFiles(findings string) []string {
	var (
		files []string
		seen  = map[string]bool{}
	)

	for line := range strings.SplitSeq(findings, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}

		rest := strings.TrimPrefix(trimmed, "- ")

		path, _, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}

		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}

		seen[path] = true
		files = append(files, path)
	}

	return files
}

// tierFromString maps a planner card-tier string to a registry.Tier. An empty
// or unrecognised value defaults to moderate (conservative: under-selecting a
// reviewer is worse than slightly over-paying).
func tierFromString(tier string) registry.Tier {
	switch tier {
	case "simple":
		return registry.TierSimple
	case "complex":
		return registry.TierComplex
	case "critical":
		return registry.TierCritical
	default:
		return registry.TierModerate
	}
}

// reviewSubagentTools is what the specialist panel gets on top of the
// workspace-confined read-only set harness.SpawnSubagents registers for it: the
// optional Skill tool, plus read, grep and glob widened to the operator's
// declared read-only roots. SpawnSubagents builds its child registry as
// tools.NewRegistry(append(tools.ReadOnlyTools(root), extras...)...), and
// NewRegistry keeps the LAST registration for a duplicate name, so the widened
// three replace the confined ones without a harness change. With no roots
// declared the panel gets exactly what it got before. rrLog is the run's
// shared read-roots tracker (Deps.ReadRootsLog); nil is safe (logs without
// dedup) for callers with none to hand in.
func reviewSubagentTools(workspace string, roots []string, skill tools.Tool, rrLog *ReadRootsLog) []tools.Tool {
	out := skillToolSlice(skill)
	if len(roots) == 0 {
		return out
	}

	// The read, grep and glob tools built here from the same (workspace, roots)
	// all sanitize identically, so only the read tool's outcome needs logging.
	readTool := tools.NewReadTool(workspace).WithReadRoots(roots)
	rrLog.Log("", workspace, readTool.ReadRoots())

	return append(out,
		readTool,
		tools.NewGrepTool(workspace).WithReadRoots(roots),
		tools.NewGlobTool(workspace).WithReadRoots(roots),
	)
}

// skillToolSlice wraps an optional Skill tool as a SubagentOpts.ExtraReadOnlyTools
// slice. Nil tool → nil slice.
func skillToolSlice(t tools.Tool) []tools.Tool {
	if t == nil {
		return nil
	}

	return []tools.Tool{t}
}

// panelSummary renders a review panel for the card log. Three identical slugs
// printed bare are indistinguishable from three independent judgements, and
// the synthesizer reads agreement as signal - so each seat carries the bar it
// actually met, whether it fell short of the bar the panel asked for, whether
// it came from an operator pin, and whether it merely repeats the seat above.
func panelSummary(panel []registry.Pick) string {
	seats := make([]string, 0, len(panel))

	for _, p := range panel {
		var seat string

		switch {
		case p.Source == registry.SourcePinned:
			seat = p.Model + " (pinned)"
		case p.AtBar():
			seat = p.Model + "@" + string(p.MetTier)
		default:
			seat = fmt.Sprintf("%s@%s (below %s)", p.Model, metTierLabel(p), p.RequestedTier)
		}

		if p.Duplicate {
			seat += " (repeat)"
		}

		seats = append(seats, seat)
	}

	return strings.Join(seats, ", ")
}

// panelBelowBar reports that not one seat cleared the tier the panel asked
// for. An operator pin exempts the panel: a pin is an explicit choice carrying
// no measured prior, so it has no shortfall to report.
func panelBelowBar(panel []registry.Pick) bool {
	for _, p := range panel {
		if p.Source == registry.SourcePinned || p.AtBar() {
			return false
		}
	}

	return true
}
