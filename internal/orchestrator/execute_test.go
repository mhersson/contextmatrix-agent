package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowLLM delegates to inner after a fixed delay, so heartbeat ticks can fire
// during a "long" coder run. Both LLM methods are overridden because the
// harness streams.
type slowLLM struct {
	inner *planLLM
	delay time.Duration
}

func (s *slowLLM) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	time.Sleep(s.delay)

	return s.inner.Send(ctx, req)
}

func (s *slowLLM) SendStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	time.Sleep(s.delay)

	return s.inner.SendStream(ctx, req, onDelta)
}

// execTestDeps builds Deps wired for the execute phase: scripted ops + git, a
// single stop-response coder LLM, full write tools, and the plan test registry.
func execTestDeps(ops *fakeOps, git *fakeGit, client llm.LLM) Deps {
	return Deps{
		Ops:        ops,
		Git:        git,
		Client:     client,
		Emit:       events.NewEmitter(nil, nil),
		Registry:   planTestRegistry(),
		WriteTools: testWriteTools(),
		ReadTools:  tools.NewRegistry(tools.NewReadTool(".")),
		Cfg: Config{
			Project:      "proj",
			CardID:       "CARD-1",
			Branch:       "cm/card-1",
			PayloadModel: "payload/model",
			DefaultModel: "default/model",
			// Comfortably above wrapUpTurns (5): these single-turn fixtures must
			// finish before the one-shot nudge fires, or it becomes the captured
			// "last user message" instead of the real prompt. Tests that exercise
			// the turn cap or the nudge itself override this explicitly.
			MaxTurns: 20,
		},
	}
}

// newExecRun builds a run with the given subtasks pre-seeded (the plan phase
// normally sets these), the parent task context, and the configured ledger cap.
func newExecRun(d Deps, subs []subtaskRef, maxCost float64) *run {
	d.Cfg.MaxCardCost = maxCost
	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent card", Description: "parent body"}
	o := newRun(d, tc)
	o.subtasks = subs
	// Pre-resolve a skip plan so runExecute's ensureVerify is a cached no-op —
	// execute tests exercise the coder loop, not verify resolution.
	isolateVerify(o)

	return o
}

func TestTopoOrder(t *testing.T) {
	t.Run("dependencies run first", func(t *testing.T) {
		// C depends on B, B depends on A — declared out of creation order to prove
		// the sort orders by dependency, then by original creation order.
		subs := []subtaskRef{
			{ID: "A", Title: "a", DependsOnIDs: nil},
			{ID: "B", Title: "b", DependsOnIDs: []string{"A"}},
			{ID: "C", Title: "c", DependsOnIDs: []string{"B"}},
		}
		got, err := topoOrder(subs)
		require.NoError(t, err)

		var ids []string
		for _, s := range got {
			ids = append(ids, s.ID)
		}

		assert.Equal(t, []string{"A", "B", "C"}, ids)
	})

	t.Run("ready ties preserve creation order", func(t *testing.T) {
		// A and B are both roots; D depends on both. Among ready nodes the
		// original order (A before B) is preserved deterministically.
		subs := []subtaskRef{
			{ID: "A", Title: "a"},
			{ID: "B", Title: "b"},
			{ID: "D", Title: "d", DependsOnIDs: []string{"A", "B"}},
		}
		got, err := topoOrder(subs)
		require.NoError(t, err)

		var ids []string
		for _, s := range got {
			ids = append(ids, s.ID)
		}

		assert.Equal(t, []string{"A", "B", "D"}, ids)
	})

	t.Run("cycle is rejected", func(t *testing.T) {
		subs := []subtaskRef{
			{ID: "A", Title: "a", DependsOnIDs: []string{"B"}},
			{ID: "B", Title: "b", DependsOnIDs: []string{"A"}},
		}
		_, err := topoOrder(subs)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cycle")
	})
}

func TestExecuteSubtaskFlow(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &planLLM{responses: []llm.Response{
		finishResp("feat(x): add y", 0.10),
	}}
	d := execTestDeps(ops, git, llmFake)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)
	require.NoError(t, runExecute(context.Background(), o))

	// Per-subtask ordered op sequence.
	calls := ops.recorded()
	claim := indexOfCall(calls, "ClaimCard:SUB-1")
	report := indexOfCall(calls, "ReportUsage:SUB-1")
	complete := indexOfCall(calls, "CompleteTask:SUB-1")
	require.GreaterOrEqual(t, claim, 0, "claim recorded; calls=%v", calls)
	require.GreaterOrEqual(t, report, 0)
	require.GreaterOrEqual(t, complete, 0)
	assert.Less(t, claim, report, "claim before report")
	assert.Less(t, report, complete, "report before complete")

	// Commit then push the branch, in that order.
	gitCalls := git.recorded()
	commitIdx := indexOfCall(gitCalls, "CommitWithMessage")
	pushIdx := indexOfCall(gitCalls, "Push:cm/card-1")
	require.GreaterOrEqual(t, commitIdx, 0, "commit recorded; git=%v", gitCalls)
	require.GreaterOrEqual(t, pushIdx, 0, "push recorded; git=%v", gitCalls)
	assert.Less(t, commitIdx, pushIdx, "commit before push")

	// Actual cost spent on the ledger.
	assert.InDelta(t, 0.10, o.ledger.Spent(), 1e-9)

	// The selected coder model is logged to the card activity feed for the user.
	assert.True(t, ops.loggedContains("coder model"),
		"executeSubtask must log the selected coder model")
}

