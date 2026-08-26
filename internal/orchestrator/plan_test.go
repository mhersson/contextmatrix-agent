package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/mob"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const goodPlanJSON = `{"card_tier":"moderate","subtasks":[` +
	`{"title":"First task","description":"do first","depends_on":[],"tier":"simple"},` +
	`{"title":"Second task","description":"do second","depends_on":[0],"tier":"moderate"}]}`

func TestParsePlan(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		p, err := parsePlan(goodPlanJSON)
		require.NoError(t, err)
		assert.Equal(t, "moderate", p.CardTier)
		require.Len(t, p.Subtasks, 2)
		assert.Equal(t, "First task", p.Subtasks[0].Title)
		assert.Equal(t, "simple", p.Subtasks[0].Tier)
		assert.Equal(t, []int{0}, p.Subtasks[1].DependsOn)
		assert.Equal(t, "moderate", p.Subtasks[1].Tier)
	})

	t.Run("junk wrapped JSON extracts", func(t *testing.T) {
		wrapped := "Here is my plan:\n```json\n" + goodPlanJSON + "\n```\nHope that helps!"
		p, err := parsePlan(wrapped)
		require.NoError(t, err)
		require.Len(t, p.Subtasks, 2)
		assert.Equal(t, "moderate", p.CardTier)
	})

	t.Run("invalid tier", func(t *testing.T) {
		bad := `{"card_tier":"moderate","subtasks":[{"title":"T","description":"d","depends_on":[],"tier":"epic"}]}`
		_, err := parsePlan(bad)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tier")
	})

	t.Run("invalid card_tier", func(t *testing.T) {
		bad := `{"card_tier":"gigantic","subtasks":[{"title":"T","description":"d","depends_on":[],"tier":"simple"}]}`
		_, err := parsePlan(bad)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "card_tier")
	})

	t.Run("critical card_tier accepted", func(t *testing.T) {
		j := `{"card_tier":"critical","subtasks":[{"title":"T","description":"d","depends_on":[],"tier":"critical"}]}`
		p, err := parsePlan(j)
		require.NoError(t, err)
		assert.Equal(t, "critical", p.CardTier)
		assert.Equal(t, "critical", p.Subtasks[0].Tier)
	})

	t.Run("unknown tier still rejected", func(t *testing.T) {
		bad := `{"card_tier":"gigantic","subtasks":[{"title":"T","description":"d","depends_on":[],"tier":"simple"}]}`
		_, err := parsePlan(bad)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "card_tier")
	})

	t.Run("dep index out of range", func(t *testing.T) {
		bad := `{"card_tier":"simple","subtasks":[{"title":"T","description":"d","depends_on":[5],"tier":"simple"}]}`
		_, err := parsePlan(bad)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "depends_on")
	})

	t.Run("forward-only dep rejected", func(t *testing.T) {
		// Subtask 0 depends on subtask 1 (a later index) - forbidden.
		bad := `{"card_tier":"simple","subtasks":[` +
			`{"title":"A","description":"d","depends_on":[1],"tier":"simple"},` +
			`{"title":"B","description":"d","depends_on":[],"tier":"simple"}]}`
		_, err := parsePlan(bad)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "depends_on")
	})

	t.Run("self dep rejected", func(t *testing.T) {
		bad := `{"card_tier":"simple","subtasks":[{"title":"A","description":"d","depends_on":[0],"tier":"simple"}]}`
		_, err := parsePlan(bad)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "depends_on")
	})

	t.Run("empty subtasks rejected", func(t *testing.T) {
		bad := `{"card_tier":"simple","subtasks":[]}`
		_, err := parsePlan(bad)
		require.Error(t, err)
	})

	t.Run("no JSON at all", func(t *testing.T) {
		_, err := parsePlan("I could not produce a plan, sorry.")
		require.Error(t, err)
	})
}

func TestIsTestOnlySubtask(t *testing.T) {
	tests := []struct {
		name  string
		title string
		desc  string
		want  bool
	}{
		{
			name:  "production violation: a listed verb then a tests token later, no path evidence",
			title: "Extend fakeOps transitionCardErr to a per-state map and add stalled-recovery tests",
			want:  true,
		},
		{
			// Re-checked with the production subtask's real description fragment -
			// a single test-file path confirms the title verdict.
			name:  "production violation: confirmed by its real Files: line",
			title: "Extend fakeOps transitionCardErr to a per-state map and add stalled-recovery tests",
			desc:  "Files: internal/worker/worker_test.go",
			want:  true,
		},
		{
			name:  "singular test token still counts",
			title: "Add a regression test for the retry path",
			want:  true,
		},
		{
			name:  "verb present but no tests token",
			title: "Add stalled-state recovery to worker.Run",
			want:  false,
		},
		{
			name:  "no listed verb at all",
			title: "Fix flaky suite timeouts",
			want:  false,
		},
		{
			name:  "code-with-tests title using an unlisted verb",
			title: "Implement stalled-state recovery and its tests",
			want:  false,
		},
		{
			name:  "standalone test-infrastructure work: tests token precedes the verb",
			title: "Tests scaffolding for the retry path; extend coverage",
			want:  false,
		},
		// File-evidence cases: a title in the verb+tests shape names a single
		// subtask that both implements and tests its own deliverable - a listed
		// non-test file in the description clears it regardless of the title.
		{
			name:  "combined code+tests title, description lists the code file too: negative",
			title: "Create the login endpoint and write its handler tests",
			desc:  "Files: internal/api/login.go, internal/api/login_test.go",
			want:  false,
		},
		{
			name:  "combined code+tests title, description lists ONLY the test file: confirmed positive",
			title: "Create the login endpoint and write its handler tests",
			desc:  "Files: internal/api/login_test.go",
			want:  true,
		},
		{
			name:  "combined code+tests title, no path evidence: title verdict stands",
			title: "Create the login endpoint and write its handler tests",
			want:  true,
		},
		{
			name:  "second combined code+tests title, description lists the code file too: negative",
			title: "Extend retry backoff and add jitter tests",
			desc:  "Files: internal/retry/backoff.go, internal/retry/backoff_test.go",
			want:  false,
		},
		{
			name:  "second combined code+tests title, description lists ONLY the test file: confirmed positive",
			title: "Extend retry backoff and add jitter tests",
			desc:  "Files: internal/retry/backoff_test.go",
			want:  true,
		},
		{
			name:  "second combined code+tests title, no path evidence: title verdict stands",
			title: "Extend retry backoff and add jitter tests",
			want:  true,
		},
		// Multi-dot filenames: JS/TS test conventions must register as file
		// evidence - a title the verb list never matches still confirms on a
		// Files: line of nothing but .test./.spec. files.
		{
			name:  "JS test file alone confirms despite a non-matching title",
			title: "Cover the auth flow",
			desc:  "Files: src/auth.test.ts",
			want:  true,
		},
		{
			name:  "JS spec file plus its code file clears",
			title: "Create the auth flow and write its spec",
			desc:  "Files: src/auth.spec.ts, src/auth.ts",
			want:  false,
		},
		// Files:-line scoping: prose routinely NAMES the code under test; only
		// the Files: section is evidence when the label exists.
		{
			name:  "prose mention of the code file does not clear a test-only Files: line",
			title: "Write tests for the parser",
			desc:  "Write tests for the parser in internal/orchestrator/plan.go.\nFiles: internal/orchestrator/plan_test.go",
			want:  true,
		},
		{
			name:  "bulleted Files: section is read across its continuation lines",
			title: "Cover the retry path",
			desc:  "Cover every branch.\nFiles:\n- internal/retry/backoff_test.go\n- internal/retry/jitter_test.go",
			want:  true,
		},
		{
			name:  "Files: section ends at the next label; criteria prose does not clear",
			title: "Write tests for the widget",
			desc:  "Files: internal/widget/widget_test.go\nAcceptance criteria: the behavior in internal/widget/widget.go is fully covered",
			want:  true,
		},
		{
			name:  "code file inside the Files: section still clears",
			title: "Extend the widget and add rendering tests",
			desc:  "Files:\n- internal/widget/widget.go\n- internal/widget/widget_test.go",
			want:  false,
		},
		{
			name:  "blank line after the Files: label falls back to whole-description evidence",
			title: "Extend the widget and add rendering tests",
			desc:  "Files:\n\n- internal/widget/widget.go\n- internal/widget/widget_test.go",
			want:  false,
		},
		{
			name:  "hyphenated label header ends the Files: section",
			title: "Write tests for the widget",
			desc:  "Files: internal/widget/widget_test.go\nNon-goals: do not touch internal/widget/widget.go here",
			want:  true,
		},
		{
			name:  "parenthesized label header ends the Files: section",
			title: "Write tests for the widget",
			desc:  "Files: internal/widget/widget_test.go\nAcceptance criteria (v2): behavior in internal/widget/widget.go is covered",
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isTestOnlySubtask(tt.title, tt.desc))
		})
	}
}

