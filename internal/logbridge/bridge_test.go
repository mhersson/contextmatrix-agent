package logbridge_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-agent/internal/logbridge"
	"github.com/mhersson/contextmatrix-harness/redact"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testProject = "proj"
	testCard    = "PROJ-001"
)

// makeEvent encodes one worker stdout JSONL line for a given kind and data.
func makeEvent(kind string, data map[string]any) []byte {
	ev := map[string]any{
		"seq":  1,
		"kind": kind,
		"time": time.Now().Format(time.RFC3339),
	}
	if data != nil {
		ev["data"] = data
	}

	b, _ := json.Marshal(ev)

	return b
}

// TestMappingTable covers every row of the kind→LogEntry spec.
func TestMappingTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		line       []byte
		wantType   string // empty = expect skip
		wantModel  string
		wantToolID string
		wantUsage  *protocol.LogTokenUsage
		// awaiting expected value (-1 = not fired, 0 = false, 1 = true)
		wantAwaiting int
		checkContent func(t *testing.T, content string)
	}{
		{
			name: "model_response → text with content and model",
			line: makeEvent("model_response", map[string]any{
				"content": "Hello, world!",
				"model":   "test-model",
			}),
			wantType:     "text",
			wantModel:    "test-model",
			wantAwaiting: 0,
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.Equal(t, "Hello, world!", content)
			},
		},
		{
			name: "thinking → thinking with content (awaiting cleared)",
			line: makeEvent("thinking", map[string]any{
				"content": "Let me reason about this.",
				"turn":    float64(2),
			}),
			wantType:     "thinking",
			wantAwaiting: 0,
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.Equal(t, "Let me reason about this.", content)
			},
		},
		{
			name: "tool_call → tool_call with id and formatted content",
			line: makeEvent("tool_call", map[string]any{
				"id":       "call_abc123",
				"name":     "bash",
				"raw_args": `{"cmd":"ls"}`,
			}),
			wantType:     "tool_call",
			wantToolID:   "call_abc123",
			wantAwaiting: 0,
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.Equal(t, `bash({"cmd":"ls"})`, content)
			},
		},
		{
			name: "tool_call truncated at 200 chars",
			line: (func() []byte {
				bigArgs := `{"x":"` + strings.Repeat("a", 300) + `"}`

				return makeEvent("tool_call", map[string]any{
					"id":       "call_trunc",
					"name":     "bash",
					"raw_args": bigArgs,
				})
			})(),
			wantType:     "tool_call",
			wantToolID:   "call_trunc",
			wantAwaiting: 0,
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.LessOrEqual(t, len(content), 200)
			},
		},
		{
			name: "tool_call truncation is rune-safe on multi-byte content",
			line: (func() []byte {
				// "é" is 2 bytes; the prefix `bash({"x":"` is 11 bytes, so the
				// 200-byte cut lands mid-rune unless truncation backs off.
				bigArgs := `{"x":"` + strings.Repeat("é", 300) + `"}`

				return makeEvent("tool_call", map[string]any{
					"id":       "call_mb",
					"name":     "bash",
					"raw_args": bigArgs,
				})
			})(),
			wantType:     "tool_call",
			wantToolID:   "call_mb",
			wantAwaiting: 0,
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.LessOrEqual(t, len(content), 200)
				assert.True(t, utf8.ValidString(content), "truncated content must be valid UTF-8")
			},
		},
		{
			name: "state_change summary truncation is rune-safe on multi-byte content",
			line: makeEvent("state_change", map[string]any{
				// "世" is 3 bytes; the JSON prefix `{"warning":"` is 12 bytes,
				// so the 200-byte cut lands mid-rune.
				"warning": strings.Repeat("世", 150),
			}),
			wantType:     "system",
			wantAwaiting: 0,
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.LessOrEqual(t, len(content), 200)
				assert.True(t, utf8.ValidString(content), "summarized content must be valid UTF-8")
			},
		},
		{
			name: "usage → usage with token counts and model",
			line: makeEvent("usage", map[string]any{
				"prompt_tokens":     float64(100),
				"completion_tokens": float64(50),
				"model":             "usage-model",
			}),
			wantType:     "usage",
			wantModel:    "usage-model",
			wantAwaiting: 0,
			wantUsage: &protocol.LogTokenUsage{
				InputTokens:  100,
				OutputTokens: 50,
			},
		},
		{
			name: "state_change awaiting_human → system + awaiting=true",
			line: makeEvent("state_change", map[string]any{
				"state": "awaiting_human",
				"turns": float64(3),
			}),
			wantType:     "system",
			wantAwaiting: 1,
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.Equal(t, "awaiting human input", content)
			},
		},
		{
			name: "state_change other → system + awaiting=false",
			line: makeEvent("state_change", map[string]any{
				"stop":  "done",
				"turns": float64(5),
			}),
			wantType:     "system",
			wantAwaiting: 0,
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.NotEmpty(t, content)
			},
		},
		{
			name: "context_limit → system",
			line: makeEvent("context_limit", map[string]any{
				"prompt_tokens":  float64(80000),
				"context_window": float64(100000),
				"ratio":          float64(0.8),
				"threshold":      float64(0.85),
			}),
			wantType:     "system",
			wantAwaiting: 0,
		},
		{
			name: "error → stderr (awaiting flag untouched)",
			line: makeEvent("error", map[string]any{
				"error": "something went wrong",
			}),
			wantType: "stderr",
			// stderr-typed entries must NOT clear awaiting-human: a parked HITL
			// worker still logs errors while waiting for a human.
			wantAwaiting: -1,
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.Contains(t, content, "something went wrong")
			},
		},
		{
			name:         "model_request → skipped",
			line:         makeEvent("model_request", map[string]any{"turn": float64(1)}),
			wantType:     "",
			wantAwaiting: -1,
		},
		{
			name:         "tool_result → skipped",
			line:         makeEvent("tool_result", map[string]any{"id": "call_x"}),
			wantType:     "",
			wantAwaiting: -1,
		},
		{
			name:         "tool_repair → skipped",
			line:         makeEvent("tool_repair", map[string]any{"id": "call_x"}),
			wantType:     "",
			wantAwaiting: -1,
		},
		{
			name:         "user_input → skipped",
			line:         makeEvent("user_input", map[string]any{"message_id": "m1"}),
			wantType:     "",
			wantAwaiting: -1,
		},
		{
			name:         "verification → skipped",
			line:         makeEvent("verification", map[string]any{}),
			wantType:     "",
			wantAwaiting: -1,
		},
		{
			name:         "unknown kind → skipped",
			line:         makeEvent("future_kind", map[string]any{"x": "y"}),
			wantType:     "",
			wantAwaiting: -1,
		},
		{
			name:     "unparsable line → stderr passthrough (awaiting flag untouched)",
			line:     []byte("goroutine 1 [running]: panic: something bad happened"),
			wantType: "stderr",
			// stderr-typed: awaiting must not be cleared.
			wantAwaiting: -1,
			checkContent: func(t *testing.T, content string) {
				t.Helper()
				assert.Equal(t, "goroutine 1 [running]: panic: something bad happened", content)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hub := logbridge.NewHub()
			_, ch := hub.Subscribe("")

			awaitingCalled := false
			awaitingVal := false
			bridge := logbridge.New(hub, nil, func(project, cardID string, awaiting bool) {
				awaitingCalled = true
				awaitingVal = awaiting
			})

			bridge.BridgeLine(testProject, testCard, tt.line, false)

			if tt.wantType == "" {
				// Expect no publish.
				select {
				case e := <-ch:
					t.Errorf("expected skip but got entry with type=%q", e.Type)
				case <-time.After(30 * time.Millisecond):
				}

				if tt.wantAwaiting == -1 {
					assert.False(t, awaitingCalled, "awaiting hook must not fire for skipped lines")
				}

				return
			}

			var got protocol.LogEntry
			select {
			case got = <-ch:
			case <-time.After(100 * time.Millisecond):
				t.Fatal("expected entry but got none (timeout)")
			}

			assert.Equal(t, tt.wantType, got.Type, "Type mismatch")
			assert.Equal(t, testProject, got.Project, "Project mismatch")
			assert.Equal(t, testCard, got.CardID, "CardID mismatch")
			assert.False(t, got.Timestamp.IsZero(), "Timestamp must be set")

			if tt.wantModel != "" {
				assert.Equal(t, tt.wantModel, got.Model)
			}

			if tt.wantToolID != "" {
				assert.Equal(t, tt.wantToolID, got.ToolUseID)
			}

			if tt.wantUsage != nil {
				require.NotNil(t, got.Usage)
				assert.Equal(t, tt.wantUsage.InputTokens, got.Usage.InputTokens)
				assert.Equal(t, tt.wantUsage.OutputTokens, got.Usage.OutputTokens)
			}

			if tt.checkContent != nil {
				tt.checkContent(t, got.Content)
			}

			switch tt.wantAwaiting {
			case 1:
				assert.True(t, awaitingCalled, "awaiting hook must fire")
				assert.True(t, awaitingVal, "awaiting must be true")
			case 0:
				assert.True(t, awaitingCalled, "awaiting hook must fire with false for bridged lines")
				assert.False(t, awaitingVal, "awaiting must be false")
			case -1:
				assert.False(t, awaitingCalled,
					"awaiting hook must NOT fire for stderr-typed entries (keeps a parked HITL worker alive)")
			}
		})
	}
}