// TestExecuteSubtaskHeartbeatsClaim pins that a claimed subtask is heartbeated
// for the whole coder span (CM's stall sweep reclaims any claimed card whose
// last_heartbeat exceeds 30m; the parent heartbeat does not cover subtasks),
// and that the heartbeat goroutine stops when the subtask completes.
func TestExecuteSubtaskHeartbeatsClaim(t *testing.T) {
	// Mutates package-level subtaskHeartbeatInterval; cannot run in parallel.
	prev := subtaskHeartbeatInterval
	subtaskHeartbeatInterval = 10 * time.Millisecond

	defer func() { subtaskHeartbeatInterval = prev }()

	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &slowLLM{
		inner: &planLLM{responses: []llm.Response{finishResp("feat: x", 0.01)}},
		delay: 80 * time.Millisecond,
	}
	d := execTestDeps(ops, git, client)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)
	require.NoError(t, runExecute(context.Background(), o))

	beats := countCalls(ops.recorded(), "Heartbeat:SUB-1")
	assert.GreaterOrEqual(t, beats, 2, "expected >=2 subtask heartbeats during a slow coder run; calls=%v", ops.recorded())

	// The goroutine must stop with the claim: no further ticks after return.
	time.Sleep(60 * time.Millisecond)
	assert.Equal(t, beats, countCalls(ops.recorded(), "Heartbeat:SUB-1"),
		"heartbeats must stop once the subtask completes (goroutine leak)")
}

// blockingHeartbeatOps wraps fakeOps and makes Heartbeat block until ctx is
// canceled, then return ctx.Err() — mirroring a well-behaved Ops transport.
// entered is closed the instant Heartbeat is invoked, so a test can wait for a
// tick to be genuinely in flight (inside the blocking call) before exercising
// the stop func.
type blockingHeartbeatOps struct {
	*fakeOps

	entered chan struct{}
}

func (b *blockingHeartbeatOps) Heartbeat(ctx context.Context, cardID string) error {
	b.record("Heartbeat:" + cardID)
	close(b.entered)

	<-ctx.Done()

	return ctx.Err()
}

// TestSubtaskHeartbeatStopUnblocksBlockedHeartbeat pins that
// startSubtaskHeartbeat's stop func returns promptly even while a heartbeat
// tick is blocked mid-call — but only because the Ops implementation honors
// context cancellation. The blocking stop func in executeClaimed's defer
// depends entirely on that contract: if a future Ops implementation or
// transport ignored ctx, stop would hang forever and every subtask completion
// would deadlock. This test proves the contract is exercised, not assumed.
func TestSubtaskHeartbeatStopUnblocksBlockedHeartbeat(t *testing.T) {
	// Mutates package-level subtaskHeartbeatInterval; cannot run in parallel.
	prev := subtaskHeartbeatInterval
	subtaskHeartbeatInterval = 10 * time.Millisecond

	defer func() { subtaskHeartbeatInterval = prev }()

	ops := &blockingHeartbeatOps{fakeOps: &fakeOps{}, entered: make(chan struct{})}

	stop := startSubtaskHeartbeat(context.Background(), ops, "SUB-1")

	select {
	case <-ops.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat tick never fired")
	}

	stopped := make(chan struct{})

	go func() {
		stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return within 2s: heartbeat goroutine leaked past cancellation")
	}
}

// TestExecuteSubtaskErrorReleasesClaim pins that an error exit after a
// successful claim releases the subtask, so an aborted run does not leave the
// in-flight subtask claimed until CM's 30-minute stall sweep.
func TestExecuteSubtaskErrorReleasesClaim(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{commitErr: assertErr("disk full")}
	llmFake := &planLLM{responses: []llm.Response{finishResp("feat: x", 0.01)}}
	d := execTestDeps(ops, git, llmFake)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)
	err := runExecute(context.Background(), o)
	require.Error(t, err)
	require.ErrorContains(t, err, "commit subtask SUB-1")

	calls := ops.recorded()
	claim := indexOfCall(calls, "ClaimCard:SUB-1")
	release := indexOfCall(calls, "ReleaseCard:SUB-1")
	require.GreaterOrEqual(t, claim, 0, "claim recorded; calls=%v", calls)
	require.GreaterOrEqual(t, release, 0, "error exit must release the subtask; calls=%v", calls)
	assert.Less(t, claim, release, "release after claim")
	assert.Equal(t, -1, indexOfCall(calls, "CompleteTask:SUB-1"), "failed subtask must not complete")
}

