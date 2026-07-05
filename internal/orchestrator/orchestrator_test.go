package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// indexOfCall returns the position of the first call matching name, or -1.
func indexOfCall(calls []string, name string) int {
	for i, c := range calls {
		if c == name {
			return i
		}
	}

	return -1
}

// countCalls returns how many recorded calls equal name.
func countCalls(calls []string, name string) int {
	n := 0

	for _, c := range calls {
		if c == name {
			n++
		}
	}

	return n
}

func TestRunPersistsPhaseBeforeWork(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{CardID: "CARD-1"},
	}
	d := Deps{Ops: ops, Git: &fakeGit{}, Cfg: Config{CardID: "CARD-1"}}

	// Stub the plan phase to fail; SetPhase("plan") must still come FIRST.
	o := newRun(d, ops.taskContext)
	planErr := errors.New("plan boom")
	o.planFn = func(context.Context) error { return planErr }

	err := o.execute(context.Background())
	require.ErrorIs(t, err, planErr)

	calls := ops.recorded()
	setIdx := indexOfCall(calls, "SetPhase:plan")
	require.GreaterOrEqual(t, setIdx, 0, "SetPhase:plan must be recorded")
	// No later phase persisted after a failing plan.
	assert.Equal(t, -1, indexOfCall(calls, "SetPhase:execute"))
}

func TestRunUnknownPhaseReturnsError(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{CardID: "CARD-1", Phase: "shipping"},
	}
	d := Deps{Ops: ops, Cfg: Config{CardID: "CARD-1"}}

	o := newRun(d, ops.taskContext)

	err := o.execute(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown persisted phase")

	// The guard must reject before persisting anything.
	for _, call := range ops.recorded() {
		assert.NotContains(t, call, "SetPhase:", "no SetPhase may be recorded for an unknown phase")
	}
}

func TestRunSetPhaseFailureSkipsWork(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{CardID: "CARD-1"},
		setPhaseErr: errors.New("cm unreachable"),
	}
	d := Deps{Ops: ops, Git: &fakeGit{}, Cfg: Config{CardID: "CARD-1"}}

	o := newRun(d, ops.taskContext)

	var planRan bool

	o.planFn = func(context.Context) error {
		planRan = true

		return nil
	}

	err := o.execute(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "persist phase plan:")
	require.ErrorIs(t, err, ops.setPhaseErr)

	assert.False(t, planRan, "plan must not run when its phase failed to persist")
}

func TestRunEntersAtPersistedPhase(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{CardID: "CARD-1", Phase: "review"},
	}
	d := Deps{Ops: ops, Git: &fakeGit{}, Cfg: Config{CardID: "CARD-1"}}

	o := newRun(d, ops.taskContext)

	var planRan, executeRan, reviewRan bool

	o.planFn = func(context.Context) error {
		planRan = true

		return nil
	}
	o.executeFn = func(context.Context) error {
		executeRan = true

		return nil
	}
	o.reviewFn = func(context.Context) error {
		reviewRan = true

		return nil
	}
	o.integrateFn = func(context.Context) error { return nil }
	o.doneFn = func(context.Context) error { return nil }

	require.NoError(t, o.execute(context.Background()))

	assert.False(t, planRan, "plan must not run when entering at review")
	assert.False(t, executeRan, "execute must not run when entering at review")
	assert.True(t, reviewRan, "review must run")

	// Phase persistence starts at review, not plan/execute.
	calls := ops.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "SetPhase:plan"))
	assert.Equal(t, -1, indexOfCall(calls, "SetPhase:execute"))
	assert.GreaterOrEqual(t, indexOfCall(calls, "SetPhase:review"), 0)
}

func TestRunBudgetBreachParks(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{CardID: "CARD-1"},
	}
	d := Deps{Ops: ops, Git: &fakeGit{}, Cfg: Config{CardID: "CARD-1", MaxCardCost: 1.00}}

	o := newRun(d, ops.taskContext)
	// Plan spends past the cap, then trips the ledger.
	o.planFn = func(context.Context) error {
		o.ledger.Spend(1.50)

		return o.ledger.Check()
	}

	err := o.execute(context.Background())

	var be *BudgetExceededError
	require.ErrorAs(t, err, &be)
	assert.InDelta(t, 1.50, be.Spent, 1e-9)
	assert.InDelta(t, 1.00, be.Max, 1e-9)

	calls := ops.recorded()
	// AddLog must be recorded on breach.
	assert.GreaterOrEqual(t, indexOfCall(calls, "AddLog:"+budgetLogMessage(be)), 0,
		"breach must AddLog the numbers; calls=%v", calls)
	// No further phase entered after the breach.
	assert.Equal(t, -1, indexOfCall(calls, "SetPhase:execute"))
}

func TestRunContextLimitParks(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{CardID: "CARD-1"},
	}
	d := Deps{Ops: ops, Git: &fakeGit{}, Cfg: Config{CardID: "CARD-1"}}

	o := newRun(d, ops.taskContext)
	// Plan stops because the model neared its context window.
	o.planFn = func(context.Context) error {
		return &ContextLimitError{Model: "anthropic/claude-x", ContextWindow: 200000}
	}

	err := o.execute(context.Background())

	var cle *ContextLimitError
	require.ErrorAs(t, err, &cle)
	assert.Equal(t, "anthropic/claude-x", cle.Model)
	assert.Equal(t, 200000, cle.ContextWindow)

	calls := ops.recorded()
	// AddLog must be recorded on the context-window park.
	assert.GreaterOrEqual(t, indexOfCall(calls, "AddLog:"+contextLimitLogMessage(cle)), 0,
		"context-window park must AddLog the numbers; calls=%v", calls)
	// No further phase entered after the park.
	assert.Equal(t, -1, indexOfCall(calls, "SetPhase:execute"))
}

func TestPhaseOrderPlacesDocumentBetweenExecuteAndReview(t *testing.T) {
	assert.Equal(t, []string{"plan", "execute", "judge", "document", "review", "integrate", "done"}, phaseOrder)
}

func TestRunWalksDocumentBetweenExecuteAndReview(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{CardID: "CARD-1"}}
	d := Deps{Ops: ops, Git: &fakeGit{}, Cfg: Config{CardID: "CARD-1"}}
	o := newRun(d, ops.taskContext)

	var order []string

	mk := func(name string) phaseFn {
		return func(context.Context) error {
			order = append(order, name)

			return nil
		}
	}

	o.planFn = mk("plan")
	o.executeFn = mk("execute")
	o.documentFn = mk("document")
	o.reviewFn = mk("review")
	o.integrateFn = mk("integrate")
	o.doneFn = mk("done")

	require.NoError(t, o.execute(context.Background()))
	assert.Equal(t, []string{"plan", "execute", "document", "review", "integrate", "done"}, order,
		"document runs immediately after execute and before review")
}

func TestRunSeedsLedgerFromReportedCost(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{CardID: "CARD-1", ReportedCostUSD: 0.90},
	}
	d := Deps{Ops: ops, Git: &fakeGit{}, Cfg: Config{CardID: "CARD-1", MaxCardCost: 1.00}}

	o := newRun(d, ops.taskContext)
	// A tiny additional spend tips the already-reported total past the cap.
	o.planFn = func(context.Context) error {
		o.ledger.Spend(0.20)

		return o.ledger.Check()
	}

	err := o.execute(context.Background())

	var be *BudgetExceededError
	require.ErrorAs(t, err, &be)
	assert.InDelta(t, 1.10, be.Spent, 1e-9)
}
