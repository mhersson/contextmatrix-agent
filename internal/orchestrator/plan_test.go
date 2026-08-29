package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

func TestParsePlanFollowupsAndUnreachable(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string // empty = valid
	}{
		{
			name: "valid followups and unreachable",
			input: `{"card_tier":"moderate",
			 "subtasks":[{"title":"a","description":"d","depends_on":[],"tier":"simple"}],
			 "followup_cards":[
			   {"title":"second deliverable","description":"self-contained body","depends_on":[],"depends_on_original":true},
			   {"title":"third deliverable","description":"body","depends_on":[0],"depends_on_original":false}],
			 "unreachable_criteria":[{"criterion":"update the sibling repo","reason":"write target outside this repo"}]}`,
		},
		{
			name: "omitted arrays still valid",
			input: `{"card_tier":"simple",
			 "subtasks":[{"title":"a","description":"d","depends_on":[],"tier":"simple"}]}`,
		},
		{
			name: "followup empty title rejected",
			input: `{"card_tier":"simple",
			 "subtasks":[{"title":"a","description":"d","depends_on":[],"tier":"simple"}],
			 "followup_cards":[{"title":" ","description":"b","depends_on":[],"depends_on_original":false}]}`,
			wantErr: "followup card 0",
		},
		{
			name: "followup empty description rejected",
			input: `{"card_tier":"simple",
			 "subtasks":[{"title":"a","description":"d","depends_on":[],"tier":"simple"}],
			 "followup_cards":[{"title":"t","description":"","depends_on":[],"depends_on_original":false}]}`,
			wantErr: "followup card 0",
		},
		{
			name: "followup forward dependency rejected",
			input: `{"card_tier":"simple",
			 "subtasks":[{"title":"a","description":"d","depends_on":[],"tier":"simple"}],
			 "followup_cards":[{"title":"t","description":"b","depends_on":[1],"depends_on_original":false},
			                   {"title":"u","description":"b","depends_on":[],"depends_on_original":false}]}`,
			wantErr: "followup card 0 depends_on",
		},
		{
			name: "unreachable empty criterion rejected",
			input: `{"card_tier":"simple",
			 "subtasks":[{"title":"a","description":"d","depends_on":[],"tier":"simple"}],
			 "unreachable_criteria":[{"criterion":"","reason":"r"}]}`,
			wantErr: "unreachable criterion 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := parsePlan(tt.input)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)

			if tt.name == "valid followups and unreachable" {
				assert.Len(t, p.FollowupCards, 2)
				assert.True(t, p.FollowupCards[0].DependsOnOriginal)
				assert.Len(t, p.Unreachable, 1)
			}
		})
	}
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
	// Lock the card-level mapping: the planner tier strings parsePlan accepts
	// must convert to the matching registry.Tier at selection time. "critical"
	// must reach registry.TierCritical (the 0.90 bar), not the moderate default.
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
	assert.Equal(t, seedSizing("moderate"), o.cardSizing)

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
		{ID: "SUB-OLD-1", Title: "Existing subtask alpha", State: "in_progress", Sizing: seedSizing("moderate")},
		{ID: "SUB-OLD-2", Title: "Existing subtask beta", State: "todo", Sizing: seedSizing("moderate")},
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
	firstPick, _ := resolveDecisionModel(context.Background(), d.Registry, d.Emit, ops, "CARD-1",
		"", d.Cfg.PayloadModel, d.Cfg.DefaultModel, nil)
	first := firstPick.Model
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

	got, _ := resolveDecisionModel(context.Background(), reg, emit, ops, "CARD-1",
		"", "payload/model", "default/model", nil)

	assert.Equal(t, "rev/alpha", got.Model)
	assert.NotEqual(t, "payload/model", got)
	assert.NotEqual(t, "default/model", got)
}

func TestResolveDecisionModelHonorsPin(t *testing.T) {
	reg := reviewerRegistry()
	emit := events.NewEmitter(nil, nil)
	ops := &fakeOps{}

	got, _ := resolveDecisionModel(context.Background(), reg, emit, ops, "CARD-1",
		"pinned/model", "payload/model", "default/model", nil)

	assert.Equal(t, "pinned/model", got.Model)
}

func TestResolveDecisionModelUnresolvablePinFloorsAndWarns(t *testing.T) {
	reg := reviewerRegistry()
	emit := events.NewEmitter(nil, nil)
	ops := &fakeOps{}

	got, _ := resolveDecisionModel(context.Background(), reg, emit, ops, "CARD-1",
		"ghost/model", "payload/model", "default/model", nil)

	assert.Equal(t, "rev/alpha", got.Model)

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

	got, _ := resolveDecisionModel(context.Background(), nil, emit, ops, "CARD-1",
		"", "payload/model", "default/model", nil)

	assert.Equal(t, "payload/model", got.Model)
}