// TestExecuteSubtaskMaxTurnsNeverCompletes pins the invariant when the verify
// gate cannot confirm the work: a coder run truncated at the turn cap with an
// UNRESOLVED verify (isolateVerify's skip plan) is NOT pushed or marked done,
// and the claim is returned (the error-path release). The salvage gate requires
// a passing verify — a skip is not a pass — so the run parks; the WIP is still
// committed as resume evidence.
func TestExecuteSubtaskMaxTurnsNeverCompletes(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	call := llm.ToolCall{
		ID:       "c1",
		Type:     "function",
		Function: llm.FunctionCall{Name: "read", Arguments: `{"path":"no-such-file.txt"}`},
	}
	llmFake := &planLLM{responses: []llm.Response{{ToolCalls: []llm.ToolCall{call}}}}
	d := execTestDeps(ops, git, llmFake)
	d.Cfg.MaxTurns = 1

	// newExecRun's isolateVerify leaves the verify plan a skip (empty Argv), so
	// the salvage gate cannot confirm the work and the run parks.
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)
	err := runExecute(context.Background(), o)
	require.Error(t, err)

	var mte *MaxTurnsError
	require.ErrorAs(t, err, &mte)

	calls := ops.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "CompleteTask:SUB-1"), "unverified work marked done; calls=%v", calls)
	assert.Empty(t, git.pushBranches, "unverified work must not be pushed")
	assert.GreaterOrEqual(t, indexOfCall(calls, "ReleaseCard:SUB-1"), 0, "parked subtask claim must be released")
	// The WIP is committed as resume evidence even though the run parks.
	require.NotEmpty(t, git.commitMsgs, "the capped subtask commits its WIP as resume evidence")
	assert.True(t, ops.loggedContains("no verify command resolved"),
		"an unresolved-verify park is activity-logged; logs=%v", ops.logs)
}

func TestExecuteCoderPromptBody(t *testing.T) {
	t.Run("planner description reaches the coder prompt", func(t *testing.T) {
		ops := &fakeOps{}
		git := &fakeGit{committed: true}
		llmFake := &planLLM{responses: []llm.Response{finishResp("feat: x", 0.01)}}
		d := execTestDeps(ops, git, llmFake)

		o := newExecRun(d, []subtaskRef{{
			ID:    "SUB-1",
			Title: "Add health endpoint",
			Body:  "Files: internal/api/health.go\nAcceptance: GET /healthz returns 200",
			Tier:  "simple",
		}}, 0)
		require.NoError(t, runExecute(context.Background(), o))

		require.NotEmpty(t, llmFake.tasks)
		assert.Contains(t, llmFake.tasks[0], "Files: internal/api/health.go",
			"coder prompt must carry the planner's description")
		assert.Contains(t, llmFake.tasks[0], "Acceptance: GET /healthz returns 200")
	})

	t.Run("empty body falls back to title", func(t *testing.T) {
		ops := &fakeOps{}
		git := &fakeGit{committed: true}
		llmFake := &planLLM{responses: []llm.Response{finishResp("feat: x", 0.01)}}
		d := execTestDeps(ops, git, llmFake)

		// Resume-loaded refs lack bodies; the title stands in as the description.
		o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Add health endpoint", Tier: "simple"}}, 0)
		require.NoError(t, runExecute(context.Background(), o))

		require.NotEmpty(t, llmFake.tasks)
		assert.Contains(t, llmFake.tasks[0], "Description:\nAdd health endpoint",
			"empty body must fall back to the subtask title")
	})
}

func TestExecuteFirstPushLeasesStaleBranch(t *testing.T) {
	// Fresh run + stale remote branch: reconcile recorded staleRemoteTip, so the
	// FIRST subtask push overwrites the stale branch with a force-with-lease, and
	// every subsequent push is plain (the branch is now ours).
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &planLLM{responses: []llm.Response{
		finishResp("feat: one", 0.01),
		finishResp("feat: two", 0.01),
	}}
	d := execTestDeps(ops, git, llmFake)

	o := newExecRun(d, []subtaskRef{
		{ID: "SUB-1", Title: "First", Tier: "simple"},
		{ID: "SUB-2", Title: "Second", Tier: "simple"},
	}, 0)
	// Simulate what reconcile records on a fresh run with a stale remote branch.
	o.staleRemoteTip = "stale-tip"

	require.NoError(t, runExecute(context.Background(), o))

	// First push is a lease push carrying the recorded tip to the git layer.
	require.Len(t, git.leaseBranches, 1, "exactly one lease push (the first); git=%v", git.recorded())
	assert.Equal(t, "cm/card-1", git.leaseBranches[0])
	require.Len(t, git.leaseTips, 1)
	assert.Equal(t, "stale-tip", git.leaseTips[0], "the reconcile-recorded tip must reach ForcePushWithLease")

	// Second push is plain.
	require.Len(t, git.pushBranches, 1, "second push must be plain; git=%v", git.recorded())
	assert.Equal(t, "cm/card-1", git.pushBranches[0])

	// Lease comes before the plain push.
	git.assertOrder(t, "ForcePushWithLease:cm/card-1", "Push:cm/card-1")
}

