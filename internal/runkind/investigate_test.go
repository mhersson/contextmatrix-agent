package runkind

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/redact"
	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkBranch(t *testing.T) {
	tests := []struct {
		name  string
		card  string
		heads []string
		want  string
	}{
		{"plain branch wins", "ABC-12", []string{"cm/abc-12", "cm/abc-123", "cm/abc-12-PROJ-7"}, "cm/abc-12"},
		{"the one keyed branch", "ABC-12", []string{"cm/abc-123", "cm/abc-12-PROJ-7"}, "cm/abc-12-PROJ-7"},
		{"a key with an underscore", "ABC-12", []string{"cm/abc-12-MY_PROJ-7"}, "cm/abc-12-MY_PROJ-7"},
		{"not a key", "ABC-12", []string{"cm/abc-12-notakey"}, ""},
		{"two keyed branches", "ABC-12", []string{"cm/abc-12-PROJ-7", "cm/abc-12-PROJ-8"}, ""},
		{"another card's branch", "ABC-12", []string{"cm/abc-123"}, ""},
		{"no heads", "ABC-12", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, workBranch(tt.card, tt.heads))
		})
	}
}

func TestCardFileID(t *testing.T) {
	tests := []struct {
		name, file, want string
	}{
		{"frontmatter id", "---\nid: ABC-13\ntitle: x\n---\nbody", "ABC-13"},
		{"no frontmatter", "no frontmatter", ""},
		{"no id", "---\ntitle: x\n---\nbody", ""},
		{"unclosed frontmatter", "---\nid: ABC-13\n", ""},
		{"invalid yaml", "---\nid: [ABC-13\n---\n", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, cardFileID(tt.file))
		})
	}
}

func TestWriteInputs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "investigation")
	in := protocol.InvestigateInput{
		Card:     "---\nid: ABC-12\n---\ncard body\n",
		Subtasks: []string{"---\nid: ABC-13\n---\nfirst\n", "no frontmatter\n"},
		Siblings: []string{"---\nid: ABC-20\n---\nsibling\n", "---\nid: ../../escape\n---\nbad id\n"},
	}

	require.NoError(t, writeInputs(root, in))

	want := map[string]string{
		"card.md":            in.Card,
		"subtasks/ABC-13.md": in.Subtasks[0],
		"subtasks/2.md":      in.Subtasks[1],
		"siblings/ABC-20.md": in.Siblings[0],
		"siblings/2.md":      in.Siblings[1],
	}

	for rel, body := range want {
		path := filepath.Join(root, rel)

		got, err := os.ReadFile(path)
		require.NoError(t, err, rel)
		assert.Equal(t, body, string(got), rel)

		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Zero(t, info.Mode().Perm()&0o222, "%s is read-only", rel)
	}

	assert.NoFileExists(t, filepath.Join(root, "run.log"))
	assert.NoFileExists(t, filepath.Join(filepath.Dir(root), "escape.md"))
}

func TestWriteInputsWithoutSubtasksOrSiblings(t *testing.T) {
	root := filepath.Join(t.TempDir(), "investigation")
	require.NoError(t, writeInputs(root, protocol.InvestigateInput{Card: "card\n"}))
	assert.FileExists(t, filepath.Join(root, "card.md"))
}

func TestParseReport(t *testing.T) {
	full := `{
		"diagnosis": "the verify command needs a service the worker lacks",
		"cause": "card",
		"fix": {
			"section": "Run the unit suite only.",
			"verify": {"command": "./ci/verify unit", "timeout_seconds": 600, "env": ["A"]},
			"retire_subtasks": ["ABC-13"]
		},
		"forward_to": ["ABC-20"],
		"findings": [{"kind": "card_advice", "text": "t", "evidence": "e"}],
		"confidence": "high"
	}`

	got, err := parseReport(json.RawMessage(full))
	require.NoError(t, err)
	assert.Equal(t, protocol.DiagnosisCard, got.Cause)
	require.NotNil(t, got.Fix)
	require.NotNil(t, got.Fix.Verify)
	assert.Equal(t, "./ci/verify unit", got.Fix.Verify.Command)
	assert.Equal(t, 600, got.Fix.Verify.TimeoutSeconds)
	assert.Equal(t, []string{"ABC-13"}, got.Fix.RetireSubtasks)
	assert.Equal(t, []string{"ABC-20"}, got.ForwardTo)
	require.Len(t, got.Findings, 1)
	assert.Equal(t, protocol.FindingCardAdvice, got.Findings[0].Kind)

	_, err = parseReport(json.RawMessage("not json"))
	require.Error(t, err)
}

