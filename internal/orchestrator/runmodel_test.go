package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHarnessConfigPopulatesTriad pins that the centralized config builder
// stamps every per-phase harness.Config with the run-wide safety triad: the
// tool-output size cap, the secret-redaction func, and the model's own context
// window (resolved from the registry). A future phase that forgets the
// hardening is impossible because every phase routes through this builder.
func TestHarnessConfigPopulatesTriad(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{}}
	d := planTestDeps(ops, &planLLM{})
	d.Cfg.ToolOutputMax = 65536

	// A non-identity redactor with a recognisable mapping, so the test pins the
	// redactor's BEHAVIOR - not just that the field is non-nil. A mis-wired but
	// non-nil func (e.g. the wrong field) would fail the behavioral assert below.
	const (
		sentinel = "tok=SECRET"
		scrubbed = "tok=[redacted]"
	)

	d.Redact = func(s string) string {
		if s == sentinel {
			return scrubbed
		}

		return s
	}

	o := newRun(d, ops.taskContext)

	// "default/model" is in planTestCatalog with ContextLength 131072.
	cfg := o.harnessConfig("default/model")

	assert.Equal(t, "default/model", cfg.Model)
	assert.Equal(t, 20, cfg.MaxTurns, "MaxTurns carried from Cfg")
	assert.Equal(t, 65536, cfg.ToolOutputMaxBytes, "tool-output cap carried from Cfg")
	require.NotNil(t, cfg.RedactToolOutput, "redactor wired from Deps.Redact")
	assert.Equal(t, scrubbed, cfg.RedactToolOutput(sentinel),
		"wired redactor must be the one from Deps.Redact (behavioral check)")
	assert.Equal(t, 131072, cfg.ContextWindow, "context window resolved from the registry")
}

// TestHarnessConfigUnknownModelZeroWindow pins that a model absent from the
// catalog yields ContextWindow 0 (the harness treats 0 as "no context-limit
// check"), so an unknown slug never trips a spurious limit.
func TestHarnessConfigUnknownModelZeroWindow(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{}}
	d := planTestDeps(ops, &planLLM{})

	o := newRun(d, ops.taskContext)

	cfg := o.harnessConfig("ghost/model")
	assert.Equal(t, 0, cfg.ContextWindow)
}

// TestHarnessConfigCompactionGate pins that the per-phase harness.Config opts
// into in-window compaction only when the run config enables it: enabled ->
// Compaction non-nil with the configured threshold/keep-recent; disabled -> nil
// (the hard context_limit stop, the agent's default behavior).
func TestHarnessConfigCompactionGate(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{}}

	t.Run("enabled yields non-nil Compaction", func(t *testing.T) {
		d := planTestDeps(ops, &planLLM{})
		d.Cfg.Compaction = Compaction{Enabled: true, Threshold: 0.8, KeepRecentTurns: 4}

		o := newRun(d, ops.taskContext)
		cfg := o.harnessConfig("default/model")

		require.NotNil(t, cfg.Compaction)
		assert.InDelta(t, 0.8, cfg.Compaction.Threshold, 1e-9)
		assert.Equal(t, 4, cfg.Compaction.KeepRecentTurns)
	})

	t.Run("disabled yields nil Compaction", func(t *testing.T) {
		d := planTestDeps(ops, &planLLM{})
		d.Cfg.Compaction = Compaction{Enabled: false, Threshold: 0.8, KeepRecentTurns: 4}

		o := newRun(d, ops.taskContext)
		cfg := o.harnessConfig("default/model")

		assert.Nil(t, cfg.Compaction)
	})
}

