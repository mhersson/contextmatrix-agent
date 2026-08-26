package orchestrator

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// res.Turns is returned and thrown away today. It is the only turn measurement
// anything in this system produces, and the CAPPED observation is the one that
// matters most - it is the row that says the budget was the binding constraint.
//
// The corrected case is here because a field whose two candidate sources agree
// in every test is a field with no coverage: the subtask runs at a bar and a
// budget the planner did not choose, so bar and planner_bar must disagree on
// the row, and max_turns must be the WIDENED window rather than the base.
func TestSizingObservationRecordsTurnsAndOutcome(t *testing.T) {
	cases := map[string]struct {
		responses    []llm.Response
		errAfter     int
		baseMaxTurns int
		sizing       sizing
		plannerBar   string
		wantBar      string
		wantBudget   int
		wantMaxTurns int
		wantWrapUp   int
		wantTurns    int
		wantOutcome  string
	}{
		"clean finish": {
			responses: []llm.Response{finishResp("feat: done", 0.01)}, baseMaxTurns: 10,
			sizing: seedSizing("moderate"), plannerBar: "moderate",
			wantBar: "moderate", wantBudget: 0, wantMaxTurns: 10, wantWrapUp: wrapUpTurns,
			wantTurns: 1, wantOutcome: "done",
		},
		"turn cap": {
			responses: burnResps(5), baseMaxTurns: 5,
			sizing: seedSizing("moderate"), plannerBar: "moderate",
			wantBar: "moderate", wantBudget: 0, wantMaxTurns: 5, wantWrapUp: wrapUpTurns,
			wantTurns: 5, wantOutcome: "max_turns",
		},
		// A transport failure on the first call still burned the turn it failed
		// on, and the row says so: turns is what the harness spent, not what it
		// completed.
		"run error": {
			errAfter: 1, baseMaxTurns: 10,
			sizing: seedSizing("moderate"), plannerBar: "moderate",
			wantBar: "moderate", wantBudget: 0, wantMaxTurns: 10, wantWrapUp: wrapUpTurns,
			wantTurns: 1, wantOutcome: "error",
		},
		"corrected away from the planner's estimate": {
			responses: []llm.Response{finishResp("feat: done", 0.01)}, baseMaxTurns: 10,
			sizing: sizing{registry.TierComplex, 1}, plannerBar: "moderate",
			wantBar: "complex", wantBudget: 1, wantMaxTurns: 15, wantWrapUp: 8,
			wantTurns: 1, wantOutcome: "done",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var transcript bytes.Buffer

			ops := &fakeOps{}
			git := &fakeGit{committed: true}
			client := &planLLM{responses: tc.responses, errAfter: tc.errAfter}

			d := execTestDeps(ops, git, client)
			d.Emit = events.NewEmitter(nil, &transcript)
			d.Cfg.MaxTurns = tc.baseMaxTurns

			o := newExecRun(d, []subtaskRef{
				{ID: "SUB-1", Title: "Only", Sizing: tc.sizing, PlannerBar: tc.plannerBar},
			}, 0)
			o.curPhase = "execute"

			_ = runExecute(context.Background(), o)

			rec := onlyEvent(t, &transcript, sizingObservationKind)
			assert.Equal(t, "SUB-1", rec["subtask"])
			assert.Equal(t, "solo", rec["solver"])
			assert.Equal(t, "execute", rec["phase"])
			assert.EqualValues(t, 0, rec["reselect"])
			assert.Equal(t, tc.wantBar, rec["bar"])
			assert.EqualValues(t, tc.wantBudget, rec["budget_step"])
			assert.Equal(t, tc.plannerBar, rec["planner_bar"],
				"the measurement is only useful paired against the estimate it is testing")
			assert.EqualValues(t, tc.wantMaxTurns, rec["max_turns"])
			assert.EqualValues(t, tc.wantWrapUp, rec["wrap_up_turns"])
			assert.EqualValues(t, tc.wantTurns, rec["turns"])
			assert.Equal(t, tc.wantOutcome, rec["outcome"])
			assert.InDelta(t, float64(tc.wantTurns)/float64(tc.wantMaxTurns), rec["turn_ratio"], 0.001)
		})
	}
}

