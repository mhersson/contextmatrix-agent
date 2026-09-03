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
	"github.com/mhersson/contextmatrix-harness/events"
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
	git       GitOps
	ledger    *Ledger
	tools     *tools.Registry
	workspace string
	// coderModel resolves the model for one coder attempt. It returns the whole
	// Pick, not the slug: the outcome report has to know whether the selector
	// got the bar it asked for, and a slug cannot carry that.
	coderModel func(ctx context.Context, sub subtaskRef, prompt string) (registry.Pick, error)
	boardOps   bool         // false: no subtask claim/heartbeat/complete (candidate mode)
	push       bool         // false: never push (candidate mode)
	tag        string       // "" parent; "candidate 2/3 (slug)" for candidate log lines
	completed  []subtaskRef // subtasks this solver actually executed
	lastSubID  string       // final subtask ID in execution order; "" disables turn-cap salvage (parent/single-solver)
	capped     bool         // the final subtask hit the turn cap; its work was salvage-committed for judge verification
	gate       gateEvidence // what the pre-commit and checkpoint gates learned about this subtask's coder work and its revise
	// toolVerify is the verdict of the coder verify tool's last actual run and
	// the worktree identity it was measured against, republished by the tool on
	// every run. Zero until the coder calls the tool, and for a candidate
	// solver, which binds no recorder. Written by the tool during the coder
	// harness run and read by the pre-commit gate after it returns, on the
	// goroutine that drives the subtask.
	toolVerify verifyToolPass
}

