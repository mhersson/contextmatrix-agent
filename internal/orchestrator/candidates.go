package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/harness"
)

// candidate is one Best-of-N implementation of the shared plan: a model working
// in its own container-local worktree, off the board and never pushed. The judge
// phase fills the verify/diff fields and picks a winner; a non-nil
// err means the candidate was dropped before judging.
type candidate struct {
	idx   int // 1-based
	model string
	// pick is the selection model came out of. It is what lets the judge's
	// outcome rollup tell a candidate that got the bar it asked for from one
	// the ladder walked down. Always written through setPick, so the slug and
	// its provenance cannot drift apart across a re-selection.
	pick      registry.Pick
	branch    string // cm/<cardid>-cK, container-local only
	dir       string // <workspace>/.worktrees/cK - NOTE: workspace IS the clone dir
	git       GitOps
	ledger    *Ledger
	completed []subtaskRef
	capped    bool  // final subtask hit the turn cap; admitted to the judge pool only on a passing verify
	err       error // non-nil = dropped before judging

	// verify is the candidate's tri-state verify result, set by the judge phase.
	// A verifyPassed status implies a command actually ran (an empty/skipped gate
	// never passes), which is what gates capped-work salvage.
	verify   verifyResult
	diff     string // set by the judge phase from the candidate worktree.
	diffStat string // set by the judge phase from the candidate worktree.
}

// setPick records the selection this candidate runs on. The slug and its
// provenance are assigned together and only here, so a re-selection can never
// leave the judge reporting one model's outcome against another model's bar.
func (c *candidate) setPick(p registry.Pick) {
	c.pick = p
	c.model = p.Model
}

// effectiveCeiling scales the run's budget ceiling for Best-of-N (N execute
// allowances plus one for the shared phases) and adds the mob session
// discussion term (BudgetFactor x MaxCardCost - discussions are read-only
// talk, far cheaper than implementations). Deterministic from the card
// fields, so resumes recompute it. With neither feature on it degenerates to
// MaxCardCost, keeping single-solver runs byte-identical.
func effectiveCeiling(cfg Config) float64 {
	if cfg.MaxCardCost <= 0 {
		return cfg.MaxCardCost
	}

	base := cfg.MaxCardCost

	if cfg.BestOfN >= 2 {
		base = cfg.MaxCardCost * float64(cfg.BestOfN+1)
	}

	if cfg.Mob.enabled() {
		base += cfg.MaxCardCost * cfg.Mob.BudgetFactor
	}

	return base
}

// degradeN shrinks the fan-out to what the remaining budget can fund: each
// candidate notionally needs one full MaxCardCost allowance. Never below 1.
// With the budget disabled (MaxCardCost <= 0) the configured N is used as is.
func degradeN(cfg Config, reported float64) int {
	if cfg.MaxCardCost <= 0 {
		return cfg.BestOfN
	}

	remaining := effectiveCeiling(cfg) - reported

	n := min(max(int(remaining/cfg.MaxCardCost), 1), cfg.BestOfN)

	return n
}

