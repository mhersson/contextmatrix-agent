package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"runtime"
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
	tc := cmclient.TaskContext{Title: "Parent card", Description: "parent body"}
	o := newRun(d, tc)
	o.subtasks = subs
	// Pre-resolve a skip plan so runExecute's ensureVerify is a cached no-op -
	// execute tests exercise the coder loop, not verify resolution.
	isolateVerify(o)

	return o
}

func TestTopoOrder(t *testing.T) {
	t.Run("dependencies run first", func(t *testing.T) {
		// C depends on B, B depends on A - declared out of creation order to prove
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
// canceled, then return ctx.Err() - mirroring a well-behaved Ops transport.
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
// tick is blocked mid-call - but only because the Ops implementation honors
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
// a passing verify - a skip is not a pass - so the run parks; the WIP is still
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
	tc := cmclient.TaskContext{Title: "Parent", Description: "body", ModelCoder: "pinned/model"}
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
	tc := cmclient.TaskContext{Title: "Parent", Description: "body"}
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

func TestExecuteSkipsNotPlanned(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &planLLM{responses: []llm.Response{finishResp("feat: x", 0.01)}}
	d := execTestDeps(ops, git, llmFake)

	// SUB-1 is not_planned (cancelled); SUB-2 is fresh and must run.
	o := newExecRun(d, []subtaskRef{
		{ID: "SUB-1", Title: "Cancelled task", Tier: "simple", State: "not_planned"},
		{ID: "SUB-2", Title: "Fresh task", Tier: "simple", State: "todo"},
	}, 0)

	require.NoError(t, runExecute(context.Background(), o))

	calls := ops.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "ClaimCard:SUB-1"), "not_planned subtask must not be claimed")
	assert.Equal(t, -1, indexOfCall(calls, "ReportUsage:SUB-1"), "not_planned subtask must not be executed")
	assert.Equal(t, -1, indexOfCall(calls, "CompleteTask:SUB-1"), "not_planned subtask must not be completed")
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
	// subtask 1 the ledger is at 1.10 - subtask 2's pre-claim Check trips.
	llmFake := &planLLM{responses: []llm.Response{
		finishResp("feat: one", 0.60),
		finishResp("feat: two", 0.60),
	}}
	d := execTestDeps(ops, git, llmFake)
	tc := cmclient.TaskContext{Title: "Parent", Description: "body", ReportedCostUSD: 0.50}
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

func TestCommitSubject(t *testing.T) {
	t.Run("multi-line returns first line", func(t *testing.T) {
		got := commitSubject("feat(x): summary\n\nbody details", "Fallback")
		assert.Equal(t, "feat(x): summary", got)
	})

	t.Run("blank message falls back to sanitized title", func(t *testing.T) {
		got := commitSubject("", "Add the Thing")
		assert.Equal(t, "feat: add the thing", got)
	})

	t.Run("whitespace-only message falls back to sanitized title", func(t *testing.T) {
		got := commitSubject("   \n  ", "Add the Thing")
		assert.Equal(t, "feat: add the thing", got)
	})

	t.Run("windows line endings produce correct first line", func(t *testing.T) {
		got := commitSubject("feat(x): summary\r\n\r\nbody details", "Fallback")
		assert.Equal(t, "feat(x): summary", got)
	})

	t.Run("single-line message returns itself trimmed", func(t *testing.T) {
		got := commitSubject("  fix: a quick fix  ", "Fallback")
		assert.Equal(t, "fix: a quick fix", got)
	})

	t.Run("empty fallback title yields feat: untitled", func(t *testing.T) {
		got := commitSubject("", "")
		assert.Equal(t, "feat: untitled", got)
	})
}

// TestExecuteCommitMessageLongFinish proves a finish message with a very long
// first line (or a long multi-line message) does not trigger a length error
// from CompleteTask: the commit subject is the first line only, so an
// arbitrarily long commit message in git does not become a board write of the
// same length.
func TestExecuteCommitMessageLongFinish(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	// A >2000-byte commit message: the first line is ~3014 bytes (well over
	// the server's cap), but commitSubject extracts only the subject line so
	// the argument to CompleteTask stays short; the full message still reaches
	// git (trimmed of trailing whitespace by finishCommitMessage).
	longLine := "feat(api): " + strings.Repeat("a", 3000)
	body := strings.Repeat("long line\n", 500)
	msg := longLine + "\n\n" + body[:len(body)-1] // no trailing newline so TrimSpace is a no-op on the body
	llmFake := &planLLM{responses: []llm.Response{
		finishResp(msg, 0.01),
	}}
	d := execTestDeps(ops, git, llmFake)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Add health endpoint", Tier: "simple"}}, 0)
	require.NoError(t, runExecute(context.Background(), o),
		"a very long commit message must not cause a length error from CompleteTask")

	// The full message reaches git (CommitWithMessage).
	require.NotEmpty(t, git.commitMsgs)
	assert.Equal(t, msg, git.commitMsgs[0],
		"the full commit message must still reach git unchanged")

	// The subject passed to CompleteTask is the first line only.
	calls := ops.recorded()
	require.GreaterOrEqual(t, indexOfCall(calls, "CompleteTask:SUB-1"), 0,
		"the subtask must complete; calls=%v", calls)

	summaries := ops.completeTaskSummaries
	require.GreaterOrEqual(t, len(summaries), 1, "CompleteTask must have been called at least once")
	assert.Equal(t, longLine, summaries[0],
		"CompleteTask must receive the commit subject (first line), not the full message")
	assert.NotContains(t, summaries[0], "\n",
		"CompleteTask summary must not contain a newline; only the first line is the subject")
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

// TestCoderRunGetsBatchNudge proves the coder phase actually arms the harness
// batching nudge: three consecutive turns that each spend a whole model call on
// one read-only lookup earn exactly one nudge, naming the three turns that
// triggered it. The mechanism ships in the harness disabled - a zero threshold
// counts nothing - so this fails whenever the coder config stops setting one,
// which is the state the phase was in before this test existed.
func TestCoderRunGetsBatchNudge(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	// Three single-read turns arm the nudge; the fourth call falls through to
	// the fake's default stop response, ending the run after the injection.
	client := &planLLM{responses: burnResps(3)}

	d := execTestDeps(ops, git, client)
	d.Emit = events.NewEmitter(nil, &transcript)
	// The base MaxTurns of 20 keeps the wrap-up nudge (which fires at 20-5=15
	// turns) well clear: a run already told to finish suppresses the batching
	// nudge for the rest of the run.
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	require.NoError(t, runExecute(context.Background(), o))

	assert.Equal(t, []int{3}, batchNudgeCounts(t, &transcript),
		"the coder is nudged once, after three consecutive single-lookup turns")
}

// TestCoderRunNoBatchNudgeBelowThreshold pins the configured threshold itself:
// two consecutive single-lookup turns are one short, so nothing fires. Paired
// with TestCoderRunGetsBatchNudge it brackets the value from both sides, making
// any later change to it a visible decision rather than silent drift.
func TestCoderRunNoBatchNudgeBelowThreshold(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: burnResps(2)}

	d := execTestDeps(ops, git, client)
	d.Emit = events.NewEmitter(nil, &transcript)
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	require.NoError(t, runExecute(context.Background(), o))

	assert.Empty(t, batchNudgeCounts(t, &transcript),
		"two single-lookup turns are below the threshold and earn no nudge")
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
// before the cap ever trips), so res.CompletionArgs is always empty here -
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
// verify completes exactly like a finish-terminated run - pushed and marked
// done - instead of parking. The single solver has no judge, so the verify runs
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
// subtask through the NORMAL finish path - pushed and marked done via the finish
// commit message - WITHOUT touching the verify-gated salvage path. No verify is
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

	// The commit carries the grace finish's own message - not the sanitized-title
	// fallback the salvage path uses - proving completion ran through finish.
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
// (MaxTurnsError) - it is not completed and not pushed - and the commit stays as
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

// TestSoloTurnCapParkNamesClassification proves the park reason distinguishes a
// skip-classified verify (here, a timeout - environmental, not a code defect)
// from a plain failure by naming the classification note on the card log.
func TestSoloTurnCapParkNamesClassification(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: burnResps(5)}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	seedResolvedVerifyPlan(o)
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{TimedOut: true, ExitCode: -1}
	}

	err := runExecute(context.Background(), o)
	require.Error(t, err, "a timed-out verify still parks the capped subtask")

	var mte *MaxTurnsError
	require.ErrorAs(t, err, &mte)

	assert.True(t, ops.loggedContains("verify timed out"),
		"the park reason names the timeout classification; logs=%v", ops.logs)
	assert.False(t, ops.loggedContains("verify did not pass (verify timed out"),
		"the note already states its own verdict; wrapping it in \"verify did not pass (...)\" would say it twice; logs=%v", ops.logs)
}

// TestSoloTurnCapStillParksOnCleanTree proves a clean tree is never salvaged:
// CommitWithMessage reporting (false, nil) means there is no diff - the only
// completion evidence a capped run has - so even a passing verify cannot rescue
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

// TestSalvageDeclineEscalatesTierAndReportsFailed proves a solo turn-cap park
// that fails the authoritative verify (never salvaged) bumps the subtask's
// persisted tier marker one step - so a resumed run selects a stronger model
// against a bigger cap instead of repeating the same losing attempt - and
// reports a "failed" outcome with VerifyPass false: the run did not complete,
// and the verify never confirmed the work.
func TestSalvageDeclineEscalatesTierAndReportsFailed(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: burnResps(5)}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "moderate"}}, 0)
	ops.taskContext = cmclient.TaskContext{
		Description: withTierMarker("Implement the thing.", "moderate"),
	}

	seedResolvedVerifyPlan(o)
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 1, Output: "FAIL"} // fail
	}

	err := runExecute(context.Background(), o)
	require.Error(t, err, "a capped subtask whose committed work fails verify parks")

	assert.Equal(t, "Implement the thing.\n\n<!-- cm:tier=complex -->", ops.bodyFor("SUB-1"),
		"the park escalates the marker one step, leaving the rest of the body unchanged")
	assert.True(t, ops.loggedContains("escalating subtask tier moderate -> complex"),
		"the escalation is activity-logged; logs=%v", ops.logs)

	require.Len(t, ops.reportOutcomes, 1)
	rows := ops.reportOutcomes[0]
	require.Len(t, rows, 1)
	assert.Equal(t, "failed", rows[0].Result)
	assert.False(t, rows[0].VerifyPass)
	assert.Equal(t, 1, rows[0].NCandidates)
}