// TestResolveDecisionModelKeepsBaseOverTheCapableDefault covers the decision
// floor's unmeasured case: a registry with no priors at all, where every rung
// is dry and the selection comes off the bottom of the ladder as the capable
// default with no met tier. That answer is a floor for the run, not a
// judgement about quality, so it has no claim over the orchestrator model the
// operator configured for this phase - the floor only ever raises base.
// TestResolveDecisionModelKeepsBaseWhenTheFloorFallsShort covers the other
// case, where the pick is measured but lands on a lower rung.
func TestResolveDecisionModelKeepsBaseOverTheCapableDefault(t *testing.T) {
	reg := registry.NewRegistryFromParts(reviewerCatalog(), registry.Priors{}, nil, nil, "default/model")
	emit := events.NewEmitter(nil, nil)
	ops := &fakeOps{}

	// The gate is genuinely exercised: the selector does answer here, with the
	// capable default, and that answer clears no configured bar.
	p := reg.SelectByComplexity(registry.SelectInput{Role: registry.RoleReviewer, Tier: registry.TierComplex})
	require.True(t, p.OK)
	require.Equal(t, "default/model", p.Model)
	require.Equal(t, registry.SourceDefault, p.Source)
	require.Empty(t, p.MetTier, "off the bottom of the ladder there is no met tier")
	require.False(t, p.AtBar())

	got, _ := resolveDecisionModel(context.Background(), reg, emit, ops, "CARD-1",
		"", "payload/model", "default/model", nil)

	assert.Equal(t, "payload/model", got.Model, "the operator's own orchestrator model survives a below-bar floor")
	assert.NotEqual(t, p.Model, got.Model, "a below-bar pick must never replace base")
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
	assert.Equal(t, seedSizing("moderate"), o.cardSizing)

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
			{ID: "SUB-CANCELLED", Title: "Cancelled subtask", State: "not_planned", Sizing: seedSizing("simple")},
			{ID: "SUB-ACTIVE", Title: "Active subtask", State: "in_progress", Sizing: seedSizing("simple")},
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
			{ID: "SUB-CANCELLED", Title: "Cancelled subtask", State: "not_planned", Sizing: seedSizing("simple")},
			{ID: "SUB-ACTIVE", Title: "Active subtask", State: "todo", Sizing: seedSizing("simple")},
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
			{ID: "SUB-DONE", Title: "Done subtask", State: "done", Sizing: seedSizing("simple")},
			{ID: "SUB-ACTIVE", Title: "Active subtask", State: "in_progress", Sizing: seedSizing("simple")},
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

			got, _ := resolveDecisionModel(context.Background(), reg, nil, &fakeOps{},
				"CARD-1", "", "payload/default", "serve/fallback", nil)

			assert.Equal(t, tt.wantModel, got.Model)
		})
	}
}

// TestReadRootsReachThePlanningPrompts: the plan and diagnosis phases have no
// shell, and every tool schema describes paths as workspace-relative, so a root
// the prompt never names is a root the model has no way to discover.
func TestReadRootsReachThePlanningPrompts(t *testing.T) {
	tests := []struct {
		name  string
		roots []string
	}{
		{"declared roots are named", []string{"/declared/dep-source", "/declared/other"}},
		{"no declaration leaves the prompts alone", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ops := &fakeOps{
				taskContext: cmclient.TaskContext{
					Title: "Fix the broken parser", Description: "it throws on empty input",
				},
				createdIDs: []string{"SUB-1", "SUB-2"},
			}
			llmFake := &planLLM{responses: []llm.Response{
				stopResp("## Diagnosis\n### Root cause\nnil slice on empty input.\n", 0.02),
				stopResp(goodPlanJSON, 0.03),
			}}

			d := planTestDeps(ops, llmFake)
			d.ReadRoots = tc.roots

			o := newRun(d, ops.taskContext)
			require.NoError(t, runPlan(context.Background(), o))
			require.Len(t, llmFake.tasks, 2, "a bug-like card runs diagnose then plan")

			for i, phase := range []string{"diagnosis", "plan"} {
				for _, r := range tc.roots {
					assert.Contains(t, llmFake.tasks[i], r,
						"the %s prompt must name the declared root %s", phase, r)
				}

				if len(tc.roots) == 0 {
					assert.NotContains(t, llmFake.tasks[i], "outside the workspace",
						"the %s prompt must carry no roots line when nothing is declared", phase)
				}
			}
		})
	}
}

