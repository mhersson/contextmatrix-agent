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
	// OperatorValues are the operator's worker env values, masked in every
	// result but a check's.
	OperatorValues []string

	Model, DefaultModel, ReasoningEffort                  string
	Selection                                             *protocol.SelectionContext
	MaxCostUSD                                            float64
	LLMKey, LLMBaseURL, LLMType, LLMOAuthToken, LLMEffort string
	BashTimeoutMax, ToolOutputMax                         int
}

// Run clones spec's base branch, runs its kind and returns the outcome: the
// cause, the model usage of a model kind, and on done the result masked with
// the run's secrets, then bounded, then fitted to the outcome limit. The LLM
// client is for model kinds; check takes nil.
func Run(ctx context.Context, spec Spec, client llm.LLM, emit *events.Emitter) runoutcome.Data {
	log := slog.Default().With("project", spec.Project, "card_id", spec.CardID, "kind", spec.Kind)
	red := redact.New([]string{spec.GitToken, spec.LLMKey, spec.LLMOAuthToken})

	return run(ctx, spec, log, red, emit, deps{
		check: newCheckDeps(log, red.Apply),
		model: func() (modelDeps, error) {
			return newModelDeps(spec, client, red.Apply)
		},
		guidance: func(ws string) guidanceDeps { return newGuidanceDeps(spec, ws) },
	})
}

// minOperatorValueBytes is the shortest operator value masked: a shorter one
// would mangle ordinary text.
const minOperatorValueBytes = 8

// operatorMask masks the provisioned secrets and each operator value of 8
// bytes or more: what a run writes to git or to a card.
func operatorMask(spec Spec) func(string) string {
	secrets := []string{spec.GitToken, spec.LLMKey, spec.LLMOAuthToken}

	for _, v := range spec.OperatorValues {
		if len(v) >= minOperatorValueBytes {
			secrets = append(secrets, v)
		}
	}

	return redact.New(secrets).Apply
}

// resultMask masks a result. The results of check and guidance_pr are repo
// text and identifiers (a PR URL, a path) that ContextMatrix matches exactly,
// so they get the provisioned secrets only; any other kind's result can reach
// git as cards, a park entry or a card section, so it gets the operator values
// too.
func resultMask(spec Spec) func(string) string {
	if spec.Kind == protocol.RunKindCheck || spec.Kind == protocol.RunKindGuidancePR {
		return redact.New([]string{spec.GitToken, spec.LLMKey, spec.LLMOAuthToken}).Apply
	}

	return operatorMask(spec)
}

// deps are the kinds' effects, one field per kind. A kind whose effects can
// fail to build gets a constructor, called only when that kind runs.
type deps struct {
	check    checkDeps
	model    func() (modelDeps, error)
	guidance func(ws string) guidanceDeps
}

// workspaceDir is the run's clone directory: the card's, or the kind's for a
// project-level kind that has no card.
func workspaceDir(spec Spec) string {
	if spec.CardID == "" {
		return filepath.Join(spec.Workspace, spec.Kind)
	}

	return filepath.Join(spec.Workspace, strings.ToLower(spec.CardID))
}

func run(ctx context.Context, spec Spec, log *slog.Logger, red *redact.Redactor, emit *events.Emitter, d deps) runoutcome.Data {
	ws := workspaceDir(spec)

	res, usage, err := runKind(ctx, spec, ws, emit, d)
	if err != nil {
		cause := causeOf(err)
		log.Error("run kind failed", "cause", cause, "error", red.Apply(err.Error()))

		return runoutcome.Data{Cause: cause, Usage: usage}
	}

	res = limitResult(maskResult(res, resultMask(spec)))

	return fitOutcome(runoutcome.Data{Cause: protocol.StopCauseDone, Result: &res, Usage: usage}, maxOutcomeBytes)
}

