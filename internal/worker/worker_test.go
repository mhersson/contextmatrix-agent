package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/frames"
	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- scripted LLM (in-package; mirrors the harness fakeLLM) ----------------

// scriptedLLM returns canned responses in order; after they run out it returns
// an empty no-tool-call response (the loop treats that as a natural stop).
type scriptedLLM struct {
	mu        sync.Mutex
	responses []llm.Response
	err       error // when set, every call returns this error
	preDelay  time.Duration
	i         int
}

func (s *scriptedLLM) Send(ctx context.Context, _ llm.Request) (llm.Response, error) {
	return s.next(ctx)
}

func (s *scriptedLLM) SendStream(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return s.next(ctx)
}

func (s *scriptedLLM) next(ctx context.Context) (llm.Response, error) {
	if s.preDelay > 0 {
		select {
		case <-time.After(s.preDelay):
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
	}

	if s.err != nil {
		return llm.Response{}, s.err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.i >= len(s.responses) {
		return llm.Response{FinishReason: "stop"}, nil
	}

	r := s.responses[s.i]
	s.i++

	return r, nil
}

func toolCall(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: args}}
}

// --- fake CardOps recorder -------------------------------------------------

type opCall struct {
	op   string
	args []any
}

// fakeOps records every CardOps call in order under a mutex (the heartbeat
// goroutine calls concurrently). GetTaskContext returns a canned context.
type fakeOps struct {
	mu    sync.Mutex
	calls []opCall
	tcx   cmclient.TaskContext
}

func newFakeOps() *fakeOps {
	return &fakeOps{tcx: cmclient.TaskContext{
		CardID:      "CMX-001",
		Title:       "Add the widget",
		Description: "Implement the widget as described.",
		State:       "in_progress",
	}}
}

func (f *fakeOps) record(op string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, opCall{op: op, args: args})
}

func (f *fakeOps) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.op
	}

	return out
}

func (f *fakeOps) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0

	for _, c := range f.calls {
		if c.op == op {
			n++
		}
	}

	return n
}

func (f *fakeOps) find(op string) (opCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, c := range f.calls {
		if c.op == op {
			return c, true
		}
	}

	return opCall{}, false
}

func (f *fakeOps) ClaimCard(_ context.Context, cardID string) error {
	f.record("ClaimCard", cardID)

	return nil
}

func (f *fakeOps) GetTaskContext(_ context.Context, cardID string) (cmclient.TaskContext, error) {
	f.record("GetTaskContext", cardID)

	return f.tcx, nil
}

func (f *fakeOps) Heartbeat(_ context.Context, cardID string) error {
	f.record("Heartbeat", cardID)

	return nil
}

func (f *fakeOps) ReportUsage(_ context.Context, cardID, model string, prompt, completion int64) error {
	f.record("ReportUsage", cardID, model, prompt, completion)

	return nil
}

func (f *fakeOps) ReportPush(_ context.Context, cardID, branch string) error {
	f.record("ReportPush", cardID, branch)

	return nil
}

func (f *fakeOps) CompleteTask(_ context.Context, cardID, summary string) error {
	f.record("CompleteTask", cardID, summary)

	return nil
}

func (f *fakeOps) ReleaseCard(_ context.Context, cardID string) error {
	f.record("ReleaseCard", cardID)

	return nil
}

// --- helpers ---------------------------------------------------------------

// baseSpec returns a RunSpec wired for a local file:// remote with no tokens.
func baseSpec(t *testing.T, remote, workspaceParent string) RunSpec {
	t.Helper()

	return RunSpec{
		CardID:       "CMX-001",
		Project:      "demo",
		RepoURL:      remote,
		BaseBranch:   "main",
		DefaultModel: "default/model",
		Workspace:    workspaceParent,
		MaxTurns:     10,
	}
}

// remoteHasBranch reports whether the bare remote has the given branch.
func remoteHasBranch(t *testing.T, remote, branch string) bool {
	t.Helper()

	cmd := exec.Command("git", "branch", "--list", branch)
	cmd.Dir = remote
	cmd.Env = gitEnv()

	out, err := cmd.CombinedOutput()
	require.NoError(t, err)

	return strings.Contains(string(out), branch)
}

// --- Test 1: autonomous happy path -----------------------------------------