func TestReportFault(t *testing.T) {
	finding := func(kind, text string) protocol.FrictionFinding {
		return protocol.FrictionFinding{Kind: kind, Text: text, Evidence: "e"}
	}

	tests := []struct {
		name   string
		report protocol.InvestigateResult
		fault  bool
	}{
		{"minimal", protocol.InvestigateResult{Diagnosis: "d", Cause: protocol.DiagnosisModel}, false},
		{"every cause and finding kind", protocol.InvestigateResult{
			Diagnosis: "d", Cause: protocol.DiagnosisEcosystem,
			Findings: []protocol.FrictionFinding{
				finding(protocol.FindingGuidance, "t"), finding(protocol.FindingCode, "t"),
				finding(protocol.FindingCardAdvice, "t"), finding(protocol.FindingProjectVerify, "t"),
				finding(protocol.FindingHardCard, "t"), finding(protocol.FindingEcosystem, "t"),
			},
		}, false},
		{"card cause without a fix", protocol.InvestigateResult{Diagnosis: "d", Cause: protocol.DiagnosisCard}, false},
		{"fix with no field set", protocol.InvestigateResult{Diagnosis: "d", Cause: protocol.DiagnosisCard, Fix: &protocol.CardFix{}}, false},
		{"empty diagnosis", protocol.InvestigateResult{Cause: protocol.DiagnosisModel}, true},
		{"blank diagnosis", protocol.InvestigateResult{Diagnosis: " \n\t", Cause: protocol.DiagnosisModel}, true},
		{"no cause", protocol.InvestigateResult{Diagnosis: "d"}, true},
		{"cause in the wrong case", protocol.InvestigateResult{Diagnosis: "d", Cause: "Card"}, true},
		{"cause that is a finding kind", protocol.InvestigateResult{Diagnosis: "d", Cause: protocol.FindingCode}, true},
		{"unknown finding kind", protocol.InvestigateResult{
			Diagnosis: "d", Cause: protocol.DiagnosisModel,
			Findings: []protocol.FrictionFinding{finding("bug", "t")},
		}, true},
		{"finding kind that is a cause", protocol.InvestigateResult{
			Diagnosis: "d", Cause: protocol.DiagnosisModel,
			Findings: []protocol.FrictionFinding{finding(protocol.DiagnosisModel, "t")},
		}, true},
		{"finding without text", protocol.InvestigateResult{
			Diagnosis: "d", Cause: protocol.DiagnosisModel,
			Findings: []protocol.FrictionFinding{finding(protocol.FindingCode, " ")},
		}, true},
		{"a later finding at fault", protocol.InvestigateResult{
			Diagnosis: "d", Cause: protocol.DiagnosisModel,
			Findings: []protocol.FrictionFinding{finding(protocol.FindingCode, "t"), finding("", "t")},
		}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.fault {
				assert.Error(t, reportFault(tt.report))
			} else {
				assert.NoError(t, reportFault(tt.report))
			}
		})
	}
}

func TestReportToolEndsTheRunOnlyOnAValidReport(t *testing.T) {
	tool := reportTool{}

	term, ok := any(tool).(tools.Terminal)
	require.True(t, ok)
	assert.True(t, term.Terminal())

	_, err := tool.Execute(t.Context(), map[string]any{"diagnosis": "d", "cause": "model"})
	require.NoError(t, err)

	_, err = tool.Execute(t.Context(), map[string]any{"diagnosis": "d", "cause": "card", "fix": "rewrite the card"})
	require.Error(t, err, "a malformed report is refused, so the model can correct it")

	_, err = tool.Execute(t.Context(), map[string]any{"diagnosis": "d", "cause": "Card"})
	require.Error(t, err, "a report ContextMatrix would reject is refused")
}