// An incapable attempt burned about one turn and wrote nothing. Pooled
// undiscriminated with real completions it makes the turns-to-cap ratio
// unusable, which is the whole reason the measurement is being taken.
func TestIncapableAttemptsAreDistinguishableRows(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &modelAwareLLM{incapable: map[string]bool{"alpha/coder": true}}

	d := execTestDeps(ops, git, llmFake)
	d.Registry = twoCoderRegistry()
	d.Emit = events.NewEmitter(nil, &transcript)

	o := newExecRun(d, []subtaskRef{
		{ID: "SUB-1", Title: "First", Sizing: seedSizing("moderate"), PlannerBar: "moderate"},
	}, 0)
	o.curPhase = "execute"

	require.NoError(t, runExecute(context.Background(), o))

	recs := eventsOfKind(t, &transcript, sizingObservationKind)
	require.Len(t, recs, 2, "one row per ATTEMPT, not one per subtask")

	assert.Equal(t, "incapable", recs[0]["outcome"])
	assert.EqualValues(t, 0, recs[0]["reselect"])
	assert.Equal(t, "alpha/coder", recs[0]["model"])

	assert.Equal(t, "done", recs[1]["outcome"])
	assert.EqualValues(t, 1, recs[1]["reselect"])
	assert.Equal(t, "beta/coder", recs[1]["model"])
}

// A fix round is a coder run too, and it is the one whose budget is seeded from
// its OWN bar rather than the card's - so its row has to say which bar and which
// budget step it actually ran at, or the corpus cannot tell a widened fix round
// from a fresh one.
//
// Its two scopes are covered separately because they disagree about what the
// row's unit is: a review round repairs the whole card, while the pre-commit
// verify fix repairs ONE subtask and is sized on that subtask's bar.
func TestFixRoundsEmitTheirOwnObservation(t *testing.T) {
	t.Run("a card-scoped round carries the card's estimate", func(t *testing.T) {
		var transcript bytes.Buffer

		ops := &fakeOps{}
		git := &fakeGit{committed: true}
		client := &planLLM{responses: []llm.Response{finishResp("fix: done", 0.01)}}

		d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
		d.Emit = events.NewEmitter(nil, &transcript)

		o := newReviewRun(d, cmclient.TaskContext{Title: "Card"}, 0)
		o.cardSizing = sizing{registry.TierModerate, 0}
		o.cardPlannerBar = "moderate"
		o.curPhase = "review"

		_, err := o.runFix(context.Background(), fixRequest{Findings: "- a.go: x", Round: 1, FixTier: "complex"})
		require.NoError(t, err)

		rec := onlyEvent(t, &transcript, sizingObservationKind)
		assert.Equal(t, "fix", rec["solver"])
		assert.Equal(t, "review", rec["phase"])
		assert.Empty(t, rec["subtask"], "a review round is not scoped to one subtask")
		assert.Equal(t, "complex", rec["bar"], "the synthesizer's fix_tier, not the card bar")
		assert.EqualValues(t, 1, rec["budget_step"], "and the budget seeded from THAT bar")
		assert.Equal(t, "moderate", rec["planner_bar"], "a card-scoped round carries the card's estimate")
		assert.Equal(t, "done", rec["outcome"])
	})

	// The pre-commit verify fix is sized on the SUBTASK's bar. Left unnamed, its
	// row pairs that bar against the CARD's estimate and reads as a correction
	// that never happened - a wrong number, on the two columns the measurement
	// exists to relate.
	t.Run("a subtask-scoped round names its subtask and carries ITS estimate", func(t *testing.T) {
		var transcript bytes.Buffer

		ops := &fakeOps{}
		client := &planLLM{responses: []llm.Response{finishResp("fix: done", 0.01)}}

		d := execTestDeps(ops, &fakeGit{committed: true}, client)
		d.Cfg.Workspace = t.TempDir()
		d.Emit = events.NewEmitter(nil, &transcript)

		o := newExecRun(d, nil, 0)
		o.cardSizing = seedSizing("moderate")
		o.cardPlannerBar = "moderate"
		o.curPhase = "execute"

		seedResolvedVerifyPlan(o)

		// Red before the fix, green after it, so the gate runs exactly one fix
		// pass and returns cleanly.
		calls := 0
		o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
			calls++
			if calls == 1 {
				return verifyexec.Outcome{ExitCode: 1, Output: "--- FAIL: TestThing"}
			}

			return verifyexec.Outcome{ExitCode: 0}
		}

		// Deliberately a bar the CARD does not have: under a row that reports the
		// card's estimate, bar and planner_bar disagree for no reason.
		sub := subtaskRef{ID: "SUB-1", Title: "Only", Sizing: seedSizing("complex"), PlannerBar: "complex"}
		require.NoError(t, o.preCommitVerify(context.Background(), o.solver, sub))

		rec := onlyEvent(t, &transcript, sizingObservationKind)
		assert.Equal(t, "fix", rec["solver"])
		assert.Equal(t, "execute", rec["phase"])
		assert.Equal(t, "SUB-1", rec["subtask"], "this round repaired one subtask, and the row says which")
		assert.Equal(t, "complex", rec["bar"], "sized on the subtask's bar")
		assert.Equal(t, "complex", rec["planner_bar"],
			"so the estimate on the row must be that subtask's, not the card's")
		assert.EqualValues(t, 1, rec["budget_step"])
		assert.Equal(t, "done", rec["outcome"])
	})
}

