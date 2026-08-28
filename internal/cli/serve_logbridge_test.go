package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/filelog"
	"github.com/mhersson/contextmatrix-agent/internal/webhook"
	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestSink builds a bridge, hub, and file logger rooted at t.TempDir(),
// wires the shared RedactorRegistry exactly as runServe does, and
// returns the OnLog callback plus the registry and the SAME *filelog.Logger the
// callback writes through (a second Logger over the same directory would not
// share its in-memory open-file state, so Begin/End must run on this one).
func newTestSink(t *testing.T) (sink func(project, cardID, correlationID string, line []byte, stderr bool), registry *logbridge.RedactorRegistry, files *filelog.Logger, dir string) {
	t.Helper()

	dir = t.TempDir()
	files = filelog.New(dir, nil)
	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.Project }, nil)
	bridge := logbridge.NewBridge(logbridge.BridgeConfig{Hub: hub})
	registry = logbridge.NewRedactorRegistry(bridge)

	return containerLogSink(bridge, files, registry), registry, files, dir
}

func readCardLog(t *testing.T, dir, project, cardID string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, project, cardID+".log"))
	require.NoError(t, err)

	return string(data)
}

func TestContainerLogSink_RedactsSecretInDurableFileLog(t *testing.T) {
	sink, registry, files, dir := newTestSink(t)

	registry.AddSessionKey("proj/card-1", "PLACEHOLDER-SECRET-AAAA")

	files.Begin("proj", "card-1", "container-1", "corr-1")

	sink("proj", "card-1", "corr-1", []byte(`{"kind":"tool_call","data":{"raw_args":"token=PLACEHOLDER-SECRET-AAAA"}}`), false)
	sink("proj", "card-1", "corr-1", []byte("fatal: auth failed for PLACEHOLDER-SECRET-AAAA"), true)

	files.End("proj", "card-1", "corr-1", 0, "exit")

	content := readCardLog(t, dir, "proj", "card-1")

	assert.NotContains(t, content, "PLACEHOLDER-SECRET-AAAA",
		"the durable file log must never store the raw secret")
	assert.Equal(t, 2, strings.Count(content, "[REDACTED]"),
		"both the stdout JSONL line and the stderr line must be masked")
}

func TestContainerLogSink_RotatedTokenRedactsFromRegistrationOnward(t *testing.T) {
	sink, registry, files, dir := newTestSink(t)

	files.Begin("proj", "card-2", "container-2", "corr-1")

	// Before the rotated token is registered (simulating OnTokenRefresh
	// firing mid-run), the redactor does not know it yet.
	sink("proj", "card-2", "corr-1", []byte("using token ROTATED-PLACEHOLDER-BBBB"), true)

	registry.AddSessionKey("proj/card-2", "ROTATED-PLACEHOLDER-BBBB")

	sink("proj", "card-2", "corr-1", []byte("using token ROTATED-PLACEHOLDER-BBBB"), true)

	files.End("proj", "card-2", "corr-1", 0, "exit")

	content := readCardLog(t, dir, "proj", "card-2")

	assert.Equal(t, 1, strings.Count(content, "ROTATED-PLACEHOLDER-BBBB"),
		"the line written before registration keeps the literal token")
	assert.Equal(t, 1, strings.Count(content, "[REDACTED]"),
		"the line written after registration is masked")
}

func TestContainerLogSink_TeeForwardsToSSEBridgeToo(t *testing.T) {
	dir := t.TempDir()
	files := filelog.New(dir, nil)
	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.Project }, nil)
	bridge := logbridge.NewBridge(logbridge.BridgeConfig{Hub: hub})
	registry := logbridge.NewRedactorRegistry(bridge)
	sink := containerLogSink(bridge, files, registry)

	subID, ch := hub.Subscribe("")
	defer hub.Unsubscribe(subID)

	registry.AddSessionKey("proj/card-3", "PLACEHOLDER-SECRET-CCCC")

	files.Begin("proj", "card-3", "container-3", "corr-1")
	sink("proj", "card-3", "corr-1", []byte("leaked PLACEHOLDER-SECRET-CCCC here"), true)
	files.End("proj", "card-3", "corr-1", 0, "exit")

	select {
	case entry := <-ch:
		assert.NotContains(t, entry.Content, "PLACEHOLDER-SECRET-CCCC",
			"one AddSessionKey call must mask the secret on the SSE stream too")
		assert.Contains(t, entry.Content, "[REDACTED]")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the hub to publish the bridged entry")
	}

	content := readCardLog(t, dir, "proj", "card-3")
	assert.NotContains(t, content, "PLACEHOLDER-SECRET-CCCC")
	assert.Contains(t, content, "[REDACTED]")
}