// runFanout races nEff candidate implementations of the shared plan, each in its
// own worktree on a container-local branch, and fills o.candidates. Candidates
// make no subtask board writes and never push - their work is judged in place.
// It returns an error only when EVERY candidate dropped out; a single survivor is
// enough for the judge phase to proceed.
func (o *run) runFanout(ctx context.Context) (retErr error) {
	// The fan-out heartbeater deliberately survives a SUCCESSFUL return (the
	// judge phase still holds the first-arrival claims); on any ERROR return
	// the run is parking, so stop it here rather than leaking the goroutine.
	defer func() {
		if retErr != nil {
			o.stopFanoutHeartbeat()
		}
	}()

	cfg := o.d.Cfg

	ordered, err := topoOrder(o.subtasks)
	if err != nil {
		return fmt.Errorf("order subtasks: %w", err)
	}

	// Crash-resume after a completed fan-out: SetPhase persists BEFORE the phase
	// body, so a crash after adoptWinner but before SetPhase("document") resumes at
	// execute -> judge. If every subtask is already board-done the winner was
	// already adopted and replayed; re-racing would burn budget and write duplicate
	// outcome rows. Mirror single-solver skip-if-done and return with NO candidates
	// (runJudge's len(candidates)==0 no-op then flows straight to document) before
	// cutting any worktree.
	if allSubtasksDone(ordered) {
		o.d.logCard(ctx, "best-of-n: every subtask already complete; skipping fan-out (resume after adoption)")

		return nil
	}

	nEff := degradeN(cfg, o.tc.ReportedCostUSD)
	if nEff < cfg.BestOfN {
		o.d.logCard(ctx,
			"best-of-n: remaining budget funds %d candidate(s); reduced to %d from %d", nEff, nEff, cfg.BestOfN)
	}

	if err := o.d.Git.DisableAutoGC(ctx); err != nil {
		return fmt.Errorf("disable auto-gc: %w", err)
	}

	// Keep candidate worktrees out of the parent clone's staging path: a WIP push
	// on a budget/context/turn park runs CommitIfDirty on the main clone while the
	// .worktrees/cK dirs are still present and untracked; without this exclude git
	// stages them as mode-160000 gitlinks onto the card branch. Clone-local and
	// idempotent (stageForCommit also skips the prefix, as belt-and-braces).
	if err := o.d.Git.AddInfoExclude(ctx, ".worktrees/"); err != nil {
		return fmt.Errorf("exclude candidate worktrees: %w", err)
	}

	// First-arrival subtask claims (made as candidates reach each subtask) must
	// stay heartbeated until the winner replay completes them: CM's stall sweep
	// reclaims ANY claimed card whose last_heartbeat lapses, and the race plus
	// the judge's serialized verifies are wall-clock unbounded. One goroutine
	// covers the whole claimed set; it outlives runFanout on purpose (the judge
	// span still holds the claims) and is stopped by adoptWinner before the
	// replay, or on the all-failed exit below. Hard parks mid-judge leave the
	// claims to CM's stall sweep, same as a single-solver crash.
	o.stopSubHB = o.startFanoutHeartbeat(ctx)

	pin := ""
	if resolvePin(o.d.Registry, o.tc.ModelCoder) {
		pin = o.tc.ModelCoder
	} else if o.tc.ModelCoder != "" {
		o.warnUnresolvablePin(ctx, "coder", o.tc.ModelCoder)
	}

	specs := o.d.Registry.SelectCandidateModels(registry.SelectInput{
		Role:      registry.RoleCoder,
		Tier:      o.cardSizing.Bar,
		EstTokens: estimateTokens(o.taskDescription),
		Exclude:   o.excluded,
	}, nEff, pin)
	if len(specs) < nEff {
		return fmt.Errorf("no coder model is selectable for the candidate fan-out")
	}

	// Build every candidate and cut its worktree BEFORE spawning any goroutine, so
	// an AddWorktree failure returns cleanly with nothing running to leak.
	o.candidates = make([]*candidate, nEff)

	for i := range nEff {
		idx := i + 1
		branch := fmt.Sprintf("%s-c%d", cfg.Branch, idx)
		dir := filepath.Join(cfg.Workspace, ".worktrees", fmt.Sprintf("c%d", idx))

		if err := o.d.Git.AddWorktree(ctx, dir, branch, cfg.Branch); err != nil {
			return fmt.Errorf("add worktree c%d: %w", idx, err)
		}

		o.candidates[i] = &candidate{
			idx:    idx,
			branch: branch,
			dir:    dir,
			git:    o.d.GitForDir(dir),
			ledger: NewLedger(cfg.MaxCardCost, 0),
		}
		o.candidates[i].setPick(specs[i])

		o.noteShortfall(ctx, candidatePhase(idx), "", specs[i])

		o.d.logCard(ctx, "best-of-n: candidate %d/%d starting (model %s)", idx, nEff, o.candidates[i].model)
	}

	// HITL: a collector drains mid-run human turns into o.notes and broadcasts
	// them to every live candidate. Started only in interactive mode with a live
	// inbox, and joined (not merely canceled) when the fan-out returns, so no
	// collector goroutine outlives it. o.notes stays nil for autonomous runs,
	// which makes unseen a no-op.
	if cfg.Interactive && o.d.Human != nil {
		o.notes = newUserNotes()

		nctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})

		go func() {
			defer close(done)

			o.notes.collect(nctx, o.d.Human)
		}()

		defer func() {
			cancel()
			<-done
		}()
	}

	var wg sync.WaitGroup

	for _, cand := range o.candidates {
		wg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					cand.err = fmt.Errorf("candidate %d panic: %v", cand.idx, r)
				}
			}()

			cand.err = o.runCandidate(ctx, cand, ordered, nEff)
		})
	}

	wg.Wait()

	// Fold every candidate's spend into the run ledger so the post-fan-out phases
	// (judge/document/review/integrate) budget against the true remaining envelope
	// rather than a pre-fan-out one. Candidates ran on their own separate ledgers,
	// so none of this spend touched o.ledger during the race. Failed candidates
	// burned real tokens too - spend is spend - so their spend counts. This keeps
	// the run under the (N+1)x effectiveCeiling: ~Nx for candidates plus plan,
	// leaving ~1x for the shared tail.
	for _, c := range o.candidates {
		if c != nil {
			o.ledger.Spend(c.ledger.Spent())
		}
	}

	// Name every dropped candidate the moment the join releases. Until here a
	// failing candidate (turn cap, context limit, pool exhaustion, ...) is
	// silent: it would otherwise surface only in the post-judge report table,
	// long after the user watched the fan-out apparently hang on it.
	for _, c := range o.candidates {
		if c != nil && c.err != nil {
			o.d.logCard(ctx, "best-of-n: candidate %d/%d (%s) dropped: %v", c.idx, len(o.candidates), c.model, c.err)
		}
	}

	for _, c := range o.candidates {
		if c.err == nil {
			return nil
		}
	}

	// The run is parking with first-arrival claims still held: release them,
	// mirroring the single-solver error path, so the board shows the truth now
	// instead of a misleading "stalled" 30 minutes later. (The heartbeater is
	// stopped by the deferred error hook before this function returns.)
	o.stopFanoutHeartbeat()

	for _, id := range o.claimedSubIDs() {
		o.releaseSubtask(ctx, id)
	}

	return fmt.Errorf("best-of-n: all %d candidates failed; first error: %w", nEff, o.candidates[0].err)
}