func TestTestSplitViolation(t *testing.T) {
	t.Run("flags a dependent test-only subtask", func(t *testing.T) {
		p := plan{Subtasks: []planSubtask{
			{Title: "Change fakeOps to a per-state map", DependsOn: nil},
			{
				Title:     "Extend fakeOps transitionCardErr to a per-state map and add stalled-recovery tests",
				DependsOn: []int{0},
			},
		}}
		title, ok := testSplitViolation(p)
		require.True(t, ok)
		assert.Equal(t, "Extend fakeOps transitionCardErr to a per-state map and add stalled-recovery tests", title)
	})

	t.Run("empty depends_on never triggers even with a matching title", func(t *testing.T) {
		p := plan{Subtasks: []planSubtask{
			{Title: "Add stalled-recovery tests", DependsOn: nil},
		}}
		_, ok := testSplitViolation(p)
		assert.False(t, ok, "a standalone (non-dependent) subtask is not the forbidden split")
	})

	t.Run("clean plan reports no violation", func(t *testing.T) {
		p := plan{Subtasks: []planSubtask{
			{Title: "Add the flag with its tests", DependsOn: nil},
			{Title: "Wire the flag into the handler", DependsOn: []int{0}},
		}}
		_, ok := testSplitViolation(p)
		assert.False(t, ok)
	})
}

func TestTierStringToRegistryTier(t *testing.T) {
	// Lock the end-to-end mapping: the planner tier strings parsePlan accepts
	// must convert to the matching registry.Tier at selection time. "critical"
	// must reach registry.TierCritical (the 0.90 bar), not the moderate default.
	t.Run("tierOf maps each subtask tier string", func(t *testing.T) {
		assert.Equal(t, registry.TierSimple, tierOf(subtaskRef{Tier: "simple"}))
		assert.Equal(t, registry.TierModerate, tierOf(subtaskRef{Tier: "moderate"}))
		assert.Equal(t, registry.TierComplex, tierOf(subtaskRef{Tier: "complex"}))
		assert.Equal(t, registry.TierCritical, tierOf(subtaskRef{Tier: "critical"}))
	})

	t.Run("tierOf unknown/empty defaults to moderate", func(t *testing.T) {
		assert.Equal(t, registry.TierModerate, tierOf(subtaskRef{Tier: "epic"}))
		assert.Equal(t, registry.TierModerate, tierOf(subtaskRef{Tier: ""}))
	})

	t.Run("tierFromString maps each card tier string", func(t *testing.T) {
		assert.Equal(t, registry.TierSimple, tierFromString("simple"))
		assert.Equal(t, registry.TierModerate, tierFromString("moderate"))
		assert.Equal(t, registry.TierComplex, tierFromString("complex"))
		assert.Equal(t, registry.TierCritical, tierFromString("critical"))
	})

	t.Run("tierFromString unknown/empty defaults to moderate", func(t *testing.T) {
		assert.Equal(t, registry.TierModerate, tierFromString("gigantic"))
		assert.Equal(t, registry.TierModerate, tierFromString(""))
	})
}

// TestPlanRunGetsNoBatchNudge proves the batching nudge is scoped to the coder
// family rather than stamped on the shared per-phase config: the planner
// already groups its lookups, and a nudge there would spend the one-shot
// injection on a phase doing it right. Four consecutive single-read turns - one
// more than the coder needs to earn its nudge - leave the planner unnudged.
func TestPlanRunGetsNoBatchNudge(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{
		taskContext: cmclient.TaskContext{Title: "Parent", Description: "body"},
		createdIDs:  []string{"SUB-1", "SUB-2"},
	}
	llmFake := &planLLM{responses: append(burnResps(4), stopResp(goodPlanJSON, 0.01))}
	d := planTestDeps(ops, llmFake)
	d.Emit = events.NewEmitter(nil, &transcript)

	o := newRun(d, ops.taskContext)
	require.NoError(t, runPlan(context.Background(), o))

	assert.Empty(t, batchNudgeCounts(t, &transcript),
		"the planner is deliberately left unnudged")
}

func TestPlanPhaseCreatesSubtasks(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{Title: "Parent", Description: "body"},
		createdIDs:  []string{"SUB-1", "SUB-2"},
	}
	llmFake := &planLLM{responses: []llm.Response{stopResp(goodPlanJSON, 0.01)}}
	d := planTestDeps(ops, llmFake)

	o := newRun(d, ops.taskContext)
	require.NoError(t, runPlan(context.Background(), o))

	require.Len(t, ops.createCardArgs, 2, "two subtasks must be created")

	// Order respects plan order.
	assert.Equal(t, "First task", ops.createCardArgs[0].title)
	assert.Equal(t, "Second task", ops.createCardArgs[1].title)

	// Parent set on both.
	assert.Equal(t, "CARD-1", ops.createCardArgs[0].parent)
	assert.Equal(t, "CARD-1", ops.createCardArgs[1].parent)

	// First has no deps; second depends on the FIRST CARD'S returned ID.
	assert.Empty(t, ops.createCardArgs[0].dependsOn)
	assert.Equal(t, []string{"SUB-1"}, ops.createCardArgs[1].dependsOn)

	// Run struct carries the resolved subtask refs and the card tier. Body holds
	// the planner's description - the execute phase feeds it to the coder.
	require.Len(t, o.subtasks, 2)
	assert.Equal(t, "SUB-1", o.subtasks[0].ID)
	assert.Equal(t, "SUB-2", o.subtasks[1].ID)
	assert.Equal(t, "do first", o.subtasks[0].Body)
	assert.Equal(t, "do second", o.subtasks[1].Body)
	assert.Equal(t, []string{"SUB-1"}, o.subtasks[1].DependsOnIDs)
	assert.Equal(t, "moderate", o.cardTier)

	// Usage was reported and budget spent.
	assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "ReportUsage:CARD-1"), 0)
	assert.InDelta(t, 0.01, o.ledger.Spent(), 1e-9)
}

func TestPlanPhaseRepairLoop(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{Title: "Parent", Description: "body"},
		createdIDs:  []string{"SUB-1"},
	}
	// First response is junk (no JSON); second is a valid one-subtask plan.
	valid := `{"card_tier":"simple","subtasks":[{"title":"Only","description":"d","depends_on":[],"tier":"simple"}]}`
	llmFake := &planLLM{responses: []llm.Response{
		stopResp("sorry, thinking out loud, no json here", 0.02),
		stopResp(valid, 0.03),
	}}
	d := planTestDeps(ops, llmFake)

	o := newRun(d, ops.taskContext)
	require.NoError(t, runPlan(context.Background(), o))

	// Two harness invocations: the original + one repair turn.
	assert.Len(t, llmFake.tasks, 2, "expected exactly two model calls (original + repair)")

	// The repair prompt must mention the parse error / contract.
	assert.Contains(t, strings.ToLower(llmFake.tasks[1]), "json")

	// Both turns' usage is spent and reported.
	assert.InDelta(t, 0.05, o.ledger.Spent(), 1e-9)

	// Subtask created from the repaired plan.
	require.Len(t, ops.createCardArgs, 1)
	assert.Equal(t, "Only", ops.createCardArgs[0].title)
}

func TestPlanPhaseRepairExhausted(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{Title: "Parent", Description: "body"},
	}
	// Both responses are junk: original + one repair both fail → hard error.
	llmFake := &planLLM{responses: []llm.Response{
		stopResp("nope", 0.01),
		stopResp("still nope", 0.01),
	}}
	d := planTestDeps(ops, llmFake)

	o := newRun(d, ops.taskContext)
	err := runPlan(context.Background(), o)
	require.Error(t, err)

	// Exactly two model calls - no third attempt.
	assert.Len(t, llmFake.tasks, 2)

	// No cards created on hard failure.
	assert.Empty(t, ops.createCardArgs)
}

// offendingTitle is the production title that triggered this validation: a
// planner split a dependent tests-only subtask off its coder subtask despite
// the prompt's absolute rule against it.
const offendingTitle = "Extend fakeOps transitionCardErr to a per-state map and add stalled-recovery tests"