// TestStderrStream verifies that isStderr=true produces a stderr frame with
// the raw line redacted.
func TestStderrStream(t *testing.T) {
	t.Parallel()

	const secret = "supersecrettoken"

	hub := logbridge.NewHub()
	_, ch := hub.Subscribe("")
	red := redact.New([]string{secret})
	bridge := logbridge.New(hub, red, nil)

	rawLine := []byte("error: auth failed with " + secret)
	bridge.BridgeLine(testProject, testCard, rawLine, true)

	var got protocol.LogEntry
	select {
	case got = <-ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected entry but got none")
	}

	assert.Equal(t, "stderr", got.Type)
	assert.NotContains(t, got.Content, secret, "secret must be redacted")
	assert.Contains(t, got.Content, "[REDACTED]")
}

// TestStderrDoesNotClearAwaiting proves that stderr output — raw stderr, the
// error-kind mapping, and unparsable lines — leaves the awaiting-human flag
// untouched, while a real agent-progress entry clears it. A parked HITL worker
// keeps logging to stderr; clearing awaiting on those lines would let the idle
// watchdog reap a container that is legitimately waiting for a human.
func TestStderrDoesNotClearAwaiting(t *testing.T) {
	t.Parallel()

	newBridge := func() (*logbridge.Bridge, *bool, *bool) {
		called := false
		val := false
		hub := logbridge.NewHub()
		b := logbridge.New(hub, nil, func(_, _ string, awaiting bool) {
			called = true
			val = awaiting
		})

		return b, &called, &val
	}

	t.Run("raw stderr line does not fire the awaiting hook", func(t *testing.T) {
		t.Parallel()

		b, called, _ := newBridge()
		b.BridgeLine(testProject, testCard, []byte("worker slog: heartbeat warning"), true)

		assert.False(t, *called, "raw stderr must not clear awaiting")
	})

	t.Run("error-kind event does not fire the awaiting hook", func(t *testing.T) {
		t.Parallel()

		b, called, _ := newBridge()
		b.BridgeLine(testProject, testCard, makeEvent("error", map[string]any{"error": "boom"}), false)

		assert.False(t, *called, "error-kind stderr must not clear awaiting")
	})

	t.Run("text event fires the awaiting hook with false", func(t *testing.T) {
		t.Parallel()

		b, called, val := newBridge()
		b.BridgeLine(testProject, testCard,
			makeEvent("model_response", map[string]any{"content": "progress", "model": "m"}), false)

		assert.True(t, *called, "agent-progress entry must fire the awaiting hook")
		assert.False(t, *val, "agent-progress entry clears awaiting (false)")
	})

	t.Run("tool_call event fires the awaiting hook with false", func(t *testing.T) {
		t.Parallel()

		b, called, val := newBridge()
		b.BridgeLine(testProject, testCard,
			makeEvent("tool_call", map[string]any{"id": "c1", "name": "bash", "raw_args": "{}"}), false)

		assert.True(t, *called, "tool_call entry must fire the awaiting hook")
		assert.False(t, *val, "tool_call entry clears awaiting (false)")
	})
}

