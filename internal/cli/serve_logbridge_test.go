package cli

import (
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
)

func TestAgentMapExtra(t *testing.T) {
	tests := []struct {
		name      string
		kind      string
		data      map[string]any
		wantEntry protocol.LogEntry
		wantOK    bool
	}{
		{
			name:      "discussion kind maps content, agent, and model",
			kind:      "discussion",
			data:      map[string]any{"content": "hello", "agent": "planner", "model": "gpt-5"},
			wantEntry: protocol.LogEntry{Type: "text", Content: "hello", Agent: "planner", Model: "gpt-5"},
			wantOK:    true,
		},
		{
			name:      "missing fields yield empty strings",
			kind:      "discussion",
			data:      map[string]any{},
			wantEntry: protocol.LogEntry{Type: "text"},
			wantOK:    true,
		},
		{
			name:      "non-string fields yield empty strings",
			kind:      "discussion",
			data:      map[string]any{"content": 42, "agent": true, "model": nil},
			wantEntry: protocol.LogEntry{Type: "text"},
			wantOK:    true,
		},
		{
			name:      "a gate poll that moved becomes a system entry",
			kind:      "gate_progress",
			data:      map[string]any{"status": "CI checks: 2 passed, 5 pending, 0 failed", "repeat": false},
			wantEntry: protocol.LogEntry{Type: "system", Content: "CI checks: 2 passed, 5 pending, 0 failed"},
			wantOK:    true,
		},
		{
			// The gate emits on every poll to keep the idle watchdog fed; an
			// unchanged status must not reach the transcript.
			name:   "a repeated gate poll is dropped",
			kind:   "gate_progress",
			data:   map[string]any{"status": "CI checks: 2 passed, 5 pending, 0 failed", "repeat": true},
			wantOK: false,
		},
		{
			name:   "seat_debug kind is not mapped",
			kind:   "seat_debug",
			data:   map[string]any{"content": "hello"},
			wantOK: false,
		},
		{
			name:   "unknown kind is not mapped",
			kind:   "unknown",
			data:   nil,
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry, awaiting, ok := agentMapExtra(tc.kind, tc.data)
			assert.Equal(t, tc.wantOK, ok)
			assert.False(t, awaiting, "agentMapExtra never signals awaiting-human")

			if tc.wantOK {
				assert.Equal(t, tc.wantEntry, entry)
			}
		})
	}
}
