package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// coderPrior builds a PriorEntry with only the coder role scored, for registries
// whose candidate selection is coder-role only.
func coderPrior(v float64) registry.PriorEntry {
	return registry.PriorEntry{Coder: &v}
}

// perDirGit hands each candidate worktree its own *fakeGit so per-candidate
// assertions stay isolated. get lazily creates a committing fake for an unseen
// dir (the happy path); set pre-scripts a dir with a failing/panicking handle.
// GitForDir is called sequentially from runFanout's main loop, so the map is
// never touched concurrently - the mutex is belt-and-braces.
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
		return testWriteTools()
	}

	return d, pdg, ws
}

// newFanoutRun seeds a run for the fan-out with the given subtasks and card
// budget, defaulting the card tier the candidate selection reads. The cleanup
// stops any fan-out heartbeater the run started: in production it outlives
// runFanout on purpose (the judge span holds the claims), but a goroutine
// outliving a TEST races the package-var interval shrink other tests do.
func newFanoutRun(t *testing.T, d Deps, subs []subtaskRef, maxCost float64) *run {
	t.Helper()

	o := newExecRun(d, subs, maxCost)
	o.cardTier = "moderate"

	t.Cleanup(o.stopFanoutHeartbeat)

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
		{
			"mob only adds the budget-factor term",
			Config{MaxCardCost: 5, Mob: MobConfig{Participants: 3, BudgetFactor: 0.75}},
			0, 8.75, 0,
		},
		{
			"mob composes with best-of-n",
			Config{MaxCardCost: 5, BestOfN: 3, Mob: MobConfig{Participants: 3, BudgetFactor: 0.75}},
			0, 23.75, 3,
		},
		{
			"mob off by participants leaves the ceiling alone",
			Config{MaxCardCost: 5, Mob: MobConfig{Participants: 1, BudgetFactor: 0.75}},
			0, 5, 0,
		},
		{
			"mob with gating disabled stays disabled",
			Config{MaxCardCost: 0, Mob: MobConfig{Participants: 3, BudgetFactor: 0.75}},
			0, 0, 0,
		},
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

	o := newFanoutRun(t, d, []subtaskRef{
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
	assert.Equal(t, 1, countCalls(gitCalls, "AddInfoExclude:.worktrees/"),
		"candidate worktrees are excluded from the parent clone's staging path")
	assert.Equal(t, []string{".worktrees/"}, mainGit.infoExcludes, "the exclude pattern is .worktrees/")

	// First-arrival claims: the RUN claims each subtask exactly once when the
	// first candidate reaches it, so the board shows in_progress during the
	// race (and CM's parent auto-transition fires). Candidates themselves never
	// claim - three racers, one claim per subtask. Completions still wait for
	// the winner replay.
	opCalls := ops.recorded()
	assert.Equal(t, 1, countCalls(opCalls, "ClaimCard:SUB-1"), "each subtask claimed exactly once, not per candidate")
	assert.Equal(t, 1, countCalls(opCalls, "ClaimCard:SUB-2"))
	assert.Equal(t, -1, indexOfCall(opCalls, "CompleteTask:SUB-1"), "candidates never complete subtasks")
	assert.Equal(t, -1, indexOfCall(opCalls, "CompleteTask:SUB-2"))

	// Candidate spend is reported against the PARENT card, never the subtasks, so
	// the run's cost authority (and resume-time degradeN) sees it.
	assert.Positive(t, countCalls(opCalls, "ReportUsage:CARD-1"), "candidate usage reports on the parent card")
	assert.Equal(t, 0, countCalls(opCalls, "ReportUsage:SUB-1"), "candidate usage never lands on a subtask card")
	assert.Equal(t, 0, countCalls(opCalls, "ReportUsage:SUB-2"))

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
	assert.False(t, ops.loggedContains("dropped:"), "no drop lines when every candidate survives")
}

func TestFanoutCandidateFailureIsolated(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	d, pdg, ws := fanoutDeps(t, ops, mainGit, &planLLM{}, 3)

	// Candidate 2's worktree cannot commit: it must drop out alone.
	pdg.set(filepath.Join(ws, ".worktrees", "c2"), &fakeGit{commitErr: assertErr("disk full")})

	o := newFanoutRun(t, d, []subtaskRef{
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

	assert.True(t, ops.loggedContains("best-of-n: candidate 2/3"),
		"the dropped candidate is named; logs=%v", ops.logs)
	assert.True(t, ops.loggedContains("dropped:"),
		"the drop is logged after the join; logs=%v", ops.logs)
	assert.True(t, ops.loggedContains("disk full"),
		"the drop reason is carried into the log; logs=%v", ops.logs)
}

func TestFanoutPanicIsolated(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	d, pdg, ws := fanoutDeps(t, ops, mainGit, &planLLM{}, 3)

	// Candidate 1's worktree panics mid-commit: the goroutine recover must
	// convert it into a per-candidate error without crashing the run.
	pdg.set(filepath.Join(ws, ".worktrees", "c1"), &panicGit{fakeGit: &fakeGit{committed: true}})

	o := newFanoutRun(t, d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)

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
	o := newFanoutRun(t, d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 5)
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

	o := newFanoutRun(t, d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)

	err := o.runFanout(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "all 3 candidates failed")

	// The run parked while holding first-arrival subtask claims: they are
	// released on the way out (mirroring the single-solver error path) instead
	// of dangling until CM's stall sweep mislabels them 30 minutes later.
	opCalls := ops.recorded()
	assert.Equal(t, countCalls(opCalls, "ClaimCard:SUB-1"), countCalls(opCalls, "ReleaseCard:SUB-1"),
		"every first-arrival claim is released on the all-failed path")
}

// TestFanoutFirstArrivalClaimBestEffort: the first-arrival claim is board
// cosmetics, not a correctness gate - a claim failure must not kill the race.
func TestFanoutFirstArrivalClaimBestEffort(t *testing.T) {
	ops := &fakeOps{claimErr: errors.New("cm unreachable")}
	mainGit := &fakeGit{}
	d, _, _ := fanoutDeps(t, ops, mainGit, &planLLM{}, 3)

	o := newFanoutRun(t, d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)

	require.NoError(t, o.runFanout(context.Background()), "claim failure must not abort the fan-out")
	assert.Equal(t, 1, countCalls(ops.recorded(), "ClaimCard:SUB-1"),
		"the claim was attempted once despite failing")

	for i, c := range o.candidates {
		require.NoError(t, c.err, "candidate %d must succeed", i+1)
	}
}

// TestFanoutHeartbeatStopBlocksAndCovers: the fan-out heartbeater ticks every
// first-arrival-claimed subtask (CM's stall sweep reclaims any claimed card
// whose heartbeat lapses, and the race + judge span is wall-clock unbounded)
// and its stop func blocks until the goroutine exits.
func TestFanoutHeartbeatStopBlocksAndCovers(t *testing.T) {
	prev := subtaskHeartbeatInterval
	subtaskHeartbeatInterval = 10 * time.Millisecond

	defer func() { subtaskHeartbeatInterval = prev }()

	ops := &fakeOps{}
	mainGit := &fakeGit{}
	d, _, _ := fanoutDeps(t, ops, mainGit, &planLLM{}, 2)

	o := newFanoutRun(t, d, nil, 0)
	o.claimedSubs = map[string]bool{"SUB-1": true, "SUB-2": true}

	stop := o.startFanoutHeartbeat(context.Background())

	require.Eventually(t, func() bool {
		calls := ops.recorded()

		return countCalls(calls, "Heartbeat:SUB-1") >= 1 && countCalls(calls, "Heartbeat:SUB-2") >= 1
	}, 2*time.Second, 5*time.Millisecond, "every claimed subtask gets heartbeated")

	stop()

	quiesced := len(ops.recorded())

	time.Sleep(5 * subtaskHeartbeatInterval)
	assert.Len(t, ops.recorded(), quiesced, "no heartbeats after stop returns")
}

// TestFanoutIncapableRecoveryRaceSafe fans out three candidates when EVERY coder
// model is harness-incapable, so each candidate drives the shared recovery state
// (o.reselects / o.excluded) and re-picks concurrently. Run under -race, it proves
// the selMu discipline serializes those mutations. With no capable model anywhere,
// every candidate fails - some via the shared re-selection cap, some via a drained
// pool (the empty-model sentinel) - so the fan-out fails.
func TestFanoutIncapableRecoveryRaceSafe(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	client := &modelAwareLLM{incapable: map[string]bool{
		"alpha/coder": true, "beta/coder": true, "capable/default": true,
	}}
	d, _, _ := fanoutDeps(t, ops, mainGit, client, 3)
	d.Registry = twoCoderRegistry()

	o := newFanoutRun(t, d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "moderate"}}, 0)

	err := o.runFanout(context.Background())
	require.Error(t, err, "no capable model exists, so every candidate fails")

	require.Len(t, o.candidates, 3)

	for i, c := range o.candidates {
		require.Error(t, c.err, "candidate %d must fail", i+1)
	}
}

// TestFanoutSkipsWhenAllSubtasksDone proves the crash-window guard: a resume that
// finds every subtask already board-done returns without cutting worktrees or
// creating candidates, so the run cannot re-race and double-report outcomes.
func TestFanoutSkipsWhenAllSubtasksDone(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	d, _, _ := fanoutDeps(t, ops, mainGit, &planLLM{}, 3)

	o := newFanoutRun(t, d, []subtaskRef{
		{ID: "SUB-1", Title: "First", State: "done"},
		{ID: "SUB-2", Title: "Second", State: "done"},
	}, 0)

	require.NoError(t, o.runFanout(context.Background()))

	assert.Nil(t, o.candidates, "no candidates created when every subtask is already done")

	gitCalls := mainGit.recorded()
	assert.Equal(t, 0, countCalls(gitCalls, "DisableAutoGC"), "no fan-out setup on the done-skip path")

	for _, c := range gitCalls {
		assert.NotContains(t, c, "AddWorktree", "no worktree is cut when the fan-out is skipped")
	}

	assert.True(t, ops.loggedContains("every subtask already complete"), "the skip is logged; logs=%v", ops.logs)
}

// TestFanoutFoldsCandidateSpendIntoParentLedger proves every candidate's spend
// (winners and losers) is folded into the run ledger after the race, so the
// post-fan-out phases budget against the true remaining envelope.
func TestFanoutFoldsCandidateSpendIntoParentLedger(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	// modelAwareLLM with no incapable models: every coder run is a canned stop
	// costing $0.01.
	d, _, _ := fanoutDeps(t, ops, mainGit, &modelAwareLLM{}, 3)

	// Generous budget so nothing parks and all three candidates run both subtasks.
	o := newFanoutRun(t, d, []subtaskRef{
		{ID: "SUB-1", Title: "First", Tier: "simple"},
		{ID: "SUB-2", Title: "Second", Tier: "simple"},
	}, 100)

	require.NoError(t, o.runFanout(context.Background()))
	require.Len(t, o.candidates, 3)

	// 3 candidates x 2 subtasks x $0.01 = $0.06, all folded into the run ledger
	// (which started at zero reported spend).
	assert.InDelta(t, 0.06, o.ledger.Spent(), 1e-9,
		"every candidate's spend folds into the run ledger")
}

// TestFanoutCandidateReselectsOnIncapable proves a candidate whose model is
// reported incapable once does NOT hot-loop that model: it re-picks the next-best
// coder model and completes on it. capGood/default is capable, so once bad/coder
// is excluded the candidate finishes rather than dropping.
func TestFanoutCandidateReselectsOnIncapable(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	// Single incapable coder model; the capable default is fine.
	client := &modelAwareLLM{incapable: map[string]bool{"solo/coder": true}}
	d, pdg, _ := fanoutDeps(t, ops, mainGit, client, 1)
	d.Registry = registry.NewRegistryFromParts(
		llm.Catalog{
			{ID: "solo/coder", ContextLength: 200000, SupportedParameters: []string{"tools"}, PromptPricePerTok: 1e-6},
			{ID: "capgood/default", ContextLength: 200000, SupportedParameters: []string{"tools"}},
		},
		registry.Priors{Models: map[string]registry.PriorEntry{"solo/coder": coderPrior(0.9)}},
		nil, nil, "capgood/default",
	)

	o := newFanoutRun(t, d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "moderate"}}, 0)

	require.NoError(t, o.runFanout(context.Background()))
	require.Len(t, o.candidates, 1)

	c := o.candidates[0]
	require.NoError(t, c.err, "the candidate must recover on a different model, not drop")
	assert.Equal(t, "capgood/default", c.model, "c.model reflects the LAST model the candidate ran")
	assert.Contains(t, ops.recorded(), "BlacklistModel:CARD-1/solo/coder", "the incapable model was blacklisted")

	fg := pdg.fakeGitFor(c.dir)
	require.NotNil(t, fg)
	assert.Positive(t, countCalls(fg.recorded(), "CommitWithMessage"), "the recovered candidate committed its work")
}