// claimSubtaskOnce claims sub for the run the first time ANY candidate reaches
// it, flipping the board card to in_progress for the duration of the race (the
// MCP claim_card auto-transitions, and CM flips the parent on the first subtask
// claim). Exactly one claim per subtask regardless of how many candidates race;
// best-effort - board visibility must never kill a healthy race. The winner
// replay later re-claims idempotently (same agent identity) and completes.
func (o *run) claimSubtaskOnce(ctx context.Context, sub subtaskRef) {
	o.subClaimMu.Lock()

	if o.claimedSubs == nil {
		o.claimedSubs = make(map[string]bool)
	}

	if o.claimedSubs[sub.ID] {
		o.subClaimMu.Unlock()

		return
	}

	o.claimedSubs[sub.ID] = true
	o.subClaimMu.Unlock()

	// Claim outside the lock: candidates must not serialize on board I/O.
	if err := o.d.Ops.ClaimCard(ctx, sub.ID); err != nil {
		slog.Warn("best-of-n: first-arrival subtask claim failed; board state will lag",
			"card_id", sub.ID, "error", err)
	}
}

// claimedSubIDs snapshots the first-arrival-claimed subtask IDs.
func (o *run) claimedSubIDs() []string {
	o.subClaimMu.Lock()
	defer o.subClaimMu.Unlock()

	ids := make([]string, 0, len(o.claimedSubs))
	for id := range o.claimedSubs {
		ids = append(ids, id)
	}

	return ids
}

// startFanoutHeartbeat ticks ops.Heartbeat for every first-arrival-claimed
// subtask on subtaskHeartbeatInterval until the returned stop func is called.
// Like startSubtaskHeartbeat, stop BLOCKS until the goroutine has exited.
func (o *run) startFanoutHeartbeat(ctx context.Context) func() {
	return StartTicker(ctx, subtaskHeartbeatInterval, func(ctx context.Context) {
		for _, id := range o.claimedSubIDs() {
			if err := o.d.Ops.Heartbeat(ctx, id); err != nil {
				slog.Warn("best-of-n: subtask heartbeat failed", "card_id", id, "error", err)
			}
		}
	})
}

// stopFanoutHeartbeat stops the fan-out heartbeater if one is running.
// Idempotent: the stop func is cleared after the first call.
func (o *run) stopFanoutHeartbeat() {
	if o.stopSubHB != nil {
		o.stopSubHB()
		o.stopSubHB = nil
	}
}