func TestRunAutonomousHappyPath(t *testing.T) {
	t.Parallel()

	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	// One write-tool call against the real registry, then a natural stop.
	llmClient := &scriptedLLM{responses: []llm.Response{
		{
			ToolCalls: []llm.ToolCall{toolCall("1", "write", `{"path":"feature.txt","content":"done\n"}`)},
			Usage:     llm.Usage{PromptTokens: 100, CompletionTokens: 40, Cost: 0.001},
		},
		{Content: "All done: added feature.txt", FinishReason: "stop", Usage: llm.Usage{PromptTokens: 50, CompletionTokens: 10}},
	}}

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, llmClient, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	// Call order: claim before context before push/usage/complete.
	order := ops.ops()
	assert.Equal(t, "ClaimCard", order[0])
	assert.Equal(t, "GetTaskContext", order[1])

	// Branch landed on the remote.
	assert.True(t, remoteHasBranch(t, remote, "cm/cmx-001"))

	// ReportPush carried the branch.
	push, ok := ops.find("ReportPush")
	require.True(t, ok, "ReportPush not called")
	assert.Equal(t, "cm/cmx-001", push.args[1])

	// ReportUsage carried the accumulated token totals.
	usage, ok := ops.find("ReportUsage")
	require.True(t, ok, "ReportUsage not called")
	assert.Equal(t, int64(150), usage.args[2])
	assert.Equal(t, int64(50), usage.args[3])

	// CompleteTask called with a summary from the model output.
	complete, ok := ops.find("CompleteTask")
	require.True(t, ok, "CompleteTask not called")
	assert.Equal(t, "All done: added feature.txt", complete.args[1])

	// No release on the happy path.
	assert.Equal(t, 0, ops.count("ReleaseCard"))
}

// --- Test 2: HITL + end_session --------------------------------------------

func TestRunHITLEndSession(t *testing.T) {
	t.Parallel()

	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	// Turn 1: no tool calls → the loop parks at awaiting-human (HITL).
	llmClient := &scriptedLLM{responses: []llm.Response{
		{Content: "Waiting for guidance", FinishReason: "stop"},
	}}

	spec := baseSpec(t, remote, wsParent)
	spec.Interactive = true

	pr, pw := io.Pipe()

	// Send an end_session frame shortly after the run parks.
	go func() {
		time.Sleep(50 * time.Millisecond)

		_ = frames.Write(pw, frames.Frame{Type: frames.TypeEndSession})
		_ = pw.Close()
	}()

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), spec, ops, llmClient, emit, pr)
	require.NoError(t, err)
	assert.Equal(t, "end_session", res.Reason)

	// Finalize without completing; the claim is released.
	assert.Equal(t, 0, ops.count("CompleteTask"))
	assert.Equal(t, 1, ops.count("ReleaseCard"))
}

// --- Test 3: harness error -------------------------------------------------

func TestRunHarnessError(t *testing.T) {
	t.Parallel()

	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	llmClient := &scriptedLLM{err: fmt.Errorf("model exploded")}

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, llmClient, emit, openStdin(t))
	require.Error(t, err)
	require.ErrorContains(t, err, "model exploded")
	assert.Equal(t, "error", res.Reason)

	assert.Equal(t, 1, ops.count("ReleaseCard"))
	assert.Equal(t, 0, ops.count("CompleteTask"))
}

// --- Test 4: model fallback ------------------------------------------------

