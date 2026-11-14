package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func execTestRun(claudeCode bool, rec *recordingExec) *run {
	d := planTestDeps(&fakeOps{}, &planLLM{})
	d.Executor = rec
	d.Cfg.ClaudeCode = claudeCode

	return newRun(context.Background(), d, cmclient.TaskContext{})
}

func TestExecRunEndpointModeIsUnchanged(t *testing.T) {
	rec := &recordingExec{}
	o := execTestRun(false, rec)
	o.progress = newProgressMarker(events.NewEmitter(nil, &bytes.Buffer{}), time.Now)

	_, err := o.execRun(t.Context(), RunRequest{Task: "t"})
	require.NoError(t, err)
	require.Len(t, rec.runs, 1)
	assert.Zero(t, rec.runs[0].Config.MaxCostUSD)
}

func TestExecRunRefusesEveryRunAfterAUsageLimit(t *testing.T) {
	for _, claudeCode := range []bool{true, false} {
		t.Run(fmt.Sprintf("claude_code=%v", claudeCode), func(t *testing.T) {
			rec := &recordingExec{results: []execResult{{err: fmt.Errorf("wrapped: %w", &UsageLimitError{Window: "seven_day"})}}}
			o := execTestRun(claudeCode, rec)

			_, err1 := o.execRun(t.Context(), RunRequest{Task: "t"})
			ule1, ok := errors.AsType[*UsageLimitError](err1)
			require.True(t, ok)

			_, err2 := o.execRun(t.Context(), RunRequest{Task: "t"})
			ule2, ok := errors.AsType[*UsageLimitError](err2)
			require.True(t, ok)
			assert.Same(t, ule1, ule2)
			assert.Len(t, rec.runs, 1)

			_, err3 := o.execFanout(t.Context(), FanoutRequest{Specs: []harness.SubagentSpec{{Role: "a"}}})
			_, ok = errors.AsType[*UsageLimitError](err3)
			assert.True(t, ok)
			assert.Empty(t, rec.fanouts)
		})
	}
}

func TestExecFanoutClaudeCodeWiresProgressAndRecords(t *testing.T) {
	specs := []harness.SubagentSpec{{Role: "a"}, {Role: "b"}, {Role: "c"}}

	t.Run("a panel child's stream feeds the marker", func(t *testing.T) {
		rec := &recordingExec{}
		o := execTestRun(true, rec)

		var out bytes.Buffer

		clock := newStepClock()
		o.progress = newProgressMarker(events.NewEmitter(nil, &out), clock.now)

		_, err := o.execFanout(t.Context(), FanoutRequest{Specs: specs})
		require.NoError(t, err)
		require.Len(t, rec.fanouts, 1)
		require.NotNil(t, rec.fanouts[0].OnDelta)

		clock.advance(modelProgressInterval)
		rec.fanouts[0].OnDelta(llm.Delta{})
		assert.Len(t, progressLines(t, out.String()), 1)
	})

	t.Run("a child usage limit refuses the next run", func(t *testing.T) {
		rec := &limitFanoutExec{}
		o := execTestRun(true, &rec.recordingExec)
		o.d.Executor = rec

		_, err := o.execFanout(t.Context(), FanoutRequest{Specs: specs})
		require.NoError(t, err)

		_, err = o.execRun(t.Context(), RunRequest{Task: "t"})
		_, ok := errors.AsType[*UsageLimitError](err)
		assert.True(t, ok)
		assert.Empty(t, rec.runs)
	})
}

// limitFanoutExec answers every fan-out with a child that hit the usage limit.
type limitFanoutExec struct{ recordingExec }

func (l *limitFanoutExec) SpawnSubagents(_ context.Context, req FanoutRequest) ([]harness.SubagentResult, error) {
	out := make([]harness.SubagentResult, len(req.Specs))
	for i, s := range req.Specs {
		out[i] = harness.SubagentResult{Role: s.Role}
	}

	out[0].Err = fmt.Errorf("child: %w", &UsageLimitError{Window: "five_hour"})

	return out, nil
}

func TestMaxCostStopParksOnTheBudget(t *testing.T) {
	rec := &recordingExec{results: []execResult{{res: harness.Result{Reason: "max_cost", TotalCostUSD: 1.0}}}}
	o := execTestRun(true, rec)
	o.ledger = NewLedger(1, 0)

	_, _, err := o.runModel(t.Context(), nil, "t", "m")
	be, ok := errors.AsType[*BudgetExceededError](err)
	require.True(t, ok, "err=%v", err)
	assert.InDelta(t, 1.0, be.Spent, 1e-9)
	assert.InDelta(t, 1.0, be.Max, 1e-9)
}

func TestMaxCostStopNamesTheScopeLedger(t *testing.T) {
	rec := &recordingExec{results: []execResult{{res: harness.Result{Reason: "max_cost", TotalCostUSD: 0.5}}}}
	o := execTestRun(true, rec)
	o.ledger = NewLedger(10, 9.5)

	_, _, err := o.runModelCfg(t.Context(), runScope{ledger: NewLedger(5, 0)}, nil, "t", "m", harness.Config{})
	be, ok := errors.AsType[*BudgetExceededError](err)
	require.True(t, ok, "err=%v", err)
	assert.InDelta(t, 0.5, be.Spent, 1e-9)
	assert.InDelta(t, 5.0, be.Max, 1e-9)
}

func TestClaudeCodeRunsKeepNoCardCeiling(t *testing.T) {
	tests := []struct {
		name       string
		claudeCode bool
		wantParked bool
	}{
		{"claude code runs past the ceiling", true, false},
		{"endpoint parks on the ceiling", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingExec{}
			d := documentTestDeps(&fakeOps{}, &fakeGit{}, &planLLM{})
			d.Executor = rec
			d.Cfg.ClaudeCode = tt.claudeCode

			o := newDocumentRun(d, cmclient.TaskContext{Title: "T", Description: "body", ReportedCostUSD: 6}, 5)

			err := runDocument(t.Context(), o)

			_, parked := errors.AsType[*BudgetExceededError](err)
			assert.Equal(t, tt.wantParked, parked, "err=%v", err)

			if tt.wantParked {
				assert.Empty(t, rec.runs)

				return
			}

			require.NoError(t, err)
			require.NotEmpty(t, rec.runs)
			assert.Zero(t, rec.runs[0].Config.MaxCostUSD)
		})
	}
}

func TestHarnessConfigFeedsTheLivenessMarker(t *testing.T) {
	var out bytes.Buffer

	o := execTestRun(true, &recordingExec{})
	clock := newStepClock()
	o.progress = newProgressMarker(events.NewEmitter(nil, &out), clock.now)

	cfg := o.harnessConfig("default/model")
	require.NotNil(t, cfg.OnDelta)

	clock.advance(modelProgressInterval)
	cfg.OnDelta(llm.Delta{})
	assert.Len(t, progressLines(t, out.String()), 1)
}