// TestRedaction ensures secrets never appear in bridged frames.
func TestRedaction(t *testing.T) {
	t.Parallel()

	const secret = "my-secret-api-key"

	hub := logbridge.NewHub()
	_, ch := hub.Subscribe("")
	red := redact.New([]string{secret})
	bridge := logbridge.New(hub, red, nil)

	t.Run("model_response content redacted", func(t *testing.T) {
		line := makeEvent("model_response", map[string]any{
			"content": "The key is " + secret + " and it works",
			"model":   "m",
		})
		bridge.BridgeLine(testProject, testCard, line, false)

		select {
		case got := <-ch:
			assert.NotContains(t, got.Content, secret)
			assert.Contains(t, got.Content, "[REDACTED]")
		case <-time.After(100 * time.Millisecond):
			t.Fatal("expected entry")
		}
	})

	t.Run("thinking content redacted", func(t *testing.T) {
		line := makeEvent("thinking", map[string]any{
			"content": "internal reasoning mentions " + secret,
		})
		bridge.BridgeLine(testProject, testCard, line, false)

		select {
		case got := <-ch:
			assert.Equal(t, "thinking", got.Type)
			assert.NotContains(t, got.Content, secret)
			assert.Contains(t, got.Content, "[REDACTED]")
		case <-time.After(100 * time.Millisecond):
			t.Fatal("expected entry")
		}
	})

	t.Run("raw stderr redacted", func(t *testing.T) {
		bridge.BridgeLine(testProject, testCard, []byte("fatal: "+secret), true)

		select {
		case got := <-ch:
			assert.NotContains(t, got.Content, secret)
			assert.Contains(t, got.Content, "[REDACTED]")
		case <-time.After(100 * time.Millisecond):
			t.Fatal("expected entry")
		}
	})
}