// gateEvidence is what the pre-commit gate learned about this subtask. It
// lives on the solver because mobCheckpoint may replace the tree between the
// gate and the report site. verified: the command ran and passed on the tree
// being committed. coderFailed: the gate was red on the coder's own work,
// even when the fix pass then repaired it.
type gateEvidence struct {
	verified    bool
	coderFailed bool
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
	plan, err := o.ensureVerify(ctx)
	if err != nil {
		return err
	}

	// Best-of-N replaces the single-solver execute with a candidate fan-out: N
	// implementations of the shared plan race in isolated worktrees, off the board
	// and never pushing, and a later phase judges them. Each candidate binds its
	// own verify tool, rooted at its own worktree.
	if o.d.Cfg.BestOfN >= 2 {
		return o.runFanout(ctx)
	}

	o.bindVerifyTool(o.solver, plan)

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

// bindVerifyTool rebuilds the solver's registry with the run's verify tool, so
// the coder calls the resolved command instead of guessing shell commands for
// it. It binds here rather than in newRun because the plan resolves at this
// point, not at construction. A run with no resolvable command leaves the
// registry exactly as it was.
//
// Ordering, since this mutates solver state mid-run: it runs after the plan has
// resolved and before the first coder harness starts, on the goroutine that
// drives the phase. Nothing reads sc.tools in between. A Best-of-N fan-out
// cannot observe it at all - that branch returns above, o.solver is untouched by
// the fan-out, and each candidate binds its own tool inside runCandidate.
func (o *run) bindVerifyTool(sc *solverCtx, plan verifyPlan) {
	if o.d.WriteToolsForDir == nil {
		return
	}

	vt := o.verifyToolFor(sc.git, sc.workspace, plan, func(p verifyToolPass) { sc.toolVerify = p })
	if vt == nil {
		return
	}

	sc.tools = o.d.WriteToolsForDir(sc.workspace, vt)
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

// VerifyParkedError marks the pre-commit verify park: the resolved verify
// command was red, the one bounded fix pass ran, and the re-run was still red.
// The worker maps it like the toolchain park - push the WIP, move the card to
// blocked, release the claim. Command, output and Environmental all travel on
// it because the park arm that reports them outlives the container.
type VerifyParkedError struct {
	Subtask string // the subtask whose gate stayed red
	Command string // the verify command, as displayed
	Output  string // a bounded excerpt of what the failing run printed

	// Environmental: the final failure carried the container
	// resource-exhaustion signature, so it is evidence about neither the
	// model nor the sizing.
	Environmental bool
}

func (e *VerifyParkedError) Error() string {
	return fmt.Sprintf("subtask %s: `%s` still fails after one fix pass", e.Subtask, e.Command)
}

// preCommitVerify gates the coder's uncommitted work on the run's verify plan.
// nil means commit: passed, no command resolved, or inconclusive (a timeout or
// missing tool is not evidence of a defect). A failure gets exactly one fix pass
// and one re-run; still red returns *VerifyParkedError, whose park path pushes
// the refused tree as WIP. Solo only - candidates are verified by the judge.
// exhausted (the coder spent its whole window) picks which sizing axes rise.
func (o *run) preCommitVerify(ctx context.Context, sc *solverCtx, sub subtaskRef, exhausted bool) error {
	// One assignment point, before any early return, so a verdict from an
	// earlier subtask can never stand in for this one.
	sc.gate = gateEvidence{}

	if !sc.boardOps {
		return nil
	}

	plan, vres, ran, err := o.runGate(ctx, sc, sub, subtaskGateContext(sub.ID))
	if err != nil {
		return err
	}

	if !ran {
		return nil
	}

	if vres.Status != verifyFailed {
		sc.gate.verified = vres.Status == verifyPassed

		return nil
	}

	// Budget gate before the fix run: the subprocess above costs no tokens, the
	// model call does, so a park happens before the spend rather than after.
	if err := o.ledger.Check(); err != nil {
		return err
	}

	o.d.logCard(ctx, "subtask %s: verify failed before the commit - running one fix pass", sub.ID)

	sc.gate.coderFailed = true

	// The subtask's own tier sizes the fix, the way it sized the coder that wrote
	// the work; an unset tier falls back to the card tier inside the fix path.
	prompt := fmt.Sprintf(verifyFixPrompt, o.skillEngage(), o.grounding, sc.workspace,
		fixVerifyLine(plan), o.tc.Title, verifyFailedFindings(plan, vres.Output))

	// Round 0: this is not a review round and has no round number. The subtask
	// rides along because this round is sized on ITS bar, so its measurement row
	// must report its estimate rather than the card's.
	req := fixRequest{Round: 0, FixTier: string(sub.Sizing.Bar), Subtask: sub.ID, PlannerBar: sub.PlannerBar}
	if _, ferr := o.runFixModel(ctx, prompt, req); ferr != nil {
		// A cap is not a verdict on the tree the fix left behind: the re-run
		// below decides, exactly as runFix does for the review rounds.
		var mte *MaxTurnsError
		if !errors.As(ferr, &mte) {
			return ferr
		}

		o.d.logCard(ctx, "subtask %s: the fix pass hit its turn cap - verifying what it left", sub.ID)
	}

	// A direct run, not the shared gate: the fix coder holds no verify tool, so
	// there is no pass of its own for the gate to take.
	vres, rerr := o.runVerifyPlan(ctx, sc.workspace, plan)
	if rerr != nil {
		return rerr
	}

	o.logVerifyGate(ctx, vres, subtaskGateContext(sub.ID))

	if vres.Status == verifyFailed {
		// One binding of the exhaustion verdict, from the final failing output,
		// so the sizing correction, the card log and the suppressed outcome row
		// cannot disagree about the cause.
		env := verifyexec.LooksResourceExhausted(vres.Output)

		o.applyPrecommitVerifyEvidence(ctx, sub, env, exhausted)

		return &VerifyParkedError{
			Subtask:       sub.ID,
			Command:       plan.Display,
			Output:        verifyFailureExcerpt(vres.Output),
			Environmental: env,
		}
	}

	// Symmetry with the first-run arm, not a live value: reaching here means
	// the gate went red, so coderFailed is set and the report site overrides
	// verifyPass to false whatever this assigns.
	sc.gate.verified = vres.Status == verifyPassed

	return nil
}

// runGate is the one verify gate the pre-commit and checkpoint-revise callers
// share: resolve the plan, take the coder's own verify-tool pass when it
// measured this exact tree, otherwise run the command, log the verdict under
// where. ran is false for the skip tier and for a resolution error, true on an
// accepted pass though nothing ran, so an error with ran true came from the run.
func (o *run) runGate(ctx context.Context, sc *solverCtx, sub subtaskRef, where string) (verifyPlan, verifyResult, bool, error) {
	plan, err := o.ensureVerify(ctx)
	if err != nil {
		return verifyPlan{}, verifyResult{}, false, err
	}

	if len(plan.Argv) == 0 {
		return plan, verifyResult{}, false, nil
	}

	if o.gateAcceptsToolPass(ctx, sc) {
		// The gate line still fires: a human reading the activity log must see a
		// gate for every subtask, and this one says out loud where the verdict
		// came from rather than implying a second run happened.
		vres := verifyResult{
			Status: verifyPassed,
			Note:   "the coder's own run measured this exact tree; not re-run",
		}

		o.logVerifyGate(ctx, vres, where)
		o.emitAcceptedToolPass(sub)

		return plan, vres, true, nil
	}

	vres, err := o.runVerifyPlan(ctx, sc.workspace, plan)
	if err != nil {
		return plan, verifyResult{}, true, err
	}

	o.logVerifyGate(ctx, vres, where)

	return plan, vres, true, nil
}

// emitAcceptedToolPass records a gate that certified on the coder's own verify
// tool pass: without an event the transcript carries no record of this gate at
// all, and consumers undercount one verification per accepted subtask. The
// source names the acceptance and no command field is set - nothing re-ran,
// and an accepted pass must never read as a gate-executed one.
func (o *run) emitAcceptedToolPass(sub subtaskRef) {
	if o.d.Emit == nil {
		return
	}

	o.d.Emit.Emit(events.Verification, map[string]any{
		"ok":      true,
		"status":  verifyStatusWord(verifyPassed),
		"source":  "coder-verify-tool-pass",
		"detail":  "the gate accepted the coder's own verify-tool pass against this exact tree and did not re-run the command",
		"subtask": sub.ID,
	})
}

// gateAcceptsToolPass reports whether the gate may certify on the verdict the
// coder's verify tool recorded instead of running the command a second time.
// All three conditions hold or it returns false:
//
//   - the tool's last actual run PASSED (a failure, an inconclusive run, or a
//     tool the coder never called leaves the pair zero);
//   - the identity recorded at that pass is readable, and so is the one this
//     reads now (either unreadable is evidence of nothing);
//   - the two are equal - the tree about to be committed is the tree the command
//     passed against: the same commit, and the same uncommitted work on top of
//     it by the repository's own ignore rules. Both halves, because the coder
//     holds a bash tool and a commit it made would otherwise leave the two trees
//     indistinguishable - see worktreeIdentity.
//
// Every other direction runs the gate, which is what makes this safe to skip:
// every read error degrades to "assume moved", so a gate that cannot prove the
// trees are identical behaves exactly as it did before.
//
// What the gate gives up is a re-run of the same command over the same bytes.
// The tool ran it through runVerifyCommand, the same executor runVerifyPlan
// wraps, on the plan ensureVerify resolved - one command, one timeout, one
// environment, one workspace. What it does not give up is the fix pass: a tool
// verdict that is anything but a pass never reaches here.
func (o *run) gateAcceptsToolPass(ctx context.Context, sc *solverCtx) bool {
	if !sc.toolVerify.passed || sc.toolVerify.identity == "" {
		return false
	}

	ictx, cancel := context.WithTimeout(ctx, worktreeStateTimeout)
	defer cancel()

	id, err := worktreeIdentity(ictx, sc.git)
	if err != nil {
		slog.Warn("verify gate: worktree identity unreadable; running the command",
			"card_id", o.d.Cfg.CardID, "error", err)

		return false
	}

	return id == sc.toolVerify.identity
}

func subtaskGateContext(subID string) string {
	return fmt.Sprintf("subtask %s, before commit", subID)
}

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

	// A tool verdict belongs to the subtask that earned it. Belt and braces on
	// top of the identity itself: the recorded identity is qualified by HEAD, so
	// an earlier subtask's verdict can no longer match once that subtask
	// committed, and clearing it here means the gate is never even offered a
	// verdict from a subtask that is over.
	sc.toolVerify = verifyToolPass{}

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

	res, pick, coderMaxTurns, err := o.runCoderWith(ctx, sc, sub, prompt)
	if err != nil {
		if o.salvageCapped(ctx, sc, sub, res, err) {
			return nil
		}

		salvaged, serr := o.salvageSoloCapped(ctx, sc, sub, pick, spendBefore, res, err)
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

	if verr := o.preCommitVerify(ctx, sc, sub, windowExhausted(res.Turns, coderMaxTurns)); verr != nil {
		// This report must fire HERE, while the claim is still held:
		// executeSubtaskWith calls releaseSubtask on this error path, and a
		// report_model_outcome against a released claim silently vanishes.
		//
		// Settled decision: a still-red park reports `failed` for the CODER
		// model. This is the strongest negative evidence the solo path ever
		// holds - the coder's work was red, a bounded fix pass ran, and it was
		// still red - and `failed` is the only value that moves the numerator
		// the coder prior is built from, since a solo row carries n_candidates
		// 1 and a `win` therefore nets to zero. The row is cross-card
		// down-weighting evidence and does not double-count against the
		// per-card sizing correction applyPrecommitVerifyEvidence makes: that
		// mechanism behaves exactly as before, and one failure feeding both is
		// deliberate - one failure measured in two currencies, not counted
		// twice. The cost delta matches the win/repaired rows' convention,
		// folding the fix model's spend in - the subtask really cost that.
		//
		// An exhausted TURN WINDOW does NOT exempt: salvageSoloCapped reports
		// failed on both its cap arms, so volume has never been an exemption
		// in this taxonomy. A container RESOURCE-EXHAUSTION signature in the
		// final failing output does - the same exemption the capped path
		// takes - with a card-log line naming the classification so the log
		// agrees with the suppression.
		var vpe *VerifyParkedError
		if errors.As(verr, &vpe) {
			if vpe.Environmental {
				o.logEnvironmentalVerify(ctx, sub)
			} else {
				o.reportSoloOutcome(ctx, sub.ID, pick, "failed", false, sc.ledger.Spent()-spendBefore)
			}
		}

		return verr
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
		// Report BEFORE CompleteTask/ReleaseCard: reportSoloOutcome needs the
		// claim still held (its doc has the claim-gating rationale).
		//
		// A subtask the bounded fix pass had to repair reports `failed` for
		// the coder model even though the work still commits, pushes and
		// completes below - `failed` is the model's verdict, not the card's.
		// See reportSoloOutcome's doc for why this is the only choice that
		// moves the leaderboard numerator, and gateEvidence's field docs for
		// what verified/coderFailed mean (including the mob-checkpoint
		// revise, which is deliberately not judged the same way).
		result, verifyPass := "win", sc.gate.verified
		if sc.gate.coderFailed {
			result, verifyPass = "failed", false
		}

		o.reportSoloOutcome(ctx, sub.ID, pick, result, verifyPass, sc.ledger.Spent()-spendBefore)

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
// recovery loop. Returns the successful run's result, alongside the PICK that
// attempt ran on - so its Model is the SELECTED catalog slug, not the
// gateway-echoed ModelUsed, because a caller reporting a model outcome must key
// the row the way Best-of-N rows are keyed (candidates.go's setPick): CM
// attaches outcome stats back onto candidates by that slug, so a row keyed on a
// gateway's echoed name would never rejoin selection. The rest of the Pick is
// what tells that caller whether the selector got the bar it asked for, which
// decides whether a failure may be charged to the model at all.
//
// The third return is the turn window the run was configured with. A caller that
// judges res.Turns against a window - the pre-commit gate does, to tell a coder
// that finished early from one that spent everything it had - must judge against
// THIS window rather than derive its own, or a cap that ever becomes per-attempt
// leaves the two silently disagreeing.
func (o *run) runCoderWith(ctx context.Context, sc *solverCtx, sub subtaskRef, prompt string) (harness.Result, registry.Pick, int, error) {
	d := o.d
	cfg := d.Cfg

	// The window every attempt runs at, with the wrap-up reserve that goes with
	// it. Computed once, above the loop, because it is a function of the
	// operator's base cap and the subtask's budget step and neither changes
	// across re-selections - and returned, so this is the only place the coder's
	// window is derived on this side of runModelCoder.
	maxTurns, wrapUp := coderTurnCfg(cfg.MaxTurns, sub.Sizing.Budget)

	// At most one initial attempt plus reselectCap re-selections; recoverIncapable
	// is the authoritative bound (it errors at the cap), the +1 is a belt-and-braces
	// ceiling so a logic slip can never spin.
	for attempt := 0; attempt <= reselectCap; attempt++ {
		pick, err := sc.coderModel(ctx, sub, prompt)
		if err != nil {
			return harness.Result{}, registry.Pick{}, maxTurns, fmt.Errorf("coder for %s: %w", sub.ID, err)
		}

		model := pick.Model

		logMsg := fmt.Sprintf("coder model %s selected for subtask %q (bar=%s, turns=%s)",
			model, sub.Title, sub.Sizing.Bar, budgetLabel(sub.Sizing.Budget))
		if sc.tag != "" {
			// A candidate solver tags its log line so parallel selections are
			// distinguishable; the parent (tag "") logs the bare line as before.
			logMsg = sc.tag + ": " + logMsg
		}

		d.logCard(ctx, "%s", logMsg)

		res, dur, err := o.runModelCoder(ctx, sc.tools, prompt, model, coderWrapUpMessage, sub.Sizing.Budget)

		// Record the resolved coder slug so the review panel excludes it: a capable
		// model must not review its own code. This runs BEFORE the incapable check
		// below, so an incapable model (which produced no code) is also recorded
		// here - harmless, and it keeps that model out of its own review via this
		// set plus o.excluded. Keyed on the slug we configured, which is what
		// SelectReviewPanelReport's Exclude set compares against. newRun initializes the
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

		// One row per ATTEMPT. It sits here rather than after the loop because
		// the three exits below - an exhausted re-selection cap, a run error,
		// and success - would each need their own copy, and the incapable
		// attempts that continue past them would get none at all.
		solver := "solo"

		if !sc.boardOps {
			solver = "candidate"
		}

		o.emitSizingObs(sizingObs{
			Phase: o.curPhase, Solver: solver, Subtask: sub.ID, Reselect: attempt,
			Model: model, Bar: string(sub.Sizing.Bar), BudgetStep: sub.Sizing.Budget,
			PlannerBar: sub.PlannerBar, MaxTurns: maxTurns, WrapUpTurns: wrapUp,
			Turns: res.Turns, Outcome: sizingOutcome(err, res.Turns, maxTurns), DurationMS: dur.Milliseconds(),
		})

		var ie *IncapableError
		if errors.As(err, &ie) {
			// recoverIncapable blacklists + excludes the model and returns an error
			// only when the per-card re-selection cap is exhausted - park then.
			if rerr := o.recoverIncapable(ctx, ie); rerr != nil {
				return res, pick, maxTurns, rerr
			}

			// Re-select (the failed model is now excluded) and re-run the SAME
			// subtask: a clean restart since the incapable model committed nothing.
			continue
		}

		if err != nil {
			return res, pick, maxTurns, fmt.Errorf("coder run for %s: %w", sub.ID, err)
		}

		return res, pick, maxTurns, nil
	}

	// Unreachable in practice: recoverIncapable errors at the cap before the loop
	// can exhaust its iterations. Defensive guard against an infinite loop.
	return harness.Result{}, registry.Pick{}, maxTurns, fmt.Errorf("coder for %s: re-selection loop exhausted", sub.ID)
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
// the subtask's tier and a real window estimate of the coder prompt. Either way
// the outcome is reported, which is what puts the pick on the run transcript. A
// selection that could not be served at the subtask's tier is also advised on;
// a *NoModelError is returned when even the operator's capable default is
// barred this run, which parks the card - there is no work to do without a
// coder.
//
// It returns the whole Pick, like its fix-round sibling resolveFixModel: the
// provenance is what separates a laddered selection from a pin or the
// off-ladder default, and the slug alone cannot carry that.
//
// The competing-pool report of the selection rides out beside the pick for
// noteShortfall's pool line; a pin consults no rung and reports no pool.
func (o *run) resolveCoderModel(ctx context.Context, sub subtaskRef, prompt string) (registry.Pick, error) {
	tier := sub.Sizing.Bar

	if resolvePin(o.d.Registry, o.tc.ModelCoder) {
		// A pinned model is returned even if it is in o.excluded: we never override
		// an explicit operator pin with an auto-selected substitute. A pinned model
		// that is harness-incapable therefore keeps being re-selected, exhausts the
		// re-selection cap, and parks - the blacklist still records it.
		p := offLadderPick(o.d.Registry, o.tc.ModelCoder, registry.RoleCoder, tier, registry.SourcePinned)

		o.noteShortfall(ctx, "coder", sub.ID, p, registry.SelectionReport{})

		return p, nil
	}

	if o.tc.ModelCoder != "" {
		o.warnUnresolvablePin(ctx, "coder", o.tc.ModelCoder)
	}

	in := registry.SelectInput{
		Role:      registry.RoleCoder,
		Tier:      tier,
		EstTokens: estimateTokens(prompt),
		Exclude:   o.excluded,
	}

	p, rep := o.d.Registry.SelectByComplexityReport(in)
	if !p.OK {
		return registry.Pick{}, o.noModelError(in, p)
	}

	o.noteShortfall(ctx, "coder", sub.ID, p, rep)

	return p, nil
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
// Turn-budget decision: the coder budget is laddered (complex 1.5x / critical
// 2x the configured base via seedBudgetStep and coderTurnCfg) with deliberately
// NO separate candidate cap - candidates run the same laddered coder budget.
// The wrap-up nudge removes post-green dithering and this salvage removes the
// cliff, so the extra headroom is spent only on genuinely productive work; a
// flat candidate bump would only fund waste (see the turn-waste design spec).
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
//
// One more exemption applies underneath all of these and is NOT enumerated per
// arm: a failure from a pick the ladder walked down is dropped inside
// reportSoloOutcome, whichever arm asked for it.
func (o *run) salvageSoloCapped(ctx context.Context, sc *solverCtx, sub subtaskRef, pick registry.Pick, spendBefore float64, res harness.Result, err error) (bool, error) {
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
		// salvage-commit, and a clean tree says only that the work did not fit in
		// the turns it had - nothing about the model that ran it.
		o.raiseSubtaskBudget(ctx, sub, "the turn cap was reached with nothing committed")
		o.reportSoloOutcome(ctx, sub.ID, pick, "failed", false, sc.ledger.Spent()-spendBefore)

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
		o.raiseSubtaskBudget(ctx, sub, "the turn cap was reached and the verify could not be resolved")
		o.reportSoloOutcome(ctx, sub.ID, pick, "failed", false, sc.ledger.Spent()-spendBefore)

		return false, nil
	}

	if len(plan.Argv) == 0 {
		o.logSoloCapPark(ctx, sub.ID, "no verify command resolved to confirm it")
		o.raiseSubtaskBudget(ctx, sub, "the turn cap was reached with no verify command to confirm the work")
		o.reportSoloOutcome(ctx, sub.ID, pick, "failed", false, sc.ledger.Spent()-spendBefore)

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

		// modelEvidence is whether this failure says anything about the MODEL.
		// One binding read by both the bar raise and the leaderboard report, so
		// the card log, the outcome report and the bar cannot disagree about the
		// cause. A skipped verify (a timeout or a missing tool - rerr != nil
		// takes the same zero-value Status) is an environment problem, and so is
		// one that died of container resource pressure; the cap itself is still
		// volume evidence either way.
		modelEvidence := vres.Status != verifySkipped && !exhausted

		if modelEvidence {
			o.raiseSubtaskBoth(ctx, sub, "the turn cap was reached and the verify then failed")
		} else {
			o.logEnvironmentalCapBudget(ctx, sub)
		}

		// The same exemption the ToolchainMissingError branch above gets,
		// extended to the exhausted-failure shape.
		if modelEvidence {
			o.reportSoloOutcome(ctx, sub.ID, pick, "failed", false, sc.ledger.Spent()-spendBefore)
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
			slog.Warn("salvage: push failed after passing verify",
				"card_id", o.d.Cfg.CardID, "subtask_id", sub.ID, "error", perr, "step", "push")
			o.d.logCard(ctx, "turn cap on subtask %s - work passed verify but push failed: %s; parking for resume", sub.ID, perr)

			return false, nil
		}
	}

	// The win report fires BEFORE CompleteTask, not after (claim-gating
	// rationale on reportSoloOutcome): the verify already confirmed the work,
	// so the win is real regardless of what CompleteTask does next - a
	// CompleteTask failure below gets no second, contradictory row.
	o.reportSoloOutcome(ctx, sub.ID, pick, "win", true, sc.ledger.Spent()-spendBefore)

	if cerr := o.d.Ops.CompleteTask(ctx, sub.ID, commitSubject(commitMsg, sub.Title)); cerr != nil {
		slog.Warn("salvage: CompleteTask failed after passing verify",
			"card_id", o.d.Cfg.CardID, "subtask_id", sub.ID, "error", cerr, "step", "complete_task")
		o.d.logCard(ctx, "turn cap on subtask %s - work passed verify but CompleteTask failed: %s; parking for resume", sub.ID, cerr)

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
// to CM's outcome ledger, keyed on the subtask card itself - not the parent,
// deliberately unlike the Best-of-N judge's parent rollup. NCandidates is
// always 1 and JudgeModel is always empty: there is no judge on the solo
// path. Best-effort: a report failure only warns, mirroring every other
// board write on this path.
//
// Claim-gating: CM's report_model_outcome handler requires an active claim on
// cardID, and complete_task atomically releases that claim on success -
// report_usage is NOT the transferable precedent here (usage reporting
// carries no claim gate). Every caller MUST report while the claim is still
// held, i.e. before CompleteTask/ReleaseCard - reporting after either would
// silently fail against an already-released claim.
//
// Ledger semantics: the ledger is observability only - selection never reads
// it - and a solo completion is not a comparative win; only a solo failure
// carries signal. That is why the call site in executeClaimedWith reports
// `failed` for a subtask the fix pass had to repair, even though the subtask
// still ships.
//
// It takes the whole Pick rather than the slug because a `failed` row from a
// selection the ladder walked down is not recorded at all (walkedDown carries
// that rule): every reporting call site on this path funnels through here, so
// the suppression sits once instead of at each of them.
func (o *run) reportSoloOutcome(ctx context.Context, cardID string, pick registry.Pick, result string, verifyPass bool, costUSD float64) {
	if result == "failed" && walkedDown(pick) {
		o.d.logCard(ctx, "subtask %s: nothing cleared the %s bar, so the ladder walked down to %s at %s - the failure is not recorded against the model",
			cardID, pick.RequestedTier, pick.Model, metTierLabel(pick))

		return
	}

	outcome := cmclient.ModelOutcome{
		Model:       pick.Model,
		Result:      result,
		VerifyPass:  verifyPass,
		CostUSD:     costUSD,
		NCandidates: 1,
	}

	if err := o.d.Ops.ReportModelOutcomes(ctx, cardID, []cmclient.ModelOutcome{outcome}); err != nil {
		slog.Warn("execute: report solo model outcome failed",
			"card_id", cardID, "model", pick.Model, "result", result, "error", err)
	}
}

// sizingEscalationKind records one correction to a subtask's sizing on the run
// transcript: which axis moved, from what to what, and on what evidence.
//
// The card log carries the human version of the same line. This is the machine
// version, and it exists because a later analysis has to pair a correction with
// the outcome that caused it and the outcome that followed - both of which live
// in other runs' rows in the same per-card transcript. The resulting MODEL is
// deliberately absent: it is not knowable here. The corrected bar is read by the
// NEXT run, whose model_selected line names the model it bought.
//
// Like model_selected, no arm claims this kind in the log bridge, so it reaches
// the durable transcript and never an operator's live card stream.
const sizingEscalationKind = "sizing_escalation"

// resizeSubtask applies the axis's raise to the subtask's persisted sizing,
// writes the result back, and says on the card log which axis moved and why.
//
// ONE fetch and ONE write, whatever the raise does, so evidence that arrives
// together lands together: a composed raise that was split into two calls could leave
// the card budget-raised with the quality half dropped on a failed second write.
//
// It reads the subtask's LIVE card body rather than the possibly-empty
// in-memory ref (a resume-loaded ref legitimately has no body), mirroring
// recordCheckpointOnSubtask, and it mutates the parsed key map rather than
// rebuilding it, so keys this package does not understand round-trip.
//
// Best-effort throughout: the run is ending either way, and a stale marker only
// means the next attempt repeats this one. But a resize that could not move -
// an axis already at its ceiling - is LOGGED rather than skipped silently.
// There is no in-run retry on this path, so that line is the whole signal an
// operator gets that automatic correction is exhausted, and silence there reads
// exactly like a write that failed.
//
// Does not mutate sub.Sizing: this run is ending, and the marker is read back
// only on the NEXT run.
//
// The axis carries both the transform and the words for it, chosen together in
// one literal per trigger, so a mismatch between them is one visible edit rather
// than two files apart.
func (o *run) resizeSubtask(ctx context.Context, sub subtaskRef, why string, axis resizeAxis) {
	tc, err := o.d.Ops.GetTaskContext(ctx, sub.ID, false)
	if err != nil {
		slog.Warn("resize subtask: body fetch failed; skipping the correction",
			"card_id", o.d.Cfg.CardID, "subtask_id", sub.ID, "error", err)

		return
	}

	kv, from := readMeta(tc.Description)

	to := axis.raise(from)
	if to == from {
		o.d.logCard(ctx, "subtask %s: %s - the %s cannot be raised any further; re-triggering will repeat this attempt. %s",
			sub.ID, why, axis.name, axis.advice)

		return
	}

	if uerr := o.d.Ops.UpdateCardBody(ctx, sub.ID, writeMeta(tc.Description, setSizing(kv, to))); uerr != nil {
		slog.Warn("resize subtask: body update failed",
			"card_id", o.d.Cfg.CardID, "subtask_id", sub.ID, "error", uerr)

		return
	}

	// Only a pin the catalog can still serve reaches the selector: an
	// unresolvable one is warned about and falls through to the laddered
	// selection, which DOES honour the raised bar. Naming it here would
	// contradict both the raise and the unresolvable-pin advisory on the same
	// card log.
	pin := ""
	if resolvePin(o.d.Registry, o.tc.ModelCoder) {
		pin = o.tc.ModelCoder
	}

	o.emitSizingEscalation(sub, from, to, why)
	o.d.logCard(ctx, "subtask %s: %s - %s for the next attempt%s",
		sub.ID, why, resizeSummary(from, to), pinClause(pin, from, to))
}

// resizeAxis pairs the transform one trigger applies with the words the card
// log uses for it: the noun phrase that names the axis, and what a human can
// still do about THAT axis once it is exhausted. Nothing here type-checks the
// pairing - what the struct buys is co-location: all three are chosen together
// in one literal per trigger, so a line naming the axis its trigger did not
// move is one visible edit rather than two functions apart. Each trigger's
// pairing is pinned by its own test case, and a fourth trigger needs one too.
type resizeAxis struct {
	name   string
	advice string
	raise  func(sizing) sizing
}

// resizeSummary names exactly the axes that moved, in the words the card log
// uses for them.
func resizeSummary(from, to sizing) string {
	switch {
	case from.Bar != to.Bar && from.Budget != to.Budget:
		return fmt.Sprintf("model bar %s -> %s and turn budget %s -> %s",
			from.Bar, to.Bar, budgetLabel(from.Budget), budgetLabel(to.Budget))
	case from.Bar != to.Bar:
		return fmt.Sprintf("model bar %s -> %s", from.Bar, to.Bar)
	default:
		return fmt.Sprintf("turn budget %s -> %s", budgetLabel(from.Budget), budgetLabel(to.Budget))
	}
}

// pinClause warns that a bar raise under a coder pin cannot change the pick:
// the selector returns an operator pin whatever bar it is asked for, so the
// correction is recorded but inert until the pin is lifted. Empty when no bar
// moved, or when there is no pin the selector would actually honour - the
// caller resolves that before calling, because an unresolvable pin falls
// through to the laddered selection, which the raise does reach.
func pinClause(pin string, from, to sizing) string {
	if pin == "" || from.Bar == to.Bar {
		return ""
	}

	return fmt.Sprintf(" (the coder model is pinned to %s, so the bar raise will not change the pick)", pin)
}

// emitSizingEscalation records one landed correction on the run transcript. A
// nil emitter is a no-op, which is how the orchestrator is wired in tests that
// do not read the transcript.
func (o *run) emitSizingEscalation(sub subtaskRef, from, to sizing, why string) {
	if o.d.Emit == nil {
		return
	}

	axis := "both"

	switch {
	case from.Bar == to.Bar:
		axis = "budget"
	case from.Budget == to.Budget:
		axis = "bar"
	}

	o.d.Emit.Emit(events.Kind(sizingEscalationKind), map[string]any{
		"subtask":     sub.ID,
		"axis":        axis,
		"from_bar":    string(from.Bar),
		"to_bar":      string(to.Bar),
		"from_budget": from.Budget,
		"to_budget":   to.Budget,
		"why":         why,
	})
}

// raiseSubtaskBudget records volume evidence: the work did not fit in the turns
// it had. It says nothing about whether the model was capable of doing it, so
// the bar is untouched.
func (o *run) raiseSubtaskBudget(ctx context.Context, sub subtaskRef, why string) {
	o.resizeSubtask(ctx, sub, why, resizeAxis{
		name: "turn budget",
		// The ladder scales the operator's configured base, so lifting the base
		// lifts every rung - the ceiling included.
		advice: "Split the subtask or raise the configured turn cap.",
		raise:  sizing.raiseBudget,
	})
}

// logEnvironmentalVerify names, on the card, a verify failure that carried the
// container resource-exhaustion signature: sizing and the leaderboard are
// untouched.
func (o *run) logEnvironmentalVerify(ctx context.Context, sub subtaskRef) {
	o.d.logCard(ctx, "subtask %s: the verify failed under container resource exhaustion - treated as environmental; sizing and model outcome unchanged", sub.ID)
}

// logEnvironmentalCapBudget records, card-log only, that a turn cap whose
// verify outcome was environmental (resource-exhausted or skipped) left the
// turn budget exactly where it was. Unlike raiseSubtaskBudget this makes no
// board write and computes no raised value: salvageSoloCapped runs after the
// attempt that hit the cap has already ended - the solo path has no
// in-process retry - so nothing in this run or any future one would ever read
// an in-run-only figure. Naming a target the budget "widened to" would be a
// card-log claim with nothing behind it; naming the UNCHANGED value is the
// honest line. It reads the LIVE marker, matching resizeSubtask's own
// rationale, so a resumed run's possibly-empty in-memory sizing does not
// misname the figure.
func (o *run) logEnvironmentalCapBudget(ctx context.Context, sub subtaskRef) {
	tc, err := o.d.Ops.GetTaskContext(ctx, sub.ID, false)
	if err != nil {
		slog.Warn("environmental cap budget: body fetch failed; skipping the log line",
			"card_id", o.d.Cfg.CardID, "subtask_id", sub.ID, "error", err)

		return
	}

	_, s := readMeta(tc.Description)

	o.d.logCard(ctx, "subtask %s: the turn cap was environmental this attempt; the card's turn budget stays at %s",
		sub.ID, budgetLabel(s.Budget))
}

// raiseSubtaskBar records quality evidence: what the model produced was wrong.
// The turn budget is untouched - a model that finished and was wrong did not
// run out of room.
func (o *run) raiseSubtaskBar(ctx context.Context, sub subtaskRef, why string) {
	o.resizeSubtask(ctx, sub, why, resizeAxis{
		name: "model bar",
		// The top rung has no stronger tier above it, so the remaining lever is an
		// explicit coder pin - the one selection path that ignores the ladder.
		advice: "Split the subtask or pin a coder model for it.",
		raise:  sizing.raiseBar,
	})
}

// raiseSubtaskBoth records volume AND quality evidence arriving together: the
// coder spent its whole turn window, and the work it produced fails the
// project's own verify. Both triggers for it are that shape - a cap that parked
// the run and a window spent by a coder that still reached its terminal call -
// because the grace turn makes the second arrive with no error at all.
func (o *run) raiseSubtaskBoth(ctx context.Context, sub subtaskRef, why string) {
	o.resizeSubtask(ctx, sub, why, resizeAxis{
		name:   "turn budget and model bar",
		advice: "Split the subtask, raise the configured turn cap, or pin a coder model for it.",
		raise:  func(s sizing) sizing { return s.raiseBudget().raiseBar() },
	})
}

// applyPrecommitVerifyEvidence raises the subtask's sizing after a still-red
// gate: a genuine failure is quality evidence (bar); a failure by a coder that
// also spent its whole window is volume evidence too (bar and budget). An
// environmental failure raises nothing, matching executeClaimedWith's
// suppression of the outcome row. The raised marker is read by the next run.
func (o *run) applyPrecommitVerifyEvidence(ctx context.Context, sub subtaskRef, environmental, exhausted bool) {
	if environmental {
		return
	}

	if exhausted {
		o.raiseSubtaskBoth(ctx, sub,
			"the coder spent its whole turn budget and the pre-commit verify still failed after a fix pass")

		return
	}

	o.raiseSubtaskBar(ctx, sub, "the pre-commit verify still failed after a fix pass")
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