func TestExecutePlainPushWhenNoStaleTip(t *testing.T) {
	// No stale remote branch (staleRemoteTip ""): every push is plain, no lease.
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &planLLM{responses: []llm.Response{finishResp("feat: x", 0.01)}}
	d := execTestDeps(ops, git, llmFake)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)

	require.NoError(t, runExecute(context.Background(), o))

	assert.Empty(t, git.leaseBranches, "no lease push without a stale remote branch")
	require.Len(t, git.pushBranches, 1)
	assert.Equal(t, "cm/card-1", git.pushBranches[0])
}

func TestExecuteCleanTreeSkipsPush(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: false} // clean tree: nothing committed
	llmFake := &planLLM{responses: []llm.Response{
		finishResp("chore: noop", 0.02),
	}}
	d := execTestDeps(ops, git, llmFake)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)
	require.NoError(t, runExecute(context.Background(), o))

	gitCalls := git.recorded()
	assert.GreaterOrEqual(t, indexOfCall(gitCalls, "CommitWithMessage"), 0)
	assert.Equal(t, -1, indexOfCall(gitCalls, "Push:cm/card-1"), "clean tree must skip push; git=%v", gitCalls)

	// Subtask still completes.
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "CompleteTask:SUB-1"), 0)
}

func TestExecuteModelSelectionPin(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &planLLM{responses: []llm.Response{finishResp("feat: x", 0.01)}}
	d := execTestDeps(ops, git, llmFake)
	// The card pins a catalog-resolvable coder model.
	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body", ModelCoder: "pinned/model"}
	d.Cfg.MaxCardCost = 0
	o := newRun(d, tc)
	isolateVerify(o)
	o.subtasks = []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "complex"}}

	require.NoError(t, runExecute(context.Background(), o))

	require.NotEmpty(t, llmFake.models)
	assert.Equal(t, "pinned/model", llmFake.models[0], "harness must run on the pinned coder model")
}

func TestExecuteModelSelectionByComplexity(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &planLLM{responses: []llm.Response{finishResp("feat: x", 0.01)}}

	// A registry where exactly one tools-capable model has a prior coder score
	// that clears every tier bar, so SelectByComplexity is forced to pick it.
	catalog := llm.Catalog{
		{ID: "the/coder", ContextLength: 200000, SupportedParameters: []string{"tools"}},
	}
	coderScore := 0.95
	priors := registry.Priors{
		Models: map[string]registry.PriorEntry{
			"the/coder": {Coder: &coderScore},
		},
	}
	reg := registry.NewRegistryFromParts(catalog, priors, nil, nil, "fallback/default")

	d := execTestDeps(ops, git, llmFake)
	d.Registry = reg
	// No coder pin -> complexity selection path.
	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body"}
	d.Cfg.MaxCardCost = 0
	o := newRun(d, tc)
	isolateVerify(o)
	o.subtasks = []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "moderate"}}

	require.NoError(t, runExecute(context.Background(), o))

	require.NotEmpty(t, llmFake.models)
	assert.Equal(t, "the/coder", llmFake.models[0],
		"with no pin the coder model must come from SelectByComplexity")
}

func TestExecuteWindowEstimatePositive(t *testing.T) {
	// estimateTokens must produce a positive budget (chars/4 + fixed overhead),
	// so the empty-prompt floor alone is already > 0.
	assert.Positive(t, estimateTokens(""))
	assert.Greater(t, estimateTokens("some longer prompt"), estimateTokens(""))
}

func TestExecuteSkipsDone(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &planLLM{responses: []llm.Response{finishResp("feat: x", 0.01)}}
	d := execTestDeps(ops, git, llmFake)

	// SUB-1 is already done (resume); SUB-2 is fresh and must run.
	o := newExecRun(d, []subtaskRef{
		{ID: "SUB-1", Title: "Done one", Tier: "simple", State: "done"},
		{ID: "SUB-2", Title: "Fresh one", Tier: "simple", State: "todo"},
	}, 0)

	require.NoError(t, runExecute(context.Background(), o))

	calls := ops.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "ClaimCard:SUB-1"), "done subtask must not be claimed")
	assert.GreaterOrEqual(t, indexOfCall(calls, "ClaimCard:SUB-2"), 0, "fresh subtask must run")
	assert.GreaterOrEqual(t, indexOfCall(calls, "CompleteTask:SUB-2"), 0)
}

