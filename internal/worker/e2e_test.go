package worker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/frames"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests compose the REAL worker stack — harness loop, real registry
// tools over a temp workspace, real frames/Inbox over an io.Pipe, real git
// against a local bare remote, and the REAL llm.Client SSE parser — with only
// OpenRouter stubbed. Unlike worker_test.go's scripted llm.LLM fake, the model
// responses here travel the full SSE wire format the client parses.

// --- stub OpenRouter SSE server --------------------------------------------

// stubOpenRouter serves /chat/completions, returning the next scripted SSE
// body per request. After the script is exhausted it returns a trivial
// natural-stop body so a stray extra turn never hangs the client.
type stubOpenRouter struct {
	mu     sync.Mutex
	bodies []string
	i      int
}

func (s *stubOpenRouter) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)

			return
		}

		s.mu.Lock()
		body := stopBody("")

		if s.i < len(s.bodies) {
			body = s.bodies[s.i]
			s.i++
		}
		s.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

// requests reports how many /chat/completions calls were served.
func (s *stubOpenRouter) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.i
}

// newStubOpenRouter starts an httptest server serving the scripted bodies and
// returns the stub plus its base URL. The server is closed in cleanup.
func newStubOpenRouter(t *testing.T, bodies ...string) (*stubOpenRouter, string) {
	t.Helper()

	stub := &stubOpenRouter{bodies: bodies}
	srv := httptest.NewServer(stub.handler())

	t.Cleanup(srv.Close)

	return stub, srv.URL
}

// --- SSE body builders (the exact wire format llm/sse.go parses) -----------

