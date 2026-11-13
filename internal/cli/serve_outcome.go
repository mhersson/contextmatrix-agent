package cli

import (
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

// exitPayload builds a run's terminal status from its container exit.
func exitPayload(
	project, cardID, correlationID string,
	exitCode int64,
	exit executor.ExitCause,
	reported runoutcome.Data,
	shuttingDown bool,
) protocol.StatusCallbackPayload {
	status, message := exitStatus(exitCode, exit)

	return protocol.StatusCallbackPayload{
		CardID:       cardID,
		Project:      project,
		WorkerStatus: status,
		Message:      message,
		RunID:        correlationID,
		StopCause:    stopCause(exit, reported.Cause, shuttingDown),
	}
}