func TestReportToolSchemaMirrorsTheResult(t *testing.T) {
	var schema struct {
		Properties struct {
			Cause    struct{ Enum []string } `json:"cause"`
			Fix      struct{ Properties map[string]json.RawMessage }
			Findings struct {
				Items struct {
					Properties struct {
						Kind struct{ Enum []string } `json:"kind"`
					}
				}
			}
		}

		Required []string
	}

	raw := reportTool{}.Schema().Function.Parameters
	require.NoError(t, json.Unmarshal(raw, &schema))

	var top struct{ Properties map[string]json.RawMessage }
	require.NoError(t, json.Unmarshal(raw, &top))

	assert.ElementsMatch(t, []string{"diagnosis", "cause", "fix", "forward_to", "findings"}, keys(top.Properties))
	assert.ElementsMatch(t, []string{"section", "verify", "retire_subtasks"}, keys(schema.Properties.Fix.Properties))
	assert.ElementsMatch(t, []string{"diagnosis", "cause"}, schema.Required)
	assert.ElementsMatch(t, []string{
		protocol.DiagnosisCard, protocol.DiagnosisModel, protocol.DiagnosisEnvironment, protocol.DiagnosisEcosystem,
	}, schema.Properties.Cause.Enum)
	assert.ElementsMatch(t, []string{
		protocol.FindingGuidance, protocol.FindingCode, protocol.FindingCardAdvice,
		protocol.FindingProjectVerify, protocol.FindingHardCard, protocol.FindingEcosystem,
	}, schema.Properties.Findings.Items.Properties.Kind.Enum)
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}

func TestHarnessStop(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{"done", protocol.StopCauseDone},
		{"max_turns", protocol.StopCauseMaxTurns},
		{"context_limit", protocol.StopCauseContextLimit},
		{"max_cost", protocol.StopCauseBudgetExceeded},
		{harness.ReasonIncapable, protocol.StopCauseError},
		{"canceled", protocol.StopCauseError},
	}

	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			assert.Equal(t, tt.want, causeOf(harnessStop(harness.Result{Reason: tt.reason}, harness.Config{Model: "vendor/a"})))
		})
	}
}

// fakeExecutor records the one run it is asked for and answers with res, err.
type fakeExecutor struct {
	calls   int
	req     orchestrator.RunRequest
	res     harness.Result
	err     error
	windows []cmclient.SubscriptionUsage
}

func (f *fakeExecutor) Run(_ context.Context, req orchestrator.RunRequest) (harness.Result, error) {
	f.calls++
	f.req = req

	return f.res, f.err
}

func (f *fakeExecutor) SpawnSubagents(context.Context, orchestrator.FanoutRequest) ([]harness.SubagentResult, error) {
	return nil, errors.New("not used")
}

func (f *fakeExecutor) SubscriptionUsage() []cmclient.SubscriptionUsage { return f.windows }

func reported(args string) harness.Result {
	return harness.Result{
		Completed: true, Reason: "done", CompletionArgs: json.RawMessage(args),
		PromptTokens: 100, CompletionTokens: 10,
	}
}

func investigateSpec(t *testing.T, repoURL string, in protocol.InvestigateInput) Spec {
	t.Helper()

	input, err := json.Marshal(in)
	require.NoError(t, err)

	runLog := filepath.Join(t.TempDir(), "run.log")
	require.NoError(t, os.WriteFile(runLog, []byte("{\"kind\":\"error\"}\n"), 0o444))

	return Spec{
		Kind:       protocol.RunKindInvestigate,
		CardID:     "ABC-012",
		Project:    "abc",
		RepoURL:    repoURL,
		BaseBranch: "main",
		Workspace:  filepath.Join(t.TempDir(), "workspace"),
		InputsDir:  filepath.Join(t.TempDir(), "investigation"),
		RunLogPath: runLog,
		Input:      input,
		Model:      "vendor/model-a",
		MaxCostUSD: 2,
	}
}

// clonedInvestigation clones spec's base branch as run does and returns the
// workspace.
func clonedInvestigation(t *testing.T, spec Spec) string {
	t.Helper()

	ws := filepath.Join(spec.Workspace, strings.ToLower(spec.CardID))
	require.NoError(t, clone(t.Context(), spec, ws))

	return ws
}

func endpointDeps(ex *fakeExecutor) investigateDeps {
	return investigateDeps{executor: ex, client: &fakeLLM{}, redact: func(s string) string { return s }}
}

// built is a deps constructor that returns d.
func built(d investigateDeps) func() (investigateDeps, error) {
	return func() (investigateDeps, error) { return d, nil }
}

func resolvedDirs(t *testing.T, dirs ...string) []string {
	t.Helper()

	out := make([]string, len(dirs))

	for i, d := range dirs {
		r, err := filepath.EvalSymlinks(d)
		require.NoError(t, err)

		out[i] = r
	}

	return out
}