// The card-level bar and budget are persisted on the parent card body, which is
// the only persistence a resumed run can read. Without that marker there is no
// restore path at all, and EVERY run resumed at execute/review/judge/integrate
// silently reviews, fixes and fans out at the moderate default - even on a card
// the planner called critical.
func TestCardSizingSurvivesResume(t *testing.T) {
	ops := &fakeOps{}
	first := newRun(planTestDeps(ops, &planLLM{}), cmclient.TaskContext{Title: "Parent", Description: "parent body"})

	require.NoError(t, first.createSubtasks(context.Background(), plan{
		CardTier: "critical",
		Subtasks: []planSubtask{{Title: "Do it", Description: "Files:\n- internal/api/router.go", Tier: "complex"}},
	}))

	// A later container starts from the persisted parent body alone.
	parentBody := ops.bodyFor("CARD-1")
	require.NotEmpty(t, parentBody, "createSubtasks must push a parent body carrying the marker")

	resumed := newRun(planTestDeps(ops, &planLLM{}),
		cmclient.TaskContext{Title: "Parent", Description: parentBody})

	assert.Equal(t, registry.TierCritical, resumed.cardSizing.Bar,
		"the review panel and the Best-of-N pool must not drop to moderate on resume")
	assert.Equal(t, 2, resumed.cardSizing.Budget)
	assert.Equal(t, "critical", resumed.cardPlannerBar, "the planner's own word stays recoverable")
	assert.NotContains(t, resumed.taskDescription, "cm:meta", "the marker must never reach a model prompt")
}

// Every subtask card carries both axes plus the write-once planner estimate,
// and the persisted budget must reproduce the pre-split turn cap.
func TestCreateSubtasksPersistsBothAxes(t *testing.T) {
	ops := &fakeOps{}
	o := newRun(planTestDeps(ops, &planLLM{}), cmclient.TaskContext{Title: "Parent", Description: "parent body"})

	require.NoError(t, o.createSubtasks(context.Background(), plan{
		CardTier: "moderate",
		Subtasks: []planSubtask{{Title: "A", Description: "do a", Tier: "critical"}},
	}))

	require.Len(t, ops.createCardArgs, 1)
	body := ops.createCardArgs[0].body

	kv, s := readMeta(body)
	assert.Equal(t, registry.TierCritical, s.Bar)
	assert.Equal(t, 90, turnBudget(45, s.Budget), "a critical subtask still opens at 2x base")
	assert.Equal(t, "critical", kv["seed"])
	assert.Equal(t, "do a", stripMeta(body), "the marker never leaks into the card text")

	require.Len(t, o.subtasks, 1)
	assert.Equal(t, sizing{registry.TierCritical, 2}, o.subtasks[0].Sizing)
	assert.Equal(t, "critical", o.subtasks[0].PlannerBar)
}

// TestCreateFollowups: two followups, [0] chains on the original card, [1]
// chains on [0]. The original card is autonomous, so both split-out cards
// must inherit the flag, and the parent card body records the split.
func TestCreateFollowups(t *testing.T) {
	ops := &fakeOps{createdTopLevelIDs: []string{"CARD-2", "CARD-3"}}
	o := newRun(planTestDeps(ops, &planLLM{}), cmclient.TaskContext{Title: "Parent", Autonomous: true})

	p := plan{
		FollowupCards: []planFollowup{
			{Title: "Extract config loader", Description: "Split out the config loader.", DependsOnOriginal: true},
			{Title: "Add config docs", Description: "Document the new loader.", DependsOn: []int{0}},
		},
	}

	require.NoError(t, o.createFollowups(context.Background(), p))

	require.Len(t, ops.createTopLevelCardArgs, 2)
	assert.Equal(t, "Extract config loader", ops.createTopLevelCardArgs[0].title)
	assert.Equal(t, "Split out the config loader.", ops.createTopLevelCardArgs[0].body)
	assert.Equal(t, []string{"CARD-1"}, ops.createTopLevelCardArgs[0].dependsOn,
		"the first followup chains on the original card being planned")

	assert.Equal(t, "Add config docs", ops.createTopLevelCardArgs[1].title)
	assert.Equal(t, []string{"CARD-2"}, ops.createTopLevelCardArgs[1].dependsOn,
		"the second followup chains on the first followup's real card ID")

	require.Len(t, ops.setAutonomousCalls, 2)
	assert.Equal(t, "CARD-2", ops.setAutonomousCalls[0].cardID)
	assert.True(t, ops.setAutonomousCalls[0].autonomous)
	assert.Equal(t, "CARD-3", ops.setAutonomousCalls[1].cardID)
	assert.True(t, ops.setAutonomousCalls[1].autonomous)

	require.NotEmpty(t, ops.logs)
	lastLog := ops.logs[len(ops.logs)-1]
	assert.Contains(t, lastLog, "CARD-2")
	assert.Contains(t, lastLog, "CARD-3")

	body := ops.lastBody()
	assert.Contains(t, body, "## Split")
	assert.Contains(t, body, "CARD-2")
	assert.Contains(t, body, "CARD-3")
}

// TestCreateFollowupsNotAutonomous: a non-autonomous original card must not
// flip the flag on the split-out cards - they stay HITL like the original.
func TestCreateFollowupsNotAutonomous(t *testing.T) {
	ops := &fakeOps{createdTopLevelIDs: []string{"CARD-2"}}
	o := newRun(planTestDeps(ops, &planLLM{}), cmclient.TaskContext{Title: "Parent", Autonomous: false})

	p := plan{
		FollowupCards: []planFollowup{
			{Title: "Extract config loader", Description: "Split out the config loader.", DependsOnOriginal: true},
		},
	}

	require.NoError(t, o.createFollowups(context.Background(), p))

	require.Len(t, ops.createTopLevelCardArgs, 1)
	assert.Empty(t, ops.setAutonomousCalls, "a non-autonomous original must not autonomize its followups")
}

