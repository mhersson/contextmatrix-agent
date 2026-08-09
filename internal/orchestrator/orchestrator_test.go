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
		taskContext: cmclient.TaskContext{},
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
		taskContext: cmclient.TaskContext{Phase: "shipping"},
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
		taskContext: cmclient.TaskContext{},
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
		taskContext: cmclient.TaskContext{Phase: "review"},
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
		taskContext: cmclient.TaskContext{},
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
		taskContext: cmclient.TaskContext{},
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

func TestRunToolchainMissingParks(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{},
	}
	d := Deps{Ops: ops, Git: &fakeGit{}, Cfg: Config{CardID: "CARD-1"}}

	o := newRun(d, ops.taskContext)
	// Execute stops because verify resolution found a toolchain that cannot run.
	o.planFn = func(context.Context) error {
		return &ToolchainMissingError{Tier: "detected", Subject: "pom.xml", Reason: "java: not found"}
	}

	err := o.execute(context.Background())

	var tme *ToolchainMissingError
	require.ErrorAs(t, err, &tme)
	assert.Equal(t, "detected", tme.Tier)
	assert.Equal(t, "pom.xml", tme.Subject)

	calls := ops.recorded()
	// AddLog must be recorded on the toolchain-missing park.
	assert.GreaterOrEqual(t, indexOfCall(calls, "AddLog:"+toolchainLogMessage(tme)), 0,
		"toolchain-missing park must AddLog the reason; calls=%v", calls)
	// No further phase entered after the park.
	assert.Equal(t, -1, indexOfCall(calls, "SetPhase:execute"))
}

// TestMaxTurnsLogMessagePhaseAware proves the turn-cap park message names a
// remedy that actually works for the phase it fires in: the plan phase's
// budget is capped at the fixed planMaxTurns constant regardless of
// CMX_MAX_TURNS, and the plan phase is what creates subtasks, so neither
// "raise CMX_MAX_TURNS" nor "split the subtask" holds any advice there. Every
// other phase keeps the original wording verbatim.
func TestMaxTurnsLogMessagePhaseAware(t *testing.T) {
	mte := &MaxTurnsError{Model: "anthropic/claude-x", Turns: 25}

	planMsg := maxTurnsLogMessage("plan", mte)
	assert.Equal(t,
		`turn cap reached on model "anthropic/claude-x" after 25 turns - parking work; narrow the card's scope`,
		planMsg)

	for _, phase := range []string{"execute", "judge", "document", "review", "integrate"} {
		msg := maxTurnsLogMessage(phase, mte)
		assert.Equal(t,
			`turn cap reached on model "anthropic/claude-x" after 25 turns - parking work; raise CMX_MAX_TURNS or split the subtask`,
			msg, "phase %q must keep the original wording verbatim", phase)
	}
}

func TestPhaseOrderPlacesDocumentBetweenExecuteAndReview(t *testing.T) {
	assert.Equal(t, []string{"plan", "execute", "judge", "document", "review", "integrate", "done"}, phaseOrder)
}

func TestRunWalksDocumentBetweenExecuteAndReview(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{}}
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
		taskContext: cmclient.TaskContext{ReportedCostUSD: 0.90},
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

func TestNewRunSeedsFilteredDescriptionAndFindings(t *testing.T) {
	grown := "Original task.\n\n## Diagnosis\n\nroot cause\n\n## Plan\n\n1. SUBTASK: fix\n\n## Review Findings\n\n- a.go: bug - fix it\n\n### Recommendation\n\nrevise\n"
	d := Deps{Ops: &fakeOps{}, Cfg: Config{CardID: "CARD-1"}}

	o := newRun(d, cmclient.TaskContext{Title: "Parent", Description: grown})

	// The write-back body stays raw - recordSection must never lose history.
	assert.Equal(t, grown, o.body)
	// Prompt sites read the stripped view.
	assert.Equal(t, "Original task.", o.taskDescription)
	// Prior findings re-enter through the cross-round memory channel.
	assert.Equal(t, reviewFindingsHistory(grown), o.lastFindings)
	assert.Contains(t, o.lastFindings, "- a.go: bug - fix it")
}

func TestNewRunFreshDescriptionSeedsEmptyFindings(t *testing.T) {
	d := Deps{Ops: &fakeOps{}, Cfg: Config{CardID: "CARD-1"}}

	o := newRun(d, cmclient.TaskContext{Title: "Parent", Description: "Just the task."})

	assert.Equal(t, "Just the task.", o.body)
	assert.Equal(t, "Just the task.", o.taskDescription)
	assert.Empty(t, o.lastFindings)
}
