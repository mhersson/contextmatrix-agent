package worker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/frames"
	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
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

// calls reports how many responses have been served, under the same lock that
// guards the write in next() so -race stays clean if the read is ever reordered
// relative to Run's return.
func (s *scriptedLLM) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.i
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

func (f *fakeOps) ReportUsage(_ context.Context, cardID, model string, prompt, completion int64, actualCostUSD float64) error {
	f.record("ReportUsage", cardID, model, prompt, completion, actualCostUSD)

	return nil
}

func (f *fakeOps) ReportPush(_ context.Context, cardID, branch, prURL string) error {
	f.record("ReportPush", cardID, branch, prURL)

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

// --- Test 1: autonomous plumbing -> FSM -------------------------------------

// TestRunAutonomousPlumbing verifies the shared setup runs before the FSM for an
// autonomous card: clone + branch + claim + context, in order, then hand-off to
// the orchestrator. The FSM owns completion (done phase), so on a nil return the
// worker reports a graceful "completed" without calling CompleteTask itself.
func TestRunAutonomousPlumbing(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	var seenWorkspace string

	swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
		seenWorkspace = d.Cfg.Workspace

		return nil
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	// Claim before context, both before the FSM ran.
	order := ops.ops()
	require.GreaterOrEqual(t, len(order), 2)
	assert.Equal(t, "ClaimCard", order[0])
	assert.Equal(t, "GetTaskContext", order[1])

	// The branch was cut and the workspace clone exists, wired into the Deps.
	assert.Equal(t, filepath.Join(wsParent, "cmx-001"), seenWorkspace)

	// The worker does not complete the card on the FSM happy path — the done
	// phase does — and does not release a successful run.
	assert.Equal(t, 0, ops.count("CompleteTask"))
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

// --- Test 3: FSM generic error ----------------------------------------------

// TestRunFSMGenericError: a non-sentinel FSM error releases the claim and
// surfaces as a non-zero exit, without completing the card.
func TestRunFSMGenericError(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	swapRunOrchestrator(t, func(_ context.Context, _ orchestrator.Deps) error {
		return fmt.Errorf("model exploded")
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, &scriptedLLM{}, emit, openStdin(t))
	require.Error(t, err)
	require.ErrorContains(t, err, "model exploded")
	assert.Equal(t, "error", res.Reason)

	assert.Equal(t, 1, ops.count("ReleaseCard"))
	assert.Equal(t, 0, ops.count("CompleteTask"))
}

// --- Test 4: model fallback ------------------------------------------------

// TestRunModelFallback verifies model resolution (step 5, shared by both paths)
// emits the catalog-fallback warning when the requested model is absent. The FSM
// seam is stubbed to a no-op completion so the assertion isolates resolution.
func TestRunModelFallback(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	swapRunOrchestrator(t, func(context.Context, orchestrator.Deps) error { return nil })

	// A real llm.Client pointed at a canned catalog: it lists a different model
	// than spec.Model, forcing resolveModel's fallback path.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"default/model","context_length":128000,"pricing":{"prompt":"0","completion":"0"},"supported_parameters":["tools"]}]}`))

			return
		}

		http.NotFound(w, r)
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

	// A warning state-change event was emitted naming the requested model.
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

	// A slow FSM run: the seam blocks long enough for several heartbeat ticks to
	// fire, proving the heartbeat goroutine covers the whole FSM run.
	swapRunOrchestrator(t, func(context.Context, orchestrator.Deps) error {
		time.Sleep(60 * time.Millisecond)

		return nil
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	_, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err)

	assert.GreaterOrEqual(t, ops.count("Heartbeat"), 2, "expected at least two heartbeats during a slow run")
}

// --- Test 6: clean tree on FSM completion -----------------------------------

// TestRunCleanTree: the FSM completes with no working-tree changes (nil return,
// clean tree). The worker reports completed and does not push or complete —
// pushes and completion are the FSM's responsibility.
func TestRunCleanTree(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	swapRunOrchestrator(t, func(context.Context, orchestrator.Deps) error { return nil })

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	assert.Equal(t, 0, ops.count("ReportPush"), "worker does not push on the FSM happy path")
	assert.False(t, remoteHasBranch(t, remote, "cm/cmx-001"), "no branch pushed by the worker")
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

// --- FSM entry / promote bridge --------------------------------------------

// swapRunOrchestrator replaces the package-level runOrchestrator seam for the
// duration of the test and restores it on cleanup. fn observes the Deps the
// worker built and decides the FSM's outcome.
func swapRunOrchestrator(t *testing.T, fn func(context.Context, orchestrator.Deps) error) {
	t.Helper()

	prev := runOrchestrator
	runOrchestrator = fn

	t.Cleanup(func() { runOrchestrator = prev })
}

// TestAutonomousEntersOrchestrator: a non-interactive spec routes to the FSM
// seam and never drives the linear harness loop. Swaps the package-level
// runOrchestrator var, so it must not run in parallel.
func TestAutonomousEntersOrchestrator(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	var fsmRan atomic.Bool

	swapRunOrchestrator(t, func(_ context.Context, _ orchestrator.Deps) error {
		fsmRan.Store(true)

		return nil
	})

	// If the linear harness ran, this scripted call would be consumed.
	llmClient := &scriptedLLM{responses: []llm.Response{
		{Content: "linear path ran", FinishReason: "stop"},
	}}

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, llmClient, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	assert.True(t, fsmRan.Load(), "autonomous spec must enter the orchestrator")
	assert.Equal(t, 0, llmClient.calls(), "linear harness loop must not run for an autonomous card")
}

// TestHITLStaysLinear: an interactive spec with no promote uses the linear
// path only; the FSM seam is never invoked. Swaps runOrchestrator to detect any
// stray FSM entry, so it must not run in parallel.
func TestHITLStaysLinear(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	var fsmRan atomic.Bool

	swapRunOrchestrator(t, func(_ context.Context, _ orchestrator.Deps) error {
		fsmRan.Store(true)

		return nil
	})

	// The model parks awaiting a human turn; an end_session (not a promote) ends
	// the run via the linear finalize, so the FSM is never reached.
	llmClient := &scriptedLLM{responses: []llm.Response{
		{Content: "linear done", FinishReason: "stop"},
	}}

	spec := baseSpec(t, remote, wsParent)
	spec.Interactive = true

	pr, pw := io.Pipe()

	go func() {
		time.Sleep(50 * time.Millisecond)

		_ = frames.Write(pw, frames.Frame{Type: frames.TypeEndSession})
	}()

	t.Cleanup(func() { _ = pw.Close() })

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), spec, ops, llmClient, emit, pr)
	require.NoError(t, err)
	assert.Equal(t, "end_session", res.Reason)

	assert.False(t, fsmRan.Load(), "no promote: must stay on the linear path")
	assert.GreaterOrEqual(t, llmClient.calls(), 1, "linear harness loop must run")
}

// TestPromoteBridge: an interactive run that receives a promote frame mid-run
// hands off to the FSM after the linear harness returns (not via end_session).
func TestPromoteBridge(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	var fsmRan atomic.Bool

	swapRunOrchestrator(t, func(_ context.Context, _ orchestrator.Deps) error {
		fsmRan.Store(true)

		return nil
	})

	// The model parks awaiting a human turn; the promote frame closes the inbox
	// so the linear loop returns, then the worker bridges to the FSM.
	llmClient := &scriptedLLM{responses: []llm.Response{
		{Content: "awaiting guidance", FinishReason: "stop"},
	}}

	spec := baseSpec(t, remote, wsParent)
	spec.Interactive = true

	pr, pw := io.Pipe()

	go func() {
		time.Sleep(50 * time.Millisecond)

		_ = frames.Write(pw, frames.Frame{Type: frames.TypePromote})
	}()

	t.Cleanup(func() { _ = pw.Close() })

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), spec, ops, llmClient, emit, pr)
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	// The bridge calls runFSM synchronously before Run returns, so the seam has
	// definitively run by now — no polling.
	require.True(t, fsmRan.Load(), "promote bridge must enter the FSM")
}