// TestCreateFollowupsOverflowParks: a plan proposing more followups than
// maxFollowupCards must park the run instead of mutating the board - no card
// is created, and the returned error is a park sentinel.
func TestCreateFollowupsOverflowParks(t *testing.T) {
	ops := &fakeOps{}
	o := newRun(planTestDeps(ops, &planLLM{}), cmclient.TaskContext{Title: "Parent", Autonomous: true})

	followups := []planFollowup{
		{Title: "Followup 1", Description: "d1"},
		{Title: "Followup 2", Description: "d2"},
		{Title: "Followup 3", Description: "d3"},
		{Title: "Followup 4", Description: "d4"},
		{Title: "Followup 5", Description: "d5"},
	}

	err := o.createFollowups(context.Background(), plan{FollowupCards: followups})
	require.Error(t, err)

	var soe *SplitOverflowError

	require.ErrorAs(t, err, &soe)
	assert.Equal(t, 5, soe.Count)
	assert.Len(t, soe.Titles, 5)

	assert.Empty(t, ops.createTopLevelCardArgs, "overflow must not mutate the board")
	assert.True(t, isParkError(err), "the FSM must stop the run on this sentinel like the other park errors")
}

// TestCreateFollowupsNoneIsNoop: a plan with no followups must not touch Ops
// at all - the common case (no split) stays byte-identical to before.
func TestCreateFollowupsNoneIsNoop(t *testing.T) {
	ops := &fakeOps{}
	o := newRun(planTestDeps(ops, &planLLM{}), cmclient.TaskContext{Title: "Parent", Autonomous: true})

	require.NoError(t, o.createFollowups(context.Background(), plan{}))

	assert.Empty(t, ops.createTopLevelCardArgs)
	assert.Empty(t, ops.setAutonomousCalls)
	assert.Empty(t, ops.recorded(), "a no-op split must not touch Ops at all")
}

// splitSectionBody renders a claim-time card description carrying a prior
// "## Split" record, as an earlier (interrupted) run of createFollowups would
// have written it - the exact shape parseSplitSection must read back.
func splitSectionBody(entries ...string) string {
	body := "Some task.\n\n## Split\n\n" +
		"Deliverables split out of this card at plan time - their scope, including any acceptance criteria that belong to them, is NOT part of this card:\n"
	for _, e := range entries {
		body += "- " + e + "\n"
	}

	return body
}

// TestCreateFollowupsResumeDedupesByTitle: a resumed run whose claim-time
// body already carries a "## Split" entry for followup 1 (an earlier,
// interrupted createFollowups run created it) must not re-create it - the
// recorded ID is reused for dependency wiring, and SetAutonomous is
// re-asserted for it exactly like a freshly created followup, covering a
// crash between CreateTopLevelCard and SetAutonomous on the prior attempt.
func TestCreateFollowupsResumeDedupesByTitle(t *testing.T) {
	ops := &fakeOps{createdTopLevelIDs: []string{"CARD-3"}}
	desc := splitSectionBody("CARD-2: Extract config loader")
	o := newRun(planTestDeps(ops, &planLLM{}), cmclient.TaskContext{Title: "Parent", Description: desc, Autonomous: true})

	p := plan{
		FollowupCards: []planFollowup{
			{Title: "Extract config loader", Description: "Split out the config loader.", DependsOnOriginal: true},
			{Title: "Add config docs", Description: "Document the new loader.", DependsOn: []int{0}},
		},
	}

	require.NoError(t, o.createFollowups(context.Background(), p))

	require.Len(t, ops.createTopLevelCardArgs, 1, "the already-recorded followup must not be re-created")
	assert.Equal(t, "Add config docs", ops.createTopLevelCardArgs[0].title)
	assert.Equal(t, []string{"CARD-2"}, ops.createTopLevelCardArgs[0].dependsOn,
		"depends_on must use the RECORDED id of the deduped followup, not a freshly minted one")

	require.Len(t, ops.setAutonomousCalls, 2, "autonomous is re-asserted for both, including the deduped one")
	assert.Equal(t, "CARD-2", ops.setAutonomousCalls[0].cardID)
	assert.True(t, ops.setAutonomousCalls[0].autonomous)
	assert.Equal(t, "CARD-3", ops.setAutonomousCalls[1].cardID)
	assert.True(t, ops.setAutonomousCalls[1].autonomous)
}

