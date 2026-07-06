package orchestrator

import (
	"context"
	"fmt"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// diffGit is a fakeGit whose Diff returns a preset per-candidate string, so the
// judge tests can assert which candidate diffs reached the prompt. DiffStat is
// left to the embedded fake (returns "").
type diffGit struct {
	*fakeGit
	diff string
}

func (g *diffGit) Diff(ctx context.Context, base string) (string, error) {
	_, _ = g.fakeGit.Diff(ctx, base) // record the call + base on the embedded fake

	return g.diff, nil
}

// judgeCandidate builds a survivor candidate (err == nil) with a diff-returning
// git handle, a distinct worktree dir the verify stub keys on, and a ledger
// pre-loaded with zero spend (the adoption tail reads c.ledger.Spent() for
// every candidate, so it must never be nil).
func judgeCandidate(idx int, model, dir, diff string) *candidate {
	return &candidate{
		idx:    idx,
		model:  model,
		dir:    dir,
		git:    &diffGit{fakeGit: &fakeGit{}, diff: diff},
		ledger: NewLedger(0, 0),
	}
}

// newJudgeRun wires a run for the judge phase: BestOfN=2, a scripted verify stub
// keyed by candidate dir, and the given candidates pre-populated. mainGit is the
// run's main (superproject) git handle — distinct from each candidate's own
// worktree-rooted git — so adoption tests can assert the hard-reset/push/
// cleanup calls landed on the right handle.
func newJudgeRun(t *testing.T, ops *fakeOps, mainGit *fakeGit, client llm.LLM, cands []*candidate, verify map[string]bool) *run {
	t.Helper()

	d := planTestDeps(ops, client)
	d.Git = mainGit
	d.Cfg.BestOfN = 2
	d.Cfg.Branch = "cm/card-1"
	d.Cfg.BaseBranch = "main"
	d.Cfg.Workspace = t.TempDir()

	o := newRun(d, cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body"})
	o.cardTier = "moderate"
	o.candidates = cands
	o.runVerify = func(_ context.Context, dir string, _ []string) (string, bool) {
		if verify[dir] {
			return "", true
		}

		return "FAIL: verify failed in " + dir + "\nexit status 1", false
	}

	return o
}

func TestPhaseOrderContainsJudge(t *testing.T) {
	require.Equal(t, "judge", phaseOrder[2], "judge sits between execute and document")

	ops := &fakeOps{taskContext: cmclient.TaskContext{CardID: "CARD-1"}}
	d := Deps{Ops: ops, Git: &fakeGit{}, Cfg: Config{CardID: "CARD-1"}}
	o := newRun(d, ops.taskContext)

	assert.NotNil(t, o.phaseFnFor("judge"), "phaseFnFor must resolve the judge phase")
}

func TestJudgeNoopWhenOff(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{}
	d := planTestDeps(ops, client)
	d.Git = &fakeGit{}
	// BestOfN unset (0): the judge is a strict no-op.
	o := newRun(d, cmclient.TaskContext{CardID: "CARD-1", Title: "P", Description: "b"})

	require.NoError(t, runJudge(context.Background(), o))
	assert.Nil(t, o.winner, "no winner picked when best-of-n is off")
	assert.Empty(t, client.tasks, "no model call when best-of-n is off")
	assert.False(t, ops.loggedContains("judge phase started"), "no announcement when best-of-n is off")
}

// TestJudgePhaseStartAndVerifyProgressLogged proves the judge narrates the
// previously-silent window between the fan-out join and the verdict: a phase
// start line before the serial verifies, and one progress line per survivor.
func TestJudgePhaseStartAndVerifyProgressLogged(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{
		stopResp(`{"winner": 1, "ranking": [1, 2], "rationale": "c1 wins.", "notes": []}`, 0.02),
	}}
	cands := []*candidate{
		judgeCandidate(1, "coder/one", "dir-c1", "DIFF_ONE"),
		judgeCandidate(2, "coder/two", "dir-c2", "DIFF_TWO"),
	}
	verify := map[string]bool{"dir-c1": true, "dir-c2": true}
	o := newJudgeRun(t, ops, &fakeGit{}, client, cands, verify)

	require.NoError(t, runJudge(context.Background(), o))

	assert.True(t, ops.loggedContains("best-of-n: judge phase started — verifying 2 candidate(s) before comparison"),
		"the judge announces itself before verifying; logs=%v", ops.logs)
	assert.True(t, ops.loggedContains("best-of-n: verifying candidate 1 (coder/one) — 1 of 2"),
		"per-candidate verify progress is logged; logs=%v", ops.logs)
	assert.True(t, ops.loggedContains("best-of-n: verifying candidate 2 (coder/two) — 2 of 2"),
		"per-candidate verify progress is logged; logs=%v", ops.logs)
}

// TestJudgeOutcomeLabels pins the report-table labels, in particular that a
// turn-capped candidate is labeled honestly instead of "failed (build)".
func TestJudgeOutcomeLabels(t *testing.T) {
	win := &candidate{idx: 1}
	o := &run{winner: win}

	tests := []struct {
		name string
		c    *candidate
		want string
	}{
		{"winner", win, "win"},
		{"turn cap", &candidate{err: &MaxTurnsError{Model: "m/x", Turns: 30}}, "failed (turn cap)"},
		{"wrapped turn cap", &candidate{err: fmt.Errorf("subtask 2: %w", &MaxTurnsError{Model: "m/x", Turns: 30})}, "failed (turn cap)"},
		{"other error", &candidate{err: assertErr("disk full")}, "failed (error)"},
		{"verify pass loses", &candidate{verifyOK: true}, "loss"},
		{"verify fail", &candidate{}, "failed (verify)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, o.judgeOutcome(tt.c))
		})
	}
}