func TestInvestigateUsesReadOnlyToolsAndReports(t *testing.T) {
	in := protocol.InvestigateInput{RunID: "abc-012-x1", Card: "---\nid: ABC-012\n---\nbody\n"}
	spec := investigateSpec(t, bareRepo(t, nil), in)
	spec.Selection = &protocol.SelectionContext{Candidates: []protocol.CandidateModel{
		{Slug: "vendor/judge", ContextWindow: 200000, ReviewerPrior: 0.95, CoderPrior: 0.95, PromptPricePerTok: 1e-6, CompletionPricePerTok: 2e-6},
	}}
	ws := clonedInvestigation(t, spec)

	ex := &fakeExecutor{res: reported(`{"diagnosis":"d","cause":"model"}`)}
	d := endpointDeps(ex)

	got, usage, err := investigate(t.Context(), ws, spec, in, d, discardEmitter())
	require.NoError(t, err)
	assert.Equal(t, protocol.DiagnosisModel, got.Cause)
	assert.Equal(t, "d", got.Diagnosis)

	require.Len(t, usage, 1)
	assert.EqualValues(t, 100, usage[0].InputTokens)
	assert.EqualValues(t, 10, usage[0].OutputTokens)
	assert.Equal(t, "vendor/judge", usage[0].Model)
	assert.Empty(t, usage[0].CostBasis)

	require.Equal(t, 1, ex.calls)

	reg := ex.req.Registry
	require.NotNil(t, reg)

	var names []string
	for _, tool := range reg.All() {
		names = append(names, tool.Name())
	}

	assert.ElementsMatch(t, []string{"read", "grep", "glob", "git", "report_investigation"}, names)

	roots := resolvedDirs(t, spec.InputsDir, filepath.Dir(spec.RunLogPath))

	read, _ := reg.Get("read")
	assert.ElementsMatch(t, roots, read.(tools.ReadTool).ReadRoots().Effective)

	grep, _ := reg.Get("grep")
	assert.ElementsMatch(t, roots, grep.(tools.GrepTool).ReadRoots().Effective)

	glob, _ := reg.Get("glob")
	assert.ElementsMatch(t, roots, glob.(tools.GlobTool).ReadRoots().Effective)

	cfg := ex.req.Config
	assert.Equal(t, "vendor/judge", cfg.Model, "the decision model over the selection")
	assert.Equal(t, 80, cfg.MaxTurns)
	assert.Equal(t, 5, cfg.WrapUpTurns)
	assert.True(t, cfg.GraceTurn)
	assert.Equal(t, 200000, cfg.ContextWindow)
	assert.Zero(t, cfg.MaxCostUSD, "on an endpoint the metered client holds the ceiling")
	require.NotNil(t, cfg.RedactToolOutput)

	assert.Equal(t, ws, ex.req.Dir)
	assert.Equal(t, d.client, ex.req.Client)
	assert.Contains(t, ex.req.Task, spec.RunLogPath)
	assert.FileExists(t, filepath.Join(spec.InputsDir, "card.md"))
}

func TestInvestigateOnClaudeCode(t *testing.T) {
	in := protocol.InvestigateInput{Card: "card\n"}
	spec := investigateSpec(t, bareRepo(t, nil), in)
	spec.LLMType = protocol.LLMEndpointTypeClaudeCode
	ws := clonedInvestigation(t, spec)

	window := cmclient.SubscriptionUsage{Window: "five_hour", Utilization: 0.4}
	ex := &fakeExecutor{res: reported(`{"diagnosis":"d","cause":"environment"}`), windows: []cmclient.SubscriptionUsage{window}}

	got, usage, err := investigate(t.Context(), ws, spec, in, investigateDeps{executor: ex, redact: func(s string) string { return s }}, discardEmitter())
	require.NoError(t, err)
	assert.Equal(t, protocol.DiagnosisEnvironment, got.Cause)
	assert.InDelta(t, spec.MaxCostUSD, ex.req.Config.MaxCostUSD, 1e-12, "on Claude Code the binary holds the ceiling")

	require.Len(t, usage, 1)
	assert.Equal(t, protocol.CostBasisSubscription, usage[0].CostBasis)
	assert.Equal(t, []protocol.SubscriptionUsage{window}, usage[0].Subscription)
}

// scriptedLLM answers each request with the next of replies, then repeats the
// last.
type scriptedLLM struct {
	replies []llm.Response
	calls   int
}

func (s *scriptedLLM) Send(context.Context, llm.Request) (llm.Response, error) {
	r := s.replies[min(s.calls, len(s.replies)-1)]
	s.calls++

	return r, nil
}

