package runoutcome_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/runoutcome"
	"github.com/mhersson/contextmatrix-harness/events"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmitThenParse(t *testing.T) {
	var out bytes.Buffer

	emit := events.NewEmitter(io.Discard, &out)
	runoutcome.Emit(emit, runoutcome.Data{Cause: protocol.StopCauseVerifyParked})

	d, ok := runoutcome.Parse(bytes.TrimSpace(out.Bytes()))
	require.True(t, ok)
	assert.Equal(t, protocol.StopCauseVerifyParked, d.Cause)
}

func TestEmitCarriesResultAndUsage(t *testing.T) {
	var out bytes.Buffer

	want := runoutcome.Data{
		Cause:  protocol.StopCauseDone,
		Result: &protocol.RunKindResult{Check: &protocol.CheckResult{Candidates: []string{"./ci/verify"}}},
		Usage: []protocol.RunUsage{{
			Model:         "vendor/model-a",
			LogTokenUsage: protocol.LogTokenUsage{InputTokens: 10, OutputTokens: 2, CostUSD: 0.01},
		}},
	}

	runoutcome.Emit(events.NewEmitter(io.Discard, &out), want)

	got, ok := runoutcome.Parse(bytes.TrimSpace(out.Bytes()))
	require.True(t, ok)
	assert.Equal(t, want, got)
}

func TestParseRejectsOtherLines(t *testing.T) {
	for _, line := range []string{
		`{"kind":"text","data":{"cause":"done"}}`,
		`{"kind":"run_outcome","data":`, // cut by the 1 MiB line limit
		`not json mentioning "run_outcome"`,
		``,
	} {
		_, ok := runoutcome.Parse([]byte(line))
		assert.False(t, ok, line)
	}
}

func TestWorkerCause(t *testing.T) {
	for _, c := range []string{
		protocol.StopCauseDone, protocol.StopCauseEnded, protocol.StopCauseReviewParked,
		protocol.StopCauseGatesParked, protocol.StopCauseUsageLimit, protocol.StopCauseBudgetExceeded,
		protocol.StopCauseContextLimit, protocol.StopCauseMaxTurns, protocol.StopCauseToolchainMissing,
		protocol.StopCauseNoModel, protocol.StopCauseVerifyParked, protocol.StopCauseEmptySubtask,
		protocol.StopCauseSplitOverflow, protocol.StopCauseError,
	} {
		assert.True(t, runoutcome.WorkerCause(c), c)
	}

	for _, c := range []string{
		protocol.StopCauseKilled, protocol.StopCauseShutdown, protocol.StopCauseTimeout,
		protocol.StopCauseInfrastructure, "", "future_value",
	} {
		assert.False(t, runoutcome.WorkerCause(c), c)
	}
}