// TestSalvageDeclineOnCleanTreeEscalatesTierAndReportsFailed proves the
// earliest decline branch, where a capped subtask committed nothing at all,
// carries the full park consequences too: the cap happened regardless of
// whether there was anything to salvage-commit, so resume still gets a bigger
// cap and a higher bar, and the leaderboard still gets the "failed" row.
func TestSalvageDeclineOnCleanTreeEscalatesTierAndReportsFailed(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: false} // clean tree: nothing committed
	client := &planLLM{responses: burnResps(5)}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)
	ops.taskContext = cmclient.TaskContext{
		Description: withTierMarker("Implement the thing.", "simple"),
	}

	err := runExecute(context.Background(), o)
	require.Error(t, err, "a clean tree carries no completion evidence, so the cap parks")

	assert.Equal(t, "Implement the thing.\n\n<!-- cm:tier=moderate -->", ops.bodyFor("SUB-1"),
		"the clean-tree decline still escalates the marker, leaving the rest of the body unchanged")
	assert.True(t, ops.loggedContains("escalating subtask tier simple -> moderate"),
		"the escalation is activity-logged; logs=%v", ops.logs)

	require.Len(t, ops.reportOutcomes, 1)
	rows := ops.reportOutcomes[0]
	require.Len(t, rows, 1)
	assert.Equal(t, "failed", rows[0].Result)
	assert.False(t, rows[0].VerifyPass, "verify was never reached on a clean tree")
}

// TestSalvageDeclineCriticalTierStaysCritical proves critical is the ceiling:
// there is nowhere left to escalate to, so the board write is skipped
// entirely (nothing to persist, nothing to announce), while the original park
// advisory still logs exactly as it would for any other tier.
func TestSalvageDeclineCriticalTierStaysCritical(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	// Critical scales coderMaxTurns to 2x the base (10), so it takes 10 burn
	// turns - not the base 5 - to trip the cap.
	client := &planLLM{responses: burnResps(10)}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "critical"}}, 0)
	ops.taskContext = cmclient.TaskContext{
		Description: withTierMarker("Implement the thing.", "critical"),
	}

	seedResolvedVerifyPlan(o)
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 1, Output: "FAIL"} // fail
	}

	err := runExecute(context.Background(), o)
	require.Error(t, err, "a capped subtask whose committed work fails verify parks")

	assert.Equal(t, -1, indexOfCall(ops.recorded(), "UpdateCardBody:SUB-1"),
		"critical is already the ceiling; no board write is needed")
	assert.Empty(t, ops.bodyFor("SUB-1"), "no marker write when the tier cannot escalate further")
	assert.True(t, ops.loggedContains("verify did not pass"), "the original park advisory still logs; logs=%v", ops.logs)
	assert.False(t, ops.loggedContains("escalating subtask tier"),
		"no escalation announcement when nothing escalated; logs=%v", ops.logs)
}

// TestSalvageDeclineAfterVerifyPassKeepsTierAndReportsNoOutcome proves the
// park consequences are asymmetric: once the authoritative verify has already
// passed, the model's work is proven correct within the cap, so a park caused
// by a downstream infrastructure failure (here, the push) must NOT escalate
// the tier - a resume at the SAME tier is the right economics, not a bigger
// model - and must NOT report an outcome either: a "failed" row would
// penalize the model at full weight for a fault that is not its own, the
// same environmental exemption a skipped verify gets.
func TestSalvageDeclineAfterVerifyPassKeepsTierAndReportsNoOutcome(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, pushErr: errors.New("push boom")}
	client := &planLLM{responses: burnResps(5)}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "moderate"}}, 0)
	ops.taskContext = cmclient.TaskContext{
		Description: withTierMarker("Implement the thing.", "moderate"),
	}

	seedResolvedVerifyPlan(o)
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 0} // pass
	}

	err := runExecute(context.Background(), o)
	require.Error(t, err, "a push failure after a passing verify still parks")

	var mte *MaxTurnsError
	require.ErrorAs(t, err, &mte)

	assert.Equal(t, -1, indexOfCall(ops.recorded(), "UpdateCardBody:SUB-1"),
		"a park after a passing verify must not touch the subtask body at all")
	assert.Empty(t, ops.bodyFor("SUB-1"), "no tier-marker write on a post-verify-pass park")
	assert.False(t, ops.loggedContains("escalating subtask tier"),
		"a post-verify-pass park must not log a tier escalation; logs=%v", ops.logs)
	assert.Empty(t, ops.reportOutcomes,
		"an infrastructure park after a passing verify is not evidence about the model")
	assert.True(t, ops.loggedContains("push"),
		"the push error must be recorded on the card log; logs=%v", ops.logs)
}