func (s *scriptedLLM) SendStream(ctx context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return s.Send(ctx, req)
}

func reportCall(id, args string) llm.Response {
	return llm.Response{
		ToolCalls: []llm.ToolCall{{
			ID: id, Type: "function", Function: llm.FunctionCall{Name: reportToolName, Arguments: args},
		}},
		FinishReason: "tool_calls",
		Usage:        llm.Usage{PromptTokens: 10, CompletionTokens: 5, Cost: 0.01},
	}
}

func TestInvestigateContinuesAfterARefusedReport(t *testing.T) {
	in := protocol.InvestigateInput{Card: "card\n"}
	spec := investigateSpec(t, bareRepo(t, nil), in)
	ws := clonedInvestigation(t, spec)

	client := &scriptedLLM{replies: []llm.Response{
		reportCall("call-1", `{"diagnosis":"d","cause":"Card"}`),
		reportCall("call-2", `{"diagnosis":"d","cause":"model"}`),
	}}

	d, err := newInvestigateDeps(spec, client, func(s string) string { return s })
	require.NoError(t, err)

	got, _, err := investigate(t.Context(), ws, spec, in, d, discardEmitter())
	require.NoError(t, err)
	assert.Equal(t, protocol.DiagnosisModel, got.Cause, "the corrected report ends the run")
	assert.Equal(t, 2, client.calls, "the refused report did not end the run")
}

func TestInvestigateWithoutAReportFails(t *testing.T) {
	in := protocol.InvestigateInput{Card: "card\n"}
	spec := investigateSpec(t, bareRepo(t, nil), in)
	ws := clonedInvestigation(t, spec)

	ex := &fakeExecutor{res: harness.Result{Completed: true, Reason: "done", Output: "it is the model", PromptTokens: 100}}

	_, usage, err := investigate(t.Context(), ws, spec, in, endpointDeps(ex), discardEmitter())
	require.Error(t, err)
	assert.Equal(t, protocol.StopCauseError, causeOf(err))
	require.Len(t, usage, 1, "a run that called the model reports its usage")
	assert.EqualValues(t, 100, usage[0].InputTokens)
}

func TestInvestigateWithoutModelAccessFails(t *testing.T) {
	in := protocol.InvestigateInput{Card: "card\n"}
	spec := investigateSpec(t, bareRepo(t, nil), in)
	ws := clonedInvestigation(t, spec)

	_, usage, err := investigate(t.Context(), ws, spec, in, investigateDeps{}, discardEmitter())
	require.Error(t, err)
	assert.Empty(t, usage)
}