func TestPlanPhaseTestSplitRevisionSucceeds(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{Title: "Parent", Description: "body"},
		createdIDs:  []string{"SUB-1"},
	}
	violating := `{"card_tier":"simple","subtasks":[` +
		`{"title":"Change fakeOps to a per-state map","description":"d","depends_on":[],"tier":"simple"},` +
		`{"title":"` + offendingTitle + `","description":"d","depends_on":[0],"tier":"simple"}]}`
	clean := `{"card_tier":"simple","subtasks":[` +
		`{"title":"Change fakeOps to a per-state map with its own tests","description":"d","depends_on":[],"tier":"simple"}]}`

	llmFake := &planLLM{responses: []llm.Response{
		stopResp(violating, 0.02),
		stopResp(clean, 0.01),
	}}
	d := planTestDeps(ops, llmFake)

	o := newRun(d, ops.taskContext)
	require.NoError(t, runPlan(context.Background(), o))

	// Exactly one revision round: the original draft plus one re-prompt.
	require.Len(t, llmFake.tasks, 2, "expected exactly two model calls (draft + one revision)")

	// The revision prompt names the offending subtask, carries the FULL
	// previous plan (the revision run is stateless - without it, "fold its
	// work into the subtask it depends on" names structure the model cannot
	// see), and instructs resubmitting the full plan.
	revisionPrompt := llmFake.tasks[1]
	assert.Contains(t, revisionPrompt, offendingTitle)
	assert.Contains(t, revisionPrompt, "Your previous plan")
	assert.Contains(t, revisionPrompt, "Change fakeOps to a per-state map",
		"the whole previous plan rides in the prompt, not just the offending subtask")
	assert.Contains(t, revisionPrompt, "fold its work into the subtask it depends on")
	assert.Contains(t, revisionPrompt, "resubmit the full corrected plan")

	// The revised (clean) plan is what actually got created - not the violating draft.
	require.Len(t, ops.createCardArgs, 1)
	assert.Equal(t, "Change fakeOps to a per-state map with its own tests", ops.createCardArgs[0].title)

	// The revision request is status-logged, naming the offending subtask.
	assert.True(t, ops.loggedContains(offendingTitle), "revision request logged; logs=%v", ops.logs)
}

// TestReviseTestSplitUsesDraftPlanRegistry proves the test-split revision
// call - reviseTestSplit's own runModelPlan call - reuses the exact registry
// draftPlan resolved for the first attempt, not d.ReadTools. It asserts on
// req.Tools' length via planLLM.toolCountsSeen for both calls: the plan
// registry here carries one more tool than ReadTools (the findings tool), so
// a regression that reverts reviseTestSplit's model call back to d.ReadTools
// changes the revision call's observed tool count and fails this test - a
// counting factory alone cannot catch that, since the factory still runs
// exactly once either way.
func TestReviseTestSplitUsesDraftPlanRegistry(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{Title: "Parent", Description: "body"},
		createdIDs:  []string{"SUB-1"},
	}
	violating := `{"card_tier":"simple","subtasks":[` +
		`{"title":"Change fakeOps to a per-state map","description":"d","depends_on":[],"tier":"simple"},` +
		`{"title":"` + offendingTitle + `","description":"d","depends_on":[0],"tier":"simple"}]}`
	clean := `{"card_tier":"simple","subtasks":[` +
		`{"title":"Change fakeOps to a per-state map with its own tests","description":"d","depends_on":[],"tier":"simple"}]}`

	llmFake := &planLLM{responses: []llm.Response{
		stopResp(violating, 0.02),
		stopResp(clean, 0.01),
	}}
	d := planTestDeps(ops, llmFake)

	planReg := tools.NewRegistry(tools.NewReadTool("."), NewFindingsTool())
	d.PlanTools = func() *tools.Registry { return planReg }

	o := newRun(d, ops.taskContext)
	require.NoError(t, runPlan(context.Background(), o))

	require.Len(t, llmFake.tasks, 2, "draft + one revision")

	toolCounts := llmFake.toolCountsSeen()
	require.Len(t, toolCounts, 2)
	assert.Equal(t, len(planReg.All()), toolCounts[0], "the draft call uses the plan registry")
	assert.Equal(t, len(planReg.All()), toolCounts[1],
		"the revision call must reuse the SAME plan registry as the draft, not fall back to d.ReadTools")
	assert.NotEqual(t, len(d.ReadTools.All()), toolCounts[1],
		"sanity: ReadTools and the plan registry differ in size, or this test proves nothing")
}

func TestPlanPhaseTestSplitRevisionWarnsAndProceedsOnRepeatViolation(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{Title: "Parent", Description: "body"},
		createdIDs:  []string{"SUB-1", "SUB-2"},
	}
	violating1 := `{"card_tier":"simple","subtasks":[` +
		`{"title":"Change fakeOps to a per-state map","description":"d","depends_on":[],"tier":"simple"},` +
		`{"title":"` + offendingTitle + `","description":"d","depends_on":[0],"tier":"simple"}]}`
	// A different offending plan on the revision turn - still a violation, so the
	// run must give up on revising and keep the ORIGINAL draft, not this one.
	violating2 := `{"card_tier":"simple","subtasks":[` +
		`{"title":"Change the widget renderer","description":"d","depends_on":[],"tier":"simple"},` +
		`{"title":"Extend the widget renderer and add rendering tests","description":"d","depends_on":[0],"tier":"simple"}]}`

	llmFake := &planLLM{responses: []llm.Response{
		stopResp(violating1, 0.02),
		stopResp(violating2, 0.02),
	}}
	d := planTestDeps(ops, llmFake)

	o := newRun(d, ops.taskContext)
	require.NoError(t, runPlan(context.Background(), o), "a repeat heuristic violation must never fail the run")

	// No third model call - one revision attempt, then give up.
	require.Len(t, llmFake.tasks, 2)

	// The ORIGINAL plan (first draft) is what actually got created, not the
	// still-violating revision.
	require.Len(t, ops.createCardArgs, 2)
	assert.Equal(t, "Change fakeOps to a per-state map", ops.createCardArgs[0].title)
	assert.Equal(t, offendingTitle, ops.createCardArgs[1].title)

	// Both the revision request and the warn-and-proceed are status-logged.
	assert.True(t, ops.loggedContains(offendingTitle), "revision request logged; logs=%v", ops.logs)
	assert.True(t, ops.loggedContains("proceeding with the original plan"),
		"warn-and-proceed logged; logs=%v", ops.logs)
}

// TestReviseTestSplitBudgetCheckFailureKeepsOriginalPlan proves a budget
// ceiling that blocks the revision call does not discard the already-parsed
// original plan p: reviseTestSplit's every OTHER failure mode (a failed
// revision call, an unparseable revision, a repeat violation) warns and falls
// back to p, and the budget check is no exception - p is paid for, and
// execute's own ledger check at its first subtask parks the run anyway if the
// budget is genuinely exhausted.
func TestReviseTestSplitBudgetCheckFailureKeepsOriginalPlan(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{Title: "Parent", Description: "body"},
	}
	d := planTestDeps(ops, &planLLM{})
	o := newRun(d, ops.taskContext)
	// Seed the ledger already at its ceiling so the revision call's pre-check parks.
	o.ledger = NewLedger(0.01, 0.02)

	p := plan{CardTier: "simple", Subtasks: []planSubtask{
		{Title: "Change fakeOps to a per-state map", Description: "d", DependsOn: nil, Tier: "simple"},
		{Title: offendingTitle, Description: "d", DependsOn: []int{0}, Tier: "simple"},
	}}

	revised, err := o.reviseTestSplit(context.Background(), d.ReadTools, "model", "", "", "", "", "", p)
	require.NoError(t, err, "a budget check that blocks the revision call must not fail the run")
	assert.Equal(t, p, revised, "the paid-for original plan is kept, not discarded")
	assert.True(t, ops.loggedContains("revision budget check failed"),
		"the fallback is status-logged; logs=%v", ops.logs)
}