// allSubtasksDone reports whether every subtask is already in a terminal state
// (done or not_planned) - the same skip-if-done / skip-if-not-planned signal
// executeSubtaskWith applies per subtask. An empty set is not terminal: there is
// nothing completed to resume, so the normal (degenerate) path handles it unchanged.
func allSubtasksDone(subs []subtaskRef) bool {
	if len(subs) == 0 {
		return false
	}

	for _, s := range subs {
		if !isTerminal(s.State) {
			return false
		}
	}

	return true
}

// runCandidate runs every subtask sequentially through a candidate solver: a
// worktree-rooted git and toolset, the candidate's own ledger and fixed model,
// with board ops and pushes disabled. Mid-run user notes (HITL) are prepended to
// each subtask body before it runs. It records the executed subtasks on the
// candidate and returns the first subtask error, which drops the candidate.
func (o *run) runCandidate(ctx context.Context, c *candidate, ordered []subtaskRef, nEff int) error {
	// One verify tool per candidate, rooted at that candidate's own worktree and
	// git handle. Candidates write concurrently, so a shared instance would let
	// one candidate's write invalidate another's cache - or report a pass one
	// candidate earned as if it were another's. Nothing records the tool's
	// verdicts here: the pre-commit gate is solo-only, and a candidate is judged
	// by the judge phase's own verify over its worktree.
	sc := &solverCtx{
		git:        c.git,
		ledger:     c.ledger,
		tools:      o.d.WriteToolsForDir(c.dir, o.verifyToolFor(c.git, c.dir, o.resolvedVerifyPlan(), nil)),
		workspace:  c.dir,
		coderModel: o.candidateCoderModel(c),
		boardOps:   false,
		push:       false,
		tag:        fmt.Sprintf("candidate %d/%d", c.idx, nEff),
		lastSubID:  lastSubtaskID(ordered),
	}

	for si, sub := range ordered {
		if msg := o.notes.unseen(c.idx); msg != "" {
			sub.Body += "\n\n## Mid-run user note\n\n" + msg
		}

		if err := o.executeSubtaskWith(ctx, sc, sub); err != nil {
			return err
		}

		o.d.logCard(ctx, "best-of-n: candidate %d/%d (%s): subtask %d/%d done", c.idx, nEff, c.model, si+1, len(ordered))
	}

	c.completed = sc.completed
	c.capped = sc.capped

	return nil
}

// lastSubtaskID returns the final subtask's ID in execution order, or "" for
// an empty plan (salvage disabled).
func lastSubtaskID(subs []subtaskRef) string {
	if len(subs) == 0 {
		return ""
	}

	return subs[len(subs)-1].ID
}

// candidateCoderModel builds candidate c's reselection-aware coder resolver.
// Unlike the fan-out's one-shot selection, it is consulted before EVERY coder
// attempt: while c's current model is viable it is returned unchanged, but once
// recoverIncapable has excluded it (o.excluded) the resolver re-picks the
// next-best coder model for the card tier - mirroring the fan-out selection with
// every dropped model merged into the exclude set - and updates c.model, so a
// candidate that drew a dud model continues on a different one instead of
// hot-looping the same slug and burning the shared reselect cap. c.model
// therefore always reflects the LAST model the candidate ran (what logs and
// outcome rows report). An explicit operator coder pin is never overridden
// (mirroring resolveCoderModel): the pinned candidate keeps the pin, exhausts the
// shared cap, and parks. When nothing is employable for this candidate any more
// it returns errCandidatePoolExhausted, which drops this candidate and leaves
// the others running.
//
// The advisory for a re-pick that fell short of the card tier is raised AFTER
// the selection lock is released, which is why the locked work lives in
// pickCandidateModel: noteShortfall takes its own mutex, and an advisory
// reaching for selMu from inside the lock would deadlock the fan-out. It names
// the subtask the re-pick happened for, which is the first subtask the fresh
// model runs.
func (o *run) candidateCoderModel(c *candidate) func(context.Context, subtaskRef, string) (registry.Pick, error) {
	return func(ctx context.Context, sub subtaskRef, prompt string) (registry.Pick, error) {
		pick, fresh, ok := o.pickCandidateModel(c, prompt)
		if !ok {
			return registry.Pick{}, errCandidatePoolExhausted
		}

		// A candidate staying on the model it already had has nothing new to
		// report; only a fresh re-pick does. c.idx is fixed when the candidate
		// is built, before any fan-out goroutine starts, so naming the seat
		// needs no lock.
		if fresh {
			o.noteShortfall(ctx, candidatePhase(c.idx), sub.ID, pick)
		}

		return pick, nil
	}
}