func TestJudgeResumeRemap(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{CardID: "CARD-1", Phase: "judge"}}
	d := Deps{Ops: ops, Git: &fakeGit{}, Cfg: Config{CardID: "CARD-1", Branch: "cm/card-1", BaseBranch: "main"}}
	o := newRun(d, ops.taskContext)

	noop := func(context.Context) error { return nil }
	o.planFn, o.executeFn, o.judgeFn = noop, noop, noop
	o.documentFn, o.reviewFn, o.integrateFn, o.doneFn = noop, noop, noop, noop

	require.NoError(t, o.execute(context.Background()))

	calls := ops.recorded()
	exIdx := indexOfCall(calls, "SetPhase:execute")
	jIdx := indexOfCall(calls, "SetPhase:judge")
	require.GreaterOrEqual(t, exIdx, 0, "resumed judge must re-enter execute; calls=%v", calls)
	require.GreaterOrEqual(t, jIdx, 0, "judge is still walked after the remap; calls=%v", calls)
	assert.Less(t, exIdx, jIdx, "a resumed judge re-races: execute is persisted before judge")
}

func TestJudgeVerifyFilter(t *testing.T) {
	ops := &fakeOps{}
	// The verdict picks pool position 2, which (after c2 is filtered) is c3.
	client := &planLLM{responses: []llm.Response{
		stopResp(`{"winner": 2, "ranking": [2, 1], "rationale": "c3 is cleaner and passes.", "notes": []}`, 0.03),
	}}
	cands := []*candidate{
		judgeCandidate(1, "coder/one", "dir-c1", "DIFF_ONE_marker"),
		judgeCandidate(2, "coder/two", "dir-c2", "DIFF_TWO_marker"),
		judgeCandidate(3, "coder/three", "dir-c3", "DIFF_THREE_marker"),
	}
	verify := map[string]bool{"dir-c1": true, "dir-c2": false, "dir-c3": true}
	o := newJudgeRun(t, ops, &fakeGit{}, client, cands, verify)

	require.NoError(t, runJudge(context.Background(), o))

	require.Len(t, client.tasks, 1, "the judge makes exactly one model call")
	prompt := client.tasks[0]
	assert.Contains(t, prompt, "DIFF_ONE_marker", "passing candidate c1's diff is in the prompt")
	assert.Contains(t, prompt, "DIFF_THREE_marker", "passing candidate c3's diff is in the prompt")
	assert.NotContains(t, prompt, "DIFF_TWO_marker", "verify-failed candidate c2 is filtered out")

	require.NotNil(t, o.winner)
	assert.Equal(t, 3, o.winner.idx, "verdict winner=2 maps to pool[1] = candidate c3")
	assert.Equal(t, "coder/three", o.winner.model)
	assert.Equal(t, "default/model", o.judgeModel, "judge ran on the selected reviewer model")
}

