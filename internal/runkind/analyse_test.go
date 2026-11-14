package runkind

import (
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/redact"
	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func analyseSpec(t *testing.T, repoURL string, in protocol.AnalyseInput) Spec {
	t.Helper()

	spec := investigateSpec(t, repoURL, protocol.InvestigateInput{})
	spec.Kind = protocol.RunKindAnalyse

	input, err := json.Marshal(in)
	require.NoError(t, err)

	spec.Input = input

	return spec
}

func TestParseAnalysis(t *testing.T) {
	t.Run("a finding", func(t *testing.T) {
		got, err := parseAnalysis(json.RawMessage(`{"findings":[{"kind":"guidance","text":"x","evidence":"e"}],"extra":1}`))
		require.NoError(t, err)
		require.Len(t, got.Findings, 1)
		assert.Equal(t, protocol.FindingGuidance, got.Findings[0].Kind)
		assert.Equal(t, "x", got.Findings[0].Text)
	})

	for _, args := range []string{`{"findings":null}`, `{}`} {
		got, err := parseAnalysis(json.RawMessage(args))
		require.NoError(t, err, args)
		assert.NotNil(t, got.Findings, args)
		assert.Empty(t, got.Findings, args)
	}

	for name, args := range map[string]string{
		"unknown kind": `{"findings":[{"kind":"lesson","text":"x"}]}`,
		"empty text":   `{"findings":[{"kind":"code","text":" "}]}`,
		"not json":     `nope`,
	} {
		_, err := parseAnalysis(json.RawMessage(args))
		assert.Error(t, err, name)
	}
}

func TestAnalysisToolSchemaMirrorsTheResult(t *testing.T) {
	var schema struct {
		Properties struct {
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

	raw := analysisTool{}.Schema().Function.Parameters
	require.NoError(t, json.Unmarshal(raw, &schema))

	var top struct{ Properties map[string]json.RawMessage }
	require.NoError(t, json.Unmarshal(raw, &top))

	assert.ElementsMatch(t, []string{"findings"}, keys(top.Properties))
	assert.ElementsMatch(t, []string{"findings"}, schema.Required)
	assert.ElementsMatch(t, []string{
		protocol.FindingGuidance, protocol.FindingCode, protocol.FindingCardAdvice,
		protocol.FindingProjectVerify, protocol.FindingHardCard, protocol.FindingEcosystem,
	}, schema.Properties.Findings.Items.Properties.Kind.Enum)

	tool := analysisTool{}
	term, ok := any(tool).(tools.Terminal)
	require.True(t, ok)
	assert.True(t, term.Terminal())

	_, err := tool.Execute(t.Context(), map[string]any{"findings": []any{}})
	require.NoError(t, err)

	_, err = tool.Execute(t.Context(), map[string]any{"findings": []any{map[string]any{"kind": "lesson", "text": "x"}}})
	require.Error(t, err, "a report ContextMatrix would reject is refused")
}

func TestAnalyseUsesReadOnlyToolsAndReports(t *testing.T) {
	in := protocol.AnalyseInput{RunID: "abc-012-x1", Card: "---\nid: ABC-012\n---\nbody\n"}
	spec := analyseSpec(t, bareRepo(t, nil), in)
	ws := clonedInvestigation(t, spec)

	ex := &fakeExecutor{res: reported(`{"findings":[]}`)}
	d := endpointDeps(ex)

	got, usage, err := analyse(t.Context(), ws, spec, in, d, discardEmitter())
	require.NoError(t, err)
	assert.NotNil(t, got.Findings)
	assert.Empty(t, got.Findings)

	require.Len(t, usage, 1)
	assert.EqualValues(t, 100, usage[0].InputTokens)

	require.Equal(t, 1, ex.calls)

	reg := ex.req.Registry
	require.NotNil(t, reg)

	var names []string
	for _, tool := range reg.All() {
		names = append(names, tool.Name())
	}

	assert.ElementsMatch(t, []string{"read", "grep", "glob", "git", "report_analysis"}, names)

	roots := resolvedDirs(t, spec.InputsDir, filepath.Dir(spec.RunLogPath))

	read, _ := reg.Get("read")
	assert.ElementsMatch(t, roots, read.(tools.ReadTool).ReadRoots().Effective)

	assert.Equal(t, 40, ex.req.Config.MaxTurns)
	assert.Equal(t, 5, ex.req.Config.WrapUpTurns)
	assert.Equal(t, ws, ex.req.Dir)
	assert.Contains(t, ex.req.Task, spec.RunLogPath)
}

func TestAnalyseWithoutAReportFails(t *testing.T) {
	in := protocol.AnalyseInput{Card: "card\n"}
	spec := analyseSpec(t, bareRepo(t, nil), in)
	ws := clonedInvestigation(t, spec)

	ex := &fakeExecutor{res: harness.Result{Completed: true, Reason: "done", PromptTokens: 100}}

	_, usage, err := analyse(t.Context(), ws, spec, in, endpointDeps(ex), discardEmitter())
	require.ErrorIs(t, err, errNoAnalysis)
	require.Len(t, usage, 1)
	assert.EqualValues(t, 100, usage[0].InputTokens)
}

func TestAnalyseWithoutModelAccessFails(t *testing.T) {
	in := protocol.AnalyseInput{Card: "card\n"}
	spec := analyseSpec(t, bareRepo(t, nil), in)
	ws := clonedInvestigation(t, spec)

	_, usage, err := analyse(t.Context(), ws, spec, in, modelDeps{}, discardEmitter())
	require.ErrorIs(t, err, errNoModelAccess)
	assert.Empty(t, usage)
}

func TestAnalyseMapsHarnessStops(t *testing.T) {
	repoURL := bareRepo(t, nil)

	tests := []struct {
		reason string
		want   string
	}{
		{"max_turns", protocol.StopCauseMaxTurns},
		{"context_limit", protocol.StopCauseContextLimit},
		{"max_cost", protocol.StopCauseBudgetExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			spec := analyseSpec(t, repoURL, protocol.AnalyseInput{Card: "card\n"})
			ex := &fakeExecutor{res: harness.Result{Reason: tt.reason, PromptTokens: 5}}

			d := run(t.Context(), spec, slog.Default(), redact.New(nil), discardEmitter(), deps{model: built(endpointDeps(ex))})
			assert.Equal(t, tt.want, d.Cause)
			assert.Nil(t, d.Result)
			assert.Len(t, d.Usage, 1)
		})
	}
}

func TestAnalyseWritesNoSiblings(t *testing.T) {
	in := protocol.AnalyseInput{Card: "---\nid: ABC-012\n---\nbody\n", Subtasks: []string{"---\nid: ABC-013\n---\nsub\n"}}
	spec := analyseSpec(t, bareRepo(t, nil), in)
	ws := clonedInvestigation(t, spec)

	ex := &fakeExecutor{res: reported(`{"findings":[]}`)}

	_, _, err := analyse(t.Context(), ws, spec, in, endpointDeps(ex), discardEmitter())
	require.NoError(t, err)

	assert.FileExists(t, filepath.Join(spec.InputsDir, "card.md"))
	assert.FileExists(t, filepath.Join(spec.InputsDir, "subtasks", "ABC-013.md"))
	assert.NoDirExists(t, filepath.Join(spec.InputsDir, "siblings"))
}

func TestAnalyseFetchesTheBranches(t *testing.T) {
	in := protocol.AnalyseInput{Card: "card\n"}
	repoURL := bareRepo(t, nil)
	spec := analyseSpec(t, repoURL, in)
	ws := clonedInvestigation(t, spec)

	remote := strings.TrimPrefix(repoURL, "file://")
	gitCmd(t, remote, "branch", "cm/abc-012", "main")
	gitCmd(t, remote, "branch", "cm/sentinel/abc", "main")

	ex := &fakeExecutor{res: reported(`{"findings":[]}`)}

	_, _, err := analyse(t.Context(), ws, spec, in, endpointDeps(ex), discardEmitter())
	require.NoError(t, err)

	gitCmd(t, ws, "rev-parse", "--verify", "refs/remotes/origin/cm/abc-012")
	gitCmd(t, ws, "rev-parse", "--verify", "refs/remotes/origin/cm/sentinel/abc")
	assert.Contains(t, ex.req.Task, "cm/abc-012")
	assert.Contains(t, ex.req.Task, "cm/sentinel/abc")
}

func TestFetchGuidanceBranch(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		repoURL := bareRepo(t, nil)
		spec := analyseSpec(t, repoURL, protocol.AnalyseInput{Card: "card\n"})
		spec.Project = "alpha"
		ws := clonedInvestigation(t, spec)

		gitCmd(t, strings.TrimPrefix(repoURL, "file://"), "branch", "cm/sentinel/alpha", "main")

		got, err := fetchGuidanceBranch(t.Context(), spec, ws)
		require.NoError(t, err)
		assert.Equal(t, "cm/sentinel/alpha", got)
		gitCmd(t, ws, "rev-parse", "--verify", "refs/remotes/origin/cm/sentinel/alpha")
	})

	t.Run("absent", func(t *testing.T) {
		spec := analyseSpec(t, bareRepo(t, nil), protocol.AnalyseInput{Card: "card\n"})
		ws := clonedInvestigation(t, spec)

		got, err := fetchGuidanceBranch(t.Context(), spec, ws)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestRunAnalyseEndToEnd(t *testing.T) {
	const token = "ghs_analysesecret123"

	spec := analyseSpec(t, bareRepo(t, nil), protocol.AnalyseInput{Card: "card\n"})
	spec.GitToken = token

	args, err := json.Marshal(protocol.AnalyseResult{Findings: []protocol.FrictionFinding{
		{Kind: protocol.FindingGuidance, Text: "use " + token, Evidence: "e"},
		{Kind: protocol.FindingCode, Title: "t", Text: "x", Evidence: "e"},
	}})
	require.NoError(t, err)

	ex := &fakeExecutor{res: reported(string(args))}

	d := run(t.Context(), spec, slog.Default(), redact.New([]string{token}), discardEmitter(), deps{model: built(endpointDeps(ex))})
	require.Equal(t, protocol.StopCauseDone, d.Cause)
	require.NotNil(t, d.Result)
	require.NotNil(t, d.Result.Analyse)
	assert.Nil(t, d.Result.Investigate)
	require.Len(t, d.Result.Analyse.Findings, 2)
	assert.NotContains(t, d.Result.Analyse.Findings[0].Text, token)
	assert.Len(t, d.Usage, 1)
}
