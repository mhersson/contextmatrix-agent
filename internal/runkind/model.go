package runkind

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/mhersson/contextmatrix-agent/internal/modelexec"
	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-agent/internal/worker"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// modelWrapUpTurns is the turns a model kind's wrap-up nudge leaves.
const modelWrapUpTurns = 5

var errNoModelAccess = errors.New("the run has no LLM access")

// modelDeps are a model kind's effects: the executor and the client it runs
// the model on (nil on Claude Code), and the run's redactor.
type modelDeps struct {
	executor orchestrator.Executor
	client   llm.LLM
	redact   func(string) string
}

// newModelDeps builds the model access as a card run does: the Claude
// Code executor on a subscription, else the harness over a client metered
// against the run's ceiling. With no LLM access, as for check, it is empty.
func newModelDeps(spec Spec, client llm.LLM, redact func(string) string) (modelDeps, error) {
	d := modelDeps{redact: redact}

	switch {
	case claudeCode(spec):
		c, err := modelexec.NewClaude(modelexec.ClaudeConfig{
			OAuthToken: spec.LLMOAuthToken,
			Env:        modelexec.ClaudeEnv(tools.ScrubbedEnv(nil), spec.CACertFile, spec.BashTimeoutMax),
			Effort:     spec.LLMEffort,
		})
		if err != nil {
			return modelDeps{}, fmt.Errorf("model executor: %w", err)
		}

		d.executor = c
	case client != nil:
		d.executor, d.client = modelexec.Harness{}, newMeteredClient(client, spec.MaxCostUSD, spec.Selection)
	}

	return d, nil
}

func claudeCode(spec Spec) bool {
	return spec.LLMType == protocol.LLMEndpointTypeClaudeCode
}

// runModel runs one model pass of a kind with its task and terminal tool, and
// returns the result with the usage row. A run error or a stop other than done
// is the error; the usage is returned on both.
func runModel(
	ctx context.Context,
	ws string,
	spec Spec,
	d modelDeps,
	emit *events.Emitter,
	task string,
	report tools.Tool,
	maxTurns int,
) (harness.Result, []protocol.RunUsage, error) {
	cfg := modelConfig(spec, d.redact, maxTurns)

	res, err := d.executor.Run(ctx, orchestrator.RunRequest{
		Client:   d.client,
		Registry: readOnlyTools(ws, spec, report),
		Emit:     emit,
		Task:     task,
		Config:   cfg,
		Dir:      ws,
	})
	usage := []protocol.RunUsage{usageRow(cfg.Model, res, claudeCode(spec), d.executor.SubscriptionUsage())}

	if err != nil {
		return harness.Result{}, usage, fmt.Errorf("model run: %w", err)
	}

	if err := harnessStop(res, cfg); err != nil {
		return harness.Result{}, usage, err
	}

	return res, usage, nil
}

// modelConfig is a model run's config: the decision model over the run's
// selection, with the ceiling on the binary on Claude Code. On an endpoint the
// metered client holds it.
func modelConfig(spec Spec, redact func(string) string, maxTurns int) harness.Config {
	base := worker.CapableDefault(spec.Model, spec.DefaultModel)

	reg, faults := registry.FromSelection(spec.Selection, base, false)
	for _, f := range faults {
		slog.Warn("selector: payload tier ladder did not validate; using the built-in bars",
			"card_id", spec.CardID, "role", f.Role, "error", f.Reason)
	}

	model := orchestrator.DecisionModel(reg, base)

	cfg := harness.Config{
		Model:              model,
		Reasoning:          orchestrator.ReasoningRaw(spec.ReasoningEffort),
		MaxTurns:           maxTurns,
		WrapUpTurns:        modelWrapUpTurns,
		GraceTurn:          true,
		ContextWindow:      reg.ContextWindow(model),
		ToolOutputMaxBytes: spec.ToolOutputMax,
		RedactToolOutput:   redact,
	}

	if claudeCode(spec) {
		cfg.MaxCostUSD = spec.MaxCostUSD
	}

	return cfg
}

// readOnlyTools are a model kind's read-only tools on the workspace, reading
// also the inputs and the run log's directory, plus its terminal tool.
func readOnlyTools(ws string, spec Spec, report tools.Tool) *tools.Registry {
	roots := []string{spec.InputsDir, filepath.Dir(spec.RunLogPath)}

	return tools.NewRegistry(
		tools.NewReadTool(ws).WithReadRoots(roots),
		tools.NewGrepTool(ws).WithReadRoots(roots),
		tools.NewGlobTool(ws).WithReadRoots(roots),
		tools.NewGitTool(ws),
		report,
	)
}

// harnessStop is the error a model run's stop reason ends the run with; nil
// for done.
func harnessStop(res harness.Result, cfg harness.Config) error {
	switch res.Reason {
	case "done":
		return nil
	case "max_turns":
		return &orchestrator.MaxTurnsError{Model: cfg.Model, Turns: res.Turns}
	case "context_limit":
		return &orchestrator.ContextLimitError{Model: cfg.Model, ContextWindow: cfg.ContextWindow}
	case "max_cost":
		return &orchestrator.BudgetExceededError{Spent: res.TotalCostUSD, Max: cfg.MaxCostUSD}
	default:
		return fmt.Errorf("model run stopped: %s", res.Reason)
	}
}