// TestCreateFollowupsPartialFailureRecordsCreatedSoFar: when the 2nd
// followup's creation fails, the error is still returned, but the 1st
// followup - already created before the failure - must be durably on record
// in the recorded body's "## Split" section, not lost with the failed run.
func TestCreateFollowupsPartialFailureRecordsCreatedSoFar(t *testing.T) {
	ops := &fakeOps{
		createdTopLevelIDs:         []string{"CARD-2"},
		createTopLevelCardErr:      errors.New("board unreachable"),
		createTopLevelCardErrAfter: 1, // the 1st call succeeds, the 2nd fails
	}
	o := newRun(planTestDeps(ops, &planLLM{}), cmclient.TaskContext{Title: "Parent", Autonomous: true})

	p := plan{
		FollowupCards: []planFollowup{
			{Title: "Extract config loader", Description: "d1", DependsOnOriginal: true},
			{Title: "Add config docs", Description: "d2", DependsOn: []int{0}},
		},
	}

	err := o.createFollowups(context.Background(), p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Add config docs")

	body := ops.lastBody()
	assert.Contains(t, body, "## Split")
	assert.Contains(t, body, "CARD-2: Extract config loader", "the followup created before the failure stays on record")
	assert.NotContains(t, body, "Add config docs", "the failed followup was never created and must not appear")
}

// TestRecordUnreachable: a plan carrying 2 unreachable entries logs one
// UNREACHABLE-AC line per entry and records an "## Unreachable Criteria"
// section on the parent body containing both criterion strings.
func TestRecordUnreachable(t *testing.T) {
	ops := &fakeOps{}
	o := newRun(planTestDeps(ops, &planLLM{}), cmclient.TaskContext{Title: "Parent", Autonomous: true})

	p := plan{
		Unreachable: []planUnreachable{
			{Criterion: "staging deploy succeeds", Reason: "no staging access from this container"},
			{Criterion: "prod metrics show zero errors", Reason: "prod is not reachable from this container"},
		},
	}

	o.recordUnreachable(context.Background(), p)

	logLines := 0

	for _, l := range ops.logs {
		if strings.Contains(l, "UNREACHABLE-AC") {
			logLines++
		}
	}

	assert.Equal(t, 2, logLines, "one UNREACHABLE-AC log line per unreachable entry")

	body := ops.lastBody()
	assert.Contains(t, body, "## Unreachable Criteria")
	assert.Contains(t, body, "staging deploy succeeds")
	assert.Contains(t, body, "prod metrics show zero errors")
}

// TestRecordUnreachableCapsLogLines: a plan with more unreachable entries than
// maxUnreachableLogLines caps the UNREACHABLE-AC add_log lines at that
// constant (first maxUnreachableLogLines-1 individually, plus one summary
// line), so a long unreachable list cannot itself burn a large share of CM's
// 50-entry-capped activity log. The "## Unreachable Criteria" section is not
// capped - it lists every entry regardless.
func TestRecordUnreachableCapsLogLines(t *testing.T) {
	ops := &fakeOps{}
	o := newRun(planTestDeps(ops, &planLLM{}), cmclient.TaskContext{Title: "Parent", Autonomous: true})

	const n = 12

	p := plan{Unreachable: make([]planUnreachable, n)}
	for i := range p.Unreachable {
		p.Unreachable[i] = planUnreachable{
			Criterion: fmt.Sprintf("criterion %d", i),
			Reason:    fmt.Sprintf("reason %d", i),
		}
	}

	o.recordUnreachable(context.Background(), p)

	logLines := 0

	for _, l := range ops.logs {
		if strings.Contains(l, "UNREACHABLE-AC") {
			logLines++
		}
	}

	assert.Equal(t, maxUnreachableLogLines, logLines,
		"log lines must be capped at maxUnreachableLogLines regardless of how many entries overflow it")

	body := ops.lastBody()
	assert.Contains(t, body, "## Unreachable Criteria")

	for i := range p.Unreachable {
		assert.Contains(t, body, fmt.Sprintf("criterion %d", i),
			"the section lists every entry even when the log is capped")
	}
}

// TestRecordUnreachableNoneIsNoop: a plan with no unreachable entries must not
// touch Ops at all - the common case (every AC reachable) stays byte-identical
// to before.
func TestRecordUnreachableNoneIsNoop(t *testing.T) {
	ops := &fakeOps{}
	o := newRun(planTestDeps(ops, &planLLM{}), cmclient.TaskContext{Title: "Parent", Autonomous: true})

	o.recordUnreachable(context.Background(), plan{})

	assert.Empty(t, ops.recorded(), "a no-op unreachable list must not touch Ops at all")
}

// TestCreateSubtasksRefreshesTaskDescription: createSubtasks re-derives
// o.taskDescription from the post-mutation body, so the "## Split" and
// "## Unreachable Criteria" sections it (and createFollowups) just wrote
// reach every downstream prompt that reads the cached field - while "## Plan",
// which createSubtasks itself records last, stays stripped like every other
// agent-recorded section.
func TestCreateSubtasksRefreshesTaskDescription(t *testing.T) {
	ops := &fakeOps{createdIDs: []string{"SUB-1"}, createdTopLevelIDs: []string{"CARD-2"}}
	o := newRun(planTestDeps(ops, &planLLM{}), cmclient.TaskContext{Title: "Parent", Autonomous: true})

	p := plan{
		CardTier: "moderate",
		Subtasks: []planSubtask{{Title: "Do it", Description: "do it", Tier: "moderate"}},
		FollowupCards: []planFollowup{
			{Title: "Extract config loader", Description: "Split out the config loader.", DependsOnOriginal: true},
		},
		Unreachable: []planUnreachable{
			{Criterion: "staging deploy succeeds", Reason: "no staging access from this container"},
		},
	}

	require.NoError(t, o.createSubtasks(context.Background(), p))

	assert.Contains(t, o.taskDescription, "## Split",
		"the refreshed snapshot must carry the split record")
	assert.Contains(t, o.taskDescription, "## Unreachable Criteria",
		"the refreshed snapshot must carry the unreachable-criteria record")
	assert.Contains(t, o.taskDescription, "staging deploy succeeds",
		"the refreshed snapshot must carry the criterion text, not just the heading")
	assert.NotContains(t, o.taskDescription, "## Plan",
		"the plan record is agent-recorded history and must stay stripped like before")
}

// The plan JSON wire contract is deliberately untouched, so parsePlan keeps
// rejecting an unrecognised bar. The bar is the SILENT axis: one repair turn on
// the loud channel is the price of protecting the axis with no other defence.
func TestParsePlanStillRejectsAnUnknownTier(t *testing.T) {
	_, err := parsePlan(`{"card_tier":"galactic","subtasks":[{"title":"a","tier":"simple"}]}`)
	require.Error(t, err)

	_, err = parsePlan(`{"card_tier":"simple","subtasks":[{"title":"a","tier":"galactic"}]}`)
	require.Error(t, err)

	p, err := parsePlan(`{"card_tier":"complex","subtasks":[{"title":"a","tier":"simple"}]}`)
	require.NoError(t, err)
	assert.Equal(t, "complex", p.CardTier, "the wire names must not have changed")
}

// mobHITLPlanRun builds a HITL run whose plan comes from a mob discussion. The
// card carries a ## Design, so brainstorm is skipped and the plan-approval gate
// is the only place left that could still call the plan decision model.
func mobHITLPlanRun(ops *fakeOps, inbox *fakeInbox, client llm.LLM, transcript *bytes.Buffer) *run {
	d := Deps{
		Ops:       ops,
		Git:       &fakeGit{},
		Client:    client,
		Emit:      events.NewEmitter(nil, transcript),
		Registry:  reviewerRegistry(),
		ReadTools: tools.NewRegistry(tools.NewReadTool(".")),
		Human:     inbox,
		Cfg: Config{
			Project: "proj", CardID: "CARD-1",
			PayloadModel: "payload/model", DefaultModel: "default/model",
			MaxTurns: 20, MaxCardCost: 2.0, Interactive: true,
			Mob: MobConfig{Participants: 3, Plan: true, Rounds: 2, BudgetFactor: 0.75},
		},
	}

	tc := cmclient.TaskContext{
		Title:       "Add a palette",
		Description: "## Design\n\nA palette config.", // present -> brainstorm skipped
	}

	return newRun(d, tc)
}

// bugPlanRun builds an autonomous run for a bug-like card on the reviewer-prior
// registry, so the plan decision is a real ladder pick with a next-best
// alternative to fall to when the first model proves harness-incapable.
func bugPlanRun(ops *fakeOps, client llm.LLM, transcript *bytes.Buffer) *run {
	d := Deps{
		Ops:       ops,
		Client:    client,
		Emit:      events.NewEmitter(nil, transcript),
		Registry:  reviewerRegistry(),
		ReadTools: tools.NewRegistry(tools.NewReadTool(".")),
		Cfg: Config{
			Project: "proj", CardID: "CARD-1",
			PayloadModel: "payload/model", DefaultModel: "default/model",
			MaxTurns: 20,
		},
	}

	tc := cmclient.TaskContext{
		Title:       "Fix the crash on startup",
		Type:        "bug", // bug-like -> the diagnose pass runs ahead of the draft
		Description: "The worker crashes on start.",
	}

	return newRun(d, tc)
}

// The two card-log fragments the plan phase uses to distinguish an entry
// announcement from a forced re-resolution. They are held here, not inlined,
// because they are the ONE place the test suite couples to that prose:
// rewording either message in runPlan means changing these two consts and
// nothing else.
const (
	decisionModelLog  = "orchestrator model: "
	reselectedLogPart = "(re-selected after diagnose)"
)

// entryAnnouncements returns the card-log lines announcing the decision model at
// phase entry, excluding the re-selection distinction line. The card log is the
// only channel that separates the entry announcement from a forced
// re-resolution, so it is what pins the announce-once guard.
//
// A reworded card log makes this helper return nothing, and the assertion it
// feeds then fails as if the announce-once guard had regressed. If that
// assertion fails, check the two consts above before suspecting the guard.
func entryAnnouncements(ops *fakeOps) []string {
	var out []string

	for _, c := range ops.recorded() {
		msg, ok := strings.CutPrefix(c, "AddLog:"+decisionModelLog)
		if !ok || strings.Contains(msg, reselectedLogPart) {
			continue
		}

		out = append(out, msg)
	}

	return out
}

// TestPlanDecisionSilentWhenTheMobDraftsThePlan pins the wire contract on the
// autonomous mob path: a model_selected line records a model that RAN, and a
// discussion that drafts the plan never calls the plan decision model, so the
// phase must leave no "plan decision" line behind. A line here would count a
// model into the plan-decision distribution that never saw a prompt.
//
// The seat selections are the live-transcript control: they prove the phase
// emitted into this buffer at all, so the empty assertion cannot pass on a run
// that never reached the code path.
func TestPlanDecisionSilentWhenTheMobDraftsThePlan(t *testing.T) {
	tests := []struct {
		name          string
		synthesis     string
		responses     []llm.Response
		wantModerator int
	}{
		{
			name:      "synthesis parses",
			synthesis: goodPlanJSON,
		},
		{
			// The repair is the only path that puts a moderator on the
			// transcript with a stubbed engine, and the draft still succeeds.
			name:          "moderator repairs the synthesis",
			synthesis:     "prose only, no JSON here",
			responses:     []llm.Response{stopResp(goodPlanJSON, 0.02)},
			wantModerator: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var transcript bytes.Buffer

			ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
			eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: tt.synthesis, Consensus: true}}}
			o := mobPlanRun(ops, &planLLM{responses: tt.responses}, eng)
			o.d.Emit = events.NewEmitter(nil, &transcript)

			require.NoError(t, runPlan(context.Background(), o))
			require.Len(t, ops.createCardArgs, 2, "the discussion produced the plan")

			sels := modelSelections(t, &transcript)
			assert.Empty(t, selectionsForPhase(sels, "plan decision"),
				"the discussion drafted the plan, so no decision model ran")
			assert.Len(t, selectionsForPhase(sels, "mob plan"), 3,
				"the seat selections prove the phase wrote to this transcript")
			assert.Len(t, selectionsForPhase(sels, "mob moderator"), tt.wantModerator,
				"the moderator records a selection exactly where it runs")
		})
	}
}

