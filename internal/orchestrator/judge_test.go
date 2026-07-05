package orchestrator

import (
	"context"
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
// git handle and a distinct worktree dir the verify stub keys on.
func judgeCandidate(idx int, model, dir, diff string) *candidate {
	return &candidate{
		idx:   idx,
		model: model,
		dir:   dir,
		git:   &diffGit{fakeGit: &fakeGit{}, diff: diff},
	}
}

// newJudgeRun wires a run for the judge phase: BestOfN=2, a scripted verify stub
// keyed by candidate dir, and the given candidates pre-populated.
func newJudgeRun(t *testing.T, ops *fakeOps, client llm.LLM, cands []*candidate, verify map[string]bool) *run {
	t.Helper()

	d := planTestDeps(ops, client)
	d.Git = &fakeGit{}
	d.Cfg.BestOfN = 2
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
	o := newJudgeRun(t, ops, client, cands, verify)

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
	o := newJudgeRun(t, ops, client, cands, verify)

	require.NoError(t, runJudge(context.Background(), o))

	assert.Empty(t, client.tasks, "auto-win short-circuits the LLM judge")
	require.NotNil(t, o.winner)
	assert.Equal(t, 1, o.winner.idx)
	assert.Empty(t, o.judgeModel, "auto-win records no judge model")
	assert.True(t, ops.loggedContains("auto-win"), "auto-win is logged; logs=%v", ops.logs)
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
	o := newJudgeRun(t, ops, client, cands, verify)

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
	o := newJudgeRun(t, ops, client, cands, verify)

	require.NoError(t, runJudge(context.Background(), o))

	assert.Len(t, client.tasks, 2, "the judge takes one repair attempt before falling back")
	require.NotNil(t, o.winner)
	assert.Equal(t, 1, o.winner.idx, "fallback picks pool[0], the first verifying candidate")
	assert.Empty(t, o.judgeModel, "no usable verdict means no recorded judge model")
	assert.True(t, ops.loggedContains("falling back"), "the fallback is logged; logs=%v", ops.logs)
}
