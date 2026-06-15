package eval

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// costLLM returns one no-tool-call response carrying a fixed cost, so each run
// "completes" immediately and adds Cost to the matrix total.
type costLLM struct{ cost float64 }

func (c costLLM) Send(_ context.Context, _ llm.Request) (llm.Response, error) {
	return llm.Response{FinishReason: "stop", Usage: llm.Usage{Cost: c.cost}}, nil
}

func (c costLLM) SendStream(_ context.Context, _ llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return llm.Response{FinishReason: "stop", Usage: llm.Usage{Cost: c.cost}}, nil
}

// fakeTask is a Provision/Check stub that never touches the filesystem or LLM
// output; pass is fixed.
type fakeTask struct {
	name string
	role registry.Role
	pass bool
}

func (t fakeTask) Name() string           { return t.name }
func (t fakeTask) Role() registry.Role    { return t.role }
func (t fakeTask) Provision(string) error { return nil }
func (t fakeTask) Prompt() string         { return "do it" }
func (t fakeTask) Check(context.Context, string, harness.Result) (harness.Verdict, error) {
	return harness.Verdict{OK: t.pass}, nil
}

// TestMinCellTrialsFloorCalibration proves the floor is calibrated against the
// per-cell trial count (samples × tasks-of-the-smallest-role), NOT the raw sample
// count. With samples=3 and a battery whose smallest role has K=2 tasks, the floor
// must equal CalibratedFloor(3*K) and be materially higher than CalibratedFloor(3) —
// the bug the calibration fix closes.
func TestMinCellTrialsFloorCalibration(t *testing.T) {
	const samples = 3

	// Smallest-battery role here is reviewer with K=2 tasks (coder has 3).
	tasks := []Task{
		fakeTask{name: "c1", role: registry.RoleCoder},
		fakeTask{name: "c2", role: registry.RoleCoder},
		fakeTask{name: "c3", role: registry.RoleCoder},
		fakeTask{name: "r1", role: registry.RoleReviewer},
		fakeTask{name: "r2", role: registry.RoleReviewer},
	}

	const k = 2

	n := MinCellTrials(tasks, samples)
	assert.Equal(t, samples*k, n, "n = samples × min-tasks-across-roles (the smallest battery)")

	floor := CalibratedFloor(n, 1.96)
	assert.InDelta(t, CalibratedFloor(samples*k, 1.96), floor, 1e-12)
	assert.Greater(t, floor, CalibratedFloor(samples, 1.96),
		"calibrating at the per-cell trial count yields a materially higher floor than at the sample count")
}

// TestMinCellTrialsEmpty: no tasks -> zero trials (degenerate; the caller normalizes
// samples but an empty task list still yields 0).
func TestMinCellTrialsEmpty(t *testing.T) {
	assert.Equal(t, 0, MinCellTrials(nil, 3))
}

// dropDetail returns the reason logged for cell in dropped, or "" if the cell was
// not dropped.
func dropDetail(dropped []DroppedCell, cell CellKey) string {
	for _, d := range dropped {
		if d.Cell == cell {
			return d.Reason
		}
	}

	return ""
}

func TestRunMatrixAggregates(t *testing.T) {
	tasks := []Task{
		fakeTask{name: "c1", role: registry.RoleCoder, pass: true},
		fakeTask{name: "r1", role: registry.RoleReviewer, pass: false},
	}
	mr, err := RunMatrix(context.Background(), costLLM{cost: 0.01}, MatrixOpts{
		Models: []string{"m1"}, Tasks: tasks, Samples: 2, MaxTurns: 3,
	})
	require.NoError(t, err)
	assert.Len(t, mr.Outcomes, 4) // 1 model × 2 tasks × 2 samples
	assert.InEpsilon(t, 0.04, mr.TotalCost, 1e-9)
	assert.False(t, mr.Aborted)
}

func TestRunMatrixBudgetAbort(t *testing.T) {
	tasks := []Task{fakeTask{name: "c1", role: registry.RoleCoder, pass: true}}
	mr, err := RunMatrix(context.Background(), costLLM{cost: 0.50}, MatrixOpts{
		Models: []string{"m1"}, Tasks: tasks, Samples: 5, MaxTurns: 3, MaxTotalCost: 0.60,
	})
	require.NoError(t, err)
	assert.True(t, mr.Aborted)
	assert.Len(t, mr.Outcomes, 2) // stops before the 3rd run (cost would be 1.50 >= 0.60)
}

// errLLM fails every call, simulating a transient provider/stream error.
type errLLM struct{}

