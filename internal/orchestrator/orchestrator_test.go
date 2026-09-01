package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

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

// indexOfCallPrefix returns the position of the first call starting with
// prefix, or -1. AddLog records the full message, so ordering assertions
// against it need a prefix match rather than indexOfCall's equality.
func indexOfCallPrefix(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
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

func TestRunVerifyParkedParks(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{},
	}
	d := Deps{Ops: ops, Git: &fakeGit{}, Cfg: Config{CardID: "CARD-1"}}

	o := newRun(d, ops.taskContext)
	// Execute stops because the pre-commit verify stayed red after its one fix pass.
	vpe := &VerifyParkedError{
		Subtask: "SUB-1",
		Command: "make test",
		Output:  "--- FAIL: TestThing\n    thing_test.go:12: want 1, got 2",
	}
	o.planFn = func(context.Context) error { return vpe }

	err := o.execute(context.Background())

	var got *VerifyParkedError
	require.ErrorAs(t, err, &got)
	assert.Equal(t, "SUB-1", got.Subtask)

	calls := ops.recorded()
	// AddLog must be recorded on the verify park.
	assert.GreaterOrEqual(t, indexOfCall(calls, "AddLog:"+verifyParkedLogMessage(vpe)), 0,
		"verify park must AddLog the reason; calls=%v", calls)
	// The evidence a human needs is on the card, not only in the container that
	// is about to be destroyed: the command that failed and what it printed.
	assert.True(t, ops.loggedContains("make test"), "the card names the command that stayed red; logs=%v", ops.logs)
	assert.True(t, ops.loggedContains("--- FAIL: TestThing"),
		"the card carries the failing output tail; logs=%v", ops.logs)
	// No further phase entered after the park.
	assert.Equal(t, -1, indexOfCall(calls, "SetPhase:execute"))
}

// bigVerifyOutput builds a numbered multi-line verify output far larger than
// any card-log entry can carry, so a truncation test can tell the head of the
// output from its tail.
func bigVerifyOutput(lines int) string {
	var b strings.Builder

	for i := range lines {
		fmt.Fprintf(&b, "verify output line %04d\n", i)
	}

	return b.String()
}

// TestVerifyParkedLogMessageFitsTheBoardsLogCap pins the bound that keeps this
// note on the card at all. ContextMatrix REJECTS an activity-log message over
// 2000 bytes rather than truncating it, and logCard swallows the rejection into
// a warning - so an over-long note does not arrive clipped, it does not arrive.
// A park whose whole purpose is visibility would then be invisible, which is
// the failure this bound exists to prevent.
//
// The truncation is tail-biased: a build tool's diagnostics are concentrated in
// its LAST bytes, so a note that kept the head would keep the least useful part
// and drop the reason the card is blocked.
func TestVerifyParkedLogMessageFitsTheBoardsLogCap(t *testing.T) {
	// The board's own limit, restated rather than read from the constant under
	// test: raising that constant past what ContextMatrix accepts must fail here.
	const boardLogCap = 2000

	t.Run("output far past the cap is trimmed from the front", func(t *testing.T) {
		out := bigVerifyOutput(500) // ~12 KB
		require.Greater(t, len(out), 4000, "the fixture must exceed the excerpt cap, or this pins nothing")

		got := verifyParkedLogMessage(&VerifyParkedError{
			Subtask: "SUB-1",
			Command: "make test",
			Output:  out,
		})

		assert.LessOrEqual(t, len(got), 1900,
			"the note must fit under the board's %d-byte log cap with margin; got %d bytes", boardLogCap, len(got))

		assert.Contains(t, got, "SUB-1", "the header survives truncation")
		assert.Contains(t, got, "make test", "the failing command survives truncation")

		assert.Contains(t, got, "verify output line 0499", "the note keeps the output's tail")
		assert.NotContains(t, got, "verify output line 0000", "the note drops the output's head")

		assert.True(t, utf8.ValidString(got), "truncation must not split a rune")
	})

	t.Run("output that already fits is carried whole", func(t *testing.T) {
		out := "--- FAIL: TestThing\n    thing_test.go:12: want 1, got 2"

		got := verifyParkedLogMessage(&VerifyParkedError{Subtask: "SUB-1", Command: "make test", Output: out})

		assert.LessOrEqual(t, len(got), 1900)
		assert.Contains(t, got, out, "a note that fits is never elided")
	})

	t.Run("no output leaves the header alone", func(t *testing.T) {
		got := verifyParkedLogMessage(&VerifyParkedError{Subtask: "SUB-1", Command: "make test"})

		assert.LessOrEqual(t, len(got), 1900)
		assert.Contains(t, got, "SUB-1")
		assert.Contains(t, got, "make test")
	})

	t.Run("a pathologically long command still fits", func(t *testing.T) {
		got := verifyParkedLogMessage(&VerifyParkedError{
			Subtask: "SUB-1",
			Command: strings.Repeat("x", 4000),
			Output:  bigVerifyOutput(500),
		})

		assert.LessOrEqual(t, len(got), 1900,
			"the bound holds even when the header alone would overrun it; got %d bytes", len(got))
		assert.True(t, utf8.ValidString(got))
	})

	t.Run("multi-byte output is cut on a rune boundary", func(t *testing.T) {
		// Every line ends in a 3-byte rune, so a byte-aligned cut lands mid-rune.
		var b strings.Builder
		for i := range 500 {
			fmt.Fprintf(&b, "line %04d ✗\n", i)
		}

		got := verifyParkedLogMessage(&VerifyParkedError{Subtask: "SUB-1", Command: "make test", Output: b.String()})

		assert.LessOrEqual(t, len(got), 1900)
		assert.True(t, utf8.ValidString(got), "a multi-byte rune at the cut must be dropped whole")
	})
}

