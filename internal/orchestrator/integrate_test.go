package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePR is a scripted PRCreator. It records the args of each Create call and
// returns the configured URL/error.
type fakePR struct {
	mu sync.Mutex

	url string
	err error

	calls []prCall
}

type prCall struct {
	title string
	body  string
	base  string
	head  string
}

func (p *fakePR) Create(_ context.Context, title, body, base, head string) (string, error) {
	p.mu.Lock()
	p.calls = append(p.calls, prCall{title: title, body: body, base: base, head: head})
	p.mu.Unlock()

	return p.url, p.err
}

// integrateTestDeps builds Deps wired for the integrate phase: scripted ops +
// git + PR creator, a stop-response orchestrator LLM, read tools, and the plan
// test registry.
func integrateTestDeps(ops *fakeOps, git *fakeGit, pr PRCreator, client llm.LLM) Deps {
	return Deps{
		Ops:        ops,
		Git:        git,
		PR:         pr,
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

// newIntegrateRun builds a run with the parent task context and ledger cap set.
func newIntegrateRun(d Deps, tc cmclient.TaskContext, maxCost float64) *run {
	d.Cfg.MaxCardCost = maxCost

	return newRun(d, tc)
}

func TestIntegrateHappyPath(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{remoteTip: "deadbeef"}
	pr := &fakePR{}
	d := integrateTestDeps(ops, git, pr, &planLLM{})

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent card", Description: "body", CreatePR: false}
	o := newIntegrateRun(d, tc, 0)

	require.NoError(t, runIntegrate(context.Background(), o))

	// Pins the full git sequence, including RemoteTip recorded BEFORE the rebase.
	git.assertOrder(t, "Fetch:main", "RemoteTip:cm/card-1", "RebaseAutosquash:origin/main", "ForcePushWithLease:cm/card-1")

	// Lease push (tip known), report push, transition to done.
	calls := ops.recorded()
	require.GreaterOrEqual(t, indexOfCall(calls, "ReportPush:CARD-1"), 0, "ReportPush recorded; calls=%v", calls)
	require.GreaterOrEqual(t, indexOfCall(calls, "TransitionCard:done"), 0)
	assert.Less(t, indexOfCall(calls, "ReportPush:CARD-1"), indexOfCall(calls, "TransitionCard:done"),
		"report push before the parent transitions to done")

	// Parent done via transition, NOT CompleteTask.
	assert.Equal(t, -1, indexOfCall(calls, "CompleteTask:CARD-1"), "parent must reach done via transition, not CompleteTask")

	// No PR when CreatePR is false.
	assert.Empty(t, pr.calls, "no PR when CreatePR is false")
}

func TestIntegrateConflictFallback(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{
		remoteTip:      "deadbeef",
		rebaseErr:      ErrRebaseConflict,
		mergeBaseValue: "ba5eba5e",
		committed:      true,
	}
	pr := &fakePR{}
	d := integrateTestDeps(ops, git, pr, &planLLM{})

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Add the thing", Description: "body"}
	o := newIntegrateRun(d, tc, 0)

	require.NoError(t, runIntegrate(context.Background(), o))

	gc := git.recorded()
	// Conflict fallback: merge-base -> soft reset to it -> single squashed commit.
	require.GreaterOrEqual(t, indexOfCall(gc, "MergeBase"), 0, "merge-base looked up; git=%v", gc)
	require.GreaterOrEqual(t, indexOfCall(gc, "SoftReset:ba5eba5e"), 0, "soft reset to merge-base; git=%v", gc)
	require.GreaterOrEqual(t, indexOfCall(gc, "CommitWithMessage"), 0, "squashed commit; git=%v", gc)
	assert.Less(t, indexOfCall(gc, "SoftReset:ba5eba5e"), indexOfCall(gc, "CommitWithMessage"),
		"soft reset must precede the squashed commit")

	// Never auto-resolves: exactly one rebase attempt, no second rebase.
	assert.Equal(t, 1, countCalls(gc, "RebaseAutosquash:origin/main"), "exactly one rebase attempt; git=%v", gc)

	// The squashed commit message is the conventional message derived from the title.
	require.NotEmpty(t, git.commitMsgs)
	assert.Equal(t, "feat: add the thing", git.commitMsgs[0])

	// Still lease-pushes (tip known) after the squash.
	assert.GreaterOrEqual(t, indexOfCall(gc, "ForcePushWithLease:cm/card-1"), 0, "lease push after squash; git=%v", gc)

	// Conflict deferral logged.
	calls := ops.recorded()

	var logged bool

	for _, c := range calls {
		if strings.HasPrefix(c, "AddLog:") && strings.Contains(c, "conflict") {
			logged = true
		}
	}

	assert.True(t, logged, "conflict deferral must be logged; calls=%v", calls)
}

func TestIntegratePRCreation(t *testing.T) {
	t.Run("CreatePR true: orchestrator-written body, pr_url reported", func(t *testing.T) {
		ops := &fakeOps{}
		git := &fakeGit{remoteTip: "deadbeef"}
		pr := &fakePR{url: "https://example.test/pr/7"}
		llmFake := &planLLM{responses: []llm.Response{
			stopResp("## What\nDid the thing\n## Why\nBecause", 0.03),
		}}
		d := integrateTestDeps(ops, git, pr, llmFake)

		tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Add the thing", Description: "body", CreatePR: true}
		o := newIntegrateRun(d, tc, 0)
		o.subtasks = []subtaskRef{{ID: "SUB-1", Title: "First step"}, {ID: "SUB-2", Title: "Second step"}}

		require.NoError(t, runIntegrate(context.Background(), o))

		// The PR-body model was invoked once.
		require.Len(t, llmFake.tasks, 1, "exactly one PR-body LLM call")

		// PR created with the orchestrator-written body.
		require.Len(t, pr.calls, 1)
		assert.Equal(t, "Add the thing", pr.calls[0].title)
		assert.Equal(t, "main", pr.calls[0].base)
		assert.Equal(t, "cm/card-1", pr.calls[0].head)
		assert.Contains(t, pr.calls[0].body, "Did the thing", "PR body is the orchestrator-written text")

		// pr_url flows into ReportPush.
		require.NotEmpty(t, ops.reportPushURLs)
		assert.Equal(t, "https://example.test/pr/7", ops.reportPushURLs[0])

		// The PR-body spend was reported.
		assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "ReportUsage:CARD-1"), 0)
	})

	t.Run("CreatePR false: no PR, no PR-body LLM call", func(t *testing.T) {
		ops := &fakeOps{}
		git := &fakeGit{remoteTip: "deadbeef"}
		pr := &fakePR{}
		llmFake := &planLLM{}
		d := integrateTestDeps(ops, git, pr, llmFake)

		tc := cmclient.TaskContext{CardID: "CARD-1", Title: "No PR card", CreatePR: false}
		o := newIntegrateRun(d, tc, 0)

		require.NoError(t, runIntegrate(context.Background(), o))

		assert.Empty(t, pr.calls, "no PR when CreatePR is false")
		assert.Empty(t, llmFake.tasks, "no PR-body LLM call when CreatePR is false")

		// ReportPush still happens, with an empty pr_url.
		require.NotEmpty(t, ops.reportPushURLs)
		assert.Empty(t, ops.reportPushURLs[0])
	})

	t.Run("PR creation failure: push succeeded, log and continue without PR", func(t *testing.T) {
		ops := &fakeOps{}
		git := &fakeGit{remoteTip: "deadbeef"}
		pr := &fakePR{err: errFakePR}
		llmFake := &planLLM{responses: []llm.Response{stopResp("body", 0.01)}}
		d := integrateTestDeps(ops, git, pr, llmFake)

		tc := cmclient.TaskContext{CardID: "CARD-1", Title: "PR fails", CreatePR: true}
		o := newIntegrateRun(d, tc, 0)

		// PR failure does NOT fail the run — the push already landed.
		require.NoError(t, runIntegrate(context.Background(), o))

		calls := ops.recorded()
		require.GreaterOrEqual(t, indexOfCall(calls, "TransitionCard:done"), 0, "run completes despite PR failure")

		// Reported push carries no URL (PR failed).
		require.NotEmpty(t, ops.reportPushURLs)
		assert.Empty(t, ops.reportPushURLs[0])
	})
}