func (errLLM) Send(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("provider stream error")
}

func (errLLM) SendStream(context.Context, llm.Request, func(llm.Delta)) (llm.Response, error) {
	return llm.Response{}, errors.New("provider stream error")
}

func TestRunMatrixSkipsErroredRuns(t *testing.T) {
	tasks := []Task{fakeTask{name: "c1", role: registry.RoleCoder, pass: true}}
	mr, err := RunMatrix(context.Background(), errLLM{}, MatrixOpts{
		Models: []string{"m1"}, Tasks: tasks, Samples: 3, MaxTurns: 3,
	})
	require.NoError(t, err)       // a per-run error must NOT abort the sweep
	assert.Equal(t, 3, mr.Errors) // all 3 runs errored and were skipped
	assert.Empty(t, mr.Outcomes)  // nothing scored
	assert.False(t, mr.Aborted)
}

// captureLLM records the last request's Provider so a test can assert that
// MatrixOpts.Provider is threaded all the way to the wire.
type captureLLM struct{ provider *json.RawMessage }

func (c captureLLM) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	*c.provider = req.Provider

	return llm.Response{FinishReason: "stop"}, nil
}

func (c captureLLM) SendStream(_ context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	*c.provider = req.Provider

	return llm.Response{FinishReason: "stop"}, nil
}

// verdictTask returns a fixed verdict from Check, letting a test drive the exact
// Detail string the runner inspects for the tamper marker.
type verdictTask struct {
	name string
	v    harness.Verdict
}

func (t verdictTask) Name() string           { return t.name }
func (t verdictTask) Role() registry.Role    { return registry.RoleCoder }
func (t verdictTask) Provision(string) error { return nil }
func (t verdictTask) Prompt() string         { return "do it" }
func (t verdictTask) Check(context.Context, string, harness.Result) (harness.Verdict, error) {
	return t.v, nil
}

// TestRunMatrixSurfacesTamper proves the tamper marker survives to the Outcome: a
// Check verdict carrying tamperedDetailPrefix sets Outcome.Tampered (Pass stays
// false), while an ordinary failing verdict does not. Without this, the marker would
// die in runOne and the merge step could not tell tampering from a wrong answer.
func TestRunMatrixSurfacesTamper(t *testing.T) {
	tasks := []Task{
		verdictTask{name: "tampered", v: harness.Verdict{OK: false, Detail: tamperedDetailPrefix + " protected fixture files altered: sumlist_test.go"}},
		verdictTask{name: "wrong", v: harness.Verdict{OK: false, Detail: "FAIL\tsumlist\t0.01s"}},
	}
	mr, err := RunMatrix(context.Background(), costLLM{}, MatrixOpts{
		Models: []string{"m1"}, Tasks: tasks, Samples: 1, MaxTurns: 2,
	})
	require.NoError(t, err)
	require.Len(t, mr.Outcomes, 2)

	byTask := map[string]Outcome{}
	for _, o := range mr.Outcomes {
		byTask[o.Task] = o
	}

	assert.False(t, byTask["tampered"].Pass)
	assert.True(t, byTask["tampered"].Tampered, "tampered verdict must set Outcome.Tampered")
	assert.False(t, byTask["wrong"].Pass)
	assert.False(t, byTask["wrong"].Tampered, "an ordinary wrong-answer fail must NOT be flagged tampered")
}

// TestRunMatrixRecordsCoverage proves the matrix records per-cell coverage and the
// expected count (samples × tasks-of-that-role), so the merge step can tell a
// complete cell from a budget-aborted partial one. With 1 model, 1 coder task and
// 3 samples, the coder cell expects 3 and (all completing) records 3.
func TestRunMatrixRecordsCoverage(t *testing.T) {
	tasks := []Task{fakeTask{name: "c1", role: registry.RoleCoder, pass: true}}
	mr, err := RunMatrix(context.Background(), costLLM{cost: 0.01}, MatrixOpts{
		Models: []string{"m1"}, Tasks: tasks, Samples: 3, MaxTurns: 3,
	})
	require.NoError(t, err)

	cell := CellKey{Model: "m1", Role: registry.RoleCoder}
	assert.Equal(t, 3, mr.Expected[cell], "expected = samples × tasks-of-role")
	assert.Equal(t, 3, mr.Coverage[cell], "all 3 runs completed -> full coverage")
}