func TestExecuteCommitMessage(t *testing.T) {
	t.Run("commit message resolved from finish call", func(t *testing.T) {
		ops := &fakeOps{}
		git := &fakeGit{committed: true}
		llmFake := &planLLM{responses: []llm.Response{
			finishResp("feat(api): add health endpoint", 0.01),
		}}
		d := execTestDeps(ops, git, llmFake)

		o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Add health endpoint", Tier: "simple"}}, 0)
		require.NoError(t, runExecute(context.Background(), o))

		require.NotEmpty(t, git.commitMsgs)
		assert.Equal(t, "feat(api): add health endpoint", git.commitMsgs[0])
	})

	t.Run("garbage handoff falls back to sanitized title", func(t *testing.T) {
		ops := &fakeOps{}
		git := &fakeGit{committed: true}
		llmFake := &planLLM{responses: []llm.Response{
			stopResp("all done, no commit line here", 0.01),
		}}
		d := execTestDeps(ops, git, llmFake)

		o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Add Health Endpoint", Tier: "simple"}}, 0)
		require.NoError(t, runExecute(context.Background(), o))

		require.NotEmpty(t, git.commitMsgs)
		// Sanitized-title fallback: lowercase conventional-ish "feat: <title>".
		assert.Equal(t, "feat: add health endpoint", git.commitMsgs[0])
	})
}

func TestExecuteBudget(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	// Subtask 1 spends 0.60; cap is 1.00 but seeded at 0.50 already, so after
	// subtask 1 the ledger is at 1.10 — subtask 2's pre-claim Check trips.
	llmFake := &planLLM{responses: []llm.Response{
		finishResp("feat: one", 0.60),
		finishResp("feat: two", 0.60),
	}}
	d := execTestDeps(ops, git, llmFake)
	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body", ReportedCostUSD: 0.50}
	d.Cfg.MaxCardCost = 1.00
	o := newRun(d, tc)
	isolateVerify(o)
	o.subtasks = []subtaskRef{
		{ID: "SUB-1", Title: "One", Tier: "simple"},
		{ID: "SUB-2", Title: "Two", Tier: "simple"},
	}

	err := runExecute(context.Background(), o)

	var be *BudgetExceededError
	require.ErrorAs(t, err, &be)

	calls := ops.recorded()
	assert.GreaterOrEqual(t, indexOfCall(calls, "ClaimCard:SUB-1"), 0, "subtask 1 ran")
	assert.Equal(t, -1, indexOfCall(calls, "ClaimCard:SUB-2"), "subtask 2 must never be claimed")
}

func TestExecuteClaimFailureAborts(t *testing.T) {
	ops := &fakeOps{claimErr: errors.New("claim conflict")}
	git := &fakeGit{committed: true}
	llmFake := &planLLM{responses: []llm.Response{finishResp("feat: x", 0.01)}}
	d := execTestDeps(ops, git, llmFake)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)
	err := runExecute(context.Background(), o)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "claim")

	// No model call once the claim failed.
	assert.Empty(t, llmFake.models, "harness must not run after a claim failure")
}

func TestExecutePushFailureAborts(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, pushErr: errors.New("remote rejected")}
	llmFake := &planLLM{responses: []llm.Response{finishResp("feat: x", 0.01)}}
	d := execTestDeps(ops, git, llmFake)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)
	err := runExecute(context.Background(), o)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "push")

	// Money was already reported before the failed push.
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "ReportUsage:SUB-1"), 0)
	// The subtask does not complete after a push failure.
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "CompleteTask:SUB-1"))
}

func TestSanitizeTitle(t *testing.T) {
	assert.Equal(t, "feat: add the thing", sanitizeTitle("Add the Thing"))
	assert.Equal(t, "feat: untitled", sanitizeTitle("   "))
}

// guard: the coder prompt template must reference the branch-state note and
// instruct the model to end the subtask by calling the finish tool.
func TestCoderPromptShape(t *testing.T) {
	low := strings.ToLower(coderPrompt)
	assert.Contains(t, low, "finish tool")
	assert.NotContains(t, low, "commit:")
	assert.Contains(t, low, "branch")
}

// burnResp is a tool-call turn that never lets the run stop on its own;
// content rides along so cap-path tests can inspect the final output.
func burnResp(content string) llm.Response {
	return llm.Response{
		Content: content,
		ToolCalls: []llm.ToolCall{{
			ID: "b", Type: "function",
			Function: llm.FunctionCall{Name: "read", Arguments: `{"path":"missing"}`},
		}},
	}
}