// TestRunModelNormalizesContextLimit pins the 0.85 threshold tightly: exactly
// int(0.85*window) prompt tokens trips context_limit (surfaced by runModel as an
// error so a phase never proceeds on truncated output), and one token below does
// NOT. A boundary-precise pair catches a drifted threshold constant - a loose
// "well over the limit" prompt would still pass against e.g. 0.95.
func TestRunModelNormalizesContextLimit(t *testing.T) {
	// "default/model" is in planTestCatalog with ContextLength 131072. The harness
	// trips when prompt_tokens >= int(contextLimitThreshold * window); mirror that
	// exact arithmetic here so the test pins the documented 0.85 constant.
	// threshold is a var (not a const) so the conversion truncates at runtime,
	// exactly as the harness does - a const expression would be a compile error.
	const window = 131072

	threshold := 0.85

	tripAt := int(threshold * float64(window)) // 111411

	tests := []struct {
		name         string
		promptTokens int
		wantTrip     bool
	}{
		{"exactly at threshold trips", tripAt, true},
		{"one token below does not trip", tripAt - 1, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{taskContext: cmclient.TaskContext{}}
			resp := llm.Response{
				Content:      "partial",
				FinishReason: "stop",
				Usage:        llm.Usage{PromptTokens: tt.promptTokens, Cost: 0.01},
			}
			llmFake := &planLLM{responses: []llm.Response{resp}}
			d := planTestDeps(ops, llmFake)

			o := newRun(d, ops.taskContext)

			res, _, err := o.runModel(context.Background(), d.ReadTools, "do the thing", "default/model")

			if tt.wantTrip {
				require.Error(t, err, "context_limit must surface as an error")

				var cle *ContextLimitError
				require.ErrorAs(t, err, &cle)
				assert.Equal(t, "default/model", cle.Model)
				assert.Equal(t, 131072, cle.ContextWindow,
					"window resolved from the registry catalog")

				// The result is returned alongside the error so the caller's
				// Spend/ReportUsage pattern still works.
				assert.Equal(t, "context_limit", res.Reason)
				assert.InDelta(t, 0.01, res.TotalCostUSD, 1e-9)
			} else {
				require.NoError(t, err, "one token below the threshold must NOT trip")
				assert.NotEqual(t, "context_limit", res.Reason)
			}
		})
	}
}

// TestRunModelNormalizesIncapable pins that when the harness returns
// Reason == "incapable" (model emits tool calls every turn but none parse),
// runModel surfaces it as an *IncapableError carrying the model name and, when
// the harness classified the failure pattern, the harness's own IncapableDetail
// sentence as the Reason (rather than the generic fallback). It mirrors the
// context-limit test: inject a fake LLM whose every response contains the same
// unparseable tool call (bad JSON → harness never marks turnHadCapableTool) so
// after IncapableThreshold turns the harness sets Reason = ReasonIncapable and,
// because every failed payload is identical, classifies it as a suspected
// upstream gateway defect.
func TestRunModelNormalizesIncapable(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{}}

	// Each response carries a single tool call with invalid JSON arguments.
	// The harness trips incapability after IncapableThreshold (default 3)
	// consecutive unproductive turns; supply enough responses to reach the
	// threshold without hitting MaxTurns.
	badCall := llm.ToolCall{
		ID:   "bad-1",
		Type: "function",
		Function: llm.FunctionCall{
			Name:      "read",
			Arguments: `{ this is not json`,
		},
	}
	badResp := llm.Response{ToolCalls: []llm.ToolCall{badCall}}
	llmFake := &planLLM{responses: []llm.Response{badResp, badResp, badResp, badResp, badResp}}
	d := planTestDeps(ops, llmFake)

	o := newRun(d, ops.taskContext)

	res, _, err := o.runModel(context.Background(), d.ReadTools, "do the thing", "default/model")

	require.Error(t, err, "incapable must surface as an error")

	var ie *IncapableError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "default/model", ie.Model)

	// The result is returned alongside the error so the caller's
	// Spend/ReportUsage pattern still works.
	assert.Equal(t, "incapable", res.Reason)

	// Identical payloads on every turn are classified: the harness's own
	// detail sentence is carried through as the error's Reason, not pinned
	// verbatim - only that it is non-empty and passed through unchanged.
	require.NotEmpty(t, res.IncapableDetail, "the identical-payload pattern must classify")
	assert.Equal(t, res.IncapableDetail, ie.Reason, "the harness detail is carried through as the error reason")
	assert.Contains(t, ie.Reason, "suspected upstream gateway defect")
}

// TestRunModelNormalizesIncapableEmptyDetail pins the fallback: when the
// harness cannot classify the failure pattern (distinct malformed payloads
// each turn, so classifyIncapable finds no evidence of a transport defect),
// Result.IncapableDetail is empty and runModel falls back to the original
// generic reason rather than surfacing an empty string.
func TestRunModelNormalizesIncapableEmptyDetail(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{}}

	resp := func(args string) llm.Response {
		return llm.Response{ToolCalls: []llm.ToolCall{{
			ID:       "bad-1",
			Type:     "function",
			Function: llm.FunctionCall{Name: "read", Arguments: args},
		}}}
	}
	llmFake := &planLLM{responses: []llm.Response{
		resp(`{ bad 1`), resp(`{ bad 2`), resp(`{ bad 3`), resp(`{ bad 4`), resp(`{ bad 5`),
	}}
	d := planTestDeps(ops, llmFake)

	o := newRun(d, ops.taskContext)

	res, _, err := o.runModel(context.Background(), d.ReadTools, "do the thing", "default/model")

	require.Error(t, err, "incapable must surface as an error")

	var ie *IncapableError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "incapable", res.Reason)
	assert.Empty(t, res.IncapableDetail, "distinct payloads give the classifier no evidence")
	assert.Equal(t, "cannot drive the tool loop", ie.Reason)
}