// TestPlanGateDecisionRecordsOnlyTheClassificationThatRan is the plan-gate half
// of the same contract, and the pair is the point. With the plan already drafted
// by the discussion, the gate is the only remaining caller of the decision
// model: a human reply reaches the classification model and records one
// selection, while a card promoted mid-run closes the inbox, so the gate returns
// without classifying and must record none.
func TestPlanGateDecisionRecordsOnlyTheClassificationThatRan(t *testing.T) {
	tests := []struct {
		name         string
		msgs         []harness.UserMessage
		responses    []llm.Response
		want         int
		wantPromoted bool
	}{
		{
			// Empty, non-blocking inbox: the gate Wait reports ErrInboxClosed,
			// exactly what a promote frame produces. No classification response
			// is scripted because no model call happens.
			name:         "promoted at the first gate",
			want:         0,
			wantPromoted: true,
		},
		{
			name:      "human reply is classified",
			msgs:      []harness.UserMessage{{Content: "approve"}},
			responses: []llm.Response{stopResp(`{"verdict":"approve","feedback":""}`, 0.001)},
			want:      1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var transcript bytes.Buffer

			ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
			eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: goodPlanJSON, Consensus: true}}}
			o := mobHITLPlanRun(ops, &fakeInbox{msgs: tt.msgs}, &planLLM{responses: tt.responses}, &transcript)
			o.mobEngine = eng.run

			require.NoError(t, runPlan(context.Background(), o))
			require.Len(t, ops.createCardArgs, 2, "the plan reached createSubtasks either way")

			// Subtask creation alone does not separate the two cases: an
			// approval creates them too. Only the promote log pins that the
			// zero came from an inbox that closed rather than from some other
			// outcome that happens to skip the classification.
			if tt.wantPromoted {
				assert.True(t, ops.loggedContains("promoted"),
					"the gate took the promote path; logs=%v", ops.logs)
			}

			sels := modelSelections(t, &transcript)
			assert.Len(t, selectionsForPhase(sels, "plan decision"), tt.want,
				"the gate records a selection only when it classifies a reply")
			assert.Len(t, selectionsForPhase(sels, "mob plan"), 3,
				"the seat selections prove the phase wrote to this transcript")
		})
	}
}

