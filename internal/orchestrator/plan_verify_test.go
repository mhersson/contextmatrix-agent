package orchestrator

import (
	"context"
	"runtime"
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunPlanParksOnMissingToolchainBeforePlanning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t) // nothing on PATH: the declared command cannot run

	ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
	client := &planLLM{responses: []llm.Response{
		stopResp(`{"command":""}`, 0.01), stopResp(goodPlanJSON, 0.01),
	}}
	o := newProposeRun(t, ops, client, t.TempDir())
	o.d.Cfg.Verify = &DeclaredVerify{Command: "pytest -q"}
	o.tc.Type = "bug" // bug-like: proves resolution precedes the diagnose pass

	err := runPlan(context.Background(), o)

	var tme *ToolchainMissingError
	require.ErrorAs(t, err, &tme)
	assert.Len(t, client.models, 1, "only the verify proposal ran; the planner never did")
	assert.Empty(t, ops.createCardArgs, "no subtask is created before the park")
	assert.Contains(t, o.body, "## Verify Command")
	assert.Nil(t, o.verify, "a toolchain park is not cached")
}

func TestRunPlanResolvesProposalBeforePlanning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "cargo")

	ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
	client := &planLLM{responses: []llm.Response{
		stopResp(`{"command":"cargo test"}`, 0.01), stopResp(goodPlanJSON, 0.01),
	}}
	o := newProposeRun(t, ops, client, t.TempDir())

	require.NoError(t, runPlan(context.Background(), o))

	require.NotNil(t, o.verify)
	assert.Equal(t, verifySourceProposed, o.verify.Source)
	assert.Equal(t, "cargo test", o.verify.Display)
	assert.Len(t, ops.createCardArgs, 2, "the plan ran on the second model call")
	require.Len(t, client.models, 2)

	// Execute entry reuses the resolved command.
	_, err := o.ensureVerify(context.Background())
	require.NoError(t, err)
	assert.Len(t, client.models, 2, "execute entry makes no second proposal call")
}

func TestRunPlanGreenFieldSkipReResolvesWithoutSecondProposal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t, "go")

	dir := t.TempDir()
	ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
	client := &planLLM{responses: []llm.Response{
		stopResp(`{"command":""}`, 0.01), stopResp(goodPlanJSON, 0.01),
	}}
	o := newProposeRun(t, ops, client, dir)

	require.NoError(t, runPlan(context.Background(), o))
	require.NotNil(t, o.verify)
	assert.Equal(t, verifySourceNone, o.verify.Source, "nothing declared or detected: cached skip")
	require.Len(t, client.models, 2)

	p0, err := o.ensureVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceNone, p0.Source)
	require.Len(t, client.models, 2, "re-resolving a skip makes no second proposal call")

	writeFile(t, dir, "go.mod", "module x\n\ngo 1.22\n")

	p, err := o.ensureVerify(context.Background())
	require.NoError(t, err)
	assert.Equal(t, verifySourceDetected, p.Source, "a marker added by the plan phase's successors is picked up")
	assert.Len(t, client.models, 2, "the proposal fires at most once per run")
}

func TestRunPlanBudgetParkInVerifyProposalSkipsPlanner(t *testing.T) {
	ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
	client := &planLLM{responses: []llm.Response{stopResp(goodPlanJSON, 0.01)}}
	o := newProposeRun(t, ops, client, t.TempDir())
	o.ledger = NewLedger(0.01, 0.02)

	err := runPlan(context.Background(), o)

	var be *BudgetExceededError
	require.ErrorAs(t, err, &be)
	assert.Empty(t, client.models, "no model call ran")
	assert.Empty(t, ops.createCardArgs)
	assert.Nil(t, o.verify, "a budget park is not cached")
	assert.True(t, o.proposeAttempted, "the budget park came from the verify proposal")
}

// A run resumed past plan never enters runPlan; execute entry resolves verify.
func TestRunExecuteResolvesVerifyWhenResumedPastPlan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-only")
	}

	stubTools(t) // nothing on PATH: the declared command cannot run

	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{stopResp(`{"command":""}`, 0.01)}}
	o := newProposeRun(t, ops, client, t.TempDir())
	o.d.Cfg.Verify = &DeclaredVerify{Command: "pytest -q"}

	err := runExecute(context.Background(), o)

	var tme *ToolchainMissingError
	require.ErrorAs(t, err, &tme)
	assert.Len(t, client.models, 1, "execute entry made the proposal call")
}
