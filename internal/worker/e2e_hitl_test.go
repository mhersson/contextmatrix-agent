package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/frames"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a mutex-guarded buffer the emitter writes to while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buf = append(b.buf, p...)

	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return string(b.buf)
}

// hitlBackend is a content-aware OpenRouter stub for the HITL flow: it inspects
// each request's first user message and returns the SSE body the relevant phase
// expects. Content matching (not call order) is required because review
// specialists run in parallel. The brainstorm turn is detected by whether the
// conversation block already contains a USER reply.
type hitlBackend struct {
	mu   sync.Mutex
	cost float64
}

func (b *hitlBackend) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)

			return
		}

		body, _ := io.ReadAll(r.Body)

		var req chatRequest

		_ = json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, b.reply(req))
	}
}

// reply selects the SSE body from the request's first user message, mirroring
// e2e_orchestrator_test.go's scriptedBackend (content match, not call order, so
// the parallel review specialists never race). hasToolResult distinguishes the
// coder's first (write) turn from its second (finish call); the brainstorm
// turn is distinguished by whether the threaded conversation already has a USER
// line. chatRequest, sseStop, sseCoderCommit, sseWriteTool, and writeArg are all
// defined in e2e_orchestrator_test.go (same package).
func (b *hitlBackend) reply(req chatRequest) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	firstUser := ""
	hasToolResult := false

	for _, m := range req.Messages {
		if m.Role == "user" && firstUser == "" {
			firstUser = m.Content
		}

		if m.Role == "tool" || m.ToolCallID != "" {
			hasToolResult = true
		}
	}

	switch {
	case strings.Contains(firstUser, "You are a design facilitator"):
		// Turn 1 asks a question; once the user has replied (a USER line appears
		// in the threaded conversation), emit the design + DESIGN_COMPLETE.
		if strings.Contains(firstUser, "USER:") {
			return sseStop("## Design\n\nA palette with N configurable slots.\n\nDESIGN_COMPLETE", b.cost)
		}

		return sseStop("Which layout do you want: grid or list?", b.cost)

	case strings.Contains(firstUser, "A human was shown a"):
		// Gate verdict classification: always approve in this scripted flow.
		return sseStop(`{"verdict":"approve","feedback":""}`, b.cost)

	case strings.Contains(firstUser, "You are the planning agent"):
		plan := `{"card_tier":"simple","subtasks":[` +
			`{"title":"Add palette config","description":"Files: palette.txt. Create palette.txt.","depends_on":[],"tier":"simple"}]}`

		return sseStop(plan, b.cost)

	case strings.Contains(firstUser, "You are the coding agent for one subtask"):
		// Turn 1 (no tool result yet) writes the file; turn 2 calls finish to end the run.
		// The message carries a scope ("ui") so it diverges from sanitizeTitle's
		// scopeless "feat: add palette config" fallback for the "Add palette
		// config" subtask title - see coderCommitFor's doc comment in
		// e2e_orchestrator_test.go for why that divergence matters.
		if hasToolResult {
			return sseCoderCommit("feat(ui): add palette config", b.cost)
		}

		return sseWriteTool("call_code", writeArg("palette.txt", "palette\n"), 0)

	case strings.Contains(firstUser, "You are the documentation agent"):
		return sseStop("No external documentation is needed.", b.cost)

	case strings.Contains(firstUser, "You are a code-review specialist"):
		return sseStop("No concerns.\nVerdict: looks good.", b.cost)

	case strings.Contains(firstUser, "You are the review synthesizer"):
		return sseStop(`{"approved":true,"summary":"clean","fixes":[]}`, b.cost)

	default:
		return sseStop("UNEXPECTED PROMPT", 0)
	}
}

// TestE2EHITLFullFlow drives a creative HITL card through the real FSM: a
// brainstorm dialogue (one question, one human reply, then DESIGN_COMPLETE), a
// plan-approval gate (approve), execution, document, a review-decision gate
// (approve), integrate, done. The stdin frame driver sends a human turn after
// each awaiting-human park.
func TestE2EHITLFullFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping HITL e2e in -short mode")
	}

	remote := setupBareRemote(t)
	backend := &hitlBackend{cost: 0.001}

	srv := httptest.NewServer(backend.handler())
	t.Cleanup(srv.Close)

	client := llm.NewClient("test-key", llm.WithBaseURL(srv.URL))

	// A creative, non-autonomous card: brainstorm runs (no ## Design yet), then
	// the plan-approval and review-decision gates. stubOps is reused from
	// e2e_orchestrator_test.go (it satisfies CardOps + orchestrator.Ops).
	ops := newStubOps()
	ops.tcx = cmclient.TaskContext{
		Title:       "Add a configurable palette",
		Description: "Let users configure the widget palette.",
		State:       "in_progress",
		Phase:       "",
		Autonomous:  false,
		CreatePR:    false,
	}

	spec := baseSpec(t, remote, t.TempDir())
	spec.Interactive = true // HITL
	spec.MaxCardCost = 0
	spec.MaxTurns = 6

	var transcript syncBuffer

	emit := events.NewEmitter(io.Discard, &transcript)

	pr, pw := io.Pipe()

	t.Cleanup(func() { _ = pw.Close() })

	// Frame driver: send a human turn after each awaiting-human park. Park 1 is
	// the brainstorm question; park 2 is the plan-approval gate; park 3 is the
	// review-decision gate.
	go func() {
		for n := 1; n <= 3; n++ {
			waitForAwaiting(t, &transcript, n)

			_ = frames.Write(pw, frames.Frame{
				Type: frames.TypeUserMessage, Content: "approve", MessageID: "m",
			})
		}
	}()

	type result struct {
		res Result
		err error
	}

	done := make(chan result, 1)

	go func() {
		res, err := Run(context.Background(), spec, ops, client, emit, pr)
		done <- result{res, err}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("HITL run did not complete; transcript so far:\n%s", transcript.String())
	}

	require.NoError(t, got.err)
	assert.Equal(t, "completed", got.res.Reason)

	// The gates and brainstorm parked at least three times and consumed human turns.
	assert.GreaterOrEqual(t, strings.Count(transcript.String(), `"state":"awaiting_human"`), 3,
		"brainstorm + plan-approval + review-decision each park")
	assert.GreaterOrEqual(t, strings.Count(transcript.String(), `"kind":"user_input"`), 3,
		"each park consumed a human turn")

	// The brainstorm produced and confirmed a design (its marked output streams to
	// the transcript via harness.Run's model_response emit), and the card reached
	// the integrated branch.
	tx := transcript.String()
	assert.Contains(t, tx, "DESIGN_COMPLETE", "brainstorm produced a confirmed design")
	assert.Contains(t, tx, "## Design", "the design section was presented")
	assert.True(t, remoteHasBranch(t, remote, "cm/cmx-001"), "the card branch was pushed")
	assert.Equal(t, "palette\n", branchFile(t, remote, "cm/cmx-001", "palette.txt"))
}

// waitForAwaiting blocks until the transcript shows at least n awaiting-human
// states, failing the test on timeout.
func waitForAwaiting(t *testing.T, transcript *syncBuffer, n int) {
	t.Helper()

	assert.Eventually(t, func() bool {
		return strings.Count(transcript.String(), `"state":"awaiting_human"`) >= n
	}, 15*time.Second, 10*time.Millisecond, "never reached awaiting-human park #%d", n)
}