func TestJudgeAutoWin(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{}
	cands := []*candidate{
		judgeCandidate(1, "coder/one", "dir-c1", "DIFF_ONE"),
		judgeCandidate(2, "coder/two", "dir-c2", "DIFF_TWO"),
	}
	// Only c1 passes verify -> single-entry pool -> auto-win, no model call.
	verify := map[string]bool{"dir-c1": true, "dir-c2": false}
	o := newJudgeRun(t, ops, &fakeGit{}, client, cands, verify)

	require.NoError(t, runJudge(context.Background(), o))

	assert.Empty(t, client.tasks, "auto-win short-circuits the LLM judge")
	require.NotNil(t, o.winner)
	assert.Equal(t, 1, o.winner.idx)
	assert.Empty(t, o.judgeModel, "auto-win records no judge model")
	assert.True(t, ops.loggedContains("auto-win"), "auto-win is logged; logs=%v", ops.logs)
	assert.True(t, ops.loggedContains("judge phase started"),
		"the start line precedes the pool filter, so auto-win still announces; logs=%v", ops.logs)
}

func TestJudgeAllVerifyFail(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{
		stopResp(`{"winner": 1, "ranking": [1, 2, 3], "rationale": "least broken.", "notes": []}`, 0.02),
	}}
	cands := []*candidate{
		judgeCandidate(1, "coder/one", "dir-c1", "DIFF_ONE"),
		judgeCandidate(2, "coder/two", "dir-c2", "DIFF_TWO"),
		judgeCandidate(3, "coder/three", "dir-c3", "DIFF_THREE"),
	}
	verify := map[string]bool{"dir-c1": false, "dir-c2": false, "dir-c3": false}
	o := newJudgeRun(t, ops, &fakeGit{}, client, cands, verify)

	require.NoError(t, runJudge(context.Background(), o))

	require.Len(t, client.tasks, 1, "no candidate passes, so the judge still runs on all survivors")
	prompt := client.tasks[0]
	assert.Contains(t, prompt, "DIFF_ONE")
	assert.Contains(t, prompt, "DIFF_TWO")
	assert.Contains(t, prompt, "DIFF_THREE")

	require.NotNil(t, o.winner)
	assert.Equal(t, 1, o.winner.idx, "verdict winner=1 maps to pool[0]")
}

func TestJudgeUnparsableVerdictFallsBack(t *testing.T) {
	ops := &fakeOps{}
	// Two non-JSON responses: both parse attempts fail, forcing the fallback.
	client := &planLLM{responses: []llm.Response{
		stopResp("I cannot decide, they all look fine to me.", 0.01),
		stopResp("still no JSON here, sorry.", 0.01),
	}}
	cands := []*candidate{
		judgeCandidate(1, "coder/one", "dir-c1", "DIFF_ONE"),
		judgeCandidate(2, "coder/two", "dir-c2", "DIFF_TWO"),
	}
	verify := map[string]bool{"dir-c1": true, "dir-c2": true}
	o := newJudgeRun(t, ops, &fakeGit{}, client, cands, verify)

	require.NoError(t, runJudge(context.Background(), o))

	assert.Len(t, client.tasks, 2, "the judge takes one repair attempt before falling back")
	require.NotNil(t, o.winner)
	assert.Equal(t, 1, o.winner.idx, "fallback picks pool[0], the first verifying candidate")
	assert.Empty(t, o.judgeModel, "no usable verdict means no recorded judge model")
	assert.True(t, ops.loggedContains("falling back"), "the fallback is logged; logs=%v", ops.logs)
}

