package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// ContextLimitError marks a phase stopping because the model neared its context
// window. The worker maps it like the budget park: push WIP, release, fail — so
// in-flight work survives and a human can split the subtask or pin a larger-window model.
type ContextLimitError struct {
	Model         string
	ContextWindow int
}

func (e *ContextLimitError) Error() string {
	return fmt.Sprintf("context limit reached for model %q (window %d tokens)", e.Model, e.ContextWindow)
}

// MaxTurnsError marks a phase stopping because the harness exhausted its turn
// cap (Reason "max_turns", Completed=false, err==nil). It is normalized to a
// typed error at the runModelCfg choke point so NO phase treats truncated work
// as success — the invariant is that partial work is never committed+pushed+
// marked done. The worker maps it like the context-limit park: push WIP,
// release, fail — so the partial work survives for resume.
type MaxTurnsError struct {
	Model string
	Turns int
}

func (e *MaxTurnsError) Error() string {
	return fmt.Sprintf("turn cap reached on model %q after %d turns", e.Model, e.Turns)
}

// IncapableError marks a phase stopping because the model cannot drive the tool
// loop — it emitted tool calls every turn but none parsed valid arguments. The
// recovery path (a later task) catches this to blacklist the model and re-select.
type IncapableError struct {
	Model  string
	Reason string
}

func (e *IncapableError) Error() string {
	return fmt.Sprintf("model %q is harness-incapable: %s", e.Model, e.Reason)
}

// harnessConfig builds the per-phase harness.Config with the run-wide safety
// fields (size cap, secret redaction) plus the model's own context window.
// Centralizing this is the guard against a new phase forgetting the hardening.
//
// A ContextWindow of 0 (model absent from the catalog) is intentional and safe:
// the harness guards the limit check with `if cfg.ContextWindow > 0`, so an
// unknown/uncatalogued model simply opts out of context-limit detection rather
// than tripping it spuriously.
func (o *run) harnessConfig(model string) harness.Config {
	cfg := harness.Config{
		Model:              model,
		MaxTurns:           o.d.Cfg.MaxTurns,
		ToolOutputMaxBytes: o.d.Cfg.ToolOutputMax,
		RedactToolOutput:   o.d.Redact,
		ContextWindow:      o.d.Registry.ContextWindow(model),
		Provider:           o.d.Cfg.Provider,
	}

	// Opt into in-window compaction only when enabled; otherwise leave
	// Compaction nil so the harness keeps its hard context_limit stop.
	if o.d.Cfg.Compaction.Enabled {
		cfg.Compaction = &harness.Compaction{
			Threshold:       o.d.Cfg.Compaction.Threshold,
			KeepRecentTurns: o.d.Cfg.Compaction.KeepRecentTurns,
		}
	}

	// Reasoning is nil when effort is empty (off) so the field is omitted and
	// models that don't support it are unaffected.
	cfg.Reasoning = reasoningRaw(o.d.Cfg.ReasoningEffort)

	return cfg
}

// reasoningRaw renders an effort string to the OpenRouter reasoning object the
// harness carries. Returns nil for "" so the field is omitted. The L1 dialect
// translates this to reasoning_effort for the openai endpoint.
func reasoningRaw(effort string) json.RawMessage {
	if effort == "" {
		return nil
	}

	raw, _ := (llm.Reasoning{Effort: &effort}).Raw() //nolint:errcheck

	return raw
}

// runModel routes a phase model-call through the centralized config and
// normalizes a context_limit/incapable result into a typed error.
func (o *run) runModel(ctx context.Context, reg *tools.Registry, prompt, model string) (harness.Result, error) {
	return o.runModelCfg(ctx, reg, prompt, model, o.harnessConfig(model))
}

// runModelImages is runModel with the card's images attached to the seed
// message. Used by the planning phase only.
func (o *run) runModelImages(ctx context.Context, reg *tools.Registry, prompt, model string, images []llm.ImageURL) (harness.Result, error) {
	cfg := o.harnessConfig(model)
	cfg.TaskImages = images

	return o.runModelCfg(ctx, reg, prompt, model, cfg)
}

func (o *run) runModelCfg(ctx context.Context, reg *tools.Registry, prompt, model string, cfg harness.Config) (harness.Result, error) {
	res, err := harness.Run(ctx, o.d.Client, reg, o.d.Emit, prompt, cfg)
	if err == nil && res.Reason == "context_limit" {
		return res, &ContextLimitError{Model: model, ContextWindow: o.d.Registry.ContextWindow(model)}
	}

	if err == nil && res.Reason == harness.ReasonIncapable {
		return res, &IncapableError{Model: model, Reason: "cannot drive the tool loop"}
	}

	if err == nil && res.Reason == "max_turns" {
		return res, &MaxTurnsError{Model: model, Turns: res.Turns}
	}

	return res, err
}