// burnResps returns n burn turns (see burnResp) for cap/budget tests.
func burnResps(n int) []llm.Response {
	rs := make([]llm.Response, n)
	for i := range rs {
		rs[i] = burnResp("")
	}

	return rs
}

func TestCoderRunGetsWrapUpNudge(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	// Three burn turns, then the exhausted fallback (stop) ends the run: with
	// MaxTurns=8 the nudge fires after 8-5=3 consumed turns.
	client := &planLLM{responses: []llm.Response{burnResp(""), burnResp(""), burnResp("")}}

	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 8
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	require.NoError(t, runExecute(context.Background(), o))

	joined := strings.Join(client.tasks, "\n")
	assert.Contains(t, joined, coderWrapUpMessage,
		"the wrap-up nudge reaches the coder conversation as a user message")
}

// TestCoderRunTierScalesTurnBudget proves a complex subtask lifts the coder turn
// budget above the flat base: 25 turns (more than the base of 20, fewer than the
// complex budget of 30 = 1.5x base) run to completion instead of capping mid-way.
func TestCoderRunTierScalesTurnBudget(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: burnResps(25)}

	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 20
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "complex"}}, 0)

	require.NoError(t, runExecute(context.Background(), o),
		"a complex subtask scales the coder budget above the base, so 25 turns do not cap")
}

// TestCoderRunSimpleTierCapsAtBase proves a simple subtask is NOT scaled: the
// same 25 turns cap at the flat base of 20, parking the single-solver run.
func TestCoderRunSimpleTierCapsAtBase(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: burnResps(25)}

	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 20
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	err := runExecute(context.Background(), o)
	require.Error(t, err, "a simple subtask keeps the flat base, so 25 turns cap")

	var mte *MaxTurnsError
	require.ErrorAs(t, err, &mte)
}

// TestSalvageCappedFinalSubtask proves a turn-capped final subtask is still
// committed and the solver marked capped, not dropped. A genuinely capped run
// never calls finish (a successful finish call would end the run cleanly
// before the cap ever trips), so res.CompletionArgs is always empty here —
// the salvage commit message is the sanitized-title fallback, proving the
// salvage path no longer scrapes free text for a commit message.
func TestSalvageCappedFinalSubtask(t *testing.T) {
	ops := &fakeOps{}
	// Five burn turns == MaxTurns(5) from execTestDeps: the coder run caps
	// without ever calling finish.
	client := &planLLM{responses: []llm.Response{
		burnResp(""), burnResp(""), burnResp(""), burnResp(""),
		burnResp("wrapping up"),
	}}
	d := execTestDeps(ops, &fakeGit{committed: true}, client)
	// execTestDeps defaults MaxTurns to 20 (avoids the wrapUpTurns==MaxTurns
	// nudge-at-turn-0 quirk for unrelated fixtures); this test scripts exactly
	// 5 burn turns and needs the cap to trip on the 5th.
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, nil, 0)

	cg := &fakeGit{committed: true}
	sc := &solverCtx{
		git: cg, ledger: NewLedger(0, 0), tools: d.WriteTools,
		workspace: "ws", coderModel: o.resolveCoderModel,
		boardOps: false, push: false, tag: "candidate 1/1",
		lastSubID: "SUB-2",
	}

	sub := subtaskRef{ID: "SUB-2", Title: "Final", Tier: "simple"}
	require.NoError(t, o.executeSubtaskWith(context.Background(), sc, sub),
		"a cap on the final subtask is salvaged, not dropped")

	assert.True(t, sc.capped, "the solver is marked capped")
	require.NotEmpty(t, cg.commitMsgs, "the worktree is committed")
	assert.Equal(t, "feat: final", cg.commitMsgs[len(cg.commitMsgs)-1],
		"a capped run never called finish, so the salvage commit uses the sanitized-title fallback, not the trailing prose")
	require.Len(t, sc.completed, 1)
	assert.Equal(t, "SUB-2", sc.completed[0].ID, "the salvaged subtask counts as completed for winner replay")
	assert.True(t, ops.loggedContains("turn cap on final subtask SUB-2"), "logs=%v", ops.logs)
}

