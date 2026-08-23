package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// estimateTokens approximates the prompt budget for window fitting: chars/4
// (the rough bytes-per-token rule) plus a fixed overhead covering the system
// prompt, the tool schemas, and headroom for the conversation that follows.
func estimateTokens(prompt string) int { return len(prompt)/4 + 24000 }

// solverCtx carries the collaborators one implementation attempt writes
// through. The parent run's solver targets the main workspace and the board;
// Best-of-N candidates target a worktree and stay off the board.
type solverCtx struct {
	git        GitOps
	ledger     *Ledger
	tools      *tools.Registry
	workspace  string
	coderModel func(ctx context.Context, sub subtaskRef, prompt string) string
	boardOps   bool         // false: no subtask claim/heartbeat/complete (candidate mode)
	push       bool         // false: never push (candidate mode)
	tag        string       // "" parent; "candidate 2/3 (slug)" for candidate log lines
	completed  []subtaskRef // subtasks this solver actually executed
	lastSubID  string       // final subtask ID in execution order; "" disables turn-cap salvage (parent/single-solver)
	capped     bool         // the final subtask hit the turn cap; its work was salvage-committed for judge verification
}

// runExecute is the execute phase: subtasks run SEQUENTIALLY in dependency
// order over a single shared workspace (no parallel writers). Each subtask gets
// a fresh-context coder harness with the full write toolset; code commits and
// pushes after every subtask. The budget ledger is checked before every
// model-bearing step. The parent run drives its solver (o.solver), bound in
// newRun to the main workspace, run ledger, and the board.
func runExecute(ctx context.Context, o *run) error {
	// Resolve the verify plan once at execute entry (the first phase to reach the
	// gate on a fresh run), so the resolution log fires early and the coder prompt
	// can name the command. A budget park during the proposal tier propagates.
	if _, err := o.ensureVerify(ctx); err != nil {
		return err
	}

	// Best-of-N replaces the single-solver execute with a candidate fan-out: N
	// implementations of the shared plan race in isolated worktrees, off the board
	// and never pushing, and a later phase judges them.
	if o.d.Cfg.BestOfN >= 2 {
		return o.runFanout(ctx)
	}

	ordered, err := topoOrder(o.subtasks)
	if err != nil {
		return fmt.Errorf("order subtasks: %w", err)
	}

	for _, sub := range ordered {
		if err := o.executeSubtaskWith(ctx, o.solver, sub); err != nil {
			return err
		}
	}

	return nil
}

// executeSubtaskWith runs one subtask end to end through the given solver:
// skip-if-done, budget check, claim, model resolution, coder harness run, usage
// accounting, commit, push, complete. Board ops (claim/heartbeat/complete/
// release) run only when sc.boardOps and pushes only when sc.push, so a
// Best-of-N candidate solver stays off the board and never pushes.
func (o *run) executeSubtaskWith(ctx context.Context, sc *solverCtx, sub subtaskRef) error {
	d := o.d

	// Resume: a terminal subtask (done or not_planned) is not re-run.
	if isTerminal(sub.State) {
		if sub.State == "not_planned" {
			slog.Info("execute: cancelled subtask, not re-run", "card_id", sub.ID)
		} else {
			slog.Info("execute: skipping completed subtask", "card_id", sub.ID)
		}

		return nil
	}

	// Budget gate BEFORE claiming, so a parked subtask is never owned. A
	// candidate solver checks its own sub-ledger AND the run ledger:
	// server-priced totals sync only into the run ledger, so a run-wide breach
	// must park the fan-out here rather than at the next phase boundary.
	if err := sc.ledger.Check(); err != nil {
		return err
	}

	if sc.ledger != o.ledger {
		if err := o.ledger.Check(); err != nil {
			return err
		}
	}

	// Claim conflicts mean another agent owns the subtask - abort the run rather
	// than skip, because the workspace is shared and we cannot safely proceed
	// without ownership of the card we are about to build on. A candidate solver
	// (boardOps false) never claims per-candidate; instead the RUN claims each
	// subtask once when the first candidate reaches it, so the board shows
	// in_progress during the race without N writers colliding.
	if sc.boardOps {
		if err := d.Ops.ClaimCard(ctx, sub.ID); err != nil {
			return fmt.Errorf("claim subtask %s: %w", sub.ID, err)
		}
	} else {
		o.claimSubtaskOnce(ctx, sub)
	}

	if err := o.executeClaimedWith(ctx, sc, sub); err != nil {
		// The run is aborting (or parking) while we still own the subtask:
		// release it, or it stays claimed until CM's stall sweep mislabels a
		// deliberately-parked run as crashed 30 minutes later. A candidate holds
		// no claim, so there is nothing to release.
		if sc.boardOps {
			o.releaseSubtask(ctx, sub.ID)
		}

		return err
	}

	sc.completed = append(sc.completed, sub)

	return nil
}