func TestIntegrateBudgetGate(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{remoteTip: "deadbeef"}
	pr := &fakePR{url: "https://example.test/pr/1"}
	llmFake := &planLLM{responses: []llm.Response{stopResp("body", 0.01)}}
	d := integrateTestDeps(ops, git, pr, llmFake)

	// Already over budget: the PR-body model call must be gated.
	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Over budget", CreatePR: true, ReportedCostUSD: 2.0}
	o := newIntegrateRun(d, tc, 1.0)

	err := runIntegrate(context.Background(), o)
	require.Error(t, err)

	var be *BudgetExceededError
	require.ErrorAs(t, err, &be)

	// No PR-body call, no PR, after a budget park.
	assert.Empty(t, llmFake.tasks, "no PR-body call once parked")
	assert.Empty(t, pr.calls)

	// The park happens before the push is reported or the parent transitions.
	calls := ops.recorded()
	assert.Equal(t, -1, indexOfCall(calls, "ReportPush:CARD-1"), "no ReportPush after a budget park; calls=%v", calls)
	assert.Equal(t, -1, indexOfCall(calls, "TransitionCard:done"), "no transition to done after a budget park; calls=%v", calls)
}

func TestIntegrateIdempotentResume(t *testing.T) {
	ops := &fakeOps{}
	// Resume after a prior successful push: the remote tip is non-empty, so the
	// lease path is taken — the lease push is still issued (a no-op server-side
	// when the remote already matches) and the flow completes.
	git := &fakeGit{remoteTip: "samehash"}
	pr := &fakePR{}
	d := integrateTestDeps(ops, git, pr, &planLLM{})

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Resume", CreatePR: false}
	o := newIntegrateRun(d, tc, 0)

	require.NoError(t, runIntegrate(context.Background(), o))

	gc := git.recorded()
	assert.GreaterOrEqual(t, indexOfCall(gc, "ForcePushWithLease:cm/card-1"), 0,
		"lease push still issued even when the remote already matches; git=%v", gc)
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

func TestIntegrateNoRemoteBranch(t *testing.T) {
	ops := &fakeOps{}
	// No remote branch yet: RemoteTip returns "" -> plain guarded Push.
	git := &fakeGit{remoteTip: ""}
	pr := &fakePR{}
	d := integrateTestDeps(ops, git, pr, &planLLM{})

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "First push", CreatePR: false}
	o := newIntegrateRun(d, tc, 0)

	require.NoError(t, runIntegrate(context.Background(), o))

	gc := git.recorded()
	assert.GreaterOrEqual(t, indexOfCall(gc, "Push:cm/card-1"), 0, "plain push when no remote tip; git=%v", gc)
	assert.Equal(t, -1, indexOfCall(gc, "ForcePushWithLease:cm/card-1"), "no lease push when no remote tip; git=%v", gc)
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "TransitionCard:done"), 0)
}

