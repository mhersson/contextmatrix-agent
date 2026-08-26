package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-backendkit/frames"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"
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
	mu                       sync.Mutex
	calls                    []opCall
	tcx                      cmclient.TaskContext
	lastGetTaskContextImages bool             // captured from GetTaskContext's includeImages arg
	transitionCardErr        map[string]error // keyed by target state; returned by TransitionCard when set
}

func newFakeOps() *fakeOps {
	return &fakeOps{tcx: cmclient.TaskContext{
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

// argsOf returns the args of the first recorded call matching op, or nil if
// op was never called.
func (f *fakeOps) argsOf(op string) []any {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, c := range f.calls {
		if c.op == op {
			return c.args
		}
	}

	return nil
}

// allArgsOf returns the args of every recorded call matching op, in order.
func (f *fakeOps) allArgsOf(op string) [][]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out [][]any

	for _, c := range f.calls {
		if c.op == op {
			out = append(out, c.args)
		}
	}

	return out
}

// state returns the fake's current card state.
func (f *fakeOps) state() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.tcx.State
}

func (f *fakeOps) ClaimCard(_ context.Context, cardID string) error {
	f.record("ClaimCard", cardID)

	// CM auto-transitions todo to in_progress on claim.
	f.mu.Lock()
	if f.tcx.State == "todo" {
		f.tcx.State = "in_progress"
	}
	f.mu.Unlock()

	return nil
}

func (f *fakeOps) GetTaskContext(_ context.Context, cardID string, includeImages bool) (cmclient.TaskContext, error) {
	f.record("GetTaskContext", cardID)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastGetTaskContextImages = includeImages

	return f.tcx, nil
}

func (f *fakeOps) Heartbeat(_ context.Context, cardID string) error {
	f.record("Heartbeat", cardID)

	return nil
}

func (f *fakeOps) ReportUsage(_ context.Context, cardID string, u cmclient.UsageReport) (float64, error) {
	f.record("ReportUsage", cardID, u.Model, u.PromptTokens, u.CompletionTokens, u.ActualCostUSD)

	return 0, nil
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

func (f *fakeOps) TransitionCard(_ context.Context, cardID, state string) error {
	f.record("TransitionCard", cardID, state)

	if f.transitionCardErr != nil {
		if err, ok := f.transitionCardErr[state]; ok {
			return err
		}
	}

	// A successful transition updates the canned state.
	f.mu.Lock()
	f.tcx.State = state
	f.mu.Unlock()

	return nil
}

func (f *fakeOps) RecordSkillEngaged(_ context.Context, cardID, skillName string) error {
	f.record("RecordSkillEngaged", cardID, skillName)

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
		// A trivial always-pass declared command makes the verify gate deterministic
		// in the full-FSM tests: resolution stops at the declared tier, so no model
		// proposal fires and the scripted response sequences stay exact.
		Verify: &protocol.VerifyConfig{Command: "true"},
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

// TestRunStalledCardRecoversDirectInProgress: a run whose GetTaskContext returns
// State=="stalled" recovers via TransitionCard(cardID,"in_progress"), then
// re-fetches context and proceeds into the FSM with state=="in_progress".
func TestRunStalledCardRecoversDirectInProgress(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()
	ops.tcx.State = "stalled"

	swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
		_ = d

		return nil
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	// Call order: ClaimCard, GetTaskContext (sees stalled), TransitionCard("in_progress"),
	// GetTaskContext (re-fetch), then the usual FSM path (no extra calls).
	calls := ops.ops()
	require.GreaterOrEqual(t, len(calls), 4)
	assert.Equal(t, "ClaimCard", calls[0])
	assert.Equal(t, "GetTaskContext", calls[1])
	assert.Equal(t, "TransitionCard", calls[2])
	assert.Equal(t, "GetTaskContext", calls[3])

	// Confirm that only one TransitionCard was called, targeting "in_progress".
	assert.Equal(t, [][]any{{"CMX-001", "in_progress"}}, ops.allArgsOf("TransitionCard"))
	// Verify the second GetTaskContext was called (the re-fetch).
	assert.Equal(t, 2, ops.count("GetTaskContext"))
	// Behavior anchor: the card ended in_progress.
	assert.Equal(t, "in_progress", ops.state())
}

// TestRunStalledCardFallbackOnRejectedInProgress: direct TransitionCard to
// "in_progress" is rejected (error contains "cannot transition from"), so
// the fallback transitions to "todo" and re-claims, then re-fetches context.
func TestRunStalledCardFallbackOnRejectedInProgress(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()
	ops.tcx.State = "stalled"
	// Exact wire text for a board with transitions stalled: [todo] - pins
	// isTransitionRejected against the real %q-quoted server format.
	ops.transitionCardErr = map[string]error{
		"in_progress": fmt.Errorf(`call transition_card: transition card CMX-001 to in_progress: validate transition: cannot transition from "stalled" to "in_progress"; valid targets: [todo]`),
	}

	swapRunOrchestrator(t, func(_ context.Context, _ orchestrator.Deps) error {
		return nil
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	// Call order: ClaimCard, GetTaskContext, TransitionCard("in_progress") [fails],
	// TransitionCard("todo"), ClaimCard, GetTaskContext.
	calls := ops.ops()
	require.GreaterOrEqual(t, len(calls), 6)
	assert.Equal(t, "ClaimCard", calls[0])
	assert.Equal(t, "GetTaskContext", calls[1])
	assert.Equal(t, "TransitionCard", calls[2])
	assert.Equal(t, "TransitionCard", calls[3])
	assert.Equal(t, "ClaimCard", calls[4])
	assert.Equal(t, "GetTaskContext", calls[5])
	// No trailing transition needed: claim_card auto-transitions todo to in_progress.
	assert.Equal(t, [][]any{{"CMX-001", "in_progress"}, {"CMX-001", "todo"}}, ops.allArgsOf("TransitionCard"))
	assert.Equal(t, 2, ops.count("ClaimCard"))
	assert.Equal(t, 2, ops.count("GetTaskContext"))
	// Behavior anchor: the fallback ended with the card in_progress.
	assert.Equal(t, "in_progress", ops.state())
}

// TestRunStalledCardBothTransitionsFailDegrades: both the direct in_progress
// transition AND the todo+reclaim fallback fail. The run logs a warning and
// continues - it does NOT return an error for the recovery step itself. The
// FSM runs with whatever tcx.State was last fetched (still "stalled").
func TestRunStalledCardBothTransitionsFailDegrades(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()
	ops.tcx.State = "stalled"
	// Both transitions rejected, exact wire text for a board with
	// transitions stalled: [] (legal config - the key only has to exist).
	ops.transitionCardErr = map[string]error{
		"in_progress": fmt.Errorf(`call transition_card: transition card CMX-001 to in_progress: validate transition: cannot transition from "stalled" to "in_progress"; valid targets: []`),
		"todo":        fmt.Errorf(`call transition_card: transition card CMX-001 to todo: validate transition: cannot transition from "stalled" to "todo"; valid targets: []`),
	}

	var fsmRan bool

	swapRunOrchestrator(t, func(_ context.Context, _ orchestrator.Deps) error {
		fsmRan = true

		return nil
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err, "both transitions failing must not cause Run to return an error")
	assert.Equal(t, "completed", res.Reason)
	assert.True(t, fsmRan, "FSM must still run even when both recovery transitions fail")

	// Both transitions were attempted, plus a re-fetch.
	calls := ops.ops()
	require.GreaterOrEqual(t, len(calls), 5)
	assert.Equal(t, "ClaimCard", calls[0])
	assert.Equal(t, "GetTaskContext", calls[1])
	assert.Equal(t, "TransitionCard", calls[2]) // in_progress
	assert.Equal(t, "TransitionCard", calls[3]) // todo
	assert.Equal(t, "GetTaskContext", calls[4]) // re-fetch (fails or returns stalled)
	assert.Equal(t, [][]any{{"CMX-001", "in_progress"}, {"CMX-001", "todo"}}, ops.allArgsOf("TransitionCard"))
	assert.Equal(t, 2, ops.count("GetTaskContext"))
	// Behavior anchor: the card is still stalled - the degrade path changed nothing.
	assert.Equal(t, "stalled", ops.state())
}

// TestRunStalledCardTransientErrorSkipsFallback: the direct in_progress
// transition fails with a non-rejection (transport-shaped) error. Recovery
// must NOT run the todo+reclaim fallback - blindly retrying through todo on a
// transient failure could double-transition the card - and the run still
// proceeds into the FSM after the verify re-fetch reports the card is still
// stalled.
func TestRunStalledCardTransientErrorSkipsFallback(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()
	ops.tcx.State = "stalled"
	// No "cannot transition from" needle: classified as non-rejection.
	ops.transitionCardErr = map[string]error{
		"in_progress": fmt.Errorf("call transition_card: connection refused"),
	}

	var fsmRan bool

	swapRunOrchestrator(t, func(_ context.Context, _ orchestrator.Deps) error {
		fsmRan = true

		return nil
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err, "a transient recovery failure must not cause Run to return an error")
	assert.Equal(t, "completed", res.Reason)
	assert.True(t, fsmRan, "FSM must still run after a transient recovery failure")

	// Call order: ClaimCard, GetTaskContext, TransitionCard("in_progress")
	// [transient failure], GetTaskContext (verify re-fetch). No fallback: no
	// todo transition, no re-claim.
	calls := ops.ops()
	require.GreaterOrEqual(t, len(calls), 4)
	assert.Equal(t, "ClaimCard", calls[0])
	assert.Equal(t, "GetTaskContext", calls[1])
	assert.Equal(t, "TransitionCard", calls[2])
	assert.Equal(t, "GetTaskContext", calls[3])
	assert.Equal(t, [][]any{{"CMX-001", "in_progress"}}, ops.allArgsOf("TransitionCard"),
		"transient error must not trigger the todo fallback")
	assert.Equal(t, 1, ops.count("ClaimCard"), "transient error must not trigger a re-claim")
	assert.Equal(t, 2, ops.count("GetTaskContext"))
	// Behavior anchor: the card is still stalled.
	assert.Equal(t, "stalled", ops.state())
}

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

	// The worker does not complete the card on the FSM happy path - the done
	// phase does - and does not release a successful run.
	assert.Equal(t, 0, ops.count("CompleteTask"))
	assert.Equal(t, 0, ops.count("ReleaseCard"))
}

// TestRunWorkerGetTaskContextNoImages verifies the worker bootstrap always
// requests GetTaskContext with includeImages=false. The worker reads only scalar
// fields (Autonomous, Title) and never uses images; requesting them here would
// waste bytes on a run-gating call.
func TestRunWorkerGetTaskContextNoImages(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	swapRunOrchestrator(t, func(_ context.Context, _ orchestrator.Deps) error {
		return nil
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	_, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err)

	// GetTaskContext must have been called with includeImages=false.
	ops.mu.Lock()
	got := ops.lastGetTaskContextImages
	ops.mu.Unlock()

	assert.False(t, got, "worker bootstrap must call GetTaskContext with includeImages=false")
}

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

// TestRunCleanTree: the FSM completes with no working-tree changes (nil return,
// clean tree). The worker reports completed and does not push or complete -
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
// The write end closes in cleanup - after Run has returned - which also lets
// the pump goroutine exit.
func openStdin(t *testing.T) io.Reader {
	t.Helper()

	pr, pw := io.Pipe()

	t.Cleanup(func() { _ = pw.Close() })

	return pr
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

// A card the server marks autonomous must enter the FSM even if a stale/forced
// interactive flag arrives - the agent self-corrects on tcx.Autonomous.
func TestAutonomousFlagOverridesInteractive(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()
	ops.tcx.Autonomous = true

	entered := false

	swapRunOrchestrator(t, func(_ context.Context, _ orchestrator.Deps) error {
		entered = true

		return nil
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	spec := baseSpec(t, remote, wsParent)
	spec.Interactive = true // forced/stale; the autonomous flag must win

	_, err := Run(context.Background(), spec, ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err)
	assert.True(t, entered, "autonomous card must enter the orchestrator FSM despite Interactive=true")
}

// An empty spec base branch must be resolved (to the clone's default) before
// the FSM runs, or the review diff / integrate rebase get `git merge-base ""
// HEAD` and fail.
func TestRunResolvesEmptyBaseBranchForFSM(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	var seenBase string

	swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
		seenBase = d.Cfg.BaseBranch

		return nil
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	spec := baseSpec(t, remote, wsParent)
	spec.BaseBranch = "" // card has no base branch set

	_, err := Run(context.Background(), spec, ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "main", seenBase, "FSM must receive the resolved base branch, not empty")
}

// toolNames extracts the registered tool names from a registry's schemas, for
// comparing two registries' composition (Registry has no public Names()).
func toolNames(t *testing.T, r *tools.Registry) []string {
	t.Helper()

	schemas := r.Schemas()
	names := make([]string, len(schemas))

	for i, s := range schemas {
		names[i] = s.Function.Name
	}

	return names
}

// TestRunFSMWiresPerDirFactories pins the Best-of-N per-candidate seam: Deps.
// GitForDir must hand back a git handle that structurally cannot push (no
// branch policy was set on it), and Deps.WriteToolsForDir must build the same
// tool composition as the main Deps.WriteTools registry when pointed at the
// same directory - proving writeToolsFor is the one source of truth behind
// both the main call site and the per-candidate factory.
func TestRunFSMWiresPerDirFactories(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	var gitForDir func(string) orchestrator.GitOps

	var writeToolsForDir func(string) *tools.Registry

	var mainWriteTools *tools.Registry

	swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
		gitForDir = d.GitForDir
		writeToolsForDir = d.WriteToolsForDir
		mainWriteTools = d.WriteTools

		return nil
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	spec := baseSpec(t, remote, wsParent)

	res, err := Run(context.Background(), spec, ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	require.NotNil(t, gitForDir, "Deps.GitForDir must be wired")
	require.NotNil(t, writeToolsForDir, "Deps.WriteToolsForDir must be wired")
	require.NotNil(t, mainWriteTools)

	ws := filepath.Join(wsParent, "cmx-001")

	// A candidate handle rooted at a worktree dir has no branch policy set on
	// it: it structurally cannot push, matching the Deps.GitForDir contract.
	candidateGit := gitForDir(filepath.Join(ws, ".worktrees", "c1"))
	require.NotNil(t, candidateGit)

	pushErr := candidateGit.Push(context.Background(), "cm/cmx-001-c1")
	require.Error(t, pushErr)
	assert.Contains(t, pushErr.Error(), "branch policy not set")

	// WriteToolsForDir(ws) must build the identical toolset as the main
	// WriteTools registry built for the same ws.
	forDirRegistry := writeToolsForDir(ws)
	require.NotNil(t, forDirRegistry)
	assert.ElementsMatch(t, toolNames(t, mainWriteTools), toolNames(t, forDirRegistry))
}

// TestRunFSMWiresSkillToolIntoCandidateRegistries: when a skills dir is
// mounted, WriteToolsForDir must include the same Skill tool the main
// WriteTools registry gets - Best-of-N candidates race with full tool parity
// instead of a skill-less write set. Swaps runOrchestrator, so it must not
// run in parallel.
func TestRunFSMWiresSkillToolIntoCandidateRegistries(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	skillsDir := filepath.Join(t.TempDir(), "skills")
	require.NoError(t, os.MkdirAll(filepath.Join(skillsDir, "go-development"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillsDir, "go-development", "SKILL.md"),
		[]byte("---\nname: go-development\ndescription: Use for Go.\n---\nbody"), 0o644))

	var writeToolsForDir func(string) *tools.Registry

	var mainWriteTools *tools.Registry

	swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
		writeToolsForDir = d.WriteToolsForDir
		mainWriteTools = d.WriteTools

		return nil
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	spec := baseSpec(t, remote, wsParent)
	spec.TaskSkillsDir = skillsDir

	res, err := Run(context.Background(), spec, ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	require.NotNil(t, writeToolsForDir, "Deps.WriteToolsForDir must be wired")
	require.NotNil(t, mainWriteTools)

	ws := filepath.Join(wsParent, "cmx-001")
	forDirRegistry := writeToolsForDir(filepath.Join(ws, ".worktrees", "c1"))
	require.NotNil(t, forDirRegistry)

	require.Contains(t, toolNames(t, mainWriteTools), "skill",
		"main registry must carry the skill tool when a skills dir is mounted")
	assert.Contains(t, toolNames(t, forDirRegistry), "skill",
		"candidate registries must carry the same skill tool as the main solver")
	assert.ElementsMatch(t, toolNames(t, mainWriteTools), toolNames(t, forDirRegistry))
}

// TestHITLEntersOrchestrator: an interactive, non-autonomous card routes to the
// FSM with HITL mode set and the live inbox injected. Swaps runOrchestrator to
// capture the Deps the worker built, so it must not run in parallel.
func TestHITLEntersOrchestrator(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	var (
		gotInteractive bool
		gotHuman       bool
	)

	swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
		gotInteractive = d.Cfg.Interactive
		gotHuman = d.Human != nil

		return nil
	})

	spec := baseSpec(t, remote, wsParent)
	spec.Interactive = true // HITL: interactive and (default) non-autonomous

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), spec, ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	assert.True(t, gotInteractive, "HITL card must set Cfg.Interactive")
	assert.True(t, gotHuman, "HITL card must inject the live inbox as Deps.Human")
}

// TestReviewParkedMapsToCompleted: a ReviewParkedError from the FSM is a
// graceful completion - exit-0 path, completed reason, no CompleteTask call.
func TestReviewParkedMapsToCompleted(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	swapRunOrchestrator(t, func(_ context.Context, _ orchestrator.Deps) error {
		return &orchestrator.ReviewParkedError{}
	})

	llmClient := &scriptedLLM{}

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, llmClient, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	assert.Equal(t, 0, ops.count("CompleteTask"), "review park must NOT complete the card")
	assert.Equal(t, 0, ops.count("ReleaseCard"), "review park leaves the card in review")
}

// TestMapFSMResult_GatesParked: a GatesParkedError is the same graceful shape as
// a review park - exit-0, completed reason, card left in review for a human, and
// no release (the pr_gates park note is already on the card).
func TestMapFSMResult_GatesParked(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	swapRunOrchestrator(t, func(_ context.Context, _ orchestrator.Deps) error {
		return &orchestrator.GatesParkedError{Reason: "PR creation failed - nothing to gate on"}
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	assert.Equal(t, 0, ops.count("CompleteTask"), "a gates park must NOT complete the card")
	assert.Equal(t, 0, ops.count("ReleaseCard"), "a gates park leaves the card in review")
}

// TestFSMDepsCarryGateWiring: the worker hands the FSM the gh gates seam and the
// container deadline the pr_gates phase waits against. The deadline is the
// container's own kill ceiling minus a finalize margin, and it stays zero
// (unbounded) when serve did not tell the worker its timeout.
func TestFSMDepsCarryGateWiring(t *testing.T) {
	t.Run("container timeout set: deadline is the kill ceiling minus the finalize margin", func(t *testing.T) {
		remote := setupBareRemote(t)
		wsParent := t.TempDir()
		ops := newFakeOps()

		var got orchestrator.Deps

		swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
			got = d

			return nil
		})

		spec := baseSpec(t, remote, wsParent)
		spec.ContainerTimeout = 90 * time.Minute
		spec.GatesPollInterval = 15 * time.Second
		spec.GatesCIWaitTimeout = 20 * time.Minute
		spec.GatesCopilotWaitTimeout = 5 * time.Minute

		start := time.Now()

		emit := events.NewEmitter(io.Discard, io.Discard)

		_, err := Run(context.Background(), spec, ops, &scriptedLLM{}, emit, openStdin(t))
		require.NoError(t, err)

		require.NotNil(t, got.PRGates, "the pr_gates phase needs the gh seam")
		assert.WithinDuration(t, start.Add(80*time.Minute), got.Cfg.Deadline, time.Minute,
			"the gates deadline leaves a 10m finalize margin before the container is killed")
		assert.Equal(t, 15*time.Second, got.Cfg.GatesPollInterval)
		assert.Equal(t, 20*time.Minute, got.Cfg.GatesCIWaitTimeout)
		assert.Equal(t, 5*time.Minute, got.Cfg.GatesCopilotWaitTimeout)
	})

	t.Run("container timeout unknown: no deadline", func(t *testing.T) {
		remote := setupBareRemote(t)
		wsParent := t.TempDir()
		ops := newFakeOps()

		var got orchestrator.Deps

		swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
			got = d

			return nil
		})

		spec := baseSpec(t, remote, wsParent) // ContainerTimeout stays 0

		emit := events.NewEmitter(io.Discard, io.Discard)

		_, err := Run(context.Background(), spec, ops, &scriptedLLM{}, emit, openStdin(t))
		require.NoError(t, err)

		assert.True(t, got.Cfg.Deadline.IsZero(), "an unknown container timeout leaves the gates unbounded")
	})
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

// TestContextLimitMapsToFailed: a ContextLimitError (raw or wrapped) pushes WIP,
// releases the claim, and surfaces a non-nil error - the budget-park shape - so
// in-flight work survives a context-window stop. The wrapped case proves
// errors.As traverses the wrap so a phase that wraps the sentinel still maps.
func TestContextLimitMapsToFailed(t *testing.T) {
	tests := []struct {
		name string
		err  func() error
	}{
		{
			name: "raw sentinel",
			err: func() error {
				return &orchestrator.ContextLimitError{Model: "m", ContextWindow: 1000}
			},
		},
		{
			name: "wrapped sentinel",
			err: func() error {
				return fmt.Errorf("coder run for SUB-1: %w",
					&orchestrator.ContextLimitError{Model: "m", ContextWindow: 1000})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remote := setupBareRemote(t)
			wsParent := t.TempDir()
			ops := newFakeOps()

			retErr := tt.err()

			swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
				// Dirty the tree so the WIP commit/push path has something to push.
				require.NoError(t, os.WriteFile(filepath.Join(d.Cfg.Workspace, "wip.txt"), []byte("partial\n"), 0o644))

				return retErr
			})

			llmClient := &scriptedLLM{}

			emit := events.NewEmitter(io.Discard, io.Discard)

			res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, llmClient, emit, openStdin(t))
			require.Error(t, err)
			assert.Equal(t, "error", res.Reason)

			assert.True(t, remoteHasBranch(t, remote, "cm/cmx-001"), "context-window park pushes WIP")
			assert.GreaterOrEqual(t, ops.count("ReportPush"), 1, "WIP push reported")
			assert.Equal(t, 1, ops.count("ReleaseCard"), "claim released on context-window park")
			assert.Equal(t, 0, ops.count("CompleteTask"))
		})
	}
}

// TestMaxTurnsMapsToFailed: a MaxTurnsError (wrapped, as the coder path wraps
// it) pushes WIP, releases the claim, and surfaces a non-nil error - the
// context-limit park shape - so truncated work survives for resume but is
// never completed.
func TestMaxTurnsMapsToFailed(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
		// Dirty the tree so the WIP commit/push path has something to push.
		require.NoError(t, os.WriteFile(filepath.Join(d.Cfg.Workspace, "wip.txt"), []byte("partial\n"), 0o644))

		return fmt.Errorf("coder run for SUB-1: %w", &orchestrator.MaxTurnsError{Model: "m", Turns: 30})
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, &scriptedLLM{}, emit, openStdin(t))
	require.Error(t, err)
	assert.Equal(t, "error", res.Reason)

	assert.True(t, remoteHasBranch(t, remote, "cm/cmx-001"), "turn-cap park pushes WIP")
	assert.GreaterOrEqual(t, ops.count("ReportPush"), 1, "WIP push reported")
	assert.Equal(t, 1, ops.count("ReleaseCard"), "claim released on turn-cap park")
	assert.Equal(t, 0, ops.count("CompleteTask"))
}

// TestToolchainMissingMapsToBlocked: a ToolchainMissingError transitions the
// card to blocked BEFORE releasing the claim, pushes WIP, and surfaces a
// non-nil error - so a human sees the environmental park on the board, not
// just a released claim. No model-outcome/blacklist call is made: the arm
// touches only TransitionCard, the WIP push, and the release, exactly like
// the Budget/Context/MaxTurns arms it mirrors.
func TestToolchainMissingMapsToBlocked(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
		// Dirty the tree so the WIP commit/push path has something to push.
		require.NoError(t, os.WriteFile(filepath.Join(d.Cfg.Workspace, "wip.txt"), []byte("partial\n"), 0o644))

		return &orchestrator.ToolchainMissingError{Tier: "detected", Subject: "pom.xml", Reason: "java not on PATH"}
	})

	llmClient := &scriptedLLM{}

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, llmClient, emit, openStdin(t))
	require.Error(t, err)
	assert.Equal(t, "error", res.Reason)

	assert.True(t, remoteHasBranch(t, remote, "cm/cmx-001"), "toolchain-missing park pushes WIP")
	assert.GreaterOrEqual(t, ops.count("ReportPush"), 1, "WIP push reported")
	assert.Equal(t, 1, ops.count("TransitionCard"), "card transitioned to blocked")
	assert.Equal(t, 1, ops.count("ReleaseCard"), "claim released on toolchain-missing park")
	assert.Equal(t, 0, ops.count("CompleteTask"))

	calls := ops.ops()
	transitionIdx := slices.Index(calls, "TransitionCard")
	releaseIdx := slices.Index(calls, "ReleaseCard")

	require.GreaterOrEqual(t, transitionIdx, 0)
	require.GreaterOrEqual(t, releaseIdx, 0)
	assert.Less(t, transitionIdx, releaseIdx, "TransitionCard must happen before ReleaseCard: ownership may be required")

	// No model-outcome/blacklist call is recorded for this arm - CardOps
	// exposes no such method for the worker to call in the first place, so the
	// full call list mirrors the other park arms exactly.
	assert.ElementsMatch(t, []string{"ClaimCard", "GetTaskContext", "ReportPush", "TransitionCard", "ReleaseCard"}, calls)

	args := ops.argsOf("TransitionCard")
	require.Len(t, args, 2)
	assert.Equal(t, "blocked", args[1])
}

// TestToolchainMissingTransitionFailureDegradesGracefully: when TransitionCard
// fails (e.g. the project's board has no in_progress -> blocked transition),
// the park must still complete exactly like the other park arms - push WIP,
// release the claim, surface the error - never fatal.
func TestToolchainMissingTransitionFailureDegradesGracefully(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()
	ops.transitionCardErr = map[string]error{"blocked": fmt.Errorf("call transition_card: invalid state transition")}

	swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
		require.NoError(t, os.WriteFile(filepath.Join(d.Cfg.Workspace, "wip.txt"), []byte("partial\n"), 0o644))

		return &orchestrator.ToolchainMissingError{Tier: "declared", Subject: "./mvnw test", Reason: "exec: \"java\": not found"}
	})

	llmClient := &scriptedLLM{}

	emit := events.NewEmitter(io.Discard, io.Discard)

	res, err := Run(context.Background(), baseSpec(t, remote, wsParent), ops, llmClient, emit, openStdin(t))
	require.Error(t, err, "the park must still surface the underlying error despite the failed transition")
	assert.Equal(t, "error", res.Reason)

	assert.True(t, remoteHasBranch(t, remote, "cm/cmx-001"), "toolchain-missing park still pushes WIP on a transition failure")
	assert.Equal(t, 1, ops.count("TransitionCard"), "transition was attempted")
	assert.Equal(t, 1, ops.count("ReleaseCard"), "claim still released despite the transition failure")
	assert.Equal(t, 0, ops.count("CompleteTask"))
}

