package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/harness"
)

// candidate is one Best-of-N implementation of the shared plan: a model working
// in its own container-local worktree, off the board and never pushed. The judge
// phase (a later task) fills the verify/diff fields and picks a winner; a non-nil
// err means the candidate was dropped before judging.
type candidate struct {
	idx       int // 1-based
	model     string
	branch    string // cm/<cardid>-cK, container-local only
	dir       string // <workspace>/.worktrees/cK — NOTE: workspace IS the clone dir
	git       GitOps
	ledger    *Ledger
	completed []subtaskRef
	err       error // non-nil = dropped before judging

	verifyOut string //nolint:unused // set by the judge phase when it verifies a candidate.
	verifyOK  bool   //nolint:unused // set by the judge phase when it verifies a candidate.
	diff      string //nolint:unused // set by the judge phase from the candidate worktree.
	diffStat  string //nolint:unused // set by the judge phase from the candidate worktree.
}

// effectiveCeiling scales the run's budget ceiling for Best-of-N: N execute
// allowances plus one for the shared phases (plan/judge/document/review/
// integrate). Deterministic from the card field, so resumes recompute it. For
// BestOfN < 2 it degenerates to MaxCardCost, keeping single-solver runs
// byte-identical.
func effectiveCeiling(cfg Config) float64 {
	if cfg.MaxCardCost <= 0 {
		return cfg.MaxCardCost
	}

	if cfg.BestOfN >= 2 {
		return cfg.MaxCardCost * float64(cfg.BestOfN+1)
	}

	return cfg.MaxCardCost
}

// degradeN shrinks the fan-out to what the remaining budget can fund: each
// candidate notionally needs one full MaxCardCost allowance. Never below 1.
// With the budget disabled (MaxCardCost <= 0) the configured N is used as is.
func degradeN(cfg Config, reported float64) int {
	if cfg.MaxCardCost <= 0 {
		return cfg.BestOfN
	}

	remaining := effectiveCeiling(cfg) - reported

	n := int(remaining / cfg.MaxCardCost)
	if n < 1 {
		n = 1
	}

	if n > cfg.BestOfN {
		n = cfg.BestOfN
	}

	return n
}

// runFanout races nEff candidate implementations of the shared plan, each in its
// own worktree on a container-local branch, and fills o.candidates. Candidates
// make no subtask board writes and never push — their work is judged in place.
// It returns an error only when EVERY candidate dropped out; a single survivor is
// enough for the judge phase to proceed.
func (o *run) runFanout(ctx context.Context) error {
	cfg := o.d.Cfg

	nEff := degradeN(cfg, o.tc.ReportedCostUSD)
	if nEff < cfg.BestOfN {
		_ = o.d.Ops.AddLog(ctx, cfg.CardID, fmt.Sprintf( //nolint:errcheck
			"best-of-n: remaining budget funds %d candidate(s); reduced to %d from %d", nEff, nEff, cfg.BestOfN))
	}

	if err := o.d.Git.DisableAutoGC(ctx); err != nil {
		return fmt.Errorf("disable auto-gc: %w", err)
	}

	pin := ""
	if resolvePin(o.d.Registry, o.tc.ModelCoder) {
		pin = o.tc.ModelCoder
	}

	specs := o.d.Registry.SelectCandidateModels(registry.SelectInput{
		Role:      registry.RoleCoder,
		Tier:      tierFromString(o.cardTier),
		EstTokens: estimateTokens(o.tc.Description),
		Exclude:   o.excluded,
	}, nEff, pin)

	ordered, err := topoOrder(o.subtasks)
	if err != nil {
		return fmt.Errorf("order subtasks: %w", err)
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
			model:  specs[i].Model,
			branch: branch,
			dir:    dir,
			git:    o.d.GitForDir(dir),
			ledger: NewLedger(cfg.MaxCardCost, 0),
		}

		_ = o.d.Ops.AddLog(ctx, cfg.CardID, fmt.Sprintf( //nolint:errcheck
			"best-of-n: candidate %d/%d starting (model %s)", idx, nEff, o.candidates[i].model))
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
		wg.Add(1)

		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					cand.err = fmt.Errorf("candidate %d panic: %v", cand.idx, r)
				}
			}()

			cand.err = o.runCandidate(ctx, cand, ordered, nEff)
		}()
	}

	wg.Wait()

	for _, c := range o.candidates {
		if c.err == nil {
			return nil
		}
	}

	return fmt.Errorf("best-of-n: all %d candidates failed; first error: %w", nEff, o.candidates[0].err)
}

// runCandidate runs every subtask sequentially through a candidate solver: a
// worktree-rooted git and toolset, the candidate's own ledger and fixed model,
// with board ops and pushes disabled. Mid-run user notes (HITL) are prepended to
// each subtask body before it runs. It records the executed subtasks on the
// candidate and returns the first subtask error, which drops the candidate.
func (o *run) runCandidate(ctx context.Context, c *candidate, ordered []subtaskRef, nEff int) error {
	model := c.model

	sc := &solverCtx{
		git:        c.git,
		ledger:     c.ledger,
		tools:      o.d.WriteToolsForDir(c.dir),
		workspace:  c.dir,
		coderModel: func(subtaskRef, string) string { return model },
		boardOps:   false,
		push:       false,
		tag:        fmt.Sprintf("candidate %d/%d (%s)", c.idx, nEff, model),
	}

	for si, sub := range ordered {
		if msg := o.notes.unseen(c.idx); msg != "" {
			sub.Body += "\n\n## Mid-run user note\n\n" + msg
		}

		if err := o.executeSubtaskWith(ctx, sc, sub); err != nil {
			return err
		}

		_ = o.d.Ops.AddLog(ctx, o.d.Cfg.CardID, fmt.Sprintf( //nolint:errcheck
			"best-of-n: candidate %d/%d (%s): subtask %d/%d done", c.idx, nEff, model, si+1, len(ordered)))
	}

	c.completed = sc.completed

	return nil
}

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