// subtaskHeartbeatInterval matches the worker's parent-card cadence (5m against
// CM's default 30m heartbeat_timeout). A var so tests can shrink it.
var subtaskHeartbeatInterval = 5 * time.Minute

// executeClaimedWith is the owned span of a subtask: coder run, commit, push,
// complete. When sc.boardOps a heartbeat goroutine covers the whole span - CM's
// stall sweep reclaims ANY claimed card whose last_heartbeat exceeds the
// timeout, the parent-card heartbeat does not cover subtask claims, and a coder
// run is wall-clock unbounded. The deferred stop cancels the goroutine AND waits
// for it to actually exit on every exit path (complete, error, park), so it can
// never outlive the claim - or this function's return. A candidate solver
// (boardOps false) holds no claim, so it runs no heartbeat and no complete.
func (o *run) executeClaimedWith(ctx context.Context, sc *solverCtx, sub subtaskRef) error {
	d := o.d

	// Snapshot the ledger before this subtask's own spend, so every solo
	// outcome report below carries only THIS subtask's cost delta - not the
	// run's cumulative total, which would inflate every later subtask's row
	// with every earlier subtask's spend (and, on a resumed run, with whole
	// prior sessions' spend too). Known limit: Spent() reconciles local and
	// server-reported totals via max(), not an additive meter, so against a
	// cost-less gateway a resumed run's deltas can under-report (down to 0).
	// Accepted: the result is the signal selection needs; CostUSD is advisory.
	spendBefore := sc.ledger.Spent()

	if sc.boardOps {
		stopHeartbeat := startSubtaskHeartbeat(ctx, d.Ops, sub.ID)
		defer stopHeartbeat()
	}

	// Capture the pre-run head when this subtask will checkpoint, so the
	// discussion sees exactly the diff this subtask introduced. Solo path
	// only - candidates never checkpoint (race isolation).
	var checkpointBase string

	if sc.boardOps && o.checkpointEligible(sub) {
		if head, herr := sc.git.Head(ctx); herr == nil {
			checkpointBase = head
		} else {
			slog.Warn("mob checkpoint: head read failed; skipping this checkpoint",
				"card_id", o.d.Cfg.CardID, "subtask_id", sub.ID, "error", herr)
		}
	}

	prompt := fmt.Sprintf(coderPrompt, o.skillEngage(), o.grounding, sc.workspace,
		verifyCommandBlock(o.resolvedVerifyPlan()), sub.Title, subtaskBody(sub), o.tc.Title, o.taskDescription)

	res, model, err := o.runCoderWith(ctx, sc, sub, prompt)
	if err != nil {
		if o.salvageCapped(ctx, sc, sub, res, err) {
			return nil
		}

		salvaged, serr := o.salvageSoloCapped(ctx, sc, sub, model, spendBefore, res, err)
		if salvaged {
			return nil
		}

		// serr is set only when verify resolution itself raised a sentinel
		// (currently *ToolchainMissingError): that supersedes the original
		// MaxTurnsError so it reaches execute()'s dedicated park arm instead of
		// parking as a plain turn-cap.
		if serr != nil {
			return serr
		}

		return err
	}

	commitMsg := finishCommitMessage(res.CompletionArgs)
	if commitMsg == "" {
		commitMsg = sanitizeTitle(sub.Title)
	}

	committed, err := sc.git.CommitWithMessage(ctx, commitMsg)
	if err != nil {
		return fmt.Errorf("commit subtask %s: %w", sub.ID, err)
	}

	// Execute checkpoint: the mob critiques the committed diff (and may run
	// one revise pass) BEFORE the push, so a revise commit rides the same
	// push and the next subtask builds on the revised base.
	if committed && checkpointBase != "" {
		o.mobCheckpoint(ctx, sc, sub, checkpointBase)
	}

	// Push after every subtask so each unit of work is durable and the next
	// subtask builds on a pushed base. A clean tree (nothing committed) skips the
	// push but still completes the card. A push failure aborts the run - the
	// spend has already been reported, so retry/resume must not double-charge.
	// A candidate solver (sc.push false) never pushes: its work is judged in place.
	if committed && sc.push {
		if err := o.pushBranch(ctx); err != nil {
			return fmt.Errorf("push after subtask %s: %w", sub.ID, err)
		}
	}

	if sc.boardOps {
		// Report the win BEFORE CompleteTask (claim-gating rationale on
		// reportSoloOutcome - a report after complete_task releases the claim
		// would silently vanish). VerifyPass false means unknown here, not
		// failure: no authoritative verify runs on a finish-terminated
		// completion. The model finished, committed, and pushed, so the win is
		// real regardless of what the board bookkeeping below does with it.
		o.reportSoloOutcome(ctx, sub.ID, model, "win", false, sc.ledger.Spent()-spendBefore)

		if err := d.Ops.CompleteTask(ctx, sub.ID, commitSubject(commitMsg, sub.Title)); err != nil {
			return fmt.Errorf("complete subtask %s: %w", sub.ID, err)
		}
	}

	return nil
}

