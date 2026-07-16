package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/frames"
	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
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
	lastGetTaskContextImages bool // captured from GetTaskContext's includeImages arg
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

func (f *fakeOps) ClaimCard(_ context.Context, cardID string) error {
	f.record("ClaimCard", cardID)

	return nil
}

func (f *fakeOps) GetTaskContext(_ context.Context, cardID string, includeImages bool) (cmclient.TaskContext, error) {
	f.mu.Lock()
	f.lastGetTaskContextImages = includeImages
	f.mu.Unlock()

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
// interactive flag arrives — the agent self-corrects on tcx.Autonomous.
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
// same directory — proving writeToolsFor is the one source of truth behind
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
// WriteTools registry gets — Best-of-N candidates race with full tool parity
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
// graceful completion — exit-0 path, completed reason, no CompleteTask call.
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
// releases the claim, and surfaces a non-nil error — the budget-park shape — so
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
// it) pushes WIP, releases the claim, and surfaces a non-nil error — the
// context-limit park shape — so truncated work survives for resume but is
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

// TestEndSessionMidFSM: an end_session frame cancels the run context while the
// FSM is in a phase; the orchestrator returns ctx.Err() and the worker takes
// the graceful path (push WIP, report usage, release, exit 0).
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

// TestReviewAttemptsCapIsThree pins the worker's review-attempts cap to three:
// with the convergence safeguards in place, three rounds suffice.
func TestReviewAttemptsCapIsThree(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 3, reviewAttemptsCap)
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