// TestFanoutCandidateDropsWhenPoolExhausted proves a candidate whose model pool
// drains (every viable model excluded) drops cleanly via the empty-model
// sentinel, while a sibling candidate on a pinned, prior-less model - invisible to
// auto-selection, so the drained candidate cannot steal it - is unaffected.
func TestFanoutCandidateDropsWhenPoolExhausted(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}
	// pinned/coder is capable; bad1/coder and the capable default are incapable.
	client := &modelAwareLLM{incapable: map[string]bool{"bad1/coder": true, "bad2/default": true}}
	d, pdg, _ := fanoutDeps(t, ops, mainGit, client, 2)
	d.Registry = registry.NewRegistryFromParts(
		llm.Catalog{
			// pinned/coder has NO prior, so it is reachable only via the pin - the
			// drained candidate's auto re-pick can never land on it.
			{ID: "pinned/coder", ContextLength: 200000, SupportedParameters: []string{"tools"}},
			{ID: "bad1/coder", ContextLength: 200000, SupportedParameters: []string{"tools"}, PromptPricePerTok: 1e-6},
			{ID: "bad2/default", ContextLength: 200000, SupportedParameters: []string{"tools"}},
		},
		registry.Priors{Models: map[string]registry.PriorEntry{"bad1/coder": coderPrior(0.8)}},
		nil, nil, "bad2/default",
	)

	o := newFanoutRun(t, d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "moderate"}}, 0)
	o.tc.ModelCoder = "pinned/coder" // fan-out gives slot 1 the pin

	require.NoError(t, o.runFanout(context.Background()), "one survivor keeps the fan-out alive")
	require.Len(t, o.candidates, 2)

	// Candidate 1 (the pinned, capable model) completes and is unaffected.
	require.NoError(t, o.candidates[0].err, "the pinned candidate completes")
	assert.Equal(t, "pinned/coder", o.candidates[0].model)

	// Candidate 2's pool drains (bad1 then the capable default, both incapable) and
	// it drops on the exhausted-pool sentinel.
	require.Error(t, o.candidates[1].err, "the drained candidate drops")
	assert.Contains(t, o.candidates[1].err.Error(), "pool exhausted")

	fg := pdg.fakeGitFor(o.candidates[0].dir)
	require.NotNil(t, fg)
	assert.Positive(t, countCalls(fg.recorded(), "CommitWithMessage"), "the surviving candidate committed its work")
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

	o := newFanoutRun(t, d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)

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

	o := newFanoutRun(t, d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)

	require.NoError(t, o.runFanout(context.Background()))

	require.NotNil(t, o.notes)
	require.Len(t, o.candidates, 2)

	for i, c := range o.candidates {
		require.NoError(t, c.err, "candidate %d", i+1)
	}
}

