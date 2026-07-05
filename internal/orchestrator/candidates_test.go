package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// perDirGit hands each candidate worktree its own *fakeGit so per-candidate
// assertions stay isolated. get lazily creates a committing fake for an unseen
// dir (the happy path); set pre-scripts a dir with a failing/panicking handle.
// GitForDir is called sequentially from runFanout's main loop, so the map is
// never touched concurrently — the mutex is belt-and-braces.
type perDirGit struct {
	mu sync.Mutex
	m  map[string]GitOps
}

func newPerDirGit() *perDirGit { return &perDirGit{m: map[string]GitOps{}} }

func (p *perDirGit) get(dir string) GitOps {
	p.mu.Lock()
	defer p.mu.Unlock()

	if g, ok := p.m[dir]; ok {
		return g
	}

	g := &fakeGit{committed: true}
	p.m[dir] = g

	return g
}

func (p *perDirGit) set(dir string, g GitOps) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.m[dir] = g
}

// fakeGitFor returns the *fakeGit registered for dir, or nil when absent or not
// a plain fake (e.g. a panicGit).
func (p *perDirGit) fakeGitFor(dir string) *fakeGit {
	p.mu.Lock()
	defer p.mu.Unlock()

	g, _ := p.m[dir].(*fakeGit)

	return g
}

// panicGit is a GitOps whose commit panics, to prove a candidate goroutine's
// panic is caught and isolated to that candidate.
type panicGit struct{ *fakeGit }

func (p *panicGit) CommitWithMessage(context.Context, string) (bool, error) {
	panic("boom in commit")
}

// fanoutDeps builds Deps wired for the Best-of-N fan-out: the execute-phase
// deps plus a per-dir git registry, a per-dir write-tools factory, a real
// workspace root, and BestOfN=n. It returns the per-dir git registry and the
// workspace so tests can script a candidate's git and assert on it.
func fanoutDeps(t *testing.T, ops *fakeOps, mainGit *fakeGit, client llm.LLM, n int) (Deps, *perDirGit, string) {
	t.Helper()

	ws := t.TempDir()
	pdg := newPerDirGit()

	d := execTestDeps(ops, mainGit, client)
	d.Cfg.Workspace = ws
	d.Cfg.BestOfN = n
	d.GitForDir = pdg.get
	d.WriteToolsForDir = func(string) *tools.Registry {
		return tools.NewRegistry(tools.NewReadTool("."))
	}

	return d, pdg, ws
}

// newFanoutRun seeds a run for the fan-out with the given subtasks and card
// budget, defaulting the card tier the candidate selection reads.
func newFanoutRun(d Deps, subs []subtaskRef, maxCost float64) *run {
	o := newExecRun(d, subs, maxCost)
	o.cardTier = "moderate"

	return o
}

func TestEffectiveCeilingAndDegrade(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		reported float64
		wantCeil float64
		wantN    int
	}{
		{"gating disabled", Config{MaxCardCost: 0, BestOfN: 3}, 5, 0, 3},
		{"n3 reported 0", Config{MaxCardCost: 5, BestOfN: 3}, 0, 20, 3},
		{"n3 reported 10.1", Config{MaxCardCost: 5, BestOfN: 3}, 10.1, 20, 1},
		{"n3 reported 12.6", Config{MaxCardCost: 5, BestOfN: 3}, 12.6, 20, 1},
		{"n3 reported 19.9 never zero", Config{MaxCardCost: 5, BestOfN: 3}, 19.9, 20, 1},
		{"n5 reported 5", Config{MaxCardCost: 5, BestOfN: 5}, 5, 30, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.InDelta(t, tt.wantCeil, effectiveCeiling(tt.cfg), 1e-9)
			assert.Equal(t, tt.wantN, degradeN(tt.cfg, tt.reported))
		})
	}
}