// TestSalvageDeclineAfterCompleteTaskFailKeepsTierAndWinRow is the sibling of
// TestSalvageDeclineAfterVerifyPassKeepsTierAndReportsNoOutcome for the OTHER
// post-verify-pass park cause: the push succeeds but the board's CompleteTask
// call fails. Same tier contract - the model's work is proven correct, so
// this is an infrastructure park that must not touch the tier marker. The
// outcome differs from the push case only because the win report fires
// BEFORE the doomed CompleteTask attempt (claim-gating requires it): exactly
// one "win" row stands, and the CompleteTask failure gets no second,
// contradictory report of its own.
func TestSalvageDeclineAfterCompleteTaskFailKeepsTierAndWinRow(t *testing.T) {
	ops := &fakeOps{completeTaskErr: errors.New("complete task boom")}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: burnResps(5)}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "moderate"}}, 0)
	ops.taskContext = cmclient.TaskContext{
		Description: withTierMarker("Implement the thing.", "moderate"),
	}

	seedResolvedVerifyPlan(o)
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 0} // pass
	}

	err := runExecute(context.Background(), o)
	require.Error(t, err, "a CompleteTask failure after a passing verify still parks")

	var mte *MaxTurnsError
	require.ErrorAs(t, err, &mte)

	assert.Equal(t, -1, indexOfCall(ops.recorded(), "UpdateCardBody:SUB-1"),
		"a park after a passing verify must not touch the subtask body at all")
	assert.Empty(t, ops.bodyFor("SUB-1"), "no tier-marker write on a post-verify-pass park")
	assert.False(t, ops.loggedContains("escalating subtask tier"),
		"a post-verify-pass park must not log a tier escalation; logs=%v", ops.logs)

	require.Len(t, ops.reportOutcomes, 1, "exactly one outcome row - no duplicate report on the CompleteTask failure")
	rows := ops.reportOutcomes[0]
	require.Len(t, rows, 1)
	assert.Equal(t, "win", rows[0].Result, "the verify already proved the work correct before CompleteTask failed")
	assert.True(t, rows[0].VerifyPass)
	assert.True(t, ops.loggedContains("CompleteTask"),
		"the CompleteTask error must be recorded on the card log; logs=%v", ops.logs)
}

// markerMidRunLLM wraps a scripted LLM and writes a toolchain marker file into
// dir the first time it is called, simulating a coder run that introduces
// project scaffolding (e.g. a pom.xml) mid-run: the run's FIRST verify
// resolution (before the coder loop starts) sees nothing, but a SECOND
// resolution after that point sees the marker.
type markerMidRunLLM struct {
	inner   llm.LLM
	t       *testing.T
	dir     string
	written bool
}

func (m *markerMidRunLLM) writeMarkerOnce() {
	if m.written {
		return
	}

	m.written = true

	writeFile(m.t, m.dir, "pom.xml", "<project></project>\n")
}

func (m *markerMidRunLLM) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	m.writeMarkerOnce()

	return m.inner.Send(ctx, req)
}

func (m *markerMidRunLLM) SendStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	m.writeMarkerOnce()

	return m.inner.SendStream(ctx, req, onDelta)
}

// TestSoloTurnCapPropagatesToolchainSentinel proves the salvage path's second
// ensureVerify call does not swallow a *ToolchainMissingError into a generic
// solo-cap park: when a toolchain marker is introduced mid-run (present only by
// the time salvage re-resolves verify, absent at runExecute's pre-loop
// resolution), the error escaping the execute phase is the toolchain sentinel -
// not MaxTurnsError - so it reaches execute()'s dedicated toolchain arm and the
// card-log line it writes.
func TestSoloTurnCapPropagatesToolchainSentinel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit probing is POSIX-only")
	}

	stubTools(t) // nothing on PATH: mvn cannot resolve

	dir := t.TempDir()

	ops := &fakeOps{}
	git := &fakeGit{committed: true} // a dirty tree the salvage commit captures
	inner := &planLLM{responses: burnResps(5)}
	client := &markerMidRunLLM{inner: inner, t: t, dir: dir}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	d.Cfg.Workspace = dir
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)
	// Drive the full execute()-FSM, not just the execute phase in isolation: the
	// card-log line this test asserts on is written by execute()'s dedicated
	// errors.As arm, not by runExecute itself. The plan phase already ran
	// (newExecRun pre-seeds o.subtasks), so it is stubbed to a no-op success.
	o.planFn = func(context.Context) error { return nil }

	err := o.execute(context.Background())
	require.Error(t, err, "an unresolvable mid-run toolchain must still park")

	var tme *ToolchainMissingError
	require.ErrorAs(t, err, &tme, "the toolchain sentinel, not MaxTurnsError, must escape the execute phase")
	assert.Equal(t, "detected", tme.Tier)
	assert.Equal(t, "maven project", tme.Subject)

	var mte *MaxTurnsError
	assert.NotErrorAs(t, err, &mte, "MaxTurnsError must not be the type observed by the caller")

	calls := ops.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "CompleteTask:SUB-1"), "an unresolvable toolchain must not complete the subtask")
	assert.Empty(t, git.pushBranches, "an unresolvable toolchain must not push")
	assert.GreaterOrEqual(t, indexOfCall(calls, "ReleaseCard:SUB-1"), 0, "the parked claim is released")

	// execute()'s dedicated toolchain arm writes this line - confirms the
	// sentinel actually reached it rather than being logged generically by
	// logSoloCapPark ("verify could not be resolved").
	assert.True(t, ops.loggedContains("parking card as blocked"),
		"the toolchain-missing park must be activity-logged via execute()'s dedicated arm; logs=%v", ops.logs)
	assert.False(t, ops.loggedContains("verify could not be resolved"),
		"a toolchain sentinel must not be logged as a generic unresolved-verify park; logs=%v", ops.logs)
}

// TestSoloTurnCapPropagatesContainerRuntimeSentinel proves the salvage path's
// authoritative runVerifyPlan call does not swallow a container-runtime park
// into a generic "did not pass": when the verify command's own output carries
// an unreachable-container-runtime signature, the error escaping the execute
// phase is the toolchain sentinel with the runtime-tier marker - not
// MaxTurnsError - so it reaches execute()'s dedicated toolchain arm and the
// card-log line it writes, exactly like the verify-resolution-time sentinel
// above.
func TestSoloTurnCapPropagatesContainerRuntimeSentinel(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true} // a dirty tree the salvage commit captures
	client := &planLLM{responses: burnResps(5)}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)
	// Drive the full execute()-FSM: the card-log line asserted below is written
	// by execute()'s dedicated errors.As arm, not by runExecute itself.
	o.planFn = func(context.Context) error { return nil }

	seedResolvedVerifyPlan(o)
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{
			ExitCode: 1,
			Output:   "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
		}
	}

	err := o.execute(context.Background())
	require.Error(t, err, "an unreachable container runtime discovered during the authoritative verify must still park")

	var tme *ToolchainMissingError
	require.ErrorAs(t, err, &tme, "the toolchain sentinel, not MaxTurnsError, must escape the execute phase")
	assert.Equal(t, "runtime", tme.Tier)
	assert.Equal(t, containerRuntimeUnavailableMarker, tme.Subject)

	var mte *MaxTurnsError
	assert.NotErrorAs(t, err, &mte, "MaxTurnsError must not be the type observed by the caller")

	calls := ops.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "CompleteTask:SUB-1"), "an unreachable container runtime must not complete the subtask")
	assert.Empty(t, git.pushBranches, "an unreachable container runtime must not push")
	assert.GreaterOrEqual(t, indexOfCall(calls, "ReleaseCard:SUB-1"), 0, "the parked claim is released")

	assert.True(t, ops.loggedContains("container runtime"),
		"the container-runtime park must be activity-logged via execute()'s dedicated arm; logs=%v", ops.logs)
	assert.False(t, ops.loggedContains("verify did not pass"),
		"a container-runtime sentinel must not be logged as a generic did-not-pass park; logs=%v", ops.logs)
}

// TestFakeOpsReportModelOutcomesEnforcesContract proves fakeOps validates
// every row against CM's actual admission contract (relaxed to n_candidates
// >= 1 by the solo-outcome fix) rather than accepting anything: a caller that
// ever assembles a malformed row - here exercised directly rather than
// through the orchestrator - gets an error back and the batch is not
// recorded, the same all-or-nothing behavior as the real store's validated
// transaction.
func TestFakeOpsReportModelOutcomesEnforcesContract(t *testing.T) {
	cases := []struct {
		name string
		row  cmclient.ModelOutcome
	}{
		{"zero n_candidates rejected", cmclient.ModelOutcome{Model: "a/b", Result: "win", NCandidates: 0}},
		{"empty model rejected", cmclient.ModelOutcome{Model: "", Result: "win", NCandidates: 1}},
		{"unknown result rejected", cmclient.ModelOutcome{Model: "a/b", Result: "meh", NCandidates: 1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops := &fakeOps{}

			err := ops.ReportModelOutcomes(context.Background(), "SUB-1", []cmclient.ModelOutcome{tc.row})
			require.Error(t, err)
			assert.Empty(t, ops.reportOutcomes, "an invalid batch must not be recorded")
		})
	}

	// A solo row (n_candidates == 1) is exactly the shape the relaxation
	// admits - it must still be accepted, not just tolerated at n_candidates
	// >= 2.
	ops := &fakeOps{}
	require.NoError(t, ops.ReportModelOutcomes(context.Background(), "SUB-1",
		[]cmclient.ModelOutcome{{Model: "a/b", Result: "win", NCandidates: 1}}))
	assert.Len(t, ops.reportOutcomes, 1)
}

