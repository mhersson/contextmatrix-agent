package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errLLM is an llm.LLM whose every call fails, simulating a transport error.
// harness.Run surfaces this as (res, err) with Reason "error"; runModel returns
// it unchanged, so runDocument sees a non-nil model error.
type errLLM struct{ err error }

func (e *errLLM) Send(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{}, e.err
}

func (e *errLLM) SendStream(context.Context, llm.Request, func(llm.Delta)) (llm.Response, error) {
	return llm.Response{}, e.err
}

// documentTestDeps builds Deps wired for the document phase: scripted ops + git,
// a stop-response orchestrator LLM, and BOTH tool registries non-nil (runModel
// passes WriteTools to harness.Run, which calls reg.Schemas()).
func documentTestDeps(ops *fakeOps, git *fakeGit, client llm.LLM) Deps {
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
			BaseBranch:   "main",
			PayloadModel: "payload/model",
			DefaultModel: "default/model",
			MaxTurns:     5,
		},
	}
}

// newDocumentRun builds a run with the ledger ceiling set (maxCost == 0 disables it).
func newDocumentRun(d Deps, tc cmclient.TaskContext, maxCost float64) *run {
	d.Cfg.MaxCardCost = maxCost

	return newRun(d, tc)
}

func TestDocumentNoOpWhenNoDocsNeeded(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: false} // agent wrote nothing -> clean tree
	llmFake := &planLLM{responses: []llm.Response{stopResp("No external documentation is needed for this internal refactor.", 0.01)}}
	d := documentTestDeps(ops, git, llmFake)

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Refactor internals", Description: "body"}
	o := newDocumentRun(d, tc, 0)

	require.NoError(t, runDocument(context.Background(), o))

	// CommitWithMessage may be probed, but a clean tree (committed=false) -> NO push.
	assert.Empty(t, git.pushBranches, "no push when nothing was committed; git=%v", git.recorded())
	assert.True(t, ops.loggedContains("no external docs needed"), "no-op outcome logged; logs=%v", ops.logs)
}

func TestDocumentWritesAndPushesDocs(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true} // agent wrote docs -> dirty tree
	llmFake := &planLLM{responses: []llm.Response{
		stopResp("Updated the API reference.\nCOMMIT: docs(api): document health endpoint", 0.03),
	}}
	d := documentTestDeps(ops, git, llmFake)

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Add health endpoint", Description: "body"}
	o := newDocumentRun(d, tc, 0)

	require.NoError(t, runDocument(context.Background(), o))

	require.NotEmpty(t, git.commitMsgs)
	assert.Equal(t, "docs(api): document health endpoint", git.commitMsgs[len(git.commitMsgs)-1],
		"commit message parsed from the COMMIT line")
	require.NotEmpty(t, git.pushBranches, "doc commit must be pushed")
	assert.Equal(t, "cm/card-1", git.pushBranches[len(git.pushBranches)-1])
}

func TestDocumentCommitMessageFallback(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &planLLM{responses: []llm.Response{stopResp("Wrote docs but omitted the commit line.", 0.02)}}
	d := documentTestDeps(ops, git, llmFake)

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Docs", Description: "body"}
	o := newDocumentRun(d, tc, 0)

	require.NoError(t, runDocument(context.Background(), o))

	require.NotEmpty(t, git.commitMsgs)
	assert.Equal(t, "docs: update documentation", git.commitMsgs[len(git.commitMsgs)-1],
		"missing COMMIT line falls back to the canonical docs message")
	require.NotEmpty(t, git.pushBranches)
}

func TestDocumentBudgetGate(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	llmFake := &planLLM{}
	d := documentTestDeps(ops, git, llmFake)

	// Already over budget: the gate must park before any model call.
	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Over budget", Description: "body", ReportedCostUSD: 2.0}
	o := newDocumentRun(d, tc, 1.0)

	err := runDocument(context.Background(), o)
	require.Error(t, err)

	var be *BudgetExceededError
	require.ErrorAs(t, err, &be, "budget park is the one error document propagates")

	assert.Empty(t, llmFake.tasks, "model never called once parked")
	assert.Empty(t, git.commitMsgs, "no commit after a budget park")
	assert.Empty(t, git.pushBranches, "no push after a budget park")
}

