// Package runoutcome is the worker's last stdout line: why the run ended. The
// worker writes it after every subprocess it started has ended, so a line a
// model's command printed earlier is overridden; serve reads it from the
// masked stdout and keeps the last one per run.
package runoutcome

import (
	"bytes"
	"encoding/json"

	"github.com/mhersson/contextmatrix-harness/events"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// Kind is the envelope kind of the outcome line.
const Kind events.Kind = "run_outcome"

// Data is the outcome line's payload.
type Data struct {
	Cause string `json:"cause,omitempty"`
}

// Emit writes the outcome line through the run's event emitter.
func Emit(emit *events.Emitter, d Data) {
	data := map[string]any{}
	if d.Cause != "" {
		data["cause"] = d.Cause
	}

	emit.Emit(Kind, data)
}

// Parse decodes an outcome line. ok is false for any other line, including a
// line cut by the log line limit.
func Parse(line []byte) (Data, bool) {
	if !bytes.Contains(line, []byte(Kind)) {
		return Data{}, false
	}

	var env struct {
		Kind events.Kind `json:"kind"`
		Data Data        `json:"data"`
	}
	if err := json.Unmarshal(line, &env); err != nil || env.Kind != Kind {
		return Data{}, false
	}

	return env.Data, true
}

// WorkerCause reports whether cause is a value the worker itself reports.
// Serve-only values and unknown ones are not.
func WorkerCause(cause string) bool {
	switch cause {
	case protocol.StopCauseDone,
		protocol.StopCauseEnded,
		protocol.StopCauseReviewParked,
		protocol.StopCauseGatesParked,
		protocol.StopCauseUsageLimit,
		protocol.StopCauseBudgetExceeded,
		protocol.StopCauseContextLimit,
		protocol.StopCauseMaxTurns,
		protocol.StopCauseToolchainMissing,
		protocol.StopCauseNoModel,
		protocol.StopCauseVerifyParked,
		protocol.StopCauseEmptySubtask,
		protocol.StopCauseSplitOverflow,
		protocol.StopCauseError:
		return true
	}

	return false
}