// The former TestNoSalvageForParentSolver asserted the single-solver path always
// parked on the cap with no commit. That is no longer the contract: a capped
// single-solver subtask now commits its WIP and salvages it when the
// authoritative verify passes (salvageSoloCapped). Its coverage moved to
// TestSoloTurnCapSalvagedWhenVerifyPasses / ...StillParksWhenVerifyFails /
// ...StillParksOnCleanTree, plus TestExecuteSubtaskMaxTurnsNeverCompletes for the
// unresolved-verify park.

// TestExecuteSubtaskFlowReportsSoloWinOutcome proves a plain finish-terminated
// solo completion reports a "win" model outcome keyed on the SUBTASK card (not
// the parent) - the ReportUsage solo precedent, deliberately unlike the
// Best-of-N judge's parent rollup. VerifyPass is false: no authoritative verify
// ran on this path, so it is informational, not a signal.
func TestExecuteSubtaskFlowReportsSoloWinOutcome(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &planLLM{responses: []llm.Response{finishResp("feat(x): add y", 0.10)}}
	d := execTestDeps(ops, git, llmFake)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)
	require.NoError(t, runExecute(context.Background(), o))

	// The outcome report MUST fire before CompleteTask: report_model_outcome is
	// claim-gated, and a successful CompleteTask atomically releases the claim -
	// reporting afterward would silently vanish against an already-released claim.
	calls := ops.recorded()
	complete := indexOfCall(calls, "CompleteTask:SUB-1")
	report := indexOfCall(calls, "ReportModelOutcomes:SUB-1")
	require.GreaterOrEqual(t, complete, 0, "calls=%v", calls)
	require.GreaterOrEqual(t, report, 0, "calls=%v", calls)
	assert.Less(t, report, complete, "the outcome reports before completion, while the claim is still held")

	require.Len(t, ops.reportOutcomes, 1)
	rows := ops.reportOutcomes[0]
	require.Len(t, rows, 1)

	assert.Equal(t, "win", rows[0].Result)
	assert.False(t, rows[0].VerifyPass, "no authoritative verify ran on the plain completion path")
	assert.Equal(t, 1, rows[0].NCandidates, "a solo report is never scaled by candidate count")
	assert.Empty(t, rows[0].JudgeModel, "there is no judge on the solo path")
	assert.Equal(t, ops.lastUsageReport().Model, rows[0].Model,
		"with no gateway echo, the row's selected slug and the billed model coincide"+
			" (TestSoloOutcomeKeyedOnSelectedSlug pins the divergent case)")
	assert.InDelta(t, 0.10, rows[0].CostUSD, 1e-9)
}

// TestSoloTurnCapSalvageReportsWinOutcome proves the salvage-success path
// (a capped subtask whose committed work passes the authoritative verify)
// reports a "win" with VerifyPass true - the verify actually confirmed the
// work, unlike the plain completion path.
func TestSoloTurnCapSalvageReportsWinOutcome(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: burnResps(5)}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	seedResolvedVerifyPlan(o)
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 0} // pass
	}

	require.NoError(t, runExecute(context.Background(), o))

	// Same claim-gating requirement as the plain completion path: the report
	// must fire before CompleteTask releases the claim.
	calls := ops.recorded()
	complete := indexOfCall(calls, "CompleteTask:SUB-1")
	report := indexOfCall(calls, "ReportModelOutcomes:SUB-1")
	require.GreaterOrEqual(t, complete, 0, "calls=%v", calls)
	require.GreaterOrEqual(t, report, 0, "calls=%v", calls)
	assert.Less(t, report, complete, "the outcome reports before completion, while the claim is still held")

	require.Len(t, ops.reportOutcomes, 1)
	rows := ops.reportOutcomes[0]
	require.Len(t, rows, 1)

	assert.Equal(t, "win", rows[0].Result)
	assert.True(t, rows[0].VerifyPass, "the authoritative verify passed before the salvage")
	assert.Equal(t, 1, rows[0].NCandidates)
	assert.NotEmpty(t, rows[0].Model)
}

// TestSalvageDeclineExhaustedVerifyFailureReportsNoOutcome proves the
// environmental exemption extends to a verify that RAN and failed when its
// output carries a resource-exhaustion signature on both attempts: under a
// pids limit `go test` compiles, then its inner fork/exec dies with EAGAIN
// and go exits 1 - classified failed, surviving the single retry - yet that
// is container pressure, not evidence about the model. The park and the tier
// escalation still stand; only the leaderboard row is withheld.
func TestSalvageDeclineExhaustedVerifyFailureReportsNoOutcome(t *testing.T) {
	withFastVerifyRetryWait(t)

	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: burnResps(5)}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "moderate"}}, 0)
	ops.taskContext = cmclient.TaskContext{
		Description: withTierMarker("Implement the thing.", "moderate"),
	}

	seedResolvedVerifyPlan(o)
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{
			ExitCode: 1,
			Output:   "fork/exec /tmp/go-build/test.bin: resource temporarily unavailable",
		}
	}

	err := runExecute(context.Background(), o)
	require.Error(t, err, "the exhausted verify still declines the salvage and parks")

	assert.Empty(t, ops.reportOutcomes,
		"a verify killed by container pressure is an environment problem, not evidence about the model")
	assert.Equal(t, "Implement the thing.\n\n<!-- cm:tier=complex -->", ops.bodyFor("SUB-1"),
		"the tier still escalates even though the outcome goes unreported")
	assert.True(t, ops.loggedContains("resource exhaustion"),
		"the card log names the environmental cause the reporting exemption acted on; logs=%v", ops.logs)
}

// TestSalvageDeclineSkippedVerifyReportsNoOutcome proves a skip-classified
// authoritative verify (here, a timeout - environmental, not a code defect)
// gets the same exemption as the *ToolchainMissingError branch: the park still
// escalates the subtask tier and logs the classification, but no
// ReportModelOutcomes call fires, because a skipped verify is not evidence
// about the model. Its sibling, TestSalvageDeclineEscalatesTierAndReportsFailed,
// proves a genuine (ran-and-failed) verify keeps reporting unchanged.
func TestSalvageDeclineSkippedVerifyReportsNoOutcome(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: burnResps(5)}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "moderate"}}, 0)
	ops.taskContext = cmclient.TaskContext{
		Description: withTierMarker("Implement the thing.", "moderate"),
	}

	seedResolvedVerifyPlan(o)
	o.runVerify = func(_ context.Context, _ string, _ []string, _ time.Duration, _ []string) verifyexec.Outcome {
		return verifyexec.Outcome{TimedOut: true, ExitCode: -1}
	}

	err := runExecute(context.Background(), o)
	require.Error(t, err, "a timed-out verify still parks the capped subtask")

	assert.Empty(t, ops.reportOutcomes, "a skipped verify is an environment problem, not evidence about the model")
	assert.Equal(t, "Implement the thing.\n\n<!-- cm:tier=complex -->", ops.bodyFor("SUB-1"),
		"the tier still escalates even though the outcome goes unreported")
	assert.True(t, ops.loggedContains("verify timed out"),
		"the park is still activity-logged; logs=%v", ops.logs)
}