// startSubtaskHeartbeat ticks ops.Heartbeat for cardID on
// subtaskHeartbeatInterval until the returned stop func is called. Failures
// are logged, never fatal - a transient MCP hiccup must not abort a healthy
// run. Stop BLOCKS until the goroutine has exited: executeClaimedWith must
// never return while a tick could still fire for a completed subtask.
func startSubtaskHeartbeat(ctx context.Context, ops Ops, cardID string) func() {
	return StartTicker(ctx, subtaskHeartbeatInterval, func(ctx context.Context) {
		if err := ops.Heartbeat(ctx, cardID); err != nil {
			slog.Warn("subtask heartbeat failed", "card_id", cardID, "error", err)
		}
	})
}

// releaseSubtask best-effort releases a claimed subtask on an error exit.
// WithoutCancel: the release must still go out when the run context is the
// thing that died (end_session/kill). An already-unclaimed card
// (ErrCardNotClaimed) is a benign no-op, mirroring the worker's releaseQuietly.
func (o *run) releaseSubtask(ctx context.Context, cardID string) {
	if err := o.d.Ops.ReleaseCard(context.WithoutCancel(ctx), cardID); err != nil &&
		!errors.Is(err, cmclient.ErrCardNotClaimed) {
		slog.Warn("release subtask failed", "card_id", cardID, "error", err)
	}
}

// runCoderWith runs the subtask's coder harness through the solver, with in-run
// recovery from a harness-incapable model: it resolves the coder model via
// sc.coderModel (skipping any model already excluded this run), logs the pick,
// runs the harness on sc.tools, and accounts for spend on sc.ledger for every
// attempt. If the model proves incapable (*IncapableError) it
// blacklists/excludes it via recoverIncapable and RE-SELECTS the next-best model
// for the SAME subtask - the incapable model wrote nothing, so re-running is
// clean (no git reset). The loop is bounded by recoverIncapable's per-card cap:
// once exhausted it returns the wrapped park error. Any non-incapable run error
// (transport, context limit, budget) is returned immediately, unwrapped of the
// recovery loop. Returns the successful run's result, alongside the SELECTED
// catalog slug for that attempt - not the gateway-echoed ModelUsed - because a
// caller reporting a model outcome must key the row the way Best-of-N rows are
// keyed (candidates.go's c.model = spec.Model): CM attaches outcome stats back
// onto candidates by that slug, so a row keyed on a gateway's echoed name would
// never rejoin selection.
func (o *run) runCoderWith(ctx context.Context, sc *solverCtx, sub subtaskRef, prompt string) (harness.Result, string, error) {
	d := o.d
	cfg := d.Cfg

	// At most one initial attempt plus reselectCap re-selections; recoverIncapable
	// is the authoritative bound (it errors at the cap), the +1 is a belt-and-braces
	// ceiling so a logic slip can never spin.
	for attempt := 0; attempt <= reselectCap; attempt++ {
		model := sc.coderModel(ctx, sub, prompt)

		// A candidate's reselection-aware resolver returns "" when its model pool is
		// exhausted (every viable model excluded this run). Drop the candidate
		// cleanly rather than run the harness with no model. The parent/single-solver
		// resolver never returns "" (it falls back to the capable default), so this
		// is candidate-only in practice.
		if model == "" {
			return harness.Result{}, "", fmt.Errorf("coder for %s: candidate model pool exhausted", sub.ID)
		}

		logMsg := fmt.Sprintf("coder model %s selected for subtask %q (tier=%s)", model, sub.Title, tierOf(sub))
		if sc.tag != "" {
			// A candidate solver tags its log line so parallel selections are
			// distinguishable; the parent (tag "") logs the bare line as before.
			logMsg = sc.tag + ": " + logMsg
		}

		d.logCard(ctx, "%s", logMsg)

		res, dur, err := o.runModelCoder(ctx, sc.tools, prompt, model, coderWrapUpMessage, tierOf(sub))

		// Record the resolved coder slug so the review panel excludes it: a capable
		// model must not review its own code. This runs BEFORE the incapable check
		// below, so an incapable model (which produced no code) is also recorded
		// here - harmless, and it keeps that model out of its own review via this
		// set plus o.excluded. Keyed on the slug we configured, which is what
		// SelectReviewPanel's Exclude set compares against. newRun initializes the
		// map unconditionally. selMu guards it: Best-of-N candidates write here in
		// parallel; the review-phase read is sequenced after the fan-out's wg.Wait.
		o.selMu.Lock()
		o.coderModels[model] = true
		o.selMu.Unlock()

		// Best-of-N candidates report spend against the PARENT card, not the subtask:
		// report_usage is not claim-gated, and folding candidate spend onto the
		// parent's token_usage is what lets a resumed run's trigger context (and thus
		// degradeN) see it. The parent/single-solver solver (boardOps) reports on the
		// subtask as before.
		target := sub.ID
		if !sc.boardOps {
			target = cfg.CardID
		}

		// The incapable attempt is charged too - it burned tokens before tripping.
		o.spendAndReport(ctx, sc.ledger, target, "execute: report usage failed", res, model, "main", dur)

		var ie *IncapableError
		if errors.As(err, &ie) {
			// recoverIncapable blacklists + excludes the model and returns an error
			// only when the per-card re-selection cap is exhausted - park then.
			if rerr := o.recoverIncapable(ctx, ie); rerr != nil {
				return res, model, rerr
			}

			// Re-select (the failed model is now excluded) and re-run the SAME
			// subtask: a clean restart since the incapable model committed nothing.
			continue
		}

		if err != nil {
			return res, model, fmt.Errorf("coder run for %s: %w", sub.ID, err)
		}

		return res, model, nil
	}

	// Unreachable in practice: recoverIncapable errors at the cap before the loop
	// can exhaust its iterations. Defensive guard against an infinite loop.
	return harness.Result{}, "", fmt.Errorf("coder for %s: re-selection loop exhausted", sub.ID)
}