// TestRunMatrixPartialCoverageOnAbort: a budget abort mid-cell leaves the coder
// cell with fewer recorded outcomes than expected, and that gap is visible in
// Coverage vs Expected.
func TestRunMatrixPartialCoverageOnAbort(t *testing.T) {
	tasks := []Task{fakeTask{name: "c1", role: registry.RoleCoder, pass: true}}
	mr, err := RunMatrix(context.Background(), costLLM{cost: 0.50}, MatrixOpts{
		Models: []string{"m1"}, Tasks: tasks, Samples: 5, MaxTurns: 3, MaxTotalCost: 0.60,
	})
	require.NoError(t, err)
	require.True(t, mr.Aborted)

	cell := CellKey{Model: "m1", Role: registry.RoleCoder}
	assert.Equal(t, 5, mr.Expected[cell], "expected counts the full battery regardless of abort")
	assert.Equal(t, 2, mr.Coverage[cell], "only 2 runs completed before the abort")
	assert.Less(t, mr.Coverage[cell], mr.Expected[cell], "partial cell")
}

// TestMergeExcludesPartialCells: a (model, role) cell with fewer outcomes than
// expected (budget abort mid-cell) is NOT written to the merged capabilities; the
// prior baseline value for that cell is retained.
func TestMergeExcludesPartialCells(t *testing.T) {
	cell := CellKey{Model: "m1", Role: registry.RoleCoder}
	mr := MatrixResult{
		Outcomes: []Outcome{
			{Model: "m1", Role: registry.RoleCoder, Pass: true, Res: harness.Result{ToolCallCount: 2}},
			{Model: "m1", Role: registry.RoleCoder, Pass: true, Res: harness.Result{ToolCallCount: 2}},
		},
		Coverage: map[CellKey]int{cell: 2},
		Expected: map[CellKey]int{cell: 5}, // 2 of 5 -> partial
	}

	measured, dropped := MeasuredComplete(mr)
	assert.Empty(t, measured["m1"], "partial cell must not enter the merged measured set")
	assert.Contains(t, dropDetail(dropped, cell), "partial", "drop must be logged as partial")
}

// TestMergeExcludesParseArtifacts: a cell where every sample failed with zero tool
// calls (the parse-artifact signature) is excluded and logged, not written as 0.00.
func TestMergeExcludesParseArtifacts(t *testing.T) {
	cell := CellKey{Model: "m1", Role: registry.RoleCoder}
	mr := MatrixResult{
		Outcomes: []Outcome{
			{Model: "m1", Role: registry.RoleCoder, Pass: false, Res: harness.Result{ToolCallCount: 0}},
			{Model: "m1", Role: registry.RoleCoder, Pass: false, Res: harness.Result{ToolCallCount: 0}},
			{Model: "m1", Role: registry.RoleCoder, Pass: false, Res: harness.Result{ToolCallCount: 0}},
		},
		Coverage: map[CellKey]int{cell: 3},
		Expected: map[CellKey]int{cell: 3}, // complete, but all-zero-tool-call artifact
	}

	measured, dropped := MeasuredComplete(mr)
	_, ok := measured["m1"][registry.RoleCoder]
	assert.False(t, ok, "all-zero-tool-call cell must not be written as 0.00")
	assert.Contains(t, dropDetail(dropped, cell), "parse", "drop must be logged as a parse artifact")
}

// TestMergeKeepsTamperedAsPresentFailure: a tampered sample is a REAL failed trial
// (it has tool calls), so a cell of tampered-but-with-tool-calls outcomes is
// complete coverage and DOES score — as a low/zero score, not an exclusion. This
// guards the reading that tampering != missing coverage and != parse artifact.
func TestMergeKeepsTamperedAsPresentFailure(t *testing.T) {
	cell := CellKey{Model: "m1", Role: registry.RoleCoder}
	mr := MatrixResult{
		Outcomes: []Outcome{
			{Model: "m1", Role: registry.RoleCoder, Pass: false, Tampered: true, Res: harness.Result{ToolCallCount: 4}},
			{Model: "m1", Role: registry.RoleCoder, Pass: false, Tampered: true, Res: harness.Result{ToolCallCount: 4}},
			{Model: "m1", Role: registry.RoleCoder, Pass: false, Tampered: true, Res: harness.Result{ToolCallCount: 4}},
		},
		Coverage: map[CellKey]int{cell: 3},
		Expected: map[CellKey]int{cell: 3},
	}

	measured, dropped := MeasuredComplete(mr)
	v, ok := measured["m1"][registry.RoleCoder]
	require.True(t, ok, "tampered-but-present cell is complete coverage and must score")
	assert.InDelta(t, 0.0, v, 1e-9, "all failed -> Wilson LB 0")
	assert.Empty(t, dropDetail(dropped, cell), "a real failed cell is not a dropped exclusion")
}

