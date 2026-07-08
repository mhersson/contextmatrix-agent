package orchestrator

import (
	"context"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
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
	ops := &fakeOps{taskContext: cmclient.TaskContext{CardID: "CARD-1"}}
	d := planTestDeps(ops, &planLLM{})
	d.Cfg.ToolOutputMax = 65536

	// A non-identity redactor with a recognisable mapping, so the test pins the
	// redactor's BEHAVIOR — not just that the field is non-nil. A mis-wired but
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
	ops := &fakeOps{taskContext: cmclient.TaskContext{CardID: "CARD-1"}}
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
	ops := &fakeOps{taskContext: cmclient.TaskContext{CardID: "CARD-1"}}

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
// NOT. A boundary-precise pair catches a drifted threshold constant — a loose
// "well over the limit" prompt would still pass against e.g. 0.95.
func TestRunModelNormalizesContextLimit(t *testing.T) {
	// "default/model" is in planTestCatalog with ContextLength 131072. The harness
	// trips when prompt_tokens >= int(contextLimitThreshold * window); mirror that
	// exact arithmetic here so the test pins the documented 0.85 constant.
	// threshold is a var (not a const) so the conversion truncates at runtime,
	// exactly as the harness does — a const expression would be a compile error.
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
			ops := &fakeOps{taskContext: cmclient.TaskContext{CardID: "CARD-1"}}
			resp := llm.Response{
				Content:      "partial",
				FinishReason: "stop",
				Usage:        llm.Usage{PromptTokens: tt.promptTokens, Cost: 0.01},
			}
			llmFake := &planLLM{responses: []llm.Response{resp}}
			d := planTestDeps(ops, llmFake)

			o := newRun(d, ops.taskContext)

			res, err := o.runModel(context.Background(), d.ReadTools, "do the thing", "default/model")

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
// runModel surfaces it as an *IncapableError carrying the model name. It mirrors
// the context-limit test: inject a fake LLM whose every response contains an
// unparseable tool call (bad JSON → harness never marks turnHadCapableTool) so
// after IncapableThreshold turns the harness sets Reason = ReasonIncapable.
func TestRunModelNormalizesIncapable(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{CardID: "CARD-1"}}

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

	res, err := o.runModel(context.Background(), d.ReadTools, "do the thing", "default/model")

	require.Error(t, err, "incapable must surface as an error")

	var ie *IncapableError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, "default/model", ie.Model)

	// The result is returned alongside the error so the caller's
	// Spend/ReportUsage pattern still works.
	assert.Equal(t, "incapable", res.Reason)
}

// TestRunModelNormalizesMaxTurns pins that a run stopping at the turn cap
// (Reason "max_turns", Completed=false, err==nil from the harness) surfaces as
// a *MaxTurnsError, so no phase can treat truncated work as success.
func TestRunModelNormalizesMaxTurns(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{CardID: "CARD-1"}}

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

	res, err := o.runModel(context.Background(), d.ReadTools, "do the thing", "default/model")

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

// TestCoderMaxTurns pins the tier-scaled coder turn budget: simple/moderate keep
// the configured base, complex gets 1.5x and critical 2x (rounded to the nearest
// turn). Expressed as factors of the base so lifting the base lifts every tier
// with it.
func TestCoderMaxTurns(t *testing.T) {
	tests := []struct {
		name string
		base int
		tier registry.Tier
		want int
	}{
		{"simple keeps base", 45, registry.TierSimple, 45},
		{"moderate keeps base", 45, registry.TierModerate, 45},
		{"complex is 1.5x base, rounded", 45, registry.TierComplex, 68}, // round(67.5)
		{"critical is 2x base", 45, registry.TierCritical, 90},
		{"complex scales with a lifted base", 60, registry.TierComplex, 90},
		{"critical scales with a lifted base", 60, registry.TierCritical, 120},
		{"complex scales with a lowered base", 30, registry.TierComplex, 45},
		{"critical scales with a lowered base", 30, registry.TierCritical, 60},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, coderMaxTurns(tt.base, tt.tier))
		})
	}
}

// TestRunModelPassesThroughNormalResult pins that a normal (done) run is NOT
// turned into an error by runModel — only context_limit is normalized.
func TestRunModelPassesThroughNormalResult(t *testing.T) {
	ops := &fakeOps{taskContext: cmclient.TaskContext{CardID: "CARD-1"}}
	llmFake := &planLLM{responses: []llm.Response{stopResp("all good", 0.02)}}
	d := planTestDeps(ops, llmFake)

	o := newRun(d, ops.taskContext)

	res, err := o.runModel(context.Background(), d.ReadTools, "do the thing", "default/model")
	require.NoError(t, err)
	assert.Equal(t, "all good", res.Output)
}
