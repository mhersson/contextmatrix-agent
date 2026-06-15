package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// execTestDeps builds Deps wired for the execute phase: scripted ops + git, a
// single stop-response coder LLM, full write tools, and the plan test registry.
func execTestDeps(ops *fakeOps, git *fakeGit, client llm.LLM) Deps {
	return Deps{
		Ops:        ops,
		Git:        git,
		Client:     client,
		Emit:       events.NewEmitter(nil, nil),
		Registry:   planTestRegistry(),
		WriteTools: tools.NewRegistry(tools.NewReadTool(".")),
		ReadTools:  tools.NewRegistry(tools.NewReadTool(".")),
		Cfg: Config{
			Project:      "proj",
			CardID:       "CARD-1",
			Branch:       "cm/card-1",
			PayloadModel: "payload/model",
			DefaultModel: "default/model",
			MaxTurns:     5,
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
		stopResp("done\nCOMMIT: feat(x): add y", 0.10),
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

func TestExecuteCoderPromptBody(t *testing.T) {
	t.Run("planner description reaches the coder prompt", func(t *testing.T) {
		ops := &fakeOps{}
		git := &fakeGit{committed: true}
		llmFake := &planLLM{responses: []llm.Response{stopResp("ok\nCOMMIT: feat: x", 0.01)}}
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
		llmFake := &planLLM{responses: []llm.Response{stopResp("ok\nCOMMIT: feat: x", 0.01)}}
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
		stopResp("ok\nCOMMIT: feat: one", 0.01),
		stopResp("ok\nCOMMIT: feat: two", 0.01),
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
	llmFake := &planLLM{responses: []llm.Response{stopResp("ok\nCOMMIT: feat: x", 0.01)}}
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
		stopResp("nothing to change\nCOMMIT: chore: noop", 0.02),
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
	llmFake := &planLLM{responses: []llm.Response{stopResp("ok\nCOMMIT: feat: x", 0.01)}}
	d := execTestDeps(ops, git, llmFake)
	// The card pins a catalog-resolvable coder model.
	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body", ModelCoder: "pinned/model"}
	d.Cfg.MaxCardCost = 0
	o := newRun(d, tc)
	o.subtasks = []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "complex"}}

	require.NoError(t, runExecute(context.Background(), o))

	require.NotEmpty(t, llmFake.models)
	assert.Equal(t, "pinned/model", llmFake.models[0], "harness must run on the pinned coder model")
}

func TestExecuteModelSelectionByComplexity(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &planLLM{responses: []llm.Response{stopResp("ok\nCOMMIT: feat: x", 0.01)}}

	// A registry where exactly one tools-capable model has a measured coder score
	// and clears every tier bar, so SelectByComplexity is forced to pick it.
	catalog := llm.Catalog{
		{ID: "the/coder", ContextLength: 200000, SupportedParameters: []string{"tools"}},
	}
	reg := registry.NewRegistryWithCapabilities(nil, "fallback/default", catalog,
		map[string]map[registry.Role]float64{
			"the/coder": {registry.RoleCoder: 0.95},
		})

	d := execTestDeps(ops, git, llmFake)
	d.Registry = reg
	// No coder pin -> complexity selection path.
	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body"}
	d.Cfg.MaxCardCost = 0
	o := newRun(d, tc)
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
	llmFake := &planLLM{responses: []llm.Response{stopResp("ok\nCOMMIT: feat: x", 0.01)}}
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
	t.Run("handoff COMMIT line extracted", func(t *testing.T) {
		ops := &fakeOps{}
		git := &fakeGit{committed: true}
		llmFake := &planLLM{responses: []llm.Response{
			stopResp("I did the work.\nCOMMIT: feat(api): add health endpoint", 0.01),
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
		stopResp("ok\nCOMMIT: feat: one", 0.60),
		stopResp("ok\nCOMMIT: feat: two", 0.60),
	}}
	d := execTestDeps(ops, git, llmFake)
	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body", ReportedCostUSD: 0.50}
	d.Cfg.MaxCardCost = 1.00
	o := newRun(d, tc)
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
	llmFake := &planLLM{responses: []llm.Response{stopResp("ok\nCOMMIT: feat: x", 0.01)}}
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
	llmFake := &planLLM{responses: []llm.Response{stopResp("ok\nCOMMIT: feat: x", 0.01)}}
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

func TestExtractCommitLine(t *testing.T) {
	t.Run("extracts trailing commit line", func(t *testing.T) {
		got, ok := extractCommitLine("blah\nCOMMIT: fix(core): handle nil\n")
		require.True(t, ok)
		assert.Equal(t, "fix(core): handle nil", got)
	})

	t.Run("multiple commit lines: last wins", func(t *testing.T) {
		got, ok := extractCommitLine("COMMIT: feat: first\nmore handoff prose\nCOMMIT: feat: final\n")
		require.True(t, ok)
		assert.Equal(t, "feat: final", got)
	})

	t.Run("no commit line", func(t *testing.T) {
		_, ok := extractCommitLine("just some prose with no marker")
		assert.False(t, ok)
	})

	t.Run("empty after marker is not accepted", func(t *testing.T) {
		_, ok := extractCommitLine("COMMIT:    ")
		assert.False(t, ok)
	})
}

func TestSubtaskSummary(t *testing.T) {
	t.Run("first non-empty line of the handoff", func(t *testing.T) {
		got := subtaskSummary("\n\nImplemented the endpoint.\nCOMMIT: feat: x\n", "Title fallback")
		assert.Equal(t, "Implemented the endpoint.", got)
	})

	t.Run("all blank lines falls back to title", func(t *testing.T) {
		got := subtaskSummary("\n   \n\t\n", "Title fallback")
		assert.Equal(t, "Title fallback", got)
	})

	t.Run("empty output falls back to title", func(t *testing.T) {
		got := subtaskSummary("", "Title fallback")
		assert.Equal(t, "Title fallback", got)
	})
}

// guard: the coder prompt template must reference the branch-state note and the
// COMMIT line convention so the extractor has something to extract.
func TestCoderPromptShape(t *testing.T) {
	assert.Contains(t, strings.ToUpper(coderPrompt), "COMMIT:")
	assert.Contains(t, strings.ToLower(coderPrompt), "branch")
}