// TestNoSalvageOnCleanTree proves a capped candidate whose final-subtask tree
// is clean (nothing to commit) is NOT salvaged: CommitWithMessage returning
// (false, nil) means there is no diff, and an empty tree carries no completion
// evidence for the judge's verify gate to ride on.
func TestNoSalvageOnCleanTree(t *testing.T) {
	ops := &fakeOps{}
	// Five burn turns == MaxTurns(5) from execTestDeps: the coder run caps.
	client := &planLLM{responses: []llm.Response{
		burnResp(""), burnResp(""), burnResp(""), burnResp(""),
		burnResp("wrapping up"),
	}}
	d := execTestDeps(ops, &fakeGit{committed: true}, client)
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, nil, 0)

	cg := &fakeGit{committed: false}
	sc := &solverCtx{
		git: cg, ledger: NewLedger(0, 0), tools: d.WriteTools,
		workspace: "ws", coderModel: o.resolveCoderModel,
		boardOps: false, push: false, tag: "candidate 1/1",
		lastSubID: "SUB-2",
	}

	sub := subtaskRef{ID: "SUB-2", Title: "Final", Tier: "simple"}
	err := o.executeSubtaskWith(context.Background(), sc, sub)
	require.Error(t, err, "a clean tree on the final subtask must not be salvaged")

	var mte *MaxTurnsError
	require.ErrorAs(t, err, &mte)
	assert.False(t, sc.capped, "the solver is not marked capped when nothing was committed")
	assert.Empty(t, sc.completed, "no subtask counts as completed when salvage is refused")
}

func TestSalvageFallsBackToTitleCommitMessage(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{
		burnResp(""), burnResp(""), burnResp(""), burnResp(""), burnResp("no commit line here"),
	}}
	d := execTestDeps(ops, &fakeGit{committed: true}, client)
	// execTestDeps defaults MaxTurns to 20; this test scripts exactly 5 burn
	// turns and needs the cap to trip on the 5th.
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, nil, 0)

	cg := &fakeGit{committed: true}
	sc := &solverCtx{
		git: cg, ledger: NewLedger(0, 0), tools: d.WriteTools,
		workspace: "ws", coderModel: o.resolveCoderModel,
		boardOps: false, push: false, tag: "candidate 1/1",
		lastSubID: "SUB-2",
	}

	require.NoError(t, o.executeSubtaskWith(context.Background(), sc,
		subtaskRef{ID: "SUB-2", Title: "Final", Tier: "simple"}))
	require.NotEmpty(t, cg.commitMsgs)
	assert.Equal(t, "feat: final", cg.commitMsgs[len(cg.commitMsgs)-1])
}

func TestNoSalvageOnEarlierSubtask(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{
		burnResp(""), burnResp(""), burnResp(""), burnResp(""), burnResp(""),
	}}
	d := execTestDeps(ops, &fakeGit{committed: true}, client)
	// execTestDeps defaults MaxTurns to 20; this test scripts exactly 5 burn
	// turns and needs the cap to trip on the 5th.
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, nil, 0)

	cg := &fakeGit{committed: true}
	sc := &solverCtx{
		git: cg, ledger: NewLedger(0, 0), tools: d.WriteTools,
		workspace: "ws", coderModel: o.resolveCoderModel,
		boardOps: false, push: false, tag: "candidate 1/1",
		lastSubID: "SUB-9", // the capped subtask is NOT the final one
	}

	err := o.executeSubtaskWith(context.Background(), sc, subtaskRef{ID: "SUB-2", Title: "Mid", Tier: "simple"})
	require.Error(t, err, "a cap on an earlier subtask still drops the candidate")

	var mte *MaxTurnsError
	require.ErrorAs(t, err, &mte)
	assert.False(t, sc.capped)
	assert.Empty(t, cg.commitMsgs, "nothing is committed for a non-final cap")
}

// TestSoloTurnCapSalvagedWhenVerifyPasses proves the single-solver (parent /
// mob session) rescue: a capped subtask whose committed work passes the authoritative
// verify completes exactly like a finish-terminated run — pushed and marked
// done — instead of parking. The single solver has no judge, so the verify runs
// inline and is the completion authority.
func TestSoloTurnCapSalvagedWhenVerifyPasses(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true} // a dirty tree the salvage commit captures
	// Five burn turns == MaxTurns(5): the coder run caps without ever calling finish.
	client := &planLLM{responses: burnResps(5)}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	// A non-empty resolved plan (so the gate is not vacuous) whose authoritative
	// verify passes.
	seedResolvedVerifyPlan(o)
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 0} // pass
	}

	require.NoError(t, runExecute(context.Background(), o),
		"a capped single-solver subtask whose committed work passes verify is salvaged as complete")

	calls := ops.recorded()
	assert.GreaterOrEqual(t, indexOfCall(calls, "CompleteTask:SUB-1"), 0,
		"the verified subtask completes; calls=%v", calls)
	assert.Equal(t, -1, indexOfCall(calls, "ReleaseCard:SUB-1"),
		"a salvaged subtask is completed, not released")
	require.NotEmpty(t, git.pushBranches, "salvaged work is pushed")
	assert.Equal(t, "cm/card-1", git.pushBranches[0])
	assert.True(t, ops.loggedContains("passed the authoritative verify"),
		"the salvage is activity-logged; logs=%v", ops.logs)
}

