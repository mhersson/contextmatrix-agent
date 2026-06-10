package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSweepRunsBothAndComparisonRenders(t *testing.T) {
	a := harness.Result{Completed: true, Reason: "done", Turns: 4, ToolCallCount: 3, RepairCount: 2, TotalCostUSD: 0.01, ModelUsed: "weak"}
	b := harness.Result{Completed: true, Reason: "done", Turns: 3, ToolCallCount: 2, RepairCount: 0, TotalCostUSD: 0.05, ModelUsed: "control"}

	var buf bytes.Buffer
	renderComparison(&buf, []sweepRow{{label: "weak", res: a}, {label: "control", res: b}})
	out := buf.String()
	assert.Contains(t, out, "weak")
	assert.Contains(t, out, "control")
	assert.Contains(t, out, "repairs")
}

func TestSweepDispatchRunsEachModel(t *testing.T) {
	calls := 0
	rows, err := sweepDispatch(context.Background(), []string{"weak", "control"},
		func(ctx context.Context, model string) (harness.Result, error) {
			calls++

			return harness.Result{ModelUsed: model, Completed: true, Reason: "done"}, nil
		})
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	require.Len(t, rows, 2)
	assert.Equal(t, "weak", rows[0].label)
	assert.Equal(t, "control", rows[1].label)
}