// TestJudgeExcludesCandidateModels proves the judge never runs a candidate's own
// coder model (a model must not judge its own work): with both candidate models
// recorded in o.coderModels, the reviewer selection falls past them.
func TestJudgeExcludesCandidateModels(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{
		stopResp(`{"winner": 1, "ranking": [1, 2], "rationale": "c1 wins.", "notes": []}`, 0.02),
	}}
	cands := []*candidate{
		judgeCandidate(1, "alpha/coder", "dir-c1", "DIFF_ONE"),
		judgeCandidate(2, "beta/coder", "dir-c2", "DIFF_TWO"),
	}
	verify := map[string]bool{"dir-c1": true, "dir-c2": true}
	o := newJudgeRun(t, ops, &fakeGit{}, client, cands, verify)
	o.d.Registry = twoCoderRegistry()
	// Post-fan-out, every candidate's coder model is recorded here; the judge must
	// exclude them via reviewExclusions.
	o.coderModels = map[string]bool{"alpha/coder": true, "beta/coder": true}

	require.NoError(t, runJudge(context.Background(), o))

	// alpha/coder is the highest-prior reviewer, but it coded a candidate. With both
	// candidate models excluded the only model left is the capable default — so the
	// judge is NOT alpha/coder (which excluding only o.excluded would have picked).
	assert.NotEqual(t, "alpha/coder", o.judgeModel, "the judge must not run a candidate's own coder model")
	assert.NotEqual(t, "beta/coder", o.judgeModel, "the judge must not run a candidate's own coder model")
	assert.Equal(t, "capable/default", o.judgeModel, "both candidate models excluded -> only the capable default remains")
}

// TestJudgeEmptyPoolErrors covers the M1 insurance guard directly: a pool with no
// survivors (every candidate dropped before judging) errors rather than indexing
// pool[0]. runFanout never reaches judge with zero survivors today, so this drives
// runJudge with a hand-built all-dropped slice.
func TestJudgeEmptyPoolErrors(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{}
	c1 := judgeCandidate(1, "coder/one", "dir-c1", "")
	c2 := judgeCandidate(2, "coder/two", "dir-c2", "")
	c1.err = assertErr("build failed")
	c2.err = assertErr("build failed")
	o := newJudgeRun(t, ops, &fakeGit{}, client, []*candidate{c1, c2}, map[string]bool{})

	err := runJudge(context.Background(), o)
	require.Error(t, err, "an empty pool must error, not panic on pool[0]")
	assert.Contains(t, err.Error(), "no candidates to judge")
	assert.Empty(t, client.tasks, "no judge model call on an empty pool")
	assert.Nil(t, o.winner, "no winner on an empty pool")
}

// TestJudgeReportRendersAssessments proves the parsed per-candidate assessments
// are rendered beneath the report table, with the pool-position clarifier.
func TestJudgeReportRendersAssessments(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{
		stopResp(`{"winner": 1, "ranking": [1, 2], "rationale": "c1 is cleaner.", "notes": [{"candidate": 1, "assessment": "solid and minimal"}, {"candidate": 2, "assessment": "overcomplicated"}]}`, 0.02),
	}}
	cands := []*candidate{
		judgeCandidate(1, "coder/one", "dir-c1", "DIFF_ONE"),
		judgeCandidate(2, "coder/two", "dir-c2", "DIFF_TWO"),
	}
	verify := map[string]bool{"dir-c1": true, "dir-c2": true}
	o := newJudgeRun(t, ops, &fakeGit{}, client, cands, verify)

	require.NoError(t, runJudge(context.Background(), o))

	body := ops.lastBody()
	require.Contains(t, body, "## Best-of-N Report", "the report body was recorded")
	assert.Contains(t, body, "### Assessments")
	assert.Contains(t, body, "candidate numbers in judge text are pool positions")
	assert.Contains(t, body, "- Candidate 1: solid and minimal")
	assert.Contains(t, body, "- Candidate 2: overcomplicated")
}

// adoptionCandidate builds on judgeCandidate with the extra fields the
// adoption tail reads: a distinct container-local branch (mirroring the
// fan-out's cm/<card>-cK convention) and a ledger pre-loaded with the
// candidate's spend.
func adoptionCandidate(idx int, model, dir, diff, branch string, spentUSD float64) *candidate {
	c := judgeCandidate(idx, model, dir, diff)
	c.branch = branch
	c.ledger = NewLedger(0, spentUSD)

	return c
}