// pushBranch pushes the card branch after a commit. On a FRESH run that found a
// stale remote branch (o.staleRemoteTip != ""), the FIRST push overwrites it
// with a force-with-lease against the recorded tip - per spec §5.1, a fresh run
// owns its card branch and reclaims a stale one at first push. Every push after
// that (firstPushDone) is plain, because the branch is now ours and a plain push
// fast-forwards. A run with no stale branch (staleRemoteTip == "", the normal
// case, including all resume runs which never record a tip) always uses a plain
// push. Shared by the execute and document phases.
func (o *run) pushBranch(ctx context.Context) error {
	branch := o.d.Cfg.Branch

	// Every exit marks the first push as attempted: the lease is a one-shot
	// overwrite, never to be repeated with a stale expected tip.
	defer func() { o.firstPushDone = true }()

	if !o.firstPushDone && o.staleRemoteTip != "" {
		if err := o.d.Git.ForcePushWithLease(ctx, branch, o.staleRemoteTip); err != nil {
			return fmt.Errorf("lease push %q: %w", branch, err)
		}

		return nil
	}

	if err := o.d.Git.Push(ctx, branch); err != nil {
		return fmt.Errorf("push %q: %w", branch, err)
	}

	return nil
}

// resolveCoderModel picks the coder model for a subtask: the card's coder pin
// when it is catalog-resolvable, else the best-value complexity selection for
// the subtask's tier and a real window estimate of the coder prompt.
func (o *run) resolveCoderModel(ctx context.Context, sub subtaskRef, prompt string) string {
	if resolvePin(o.d.Registry, o.tc.ModelCoder) {
		// A pinned model is returned even if it is in o.excluded: we never override
		// an explicit operator pin with an auto-selected substitute. A pinned model
		// that is harness-incapable therefore keeps being re-selected, exhausts the
		// re-selection cap, and parks - the blacklist still records it.
		return o.tc.ModelCoder
	}

	if o.tc.ModelCoder != "" {
		o.warnUnresolvablePin(ctx, "coder", o.tc.ModelCoder)
	}

	spec := o.d.Registry.SelectByComplexity(registry.SelectInput{
		Role:      registry.RoleCoder,
		Tier:      tierOf(sub),
		EstTokens: estimateTokens(prompt),
		Exclude:   o.excluded,
	})

	return spec.Model
}