// TestSoloOutcomeKeyedOnSelectedSlug proves the outcome row is keyed on the
// SELECTED catalog slug, not the gateway-echoed model name: CM attaches
// outcome stats back onto candidates by the slug selection knows (the same
// keying as Best-of-N rows), so a row keyed on a gateway's echoed alias would
// never rejoin selection and the whole solo-learning loop would silently go
// dark. Usage reporting deliberately differs - it bills the echoed name.
func TestSoloOutcomeKeyedOnSelectedSlug(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}

	resp := finishResp("feat(x): add y", 0.10)
	resp.Model = "gateway/echoed-alias" // the gateway reports its own name for the model
	llmFake := &planLLM{responses: []llm.Response{resp}}
	d := execTestDeps(ops, git, llmFake)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)
	require.NoError(t, runExecute(context.Background(), o))

	require.Len(t, ops.reportOutcomes, 1)
	rows := ops.reportOutcomes[0]
	require.Len(t, rows, 1)

	assert.Equal(t, "gateway/echoed-alias", ops.lastUsageReport().Model,
		"usage bills the model the gateway says it served")
	assert.NotEqual(t, "gateway/echoed-alias", rows[0].Model,
		"the outcome row must not key on the gateway echo")
	assert.NotEmpty(t, rows[0].Model)
}

// TestSoloTurnCapToolchainSentinelReportsNoOutcome proves the toolchain-missing
// park (a distinct sentinel that supersedes the plain turn-cap) does not report
// a model outcome: a missing toolchain is an environment problem, not evidence
// about the model, and this park is left unlogged on the card for the same
// reason (see execute()'s dedicated arm).
func TestSoloTurnCapToolchainSentinelReportsNoOutcome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit probing is POSIX-only")
	}

	stubTools(t) // nothing on PATH: mvn cannot resolve

	dir := t.TempDir()

	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	inner := &planLLM{responses: burnResps(5)}
	client := &markerMidRunLLM{inner: inner, t: t, dir: dir}
	d := execTestDeps(ops, git, client)
	d.Cfg.MaxTurns = 5
	d.Cfg.Workspace = dir
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)
	o.planFn = func(context.Context) error { return nil }

	err := o.execute(context.Background())
	require.Error(t, err)

	var tme *ToolchainMissingError
	require.ErrorAs(t, err, &tme)

	assert.Empty(t, ops.reportOutcomes, "a toolchain-missing park must not report a model outcome")
}

// TestCandidateCompletionDoesNotReportSoloOutcome proves the solo-outcome
// report stays board-ops only: a Best-of-N candidate (boardOps false) that
// finishes a subtask normally must not emit any ReportModelOutcomes call -
// candidate outcomes are reported once, in aggregate, by the judge's adoption
// tail (reportCandidateOutcomes), never per-subtask. Pins that this task's
// change leaves the Best-of-N reporting path untouched.
func TestCandidateCompletionDoesNotReportSoloOutcome(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{finishResp("feat: x", 0.02)}}
	d := execTestDeps(ops, &fakeGit{committed: true}, client)
	o := newExecRun(d, nil, 0)

	cg := &fakeGit{committed: true}
	sc := &solverCtx{
		git: cg, ledger: NewLedger(0, 0), tools: d.WriteTools,
		workspace: "ws", coderModel: o.resolveCoderModel,
		boardOps: false, push: false, tag: "candidate 1/1",
	}

	require.NoError(t, o.executeSubtaskWith(context.Background(), sc,
		subtaskRef{ID: "SUB-1", Title: "Work", Tier: "simple"}))

	assert.Empty(t, ops.reportOutcomes, "a candidate's per-subtask completion must not report a solo model outcome")
}

// TestSequentialSoloSubtasksReportPerSubtaskCostDelta proves CostUSD on each
// solo outcome row is the SUBTASK's own ledger delta, not the run's
// cumulative total: the second subtask's win report must carry only its own
// spend, not its own spend plus the first subtask's - which the run ledger's
// running total would otherwise silently fold in (and which would compound
// further on a resumed run carrying whole prior sessions' spend).
func TestSequentialSoloSubtasksReportPerSubtaskCostDelta(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &planLLM{responses: []llm.Response{
		finishResp("feat: one", 0.10),
		finishResp("feat: two", 0.25),
	}}
	d := execTestDeps(ops, git, llmFake)

	o := newExecRun(d, []subtaskRef{
		{ID: "SUB-1", Title: "First", Tier: "simple"},
		{ID: "SUB-2", Title: "Second", Tier: "simple"},
	}, 0)
	require.NoError(t, runExecute(context.Background(), o))

	require.Len(t, ops.reportOutcomes, 2, "one outcome report per subtask")

	row1 := ops.reportOutcomes[0]
	require.Len(t, row1, 1)
	assert.InDelta(t, 0.10, row1[0].CostUSD, 1e-9, "SUB-1's own cost only")

	row2 := ops.reportOutcomes[1]
	require.Len(t, row2, 1)
	assert.InDelta(t, 0.25, row2[0].CostUSD, 1e-9,
		"SUB-2's own cost only - not cumulative with SUB-1's 0.10")

	// The run ledger itself stays cumulative (it enforces the whole-run
	// ceiling); only the per-subtask REPORT is a delta.
	assert.InDelta(t, 0.35, o.ledger.Spent(), 1e-9)
}

// TestCandidateSubtaskParksOnRunLedgerBreach pins the fan-out window close: a
// candidate whose own sub-ledger is clean still parks when the RUN ledger -
// the only ledger server-priced totals sync into - is over the ceiling.
func TestCandidateSubtaskParksOnRunLedgerBreach(t *testing.T) {
	ops := &fakeOps{}
	d := execTestDeps(ops, &fakeGit{committed: true}, &planLLM{})
	o := newExecRun(d, nil, 0)

	o.ledger = NewLedger(8.75, 0)
	o.ledger.SyncServerTotal("PARENT-1", 41.07)

	sc := &solverCtx{
		git: &fakeGit{committed: true}, ledger: NewLedger(0, 0), tools: d.WriteTools,
		workspace: "ws", coderModel: o.resolveCoderModel,
		boardOps: false, push: false, tag: "candidate 1/1",
	}

	err := o.executeSubtaskWith(context.Background(), sc,
		subtaskRef{ID: "SUB-1", Title: "Work", Tier: "simple"})

	var bee *BudgetExceededError

	require.ErrorAs(t, err, &bee, "a run-ledger breach parks the candidate")
	assert.Empty(t, ops.recorded(), "parked before any board op or model call")
}

func TestCoderPromptParentSlotOmitsRecordedHistory(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &planLLM{responses: []llm.Response{finishResp("feat: x", 0.01)}}
	d := execTestDeps(ops, git, llmFake)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Add flag", Body: "Files: b.go", Tier: "simple"}}, 0)
	o.tc.Description = grownDescription
	o.taskDescription = stripAgentSections(grownDescription)

	require.NoError(t, runExecute(context.Background(), o))

	require.NotEmpty(t, llmFake.tasks)
	assert.Contains(t, llmFake.tasks[0], "Add a config flag to toggle the feature.", "human intro in the parent slot")
	assert.NotContains(t, llmFake.tasks[0], "naming could improve", "review history stays out of the coder prompt")
	assert.NotContains(t, llmFake.tasks[0], "1. SUBTASK: Add the flag", "parent plan not re-imported per subtask")
}

// TestResolveCoderModelPinMissingAdvisory proves that a non-empty but
// unresolvable coder pin reaches the card exactly once per run, however many
// subtasks resolve a coder, and does not become the model selected.
//
// The count is taken over the lines that NAME the pin. The card log also
// carries tier-shortfall advisories, deduped on their own key and therefore
// one per distinct requested tier, so a total-line count would be measuring a
// different guarantee and would move whenever an unrelated advisory is added.
func TestResolveCoderModelPinMissingAdvisory(t *testing.T) {
	const missingPin = "pinned/missing" // not in planTestCatalog

	ops := &fakeOps{}
	d := execTestDeps(ops, &fakeGit{}, &planLLM{})
	o := newExecRun(d, nil, 5)
	ctx := context.Background()

	o.tc.ModelCoder = missingPin

	// Multiple calls across different subtasks.
	first, err := o.resolveCoderModel(ctx, subtaskRef{ID: "SUB-1", Title: "First", Tier: "simple"}, "prompt1")
	require.NoError(t, err)

	second, err := o.resolveCoderModel(ctx, subtaskRef{ID: "SUB-2", Title: "Second", Tier: "moderate"}, "prompt2")
	require.NoError(t, err)

	assert.NotEqual(t, missingPin, first, "an unresolvable pin never becomes the selected model")
	assert.NotEqual(t, missingPin, second)

	var pinAdvisories []string

	for _, l := range ops.logs {
		if strings.Contains(l, missingPin) {
			pinAdvisories = append(pinAdvisories, l)
		}
	}

	require.Len(t, pinAdvisories, 1, "exactly one advisory log for the missing pin")
}

