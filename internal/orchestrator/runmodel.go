package orchestrator

import (
	"context"
	"fmt"

	"github.com/mhersson/contextmatrix-harness/harness"
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
	return harness.Config{
		Model:              model,
		MaxTurns:           o.d.Cfg.MaxTurns,
		ToolOutputMaxBytes: o.d.Cfg.ToolOutputMax,
		RedactToolOutput:   o.d.Redact,
		ContextWindow:      o.d.Registry.ContextWindow(model),
	}
}

// runModel routes a phase model-call through the centralized config and
// normalizes a context_limit result into an error, so a phase never proceeds on
// truncated output. It returns res alongside the error so callers can still
// Spend/ReportUsage before checking err (their existing pattern).
func (o *run) runModel(ctx context.Context, reg *tools.Registry, prompt, model string) (harness.Result, error) {
	res, err := harness.Run(ctx, o.d.Client, reg, o.d.Emit, prompt, o.harnessConfig(model))
	if err == nil && res.Reason == "context_limit" {
		return res, &ContextLimitError{Model: model, ContextWindow: o.d.Registry.ContextWindow(model)}
	}

	if err == nil && res.Reason == harness.ReasonIncapable {
		return res, &IncapableError{Model: model, Reason: "cannot drive the tool loop"}
	}

	return res, err
}
