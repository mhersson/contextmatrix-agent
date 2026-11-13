// Package runkind runs the sandboxed run kinds ContextMatrix dispatches for one
// card: one file per kind, no MCP. Each run clones the base branch, runs its
// kind and ends with one run_outcome line carrying the masked, bounded result.
package runkind

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-agent/internal/runoutcome"
	"github.com/mhersson/contextmatrix-agent/internal/worker"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/redact"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// progressKind is the event a run kind writes while a long step runs, so its
// stream never looks idle.
const progressKind events.Kind = "run_kind_progress"

// progressInterval is how often a long step reports progress. A var so tests
// can shrink it.
var progressInterval = time.Minute

// Spec is everything a run kind reads from its container.
type Spec struct {
	Kind, CardID, Project, RepoURL, BaseBranch string
	GitToken, SecretsPath, CACertFile          string

	// Workspace holds the clone; InputsDir is where a model kind writes the
	// files its model reads.
	Workspace, InputsDir string
	// Input is the kind's input JSON.
	Input []byte
	// RunLogPath is the stopped run's log section a model kind reads.
	RunLogPath string

	Model, DefaultModel, ReasoningEffort                  string
	Selection                                             *protocol.SelectionContext
	MaxCostUSD                                            float64
	LLMKey, LLMBaseURL, LLMType, LLMOAuthToken, LLMEffort string
	BashTimeoutMax, ToolOutputMax                         int
}

// Run clones spec's base branch, runs its kind and returns the outcome: the
// cause, and on done the result masked with the run's secrets and then bounded.
// The LLM client is for model kinds; check takes nil.
func Run(ctx context.Context, spec Spec, _ llm.LLM, emit *events.Emitter) runoutcome.Data {
	log := slog.Default().With("card_id", spec.CardID, "kind", spec.Kind)
	red := redact.New([]string{spec.GitToken, spec.LLMKey, spec.LLMOAuthToken})

	return run(ctx, spec, log, red, emit, deps{check: newCheckDeps(log, red.Apply)})
}

// deps are the kinds' effects, one field per kind.
type deps struct {
	check checkDeps
}

func run(ctx context.Context, spec Spec, log *slog.Logger, red *redact.Redactor, emit *events.Emitter, d deps) runoutcome.Data {
	ws := filepath.Join(spec.Workspace, strings.ToLower(spec.CardID))

	res, err := runKind(ctx, spec, ws, emit, d)
	if err != nil {
		cause := causeOf(err)
		log.Error("run kind failed", "cause", cause, "error", red.Apply(err.Error()))

		return runoutcome.Data{Cause: cause}
	}

	res = limitResult(maskResult(res, red.Apply))

	return runoutcome.Data{Cause: protocol.StopCauseDone, Result: &res}
}

// runKind is the one place that names the kinds.
func runKind(ctx context.Context, spec Spec, ws string, emit *events.Emitter, d deps) (protocol.RunKindResult, error) {
	if err := clone(ctx, spec, ws); err != nil {
		return protocol.RunKindResult{}, err
	}

	switch spec.Kind {
	case protocol.RunKindCheck:
		var in protocol.CheckInput
		if err := json.Unmarshal(spec.Input, &in); err != nil {
			return protocol.RunKindResult{}, fmt.Errorf("decode check input: %w", err)
		}

		r, err := check(ctx, ws, in, d.check, emit)
		if err != nil {
			return protocol.RunKindResult{}, err
		}

		return protocol.RunKindResult{Check: &r}, nil
	default:
		return protocol.RunKindResult{}, fmt.Errorf("unknown run kind %q", spec.Kind)
	}
}

// clone checks out the base branch, or the remote default when it is empty,
// into ws.
func clone(ctx context.Context, spec Spec, ws string) error {
	if err := os.MkdirAll(spec.Workspace, 0o755); err != nil { //nolint:gosec // G301: the workspace is a git checkout, which uses 0755
		return fmt.Errorf("create workspace: %w", err)
	}

	git := worker.NewGit(ws, spec.SecretsPath, worker.HostFromRepoURL(spec.RepoURL), spec.CACertFile)
	if err := git.Clone(ctx, spec.RepoURL, spec.BaseBranch); err != nil {
		return fmt.Errorf("clone base branch: %w", err)
	}

	return nil
}

// causeOf maps a run kind's error to its stop cause.
func causeOf(err error) string {
	if err == nil {
		return protocol.StopCauseDone
	}

	if _, ok := errors.AsType[*orchestrator.BudgetExceededError](err); ok {
		return protocol.StopCauseBudgetExceeded
	}

	if _, ok := errors.AsType[*orchestrator.UsageLimitError](err); ok {
		return protocol.StopCauseUsageLimit
	}

	return protocol.StopCauseError
}

// maskResult masks every text field of r.
func maskResult(r protocol.RunKindResult, mask func(string) string) protocol.RunKindResult {
	if r.Check != nil {
		c := *r.Check
		c.Candidates = mapStrings(c.Candidates, mask)
		c.AbsentFromRepo = mapStrings(c.AbsentFromRepo, mask)
		r.Check = &c
	}

	return r
}

// mapStrings returns f applied to each of ss in a new slice; nil stays nil.
func mapStrings(ss []string, f func(string) string) []string {
	if ss == nil {
		return nil
	}

	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = f(s)
	}

	return out
}