// TestFanoutCoderPromptsCarryWorktreeRoot proves each candidate's coder prompt
// names ITS OWN worktree as the repo root, not the shared parent workspace.
func TestFanoutCoderPromptsCarryWorktreeRoot(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{}
	d, _, ws := fanoutDeps(t, ops, &fakeGit{}, client, 2)

	o := newFanoutRun(t, d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)
	require.NoError(t, o.runFanout(context.Background()))

	joined := strings.Join(client.tasks, "\n")
	assert.Contains(t, joined, filepath.Join(ws, ".worktrees", "c1"))
	assert.Contains(t, joined, filepath.Join(ws, ".worktrees", "c2"))
}

// TestFanoutSalvagesCappedCandidates proves a candidate that hits the turn cap
// on its FINAL subtask survives the fan-out marked capped, with its work
// committed, instead of dropping and shrinking the judge pool.
func TestFanoutSalvagesCappedCandidates(t *testing.T) {
	ops := &fakeOps{}
	mainGit := &fakeGit{}

	// Every request burns a turn: both candidates (MaxTurns=5 each, one subtask)
	// cap. 12 responses > 2x5 so neither ever reaches the exhausted stop.
	responses := make([]llm.Response, 0, 12)
	for range 12 {
		responses = append(responses, burnResp(""))
	}

	d, pdg, ws := fanoutDeps(t, ops, mainGit, &planLLM{responses: responses}, 2)
	// fanoutDeps builds on execTestDeps, which defaults MaxTurns to 20; this
	// test needs each candidate's coder run to cap after 5 scripted burn
	// turns - set BEFORE newFanoutRun, or 12 responses against MaxTurns=20
	// exhaust into a clean stop and nothing caps.
	d.Cfg.MaxTurns = 5
	o := newFanoutRun(t, d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	require.NoError(t, o.runFanout(context.Background()), "capped candidates are survivors, not failures")

	require.Len(t, o.candidates, 2)

	for i, c := range o.candidates {
		require.NoError(t, c.err, "candidate %d survives", i+1)
		assert.True(t, c.capped, "candidate %d is marked capped", i+1)
		require.Len(t, c.completed, 1, "candidate %d records the salvaged subtask", i+1)

		fg := pdg.fakeGitFor(filepath.Join(ws, ".worktrees", fmt.Sprintf("c%d", i+1)))
		require.NotNil(t, fg)
		assert.NotEmpty(t, fg.commitMsgs, "candidate %d worktree is committed", i+1)
	}

	assert.True(t, ops.loggedContains("turn cap on final subtask"), "logs=%v", ops.logs)
}