// TestRunModelNormalizesMaxTurns pins that a run stopping at the turn cap
// (Reason "max_turns", Completed=false, err==nil from the harness) surfaces as
// a *MaxTurnsError, so no phase can treat truncated work as success.
func TestRunModelNormalizesMaxTurns(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{}}

	// One valid-JSON tool call keeps the loop alive without tripping the
	// incapable detector (threshold 3); MaxTurns=1 stops after turn one.
	call := llm.ToolCall{
		ID:       "c1",
		Type:     "function",
		Function: llm.FunctionCall{Name: "read", Arguments: `{"path":"no-such-file.txt"}`},
	}
	llmFake := &planLLM{responses: []llm.Response{{ToolCalls: []llm.ToolCall{call}, Usage: llm.Usage{Cost: 0.01}}}}
	d := planTestDeps(ops, llmFake)
	d.Cfg.MaxTurns = 1

	o := newRun(d, ops.taskContext)

	res, _, err := o.runModel(context.Background(), d.ReadTools, "do the thing", "default/model")

	require.Error(t, err, "max_turns must surface as an error")

	var mte *MaxTurnsError
	require.ErrorAs(t, err, &mte)
	assert.Equal(t, "default/model", mte.Model)
	assert.Equal(t, 1, mte.Turns)

	// The result comes back alongside the error so the caller's Spend/ReportUsage
	// pattern still works.
	assert.Equal(t, "max_turns", res.Reason)
	assert.InDelta(t, 0.01, res.TotalCostUSD, 1e-9)
}

// TestReasoningRaw pins the reasoningRaw helper: empty effort returns nil so
// cfg.Reasoning is omitted; a non-empty effort marshals to the OpenRouter
// reasoning object. Non-standard tiers (e.g. "xhigh") pass through verbatim.
func TestReasoningRaw(t *testing.T) {
	assert.Nil(t, reasoningRaw(""))
	assert.JSONEq(t, `{"effort":"high"}`, string(reasoningRaw("high")))
	assert.JSONEq(t, `{"effort":"xhigh"}`, string(reasoningRaw("xhigh"))) // non-standard tier passes through
}

func TestPlanTurnCap(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		base   int
		repair bool
		want   int
	}{
		"first attempt caps at the plan cap":      {base: 45, repair: false, want: planMaxTurns},
		"repair caps tighter":                     {base: 45, repair: true, want: planRepairMaxTurns},
		"a smaller configured base is not raised": {base: 8, repair: false, want: 8},
		"a smaller base wins over the repair cap": {base: 8, repair: true, want: 8},
		"base equal to the cap is unchanged":      {base: planMaxTurns, repair: false, want: planMaxTurns},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, planTurnCap(tc.base, tc.repair))
		})
	}
}

// The nudge must land inside the capped budget or the cap silently removes the
// only forcing function the plan phase has.
func TestPlanCapLeavesRoomForTheWrapUpNudge(t *testing.T) {
	t.Parallel()

	assert.Greater(t, planMaxTurns, wrapUpTurns,
		"planMaxTurns must exceed wrapUpTurns so the wrap-up nudge still fires")
	assert.Greater(t, planRepairMaxTurns, wrapUpTurns,
		"the repair budget must also leave room for the nudge")
}