// pickCandidateModel does candidate c's coder resolution under the selection
// lock and nothing else, so the lock is released by defer and a panic in the
// selector cannot strand it. A leaked selMu would block every other candidate
// forever: the fan-out recovers a candidate panic and carries on, so the run
// would hang rather than fail.
//
// It returns the pick the candidate is now on, which is c's standing pick when
// nothing was re-selected. fresh says whether that pick is new and therefore
// worth reporting once the lock is released - a candidate that kept the model
// it already had (a pin, or a model this run has not excluded) had that model
// reported when it was chosen. ok is false when nothing is employable for this
// candidate any more.
func (o *run) pickCandidateModel(c *candidate, prompt string) (pick registry.Pick, fresh, ok bool) {
	o.selMu.Lock()
	defer o.selMu.Unlock()

	// Never override an explicit operator coder pin (the fan-out assigns it to a
	// single candidate); let a pinned-but-incapable model park via the cap.
	if c.model == o.tc.ModelCoder && resolvePin(o.d.Registry, o.tc.ModelCoder) {
		return c.pick, false, true
	}

	if !o.excluded[c.model] {
		return c.pick, false, true
	}

	next := o.d.Registry.SelectByComplexity(registry.SelectInput{
		Role:      registry.RoleCoder,
		Tier:      o.cardSizing.Bar,
		EstTokens: estimateTokens(prompt),
		Exclude:   o.excluded,
	})

	// Nothing is employable for this candidate any more - every rung is dry
	// and the capable default is excluded too. Drop this candidate; the
	// others carry on.
	if !next.OK {
		return registry.Pick{}, false, false
	}

	c.setPick(next)

	return next, true, true
}

// candidatePhase gives the fan-out's own pick and a later re-pick for the
// same candidate a shared dedupe key in selection advisories.
func candidatePhase(idx int) string { return fmt.Sprintf("best-of-n candidate %d", idx) }

// errCandidatePoolExhausted drops one Best-of-N candidate cleanly: every model
// it could run on is excluded this run, and the other candidates carry on.
var errCandidatePoolExhausted = errors.New("candidate model pool exhausted")

// userNotes fans mid-run human turns out to the live Best-of-N candidates. In
// HITL mode collect drains the inbox into a shared, mutex-guarded buffer; each
// candidate keeps its own cursor, so unseen(idx) returns exactly the notes that
// candidate has not yet folded into a subtask body. A nil *userNotes (autonomous
// fan-out) makes unseen a no-op.
//
// Inbox.Wait is destructive, so a note delivered to the candidates here is not
// separately re-delivered to a later gate; it is retained in this buffer for the
// fan-out's lifetime only. Re-feeding consumed frames to post-judge gates would
// need a non-destructive inbox, which is out of scope for the fan-out.
type userNotes struct {
	mu      sync.Mutex
	msgs    []string
	cursors map[int]int
}

func newUserNotes() *userNotes { return &userNotes{cursors: map[int]int{}} }

// add buffers one human turn, ignoring blank content.
func (n *userNotes) add(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	n.mu.Lock()
	n.msgs = append(n.msgs, text)
	n.mu.Unlock()
}

// collect drains the inbox into the note buffer until ctx is canceled or the
// inbox closes (both surface as a Wait error). Run as a goroutine by runFanout in
// HITL mode; stopped by canceling the derived context on return.
func (n *userNotes) collect(ctx context.Context, inbox harness.Inbox) {
	for {
		msg, err := inbox.Wait(ctx)
		if err != nil {
			return
		}

		n.add(msg.Content)
	}
}

// unseen returns the notes candidate idx has not consumed, joined by blank lines,
// and advances that candidate's cursor. A nil receiver returns "".
func (n *userNotes) unseen(idx int) string {
	if n == nil {
		return ""
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	seen := n.cursors[idx]
	if seen >= len(n.msgs) {
		return ""
	}

	fresh := strings.Join(n.msgs[seen:], "\n\n")
	n.cursors[idx] = len(n.msgs)

	return fresh
}