// runKind is the one place that names the kinds. A model kind's usage comes
// back on every path that ran the model.
func runKind(ctx context.Context, spec Spec, ws string, emit *events.Emitter, d deps) (protocol.RunKindResult, []protocol.RunUsage, error) {
	if err := clone(ctx, spec, ws); err != nil {
		return protocol.RunKindResult{}, nil, err
	}

	switch spec.Kind {
	case protocol.RunKindCheck:
		var in protocol.CheckInput
		if err := json.Unmarshal(spec.Input, &in); err != nil {
			return protocol.RunKindResult{}, nil, fmt.Errorf("decode check input: %w", err)
		}

		r, err := check(ctx, ws, in, d.check, emit)
		if err != nil {
			return protocol.RunKindResult{}, nil, err
		}

		return protocol.RunKindResult{Check: &r}, nil, nil
	case protocol.RunKindInvestigate:
		var in protocol.InvestigateInput
		if err := json.Unmarshal(spec.Input, &in); err != nil {
			return protocol.RunKindResult{}, nil, fmt.Errorf("decode investigate input: %w", err)
		}

		md, err := d.model()
		if err != nil {
			return protocol.RunKindResult{}, nil, err
		}

		r, usage, err := investigate(ctx, ws, spec, in, md, emit)
		if err != nil {
			return protocol.RunKindResult{}, usage, err
		}

		return protocol.RunKindResult{Investigate: &r}, usage, nil
	case protocol.RunKindAnalyse:
		var in protocol.AnalyseInput
		if err := json.Unmarshal(spec.Input, &in); err != nil {
			return protocol.RunKindResult{}, nil, fmt.Errorf("decode analyse input: %w", err)
		}

		md, err := d.model()
		if err != nil {
			return protocol.RunKindResult{}, nil, err
		}

		r, usage, err := analyse(ctx, ws, spec, in, md, emit)
		if err != nil {
			return protocol.RunKindResult{}, usage, err
		}

		return protocol.RunKindResult{Analyse: &r}, usage, nil
	case protocol.RunKindGuidancePR:
		var in protocol.GuidancePRInput
		if err := json.Unmarshal(spec.Input, &in); err != nil {
			return protocol.RunKindResult{}, nil, fmt.Errorf("decode guidance_pr input: %w", err)
		}

		r, err := guidancePR(ctx, ws, spec, in, d.guidance(ws))
		if err != nil {
			return protocol.RunKindResult{}, nil, err
		}

		return protocol.RunKindResult{GuidancePR: &r}, nil, nil
	default:
		return protocol.RunKindResult{}, nil, fmt.Errorf("unknown run kind %q", spec.Kind)
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

	if _, ok := errors.AsType[*orchestrator.MaxTurnsError](err); ok {
		return protocol.StopCauseMaxTurns
	}

	if _, ok := errors.AsType[*orchestrator.ContextLimitError](err); ok {
		return protocol.StopCauseContextLimit
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

	if r.Investigate != nil {
		i := maskInvestigate(*r.Investigate, mask)
		r.Investigate = &i
	}

	if r.Analyse != nil {
		a := *r.Analyse
		a.Findings = maskFindings(a.Findings, mask)
		r.Analyse = &a
	}

	if r.GuidancePR != nil {
		g := *r.GuidancePR
		g.Outcome, g.PRURL, g.HeadSHA, g.File = mask(g.Outcome), mask(g.PRURL), mask(g.HeadSHA), mask(g.File)
		g.AbsentFromRepo = mapStrings(g.AbsentFromRepo, mask)
		r.GuidancePR = &g
	}

	return r
}

// maskInvestigate masks every string in r, enums and IDs included: a token the
// model echoed can land in any of them.
func maskInvestigate(r protocol.InvestigateResult, mask func(string) string) protocol.InvestigateResult {
	r.Diagnosis = mask(r.Diagnosis)
	r.Cause = mask(r.Cause)
	r.ForwardTo = mapStrings(r.ForwardTo, mask)

	if r.Fix != nil {
		fix := *r.Fix
		fix.Section = mask(fix.Section)
		fix.RetireSubtasks = mapStrings(fix.RetireSubtasks, mask)

		if fix.Verify != nil {
			v := *fix.Verify
			v.Command = mask(v.Command)
			v.Env = mapStrings(v.Env, mask)
			fix.Verify = &v
		}

		r.Fix = &fix
	}

	r.Findings = maskFindings(r.Findings, mask)

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