func TestInvestigateFetchesTheWorkBranch(t *testing.T) {
	in := protocol.InvestigateInput{Card: "card\n"}

	t.Run("present", func(t *testing.T) {
		repoURL := bareRepo(t, nil)
		spec := investigateSpec(t, repoURL, in)
		ws := clonedInvestigation(t, spec)

		// Created after the clone, so only the investigation's own fetch finds it.
		gitCmd(t, strings.TrimPrefix(repoURL, "file://"), "branch", "cm/abc-012", "main")

		ex := &fakeExecutor{res: reported(`{"diagnosis":"d","cause":"model"}`)}

		_, _, err := investigate(t.Context(), ws, spec, in, endpointDeps(ex), discardEmitter())
		require.NoError(t, err)

		gitCmd(t, ws, "rev-parse", "--verify", "refs/remotes/origin/cm/abc-012")
		assert.Contains(t, ex.req.Task, "cm/abc-012")
	})

	t.Run("absent", func(t *testing.T) {
		spec := investigateSpec(t, bareRepo(t, nil), in)
		ws := clonedInvestigation(t, spec)

		ex := &fakeExecutor{res: reported(`{"diagnosis":"d","cause":"model"}`)}

		_, _, err := investigate(t.Context(), ws, spec, in, endpointDeps(ex), discardEmitter())
		require.NoError(t, err)
		assert.Equal(t, 1, ex.calls)
	})

	t.Run("list failure", func(t *testing.T) {
		repoURL := bareRepo(t, nil)
		spec := investigateSpec(t, repoURL, in)
		ws := clonedInvestigation(t, spec)

		require.NoError(t, os.RemoveAll(strings.TrimPrefix(repoURL, "file://")))

		ex := &fakeExecutor{res: reported(`{"diagnosis":"d","cause":"model"}`)}

		_, _, err := investigate(t.Context(), ws, spec, in, endpointDeps(ex), discardEmitter())
		require.Error(t, err)
		assert.Zero(t, ex.calls)
	})

	t.Run("fetch failure", func(t *testing.T) {
		repoURL := bareRepo(t, nil)
		spec := investigateSpec(t, repoURL, in)
		ws := clonedInvestigation(t, spec)

		gitCmd(t, strings.TrimPrefix(repoURL, "file://"), "branch", "cm/abc-012", "main")

		// A held ref lock makes the fetch's ref update fail.
		lock := filepath.Join(ws, ".git", "refs", "remotes", "origin", "cm", "abc-012.lock")
		require.NoError(t, os.MkdirAll(filepath.Dir(lock), 0o755))
		require.NoError(t, os.WriteFile(lock, nil, 0o644))

		ex := &fakeExecutor{res: reported(`{"diagnosis":"d","cause":"model"}`)}

		_, _, err := investigate(t.Context(), ws, spec, in, endpointDeps(ex), discardEmitter())
		require.Error(t, err)
		assert.Zero(t, ex.calls)
	})
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...) //nolint:gosec // G204: test-controlled arguments against temp repos
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestInvestigateMapsHarnessStops(t *testing.T) {
	repoURL := bareRepo(t, nil)

	tests := []struct {
		name string
		res  harness.Result
		err  error
		want string
	}{
		{name: "max turns", res: harness.Result{Reason: "max_turns", PromptTokens: 5}, want: protocol.StopCauseMaxTurns},
		{name: "context limit", res: harness.Result{Reason: "context_limit", PromptTokens: 5}, want: protocol.StopCauseContextLimit},
		{name: "max cost", res: harness.Result{Reason: "max_cost", PromptTokens: 5}, want: protocol.StopCauseBudgetExceeded},
		{name: "ceiling before a call", err: &orchestrator.BudgetExceededError{Spent: 2, Max: 2}, want: protocol.StopCauseBudgetExceeded},
		{name: "usage limit", err: &orchestrator.UsageLimitError{}, want: protocol.StopCauseUsageLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := investigateSpec(t, repoURL, protocol.InvestigateInput{Card: "card\n"})
			ex := &fakeExecutor{res: tt.res, err: tt.err}

			d := run(t.Context(), spec, slog.Default(), redact.New(nil), discardEmitter(), deps{investigate: built(endpointDeps(ex))})
			assert.Equal(t, tt.want, d.Cause)
			assert.Nil(t, d.Result)
			assert.Len(t, d.Usage, 1, "the terminal status carries the usage")
		})
	}
}

func TestRunMasksAndBoundsTheInvestigation(t *testing.T) {
	const token = "ghs_investigatesecret123"

	spec := investigateSpec(t, bareRepo(t, nil), protocol.InvestigateInput{Card: "card\n"})
	spec.GitToken = token

	report := protocol.InvestigateResult{
		Diagnosis: token + strings.Repeat("d", 40<<10),
		Cause:     protocol.DiagnosisCard,
		Fix: &protocol.CardFix{
			Section:        "use " + token,
			Verify:         &protocol.VerifyConfig{Command: "./ci/verify " + token, Env: []string{"E_" + token}},
			RetireSubtasks: []string{"R-" + token},
		},
		ForwardTo: []string{"F-" + token},
		Findings: []protocol.FrictionFinding{{
			Kind: protocol.FindingCode, Title: "t " + token, Text: "x " + token, Evidence: "e " + token,
		}},
	}
	args, err := json.Marshal(report)
	require.NoError(t, err)

	ex := &fakeExecutor{res: reported(string(args))}
	red := redact.New([]string{token})

	d := run(t.Context(), spec, slog.Default(), red, discardEmitter(), deps{investigate: built(endpointDeps(ex))})
	require.Equal(t, protocol.StopCauseDone, d.Cause)
	require.NotNil(t, d.Result)
	require.NotNil(t, d.Result.Investigate)
	assert.Nil(t, d.Result.Check)
	require.Len(t, d.Usage, 1)

	out, err := json.Marshal(d.Result)
	require.NoError(t, err)
	assert.NotContains(t, string(out), token)

	got := d.Result.Investigate
	assert.LessOrEqual(t, len(got.Diagnosis), 32<<10)
	require.NotNil(t, got.Fix)
	require.NotNil(t, got.Fix.Verify)
	assert.Contains(t, got.Fix.Verify.Command, "./ci/verify")
	require.Len(t, got.Findings, 1)
}
