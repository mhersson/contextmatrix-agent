package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-agent/internal/runoutcome"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

func TestStopCause(t *testing.T) {
	cases := []struct {
		name     string
		exit     executor.ExitCause
		reported string
		shutdown bool
		want     string
	}{
		{"worker cause", executor.ExitNormal, "verify_parked", false, "verify_parked"},
		{"done", executor.ExitNormal, "done", false, "done"},
		{"ended", executor.ExitNormal, "ended", false, "ended"},
		{"ended during shutdown", executor.ExitNormal, "ended", true, "shutdown"},
		{"killed during shutdown", executor.ExitKilled, "", true, "shutdown"},
		{"killed wins over the worker", executor.ExitKilled, "done", false, "killed"},
		{"timeout", executor.ExitTimeout, "", false, "timeout"},
		{"idle timeout wins over the worker", executor.ExitIdleTimeout, "error", false, "timeout"},
		{"wait failure", executor.ExitWaitFailure, "", false, "infrastructure"},
		{"daemon error wins over the worker", executor.ExitDaemonError, "done", false, "infrastructure"},
		{"a worker cannot claim a host value", executor.ExitNormal, "killed", false, ""},
		{"a newer image's value", executor.ExitNormal, "future_value", false, ""},
		{"an older image wrote no line", executor.ExitNormal, "", false, ""},
		{"another worker cause during shutdown", executor.ExitNormal, "done", true, "done"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stopCause(tc.exit, tc.reported, tc.shutdown))
		})
	}
}

func TestRunOutcomesKeepTheLastLine(t *testing.T) {
	o := newRunOutcomes()
	o.record("r1", []byte(`{"kind":"run_outcome","data":{"cause":"killed"}}`)) // printed by a model's command
	o.record("r1", []byte(`{"kind":"text","data":{}}`))
	o.record("r1", []byte(`{"kind":"run_outcome","data":{"cause":"verify_parked"}}`))
	o.record("r2", []byte(`{"kind":"run_outcome","data":{"cause":"done"}}`))

	assert.Equal(t, "verify_parked", o.take("r1").Cause)
	assert.Empty(t, o.take("r1").Cause) // taken once
	assert.Equal(t, "done", o.take("r2").Cause)
}

func TestExitPayload(t *testing.T) {
	cases := []struct {
		name          string
		exitCode      int64
		exit          executor.ExitCause
		reported      runoutcome.Data
		shutdown      bool
		wantStatus    string
		wantMessage   string
		wantStopCause string
	}{
		{"worker failure", 1, executor.ExitNormal, runoutcome.Data{Cause: "verify_parked"}, false, "failed", "worker exited with code 1", "verify_parked"},
		{"worker success", 0, executor.ExitNormal, runoutcome.Data{Cause: "done"}, false, "completed", "", "done"},
		{"killed without a line", -1, executor.ExitKilled, runoutcome.Data{}, false, "failed", "worker exited with code -1", "killed"},
		{"shutdown kill", -1, executor.ExitKilled, runoutcome.Data{}, true, "failed", "worker exited with code -1", "shutdown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := exitPayload(executor.RunKey{Project: "proj", CardID: "CM-1"}, "CM-1-abc", tc.exitCode, tc.exit, tc.reported, tc.shutdown)

			assert.Equal(t, protocol.StatusCallbackPayload{
				CardID:       "CM-1",
				Project:      "proj",
				WorkerStatus: tc.wantStatus,
				Message:      tc.wantMessage,
				RunID:        "CM-1-abc",
				StopCause:    tc.wantStopCause,
			}, got)
		})
	}
}

func TestExitPayloadForRunKinds(t *testing.T) {
	investigate := executor.RunKey{Project: "proj", CardID: "CM-1", Kind: protocol.RunKindInvestigate}
	card := executor.RunKey{Project: "proj", CardID: "CM-1"}

	result := &protocol.RunKindResult{Investigate: &protocol.InvestigateResult{Diagnosis: "d", Cause: protocol.DiagnosisModel}}
	huge := &protocol.RunKindResult{Investigate: &protocol.InvestigateResult{Diagnosis: strings.Repeat("x", 1<<20), Cause: protocol.DiagnosisModel}}
	usage := []protocol.RunUsage{{Model: "vendor/model-a", LogTokenUsage: protocol.LogTokenUsage{InputTokens: 5}}}
	sixteen := make([]protocol.RunUsage, 16)
	seventeen := make([]protocol.RunUsage, 17)

	cases := []struct {
		name       string
		key        executor.RunKey
		exitCode   int64
		reported   runoutcome.Data
		wantStatus string
		wantCause  string
		wantKind   string
		wantResult *protocol.RunKindResult
		wantUsage  []protocol.RunUsage
	}{
		{
			"completed and done keeps the result", investigate, 0,
			runoutcome.Data{Cause: protocol.StopCauseDone, Result: result, Usage: usage},
			"completed", protocol.StopCauseDone, protocol.RunKindInvestigate, result, usage,
		},
		{
			"a failed run drops the result", investigate, 1,
			runoutcome.Data{Cause: protocol.StopCauseBudgetExceeded, Result: result, Usage: usage},
			"failed", protocol.StopCauseBudgetExceeded, protocol.RunKindInvestigate, nil, usage,
		},
		{
			"a card run carries neither", card, 0,
			runoutcome.Data{Cause: protocol.StopCauseDone, Result: result, Usage: usage},
			"completed", protocol.StopCauseDone, "", nil, nil,
		},
		{
			"16 usage rows are kept", investigate, 0,
			runoutcome.Data{Cause: protocol.StopCauseDone, Result: result, Usage: sixteen},
			"completed", protocol.StopCauseDone, protocol.RunKindInvestigate, result, sixteen,
		},
		{
			"17 usage rows fail the run", investigate, 0,
			runoutcome.Data{Cause: protocol.StopCauseDone, Result: result, Usage: seventeen},
			"failed", protocol.StopCauseError, protocol.RunKindInvestigate, nil, nil,
		},
		{
			"a status over 1 MiB fails the run", investigate, 0,
			runoutcome.Data{Cause: protocol.StopCauseDone, Result: huge, Usage: usage},
			"failed", protocol.StopCauseError, protocol.RunKindInvestigate, nil, usage,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := exitPayload(tc.key, "CM-1-abc", tc.exitCode, executor.ExitNormal, tc.reported, false)

			assert.Equal(t, tc.wantStatus, got.WorkerStatus)
			assert.Equal(t, tc.wantCause, got.StopCause)
			assert.Equal(t, tc.wantKind, got.Kind)
			assert.Equal(t, tc.wantResult, got.Result)
			assert.Equal(t, tc.wantUsage, got.Usage)
			assert.Equal(t, "CM-1-abc", got.RunID)
		})
	}
}
