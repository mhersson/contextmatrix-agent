package runkind

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/config"
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

// The investigation's turn cap and the turns its wrap-up nudge leaves.
const (
	investigateMaxTurns    = 80
	investigateWrapUpTurns = 5
)

var (
	errNoModelAccess = errors.New("investigate: the run has no LLM access")
	errNoReport      = errors.New("investigate: the run ended without " + reportToolName)
)

// investigateDeps are the investigation's effects: the executor and the client
// it runs the model on (nil on Claude Code), and the run's redactor.
type investigateDeps struct {
	executor orchestrator.Executor
	client   llm.LLM
	redact   func(string) string
}

// newInvestigateDeps builds the model access as a card run does: the Claude
// Code executor on a subscription, else the harness over a client metered
// against the run's ceiling. With no LLM access, as for check, it is empty.
func newInvestigateDeps(spec Spec, client llm.LLM, redact func(string) string) (investigateDeps, error) {
	d := investigateDeps{redact: redact}

	switch {
	case claudeCode(spec):
		c, err := modelexec.NewClaude(modelexec.ClaudeConfig{
			OAuthToken: spec.LLMOAuthToken,
			Env:        modelexec.ClaudeEnv(tools.ScrubbedEnv(nil), spec.CACertFile, spec.BashTimeoutMax),
			Effort:     spec.LLMEffort,
		})
		if err != nil {
			return investigateDeps{}, fmt.Errorf("investigate executor: %w", err)
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

// investigate runs one read-only model pass over the stopped run's inputs and
// returns its report. The usage is returned on every path that ran the model.
func investigate(
	ctx context.Context,
	ws string,
	spec Spec,
	in protocol.InvestigateInput,
	d investigateDeps,
	emit *events.Emitter,
) (protocol.InvestigateResult, []protocol.RunUsage, error) {
	if d.executor == nil {
		return protocol.InvestigateResult{}, nil, errNoModelAccess
	}

	branch, err := fetchWorkBranch(ctx, spec, ws)
	if err != nil {
		return protocol.InvestigateResult{}, nil, err
	}

	// The read roots must exist before the tools are built.
	if err := writeInputs(spec.InputsDir, in); err != nil {
		return protocol.InvestigateResult{}, nil, fmt.Errorf("write inputs: %w", err)
	}

	cfg := investigateConfig(spec, d.redact)
	task := d.redact(investigatePrompt(orchestrator.Grounding(ctx, ws), ws, spec.InputsDir, spec.RunLogPath, in, branch))

	res, err := d.executor.Run(ctx, orchestrator.RunRequest{
		Client:   d.client,
		Registry: investigateTools(ws, spec),
		Emit:     emit,
		Task:     task,
		Config:   cfg,
		Dir:      ws,
	})
	usage := []protocol.RunUsage{usageRow(cfg.Model, res, claudeCode(spec), d.executor.SubscriptionUsage())}

	if err != nil {
		return protocol.InvestigateResult{}, usage, fmt.Errorf("investigate run: %w", err)
	}

	if err := harnessStop(res, cfg); err != nil {
		return protocol.InvestigateResult{}, usage, err
	}

	if res.CompletionArgs == nil {
		return protocol.InvestigateResult{}, usage, errNoReport
	}

	r, err := parseReport(res.CompletionArgs)

	return r, usage, err
}

// investigateConfig is the model run's config: the decision model over the
// run's selection, with the ceiling on the binary on Claude Code. On an
// endpoint the metered client holds it.
func investigateConfig(spec Spec, redact func(string) string) harness.Config {
	base := baseModel(spec)

	reg, faults := registry.FromSelection(spec.Selection, base, false)
	for _, f := range faults {
		slog.Warn("selector: payload tier ladder did not validate; using the built-in bars",
			"card_id", spec.CardID, "role", f.Role, "error", f.Reason)
	}

	model := orchestrator.DecisionModel(reg, base)

	cfg := harness.Config{
		Model:              model,
		Reasoning:          reasoning(spec.ReasoningEffort),
		MaxTurns:           investigateMaxTurns,
		WrapUpTurns:        investigateWrapUpTurns,
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

// baseModel is the model the decision floor may raise: the payload's, else
// the serve default, else the built-in capable default.
func baseModel(spec Spec) string {
	switch {
	case spec.Model != "":
		return spec.Model
	case spec.DefaultModel != "":
		return spec.DefaultModel
	default:
		return config.DefaultCapableModel
	}
}

// reasoning is the harness reasoning object for effort; nil when it is empty,
// so the model reasons at its own default.
func reasoning(effort string) json.RawMessage {
	if effort == "" {
		return nil
	}

	raw, _ := (llm.Reasoning{Effort: &effort}).Raw()

	return raw
}

// investigateTools are the diagnose pass's read-only tools on the workspace,
// reading also the inputs and the run log's directory, plus the report tool.
func investigateTools(ws string, spec Spec) *tools.Registry {
	roots := []string{spec.InputsDir, filepath.Dir(spec.RunLogPath)}

	return tools.NewRegistry(
		tools.NewReadTool(ws).WithReadRoots(roots),
		tools.NewGrepTool(ws).WithReadRoots(roots),
		tools.NewGlobTool(ws).WithReadRoots(roots),
		tools.NewGitTool(ws),
		reportTool{},
	)
}

// harnessStop is the error a model run's stop reason ends the investigation
// with; nil for done.
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
		return fmt.Errorf("investigate run stopped: %s", res.Reason)
	}
}

// fetchWorkBranch fetches the card's work branch into refs/remotes/origin/ and
// returns its name; "" when the remote has none.
func fetchWorkBranch(ctx context.Context, spec Spec, ws string) (string, error) {
	git := worker.NewGit(ws, spec.SecretsPath, worker.HostFromRepoURL(spec.RepoURL), spec.CACertFile)

	heads, err := git.RemoteHeads(ctx, cardBranch(spec.CardID))
	if err != nil {
		return "", fmt.Errorf("list work branches: %w", err)
	}

	branch := workBranch(spec.CardID, heads)
	if branch == "" {
		return "", nil
	}

	if err := git.Fetch(ctx, "+refs/heads/"+branch+":refs/remotes/origin/"+branch); err != nil {
		return "", fmt.Errorf("fetch work branch: %w", err)
	}

	return branch, nil
}

// cardBranch is the card's plain work branch in the worker's cm/ namespace.
func cardBranch(cardID string) string {
	return "cm/" + strings.ToLower(cardID)
}

// workBranch picks the card's work branch among heads: the plain branch, else
// the one branch suffixed with a Jira key; "" when neither is listed.
func workBranch(cardID string, heads []string) string {
	plain := cardBranch(cardID)

	var keyed []string

	for _, h := range heads {
		if h == plain {
			return plain
		}

		if key, ok := strings.CutPrefix(h, plain+"-"); ok && worker.BranchKey(key) {
			keyed = append(keyed, h)
		}
	}

	if len(keyed) == 1 {
		return keyed[0]
	}

	return ""
}