// TestSpendAndReport pins the shared accounting tail of every model call: the
// spend lands on the GIVEN ledger, the reported model is res.ModelUsed with a
// fallback to the configured slug only when the provider echoed none, the
// report targets the given card, the current phase / step / duration telemetry
// rides the report, and a report failure is advisory (the used model still
// returns, nothing propagates).
func TestSpendAndReport(t *testing.T) {
	tests := []struct {
		name      string
		modelUsed string
		reportErr error
		want      string
	}{
		{"echoed model reported as-is", "provider/echoed", nil, "provider/echoed"},
		{"empty echo falls back to configured", "", nil, "configured/model"},
		{"report failure is advisory", "provider/echoed", errors.New("report boom"), "provider/echoed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{taskContext: cmclient.TaskContext{}, reportUsageErr: tt.reportErr}
			d := planTestDeps(ops, &planLLM{})

			o := newRun(d, ops.taskContext)
			o.curPhase = "review"

			res := harness.Result{
				ModelUsed:        tt.modelUsed,
				PromptTokens:     10,
				CompletionTokens: 5,
				TotalCostUSD:     0.25,
			}

			before := o.ledger.Spent()

			used := o.spendAndReport(context.Background(), o.ledger, "TARGET-1",
				"test: report usage failed", res, "configured/model", "gate", 1234*time.Millisecond)

			assert.Equal(t, tt.want, used)
			assert.InDelta(t, 0.25, o.ledger.Spent()-before, 1e-9, "spend lands on the given ledger")
			assert.GreaterOrEqual(t, indexOfCall(ops.recorded(), "ReportUsage:TARGET-1"), 0,
				"usage reported on the given target card")

			report := ops.lastUsageReport()
			assert.Equal(t, tt.want, report.Model, "reported model matches the returned one")
			assert.Equal(t, "review", report.Phase, "current phase rides the report")
			assert.Equal(t, "gate", report.Step, "step tag rides the report")
			assert.Equal(t, int64(1234), report.DurationMS, "duration rides the report in milliseconds")
		})
	}
}

// TestRunModelPassesThroughNormalResult pins that a normal (done) run is NOT
// turned into an error by runModel - only context_limit is normalized.
func TestRunModelPassesThroughNormalResult(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{}}
	llmFake := &planLLM{responses: []llm.Response{stopResp("all good", 0.02)}}
	d := planTestDeps(ops, llmFake)

	o := newRun(d, ops.taskContext)

	res, _, err := o.runModel(context.Background(), d.ReadTools, "do the thing", "default/model")
	require.NoError(t, err)
	assert.Equal(t, "all good", res.Output)
}

// TestSpendAndReportSyncsRunLedger pins the server-floor fix: a cost-less
// gateway call (TotalCostUSD 0) still lands CM's server-priced total on the
// run ledger via the report_usage response.
func TestSpendAndReportSyncsRunLedger(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{}, reportUsageTotal: 3.5}
	d := planTestDeps(ops, &planLLM{})
	o := newRun(d, ops.taskContext)

	res := harness.Result{PromptTokens: 10, CompletionTokens: 5, TotalCostUSD: 0}

	o.spendAndReport(context.Background(), o.ledger, "TARGET-1",
		"test: report usage failed", res, "configured/model", "main", time.Second)

	assert.InDelta(t, 3.5, o.ledger.Spent(), 1e-9,
		"the server-priced total enforces despite a $0 local charge")
}

// TestSpendAndReportCandidateLedgerRouting pins the routing rule: a candidate
// charges its own sub-ledger locally, but the parent-card server total syncs
// into the RUN ledger only - candidates must not absorb their siblings' spend.
func TestSpendAndReportCandidateLedgerRouting(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{}, reportUsageTotal: 3.5}
	d := planTestDeps(ops, &planLLM{})
	o := newRun(d, ops.taskContext)

	cand := NewLedger(2.0, 0)
	res := harness.Result{PromptTokens: 10, CompletionTokens: 5, TotalCostUSD: 0.25}

	o.spendAndReport(context.Background(), cand, "PARENT-1",
		"test: report usage failed", res, "configured/model", "main", time.Second)

	assert.InDelta(t, 0.25, cand.Spent(), 1e-9, "candidate ledger takes only its local charge")
	assert.InDelta(t, 3.5, o.ledger.Spent(), 1e-9, "the server total lands on the run ledger only")
}

// TestSpendAndReportNoSyncOnReportFailure pins that a failed report leaves the
// ledger on local accounting alone - no stale zero overwrites, no phantom sync.
func TestSpendAndReportNoSyncOnReportFailure(t *testing.T) {
	ops := &fakeOps{
		taskContext: cmclient.TaskContext{}, reportUsageTotal: 3.5,
		reportUsageErr: errors.New("report boom"),
	}
	d := planTestDeps(ops, &planLLM{})
	o := newRun(d, ops.taskContext)

	res := harness.Result{TotalCostUSD: 0.25}

	o.spendAndReport(context.Background(), o.ledger, "TARGET-1",
		"test: report usage failed", res, "configured/model", "main", time.Second)

	assert.InDelta(t, 0.25, o.ledger.Spent(), 1e-9, "a failed report leaves only the local charge")
}