func TestDocumentBestEffortOnModelError(t *testing.T) {
	t.Run("transport error returns nil, no commit, no push", func(t *testing.T) {
		ops := &fakeOps{}
		git := &fakeGit{committed: true} // would commit if reached; the model-error path must not reach it
		d := documentTestDeps(ops, git, &errLLM{err: errors.New("connection reset")})

		tc := cmclient.TaskContext{CardID: "CARD-1", Title: "T", Description: "body"}
		o := newDocumentRun(d, tc, 0)

		require.NoError(t, runDocument(context.Background(), o), "a model error never fails the run")

		assert.Empty(t, git.commitMsgs, "no commit attempted after a model error")
		assert.Empty(t, git.pushBranches, "no push after a model error")
		assert.True(t, ops.loggedContains("model run failed"), "model failure logged; logs=%v", ops.logs)
	})

	t.Run("context limit is caught, not parked", func(t *testing.T) {
		ops := &fakeOps{}
		git := &fakeGit{committed: true}
		// PromptTokens >= int(0.85*131072) trips context_limit on payload/model,
		// which runModel surfaces as *ContextLimitError. document MUST catch it
		// (return nil) — propagating would make execute() park an otherwise-good run.
		const tripAt = 111411 // int(0.85 * 131072)

		resp := llm.Response{Content: "partial", FinishReason: "stop", Usage: llm.Usage{PromptTokens: tripAt, Cost: 0.01}}
		d := documentTestDeps(ops, git, &planLLM{responses: []llm.Response{resp}})

		tc := cmclient.TaskContext{CardID: "CARD-1", Title: "T", Description: "body"}
		o := newDocumentRun(d, tc, 0)

		// NoError proves the *ContextLimitError did not propagate.
		require.NoError(t, runDocument(context.Background(), o), "a doc-phase context limit is best-effort, never a park")
		assert.Empty(t, git.pushBranches, "no push after a context-limit model error")
	})
}

func TestDocumentBestEffortOnCommitFailure(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{commitErr: errors.New("index locked")}
	llmFake := &planLLM{responses: []llm.Response{stopResp("Docs.\nCOMMIT: docs: x", 0.02)}}
	d := documentTestDeps(ops, git, llmFake)

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "T", Description: "body"}
	o := newDocumentRun(d, tc, 0)

	require.NoError(t, runDocument(context.Background(), o), "a commit failure never fails the run")
	assert.Empty(t, git.pushBranches, "no push when the commit failed")
	assert.True(t, ops.loggedContains("committing docs failed"), "commit failure logged; logs=%v", ops.logs)
}

func TestDocumentBestEffortOnPushFailure(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, pushErr: errors.New("remote rejected")}
	llmFake := &planLLM{responses: []llm.Response{stopResp("Docs.\nCOMMIT: docs: update", 0.02)}}
	d := documentTestDeps(ops, git, llmFake)

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "T", Description: "body"}
	o := newDocumentRun(d, tc, 0)

	require.NoError(t, runDocument(context.Background(), o), "phase invariant: a push failure never fails the run")

	require.NotEmpty(t, git.pushBranches, "push was attempted")
	assert.True(t, ops.loggedContains("pushing docs failed"), "push failure logged; logs=%v", ops.logs)
}

func TestDocumentReportsUsage(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &planLLM{responses: []llm.Response{stopResp("Docs.\nCOMMIT: docs: x", 0.05)}}
	d := documentTestDeps(ops, git, llmFake)

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "T", Description: "body"}
	o := newDocumentRun(d, tc, 0)

	require.NoError(t, runDocument(context.Background(), o))
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "ReportUsage:CARD-1"), 0,
		"doc-phase usage reported; calls=%v", ops.recorded())
}

func TestDocumentPromptContent(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: false}
	llmFake := &planLLM{responses: []llm.Response{stopResp("No docs needed.", 0.01)}}
	d := documentTestDeps(ops, git, llmFake)

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Add the health endpoint", Description: "body"}
	o := newDocumentRun(d, tc, 0)
	o.subtasks = []subtaskRef{{ID: "SUB-1", Title: "Wire the route"}}

	require.NoError(t, runDocument(context.Background(), o))

	require.Len(t, llmFake.tasks, 1, "exactly one document model call")
	task := llmFake.tasks[0]
	assert.Contains(t, task, "Add the health endpoint", "prompt carries the card title")
	assert.Contains(t, task, "Wire the route", "prompt carries the plan overview")
	assert.Contains(t, strings.ToLower(task), "no external documentation is needed", "prompt carries the conservative gate")

	require.Len(t, llmFake.models, 1)
	assert.Equal(t, "payload/model", llmFake.models[0], "document runs on the resolved orchestrator model")

	assert.Contains(t, git.diffBases, "main", "branch diff requested against the base; bases=%v", git.diffBases)
}
