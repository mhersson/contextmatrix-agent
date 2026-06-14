package orchestrator

import (
	"context"
	"fmt"

	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
)

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
		return res, fmt.Errorf("context limit reached for model %q", model)
	}

	return res, err
}