// TestReviseMobTestSplit proves mob-drafted plans get the same test-split
// validation as solo drafts, through the mob's own channel: one re-opened
// feedback round (non-blind, the panel sees its own plan and the finding),
// the revised plan adopted when clean, and the original kept when the
// violation persists or the round fails.
func TestReviseMobTestSplit(t *testing.T) {
	violating := plan{CardTier: "simple", Subtasks: []planSubtask{
		{Title: "Change fakeOps to a per-state map", Description: "d", Tier: "simple"},
		{Title: offendingTitle, Description: "d", DependsOn: []int{0}, Tier: "simple"},
	}}
	cleanJSON := `{"card_tier":"simple","subtasks":[` +
		`{"title":"Change fakeOps to a per-state map with its own tests","description":"d","depends_on":[],"tier":"simple"}]}`

	t.Run("revision round fixes the split", func(t *testing.T) {
		ops := &fakeOps{}
		o := mobTestRun(ops, MobConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 2.0)
		eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: cleanJSON}}}
		o.mobEngine = eng.run

		prior := &mob.Outcome{Synthesis: "prior synthesis"}

		revised, rout := o.reviseMobTestSplit(context.Background(), "", "", violating, prior)

		require.Len(t, eng.topics, 1, "exactly one revision round")
		assert.Equal(t, 1, eng.topics[0].Rounds, "the revision reuses the one-round adjust mechanism")
		assert.False(t, eng.topics[0].Blind, "the panel must see its own plan and the finding")
		assert.Contains(t, eng.topics[0].Briefing, "tests-ship-with-code",
			"the briefing carries the validation finding")
		assert.Contains(t, eng.topics[0].Briefing, offendingTitle)

		require.Len(t, revised.Subtasks, 1, "the revised plan is adopted")
		assert.Equal(t, "Change fakeOps to a per-state map with its own tests", revised.Subtasks[0].Title)
		require.NotNil(t, rout)
		assert.JSONEq(t, cleanJSON, rout.Synthesis, "the revised outcome replaces the prior one")
	})

	t.Run("clean plan passes through with no engine call", func(t *testing.T) {
		ops := &fakeOps{}
		o := mobTestRun(ops, MobConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 2.0)
		eng := &scriptedEngine{}
		o.mobEngine = eng.run

		clean := plan{Subtasks: []planSubtask{{Title: "Add the flag with its tests", Description: "d"}}}
		prior := &mob.Outcome{Synthesis: "prior synthesis"}

		got, rout := o.reviseMobTestSplit(context.Background(), "", "", clean, prior)

		assert.Empty(t, eng.topics, "no violation, no revision round")
		assert.Equal(t, clean, got)
		assert.Same(t, prior, rout)
	})

	t.Run("persisting violation keeps the original plan", func(t *testing.T) {
		ops := &fakeOps{}
		o := mobTestRun(ops, MobConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 2.0)

		stillViolating := `{"card_tier":"simple","subtasks":[` +
			`{"title":"Change the widget renderer","description":"d","depends_on":[],"tier":"simple"},` +
			`{"title":"Extend the widget renderer and add rendering tests","description":"d","depends_on":[0],"tier":"simple"}]}`
		eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: stillViolating}}}
		o.mobEngine = eng.run

		prior := &mob.Outcome{Synthesis: "prior synthesis"}

		got, rout := o.reviseMobTestSplit(context.Background(), "", "", violating, prior)

		require.Len(t, eng.topics, 1, "one revision attempt, then give up")
		assert.Equal(t, violating, got, "the original plan is kept, not the still-violating revision")
		assert.Same(t, prior, rout, "the original outcome is kept with the original plan")
		assert.True(t, ops.loggedContains("proceeding with the original plan"),
			"the fallback is status-logged; logs=%v", ops.logs)
	})

	t.Run("failed revision round keeps the original plan", func(t *testing.T) {
		ops := &fakeOps{}
		o := mobTestRun(ops, MobConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 2.0)
		eng := &scriptedEngine{outcomes: []mob.Outcome{{}}, errs: []error{errors.New("engine boom")}}
		o.mobEngine = eng.run

		prior := &mob.Outcome{Synthesis: "prior synthesis"}

		got, rout := o.reviseMobTestSplit(context.Background(), "", "", violating, prior)

		assert.Equal(t, violating, got)
		assert.Same(t, prior, rout)
		assert.True(t, ops.loggedContains("proceeding with the original plan"),
			"the fallback is status-logged; logs=%v", ops.logs)
	})
}

func TestPlanPhaseResume(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{Title: "Parent", Description: "body"},
		createdIDs:  []string{"SUB-1", "SUB-2"},
	}
	llmFake := &planLLM{responses: []llm.Response{stopResp(goodPlanJSON, 0.01)}}
	d := planTestDeps(ops, llmFake)

	o := newRun(d, ops.taskContext)
	// The planner reuse list is fed from the RECONCILED refs (set by reconcile in
	// the plan-resume case), NOT a fresh SubtaskStates call inside runPlan.
	o.subtasks = []subtaskRef{
		{ID: "SUB-OLD-1", Title: "Existing subtask alpha", State: "in_progress", Tier: "moderate"},
		{ID: "SUB-OLD-2", Title: "Existing subtask beta", State: "todo", Tier: "moderate"},
	}

	require.NoError(t, runPlan(context.Background(), o))

	require.NotEmpty(t, llmFake.tasks)
	prompt := llmFake.tasks[0]
	assert.Contains(t, prompt, "Existing subtask alpha", "resume prompt must list existing subtask titles")
	assert.Contains(t, prompt, "Existing subtask beta")

	// runPlan must NOT call SubtaskStates itself - the reconciled list is the
	// source of truth for the reuse block.
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "SubtaskStates:proj/CARD-1"),
		"runPlan must consume the reconciled refs, not re-call SubtaskStates")
}

func TestPlanPhaseDiagnosesBugLikeCard(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{
			Title: "Fix the broken parser", Description: "it throws on empty input",
		},
		createdIDs: []string{"SUB-1", "SUB-2"},
	}
	// Call 0 is the diagnose pass (returns a ## Diagnosis blob); call 1 is the
	// planner (returns a valid plan). The diagnosis must be threaded into the
	// plan prompt.
	diagnosis := "## Diagnosis\n### Root cause\nThe parser dereferences a nil slice on empty input.\n"
	llmFake := &planLLM{responses: []llm.Response{
		stopResp(diagnosis, 0.02),
		stopResp(goodPlanJSON, 0.03),
	}}
	d := planTestDeps(ops, llmFake)

	o := newRun(d, ops.taskContext)
	require.NoError(t, runPlan(context.Background(), o))

	// Two model calls: diagnose then plan.
	require.Len(t, llmFake.tasks, 2, "bug-like card must run the diagnose step then the plan")

	// The bug-like card triggers a diagnose run, and the diagnosis text is
	// threaded into the plan prompt.
	assert.True(t, ops.loggedContains("root-cause investigation"),
		"bug-like card must log the diagnose step")
	assert.Contains(t, llmFake.tasks[1], "Root cause", "plan prompt must carry the diagnosis")

	// Both turns' usage is spent.
	assert.InDelta(t, 0.05, o.ledger.Spent(), 1e-9)
}

func TestPlanPhaseSkipsDiagnoseForFeatureCard(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{
			Title: "Add a health endpoint", Description: "expose /healthz", Type: "task",
		},
		createdIDs: []string{"SUB-1", "SUB-2"},
	}
	llmFake := &planLLM{responses: []llm.Response{stopResp(goodPlanJSON, 0.01)}}
	d := planTestDeps(ops, llmFake)

	o := newRun(d, ops.taskContext)
	require.NoError(t, runPlan(context.Background(), o))

	// A non-bug card skips the diagnose step: exactly one model call (the plan).
	require.Len(t, llmFake.tasks, 1, "feature card must make exactly one model call (no diagnose)")
	assert.False(t, ops.loggedContains("root-cause investigation"),
		"feature card must not run the diagnose step")
	assert.NotContains(t, llmFake.tasks[0], "ground the plan in this",
		"feature card plan prompt must not carry an injected diagnosis block")
}

// TestPlanPhaseDiagnoseIncapableModelIsRecovered: a model that cannot drive
// the tool loop in the diagnose step is blacklisted and excluded right there,
// and the plan does not proceed on the same model - the run that motivated
// this re-used such a model as seat, moderator and first coder.
func TestPlanPhaseDiagnoseIncapableModelIsRecovered(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{
			Title: "Fix the broken parser", Description: "it throws on empty input",
		},
		createdIDs: []string{"SUB-1", "SUB-2"},
	}
	d := planTestDeps(ops, nil)
	// planTestRegistry carries no priors, so every decision pick degrades to the
	// capable default and excluding it cannot change the answer. The reviewer
	// registry scores four reviewer-tier models, so the re-selection lands on a
	// different one and the "not the incapable model" assertion is not vacuous.
	d.Registry = reviewerRegistry()

	// The decision model the registry resolves first is incapable; the
	// registry's next pick must answer the plan.
	first := resolveDecisionModel(context.Background(), d.Registry, d.Emit, ops, "CARD-1",
		"", d.Cfg.PayloadModel, d.Cfg.DefaultModel, nil)
	llmFake := &modelAwareLLM{
		incapable: map[string]bool{first: true},
		responses: []llm.Response{stopResp(goodPlanJSON, 0.03)},
	}
	d.Client = llmFake

	o := newRun(d, ops.taskContext)
	require.NoError(t, runPlan(context.Background(), o))

	assert.Contains(t, ops.recorded(), "BlacklistModel:CARD-1/"+first,
		"the diagnose-incapable model is blacklisted; calls=%v", ops.recorded())
	assert.True(t, o.excluded[first], "and excluded for the rest of the run")

	models := llmFake.recordedModels()
	require.NotEmpty(t, models)
	assert.NotEqual(t, first, models[len(models)-1],
		"the plan did not run on the incapable model; models=%v", models)
	assert.True(t, ops.loggedContains("planning without a diagnosis"), "logs=%v", ops.recorded())
	assert.True(t, ops.loggedContains("re-selected after diagnose"),
		"the new model differs from the first, so the re-selection path is logged; logs=%v", ops.recorded())
}

