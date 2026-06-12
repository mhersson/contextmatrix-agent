// Package events defines the harness event stream: a sequence of typed events
// written human-readably to stdout and as JSON lines to a transcript.
package events

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

type Kind string

const (
	ModelRequest  Kind = "model_request"
	ModelResponse Kind = "model_response"
	ToolCallKind  Kind = "tool_call"
	ToolResult    Kind = "tool_result"
	UsageKind     Kind = "usage"
	StateChange   Kind = "state_change"
	ContextLimit  Kind = "context_limit"
	Verification  Kind = "verification"
	ErrorKind     Kind = "error"
	ToolRepair    Kind = "tool_repair"
	UserInput     Kind = "user_input"
)

// Event is one entry in the stream.
type Event struct {
	Seq  int            `json:"seq"`
	Kind Kind           `json:"kind"`
	Time time.Time      `json:"time"`
	Data map[string]any `json:"data,omitempty"`
}

// Emitter serializes events to a human writer and a JSON-lines transcript.
// Either writer may be nil. now is injectable for deterministic tests.
type Emitter struct {
	mu         sync.Mutex
	seq        int
	human      io.Writer
	transcript io.Writer
	now        func() time.Time
}

func NewEmitter(human, transcript io.Writer) *Emitter {
	return &Emitter{human: human, transcript: transcript, now: time.Now}
}

// Emit records an event and returns it.
func (e *Emitter) Emit(kind Kind, data map[string]any) Event {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.seq++

	ev := Event{Seq: e.seq, Kind: kind, Time: e.now(), Data: data}
	if e.transcript != nil {
		if b, err := json.Marshal(ev); err == nil {
			fmt.Fprintln(e.transcript, string(b)) //nolint:errcheck
		}
	}

	if e.human != nil {
		fmt.Fprintf(e.human, "[%d] %-14s %s\n", ev.Seq, ev.Kind, summarize(data)) //nolint:errcheck
	}

	return ev
}

func summarize(data map[string]any) string {
	if len(data) == 0 {
		return ""
	}

	b, _ := json.Marshal(data)

	s := string(b)
	if len(s) > 240 {
		s = s[:240] + "…"
	}

	return s
}
