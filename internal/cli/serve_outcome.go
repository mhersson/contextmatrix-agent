package cli

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-agent/internal/runoutcome"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// runOutcomes keeps each run's last run_outcome line until its exit.
type runOutcomes struct {
	mu   sync.Mutex
	last map[string]runoutcome.Data // correlation id -> last outcome
}

func newRunOutcomes() *runOutcomes {
	return &runOutcomes{last: make(map[string]runoutcome.Data)}
}

// record keeps maskedLine when it is an outcome line, whatever its cause.
func (o *runOutcomes) record(correlationID string, maskedLine []byte) {
	d, ok := runoutcome.Parse(maskedLine)
	if !ok {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	o.last[correlationID] = d
}

// take returns the run's last outcome and forgets it; zero when it had none.
func (o *runOutcomes) take(correlationID string) runoutcome.Data {
	o.mu.Lock()
	defer o.mu.Unlock()

	d := o.last[correlationID]
	delete(o.last, correlationID)

	return d
}

// stopCause says why a run ended. The host's own observations win over the
// worker's claim, and a worker claim naming a host value or an unknown one
// gives no cause.
func stopCause(exit executor.ExitCause, reported string, shuttingDown bool) string {
	if shuttingDown && (exit == executor.ExitKilled || reported == protocol.StopCauseEnded) {
		return protocol.StopCauseShutdown
	}

	switch exit {
	case executor.ExitKilled:
		return protocol.StopCauseKilled
	case executor.ExitTimeout, executor.ExitIdleTimeout:
		return protocol.StopCauseTimeout
	case executor.ExitWaitFailure, executor.ExitDaemonError:
		return protocol.StopCauseInfrastructure
	}

	if runoutcome.WorkerCause(reported) {
		return reported
	}

	return ""
}

// The run-kind status limits (wire contract 3.4).
const (
	maxUsageRows   = 16
	maxStatusBytes = 1 << 20
)

// exitPayload builds a run's terminal status from its container exit. A card
// run's status never carries a result or usage, whatever its line holds.
func exitPayload(
	k executor.RunKey,
	correlationID string,
	exitCode int64,
	exit executor.ExitCause,
	reported runoutcome.Data,
	shuttingDown bool,
) protocol.StatusCallbackPayload {
	status, message := exitStatus(exitCode, exit)

	p := protocol.StatusCallbackPayload{
		CardID:       k.CardID,
		Project:      k.Project,
		WorkerStatus: status,
		Message:      message,
		RunID:        correlationID,
		StopCause:    stopCause(exit, reported.Cause, shuttingDown),
	}

	if k.Kind == "" {
		return p
	}

	return withRunKindOutcome(p, k.Kind, reported)
}

// withRunKindOutcome adds a run kind's kind, usage and result to its terminal
// status. Usage goes on completed and failed alike; the result only on a
// completed run that stopped done. A status that breaks a limit fails the run
// with cause error rather than reach ContextMatrix, which would reject it.
func withRunKindOutcome(p protocol.StatusCallbackPayload, kind string, reported runoutcome.Data) protocol.StatusCallbackPayload {
	p.Kind = kind

	if len(reported.Usage) > maxUsageRows {
		return failedOverLimit(p, fmt.Sprintf("run reported %d usage rows, over the %d limit", len(reported.Usage), maxUsageRows))
	}

	p.Usage = reported.Usage

	if p.WorkerStatus == "completed" && p.StopCause == protocol.StopCauseDone {
		p.Result = reported.Result
	}

	if b, err := json.Marshal(p); err != nil || len(b) > maxStatusBytes {
		p.Result = nil

		return failedOverLimit(p, "run result over the status size limit")
	}

	return p
}

func failedOverLimit(p protocol.StatusCallbackPayload, message string) protocol.StatusCallbackPayload {
	p.WorkerStatus = "failed"
	p.Message = message
	p.StopCause = protocol.StopCauseError

	return p
}