// writeToolBody is a turn that emits one `write` tool call (delivered as
// streamed deltas: id+name first, then the JSON arguments incrementally) plus
// a usage chunk and a tool_calls finish, terminated by [DONE].
func writeToolBody(callID, args string, prompt, completion int) string {
	// Split the arguments across two deltas to exercise the parser's
	// index-keyed accumulation (a real provider streams them in fragments).
	half := len(args) / 2

	var b strings.Builder

	b.WriteString(`data: {"model":"default/model","choices":[{"delta":{"tool_calls":[` +
		`{"index":0,"id":"` + callID + `","type":"function","function":{"name":"write","arguments":""}}]}}]}` + "\n")
	b.WriteString(`data: {"choices":[{"delta":{"tool_calls":[` +
		`{"index":0,"function":{"arguments":` + jsonString(args[:half]) + `}}]}}]}` + "\n")
	b.WriteString(`data: {"choices":[{"delta":{"tool_calls":[` +
		`{"index":0,"function":{"arguments":` + jsonString(args[half:]) + `}}]}}]}` + "\n")
	b.WriteString(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],` +
		usageJSON(prompt, completion) + "}\n")
	b.WriteString("data: [DONE]\n")

	return b.String()
}

// stopBody is a turn with a content message, a stop finish, and a usage chunk.
func stopBody(content string) string {
	return `data: {"model":"default/model","choices":[{"delta":{"content":` + jsonString(content) +
		`},"finish_reason":"stop"}],` + usageJSON(0, 0) + "}\n" +
		"data: [DONE]\n"
}

// stopBodyUsage is stopBody with explicit token counts.
func stopBodyUsage(content string, prompt, completion int) string {
	return `data: {"model":"default/model","choices":[{"delta":{"content":` + jsonString(content) +
		`},"finish_reason":"stop"}],` + usageJSON(prompt, completion) + "}\n" +
		"data: [DONE]\n"
}

func usageJSON(prompt, completion int) string {
	return `"usage":{"prompt_tokens":` + itoa(prompt) +
		`,"completion_tokens":` + itoa(completion) +
		`,"total_tokens":` + itoa(prompt+completion) + `}`
}

// jsonString quotes s as a JSON string literal (handles the content/args
// escaping the wire format requires).
func jsonString(s string) string {
	var b strings.Builder

	b.WriteByte('"')

	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}

	b.WriteByte('"')

	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	neg := n < 0
	if neg {
		n = -n
	}

	var buf [20]byte

	i := len(buf)

	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	if neg {
		i--
		buf[i] = '-'
	}

	return string(buf[i:])
}

// branchFile reads a file's content from a branch in the bare remote.
func branchFile(t *testing.T, remote, branch, path string) string {
	t.Helper()

	//nolint:gosec // G204: test-controlled branch/path, reading from a temp bare repo
	cmd := exec.Command("git", "show", branch+":"+path)
	cmd.Dir = remote
	cmd.Env = gitEnv()

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git show %s:%s: %s", branch, path, out)

	return string(out)
}

// --- Test: autonomous end-to-end via the real SSE client -------------------

func TestE2EAutonomousCompletes(t *testing.T) {
	t.Parallel()

	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	const (
		writePrompt, writeCompletion = 100, 40
		stopPrompt, stopCompletion   = 50, 10
		summary                      = "Added hello.txt as requested."
	)

	stub, stubURL := newStubOpenRouter(t,
		writeToolBody("call_1", `{"path":"hello.txt","content":"hello from the model\n"}`, writePrompt, writeCompletion),
		stopBodyUsage(summary, stopPrompt, stopCompletion),
	)

	client := llm.NewClient("test-key", llm.WithBaseURL(stubURL))

	var transcript syncBuffer

	emit := events.NewEmitter(io.Discard, &transcript)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, client, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	// Two turns served: the write turn and the stop turn.
	assert.Equal(t, 2, stub.requests())

	// Call order: claim, then context; heartbeats (>= 0) may appear after.
	order := ops.ops()
	require.GreaterOrEqual(t, len(order), 2)
	assert.Equal(t, "ClaimCard", order[0])
	assert.Equal(t, "GetTaskContext", order[1])

	// The real write tool landed hello.txt, committed and pushed to the remote.
	branch := "cm/cmx-001"
	assert.True(t, remoteHasBranch(t, remote, branch))
	assert.Equal(t, "hello from the model\n", branchFile(t, remote, branch, "hello.txt"))

	// ReportPush carried the branch.
	push, ok := ops.find("ReportPush")
	require.True(t, ok, "ReportPush not called")
	assert.Equal(t, branch, push.args[1])

	// ReportUsage carried the summed token totals from both scripted usage frames.
	usage, ok := ops.find("ReportUsage")
	require.True(t, ok, "ReportUsage not called")
	assert.Equal(t, int64(writePrompt+stopPrompt), usage.args[2])
	assert.Equal(t, int64(writeCompletion+stopCompletion), usage.args[3])

	// CompleteTask with a non-empty summary derived from the model output.
	complete, ok := ops.find("CompleteTask")
	require.True(t, ok, "CompleteTask not called")
	require.Len(t, complete.args, 2)
	assert.NotEmpty(t, complete.args[1])
	assert.Equal(t, summary, complete.args[1])

	// No release on the happy path.
	assert.Equal(t, 0, ops.count("ReleaseCard"))

	// The JSONL transcript records each event kind plus a terminal stop=done.
	tx := transcript.String()
	assert.Contains(t, tx, `"kind":"model_request"`)
	assert.Contains(t, tx, `"kind":"tool_call"`)
	assert.Contains(t, tx, `"kind":"usage"`)
	assert.Contains(t, tx, `"kind":"state_change"`)
	assert.Contains(t, tx, `"stop":"done"`)
}

// --- Test: HITL round-trip then end_session releases -----------------------

func TestE2EHITLEndSessionReleases(t *testing.T) {
	t.Parallel()

	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	// Both turns are natural stops (no tool calls). In interactive mode each
	// parks the loop at awaiting_human; the test drives it forward with frames.
	stub, stubURL := newStubOpenRouter(t,
		stopBody("How can I help?"),
		stopBody("Acknowledged."),
	)

	client := llm.NewClient("test-key", llm.WithBaseURL(stubURL))

	spec := baseSpec(t, remote, wsParent)
	spec.Interactive = true

	pr, pw := io.Pipe()

	t.Cleanup(func() { _ = pw.Close() })

	var transcript syncBuffer

	emit := events.NewEmitter(io.Discard, &transcript)

	type result struct {
		res Result
		err error
	}

	done := make(chan result, 1)

	go func() {
		res, err := Run(context.Background(), spec, ops, client, emit, pr)
		done <- result{res, err}
	}()

	// Turn 1 parks at awaiting_human; send a user message to trigger turn 2.
	require.Eventually(t, func() bool {
		return strings.Contains(transcript.String(), `"state":"awaiting_human"`)
	}, 5*time.Second, 10*time.Millisecond, "run never parked at awaiting_human")

	require.NoError(t, frames.Write(pw, frames.Frame{
		Type:      frames.TypeUserMessage,
		Content:   "Please proceed.",
		MessageID: "m1",
	}))

	// The user message must be consumed (a user_input event) and turn 2 must
	// park again at awaiting_human before we end the session.
	require.Eventually(t, func() bool {
		tx := transcript.String()

		return strings.Contains(tx, `"kind":"user_input"`) &&
			strings.Count(tx, `"state":"awaiting_human"`) >= 2
	}, 5*time.Second, 10*time.Millisecond, "user message not processed into a second park")

	require.NoError(t, frames.Write(pw, frames.Frame{Type: frames.TypeEndSession}))

	// The run must exit promptly once end_session cancels the parked Wait.
	var got result

	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit after end_session")
	}

	require.NoError(t, got.err)
	assert.Equal(t, "end_session", got.res.Reason)

	// Exactly two turns served: the initial park and the post-user-message
	// turn. end_session arrives while parked, so the script-exhaustion
	// fallback body must never be reached.
	assert.Equal(t, 2, stub.requests())

	// end_session finalizes without completing; the claim is released.
	assert.Equal(t, 0, ops.count("CompleteTask"))
	assert.Equal(t, 1, ops.count("ReleaseCard"))

	// The human turn surfaced as a user_input event in the transcript.
	assert.Contains(t, transcript.String(), `"kind":"user_input"`)
}
