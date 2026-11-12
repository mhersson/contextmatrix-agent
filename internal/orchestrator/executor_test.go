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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func execTestRun(claudeCode bool, rec *recordingExec) *run {
	d := planTestDeps(&fakeOps{}, &planLLM{})
	d.Executor = rec
	d.Cfg.ClaudeCode = claudeCode

	return newRun(context.Background(), d, cmclient.TaskContext{})
}

func TestExecRunClaudeCodeNeverStartsOnAnExhaustedLedger(t *testing.T) {
	rec := &recordingExec{}
	o := execTestRun(true, rec)

	_, err := o.execRun(t.Context(), NewLedger(1, 1), RunRequest{Task: "t"})

	_, ok := errors.AsType[*BudgetExceededError](err)
	assert.True(t, ok)
	assert.Empty(t, rec.runs)
}

func TestExecRunClaudeCodeCapsTheRunAtTheRemainingBudget(t *testing.T) {
	tests := []struct {
		name   string
		ledger *Ledger
		cap    float64
		want   float64
	}{
		{"remaining", NewLedger(5, 2), 0, 3.0},
		{"caller cap kept", NewLedger(5, 2), 0.25, 0.25},
		{"uncapped", NewLedger(0, 0), 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingExec{}
			o := execTestRun(true, rec)

			_, err := o.execRun(t.Context(), tt.ledger, RunRequest{Task: "t", Config: harness.Config{MaxCostUSD: tt.cap}})
			require.NoError(t, err)
			require.Len(t, rec.runs, 1)
			assert.InDelta(t, tt.want, rec.runs[0].Config.MaxCostUSD, 1e-9)
		})
	}
}

func TestExecRunClaudeCodeFeedsTheLivenessMarker(t *testing.T) {
	rec := &recordingExec{}
	o := execTestRun(true, rec)
	o.progress = newProgressMarker(events.NewEmitter(nil, &bytes.Buffer{}), time.Now)

	_, err := o.execRun(t.Context(), NewLedger(0, 0), RunRequest{Task: "t"})
	require.NoError(t, err)
	require.Len(t, rec.runs, 1)
	assert.NotNil(t, rec.runs[0].Config.OnDelta)
}

func TestExecRunEndpointModeIsUnchanged(t *testing.T) {
	rec := &recordingExec{}
	o := execTestRun(false, rec)
	o.progress = newProgressMarker(events.NewEmitter(nil, &bytes.Buffer{}), time.Now)

	_, err := o.execRun(t.Context(), NewLedger(1, 1), RunRequest{Task: "t"})
	require.NoError(t, err)
	require.Len(t, rec.runs, 1)
	assert.Zero(t, rec.runs[0].Config.MaxCostUSD)
	assert.Nil(t, rec.runs[0].Config.OnDelta)
}