func TestFanoutHappyPath(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	// Empty planLLM -> exhausted fallback stop turns: every coder run completes
	// cleanly at zero cost; commits land because the per-dir fakes commit=true.
	d, pdg, _ := fanoutDeps(t, ops, mainGit, &planLLM{}, 3)

	o := newFanoutRun(d, []subtaskRef{
		{ID: "SUB-1", Title: "First", Tier: "simple"},
		{ID: "SUB-2", Title: "Second", Tier: "simple"},
	}, 0)

	require.NoError(t, o.runFanout(context.Background()))

	// Three worktrees cut on the main git, one per candidate branch.
	gitCalls := mainGit.recorded()
	for _, br := range []string{"cm/card-1-c1", "cm/card-1-c2", "cm/card-1-c3"} {
		assert.GreaterOrEqual(t, indexOfCall(gitCalls, "AddWorktree:"+br), 0,
			"worktree %s must be added; git=%v", br, gitCalls)
	}

	assert.Equal(t, 1, countCalls(gitCalls, "DisableAutoGC"), "auto-gc disabled exactly once")

	// Zero subtask board writes during the fan-out.
	opCalls := ops.recorded()
	assert.Equal(t, 0, countCalls(opCalls, "ClaimCard:SUB-1"), "candidates never claim subtasks")
	assert.Equal(t, 0, countCalls(opCalls, "ClaimCard:SUB-2"))
	assert.Equal(t, -1, indexOfCall(opCalls, "CompleteTask:SUB-1"), "candidates never complete subtasks")
	assert.Equal(t, -1, indexOfCall(opCalls, "CompleteTask:SUB-2"))

	// Zero pushes anywhere: the main branch and every candidate branch stay local.
	assert.Empty(t, mainGit.pushBranches, "no push on the main git")

	require.Len(t, o.candidates, 3)

	for i, c := range o.candidates {
		require.NoError(t, c.err, "candidate %d must succeed", i+1)
		assert.Len(t, c.completed, 2, "candidate %d ran both subtasks", i+1)

		fg := pdg.fakeGitFor(c.dir)
		require.NotNil(t, fg, "per-dir git for candidate %d (%s)", i+1, c.dir)
		assert.Equal(t, 2, countCalls(fg.recorded(), "CommitWithMessage"),
			"candidate %d commits once per subtask", i+1)
		assert.Empty(t, fg.pushBranches, "candidate %d must not push", i+1)
	}

	assert.True(t, ops.loggedContains("best-of-n: candidate 2/3"),
		"the fan-out logs each candidate start; logs=%v", ops.logs)
}

func TestFanoutCandidateFailureIsolated(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	d, pdg, ws := fanoutDeps(t, ops, mainGit, &planLLM{}, 3)

	// Candidate 2's worktree cannot commit: it must drop out alone.
	pdg.set(filepath.Join(ws, ".worktrees", "c2"), &fakeGit{commitErr: assertErr("disk full")})

	o := newFanoutRun(d, []subtaskRef{
		{ID: "SUB-1", Title: "First", Tier: "simple"},
		{ID: "SUB-2", Title: "Second", Tier: "simple"},
	}, 0)

	// Two candidates still succeed, so the fan-out as a whole succeeds.
	require.NoError(t, o.runFanout(context.Background()))

	require.Len(t, o.candidates, 3)
	require.NoError(t, o.candidates[0].err)
	require.Error(t, o.candidates[1].err, "the failing candidate is dropped")
	require.NoError(t, o.candidates[2].err)
	assert.Len(t, o.candidates[0].completed, 2)
	assert.Len(t, o.candidates[2].completed, 2)
}

func TestFanoutPanicIsolated(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	d, pdg, ws := fanoutDeps(t, ops, mainGit, &planLLM{}, 3)

	// Candidate 1's worktree panics mid-commit: the goroutine recover must
	// convert it into a per-candidate error without crashing the run.
	pdg.set(filepath.Join(ws, ".worktrees", "c1"), &panicGit{fakeGit: &fakeGit{committed: true}})

	o := newFanoutRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)

	require.NoError(t, o.runFanout(context.Background()))

	require.Len(t, o.candidates, 3)
	require.Error(t, o.candidates[0].err)
	assert.Contains(t, o.candidates[0].err.Error(), "panic", "the panic is captured as the candidate error")
	require.NoError(t, o.candidates[1].err)
	require.NoError(t, o.candidates[2].err)
}