// TestDiagnoseConfigCarriesWrapUpNudge proves the diagnosis phase opts into
// the wrap-up nudge, exactly like the planner: when the run burns down to
// wrapUpTurns remaining, the diagnosis-specific nudge is injected as a user
// message, steering the model to emit its "## Diagnosis" block before the cap
// instead of exploring into it.
func TestDiagnoseConfigCarriesWrapUpNudge(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{Title: "Fix the bug", Description: "it crashes"}}
	// Three burn turns, then the diagnosis text: with MaxTurns=8 the nudge
	// fires after 8-5=3 consumed turns, before the model emits its diagnosis.
	client := &planLLM{responses: []llm.Response{
		burnResp(""), burnResp(""), burnResp(""),
		stopResp("## Diagnosis\n### Root cause\nx\n", 0.01),
	}}
	d := planTestDeps(ops, client)
	d.Cfg.MaxTurns = 8
	o := newRun(d, ops.taskContext)

	_, err := o.runDiagnose(context.Background(), "default/model")
	require.NoError(t, err)

	joined := strings.Join(client.tasks, "\n")
	assert.Contains(t, joined, diagnoseWrapUpMessage,
		"the wrap-up nudge reaches the diagnosis conversation as a user message")
}

// TestDiagnoseTurnCapNeverRaisesTheConfiguredBase mirrors TestPlanTurnCap: the
// diagnosis phase cap only ever tightens the configured budget, never lifts it.
func TestDiagnoseTurnCapNeverRaisesTheConfiguredBase(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		base int
		want int
	}{
		"a smaller configured base is not raised": {base: 10, want: 10},
		"a larger configured base is capped":      {base: 100, want: diagnoseMaxTurns},
		"base equal to the cap is unchanged":      {base: diagnoseMaxTurns, want: diagnoseMaxTurns},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, diagnoseTurnCap(tc.base))
		})
	}
}

// TestGuardedPhasesUnchanged pins that giving the diagnosis phase its own
// end-of-run guards does not disturb the already-guarded phases. The coder
// keeps its wrap-up nudge and its grace-turn finish at the cap - its
// WriteTools registry carries the finish tool, so the grace call actually
// lands. The plan phase keeps its own wrap-up nudge and its own turn cap,
// distinct from diagnoseMaxTurns, and still requests no grace turn: like
// diagnosis, the planner's registry carries no Terminal tool, so the field
// would be inert there too - see the doc comment on runModelDiagnose.
func TestGuardedPhasesUnchanged(t *testing.T) {
	t.Run("coder keeps its wrap-up nudge and grace-turn finish", func(t *testing.T) {
		ops := &fakeOps{}
		git := &fakeGit{committed: true}
		// Five burn turns == MaxTurns(5) caps the main loop; the sixth response
		// is consumed by the grace call, which lands finish before max_turns is
		// returned (mirrors TestCoderGraceTurnFinishes).
		client := &planLLM{responses: append(burnResps(5), finishResp("feat: done", 0.01))}
		d := execTestDeps(ops, git, client)
		d.Cfg.MaxTurns = 5
		o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Sizing: seedSizing("simple")}}, 0)

		require.NoError(t, runExecute(context.Background(), o))

		joined := strings.Join(client.tasks, "\n")
		assert.Contains(t, joined, coderWrapUpMessage(wrapUpTurns), "the coder still gets its own wrap-up nudge")

		calls := ops.recorded()
		assert.GreaterOrEqual(t, indexOfCall(calls, "CompleteTask:SUB-1"), 0,
			"the coder still lands a grace-turn finish at its cap; calls=%v", calls)
	})

	t.Run("plan keeps its own wrap-up nudge and its own turn cap", func(t *testing.T) {
		ops := &fakeOps{}
		// Three burn turns, then the plan JSON: with MaxTurns=8 the nudge fires
		// after 8-5=3 consumed turns (mirrors TestRunPlanGetsWrapUpNudge).
		client := &planLLM{responses: []llm.Response{
			burnResp(""), burnResp(""), burnResp(""),
			stopResp(onePlanJSON, 0.01),
		}}
		o := autoPlanRun(ops, client, 8)

		require.NoError(t, runPlan(context.Background(), o))
		assert.Len(t, ops.createCardArgs, 1, "the plan still runs to completion")

		joined := strings.Join(client.tasks, "\n")
		assert.Contains(t, joined, planWrapUpMessage, "the plan still gets its own wrap-up nudge")

		assert.Equal(t, planMaxTurns, planTurnCap(999, false),
			"the plan's own cap is untouched and stays distinct from diagnoseMaxTurns")
	})
}