func TestResolveOrchestratorModel(t *testing.T) {
	reg := planTestRegistry()
	emit := events.NewEmitter(nil, nil)

	t.Run("card pin honoured when catalog-resolvable", func(t *testing.T) {
		ops := &fakeOps{}
		got := resolveOrchestratorModel(context.Background(), reg, emit, ops, "CARD-1",
			"pinned/model", "payload/model", "default/model")
		assert.Equal(t, "pinned/model", got)
	})

	t.Run("unresolvable pin falls back to payload model with warning", func(t *testing.T) {
		ops := &fakeOps{}
		got := resolveOrchestratorModel(context.Background(), reg, emit, ops, "CARD-1",
			"ghost/model", "payload/model", "default/model")
		assert.Equal(t, "payload/model", got)

		// A warning note must be logged to the card - specifically an AddLog
		// entry naming the unresolvable pin.
		var addLogs []string

		for _, c := range ops.recorded() {
			if strings.HasPrefix(c, "AddLog:") {
				addLogs = append(addLogs, c)
			}
		}

		require.Len(t, addLogs, 1, "exactly one AddLog warning expected")
		assert.Contains(t, addLogs[0], "ghost/model")
		assert.Contains(t, addLogs[0], "payload/model")
	})

	t.Run("no pin uses payload model", func(t *testing.T) {
		ops := &fakeOps{}
		got := resolveOrchestratorModel(context.Background(), reg, emit, ops, "CARD-1",
			"", "payload/model", "default/model")
		assert.Equal(t, "payload/model", got)
	})

	t.Run("no pin no payload uses default", func(t *testing.T) {
		ops := &fakeOps{}
		got := resolveOrchestratorModel(context.Background(), reg, emit, ops, "CARD-1",
			"", "", "default/model")
		assert.Equal(t, "default/model", got)
	})
}

func TestResolveDecisionModelFloorsWeakPayload(t *testing.T) {
	reg := reviewerRegistry()
	emit := events.NewEmitter(nil, nil)
	ops := &fakeOps{}

	got := resolveDecisionModel(context.Background(), reg, emit, ops, "CARD-1",
		"", "payload/model", "default/model", nil)

	assert.Equal(t, "rev/alpha", got)
	assert.NotEqual(t, "payload/model", got)
	assert.NotEqual(t, "default/model", got)
}

func TestResolveDecisionModelHonorsPin(t *testing.T) {
	reg := reviewerRegistry()
	emit := events.NewEmitter(nil, nil)
	ops := &fakeOps{}

	got := resolveDecisionModel(context.Background(), reg, emit, ops, "CARD-1",
		"pinned/model", "payload/model", "default/model", nil)

	assert.Equal(t, "pinned/model", got)
}

func TestResolveDecisionModelUnresolvablePinFloorsAndWarns(t *testing.T) {
	reg := reviewerRegistry()
	emit := events.NewEmitter(nil, nil)
	ops := &fakeOps{}

	got := resolveDecisionModel(context.Background(), reg, emit, ops, "CARD-1",
		"ghost/model", "payload/model", "default/model", nil)

	assert.Equal(t, "rev/alpha", got)

	var addLogs []string

	for _, c := range ops.recorded() {
		if strings.HasPrefix(c, "AddLog:") {
			addLogs = append(addLogs, c)
		}
	}

	require.Len(t, addLogs, 1)
	assert.Contains(t, addLogs[0], "ghost/model")
}

func TestResolveDecisionModelNilRegistryFallsBack(t *testing.T) {
	emit := events.NewEmitter(nil, nil)
	ops := &fakeOps{}

	got := resolveDecisionModel(context.Background(), nil, emit, ops, "CARD-1",
		"", "payload/model", "default/model", nil)

	assert.Equal(t, "payload/model", got)
}

func TestResolveDecisionModelEmptyPoolReturnsCapableDefault(t *testing.T) {
	reg := registry.NewRegistryFromParts(reviewerCatalog(), registry.Priors{}, nil, nil, "default/model")
	emit := events.NewEmitter(nil, nil)
	ops := &fakeOps{}

	got := resolveDecisionModel(context.Background(), reg, emit, ops, "CARD-1",
		"", "payload/model", "default/model", nil)

	assert.Equal(t, "default/model", got)
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name, in, want string
		ok             bool
	}{
		{"plain", `{"approved":true}`, `{"approved":true}`, true},
		{"fenced after prose", "Verdict.\n```json\n{\"approved\":true,\"fixes\":[]}\n```", `{"approved":true,"fixes":[]}`, true},
		{"brace in code before fenced json", "if m.conns >= m.max { m.mu.Unlock() }\n```json\n{\"approved\":false}\n```", `{"approved":false}`, true},
		{"brace in prose then json, unfenced", "foo { bar } then {\"approved\":false}", `{"approved":false}`, true},
		{"nested object", `pre {"a":{"b":1}} post`, `{"a":{"b":1}}`, true},
		{
			"bare object with in-string fence (compact)",
			"{\"desc\":\"use ```go\\nfunc foo() {}\\n``` here\",\"ok\":true}",
			"{\"desc\":\"use ```go\\nfunc foo() {}\\n``` here\",\"ok\":true}",
			true,
		},
		{
			"bare pretty object with fences in two string values",
			"{\n  \"a\": \"x: ```go\\nfunc a() {}\\n``` end\",\n  \"b\": \"y: ```go\\nfunc b() {}\\n``` end\"\n}",
			"{\"a\":\"x: ```go\\nfunc a() {}\\n``` end\",\"b\":\"y: ```go\\nfunc b() {}\\n``` end\"}",
			true,
		},
		{"no object", "no json here", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractJSON(tt.in)
			assert.Equal(t, tt.ok, ok)

			if tt.ok {
				assert.JSONEq(t, tt.want, got)
			}
		})
	}
}

// TestParsePlanBareJSONWithInStringFences mirrors the live failure payload: a
// bare, valid JSON plan whose subtask descriptions carry fenced code blocks
// must parse on attempt 0 with the fenced content intact.
func TestParsePlanBareJSONWithInStringFences(t *testing.T) {
	raw := "{\n" +
		"  \"card_tier\": \"simple\",\n" +
		"  \"subtasks\": [\n" +
		"    {\"title\": \"Add fast path\", \"description\": \"Guard extraction: ```go\\nif json.Valid(b) {\\n\\treturn s, true\\n}\\n``` before fence stripping.\", \"depends_on\": [], \"tier\": \"simple\"},\n" +
		"    {\"title\": \"Add regression tests\", \"description\": \"Cover payloads like ```go\\nfunc a() {}\\n``` inside descriptions.\", \"depends_on\": [0], \"tier\": \"moderate\"}\n" +
		"  ]\n" +
		"}"

	p, err := parsePlan(raw)
	require.NoError(t, err)
	assert.Equal(t, "simple", p.CardTier)
	require.Len(t, p.Subtasks, 2)
	assert.Contains(t, p.Subtasks[0].Description, "```go\nif json.Valid(b) {\n\treturn s, true\n}\n```")
	assert.Equal(t, []int{0}, p.Subtasks[1].DependsOn)
}

// hitlPlanRun builds a *run for HITL plan-phase tests. The card has a ## Design
// already, so brainstorm is skipped and the plan-approval gate is exercised in
// isolation. client serves the planner draft(s) AND the gate classification(s).
func hitlPlanRun(ops *fakeOps, inbox *fakeInbox, client llm.LLM) *run {
	d := Deps{
		Ops:       ops,
		Client:    client,
		Emit:      events.NewEmitter(nil, nil),
		Registry:  planTestRegistry(),
		ReadTools: tools.NewRegistry(tools.NewReadTool(".")),
		Human:     inbox,
		Cfg: Config{
			Project: "proj", CardID: "CARD-1",
			PayloadModel: "payload/model", DefaultModel: "default/model",
			MaxTurns: 20, Interactive: true, // > wrapUpTurns so the nudge never fires on these 1-turn fakes
		},
	}

	tc := cmclient.TaskContext{
		Title:       "Add a palette",
		Description: "## Design\n\nA palette config.", // present -> brainstorm skipped
	}

	return newRun(d, tc)
}