// TestPlanDecisionRecordedOnceAcrossTheHITLAdjustLoop pins the resolve-once half
// of the contract. The adjust loop puts four decision-model calls on the wire
// (draft, classify, re-draft, classify), and a consumer reads one selection line
// per phase as one model: resolving per call would report four models for a
// phase that ran on one.
func TestPlanDecisionRecordedOnceAcrossTheHITLAdjustLoop(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{
		{Content: "make it two subtasks"},
		{Content: "approve"},
	}}
	client := &planLLM{responses: []llm.Response{
		stopResp(onePlanJSON, 0.01),                                       // draft 1
		stopResp(`{"verdict":"adjust","feedback":"two subtasks"}`, 0.001), // gate -> adjust
		stopResp(onePlanJSON, 0.01),                                       // draft 2 (re-draft)
		stopResp(`{"verdict":"approve","feedback":""}`, 0.001),            // gate -> approve
	}}
	o := hitlPlanRun(ops, inbox, client)
	// The reviewer-prior registry resolves the decision to a ladder model that
	// is NOT the configured payload model, so "the transcript names the model
	// the calls ran on" is a real comparison rather than two names that coincide.
	o.d.Registry = reviewerRegistry()
	o.d.Emit = events.NewEmitter(nil, &transcript)

	require.NoError(t, runPlan(context.Background(), o))
	require.Len(t, client.models, 4, "the adjust loop made four decision-model calls")

	sels := selectionsForPhase(modelSelections(t, &transcript), "plan decision")
	require.Len(t, sels, 1, "one resolution is reused across the whole phase")

	for i, m := range client.models {
		assert.Equal(t, sels[0].Model, m, "call %d ran on the model the transcript names", i)
	}
}

