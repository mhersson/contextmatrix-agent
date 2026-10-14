package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
)

// candidateSolver builds a solver shaped like a Best-of-N candidate: no board,
// no push, no head capture, so the only Head read is the one after a commit.
func candidateSolver(o *run, d Deps, git *fakeGit) *solverCtx {
	return &solverCtx{
		git: git, ledger: NewLedger(0, 0), tools: d.WriteTools,
		workspace: "ws", coderModel: o.resolveCoderModel,
		tag: "candidate 1/1",
	}
}

func TestPriorSubtasksRecordCommitShaAndSubject(t *testing.T) {
	git := &fakeGit{committed: true, headSHAs: []string{"aaaaaaa1111111", "bbbbbbb2222222"}}
	client := &planLLM{responses: []llm.Response{
		finishResp("feat(a): add a\n\nbody", 0.01),
		finishResp("feat(b): add b", 0.01),
	}}
	d := execTestDeps(&fakeOps{}, git, client)
	o := newExecRun(d, nil, 0)
	sc := candidateSolver(o, d, git)

	subA := subtaskRef{ID: "SUB-1", Title: "Add a", Sizing: seedSizing("simple")}
	require.NoError(t, o.executeSubtaskWith(context.Background(), sc, subA))

	assert.Equal(t, []priorSubtask{
		{ID: "SUB-1", Title: "Add a", Kind: priorCommitted, SHA: "aaaaaaa", Subject: "feat(a): add a"},
	}, sc.prior, "the ledger subtask B is handed carries A's short sha and subject")

	subB := subtaskRef{ID: "SUB-2", Title: "Add b", Sizing: seedSizing("simple")}
	require.NoError(t, o.executeSubtaskWith(context.Background(), sc, subB))

	require.Len(t, sc.prior, 2)
	assert.Equal(t, priorSubtask{ID: "SUB-2", Title: "Add b", Kind: priorCommitted, SHA: "bbbbbbb", Subject: "feat(b): add b"}, sc.prior[1])
}

func TestPriorSubtasksRecordTerminalSkips(t *testing.T) {
	git := &fakeGit{committed: true}
	d := execTestDeps(&fakeOps{}, git, &planLLM{})
	o := newExecRun(d, nil, 0)
	sc := candidateSolver(o, d, git)

	require.NoError(t, o.executeSubtaskWith(context.Background(), sc,
		subtaskRef{ID: "SUB-1", Title: "Done one", State: "done"}))
	require.NoError(t, o.executeSubtaskWith(context.Background(), sc,
		subtaskRef{ID: "SUB-2", Title: "Cancelled", State: "not_planned"}))

	assert.Equal(t, []priorSubtask{
		{ID: "SUB-1", Title: "Done one", Kind: priorDoneEarlier},
		{ID: "SUB-2", Title: "Cancelled", Kind: priorNotPlanned},
	}, sc.prior)
	assert.Empty(t, sc.completed, "a skipped subtask is not an executed one")
}

func TestPriorSubtasksRecordNoCommitOnCleanCandidateTree(t *testing.T) {
	git := &fakeGit{committed: false, headSHA: "ccccccc3333333"}
	client := &planLLM{responses: []llm.Response{finishResp("feat: nothing", 0.01)}}
	d := execTestDeps(&fakeOps{}, git, client)
	o := newExecRun(d, nil, 0)
	sc := candidateSolver(o, d, git)

	require.NoError(t, o.executeSubtaskWith(context.Background(), sc,
		subtaskRef{ID: "SUB-1", Title: "Nothing", Sizing: seedSizing("simple")}))

	assert.Equal(t, []priorSubtask{{ID: "SUB-1", Title: "Nothing", Kind: priorNoCommit}}, sc.prior,
		"a clean tree must not be claimed as committed work")
}

func TestPriorSubtasksRecordCandidateSalvageCommit(t *testing.T) {
	git := &fakeGit{committed: true, headSHA: "ddddddd4444444"}
	d := execTestDeps(&fakeOps{}, git, &planLLM{responses: burnResps(5)})
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, nil, 0)
	sc := candidateSolver(o, d, git)
	sc.lastSubID = "SUB-1"

	require.NoError(t, o.executeSubtaskWith(context.Background(), sc,
		subtaskRef{ID: "SUB-1", Title: "Final", Sizing: seedSizing("simple")}))

	assert.Equal(t, []priorSubtask{
		{ID: "SUB-1", Title: "Final", Kind: priorCommitted, SHA: "ddddddd", Subject: "feat: final"},
	}, sc.prior)
}

func TestPriorSubtasksRecordSoloSalvageCommit(t *testing.T) {
	git := &fakeGit{committed: true, headSHAs: []string{"base000", "eeeeeee5555555"}}
	d := execTestDeps(&fakeOps{}, git, &planLLM{responses: burnResps(5)})
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Sizing: seedSizing("simple")}}, 0)
	seedResolvedVerifyPlan(o)
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 0}
	}

	require.NoError(t, runExecute(context.Background(), o))

	require.Len(t, o.solver.prior, 1)
	assert.Equal(t, priorCommitted, o.solver.prior[0].Kind)
	assert.Equal(t, "eeeeeee", o.solver.prior[0].SHA)
}

func TestPriorSubtasksLedgerIsPerSolver(t *testing.T) {
	git := &fakeGit{committed: true, headSHA: "fffffff6666666"}
	client := &planLLM{responses: []llm.Response{finishResp("feat: x", 0.01)}}
	d := execTestDeps(&fakeOps{}, git, client)
	o := newExecRun(d, nil, 0)
	scA := candidateSolver(o, d, git)
	scB := candidateSolver(o, d, git)

	require.NoError(t, o.executeSubtaskWith(context.Background(), scA,
		subtaskRef{ID: "SUB-1", Title: "X", Sizing: seedSizing("simple")}))

	assert.Len(t, scA.prior, 1)
	assert.Empty(t, scB.prior, "each solver keeps its own ledger")
}

func TestPriorSubtasksRecordOnlyTheCompletingAttempt(t *testing.T) {
	git := &fakeGit{commitResults: []bool{false, true}, headSHAs: []string{"base000", "base000", "base000", "9999999aaaaaaa"}}
	client := &planLLM{responses: []llm.Response{
		finishResp("feat: nothing", 0.01),
		finishResp("feat: the work", 0.02),
	}}
	d := execTestDeps(&fakeOps{}, git, client)
	d.Registry = tierNamingRegistry()

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Sizing: seedSizing("moderate")}}, 0)
	require.NoError(t, runExecute(context.Background(), o))

	assert.Equal(t, []priorSubtask{
		{ID: "SUB-1", Title: "First", Kind: priorCommitted, SHA: "9999999", Subject: "feat: the work"},
	}, o.solver.prior, "the discarded first attempt leaves no entry; the retry's commit does")
}