const onePlanJSON = `{"card_tier":"simple","subtasks":[{"title":"Add the flag","description":"Files: a.go","depends_on":[],"tier":"simple"}]}`

func TestRunPlanHITLApproveCreatesSubtasks(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "approve"}}}
	client := &planLLM{responses: []llm.Response{
		stopResp(onePlanJSON, 0.01),                            // draft
		stopResp(`{"verdict":"approve","feedback":""}`, 0.001), // gate classify
	}}
	o := hitlPlanRun(ops, inbox, client)

	require.NoError(t, runPlan(context.Background(), o))
	assert.Len(t, ops.createCardArgs, 1, "subtasks created after approval")
}

// TestRunPlanHITLPromoteContinuesAsApprove pins that a promotion at the
// plan-approval gate creates the subtasks and moves on - autonomous runs never
// gate the plan. The script has no gate classification response: a promotion
// consumes no human turn and no model call.
func TestRunPlanHITLPromoteContinuesAsApprove(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{} // empty, non-blocking -> the gate Wait reports ErrInboxClosed
	client := &planLLM{responses: []llm.Response{
		stopResp(onePlanJSON, 0.01), // draft only
	}}
	o := hitlPlanRun(ops, inbox, client)

	require.NoError(t, runPlan(context.Background(), o))
	assert.Len(t, ops.createCardArgs, 1, "a promoted plan gate creates the subtasks")
	assert.True(t, ops.loggedContains("promoted"), "promote logged; logs=%v", ops.logs)
}

// autoPlanRun builds an autonomous (non-interactive) *run for planner tests: a
// plain, non-bug-like card so runPlan goes straight to draft → createSubtasks
// with no brainstorm or diagnose detour consuming a scripted response.
func autoPlanRun(ops *fakeOps, client llm.LLM, maxTurns int) *run {
	d := Deps{
		Ops:       ops,
		Client:    client,
		Emit:      events.NewEmitter(nil, nil),
		Registry:  planTestRegistry(),
		ReadTools: tools.NewRegistry(tools.NewReadTool(".")),
		Cfg: Config{
			Project: "proj", CardID: "CARD-1",
			PayloadModel: "payload/model", DefaultModel: "default/model",
			MaxTurns: maxTurns, Interactive: false,
		},
	}

	tc := cmclient.TaskContext{
		Title:       "Add a config flag",
		Description: "Add a config flag to toggle the feature.",
	}

	return newRun(d, tc)
}

// TestRunPlanGetsWrapUpNudge proves the planner opts into the wrap-up nudge:
// when the run burns down to wrapUpTurns remaining, the plan-specific nudge is
// injected as a user message, steering the model to emit its JSON plan before
// the cap instead of exploring into it.
func TestRunPlanGetsWrapUpNudge(t *testing.T) {
	ops := &fakeOps{}
	// Three burn turns, then the plan JSON: with MaxTurns=8 the nudge fires
	// after 8-5=3 consumed turns, before the model emits its plan.
	client := &planLLM{responses: []llm.Response{
		burnResp(""), burnResp(""), burnResp(""),
		stopResp(onePlanJSON, 0.01),
	}}
	o := autoPlanRun(ops, client, 8)

	require.NoError(t, runPlan(context.Background(), o))
	assert.Len(t, ops.createCardArgs, 1, "the plan is emitted and its subtask created")

	joined := strings.Join(client.tasks, "\n")
	assert.Contains(t, joined, planWrapUpMessage,
		"the wrap-up nudge reaches the planner conversation as a user message")
}

func TestRunPlanHITLAdjustRedraftsThenApproves(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{
		{Content: "make it two subtasks"},
		{Content: "approve"},
	}}
	llmFake := &planLLM{responses: []llm.Response{
		stopResp(onePlanJSON, 0.01),                                       // draft 1
		stopResp(`{"verdict":"adjust","feedback":"two subtasks"}`, 0.001), // gate -> adjust
		stopResp(onePlanJSON, 0.01),                                       // draft 2 (re-draft)
		stopResp(`{"verdict":"approve","feedback":""}`, 0.001),            // gate -> approve
	}}
	o := hitlPlanRun(ops, inbox, llmFake)

	require.NoError(t, runPlan(context.Background(), o))
	assert.Len(t, ops.createCardArgs, 1, "subtasks created only after the final approval")

	// The re-draft prompt carried the human's feedback.
	var sawFeedback bool

	for _, task := range llmFake.tasks {
		if strings.Contains(task, "REQUESTED CHANGES") && strings.Contains(task, "two subtasks") {
			sawFeedback = true
		}
	}

	assert.True(t, sawFeedback, "the re-draft prompt includes the adjust feedback")
}

// creativePlanRun builds a *run for the design-grounding test: a creative HITL
// card with NO ## Design yet, so brainstorm runs and the produced design must
// reach the planner prompt.
func creativePlanRun(ops *fakeOps, inbox *fakeInbox, client llm.LLM) *run {
	d := Deps{
		Ops:       ops,
		Client:    client,
		Emit:      events.NewEmitter(nil, nil),
		Registry:  planTestRegistry(),
		ReadTools: tools.NewRegistry(tools.NewReadTool(".")),
		Human:     inbox,
		Cfg: Config{
			Project: "proj", CardID: "CARD-1",
			PayloadModel: "payload/model", DefaultModel: "default/model",
			MaxTurns: 20, Interactive: true, // > wrapUpTurns so the nudge never fires on these 1-turn fakes
		},
	}

	tc := cmclient.TaskContext{
		Title:       "Add a palette",
		Description: "Add colour-scheme support.", // no ## Design → brainstorm runs
	}

	return newRun(d, tc)
}

// TestRunPlanHITLDesignReachesPlanner proves the fresh-run/resume asymmetry is
// fixed: the brainstormed design must appear in the planner prompt (not only in
// the card body that is re-fetched on resume).
func TestRunPlanHITLDesignReachesPlanner(t *testing.T) {
	ops := &fakeOps{}
	// The human replies once (to the brainstorm question) and then approves the plan.
	inbox := &fakeInbox{msgs: []harness.UserMessage{
		{Content: "use option A"},
		{Content: "approve"},
	}}

	const agreedDesign = "## Design\n\nApproach A: a 4-slot palette config."

	llmFake := &planLLM{responses: []llm.Response{
		// Brainstorm turn 1: model asks a question.
		stopResp("Which approach: A or B?", 0.01),
		// Brainstorm turn 2: model confirms the design.
		stopResp(agreedDesign+"\n\nDESIGN_COMPLETE", 0.01),
		// Plan draft.
		stopResp(onePlanJSON, 0.01),
		// Gate classify: approve.
		stopResp(`{"verdict":"approve","feedback":""}`, 0.001),
	}}

	o := creativePlanRun(ops, inbox, llmFake)
	require.NoError(t, runPlan(context.Background(), o))

	// Subtasks created after approval.
	assert.Len(t, ops.createCardArgs, 1, "subtasks created after approval")

	// The planner prompt (the plan-draft call) must carry the agreed design.
	// llmFake.tasks captures the last user message of each harness.Run call in
	// order: [brainstorm-q1, brainstorm-q2(design_complete), plan-draft, gate].
	// The plan-draft is tasks[2].
	require.GreaterOrEqual(t, len(llmFake.tasks), 3, "expected at least 3 model calls")
	planDraftPrompt := llmFake.tasks[2]
	assert.Contains(t, planDraftPrompt, "AGREED DESIGN",
		"planner prompt must contain the AGREED DESIGN block")
	assert.Contains(t, planDraftPrompt, "Approach A",
		"planner prompt must contain the brainstormed design text")
}

// mobPlanRun builds an autonomous run with mob session plan enabled and a
// scripted engine.
func mobPlanRun(ops *fakeOps, client llm.LLM, eng *scriptedEngine) *run {
	d := Deps{
		Ops:       ops,
		Git:       &fakeGit{},
		Client:    client,
		Emit:      events.NewEmitter(nil, nil),
		Registry:  reviewerRegistry(),
		ReadTools: tools.NewRegistry(tools.NewReadTool(".")),
		Cfg: Config{
			Project: "proj", CardID: "CARD-1",
			PayloadModel: "payload/model", DefaultModel: "default/model",
			MaxTurns: 20, MaxCardCost: 2.0,
			Mob: MobConfig{Participants: 3, Plan: true, Rounds: 2, BudgetFactor: 0.75},
		},
	}

	tc := cmclient.TaskContext{
		Title:       "Add a config flag",
		Description: "Add a config flag to toggle the feature.",
	}

	o := newRun(d, tc)
	o.mobEngine = eng.run

	return o
}