func TestContainerLogSink_RemoveSessionKeyStopsFileRedaction(t *testing.T) {
	sink, registry, files, dir := newTestSink(t)

	files.Begin("proj", "card-4", "container-4", "corr-1")

	registry.AddSessionKey("proj/card-4", "PLACEHOLDER-SECRET-DDDD")
	sink("proj", "card-4", "corr-1", []byte("first PLACEHOLDER-SECRET-DDDD"), true)

	registry.RemoveSessionKey("proj/card-4")
	sink("proj", "card-4", "corr-1", []byte("second PLACEHOLDER-SECRET-DDDD"), true)

	files.End("proj", "card-4", "corr-1", 0, "exit")

	content := readCardLog(t, dir, "proj", "card-4")

	assert.Equal(t, 1, strings.Count(content, "[REDACTED]"),
		"only the line written while the key was registered is masked")
	assert.Equal(t, 1, strings.Count(content, "PLACEHOLDER-SECRET-DDDD"),
		"the line written after removal keeps the literal secret")
}

// TestContainerLogSink_StaleExitCannotStripFreshRunKeys pins the fix for the
// pump-drain re-trigger race: a re-triggered run admitted while a previous
// run's exit callback is still in flight (up to pumpDrainTimeout after the
// tracker forgets the old run) must not have its redaction keys stripped by
// the stale run's own RemoveSessionKey call. Session ids are now scoped by
// the run's correlation id (webhook.SessionID), so run 1's removal and run 2's
// registration are structurally different map keys - no shared mutable
// "who owns this" state to race on. Both surfaces the redaction covers - the
// live SSE stream and the durable file log - are asserted.
func TestContainerLogSink_StaleExitCannotStripFreshRunKeys(t *testing.T) {
	dir := t.TempDir()
	files := filelog.New(dir, nil)
	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.Project }, nil)
	bridge := logbridge.NewBridge(logbridge.BridgeConfig{Hub: hub})
	registry := logbridge.NewRedactorRegistry(bridge)
	sink := containerLogSink(bridge, files, registry)

	subID, ch := hub.Subscribe("proj")
	defer hub.Unsubscribe(subID)

	// Run 1 registers its own secret under its own correlation id.
	registry.AddSessionKey(webhook.SessionID("proj", "card-race", "corr-1"), "PLACEHOLDER-RUN1-SECRET")

	// Run 2 is a re-trigger of the SAME card, admitted during run 1's
	// pump-drain window, and registers its own secret under a DIFFERENT
	// correlation id.
	registry.AddSessionKey(webhook.SessionID("proj", "card-race", "corr-2"), "PLACEHOLDER-RUN2-SECRET")

	files.Begin("proj", "card-race", "container-2", "corr-1")

	// Run 1's stale exit fires now and removes only its own bucket.
	registry.RemoveSessionKey(webhook.SessionID("proj", "card-race", "corr-1"))

	// Run 2's secret must still redact on the durable file log...
	sink("proj", "card-race", "corr-1", []byte("leaked PLACEHOLDER-RUN2-SECRET here"), true)
	files.End("proj", "card-race", "corr-1", 0, "exit")

	content := readCardLog(t, dir, "proj", "card-race")
	assert.NotContains(t, content, "PLACEHOLDER-RUN2-SECRET",
		"a stale run's exit must not strip a fresh run's durable-file redaction")
	assert.Contains(t, content, "[REDACTED]")

	// ...and on the live SSE stream.
	select {
	case entry := <-ch:
		assert.NotContains(t, entry.Content, "PLACEHOLDER-RUN2-SECRET",
			"a stale run's exit must not strip a fresh run's SSE redaction")
		assert.Contains(t, entry.Content, "[REDACTED]")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the hub to publish the bridged entry")
	}
}

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
			// Shadow telemetry stays off a human's live card stream. The
			// mechanism is the ABSENCE of an arm in agentMapExtra, and adding
			// one is a two-line change that would look harmless.
			name:   "sizing_escalation kind is not mapped",
			kind:   "sizing_escalation",
			data:   map[string]any{"subtask": "SUB-1", "axis": "budget"},
			wantOK: false,
		},
		{
			// Shadow telemetry stays off a human's live card stream. The
			// mechanism is the ABSENCE of an arm in agentMapExtra, and adding
			// one is a two-line change that would look harmless.
			name:   "sizing_observation kind is not mapped",
			kind:   "sizing_observation",
			data:   map[string]any{"subtask": "SUB-1", "turns": 7},
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