// TestResolveCoderModelPinResolvableNoAdvisory proves that a resolvable coder
// pin produces zero advisory log entries.
func TestResolveCoderModelPinResolvableNoAdvisory(t *testing.T) {
	ops := &fakeOps{}
	d := execTestDeps(ops, &fakeGit{}, &planLLM{})
	o := newExecRun(d, nil, 5)
	ctx := context.Background()

	o.tc.ModelCoder = "pinned/model" // IS in planTestCatalog

	_, _ = o.resolveCoderModel(ctx, subtaskRef{ID: "SUB-1", Title: "First", Tier: "simple"}, "prompt1")
	_, _ = o.resolveCoderModel(ctx, subtaskRef{ID: "SUB-2", Title: "Second", Tier: "moderate"}, "prompt2")

	// No advisory logs because the pin is resolvable.
	assert.Empty(t, ops.logs, "resolvable pin produces no advisory logs")
}

// subFloorCoderRegistry seeds one coder model whose prior sits below every
// configured bar, so the only employable answer is the operator's capable
// default.
func subFloorCoderRegistry() *registry.Registry {
	return registry.NewRegistryFromParts(
		llm.Catalog{{ID: "weak/model", ContextLength: 200000, SupportedParameters: []string{"tools"}}},
		registry.Priors{Models: map[string]registry.PriorEntry{"weak/model": coderPrior(0.30)}},
		nil, nil, "operator/default")
}

// TestResolveCoderModelParksOnlyWhenEvenTheDefaultIsBarred pins the narrow
// refusal: a sub-floor catalog still runs on the operator's default, and the
// run parks only when that default is itself barred this run.
func TestResolveCoderModelParksOnlyWhenEvenTheDefaultIsBarred(t *testing.T) {
	sub := subtaskRef{ID: "SUB-1", Title: "First", Tier: "moderate"}

	t.Run("sub-floor catalog falls to the operator default", func(t *testing.T) {
		d := execTestDeps(&fakeOps{}, &fakeGit{}, &planLLM{})
		d.Registry = subFloorCoderRegistry()
		o := newExecRun(d, nil, 5)

		model, err := o.resolveCoderModel(context.Background(), sub, "prompt")
		require.NoError(t, err)
		assert.Equal(t, "operator/default", model,
			"the capable default is the trigger's default_model - operator intent, not junk")
	})

	t.Run("an excluded default parks the run", func(t *testing.T) {
		ops := &fakeOps{}
		d := execTestDeps(ops, &fakeGit{}, &planLLM{})
		d.Registry = subFloorCoderRegistry()
		o := newExecRun(d, nil, 5)
		o.excluded = map[string]bool{"operator/default": true, "weak/model": true}

		model, err := o.resolveCoderModel(context.Background(), sub, "prompt")
		require.Error(t, err)
		assert.Empty(t, model)

		var nme *NoModelError

		require.ErrorAs(t, err, &nme)
		assert.Equal(t, registry.RoleCoder, nme.Role)
		assert.Equal(t, registry.TierModerate, nme.Tier)
		assert.Equal(t, 2, nme.Excluded)
		assert.False(t, nme.WindowLimited)

		assert.True(t, isParkError(err),
			"a run with no selectable coder must park, not walk into the next phase")
	})
}

// TestNoModelErrorNamesTheWindowWhenThatIsTheCause proves the refusal does not
// send the operator to look at priors when the actual constraint is context
// size - the ladder cannot buy a bigger window.
func TestNoModelErrorNamesTheWindowWhenThatIsTheCause(t *testing.T) {
	d := execTestDeps(&fakeOps{}, &fakeGit{}, &planLLM{})
	// Both the scored model and the capable default are window-short, so the
	// pool empties on fit rather than on quality.
	d.Registry = registry.NewRegistryFromParts(
		llm.Catalog{
			{ID: "small/window", ContextLength: 8000, SupportedParameters: []string{"tools"}},
			{ID: "tiny/default", ContextLength: 8000, SupportedParameters: []string{"tools"}},
		},
		registry.Priors{Models: map[string]registry.PriorEntry{"small/window": coderPrior(0.90)}},
		nil, nil, "tiny/default")
	o := newExecRun(d, nil, 5)

	in := registry.SelectInput{
		Role: registry.RoleCoder, Tier: registry.TierModerate, EstTokens: 500000,
	}

	p := d.Registry.SelectByComplexity(in)
	require.False(t, p.OK, "the fixture must actually refuse, or the classification is never exercised")

	nme := o.noModelError(in, p)

	assert.True(t, nme.WindowLimited,
		"a pool that is non-empty at EstTokens 0 emptied on window fit, not on quality")
}

// midTierCoderDeps wires an execute run whose only scored coder clears the
// moderate bar (0.76) but neither complex (0.82) nor critical (0.90), so every
// selection above moderate reports a shortfall.
func midTierCoderDeps(ops *fakeOps) Deps {
	d := execTestDeps(ops, &fakeGit{}, &planLLM{})
	d.Registry = registry.NewRegistryFromParts(
		llm.Catalog{{ID: "mid/model", ContextLength: 200000, SupportedParameters: []string{"tools"}}},
		registry.Priors{Models: map[string]registry.PriorEntry{"mid/model": coderPrior(0.78)}},
		nil, nil, "capable/default")

	return d
}

// TestShortfallAdvisoryIsOncePerPhaseRoleAndTier mirrors the pin-advisory
// guard: the fixed-tier callers ask for the same tier on every call and the
// card's activity log is capped, so an undeduped advisory would evict real
// history. A distinct requested bar is a distinct fact and earns its own line.
func TestShortfallAdvisoryIsOncePerPhaseRoleAndTier(t *testing.T) {
	t.Run("repeated selections at one tier advise once", func(t *testing.T) {
		ops := &fakeOps{}
		o := newExecRun(midTierCoderDeps(ops), nil, 5)
		ctx := context.Background()

		for _, id := range []string{"SUB-1", "SUB-2", "SUB-3"} {
			model, err := o.resolveCoderModel(ctx, subtaskRef{ID: id, Title: id, Tier: "complex"}, "prompt")
			require.NoError(t, err)
			assert.Equal(t, "mid/model", model, "the clamped pick is still a measured model")
		}

		require.Len(t, ops.logs, 1,
			"one shortfall advisory across repeated same-tier selections; logs=%v", ops.logs)
		assert.Contains(t, ops.logs[0], "mid/model")
	})

	t.Run("two requested tiers in one phase advise separately", func(t *testing.T) {
		ops := &fakeOps{}
		o := newExecRun(midTierCoderDeps(ops), nil, 5)
		ctx := context.Background()

		_, err := o.resolveCoderModel(ctx, subtaskRef{ID: "SUB-1", Title: "First", Tier: "complex"}, "prompt")
		require.NoError(t, err)

		_, err = o.resolveCoderModel(ctx, subtaskRef{ID: "SUB-2", Title: "Second", Tier: "critical"}, "prompt")
		require.NoError(t, err)

		require.Len(t, ops.logs, 2,
			"a different requested bar is a different fact and earns its own line; logs=%v", ops.logs)
	})
}