func TestRunPlanMobCreatesSubtasksFromSynthesis(t *testing.T) {
	ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
	llmFake := &planLLM{}
	eng := &scriptedEngine{outcomes: []mob.Outcome{{
		Transcript: []mob.Entry{
			{Author: "seat-1", Lens: "feasibility/simplicity", Round: 0, Content: "proposal A"},
			{Author: "seat-2", Lens: "architecture/extensibility", Round: 1, Content: "critique"},
		},
		Synthesis: goodPlanJSON,
		Consensus: true,
		CostUSD:   0.10,
	}}}

	o := mobPlanRun(ops, llmFake, eng)
	require.NoError(t, runPlan(context.Background(), o))

	// Subtasks come from the synthesized JSON, through the normal parser.
	require.Len(t, ops.createCardArgs, 2)
	assert.Equal(t, "First task", ops.createCardArgs[0].title)
	assert.Equal(t, "Second task", ops.createCardArgs[1].title)
	assert.Equal(t, "moderate", o.cardTier)

	// The topic carried the plan knobs and the briefing content.
	require.Len(t, eng.topics, 1)
	topic := eng.topics[0]
	assert.Equal(t, "plan", topic.Kind)
	assert.True(t, topic.Blind)
	assert.Equal(t, 2, topic.Rounds)
	assert.Equal(t, planLenses[:3], topic.Lenses)
	assert.Contains(t, topic.Briefing, "Add a config flag")
	assert.Contains(t, topic.SynthesisPrompt, "JSON")

	// No solo planner model call happened.
	assert.Empty(t, llmFake.tasks, "the discussion replaced the solo planner call")

	// ## Discussion recorded AFTER acceptance, naming seats and outcome.
	joined := strings.Join(ops.bodyUpdates, "\n===\n")
	assert.Contains(t, joined, "## Discussion")
	assert.Contains(t, joined, "seat-1")
	assert.Contains(t, joined, "consensus")
}

func TestRunPlanMobRepairSucceeds(t *testing.T) {
	ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
	// The moderator repair call is the only LLM call: it returns valid JSON.
	llmFake := &planLLM{responses: []llm.Response{stopResp(goodPlanJSON, 0.02)}}
	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: "sorry, prose only", Consensus: true}}}

	o := mobPlanRun(ops, llmFake, eng)
	require.NoError(t, runPlan(context.Background(), o))

	require.Len(t, ops.createCardArgs, 2, "subtasks created from the repaired synthesis")
	require.Len(t, llmFake.tasks, 1, "exactly one moderator repair call")
	assert.Contains(t, llmFake.tasks[0], "COULD NOT BE PARSED", "the repair prompt names the parse failure")
}

func TestRunPlanMobParseFailureFallsBackToDraftPlan(t *testing.T) {
	ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
	// Call 1: moderator repair - still junk. Call 2: solo draftPlan - good.
	llmFake := &planLLM{responses: []llm.Response{
		stopResp("still not json", 0.01),
		stopResp(goodPlanJSON, 0.01),
	}}
	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: "prose"}}}

	o := mobPlanRun(ops, llmFake, eng)
	require.NoError(t, runPlan(context.Background(), o))

	require.Len(t, ops.createCardArgs, 2, "the solo fallback produced the plan")
	assert.Len(t, llmFake.tasks, 2, "one repair attempt, then one solo planner call")

	joined := strings.Join(ops.bodyUpdates, "\n===\n")
	assert.NotContains(t, joined, "## Discussion", "no discussion record when the discussion was abandoned")
}

func TestRunPlanMobEngineFailureFallsBackToDraftPlan(t *testing.T) {
	ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
	llmFake := &planLLM{responses: []llm.Response{stopResp(goodPlanJSON, 0.01)}}
	eng := &scriptedEngine{outcomes: []mob.Outcome{{}}, errs: []error{mob.ErrNoQuorum}}

	o := mobPlanRun(ops, llmFake, eng)
	require.NoError(t, runPlan(context.Background(), o))

	require.Len(t, ops.createCardArgs, 2, "quorum failure degrades to the solo path")
	assert.Len(t, llmFake.tasks, 1, "exactly the solo planner call")
}

func TestRunPlanMobHITLAdjustReopensDiscussion(t *testing.T) {
	ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
	inbox := &fakeInbox{msgs: []harness.UserMessage{
		{Content: "make it two subtasks"},
		{Content: "approve"},
	}}
	// LLM calls: gate classify (adjust), then gate classify (approve).
	llmFake := &planLLM{responses: []llm.Response{
		stopResp(`{"verdict":"adjust","feedback":"two subtasks"}`, 0.001),
		stopResp(`{"verdict":"approve","feedback":""}`, 0.001),
	}}
	eng := &scriptedEngine{outcomes: []mob.Outcome{
		{
			Transcript: []mob.Entry{{Author: "seat-1", Lens: "feasibility/simplicity", Round: 0, Content: "proposal"}},
			Synthesis:  goodPlanJSON,
			Consensus:  true,
		},
		{
			Transcript: []mob.Entry{{Author: "seat-1", Lens: "feasibility/simplicity", Round: 1, Content: "revised"}},
			Synthesis:  goodPlanJSON,
			Consensus:  true,
		},
	}}

	o := mobPlanRun(ops, llmFake, eng)
	o.d.Cfg.Interactive = true
	o.d.Human = inbox
	// Pre-existing design section on the claim-time body (what recordedDesign
	// reads) so the creative brainstorm is skipped and both scripted inbox
	// messages reach the plan gate.
	o.tc.Description = "## Design\n\nA config flag."

	require.NoError(t, runPlan(context.Background(), o))

	require.Len(t, eng.topics, 2, "the adjust re-opened the discussion")
	reopen := eng.topics[1]
	assert.False(t, reopen.Blind, "re-open is a critique-style round, not blind")
	assert.Equal(t, 1, reopen.Rounds, "one feedback round")
	assert.Contains(t, reopen.Briefing, "two subtasks", "the human feedback rides the briefing")
	assert.Contains(t, reopen.Briefing, "human:", "feedback appears as a human-authored entry")
	assert.Contains(t, reopen.Briefing, "proposal", "the prior transcript tail restores context")

	assert.Len(t, ops.createCardArgs, 2, "subtasks created only after the final approval")
}

// grownDescription is a resumed card's body as GetTaskContext returns it after
// a park: the human task plus the prior run's recorded history. The findings
// text stays free of bug markers so isBugLike does not flip on it.
const grownDescription = "Add a config flag to toggle the feature.\n\n" +
	"## Design\n\nUse a palette config keyed by name.\n\n" +
	"## Plan\n\n1. SUBTASK: Add the flag\n   Files: a.go\n\n" +
	"## Review Findings\n\n- a.go: naming could improve\n\n### Recommendation\n\nrevise\n"

func resumedAutoPlanRun(ops *fakeOps, client llm.LLM) *run {
	d := Deps{
		Ops:       ops,
		Client:    client,
		Emit:      events.NewEmitter(nil, nil),
		Registry:  planTestRegistry(),
		ReadTools: tools.NewRegistry(tools.NewReadTool(".")),
		Cfg: Config{
			Project: "proj", CardID: "CARD-1",
			PayloadModel: "payload/model", DefaultModel: "default/model",
			MaxTurns: 20, Interactive: false,
		},
	}

	tc := cmclient.TaskContext{Title: "Add a config flag", Description: grownDescription}

	return newRun(d, tc)
}

func TestRunPlanResumeCarriesPlanAndDesignNotHistory(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{stopResp(onePlanJSON, 0.01)}}
	o := resumedAutoPlanRun(ops, client)

	require.NoError(t, runPlan(context.Background(), o))

	task := client.tasks[0]
	assert.Contains(t, task, "Add a config flag to toggle the feature.", "human intro kept")
	assert.Contains(t, task, "AGREED DESIGN", "recovered design flows via the design block")
	assert.Contains(t, task, "Use a palette config keyed by name.")
	assert.Contains(t, task, "1. SUBTASK: Add the flag", "prior plan re-supplied")
	assert.NotContains(t, task, "naming could improve", "review history stripped")
}

func TestRunPlanFreshPromptHasNoResupplyBlocks(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{stopResp(onePlanJSON, 0.01)}}
	o := autoPlanRun(ops, client, 20)

	require.NoError(t, runPlan(context.Background(), o))

	task := client.tasks[0]
	assert.Contains(t, task, "Add a config flag to toggle the feature.")
	assert.NotContains(t, task, "AGREED DESIGN")
}