func TestExecRunRefusesEveryRunAfterAUsageLimit(t *testing.T) {
	for _, claudeCode := range []bool{true, false} {
		t.Run(fmt.Sprintf("claude_code=%v", claudeCode), func(t *testing.T) {
			rec := &recordingExec{results: []execResult{{err: fmt.Errorf("wrapped: %w", &UsageLimitError{Window: "seven_day"})}}}
			o := execTestRun(claudeCode, rec)

			_, err1 := o.execRun(t.Context(), NewLedger(0, 0), RunRequest{Task: "t"})
			ule1, ok := errors.AsType[*UsageLimitError](err1)
			require.True(t, ok)

			_, err2 := o.execRun(t.Context(), NewLedger(0, 0), RunRequest{Task: "t"})
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

func TestExecFanoutClaudeCodeCapsAndRecords(t *testing.T) {
	specs := []harness.SubagentSpec{{Role: "a"}, {Role: "b"}, {Role: "c"}}

	t.Run("capped ledger splits equally", func(t *testing.T) {
		rec := &recordingExec{}
		o := execTestRun(true, rec)
		o.ledger = NewLedger(5, 2)
		o.progress = newProgressMarker(events.NewEmitter(nil, &bytes.Buffer{}), time.Now)

		_, err := o.execFanout(t.Context(), FanoutRequest{Specs: specs})
		require.NoError(t, err)
		require.Len(t, rec.fanouts, 1)
		assert.InDelta(t, 1.0, rec.fanouts[0].BudgetUSD, 1e-9)
		assert.NotNil(t, rec.fanouts[0].OnDelta)
	})

	t.Run("uncapped ledger", func(t *testing.T) {
		rec := &recordingExec{}
		o := execTestRun(true, rec)
		o.ledger = NewLedger(0, 0)

		_, err := o.execFanout(t.Context(), FanoutRequest{Specs: specs})
		require.NoError(t, err)
		require.Len(t, rec.fanouts, 1)
		assert.Zero(t, rec.fanouts[0].BudgetUSD)
	})

	t.Run("exhausted ledger never starts", func(t *testing.T) {
		rec := &recordingExec{}
		o := execTestRun(true, rec)
		o.ledger = NewLedger(2, 2.5)

		_, err := o.execFanout(t.Context(), FanoutRequest{Specs: specs})
		be, ok := errors.AsType[*BudgetExceededError](err)
		require.True(t, ok)
		assert.InDelta(t, 2.5, be.Spent, 1e-9)
		assert.InDelta(t, 2.0, be.Max, 1e-9, "the refusal names the ceiling")
		assert.Empty(t, rec.fanouts)
	})

	t.Run("a child usage limit refuses the next run", func(t *testing.T) {
		rec := &limitFanoutExec{}
		o := execTestRun(true, &rec.recordingExec)
		o.d.Executor = rec
		o.ledger = NewLedger(0, 0)

		_, err := o.execFanout(t.Context(), FanoutRequest{Specs: specs})
		require.NoError(t, err)

		_, err = o.execRun(t.Context(), NewLedger(0, 0), RunRequest{Task: "t"})
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

	res, dur, err := o.runModel(t.Context(), nil, "t", "m")
	be, ok := errors.AsType[*BudgetExceededError](err)
	require.True(t, ok, "err=%v", err)
	assert.InDelta(t, 1.0, be.Spent, 1e-9)
	assert.InDelta(t, 1.0, be.Max, 1e-9)

	o.spendAndReport(t.Context(), o.ledger, "CARD-1", "warn", res, "m", "step", dur)

	_, _, err = o.runModel(t.Context(), nil, "t", "m")
	_, ok = errors.AsType[*BudgetExceededError](err)
	assert.True(t, ok)
	assert.Len(t, rec.runs, 1)
}

func TestMaxCostStopNamesTheBindingLedger(t *testing.T) {
	rec := &recordingExec{results: []execResult{{res: harness.Result{Reason: "max_cost", TotalCostUSD: 0.5}}}}
	o := execTestRun(true, rec)
	o.ledger = NewLedger(10, 9.5)

	_, _, err := o.runModelCfg(t.Context(), runScope{ledger: NewLedger(5, 0)}, nil, "t", "m", harness.Config{})
	be, ok := errors.AsType[*BudgetExceededError](err)
	require.True(t, ok, "err=%v", err)
	assert.InDelta(t, 10.0, be.Spent, 1e-9)
	assert.InDelta(t, 10.0, be.Max, 1e-9)
}

func TestExecRunClaudeCodeHonoursTheCardLedgerForCandidates(t *testing.T) {
	t.Run("capped at the smaller remainder", func(t *testing.T) {
		rec := &recordingExec{}
		o := execTestRun(true, rec)
		o.ledger = NewLedger(5, 4.5)

		_, err := o.execRun(t.Context(), NewLedger(5, 0), RunRequest{Task: "t"})
		require.NoError(t, err)
		require.Len(t, rec.runs, 1)
		assert.InDelta(t, 0.5, rec.runs[0].Config.MaxCostUSD, 1e-9)
	})

	t.Run("exhausted card ledger refuses", func(t *testing.T) {
		rec := &recordingExec{}
		o := execTestRun(true, rec)
		o.ledger = NewLedger(5, 5)

		_, err := o.execRun(t.Context(), NewLedger(5, 0), RunRequest{Task: "t"})
		_, ok := errors.AsType[*BudgetExceededError](err)
		assert.True(t, ok)
		assert.Empty(t, rec.runs)
	})
}