// TestCoderGraceTurnFinishes proves the grace turn is the first net at the cap:
// a coder that dithers past the wrap-up nudge to the turn cap but is actually
// done lands `finish` in the harness's terminal-only grace call, completing the
// subtask through the NORMAL finish path — pushed and marked done via the finish
// commit message — WITHOUT touching the verify-gated salvage path. No verify is
// stubbed here: the grace finish never consults it.
func TestCoderGraceTurnFinishes(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	// Five burn turns == MaxTurns(5) caps the main loop; the sixth response is
	// consumed by the grace call, which lands finish before max_turns is returned.
	client := &planLLM{responses: append(burnResps(5), finishResp("feat: done", 0.01))}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	// A simple tier keeps coderMaxTurns at the flat base (5), so the cap trips on
	// the fifth burn and the grace call fires on the sixth response.
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	// Deliberately NO seedResolvedVerifyPlan / o.runVerify stub: a grace finish
	// completes without the salvage path's authoritative verify.

	require.NoError(t, runExecute(context.Background(), o),
		"a coder that lands finish in the grace call completes like a normal finish")

	calls := ops.recorded()
	assert.GreaterOrEqual(t, indexOfCall(calls, "CompleteTask:SUB-1"), 0,
		"the grace-finished subtask completes; calls=%v", calls)
	assert.Equal(t, -1, indexOfCall(calls, "ReleaseCard:SUB-1"),
		"a completed subtask is not released")
	require.NotEmpty(t, git.pushBranches, "grace-finished work is pushed")
	assert.Equal(t, "cm/card-1", git.pushBranches[0])

	// The commit carries the grace finish's own message — not the sanitized-title
	// fallback the salvage path uses — proving completion ran through finish.
	require.NotEmpty(t, git.commitMsgs)
	assert.Equal(t, "feat: done", git.commitMsgs[len(git.commitMsgs)-1])

	// No salvage advisory: the run finished through the grace call, not the
	// verify-gated turn-cap salvage.
	assert.False(t, ops.loggedContains("passed the authoritative verify"),
		"a grace finish must not log the salvage advisory; logs=%v", ops.logs)
	assert.False(t, ops.loggedContains("turn cap"),
		"a grace finish must not log any turn-cap advisory; logs=%v", ops.logs)
}

// TestSoloTurnCapStillParksWhenVerifyFails proves the gate is inviolable: a
// capped subtask whose committed work FAILS the authoritative verify parks
// (MaxTurnsError) — it is not completed and not pushed — and the commit stays as
// WIP evidence for resume.
func TestSoloTurnCapStillParksWhenVerifyFails(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: burnResps(5)}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	seedResolvedVerifyPlan(o)
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 1, Output: "FAIL"} // fail
	}

	err := runExecute(context.Background(), o)
	require.Error(t, err, "a capped subtask whose committed work fails verify parks")

	var mte *MaxTurnsError
	require.ErrorAs(t, err, &mte)

	calls := ops.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "CompleteTask:SUB-1"), "a failed verify must not complete the subtask")
	assert.Empty(t, git.pushBranches, "failed-verify work must not be pushed")
	assert.GreaterOrEqual(t, indexOfCall(calls, "ReleaseCard:SUB-1"), 0, "the parked claim is released")
	assert.True(t, ops.loggedContains("verify did not pass"), "the park is activity-logged; logs=%v", ops.logs)
}

// TestSoloTurnCapStillParksOnCleanTree proves a clean tree is never salvaged:
// CommitWithMessage reporting (false, nil) means there is no diff — the only
// completion evidence a capped run has — so even a passing verify cannot rescue
// it and the run parks.
func TestSoloTurnCapStillParksOnCleanTree(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: false} // clean tree: nothing committed
	client := &planLLM{responses: burnResps(5)}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	// Even a passing verify cannot rescue a clean tree: nothing was committed.
	seedResolvedVerifyPlan(o)
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 0}
	}

	err := runExecute(context.Background(), o)
	require.Error(t, err, "a clean tree carries no completion evidence, so the cap parks")

	var mte *MaxTurnsError
	require.ErrorAs(t, err, &mte)

	calls := ops.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "CompleteTask:SUB-1"), "a clean tree must not complete")
	assert.Empty(t, git.pushBranches, "a clean tree must not push")
	assert.GreaterOrEqual(t, indexOfCall(calls, "ReleaseCard:SUB-1"), 0, "the parked claim is released")
}

// The former TestNoSalvageForParentSolver asserted the single-solver path always
// parked on the cap with no commit. That is no longer the contract: a capped
// single-solver subtask now commits its WIP and salvages it when the
// authoritative verify passes (salvageSoloCapped). Its coverage moved to
// TestSoloTurnCapSalvagedWhenVerifyPasses / ...StillParksWhenVerifyFails /
// ...StillParksOnCleanTree, plus TestExecuteSubtaskMaxTurnsNeverCompletes for the
// unresolved-verify park.
