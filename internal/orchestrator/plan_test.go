package orchestrator

import (
	"context"
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
		// Subtask 0 depends on subtask 1 (a later index) — forbidden.
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

func TestPlanPhaseCreatesSubtasks(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body"},
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
	// the planner's description — the execute phase feeds it to the coder.
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
		taskContext: cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body"},
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
		taskContext: cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body"},
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

	// Exactly two model calls — no third attempt.
	assert.Len(t, llmFake.tasks, 2)

	// No cards created on hard failure.
	assert.Empty(t, ops.createCardArgs)
}

func TestPlanPhaseResume(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body"},
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

	// runPlan must NOT call SubtaskStates itself — the reconciled list is the
	// source of truth for the reuse block.
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "SubtaskStates:proj/CARD-1"),
		"runPlan must consume the reconciled refs, not re-call SubtaskStates")
}

func TestPlanPhaseDiagnosesBugLikeCard(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{
			CardID: "CARD-1", Title: "Fix the broken parser", Description: "it throws on empty input",
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
			CardID: "CARD-1", Title: "Add a health endpoint", Description: "expose /healthz", Type: "task",
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

		// A warning note must be logged to the card — specifically an AddLog
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
		"", "payload/model", "default/model")

	assert.Equal(t, "rev/alpha", got)
	assert.NotEqual(t, "payload/model", got)
	assert.NotEqual(t, "default/model", got)
}

func TestResolveDecisionModelHonorsPin(t *testing.T) {
	reg := reviewerRegistry()
	emit := events.NewEmitter(nil, nil)
	ops := &fakeOps{}

	got := resolveDecisionModel(context.Background(), reg, emit, ops, "CARD-1",
		"pinned/model", "payload/model", "default/model")

	assert.Equal(t, "pinned/model", got)
}

func TestResolveDecisionModelUnresolvablePinFloorsAndWarns(t *testing.T) {
	reg := reviewerRegistry()
	emit := events.NewEmitter(nil, nil)
	ops := &fakeOps{}

	got := resolveDecisionModel(context.Background(), reg, emit, ops, "CARD-1",
		"ghost/model", "payload/model", "default/model")

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
		"", "payload/model", "default/model")

	assert.Equal(t, "payload/model", got)
}

func TestResolveDecisionModelEmptyPoolReturnsCapableDefault(t *testing.T) {
	reg := registry.NewRegistryFromParts(reviewerCatalog(), registry.Priors{}, nil, nil, "default/model")
	emit := events.NewEmitter(nil, nil)
	ops := &fakeOps{}

	got := resolveDecisionModel(context.Background(), reg, emit, ops, "CARD-1",
		"", "payload/model", "default/model")

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
		CardID: "CARD-1", Title: "Add a palette",
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
		CardID: "CARD-1", Title: "Add a config flag",
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
		CardID: "CARD-1", Title: "Add a palette",
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

// coopPlanRun builds an autonomous run with co-op plan enabled and a
// scripted engine.
func coopPlanRun(ops *fakeOps, client llm.LLM, eng *scriptedEngine) *run {
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
			Coop: CoopConfig{Participants: 3, Plan: true, Rounds: 2, BudgetFactor: 0.75},
		},
	}

	tc := cmclient.TaskContext{
		CardID: "CARD-1", Title: "Add a config flag",
		Description: "Add a config flag to toggle the feature.",
	}

	o := newRun(d, tc)
	o.coopEngine = eng.run

	return o
}

func TestRunPlanCoopCreatesSubtasksFromSynthesis(t *testing.T) {
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

	o := coopPlanRun(ops, llmFake, eng)
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

func TestRunPlanCoopRepairSucceeds(t *testing.T) {
	ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
	// The moderator repair call is the only LLM call: it returns valid JSON.
	llmFake := &planLLM{responses: []llm.Response{stopResp(goodPlanJSON, 0.02)}}
	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: "sorry, prose only", Consensus: true}}}

	o := coopPlanRun(ops, llmFake, eng)
	require.NoError(t, runPlan(context.Background(), o))

	require.Len(t, ops.createCardArgs, 2, "subtasks created from the repaired synthesis")
	require.Len(t, llmFake.tasks, 1, "exactly one moderator repair call")
	assert.Contains(t, llmFake.tasks[0], "COULD NOT BE PARSED", "the repair prompt names the parse failure")
}

func TestRunPlanCoopParseFailureFallsBackToDraftPlan(t *testing.T) {
	ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
	// Call 1: moderator repair — still junk. Call 2: solo draftPlan — good.
	llmFake := &planLLM{responses: []llm.Response{
		stopResp("still not json", 0.01),
		stopResp(goodPlanJSON, 0.01),
	}}
	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: "prose"}}}

	o := coopPlanRun(ops, llmFake, eng)
	require.NoError(t, runPlan(context.Background(), o))

	require.Len(t, ops.createCardArgs, 2, "the solo fallback produced the plan")
	assert.Len(t, llmFake.tasks, 2, "one repair attempt, then one solo planner call")

	joined := strings.Join(ops.bodyUpdates, "\n===\n")
	assert.NotContains(t, joined, "## Discussion", "no discussion record when the discussion was abandoned")
}

func TestRunPlanCoopEngineFailureFallsBackToDraftPlan(t *testing.T) {
	ops := &fakeOps{createdIDs: []string{"SUB-1", "SUB-2"}}
	llmFake := &planLLM{responses: []llm.Response{stopResp(goodPlanJSON, 0.01)}}
	eng := &scriptedEngine{outcomes: []mob.Outcome{{}}, errs: []error{mob.ErrNoQuorum}}

	o := coopPlanRun(ops, llmFake, eng)
	require.NoError(t, runPlan(context.Background(), o))

	require.Len(t, ops.createCardArgs, 2, "quorum failure degrades to the solo path")
	assert.Len(t, llmFake.tasks, 1, "exactly the solo planner call")
}

func TestRunPlanCoopHITLAdjustReopensDiscussion(t *testing.T) {
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

	o := coopPlanRun(ops, llmFake, eng)
	o.d.Cfg.Interactive = true
	o.d.Human = inbox
	// Pre-existing design section on the accumulated card body (what
	// hasDesignSection actually checks) so the creative brainstorm is
	// skipped and both scripted inbox messages reach the plan gate.
	o.body = "## Design\n\nA config flag."

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