// tierNamingRegistry prices one model per tier so that exactly one is
// affordable at each rung: the best-value band around the cheapest candidate
// excludes everything above it. The model that comes back therefore NAMES the
// tier that went in, which is what lets a test observe the tier a call site
// asked for without a spy.
func tierNamingRegistry() *registry.Registry {
	return registry.NewRegistryFromParts(
		llm.Catalog{
			{ID: "tier/simple", ContextLength: 200000, PromptPricePerTok: 5e-10, CompletionPricePerTok: 5e-10, SupportedParameters: []string{"tools"}},
			{ID: "tier/moderate", ContextLength: 200000, PromptPricePerTok: 5e-7, CompletionPricePerTok: 5e-7, SupportedParameters: []string{"tools"}},
			{ID: "tier/complex", ContextLength: 200000, PromptPricePerTok: 5e-4, CompletionPricePerTok: 5e-4, SupportedParameters: []string{"tools"}},
			{ID: "capable/default", ContextLength: 200000, SupportedParameters: []string{"tools"}},
		},
		registry.Priors{Models: map[string]registry.PriorEntry{
			"tier/simple":   coderPrior(0.70),
			"tier/moderate": coderPrior(0.80),
			"tier/complex":  coderPrior(0.90),
		}},
		nil, nil, "capable/default")
}

// TestCoderTierIsAlwaysDerivedFromTheWork guards the distinction this whole
// selection design rests on.
//
// An AUTHORING tier is a claim about how hard the work is. The run can check
// that claim against what actually happens, so it is measured and corrected -
// never pinned to a constant. A JUDGMENT bar is a claim about how good the
// judgement has to be; nothing in the run measures that, so it is stated
// policy with a stated reason.
//
// Every coder selection is authoring, so all three of them must keep deriving
// their tier from the subtask or the card. Flooring any of them at a constant
// would silently overpay on trivial work and, worse, make the tier stop
// meaning anything the run could correct.
//
// The one deliberate exception is the authoritative fix pass, which pins the
// complex tier because it is the last round before a human takes the card.
// That is precedent for nothing else and this guard does not cover it.
func TestCoderTierIsAlwaysDerivedFromTheWork(t *testing.T) {
	tiers := []struct {
		tier      string
		wantModel string
	}{
		{tier: "simple", wantModel: "tier/simple"},
		{tier: "moderate", wantModel: "tier/moderate"},
		{tier: "complex", wantModel: "tier/complex"},
	}

	t.Run("the subtask coder derives from the subtask", func(t *testing.T) {
		for _, tt := range tiers {
			t.Run(tt.tier, func(t *testing.T) {
				d := execTestDeps(&fakeOps{}, &fakeGit{}, &planLLM{})
				d.Registry = tierNamingRegistry()
				o := newExecRun(d, nil, 0)

				model, err := o.resolveCoderModel(context.Background(),
					subtaskRef{ID: "SUB-1", Title: "First", Tier: tt.tier}, "prompt")
				require.NoError(t, err)
				assert.Equal(t, tt.wantModel, model,
					"the coder tier must be the subtask's, not a constant")
			})
		}
	})

	t.Run("the candidate fan-out derives from the card", func(t *testing.T) {
		for _, tt := range tiers {
			t.Run(tt.tier, func(t *testing.T) {
				d, _, _ := fanoutDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, 2)
				d.Registry = tierNamingRegistry()

				o := newFanoutRun(t, d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "simple"}}, 0)
				o.cardTier = tt.tier

				require.NoError(t, o.runFanout(context.Background()))
				require.NotEmpty(t, o.candidates)
				assert.Equal(t, tt.wantModel, o.candidates[0].model,
					"the fan-out's first seat must be the card's tier, not a constant")
			})
		}
	})

	t.Run("the candidate re-pick derives from the card", func(t *testing.T) {
		for _, tt := range tiers {
			t.Run(tt.tier, func(t *testing.T) {
				d := execTestDeps(&fakeOps{}, &fakeGit{}, &planLLM{})
				d.Registry = tierNamingRegistry()

				o := newExecRun(d, nil, 0)
				o.cardTier = tt.tier
				o.excluded = map[string]bool{"dropped/model": true}

				c := &candidate{idx: 1, model: "dropped/model"}

				model, err := o.candidateCoderModel(c)(context.Background(), subtaskRef{ID: "SUB-1"}, "prompt")
				require.NoError(t, err)
				assert.Equal(t, tt.wantModel, model,
					"a candidate re-pick must stay on the card's tier, not a constant")
			})
		}
	})
}

// TestShortfallAdvisoryNeverTakesTheSelectionLock observes the lock directly
// instead of through a behaviour that happens to break: hold the selection
// lock, raise an advisory from another goroutine, and wait. It returns only if
// the advisory takes some other mutex. The candidate resolver holds this lock
// across a selection, so an advisory that reached for it would deadlock the
// Best-of-N fan-out rather than fail visibly.
func TestShortfallAdvisoryNeverTakesTheSelectionLock(t *testing.T) {
	o := newExecRun(midTierCoderDeps(&fakeOps{}), nil, 5)

	o.selMu.Lock()
	defer o.selMu.Unlock()

	done := make(chan struct{})

	go func() {
		defer close(done)

		o.noteShortfall(context.Background(), "probe", registry.Pick{
			ModelSpec:     registry.ModelSpec{Model: "mid/model"},
			Role:          registry.RoleCoder,
			RequestedTier: registry.TierComplex,
			MetTier:       registry.TierModerate,
			HasPrior:      true,
			OK:            true,
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the shortfall advisory blocked on the selection lock")
	}
}

// TestUnmeasuredPickReportsNoPriorRatherThanZero pins the difference HasPrior
// exists to carry, at the point where it reaches a human. The capable default
// is normally absent from the live catalog, so the priors built from that
// catalog cannot score it - and a card line reading "prior 0.00" tells an
// operator their own chosen fallback rated worst possible, which is grounds to
// blacklist a model whose only fault is having no rating.
func TestUnmeasuredPickReportsNoPriorRatherThanZero(t *testing.T) {
	ops := &fakeOps{}
	// The plan registry carries no priors at all, so every rung is dry and the
	// answer is the operator's capable default: the standard degraded shape.
	o := newExecRun(execTestDeps(ops, &fakeGit{}, &planLLM{}), nil, 5)

	model, err := o.resolveCoderModel(context.Background(),
		subtaskRef{ID: "SUB-1", Title: "First", Tier: "moderate"}, "prompt")
	require.NoError(t, err)
	require.Equal(t, "default/model", model)

	require.Len(t, ops.logs, 1, "logs=%v", ops.logs)
	assert.Contains(t, ops.logs[0], "no measured prior")
	assert.NotContains(t, ops.logs[0], "prior 0.00",
		"nothing measured this model, so no number may be printed for it; logs=%v", ops.logs)
}

// verifyCall is one recorded invocation of the verify exec seam: everything a
// gate resolved and passed down, so two gates can be compared tuple for tuple.
type verifyCall struct {
	dir     string
	argv    []string
	timeout time.Duration
	env     []string
}

// verifyFixPasses counts the scripted model calls whose prompt carries a failed
// verify gate's finding, so a test can assert how many fix passes ran. Keyed on
// verifyFailedPrefix - the constant that marks the finding - not on prose.
func verifyFixPasses(c *planLLM) int {
	n := 0

	for i := range modelCallCount(c) {
		if strings.Contains(promptOfCall(c, i), verifyFailedPrefix) {
			n++
		}
	}

	return n
}

// TestPreCommitVerifyFailureBlocksCommitAndRoutesToFix proves the gate stops a
// broken subtask from reaching a commit. Two commits carrying three failing
// tests and a nil-pointer panic shipped because nothing ran between the coder's
// terminal call and CommitWithMessage; a verify that fails, earns one fix pass,
// and fails again must leave the work uncommitted and park the subtask.
func TestPreCommitVerifyFailureBlocksCommitAndRoutesToFix(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		finishResp("feat: subtask done", 0.01),     // the coder's terminal call
		stopResp("coder: attempted the fix", 0.02), // the one fix pass
	}}
	d := execTestDeps(ops, git, client)
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	seedResolvedVerifyPlan(o)
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 1, Output: "--- FAIL: TestThing"}
	}

	err := runExecute(context.Background(), o)
	require.Error(t, err, "a subtask whose verify is still red after one fix pass must not commit")

	assert.Empty(t, git.commitMsgs, "nothing may be committed while the verify is red; git=%v", git.recorded())
	assert.Equal(t, 1, verifyFixPasses(client), "exactly one bounded fix pass, never a loop")

	calls := ops.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "CompleteTask:SUB-1"), "an uncommitted subtask is not completed")
	assert.GreaterOrEqual(t, indexOfCall(calls, "ReleaseCard:SUB-1"), 0, "the parked claim is released")
	assert.Empty(t, git.pushBranches, "a red tree is never pushed")
}