// TestToolchainMissingDuringEndSessionMapsToEndSession: an end_session frame
// arrives while the orchestrator is mid-toolchain-check, but a context-canceled
// Tier-3 call can still surface a ToolchainMissingError instead of the ctx
// error (the race the finding describes; the Tier-4 trigger side is an
// orchestrator-side concern, not covered here). mapFSMResult must let the
// end-session/cancel arm win over the toolchain arm: no TransitionCard("blocked"),
// no failure return - the graceful pause wins.
func TestToolchainMissingDuringEndSessionMapsToEndSession(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	swapRunOrchestrator(t, func(ctx context.Context, d orchestrator.Deps) error {
		// Block until the end_session cancels the run context, then return the
		// toolchain sentinel - exactly what a canceled Tier-3 call can still
		// surface per the finding, even though end_session has already fired.
		require.NoError(t, os.WriteFile(filepath.Join(d.Cfg.Workspace, "wip.txt"), []byte("partial\n"), 0o644))
		<-ctx.Done()

		return &orchestrator.ToolchainMissingError{Tier: "detected", Subject: "pom.xml", Reason: "java not on PATH"}
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
	require.NoError(t, err, "end-session/cancel must win the race: no failure return")
	assert.Equal(t, "end_session", res.Reason)

	assert.True(t, remoteHasBranch(t, remote, "cm/cmx-001"), "WIP still pushed on the graceful pause")
	assert.Equal(t, 0, ops.count("TransitionCard"), "end-session/cancel must win: no blocked transition")
	assert.Equal(t, 1, ops.count("ReleaseCard"))
	assert.Equal(t, 0, ops.count("CompleteTask"))
}

// TestEndSessionMidFSM: an end_session frame cancels the run context while the
// FSM is in a phase; the orchestrator returns ctx.Err() and the worker takes
// the graceful path (push WIP, report usage, release, exit 0).
func TestEndSessionMidFSM(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	swapRunOrchestrator(t, func(ctx context.Context, d orchestrator.Deps) error {
		// Block until the end_session cancels the run context, then return its
		// error - exactly what the real FSM does when its ctx is canceled.
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

// TestReviewAttemptsCapNormalization covers withDefaults' resolution policy:
// an unset (zero or negative) cap resolves to the shared default rather than a
// worker-local third answer, an in-range value passes through, and a value
// above the safe maximum is lowered to it.
func TestReviewAttemptsCapNormalization(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   int
		want int
	}{
		{"unset resolves to the shared default", 0, config.DefaultReviewAttemptsCap},
		{"negative resolves to the shared default", -5, config.DefaultReviewAttemptsCap},
		{"in-range value passes through", 5, 5},
		{"the maximum passes through", config.MaxReviewAttemptsCap, config.MaxReviewAttemptsCap},
		{"above the maximum is lowered", 10, config.MaxReviewAttemptsCap},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := baseSpec(t, setupBareRemote(t), t.TempDir())
			spec.ReviewAttemptsCap = tc.in

			assert.Equal(t, tc.want, withDefaults(spec).ReviewAttemptsCap)
		})
	}
}

// TestReviewAttemptsCapReachesOrchestrator proves the RunSpec field is what the
// review loop actually reads: the const it replaced is gone, so a wiring
// regression would otherwise be invisible until a live run.
func TestReviewAttemptsCapReachesOrchestrator(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()

	spec := baseSpec(t, remote, wsParent)
	spec.ReviewAttemptsCap = 5

	var got int

	swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
		got = d.Cfg.ReviewAttemptsCap

		return nil
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	_, err := Run(context.Background(), spec, newFakeOps(), &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, 5, got, "the configured cap must reach orchestrator.Config")
}

// --- pushWIP direct tests ---------------------------------------------------

// TestPushWIPPushesStrandedCleanTreeCommit is the direct regression for the
// turn-cap salvage exposure: the salvage path commits the coder's work before
// its verify gate, so a declined salvage leaves a real commit sitting on an
// otherwise CLEAN tree. pushWIP's old `if !dirty { return }` short-circuit
// skipped the push entirely in that case, stranding the commit with the
// container. This simulates the salvage path directly - CommitWithMessage
// leaves the tree clean - then calls pushWIP and asserts the commit still
// reaches the remote.
func TestPushWIPPushesStrandedCleanTreeCommit(t *testing.T) {
	t.Parallel()

	remote := setupBareRemote(t)
	ws := filepath.Join(t.TempDir(), "ws")

	g := NewGit(ws, "", "", "")
	ctx := context.Background()

	require.NoError(t, g.Clone(ctx, remote, "main"))
	require.NoError(t, g.CreateBranch(ctx, "cm/cmx-001"))
	g.SetBranchPolicy("cm/cmx-001", "main", "main")

	require.NoError(t, os.WriteFile(filepath.Join(ws, "salvage.txt"), []byte("committed by salvage\n"), 0o644))

	committed, err := g.CommitWithMessage(ctx, "salvage: partial work")
	require.NoError(t, err)
	require.True(t, committed)

	ops := newFakeOps()

	a := fsmArgs{
		ops:    ops,
		git:    g,
		spec:   RunSpec{CardID: "CMX-001"},
		tcx:    cmclient.TaskContext{Title: "Add the widget"},
		branch: "cm/cmx-001",
	}

	pushWIP(ctx, a)

	assert.True(t, remoteHasBranch(t, remote, "cm/cmx-001"), "the stranded commit must reach the remote")
	assert.Equal(t, 1, ops.count("ReportPush"), "the push must be reported")
}

// TestPushWIPCleanTreeNoUnpushedCommitsIsNoOp is the regression guard: a clean
// tree with zero commits beyond what origin already has must not push at all.
func TestPushWIPCleanTreeNoUnpushedCommitsIsNoOp(t *testing.T) {
	t.Parallel()

	remote := setupBareRemote(t)
	ws := filepath.Join(t.TempDir(), "ws")

	g := NewGit(ws, "", "", "")
	ctx := context.Background()

	require.NoError(t, g.Clone(ctx, remote, "main"))
	require.NoError(t, g.CreateBranch(ctx, "cm/cmx-001"))
	g.SetBranchPolicy("cm/cmx-001", "main", "main")

	ops := newFakeOps()

	a := fsmArgs{
		ops:    ops,
		git:    g,
		spec:   RunSpec{CardID: "CMX-001"},
		tcx:    cmclient.TaskContext{Title: "Add the widget"},
		branch: "cm/cmx-001",
	}

	pushWIP(ctx, a)

	assert.False(t, remoteHasBranch(t, remote, "cm/cmx-001"), "no commits to push means no branch created")
	assert.Equal(t, 0, ops.count("ReportPush"), "no push means no report")
}

func TestBuildSkillToolPresentAndAbsent(t *testing.T) {
	root := t.TempDir()
	skillsDir := filepath.Join(root, "skills")
	require.NoError(t, os.MkdirAll(filepath.Join(skillsDir, "go-development"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillsDir, "go-development", "SKILL.md"),
		[]byte("---\nname: go-development\ndescription: Use for Go.\n---\nbody"), 0o644))

	// Present: a populated dir yields a tool.
	tool := buildSkillTool(RunSpec{CardID: "C1", TaskSkillsDir: skillsDir}, nil)
	require.NotNil(t, tool)
	assert.Equal(t, "skill", tool.Name())

	// Absent: no dir -> no tool (no-skills runs stay byte-identical).
	assert.Nil(t, buildSkillTool(RunSpec{CardID: "C1"}, nil))

	// Absent: explicit empty subset -> no tool.
	assert.Nil(t, buildSkillTool(RunSpec{CardID: "C1", TaskSkillsDir: skillsDir, TaskSkillsSet: true, TaskSkills: nil}, nil))
}

func TestPlanToolsRegistryCarriesFindingsTool(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	plan := tools.NewRegistry(append(tools.ReadOnlyTools(ws), orchestrator.NewFindingsTool())...)

	_, ok := plan.Get("record_finding")
	assert.True(t, ok, "the plan registry must carry the findings tool")

	_, ok = tools.NewReadOnlyRegistry(ws).Get("record_finding")
	assert.False(t, ok, "the shared read-only registry must NOT carry it: gate, review, judge, verify-propose and mob planning seats all use it, and mob round 0 is blind")

	assert.Len(t, plan.All(), len(tools.ReadOnlyTools(ws))+1)
}

func TestWriteToolsForThreadsExtraEnv(t *testing.T) {
	t.Parallel()

	wt := writeToolsFor(t.TempDir(), 60, []string{"TEST_EXTRA_VAR=hello-from-extra"})

	var bash tools.Tool

	for _, tl := range wt {
		if tl.Name() == "bash" {
			bash = tl
		}
	}

	require.NotNil(t, bash, "writeToolsFor must include the bash tool")

	res, err := bash.Execute(context.Background(), map[string]any{"command": `printf '%s' "$TEST_EXTRA_VAR"`})
	require.NoError(t, err)
	assert.Equal(t, "hello-from-extra", res.Text, "extra env entries must reach the bash subprocess")
}

func TestRunFSMPassesVerifyEnvToWriteTools(t *testing.T) {
	t.Setenv("TEST_PASSTHROUGH_VAR", "resolved-value")
	t.Setenv("GITHUB_TOKEN", "denied-secret")

	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	var mainWriteTools *tools.Registry

	var writeToolsForDir func(string) *tools.Registry

	swapRunOrchestrator(t, func(_ context.Context, d orchestrator.Deps) error {
		mainWriteTools = d.WriteTools
		writeToolsForDir = d.WriteToolsForDir

		return nil
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	spec := baseSpec(t, remote, wsParent)
	spec.Verify.Env = []string{"TEST_PASSTHROUGH_VAR", "GITHUB_TOKEN"}

	res, err := Run(context.Background(), spec, ops, &scriptedLLM{}, emit, openStdin(t))
	require.NoError(t, err)
	assert.Equal(t, "completed", res.Reason)

	require.NotNil(t, mainWriteTools)
	require.NotNil(t, writeToolsForDir)

	probe := `printf '%s|%s' "$TEST_PASSTHROUGH_VAR" "${GITHUB_TOKEN:-unset}"`

	for name, reg := range map[string]*tools.Registry{
		"main workspace": mainWriteTools,
		"candidate dir":  writeToolsForDir(t.TempDir()),
	} {
		bash, ok := reg.Get("bash")
		require.True(t, ok, name)

		out, err := bash.Execute(context.Background(), map[string]any{"command": probe})
		require.NoError(t, err, name)
		assert.Equal(t, "resolved-value|unset", out.Text,
			"%s: declared allowed name must resolve, denied name must stay absent", name)
	}
}

// TestBuildRegistryFallbackPrecedence verifies that buildRegistry resolves the
// capable default with the correct precedence: spec.Model (trigger default_model)
// first, then spec.DefaultModel (serve-config), then config.DefaultCapableModel
// (compiled-in guard). With a nil Selection, the registry has an empty candidate
// pool so SelectByComplexity returns the capable default directly.
func TestBuildRegistryFallbackPrecedence(t *testing.T) {
	tests := []struct {
		name string
		spec RunSpec
		want string
	}{
		{
			name: "trigger default_model only",
			spec: RunSpec{Model: "payload/model"},
			want: "payload/model",
		},
		{
			name: "serve-config default only",
			spec: RunSpec{DefaultModel: "serve/default"},
			want: "serve/default",
		},
		{
			name: "trigger wins over serve-config",
			spec: RunSpec{Model: "payload/model", DefaultModel: "serve/default"},
			want: "payload/model",
		},
		{
			name: "empty spec falls back to compiled-in default",
			spec: RunSpec{},
			want: config.DefaultCapableModel,
		},
	}

	in := registry.SelectInput{Role: registry.RoleCoder, Tier: registry.TierSimple}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := buildRegistry(tt.spec)
			require.NoError(t, err)

			got := r.SelectByComplexity(in)
			assert.Equal(t, tt.want, got.Model,
				"buildRegistry capable default: want %q, got %q", tt.want, got.Model)
		})
	}
}

// TestBuildRegistryThreadsTierBars proves the operator ladder actually reaches
// the registry buildRegistry returns, not just the FromSelection call: the
// registry-level tests never exercise buildRegistry, so a build that parses
// SelectorTierBars but drops the WithTierBars chain passes every test in
// that package while the knob does nothing.
func TestBuildRegistryThreadsTierBars(t *testing.T) {
	spec := RunSpec{
		Selection: &protocol.SelectionContext{
			Candidates: []protocol.CandidateModel{
				{
					Slug:                  "mid/model",
					PromptPricePerTok:     0.000001,
					CompletionPricePerTok: 0.000002,
					ContextWindow:         200000,
					CoderPrior:            0.85,
					ReviewerPrior:         0.85,
				},
			},
		},
		// Raising complex above the model's 0.85 prior (and critical to match,
		// keeping the ladder monotone) means a complex request cannot be met
		// directly and must clamp down to moderate (default bar 0.76, which
		// 0.85 clears).
		SelectorTierBars: map[string]float64{"complex": 0.99, "critical": 0.99},
	}

	r, err := buildRegistry(spec)
	require.NoError(t, err)

	got := r.SelectByComplexity(registry.SelectInput{Role: registry.RoleCoder, Tier: registry.TierComplex})

	require.True(t, got.OK)
	assert.False(t, got.AtBar(),
		"the operator's elevated complex bar must not be silently satisfied by an unthreaded default ladder")
	assert.Equal(t, registry.TierModerate, got.MetTier)
}

// TestRunReleasesClaimOnInvalidTierLadder proves an invalid operator ladder
// stops the worker before any orchestrator phase runs: the claim taken by
// Run is released, the orchestrator is never invoked, and the error message
// carries the underlying reason exactly once rather than being wrapped a
// second time on its way out of runFSM.
func TestRunReleasesClaimOnInvalidTierLadder(t *testing.T) {
	remote := setupBareRemote(t)
	wsParent := t.TempDir()
	ops := newFakeOps()

	swapRunOrchestrator(t, func(context.Context, orchestrator.Deps) error {
		t.Fatal("orchestrator must not run when the tier ladder fails to build")

		return nil
	})

	emit := events.NewEmitter(io.Discard, io.Discard)

	spec := baseSpec(t, remote, wsParent)
	// Inverted ladder: simple above critical fails the monotone check.
	spec.SelectorTierBars = map[string]float64{"simple": 0.90, "critical": 0.10}

	res, err := Run(context.Background(), spec, ops, &scriptedLLM{}, emit, openStdin(t))

	require.Error(t, err)
	assert.Equal(t, "error", res.Reason)
	assert.Equal(t, 1, ops.count("ReleaseCard"))

	msg := err.Error()
	assert.Contains(t, msg, "ladder must not decrease")
	assert.Equal(t, 1, strings.Count(msg, "build registry:"),
		"the error must be wrapped with context exactly once, not doubled on the way out of runFSM")
}

// TestLogReachabilityLogsCardOnlyWhenSomeTierIsUnreachable pins the worker-side
// gate on top of registry.Reachability: a catalog that clears every configured
// tier logs no card line at all, and a catalog that cannot reach any tier
// (nothing injected, so every request falls to the capable default) logs
// exactly one.
func TestLogReachabilityLogsCardOnlyWhenSomeTierIsUnreachable(t *testing.T) {
	catalog := llm.Catalog{
		{ID: "top/one", ContextLength: 200000, SupportedParameters: []string{"tools"}},
	}
	prior := new(0.95)
	priors := registry.Priors{Models: map[string]registry.PriorEntry{
		"top/one": {Coder: prior, Reviewer: prior},
	}}

	reachable := registry.NewRegistryFromParts(catalog, priors, nil, nil, "top/one")

	ops := newStubOps()
	logReachability(context.Background(), reachable, ops, "CMX-001")
	assert.Equal(t, 0, ops.count("AddLog"), "every tier reachable means no card log")

	unreachable := registry.NewRegistry("top/one", nil)

	ops2 := newStubOps()
	logReachability(context.Background(), unreachable, ops2, "CMX-001")
	assert.Equal(t, 1, ops2.count("AddLog"), "an unreachable tier logs exactly one card line")
}

// TestLogReachabilityToleratesNilOps proves the worker-side call is safe when
// ops2orchestrator hands back nil, which it does for a test fake that only
// implements CardOps: such tests swap runOrchestrator and never touch
// Deps.Ops, but logReachability runs before that swap takes effect.
func TestLogReachabilityToleratesNilOps(t *testing.T) {
	assert.NotPanics(t, func() {
		logReachability(context.Background(), registry.NewRegistry("top/one", nil), nil, "CMX-001")
	})
}