func TestRunModelFallback(t *testing.T) {
	t.Parallel()

	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	// A real llm.Client pointed at a canned catalog + chat server. The catalog
	// lists a different model than spec.Model, forcing the fallback path. The
	// chat endpoint returns a trivial natural-stop so the harness completes.
	var gotModel string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"default/model","context_length":128000,"pricing":{"prompt":"0","completion":"0"},"supported_parameters":["tools"]}]}`))
		case "/chat/completions":
			var req struct {
				Model string `json:"model"`
			}

			_ = json.NewDecoder(r.Body).Decode(&req)
			gotModel = req.Model
			// The harness streams (SendStream): respond as SSE ending in [DONE].
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"model\":\"default/model\",\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\ndata: [DONE]\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := llm.NewClient("test-key", llm.WithBaseURL(srv.URL))

	spec := baseSpec(t, remote, wsParent)
	spec.Model = "missing/model" // not in the canned catalog → fallback expected

	var transcript syncBuffer

	emit := events.NewEmitter(io.Discard, &transcript)

	res, err := Run(context.Background(), spec, ops, client, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	// The harness ran with the default model, not the requested one.
	assert.Equal(t, "default/model", gotModel)

	// A warning state-change event was emitted.
	assert.Contains(t, transcript.String(), "model not in catalog, using default")
	assert.Contains(t, transcript.String(), "missing/model")
}

// --- Test 5: heartbeats ----------------------------------------------------

func TestRunHeartbeats(t *testing.T) {
	// Mutates package-level heartbeatInterval; cannot run in parallel.
	prev := heartbeatInterval
	heartbeatInterval = 10 * time.Millisecond

	defer func() { heartbeatInterval = prev }()

	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	// A slow run: each model call sleeps so several heartbeat ticks fire.
	llmClient := &scriptedLLM{
		preDelay:  60 * time.Millisecond,
		responses: []llm.Response{{Content: "done", FinishReason: "stop"}},
	}

	emit := events.NewEmitter(io.Discard, io.Discard)

	_, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, llmClient, emit, openStdin(t))
	require.NoError(t, err)

	assert.GreaterOrEqual(t, ops.count("Heartbeat"), 2, "expected at least two heartbeats during a slow run")
}

// --- Test 6: clean tree ----------------------------------------------------

func TestRunCleanTree(t *testing.T) {
	t.Parallel()

	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	// Model makes no edits and stops immediately → no commit, no push.
	llmClient := &scriptedLLM{responses: []llm.Response{
		{Content: "Nothing to change", FinishReason: "stop"},
	}}

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, llmClient, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	assert.Equal(t, 0, ops.count("ReportPush"), "no push on a clean tree")
	assert.False(t, remoteHasBranch(t, remote, "cm/cmx-001"), "no branch pushed on a clean tree")
	assert.Equal(t, 1, ops.count("CompleteTask"), "completion still reported on a clean tree")
}

// --- Test 7: autonomous + end_session mid-run --------------------------------

func TestRunAutonomousEndSessionMidRun(t *testing.T) {
	t.Parallel()

	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	// The model turn is slow; the end_session frame arrives mid-run and must
	// abort it: finalize WITHOUT CompleteTask, release the claim.
	llmClient := &scriptedLLM{
		preDelay:  500 * time.Millisecond,
		responses: []llm.Response{{Content: "would have finished", FinishReason: "stop"}},
	}

	pr, pw := io.Pipe()

	t.Cleanup(func() { _ = pw.Close() })

	go func() {
		time.Sleep(50 * time.Millisecond)

		_ = frames.Write(pw, frames.Frame{Type: frames.TypeEndSession})
	}()

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, llmClient, emit, pr)
	require.NoError(t, err)
	assert.Equal(t, "end_session", res.Reason)

	assert.Equal(t, 0, ops.count("CompleteTask"))
	assert.Equal(t, 1, ops.count("ReleaseCard"))
}

// --- shared test plumbing --------------------------------------------------

// openStdin yields a stdin held open for the test's duration, mirroring the
// production attach: the host service keeps the container's stdin open for
// its whole life, so EOF legitimately means "session over" in every mode.
// The write end closes in cleanup — after Run has returned — which also lets
// the pump goroutine exit.
func openStdin(t *testing.T) io.Reader {
	t.Helper()

	pr, pw := io.Pipe()

	t.Cleanup(func() { _ = pw.Close() })

	return pr
}

// syncBuffer is a mutex-guarded buffer the Emitter can write to concurrently
// with reads in the test (the heartbeat goroutine shares the emitter).
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

// --- summaryFrom -----------------------------------------------------------

func TestSummaryFrom_RuneSafeTruncation(t *testing.T) {
	t.Parallel()

	// "世" is 3 bytes; 67 of them is 201 bytes, so a naive 200-byte cut lands
	// mid-rune (200 is not a rune boundary). The backoff must keep it valid.
	out := strings.Repeat("世", 67)
	require.Greater(t, len(out), summaryMaxLen)

	got := summaryFrom(harness.Result{Output: out}, cmclient.TaskContext{Title: "fallback"})

	assert.LessOrEqual(t, len(got), summaryMaxLen, "summary stays within the byte cap")
	assert.True(t, utf8.ValidString(got), "truncated summary must be valid UTF-8")
}

func TestSummaryFrom_FirstLineAndFallback(t *testing.T) {
	t.Parallel()

	// First line only.
	got := summaryFrom(harness.Result{Output: "done the thing\nmore detail"},
		cmclient.TaskContext{Title: "title"})
	assert.Equal(t, "done the thing", got)

	// Empty output falls back to the card title.
	got = summaryFrom(harness.Result{Output: "   \n  "}, cmclient.TaskContext{Title: "card title"})
	assert.Equal(t, "card title", got)
}