// TestMergeKeepsPassingZeroToolCallCell: the parse-artifact rule is all-failed AND
// all-zero-tool-call. A cell that PASSED with zero tool calls (a reviewer deciding on
// text alone, or a stub) is a real measurement and must be kept, not dropped.
func TestMergeKeepsPassingZeroToolCallCell(t *testing.T) {
	cell := CellKey{Model: "m1", Role: registry.RoleReviewer}
	mr := MatrixResult{
		Outcomes: []Outcome{
			{Model: "m1", Role: registry.RoleReviewer, Pass: true, Res: harness.Result{ToolCallCount: 0}},
			{Model: "m1", Role: registry.RoleReviewer, Pass: true, Res: harness.Result{ToolCallCount: 0}},
		},
		Coverage: map[CellKey]int{cell: 2},
		Expected: map[CellKey]int{cell: 2},
	}

	measured, dropped := MeasuredComplete(mr)
	_, ok := measured["m1"][registry.RoleReviewer]
	assert.True(t, ok, "a passing zero-tool-call cell is a real measurement, kept")
	assert.Empty(t, dropDetail(dropped, cell))
}

// TestReferenceModelAssertion: the reference model must clear the calibrated floor on
// every role it was measured on. Below-floor on any role is an error advising the
// battery is broken; an unmeasured role is skipped; an absent reference model asserts
// nothing.
func TestReferenceModelAssertion(t *testing.T) {
	floor := CalibratedFloor(24, DefaultZ)

	// Reference model below the floor (all coder samples fail) -> error.
	below := map[string]map[registry.Role]float64{"ref": {registry.RoleCoder: 0.0}}
	err := AssertReferenceModel(below, "ref", floor)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "battery", "error must advise the battery is broken")
	assert.Contains(t, err.Error(), "ref")

	// Reference model clears the floor on every measured role -> ok.
	above := map[string]map[registry.Role]float64{"ref": {
		registry.RoleCoder:    floor + 0.1,
		registry.RoleReviewer: floor + 0.05,
	}}
	require.NoError(t, AssertReferenceModel(above, "ref", floor))

	// Reference model absent from the measured set is NOT asserted -> no error.
	require.NoError(t, AssertReferenceModel(map[string]map[registry.Role]float64{}, "ref", floor))
}

// TestReferenceModelAssertionReviewerOnly: on a reviewer-only sweep (no coder cell),
// the canary must still fire from the reviewer score — the bug where a hardcoded
// coder role silently passed a reviewer-only run.
func TestReferenceModelAssertionReviewerOnly(t *testing.T) {
	floor := CalibratedFloor(24, DefaultZ)

	below := map[string]map[registry.Role]float64{"ref": {registry.RoleReviewer: 0.0}}
	err := AssertReferenceModel(below, "ref", floor)
	require.Error(t, err, "reviewer-only sweep below floor must fire the canary")
	assert.Contains(t, err.Error(), string(registry.RoleReviewer), "error must name the reviewer role")

	above := map[string]map[registry.Role]float64{"ref": {registry.RoleReviewer: floor + 0.1}}
	require.NoError(t, AssertReferenceModel(above, "ref", floor))
}

// TestReferenceModelAssertionSkipsUnmeasuredRole: a role the reference model was not
// measured on this sweep is skipped (can't assert what wasn't run), while a measured
// below-floor role still errors.
func TestReferenceModelAssertionSkipsUnmeasuredRole(t *testing.T) {
	floor := CalibratedFloor(24, DefaultZ)

	// Coder clears; reviewer was not measured (absent) -> no error despite the gap.
	mixed := map[string]map[registry.Role]float64{"ref": {registry.RoleCoder: floor + 0.1}}
	require.NoError(t, AssertReferenceModel(mixed, "ref", floor))
}

func TestRunMatrixForwardsProvider(t *testing.T) {
	var seen json.RawMessage

	prov := json.RawMessage(`{"sort":"throughput","require_parameters":true}`)
	_, err := RunMatrix(context.Background(), captureLLM{provider: &seen}, MatrixOpts{
		Models:  []string{"m1"},
		Tasks:   []Task{fakeTask{name: "c", role: registry.RoleCoder, pass: true}},
		Samples: 1, MaxTurns: 2, Provider: prov,
	})
	require.NoError(t, err)
	assert.JSONEq(t, string(prov), string(seen))
}