// TestPreCommitVerifyFailureThenFixPassCommits proves the fix pass is a real
// second chance: a gate that goes green after it commits the coder's own work
// under the coder's own message.
func TestPreCommitVerifyFailureThenFixPassCommits(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		finishResp("feat: subtask done", 0.01),
		stopResp("coder: fixed the failing test", 0.02),
	}}
	d := execTestDeps(ops, git, client)
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	seedResolvedVerifyPlan(o)

	runs := 0
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		runs++
		if runs == 1 {
			return verifyexec.Outcome{ExitCode: 1, Output: "--- FAIL: TestThing"}
		}

		return verifyexec.Outcome{ExitCode: 0}
	}

	require.NoError(t, runExecute(context.Background(), o),
		"a gate that goes green after the fix pass commits")

	require.Len(t, git.commitMsgs, 1, "exactly one commit; git=%v", git.recorded())
	assert.Equal(t, "feat: subtask done", git.commitMsgs[0], "the commit carries the coder's finish message")
	assert.Equal(t, 1, verifyFixPasses(client), "exactly one bounded fix pass, never a loop")
	assert.Equal(t, 2, runs, "the gate re-runs once after the fix pass")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "CompleteTask:SUB-1"), 0, "the verified subtask completes")
}

// TestPreCommitVerifyPassCommitsAsToday proves a green gate is invisible: the
// commit lands under the coder's own message and no fix model is spent.
func TestPreCommitVerifyPassCommitsAsToday(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{finishResp("feat: subtask done", 0.01)}}
	d := execTestDeps(ops, git, client)
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	seedResolvedVerifyPlan(o)
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 0}
	}

	require.NoError(t, runExecute(context.Background(), o))

	require.Len(t, git.commitMsgs, 1, "exactly one commit; git=%v", git.recorded())
	assert.Equal(t, "feat: subtask done", git.commitMsgs[0])
	assert.Equal(t, 0, verifyFixPasses(client), "a green gate spends no fix model")
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "CompleteTask:SUB-1"), 0)
}

// TestPreCommitVerifySkippedCommitsAsToday proves an inconclusive gate is
// neither a pass nor a failure: a timed-out run (or a missing tool) is not
// evidence of a defect, so the commit proceeds exactly as it did before the
// gate existed and no fix model is spent. This mirrors the review round's own
// verifySkipped arm.
func TestPreCommitVerifySkippedCommitsAsToday(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{finishResp("feat: subtask done", 0.01)}}
	d := execTestDeps(ops, git, client)
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	seedResolvedVerifyPlan(o)
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		return verifyexec.Outcome{TimedOut: true, ExitCode: -1}
	}

	require.NoError(t, runExecute(context.Background(), o),
		"an inconclusive gate is not a failure - the commit proceeds as it did before the gate existed")

	require.Len(t, git.commitMsgs, 1, "exactly one commit; git=%v", git.recorded())
	assert.Equal(t, "feat: subtask done", git.commitMsgs[0])
	assert.Equal(t, 0, verifyFixPasses(client), "an inconclusive gate spends no fix model")
	assert.True(t, ops.loggedContains("verify timed out"),
		"the skip is logged with its classification; logs=%v", ops.logs)
}

// TestNoResolvableVerifyCommitsAsToday proves the skip tier is byte-identical
// to today: an empty resolved argv means there is nothing to run, so no
// subprocess starts and the commit proceeds.
func TestNoResolvableVerifyCommitsAsToday(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{finishResp("feat: subtask done", 0.01)}}
	d := execTestDeps(ops, git, client)
	// newExecRun's isolateVerify leaves the plan at the skip tier (empty Argv).
	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	ran := false
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		ran = true

		return verifyexec.Outcome{ExitCode: 0}
	}

	require.NoError(t, runExecute(context.Background(), o))

	require.Len(t, git.commitMsgs, 1, "exactly one commit; git=%v", git.recorded())
	assert.Equal(t, "feat: subtask done", git.commitMsgs[0])
	assert.False(t, ran, "an empty resolved argv means there is nothing to run")
	assert.Equal(t, 0, verifyFixPasses(client))
}

// TestPreCommitGateAndReviewGateResolveIdentically proves the two gates cannot
// drift into different commands, timeouts or environments: both take the plan
// ensureVerify resolved and hand it to runVerifyPlan unchanged, so the tuple
// reaching the exec seam is the same from either caller.
func TestPreCommitGateAndReviewGateResolveIdentically(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{stopResp("coder: attempted the fix", 0.02)}}
	d := execTestDeps(ops, git, client)
	d.Cfg.Workspace = t.TempDir()
	o := newExecRun(d, nil, 0)

	// A plan whose timeout and env are both distinct from what the run would
	// resolve on its own, so a gate that resolved its own instead of taking the
	// plan's is visible in the tuple.
	o.verify = &verifyPlan{
		Argv:    []string{"declared", "check"},
		Display: "declared check",
		Source:  verifySourceDeclared,
		Timeout: 42 * time.Minute,
		Env:     []string{"DECLARED=1"},
	}

	var seen []verifyCall

	o.runVerify = func(_ context.Context, dir string, argv []string, timeout time.Duration, env []string) verifyexec.Outcome {
		seen = append(seen, verifyCall{dir: dir, argv: argv, timeout: timeout, env: env})

		return verifyexec.Outcome{ExitCode: 1, Output: "--- FAIL: TestThing"}
	}

	require.NotEqual(t, o.verifyTimeout(), o.verify.Timeout,
		"the plan's timeout must differ from the run default, or this test cannot see the drift")

	sub := subtaskRef{ID: "SUB-1", Title: "Only", Tier: "simple"}
	require.Error(t, o.preCommitVerify(context.Background(), o.solver, sub))
	require.NotEmpty(t, seen, "the pre-commit gate ran the command")

	preCommit := seen[0]
	seen = nil

	_, _, approved, _, _, err := o.reviewRound(context.Background(), o.resolvedVerifyPlan(), 1, false)
	require.NoError(t, err)
	require.False(t, approved, "a red gate short-circuits the review round")
	require.NotEmpty(t, seen, "the review gate ran the command")

	assert.Equal(t, preCommit, seen[0],
		"the pre-commit gate and the review gate must run the identical command, timeout and environment")
}

// TestPreCommitVerifySkippedForCandidateSolver proves the gate is solo-only. A
// Best-of-N candidate is judged by the judge phase's authoritative verify over
// every candidate worktree, and candidates race in parallel over one shared
// run: resolving and fixing per candidate would race the run's cached plan and
// charge the run ledger during the fan-out, which the candidate sub-ledgers
// exist to keep separate.
func TestPreCommitVerifySkippedForCandidateSolver(t *testing.T) {
	ops := &fakeOps{}
	d := execTestDeps(ops, &fakeGit{committed: true}, &planLLM{})
	o := newExecRun(d, nil, 0)

	seedResolvedVerifyPlan(o)

	ran := false
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		ran = true

		return verifyexec.Outcome{ExitCode: 1, Output: "--- FAIL: TestThing"}
	}

	sc := &solverCtx{
		git: &fakeGit{committed: true}, ledger: NewLedger(0, 0), tools: d.WriteTools,
		workspace: "ws", coderModel: o.resolveCoderModel,
		boardOps: false, push: false, tag: "candidate 1/2",
	}

	require.NoError(t, o.preCommitVerify(context.Background(), sc, subtaskRef{ID: "SUB-1", Title: "Only"}))
	assert.False(t, ran, "a candidate solver never runs the pre-commit gate")
}