// TestReviewParkedMapsToCompleted: a ReviewParkedError from the FSM is a
// graceful completion — exit-0 path, completed reason, no CompleteTask call.
func TestReviewParkedMapsToCompleted(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	swapRunOrchestrator(t, func(_ context.Context, _ orchestrator.Deps) error {
		return &orchestrator.ReviewParkedError{Findings: "outstanding nits"}
	})

	llmClient := &scriptedLLM{}

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, llmClient, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	assert.Equal(t, 0, ops.count("CompleteTask"), "review park must NOT complete the card")
	assert.Equal(t, 0, ops.count("ReleaseCard"), "review park leaves the card in review")
}

// TestBudgetMapsToFailed: a BudgetExceededError pushes WIP, releases the claim,
// and surfaces a non-nil error (serve maps the error to the failed callback).
func TestBudgetMapsToFailed(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
		// Dirty the tree so the WIP commit/push path has something to push.
		require.NoError(t, os.WriteFile(filepath.Join(d.Cfg.Workspace, "wip.txt"), []byte("partial\n"), 0o644))

		return &orchestrator.BudgetExceededError{Spent: 1.50, Max: 1.00}
	})

	llmClient := &scriptedLLM{}

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, llmClient, emit, openStdin(t))
	require.Error(t, err)
	assert.Equal(t, "error", res.Reason)

	assert.True(t, remoteHasBranch(t, remote, "cm/cmx-001"), "budget breach pushes WIP")
	assert.GreaterOrEqual(t, ops.count("ReportPush"), 1, "WIP push reported")
	assert.Equal(t, 1, ops.count("ReleaseCard"), "claim released on budget breach")
	assert.Equal(t, 0, ops.count("CompleteTask"))
	// Usage is reported per-phase by the orchestrator as it spends, and the
	// budget numbers are logged by its execute loop (see TestRunBudgetBreachParks);
	// the worker re-reports neither on the park path.
}

// TestEndSessionMidFSM: an end_session frame cancels the run context while the
// FSM is in a phase; the orchestrator returns ctx.Err() and the worker takes
// the C1 graceful path (push WIP, report usage, release, exit 0).
func TestEndSessionMidFSM(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	swapRunOrchestrator(t, func(ctx context.Context, d orchestrator.Deps) error {
		// Block until the end_session cancels the run context, then return its
		// error — exactly what the real FSM does when its ctx is canceled.
		require.NoError(t, os.WriteFile(filepath.Join(d.Cfg.Workspace, "wip.txt"), []byte("partial\n"), 0o644))
		<-ctx.Done()

		return ctx.Err()
	})

	llmClient := &scriptedLLM{}

	pr, pw := io.Pipe()

	go func() {
		time.Sleep(50 * time.Millisecond)

		_ = frames.Write(pw, frames.Frame{Type: frames.TypeEndSession})
	}()

	t.Cleanup(func() { _ = pw.Close() })

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, llmClient, emit, pr)
	require.NoError(t, err)
	assert.Equal(t, "end_session", res.Reason)

	assert.True(t, remoteHasBranch(t, remote, "cm/cmx-001"), "WIP pushed on end_session mid-FSM")
	assert.Equal(t, 1, ops.count("ReleaseCard"), "claim released on end_session")
	assert.Equal(t, 0, ops.count("CompleteTask"), "no completion on a parked session")
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