// A Best-of-N candidate is a coder attempt too, and N of them race on the SAME
// subtask. Labelled like the solo run, a corpus reading turns per subtask would
// count one race as N independent sizings of that unit.
func TestCandidateAttemptsAreLabelledAsSuch(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{finishResp("feat: done", 0.01)}}

	d := execTestDeps(ops, &fakeGit{committed: true}, client)
	d.Emit = events.NewEmitter(nil, &transcript)

	o := newExecRun(d, nil, 0)
	o.curPhase = "execute"

	sc := &solverCtx{
		git: &fakeGit{committed: true}, ledger: NewLedger(0, 0), tools: d.WriteTools,
		workspace: "ws", coderModel: o.solverCoderModel,
		boardOps: false, push: false, tag: "candidate 1/2",
	}

	sub := subtaskRef{ID: "SUB-1", Title: "Only", Sizing: seedSizing("moderate"), PlannerBar: "moderate"}

	_, _, err := o.runCoderWith(context.Background(), sc, sub, "do it")
	require.NoError(t, err)

	rec := onlyEvent(t, &transcript, sizingObservationKind)
	assert.Equal(t, "candidate", rec["solver"])
	assert.Equal(t, "SUB-1", rec["subtask"], "a candidate row still names the unit it raced on")
}

// turn_ratio is derived from the window the attempt actually ran at, never
// taken from the record: a caller that filled the field in cannot put its own
// number on the transcript - including on the unset-cap path, where the
// derivation has no denominator and the guard would otherwise pass the value
// straight through.
func TestTurnRatioIsAlwaysDerived(t *testing.T) {
	var transcript bytes.Buffer

	d := execTestDeps(&fakeOps{}, &fakeGit{}, &planLLM{})
	d.Emit = events.NewEmitter(nil, &transcript)

	o := newExecRun(d, nil, 0)
	o.emitSizingObs(sizingObs{Subtask: "SUB-1", MaxTurns: 0, Turns: 7, TurnRatio: 0.99})

	rec := onlyEvent(t, &transcript, sizingObservationKind)
	// InDelta, not EqualValues: EqualValues would convert a float64 actual to
	// the int expected and let 0.99 through as 0.
	assert.InDelta(t, 0.0, rec["turn_ratio"], 0.001, "no cap, no denominator - and no borrowed value either")
	assert.EqualValues(t, 7, rec["turns"], "the turns are still recorded; max_turns is what tells a consumer why the ratio is absent")
}