// TestPlanDecisionRecordedOnceForTheAutonomousSoloDraft is the control for the
// lazy resolution: moving the resolve to the point of use must not lose the line
// where the model DOES run. The autonomous solo draft is that point, and the
// recorded model has to be the one the planner call was configured with.
func TestPlanDecisionRecordedOnceForTheAutonomousSoloDraft(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{stopResp(onePlanJSON, 0.01)}}
	o := autoPlanRun(ops, client, 20)
	// A ladder model distinct from the configured payload model, so the
	// recorded slug cannot match the planner's by coincidence.
	o.d.Registry = reviewerRegistry()
	o.d.Emit = events.NewEmitter(nil, &transcript)

	require.NoError(t, runPlan(context.Background(), o))
	require.Len(t, ops.createCardArgs, 1, "the solo draft produced the plan")
	require.Len(t, client.models, 1, "exactly the planner call")

	sels := selectionsForPhase(modelSelections(t, &transcript), "plan decision")
	require.Len(t, sels, 1, "the solo draft records the selection it ran on")
	assert.Equal(t, client.models[0], sels[0].Model, "the transcript names the model the planner ran on")
	assert.Equal(t, "complex", sels[0].TierRequested, "decision phases are floored to complex")
}

// TestPlanDecisionReresolvesOffAnIncapableDiagnoseModel pins the diagnose
// recovery. The diagnose model cannot drive the tool loop, so it is blacklisted
// and excluded, and the phase must re-select off that slug: the transcript
// carries a SECOND selection naming the replacement, because a second model
// genuinely ran. The card log is the only channel separating the entry
// announcement from that forced re-resolution, so it is asserted here and only
// here - the re-selection must speak through its own distinction line and must
// not re-announce as a fresh phase entry.
//
// TestPlanPhaseDiagnoseIncapableModelIsRecovered covers the recovery mechanism
// itself (exclusion, blacklist, the plan not running on the incapable model).
// What this test adds, and what a later deduplication of the two must keep, is
// the pair of "plan decision" transcript selections and the announce-once
// check: the observable half of the contract, which the sibling does not read.
func TestPlanDecisionReresolvesOffAnIncapableDiagnoseModel(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{}
	// rev/alpha is the top reviewer prior, so it is the first decision pick;
	// every turn it emits unparseable tool-call arguments, which the harness
	// reports as incapable. rev/beta is the next-best pick and drafts the plan.
	client := &modelAwareLLM{
		incapable: map[string]bool{"rev/alpha": true},
		responses: []llm.Response{stopResp(onePlanJSON, 0.01)},
	}
	o := bugPlanRun(ops, client, &transcript)

	require.NoError(t, runPlan(context.Background(), o), "a failed diagnosis must not fail planning")
	require.Len(t, ops.createCardArgs, 1, "planning continued without a diagnosis")
	assert.True(t, o.excluded["rev/alpha"], "the incapable model is excluded for the rest of the run")

	sels := selectionsForPhase(modelSelections(t, &transcript), "plan decision")
	require.Len(t, sels, 2, "the diagnose pass and the re-selected draft each ran a model")
	assert.Equal(t, "rev/alpha", sels[0].Model, "the diagnose pass ran on the first pick")
	assert.Equal(t, "rev/beta", sels[1].Model, "the re-resolution picked off the excluded slug")
	assert.NotEqual(t, sels[0].Model, sels[1].Model,
		"the re-resolution must land somewhere else, whatever the priors rank first")

	models := client.recordedModels()
	require.NotEmpty(t, models)
	assert.Equal(t, "rev/beta", models[len(models)-1], "the planner ran on the replacement")

	assert.True(t, ops.loggedContains("rev/beta "+reselectedLogPart),
		"the re-selection is logged as a re-selection; logs=%v", ops.logs)
	assert.Equal(t, []string{"rev/alpha"}, entryAnnouncements(ops),
		"the phase announces its model once, at entry; the re-resolution does not re-announce")
}