func TestDonePhase(t *testing.T) {
	ops := &fakeOps{}
	d := Deps{Ops: ops, Cfg: Config{CardID: "CARD-1"}}
	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "All done"}
	o := newRun(d, tc)

	require.NoError(t, runDone(context.Background(), o))

	calls := ops.recorded()
	require.GreaterOrEqual(t, indexOfCall(calls, "ReleaseCard:CARD-1"), 0, "claim released; calls=%v", calls)

	var logged bool

	for _, c := range calls {
		if strings.HasPrefix(c, "AddLog:") {
			logged = true
		}
	}

	assert.True(t, logged, "a completion summary is logged; calls=%v", calls)
}

// TestRunDoneToleratesReleaseError: runDone must not fail the run when
// ReleaseCard returns ErrCardNotClaimed (already released by another path) or a
// transient error — in both cases it should warn and continue, leaving the
// completion log entry still written.
func TestRunDoneToleratesReleaseError(t *testing.T) {
	t.Run("ErrCardNotClaimed is swallowed silently", func(t *testing.T) {
		ops := &fakeOps{releaseCardErr: cmclient.ErrCardNotClaimed}
		d := Deps{Ops: ops, Cfg: Config{CardID: "CARD-1"}}
		tc := cmclient.TaskContext{CardID: "CARD-1", Title: "All done"}
		o := newRun(d, tc)

		err := runDone(context.Background(), o)
		require.NoError(t, err, "ErrCardNotClaimed must not fail runDone")

		calls := ops.recorded()
		require.GreaterOrEqual(t, indexOfCall(calls, "ReleaseCard:CARD-1"), 0)

		var logged bool

		for _, c := range calls {
			if strings.HasPrefix(c, "AddLog:") {
				logged = true
			}
		}

		assert.True(t, logged, "completion log written even when release returns ErrCardNotClaimed")
	})

	t.Run("transient error is warned and swallowed", func(t *testing.T) {
		ops := &fakeOps{releaseCardErr: errors.New("network timeout")}
		d := Deps{Ops: ops, Cfg: Config{CardID: "CARD-1"}}
		tc := cmclient.TaskContext{CardID: "CARD-1", Title: "All done"}
		o := newRun(d, tc)

		err := runDone(context.Background(), o)
		require.NoError(t, err, "transient release error must not fail runDone")

		calls := ops.recorded()
		require.GreaterOrEqual(t, indexOfCall(calls, "ReleaseCard:CARD-1"), 0)

		var logged bool

		for _, c := range calls {
			if strings.HasPrefix(c, "AddLog:") {
				logged = true
			}
		}

		assert.True(t, logged, "completion log written even when release returns a transient error")
	})
}

func TestSquashMessage(t *testing.T) {
	assert.Equal(t, "feat: add the thing", squashMessage("Add the Thing"))
	assert.Equal(t, "feat: untitled", squashMessage("  "))
}

// errFakePR scripts a PRCreator failure.
var errFakePR = errors.New("pr creation failed")

// guard: the PR-body prompt template must reference the plan overview and the
// review outcome so the orchestrator-written body carries them.
func TestPRBodyPromptShape(t *testing.T) {
	low := strings.ToLower(prBodyPrompt)
	assert.Contains(t, low, "what")
	assert.Contains(t, low, "why")
	assert.Contains(t, low, "plan overview")
	assert.Contains(t, low, "review")
}