// TestHubSubscribers verifies fan-out, filtering, drop-on-full, Unsubscribe,
// and PublishUser.
func TestHubSubscribers(t *testing.T) {
	t.Parallel()

	t.Run("all-project subscriber receives entries from any project", func(t *testing.T) {
		t.Parallel()

		hub := logbridge.NewHub()
		_, ch := hub.Subscribe("") // empty = all
		hub.Publish(protocol.LogEntry{Type: "text", Project: "proj-a", Content: "hello"})

		select {
		case got := <-ch:
			assert.Equal(t, "proj-a", got.Project)
		case <-time.After(100 * time.Millisecond):
			t.Fatal("expected entry")
		}
	})

	t.Run("project-filtered subscriber only receives matching project", func(t *testing.T) {
		t.Parallel()

		hub := logbridge.NewHub()
		_, chA := hub.Subscribe("proj-a")
		_, chB := hub.Subscribe("proj-b")

		hub.Publish(protocol.LogEntry{Type: "text", Project: "proj-a", Content: "for a"})

		select {
		case got := <-chA:
			assert.Equal(t, "proj-a", got.Project)
		case <-time.After(100 * time.Millisecond):
			t.Fatal("proj-a subscriber did not receive")
		}

		// proj-b should not receive proj-a's entry.
		select {
		case e := <-chB:
			t.Errorf("proj-b received unexpected entry: %+v", e)
		case <-time.After(30 * time.Millisecond):
			// correct: no delivery
		}
	})

	t.Run("full subscriber drops without stalling", func(t *testing.T) {
		t.Parallel()

		hub := logbridge.NewHub()
		_, ch := hub.Subscribe("")

		// Publish more entries than the channel buffer without consuming.
		done := make(chan struct{})

		go func() {
			defer close(done)

			for i := range 300 {
				hub.Publish(protocol.LogEntry{Type: "text", Content: fmt.Sprintf("msg-%d", i)})
			}
		}()

		// The goroutine must complete without blocking.
		select {
		case <-done:
			// success
		case <-time.After(2 * time.Second):
			t.Fatal("Publish blocked on full subscriber")
		}

		_ = ch
	})

	t.Run("Unsubscribe closes channel", func(t *testing.T) {
		t.Parallel()

		hub := logbridge.NewHub()
		id, ch := hub.Subscribe("")
		hub.Unsubscribe(id)

		_, open := <-ch
		assert.False(t, open, "channel must be closed after Unsubscribe")
	})

	t.Run("PublishUser emits user-type entry not redacted", func(t *testing.T) {
		t.Parallel()

		const secret = "plain-secret-1234"

		hub := logbridge.NewHub()
		_, ch := hub.Subscribe("")

		hub.PublishUser(testProject, testCard, "user said: "+secret)

		select {
		case got := <-ch:
			assert.Equal(t, "user", got.Type)
			assert.Contains(t, got.Content, secret, "user content must NOT be redacted")
		case <-time.After(100 * time.Millisecond):
			t.Fatal("expected user entry")
		}
	})
}