func TestFanoutDegradeLogged(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	d, _, _ := fanoutDeps(t, ops, mainGit, &planLLM{}, 3)

	// MaxCardCost 5, ceiling 20, already reported 12.6 -> remaining 7.4 funds one.
	o := newFanoutRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 5)
	o.tc.ReportedCostUSD = 12.6

	require.NoError(t, o.runFanout(context.Background()))

	gitCalls := mainGit.recorded()
	assert.Equal(t, 1, countCalls(gitCalls, "AddWorktree:cm/card-1-c1"), "one worktree")
	assert.Equal(t, 0, countCalls(gitCalls, "AddWorktree:cm/card-1-c2"), "no second worktree")
	assert.Len(t, o.candidates, 1)
	assert.True(t, ops.loggedContains("reduced to 1"), "the degrade is logged; logs=%v", ops.logs)
}

func TestFanoutAllFailed(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	// Every coder run errors (transport failure), so no candidate survives.
	d, _, _ := fanoutDeps(t, ops, mainGit, &errLLM{err: errors.New("connection reset")}, 3)

	o := newFanoutRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)

	err := o.runFanout(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "all 3 candidates failed")
}

// TestFanoutIncapableRecoveryRaceSafe fans out three candidates whose models are
// all harness-incapable, so every candidate drives the shared incapable-recovery
// state (o.reselects / o.excluded) and the coder-model record concurrently. Run
// under -race, it proves the selMu discipline serializes those mutations; the
// shared re-selection cap starves every candidate, so the fan-out fails.
func TestFanoutIncapableRecoveryRaceSafe(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	client := &modelAwareLLM{incapable: map[string]bool{
		"alpha/coder": true, "beta/coder": true, "capable/default": true,
	}}
	d, _, _ := fanoutDeps(t, ops, mainGit, client, 3)
	d.Registry = twoCoderRegistry()

	o := newFanoutRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "moderate"}}, 0)

	err := o.runFanout(context.Background())
	require.Error(t, err, "all candidates park on the shared re-selection cap")

	require.Len(t, o.candidates, 3)

	for i, c := range o.candidates {
		require.Error(t, c.err, "candidate %d must park", i+1)
	}
}

func TestUserNotesUnseen(t *testing.T) {
	var nilNotes *userNotes
	assert.Empty(t, nilNotes.unseen(1), "a nil *userNotes (autonomous) is a no-op")

	n := newUserNotes()
	assert.Empty(t, n.unseen(1), "no notes yet")

	n.add("first note")
	n.add(" ") // blank turns are ignored
	n.add("second note")

	// A candidate sees every unconsumed note joined, then its cursor advances.
	assert.Equal(t, "first note\n\nsecond note", n.unseen(1))
	assert.Empty(t, n.unseen(1), "already-consumed notes are not re-served")

	// Each candidate carries its own cursor.
	n.add("third note")
	assert.Equal(t, "first note\n\nsecond note\n\nthird note", n.unseen(2))
	assert.Equal(t, "third note", n.unseen(1), "cursor 1 only sees the new note")
}

// TestFanoutInteractiveBroadcast proves the HITL note collector runs alongside
// the candidates and buffers a mid-run user turn. runFanout joins the collector
// before returning, so the buffered note is observable deterministically.
func TestFanoutInteractiveBroadcast(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	d, _, _ := fanoutDeps(t, ops, mainGit, &planLLM{}, 2)
	d.Cfg.Interactive = true
	// One queued turn, then the inbox closes (block=false) so the collector stops.
	d.Human = &fakeInbox{msgs: []harness.UserMessage{{Content: "please add tests", MessageID: "m1"}}}

	o := newFanoutRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)

	require.NoError(t, o.runFanout(context.Background()))

	require.NotNil(t, o.notes)
	assert.Equal(t, "please add tests", o.notes.unseen(99),
		"the collector must buffer the mid-run note before the fan-out returns")
}

// TestFanoutInteractiveCollectorLifecycle proves the collector's other exit
// path: it blocks in Wait until the fan-out's derived context is canceled on
// return. If the join hung, the test would time out.
func TestFanoutInteractiveCollectorLifecycle(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	d, _, _ := fanoutDeps(t, ops, mainGit, &planLLM{}, 2)
	d.Cfg.Interactive = true
	d.Human = &fakeInbox{block: true} // no messages; blocks until canceled

	o := newFanoutRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)

	require.NoError(t, o.runFanout(context.Background()))

	require.NotNil(t, o.notes)
	require.Len(t, o.candidates, 2)

	for i, c := range o.candidates {
		require.NoError(t, c.err, "candidate %d", i+1)
	}
}