// TestWinnerAdoption exercises the full adoption tail at the end of runJudge:
// the winner (c2) is hard-reset onto the main card branch and pushed, its
// completed subtasks are replayed to the board in order, every candidate's
// worktree/branch is cleaned up on the MAIN git (winner and dropped candidate
// included), and one outcome row per candidate is reported in a single call.
func TestWinnerAdoption(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	// Verdict picks pool position 2. c3 drops out before judging (err != nil),
	// so the pool is only [c1, c2] and pool[1] is c2.
	client := &planLLM{responses: []llm.Response{
		stopResp(`{"winner": 2, "ranking": [2, 1], "rationale": "c2 is the cleanest.", "notes": []}`, 0.04),
	}}

	c1 := adoptionCandidate(1, "coder/one", "dir-c1", "DIFF_ONE", "cm/card-1-c1", 0.01)
	c2 := adoptionCandidate(2, "coder/two", "dir-c2", "DIFF_TWO", "cm/card-1-c2", 0.02)
	c3 := adoptionCandidate(3, "coder/three", "dir-c3", "DIFF_THREE", "cm/card-1-c3", 0.03)
	c3.err = assertErr("candidate 3 build failed")

	c2.completed = []subtaskRef{{ID: "SUB-1", Title: "First"}, {ID: "SUB-2", Title: "Second"}}

	// c2's worktree git reports a distinct head SHA; the adoption must hard-reset
	// the MAIN branch to it.
	c2Git, ok := c2.git.(*diffGit)
	require.True(t, ok, "judgeCandidate must build a *diffGit handle")

	c2Git.headSHA = "c2-winner-sha"

	verify := map[string]bool{"dir-c1": true, "dir-c2": true}
	o := newJudgeRun(t, ops, mainGit, client, []*candidate{c1, c2, c3}, verify)

	// A fan-out heartbeater is still running when adoption starts (the judge
	// span is covered by it); adoption must stop it before replaying the
	// subtasks, or completed cards would keep receiving heartbeats.
	hbStopped := false
	o.stopSubHB = func() { hbStopped = true }

	require.NoError(t, runJudge(context.Background(), o))

	assert.True(t, hbStopped, "adoption stops the fan-out heartbeater")
	assert.Nil(t, o.stopSubHB, "the stop func is cleared so a second stop is a no-op")

	require.NotNil(t, o.winner)
	assert.Same(t, c2, o.winner, "verdict winner=2 maps to pool[1] = c2 once c3 is excluded")

	// (1) Main branch hard-reset to the winner's head, then the run's first push.
	mainGit.assertOrder(t, "HardReset:c2-winner-sha", "Push:cm/card-1")
	assert.Equal(t, []string{"c2-winner-sha"}, mainGit.hardResetRefs)
	assert.Equal(t, []string{"cm/card-1"}, mainGit.pushBranches, "the run's first push")

	// (2) Winner's completed subtasks replayed to the board, in order.
	opCalls := ops.recorded()
	i1 := indexOfCall(opCalls, "ClaimCard:SUB-1")
	i2 := indexOfCall(opCalls, "CompleteTask:SUB-1")
	i3 := indexOfCall(opCalls, "ClaimCard:SUB-2")
	i4 := indexOfCall(opCalls, "CompleteTask:SUB-2")
	require.True(t, i1 >= 0 && i2 >= 0 && i3 >= 0 && i4 >= 0, "opCalls=%v", opCalls)
	assert.True(t, i1 < i2 && i2 < i3 && i3 < i4, "subtasks must be claimed/completed in order; opCalls=%v", opCalls)

	// (3) Every candidate's worktree and branch cleaned up, on the MAIN git,
	// including the winner and the dropped candidate.
	assert.Equal(t, []string{"dir-c1", "dir-c2", "dir-c3"}, mainGit.removedWorktrees)
	assert.Equal(t, []string{"cm/card-1-c1", "cm/card-1-c2", "cm/card-1-c3"}, mainGit.deletedBranches)

	// (4) Outcomes reported once, one row per candidate.
	require.Len(t, ops.reportOutcomes, 1)
	rows := ops.reportOutcomes[0]
	require.Len(t, rows, 3)

	byModel := map[string]cmclient.ModelOutcome{}
	for _, row := range rows {
		byModel[row.Model] = row
	}

	assert.Equal(t, "win", byModel["coder/two"].Result)
	assert.Equal(t, "loss", byModel["coder/one"].Result)
	assert.Equal(t, "failed", byModel["coder/three"].Result)

	for model, row := range byModel {
		assert.Equal(t, 3, row.NCandidates, "model %s: NCandidates must count every candidate", model)
		assert.Equal(t, "default/model", row.JudgeModel, "model %s: judge slug recorded on every row", model)
	}

	assert.InDelta(t, 0.01, byModel["coder/one"].CostUSD, 1e-9)
	assert.InDelta(t, 0.02, byModel["coder/two"].CostUSD, 1e-9)
	assert.InDelta(t, 0.03, byModel["coder/three"].CostUSD, 1e-9)
	assert.True(t, byModel["coder/two"].VerifyPass)
	assert.False(t, byModel["coder/three"].VerifyPass, "c3 never verified")
}