// subtaskBody returns the description text for a subtask: the planner's
// description (file lists, acceptance criteria) on the fresh-plan path. The
// title fallback exists for resume-loaded refs, which legitimately lack bodies
// (SubtaskStates carries no body field) - it is not the primary path.
func subtaskBody(sub subtaskRef) string {
	if sub.Body != "" {
		return sub.Body
	}

	return sub.Title
}

// tierOf maps a subtask's planner tier string to a registry.Tier. An empty or
// unrecognised tier defaults to moderate: conservative, since under-selecting a
// model for real work is worse than slightly over-paying.
func tierOf(sub subtaskRef) registry.Tier {
	switch sub.Tier {
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

// salvageCapped rescues a Best-of-N candidate that hit the turn cap on its
// FINAL subtask: the work may well be complete (the observed failure mode is
// turns burned on post-green re-verification, not missing work), and the judge
// verifies every candidate in place - so the project's verify command, not the
// model's self-report, is the completion authority. The tree is committed with
// the sanitized-title fallback message - a capped run by definition never
// completed via a successful finish call (that would have ended the run before
// the cap could trip), so res.CompletionArgs is always empty here - and the
// solver marked capped, but ONLY when the commit actually captures a change: a
// clean tree (nothing to commit) has no diff - the only completion evidence a
// capped run has - so it is NOT salvaged and the candidate drops exactly as it
// would without this rescue path. runJudge admits a capped candidate only when
// its verify passes. A cap on an EARLIER subtask is never salvaged - whole
// subtasks are missing, which a green verify cannot expose - and the
// parent/single-solver (boardOps) keeps its park-and-resume path.
//
// Turn-budget decision: the coder budget is tier-scaled (complex 1.5x / critical
// 2x the configured base via coderMaxTurns) with deliberately NO separate
// candidate cap - candidates run the same tier-sized coder budget. The wrap-up
// nudge removes post-green dithering and this salvage removes the cliff, so the
// extra headroom is spent only on genuinely productive work; a flat candidate
// bump would only fund waste (see the turn-waste design spec).
func (o *run) salvageCapped(ctx context.Context, sc *solverCtx, sub subtaskRef, res harness.Result, err error) bool {
	var mte *MaxTurnsError
	if sc.boardOps || sc.lastSubID == "" || sub.ID != sc.lastSubID || !errors.As(err, &mte) {
		return false
	}

	commitMsg := finishCommitMessage(res.CompletionArgs)
	if commitMsg == "" {
		commitMsg = sanitizeTitle(sub.Title)
	}

	committed, cerr := sc.git.CommitWithMessage(ctx, commitMsg)
	if cerr != nil || !committed {
		// No commit (error or clean tree), no salvage: the diff is the only
		// completion evidence a capped run has - an empty tree has none.
		// The candidate drops exactly as before.
		return false
	}

	sc.capped = true

	o.d.logCard(ctx, "%s: turn cap on final subtask %s - work committed; the judge's verify decides", sc.tag, sub.ID)

	return true
}

// salvageSoloCapped rescues a single-solver (parent / mob session) subtask that hit
// the turn cap - the run-1 failure mode: the work is complete and verified
// in-run, but no turn is left for the finish call. Unlike the Best-of-N variant
// (whose judge verifies every candidate later), the single solver has no judge,
// so the authoritative verify runs HERE and gates the rescue: committed &&
// verify actually ran && verify passed - the turn-waste campaign's contract,
// never weakened. A skipped or unresolved verify plan is NOT a pass. On a pass
// the subtask completes exactly like a finish-terminated run (push when sc.push,
// then CompleteTask); on any other outcome the run parks unchanged and the
// commit stays as WIP evidence for resume. Only the single-solver (boardOps)
// path is eligible - a candidate solver is handled by salvageCapped.
//
// Return contract: (true, nil) means salvaged - the caller returns nil. (false,
// nil) means not salvaged - the caller returns the original MaxTurnsError
// unchanged, today's park. (false, err) means not salvaged AND err must replace
// the original error on the caller's return path: these are the
// *ToolchainMissingError cases below, whether raised while RESOLVING the plan
// (ensureVerify) or while RUNNING it (runVerifyPlan, e.g. a container runtime
// the worker does not have) - either way it is the environment, not the coder
// run, that's parking the card, and the sentinel must reach execute()'s
// dedicated toolchain arm - and from there the worker's blocked-transition arm -
// instead of surfacing as a plain turn-cap.
//
// Every exit that has not yet reached a verified win reports a "failed" model
// outcome - the run did not complete, regardless of cause - except the
// environment-caused exits, none of which is evidence about the model: the
// *ToolchainMissingError branches (a missing tool or unreachable container
// runtime, left unreported like their unlogged card-log line), a verifySkipped result
// from the authoritative run itself (a timeout or a missing tool discovered
// at execution time rather than resolution time), a verify that ran but died
// of container resource pressure on both attempts (LooksResourceExhausted on
// the final output), and the push-failure exit below a PASSING verify - the
// model's work was already proven correct there, so the park is
// infrastructure, not a capability gap, and it earns no row in either
// direction. The win report fires BEFORE CompleteTask (claim-gating rationale
// on reportSoloOutcome) - once it fires, a subsequent CompleteTask failure
// gets no report of its own: the model's work already earned its win, and a
// board-write hiccup afterward is not a second, contradictory outcome.
// spendBefore is the run ledger's total at this subtask's start (captured by
// the caller before the coder run), so every report below carries this
// subtask's own cost delta, not the run's cumulative spend.
func (o *run) salvageSoloCapped(ctx context.Context, sc *solverCtx, sub subtaskRef, model string, spendBefore float64, res harness.Result, err error) (bool, error) {
	var mte *MaxTurnsError
	if !sc.boardOps || !errors.As(err, &mte) {
		return false, nil
	}

	commitMsg := finishCommitMessage(res.CompletionArgs)
	if commitMsg == "" {
		commitMsg = sanitizeTitle(sub.Title)
	}

	// The commit is the only completion evidence a capped run has: a clean tree
	// (nothing to commit) has no diff, so there is nothing to salvage and the run
	// parks exactly as it would without this path.
	committed, cerr := sc.git.CommitWithMessage(ctx, commitMsg)
	if cerr != nil || !committed {
		// The cap happened regardless of whether there was anything to
		// salvage-commit - escalate so resume does not repeat it at the same tier.
		o.escalateSubtaskTier(ctx, sub)
		o.reportSoloOutcome(ctx, sub.ID, model, "failed", false, sc.ledger.Spent()-spendBefore)

		return false, nil
	}

	// The authoritative verify: with no judge, the project's verify command - not
	// the model's self-report - is the completion authority. A budget park during
	// resolution or a skip (no command resolved) leaves the work unverified, which
	// is NOT a pass: park with the commit standing as WIP evidence.
	plan, verr := o.ensureVerify(ctx)
	if verr != nil {
		// A toolchain that implicated itself mid-run (e.g. a marker file the
		// coder added) is not a generic unresolvable-verify park: propagate it
		// unlogged here so execute()'s dedicated arm writes the toolchain card-log
		// line and the run parks as blocked, not as a plain turn cap. It is also
		// left unreported to the leaderboard for the same reason - a missing
		// toolchain is not evidence about the model.
		var tme *ToolchainMissingError
		if errors.As(verr, &tme) {
			return false, verr
		}

		o.logSoloCapPark(ctx, sub.ID, "verify could not be resolved")
		o.escalateSubtaskTier(ctx, sub)
		o.reportSoloOutcome(ctx, sub.ID, model, "failed", false, sc.ledger.Spent()-spendBefore)

		return false, nil
	}

	if len(plan.Argv) == 0 {
		o.logSoloCapPark(ctx, sub.ID, "no verify command resolved to confirm it")
		o.escalateSubtaskTier(ctx, sub)
		o.reportSoloOutcome(ctx, sub.ID, model, "failed", false, sc.ledger.Spent()-spendBefore)

		return false, nil
	}

	vres, rerr := o.runVerifyPlan(ctx, sc.workspace, plan)
	if rerr != nil {
		// The container-runtime park (or any other sentinel runVerifyPlan
		// raises) discovered during the authoritative verify itself is the
		// same shape as the ensureVerify arm above: propagate it unlogged so
		// it supersedes the turn cap and reaches execute()'s dedicated
		// toolchain arm instead of being read as a plain "did not pass".
		var tme *ToolchainMissingError
		if errors.As(rerr, &tme) {
			return false, rerr
		}
	}

	if rerr != nil || vres.Status != verifyPassed {
		// A verify that ran and failed can still be environmental: under a pids
		// limit `go test` compiles, then its inner fork/exec dies with EAGAIN and
		// go exits 1 - classified failed, surviving the single retry when the
		// pressure persists. The signature check keeps that shape off the
		// leaderboard below; a genuine failure whose output happens to print an
		// exhaustion string loses one row - bounded, and the park itself still
		// stands either way.
		exhausted := vres.Status != verifySkipped && verifyexec.LooksResourceExhausted(vres.Output)

		// Name the classification when there is one (e.g. a timeout or a missing
		// tool) so a park caused by the environment reads differently on the card
		// log from a real failure, which carries no note. The note already states
		// its own verdict, so use it alone rather than wrapping it in a second
		// "verify did not pass" that repeats the same thing. The exhausted-failure
		// shape carries no note of its own, so name it here - the card log must
		// agree with the reporting exemption below about the cause.
		reason := "verify did not pass"

		switch {
		case vres.Note != "":
			reason = vres.Note
		case exhausted:
			reason = "verify failed under container resource exhaustion - treated as environmental"
		}

		o.logSoloCapPark(ctx, sub.ID, reason)
		o.escalateSubtaskTier(ctx, sub)

		// A skipped verify (timeout or missing tool - rerr != nil takes the same
		// zero-value Status) is an environment problem, not evidence about the
		// model - the same exemption the ToolchainMissingError branch above gets,
		// extended to the exhausted-failure shape.
		if vres.Status != verifySkipped && !exhausted {
			o.reportSoloOutcome(ctx, sub.ID, model, "failed", false, sc.ledger.Spent()-spendBefore)
		}

		return false, nil
	}

	// Verified: complete exactly like a finish-terminated run. A push failure
	// declines the salvage (the run parks); the spend was already reported, so a
	// resume must not double-charge. No tier escalation and no outcome row below
	// this point on a decline: the authoritative verify already confirmed the
	// model's work is correct, so a park here is infrastructure (a push or a
	// board-write hiccup), not a capability gap - a "failed" row would penalize
	// the model at full weight for a fault that is not its own.
	if sc.push {
		if perr := o.pushBranch(ctx); perr != nil {
			return false, nil
		}
	}

	// The win report fires BEFORE CompleteTask, not after (claim-gating
	// rationale on reportSoloOutcome): the verify already confirmed the work,
	// so the win is real regardless of what CompleteTask does next - a
	// CompleteTask failure below gets no second, contradictory row.
	o.reportSoloOutcome(ctx, sub.ID, model, "win", true, sc.ledger.Spent()-spendBefore)

	if cerr := o.d.Ops.CompleteTask(ctx, sub.ID, commitSubject(commitMsg, sub.Title)); cerr != nil {
		return false, nil
	}

	o.d.logCard(ctx, "turn cap on subtask %s - committed work passed the authoritative verify; salvaged as complete", sub.ID)

	return true, nil
}

// logSoloCapPark records the advisory when a capped single-solver subtask
// committed work but could not be salvaged (verify unresolved, skipped, or not
// passing): the run parks and the commit stands as WIP evidence for resume.
func (o *run) logSoloCapPark(ctx context.Context, subID, reason string) {
	o.d.logCard(ctx, "turn cap on subtask %s - work committed but %s; parking for resume", subID, reason)
}

// reportSoloOutcome reports one solo (boardOps) run's terminal model outcome
// to CM's leaderboard, keyed on the subtask card itself - not the parent,
// deliberately unlike the Best-of-N judge's parent rollup - mirroring the
// existing ReportUsage solo precedent. NCandidates is always 1 and JudgeModel
// is always empty: there is no judge on the solo path. Best-effort: a report
// failure only warns, mirroring every other board write on this path.
//
// Claim-gating: CM's report_model_outcome handler requires an active claim on
// cardID, and complete_task atomically releases that claim on success -
// report_usage is NOT the transferable precedent here (usage reporting
// carries no claim gate). Every caller MUST report while the claim is still
// held, i.e. before CompleteTask/ReleaseCard - reporting after either would
// silently fail against an already-released claim.
//
// Bias-math note: a Best-of-N judge samples N candidates, so a model's win
// rate over many judged runs settles below 100% and the leaderboard's
// calibration factor is built to track that spread. A solo run is a sample of
// exactly one candidate, so its expected win rate is already 1.0 - an
// unbroken string of solo wins is therefore neutral, not inflationary: it
// cannot push a model's calibration factor above the parity a single BoN win
// already implies. Only a solo failure moves the needle, and only downward
// toward the calibration floor. Solo reporting can widen the gap between a
// reliable and an unreliable model; it can never manufacture a prior a model
// has not earned.
func (o *run) reportSoloOutcome(ctx context.Context, cardID, model, result string, verifyPass bool, costUSD float64) {
	outcome := cmclient.ModelOutcome{
		Model:       model,
		Result:      result,
		VerifyPass:  verifyPass,
		CostUSD:     costUSD,
		NCandidates: 1,
	}

	if err := o.d.Ops.ReportModelOutcomes(ctx, cardID, []cmclient.ModelOutcome{outcome}); err != nil {
		slog.Warn("execute: report solo model outcome failed",
			"card_id", cardID, "model", model, "result", result, "error", err)
	}
}

// escalateSubtaskTier bumps the persisted tier marker on sub's card body one
// step (nextTier) so a resumed run selects a stronger model against a bigger
// turn cap - a park at the same tier would otherwise repeat the same losing
// attempt indefinitely. It fetches the subtask's live card body (never the
// possibly-empty in-memory subtaskRef.Body) and writes it back, mirroring
// recordCheckpointOnSubtask. Best-effort: a fetch or write failure only
// warns - the run is parking either way, and a stale marker just means resume
// retries at the prior tier instead of escalating. Does not mutate sub.Tier -
// this run is ending; the marker is read back only on the NEXT run. Critical
// is already the ceiling (nextTier maps it to itself), so that case skips the
// board write entirely - there is nothing to persist.
func (o *run) escalateSubtaskTier(ctx context.Context, sub subtaskRef) {
	tc, err := o.d.Ops.GetTaskContext(ctx, sub.ID, false)
	if err != nil {
		slog.Warn("turn cap: subtask body fetch failed; skipping tier escalation",
			"card_id", o.d.Cfg.CardID, "subtask_id", sub.ID, "error", err)

		return
	}

	tier, clean := parseTierMarker(tc.Description)
	next := nextTier(tier)

	if next == tier {
		return
	}

	if uerr := o.d.Ops.UpdateCardBody(ctx, sub.ID, withTierMarker(clean, next)); uerr != nil {
		slog.Warn("turn cap: subtask tier escalation write failed",
			"card_id", o.d.Cfg.CardID, "subtask_id", sub.ID, "error", uerr)

		return
	}

	from := tier
	if from == "" {
		from = reconcileTierDefault
	}

	o.d.logCard(ctx, "turn cap: escalating subtask tier %s -> %s for the next attempt", from, next)
}

// sanitizeTitle builds the fallback commit message from a subtask title when the
// coder's finish call carries no usable commit message. Format: lowercase
// "feat: <title>" - a sane, conventional-ish default. A blank title yields
// "feat: untitled".
func sanitizeTitle(title string) string {
	t := strings.ToLower(strings.TrimSpace(title))
	if t == "" {
		t = "untitled"
	}

	return "feat: " + t
}

// commitSubject returns the first line of a commit message, trimmed of
// surrounding whitespace (including Windows \r line endings). When the message
// is empty or blank it falls back to sanitizeTitle(title). This is used for the
// one-line summary sent to CompleteTask; the full message still reaches
// CommitWithMessage and git.
func commitSubject(msg, title string) string {
	line, _, _ := strings.Cut(msg, "\n")

	line = strings.TrimSpace(line)
	if line == "" {
		return sanitizeTitle(title)
	}

	return line
}

// topoOrder returns the subtasks in dependency order via Kahn's algorithm:
// dependencies precede dependents, and among nodes that are simultaneously ready
// the original creation order is preserved (deterministic). A dependency cycle
// returns an error - the planner forbids cycles, but a resume-loaded set might
// not, so the guard is defensive. Dependency IDs not present in the set are
// ignored (already-done prerequisites from a prior run do not block scheduling).
func topoOrder(subs []subtaskRef) ([]subtaskRef, error) {
	index := make(map[string]int, len(subs))
	for i, s := range subs {
		index[s.ID] = i
	}

	// indegree counts only in-set dependencies; out-of-set deps are satisfied.
	indegree := make([]int, len(subs))
	dependents := make([][]int, len(subs))

	for i, s := range subs {
		for _, dep := range s.DependsOnIDs {
			j, ok := index[dep]
			if !ok {
				continue
			}

			indegree[i]++
			dependents[j] = append(dependents[j], i)
		}
	}

	// Seed the ready set in creation order so ties are deterministic.
	var ready []int

	for i := range subs {
		if indegree[i] == 0 {
			ready = append(ready, i)
		}
	}

	ordered := make([]subtaskRef, 0, len(subs))

	for len(ready) > 0 {
		// Pop the lowest original index among the ready nodes: preserves creation
		// order among simultaneously-ready siblings.
		pick := 0
		for k, idx := range ready {
			if idx < ready[pick] {
				pick = k
			}
		}

		i := ready[pick]
		ready = append(ready[:pick], ready[pick+1:]...)
		ordered = append(ordered, subs[i])

		for _, dep := range dependents[i] {
			indegree[dep]--
			if indegree[dep] == 0 {
				ready = append(ready, dep)
			}
		}
	}

	if len(ordered) != len(subs) {
		return nil, fmt.Errorf("subtask dependency cycle detected (%d of %d schedulable)", len(ordered), len(subs))
	}

	return ordered, nil
}