func TestRunPlanHumanDesignReachesPlannerViaDesignBlock(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "approve"}}}
	client := &planLLM{responses: []llm.Response{
		stopResp(onePlanJSON, 0.01),
		stopResp(`{"verdict":"approve","feedback":""}`, 0.001),
	}}
	o := hitlPlanRun(ops, inbox, client)

	require.NoError(t, runPlan(context.Background(), o))
	assert.Contains(t, client.tasks[0], "AGREED DESIGN")
	assert.Contains(t, client.tasks[0], "A palette config.")
}

func TestRunDiagnosePromptOmitsRecordedHistory(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{
		stopResp("The flag parsing skips empty values.", 0.01), // diagnose
		stopResp(onePlanJSON, 0.01),                            // plan draft
	}}
	o := resumedAutoPlanRun(ops, client)
	o.tc.Type = "bug"

	require.NoError(t, runPlan(context.Background(), o))

	diagnoseTask := client.tasks[0]
	assert.Contains(t, diagnoseTask, "Add a config flag to toggle the feature.")
	assert.NotContains(t, diagnoseTask, "naming could improve", "history stripped from diagnose")
}

func TestPlannerDescription(t *testing.T) {
	o := &run{
		tc:              cmclient.TaskContext{Description: grownDescription},
		taskDescription: stripAgentSections(grownDescription),
	}

	got := o.plannerDescription()

	assert.Contains(t, got, "Add a config flag to toggle the feature.")
	assert.Contains(t, got, "## Plan\n\n1. SUBTASK: Add the flag")
	assert.NotContains(t, got, "Review Findings")
	assert.NotContains(t, got, "## Design", "design flows via the design block, not the description")
}

func TestDraftPlanUsesPlanToolsOncePerCall(t *testing.T) {
	ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
	// Attempt 1 is unparsable so draftPlan takes its repair attempt; attempt 2
	// parses. Both must share one findings list, so the factory runs once.
	llmFake := &planLLM{responses: []llm.Response{
		stopResp("not json", 0.01),
		stopResp(goodPlanJSON, 0.01),
	}}

	o := autoPlanRun(ops, llmFake, 20)

	called := 0
	reg := tools.NewRegistry(tools.NewReadTool("."), NewFindingsTool())
	o.d.PlanTools = func() *tools.Registry {
		called++

		return reg
	}

	require.NoError(t, runPlan(context.Background(), o))

	assert.Len(t, llmFake.tasks, 2, "one failed attempt, then the repair")
	assert.Equal(t, 1, called,
		"the factory runs once per draftPlan so the repair inherits the first attempt's findings")
}

func TestDraftPlanFallsBackToReadToolsWhenPlanToolsNil(t *testing.T) {
	ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
	llmFake := &planLLM{responses: []llm.Response{stopResp(goodPlanJSON, 0.01)}}

	o := autoPlanRun(ops, llmFake, 20)
	o.d.PlanTools = nil

	require.NoError(t, runPlan(context.Background(), o),
		"a nil PlanTools must degrade to ReadTools, not panic")
	require.Len(t, ops.createCardArgs, 2)

	// The would-be plan registry (what a wired PlanTools would build) carries
	// the findings tool on top of the read-only set, so it differs in size
	// from d.ReadTools - a sanity check that the equality assertion below is
	// not vacuously true.
	planReg := tools.NewRegistry(append(tools.ReadOnlyTools("."), NewFindingsTool())...)
	require.NotEqual(t, len(planReg.All()), len(o.d.ReadTools.All()),
		"sanity: the plan registry and ReadTools differ in size, or this test proves nothing")

	toolCounts := llmFake.toolCountsSeen()
	require.Len(t, toolCounts, 1)
	assert.Equal(t, len(o.d.ReadTools.All()), toolCounts[0],
		"nil PlanTools must fall back to exactly d.ReadTools, not some other registry")
}

// TestPlanReuseListExcludesCancelled proves that cancelled (not_planned)
// subtasks are filtered from the existingTitles slice fed to resumeBlock, so
// CM's duplicate-subtask guard never resurrects the old cancelled card ID when
// a new plan names a matching title.
func TestPlanReuseListExcludesCancelled(t *testing.T) {
	t.Run("draftPlan excludes not_planned from the reuse list", func(t *testing.T) {
		ops := &fakeOps{createdIDs: []string{"SUB-1"}}
		llmFake := &planLLM{responses: []llm.Response{stopResp(onePlanJSON, 0.01)}}
		d := planTestDeps(ops, llmFake)

		o := newRun(d, ops.taskContext)
		o.subtasks = []subtaskRef{
			{ID: "SUB-CANCELLED", Title: "Cancelled subtask", State: "not_planned", Tier: "simple"},
			{ID: "SUB-ACTIVE", Title: "Active subtask", State: "in_progress", Tier: "simple"},
		}

		require.NoError(t, runPlan(context.Background(), o))

		require.NotEmpty(t, llmFake.tasks)
		prompt := llmFake.tasks[0]

		assert.Contains(t, prompt, "Active subtask",
			"the active subtask must appear in the reuse list")
		assert.NotContains(t, prompt, "Cancelled subtask",
			"the cancelled subtask must NOT appear in the reuse list")
	})

	t.Run("mobPlanBriefing excludes not_planned from the reuse list", func(t *testing.T) {
		ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
		llmFake := &planLLM{}
		eng := &scriptedEngine{outcomes: []mob.Outcome{{
			Transcript: []mob.Entry{
				{Author: "seat-1", Lens: "feasibility/simplicity", Round: 0, Content: "proposal"},
			},
			Synthesis: onePlanJSON,
			Consensus: true,
			CostUSD:   0.10,
		}}}

		o := mobPlanRun(ops, llmFake, eng)
		o.subtasks = []subtaskRef{
			{ID: "SUB-CANCELLED", Title: "Cancelled subtask", State: "not_planned", Tier: "simple"},
			{ID: "SUB-ACTIVE", Title: "Active subtask", State: "todo", Tier: "simple"},
		}

		require.NoError(t, runPlan(context.Background(), o))

		require.Len(t, eng.topics, 1)
		briefing := eng.topics[0].Briefing

		assert.Contains(t, briefing, "Active subtask",
			"the active subtask must appear in the mob briefing reuse list")
		assert.NotContains(t, briefing, "Cancelled subtask",
			"the cancelled subtask must NOT appear in the mob briefing reuse list")
	})

	t.Run("done subtask included in reuse list (regression check)", func(t *testing.T) {
		ops := &fakeOps{createdIDs: []string{"SUB-1"}}
		llmFake := &planLLM{responses: []llm.Response{stopResp(onePlanJSON, 0.01)}}
		d := planTestDeps(ops, llmFake)

		o := newRun(d, ops.taskContext)
		o.subtasks = []subtaskRef{
			{ID: "SUB-DONE", Title: "Done subtask", State: "done", Tier: "simple"},
			{ID: "SUB-ACTIVE", Title: "Active subtask", State: "in_progress", Tier: "simple"},
		}

		require.NoError(t, runPlan(context.Background(), o))

		require.NotEmpty(t, llmFake.tasks)
		prompt := llmFake.tasks[0]

		assert.Contains(t, prompt, "Active subtask",
			"the active subtask must appear in the reuse list")
		assert.Contains(t, prompt, "Done subtask",
			"the done subtask must appear in the reuse list so the planner sees it")
	})
}

// TestResolveDecisionModelKeepsBaseWhenTheFloorFallsShort is the regression
// guard for the one call site where a walked-down pick would REPLACE a stronger
// operator-configured model. The floor exists to raise the decision model, and
// a below-bar pick has no claim over base.
func TestResolveDecisionModelKeepsBaseWhenTheFloorFallsShort(t *testing.T) {
	tests := []struct {
		name      string
		reviewer  float64
		wantModel string
	}{
		{name: "floor clears the complex bar and wins", reviewer: 0.88, wantModel: "strong/reviewer"},
		{name: "floor falls short and base is kept", reviewer: 0.70, wantModel: "payload/default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := registry.NewRegistryFromParts(
				llm.Catalog{{ID: "strong/reviewer", ContextLength: 200000, SupportedParameters: []string{"tools"}}},
				registry.Priors{Models: map[string]registry.PriorEntry{
					"strong/reviewer": {Reviewer: &tt.reviewer},
				}}, nil, nil, "capable/default")

			got := resolveDecisionModel(context.Background(), reg, nil, &fakeOps{},
				"CARD-1", "", "payload/default", "serve/fallback", nil)

			assert.Equal(t, tt.wantModel, got)
		})
	}
}