// TestAdoptionOutcomeReportBestEffort proves a failed outcome report does not
// fail the run: the rest of the adoption tail (adopt, push, cleanup) still
// completes, and the report is still attempted.
func TestAdoptionOutcomeReportBestEffort(t *testing.T) {
	ops := &fakeOps{reportOutcomesErr: assertErr("cm unreachable")}
	mainGit := &fakeGit{}
	client := &planLLM{}

	c1 := adoptionCandidate(1, "coder/one", "dir-c1", "DIFF_ONE", "cm/card-1-c1", 0.0)
	c2 := adoptionCandidate(2, "coder/two", "dir-c2", "DIFF_TWO", "cm/card-1-c2", 0.0)
	// Only c1 verifies -> single-entry pool -> auto-win (no model call), which
	// is one of the three paths that sets o.winner and reaches the adoption tail.
	verify := map[string]bool{"dir-c1": true, "dir-c2": false}
	o := newJudgeRun(t, ops, mainGit, client, []*candidate{c1, c2}, verify)

	err := runJudge(context.Background(), o)

	require.NoError(t, err, "a failed outcome report must not fail the run")
	require.NotNil(t, o.winner)
	assert.Equal(t, 1, o.winner.idx)

	assert.NotEmpty(t, mainGit.pushBranches, "the winner is still adopted and pushed")
	assert.NotEmpty(t, mainGit.removedWorktrees, "cleanup still runs")
	require.Len(t, ops.reportOutcomes, 1, "the report call is still attempted despite the scripted error")
}

// TestAdoptionNilCandidateSkipped proves cleanup and outcome reporting are
// nil-safe: a partially-populated fan-out slice (a candidate slot whose
// worktree build aborted before the struct was created) must not panic, and
// its nil slot must not count toward NCandidates — only candidates that
// actually raced do.
func TestAdoptionNilCandidateSkipped(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	client := &planLLM{}

	c1 := adoptionCandidate(1, "coder/one", "dir-c1", "DIFF_ONE", "cm/card-1-c1", 0.0)
	c2 := adoptionCandidate(2, "coder/two", "dir-c2", "DIFF_TWO", "cm/card-1-c2", 0.0)
	verify := map[string]bool{"dir-c1": true, "dir-c2": false}
	o := newJudgeRun(t, ops, mainGit, client, []*candidate{c1, c2, nil}, verify)

	require.NoError(t, runJudge(context.Background(), o), "a nil candidate slot must not panic or fail the run")

	assert.Equal(t, []string{"dir-c1", "dir-c2"}, mainGit.removedWorktrees, "the nil slot is skipped during cleanup")

	require.Len(t, ops.reportOutcomes, 1)
	rows := ops.reportOutcomes[0]
	require.Len(t, rows, 2, "the nil slot produces no outcome row")

	for _, row := range rows {
		assert.Equal(t, 2, row.NCandidates, "NCandidates counts only the non-nil candidates that actually raced")
	}
}