// TestSplitOverflowLogMessageFitsTheBoardsLogCap pins the same bound
// verifyParkedLogMessage carries: the note must fit under ContextMatrix's
// activity-log cap with margin, and a truncated title list must never split a
// multi-byte rune - splitOverflowLogMessage computes its room against
// verifyParkNoteMax via truncateBytes rather than a fixed byte count.
func TestSplitOverflowLogMessageFitsTheBoardsLogCap(t *testing.T) {
	t.Run("titles far past the cap are trimmed", func(t *testing.T) {
		titles := make([]string, 0, maxFollowupCards+1)
		for i := range 500 {
			titles = append(titles, fmt.Sprintf("A very long proposed follow-up card title number %04d that eats a lot of room", i))
		}

		got := splitOverflowLogMessage(&SplitOverflowError{Count: len(titles), Titles: titles})

		assert.LessOrEqual(t, len(got), verifyParkNoteMax,
			"the note must fit under the board's log cap with margin; got %d bytes", len(got))
		assert.Contains(t, got, "500", "the header survives truncation")
		assert.Contains(t, got, fmt.Sprintf("max %d", maxFollowupCards))
		assert.True(t, utf8.ValidString(got), "truncation must not split a rune")
	})

	t.Run("titles that already fit are carried whole", func(t *testing.T) {
		soe := &SplitOverflowError{Count: 2, Titles: []string{"Extract config loader", "Add config docs"}}

		got := splitOverflowLogMessage(soe)

		assert.LessOrEqual(t, len(got), verifyParkNoteMax)
		assert.Contains(t, got, "Extract config loader; Add config docs", "a note that fits is never elided")
	})

	t.Run("multi-byte titles are cut on a rune boundary", func(t *testing.T) {
		// Every title ends in a 3-byte rune, so a byte-aligned cut lands mid-rune.
		titles := make([]string, 0, 500)
		for i := range 500 {
			titles = append(titles, fmt.Sprintf("Follow-up title %04d ✗", i))
		}

		got := splitOverflowLogMessage(&SplitOverflowError{Count: len(titles), Titles: titles})

		assert.LessOrEqual(t, len(got), verifyParkNoteMax)
		assert.True(t, utf8.ValidString(got), "a multi-byte rune at the cut must be dropped whole")
	})
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
	assert.Equal(t, []string{"plan", "execute", "judge", "document", "review", "integrate", "pr_gates", "done"}, phaseOrder)
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
	o.prGatesFn = mk("pr_gates")
	o.doneFn = mk("done")

	require.NoError(t, o.execute(context.Background()))
	assert.Equal(t, []string{"plan", "execute", "document", "review", "integrate", "pr_gates", "done"}, order,
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
	assert.Equal(t, recentReviewFindingsHistory(grown), o.lastFindings)
	assert.Contains(t, o.lastFindings, "- a.go: bug - fix it")
}

// TestNewRunSeedsBoundedFindingsOnResume proves the resume seed at newRun is
// windowed like the authoritative reads. o.lastFindings is the cross-round-
// memory field that feeds the first non-authoritative panel/synthesis call
// after a resume (before recordRoundFindings overwrites it), so an unbounded
// seed here reproduces the same unbounded-prompt failure mode through that
// path, defeating the point of windowing the authoritative reads.
func TestNewRunSeedsBoundedFindingsOnResume(t *testing.T) {
	grown := "Original task.\n\n" +
		"## Review Findings\n\nround one text\n\n" +
		"## Review Findings (Round 2)\n\nround two text\n\n" +
		"## Review Findings (Round 3)\n\nround three text\n\n" +
		"## Review Findings (Round 4)\n\nround four text\n"
	d := Deps{Ops: &fakeOps{}, Cfg: Config{CardID: "CARD-1"}}

	o := newRun(d, cmclient.TaskContext{Title: "Parent", Description: grown})

	assert.Contains(t, o.lastFindings, "round four text")
	assert.Contains(t, o.lastFindings, "round three text")
	assert.Contains(t, o.lastFindings, "round two text")
	assert.NotContains(t, o.lastFindings, "round one text")
}

func TestNewRunFreshDescriptionSeedsEmptyFindings(t *testing.T) {
	d := Deps{Ops: &fakeOps{}, Cfg: Config{CardID: "CARD-1"}}

	o := newRun(d, cmclient.TaskContext{Title: "Parent", Description: "Just the task."})

	assert.Equal(t, "Just the task.", o.body)
	assert.Equal(t, "Just the task.", o.taskDescription)
	assert.Empty(t, o.lastFindings)
}
