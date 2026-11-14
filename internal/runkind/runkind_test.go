package runkind

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/redact"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCauseOf(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, protocol.StopCauseDone},
		{"budget", fmt.Errorf("model call: %w", &orchestrator.BudgetExceededError{}), protocol.StopCauseBudgetExceeded},
		{"usage limit", &orchestrator.UsageLimitError{}, protocol.StopCauseUsageLimit},
		{"turn cap", fmt.Errorf("run: %w", &orchestrator.MaxTurnsError{}), protocol.StopCauseMaxTurns},
		{"context limit", &orchestrator.ContextLimitError{}, protocol.StopCauseContextLimit},
		{"anything else", errors.New("x"), protocol.StopCauseError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, causeOf(tt.err))
		})
	}
}

// bareRepo makes a bare repo with one commit on main, holding a README and
// the given executable scripts, and returns its file URL.
func bareRepo(t *testing.T, scripts map[string]string) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	root := t.TempDir()
	src := filepath.Join(root, "src")
	bare := filepath.Join(root, "repo.git")

	gitIn := func(dir string, args ...string) {
		t.Helper()

		//nolint:gosec // G204: test-controlled arguments against temp repos
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = dir

		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}

	require.NoError(t, os.MkdirAll(src, 0o755))
	gitIn(src, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(src, "README.md"), []byte("hello\n"), 0o644))

	for name, body := range scripts {
		path := filepath.Join(src, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	}

	gitIn(src, "add", ".")
	gitIn(src, "commit", "-q", "-m", "init")
	gitIn(root, "clone", "-q", "--bare", src, bare)

	return "file://" + bare
}

func checkSpec(t *testing.T, repoURL, input string) Spec {
	t.Helper()

	return Spec{
		Kind:       protocol.RunKindCheck,
		CardID:     "ABC-012",
		Project:    "abc",
		RepoURL:    repoURL,
		BaseBranch: "main",
		Workspace:  filepath.Join(t.TempDir(), "workspace"),
		Input:      []byte(input),
	}
}

func TestRunCheckAgainstALocalRepo(t *testing.T) {
	spec := checkSpec(t, bareRepo(t, nil), `{}`)
	// Claude Code without a token: a check never builds model access.
	spec.LLMType = protocol.LLMEndpointTypeClaudeCode

	d := Run(context.Background(), spec, nil, discardEmitter())
	require.Equal(t, protocol.StopCauseDone, d.Cause)
	require.NotNil(t, d.Result)
	require.NotNil(t, d.Result.Check)
	assert.NotNil(t, d.Result.Check.Candidates)
	assert.Nil(t, d.Result.Investigate)
	assert.DirExists(t, filepath.Join(spec.Workspace, "abc-012", ".git"), "the base branch is cloned under the card's lower-case id")
}

func TestRunEndsInError(t *testing.T) {
	repoURL := bareRepo(t, nil)

	checking := func(t *testing.T) Spec { return checkSpec(t, repoURL, `{}`) }
	investigating := func(t *testing.T) Spec {
		return investigateSpec(t, repoURL, protocol.InvestigateInput{Card: "card\n"})
	}

	tests := []struct {
		name string
		spec func(*testing.T) Spec
		edit func(*Spec)
	}{
		{"missing repo", checking, func(s *Spec) { s.RepoURL = "file://" + filepath.Join(t.TempDir(), "nope.git") }},
		{"missing branch", checking, func(s *Spec) { s.BaseBranch = "no-such-branch" }},
		{"bad input", checking, func(s *Spec) { s.Input = []byte(`{"verify":`) }},
		{"unknown kind", checking, func(s *Spec) { s.Kind = "unknown" }},
		{"claude code without a token", investigating, func(s *Spec) { s.LLMType = protocol.LLMEndpointTypeClaudeCode }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := tt.spec(t)
			tt.edit(&spec)

			d := Run(context.Background(), spec, nil, discardEmitter())
			assert.Equal(t, protocol.StopCauseError, d.Cause)
			assert.Nil(t, d.Result)
		})
	}
}

func TestRunMasksTheResult(t *testing.T) {
	const token = "ghs_secrettoken123"

	spec := checkSpec(t, bareRepo(t, nil), `{}`)
	spec.GitToken = token

	cands := []orchestrator.VerifyCandidate{{Argv: []string{"./run", token}, Display: "./run " + token}}

	red := redact.New([]string{spec.GitToken})
	d := run(context.Background(), spec, slog.Default(), red, discardEmitter(), deps{check: fakeDeps(cands, &trialFake{})})
	require.Equal(t, protocol.StopCauseDone, d.Cause)
	require.NotNil(t, d.Result)
	require.Len(t, d.Result.Check.Candidates, 1)
	assert.NotContains(t, d.Result.Check.Candidates[0], token)
}

func TestRunMasksBeforeItLimits(t *testing.T) {
	// A candidate that is over the limit only because of the secret it holds is
	// kept: masking shortens it before the limit applies.
	token := "ghs_" + strings.Repeat("x9", 300)

	spec := checkSpec(t, bareRepo(t, nil), `{}`)
	spec.GitToken = token

	cands := []orchestrator.VerifyCandidate{{Argv: []string{"./run", token}, Display: "./run " + token}}

	d := run(context.Background(), spec, slog.Default(), redact.New([]string{token}), discardEmitter(), deps{check: fakeDeps(cands, &trialFake{})})
	require.Equal(t, protocol.StopCauseDone, d.Cause)
	require.Len(t, d.Result.Check.Candidates, 1)
	assert.NotContains(t, d.Result.Check.Candidates[0], token)
}

func TestRunTrialsThroughTheProductionDeps(t *testing.T) {
	const token = "ghs_trialsecret123"

	repoURL := bareRepo(t, map[string]string{"ci/verify": "#!/bin/sh\necho \"token $1\"\n"})

	cmd := "./ci/verify " + token
	input, err := json.Marshal(protocol.CheckInput{Verify: &protocol.VerifyConfig{Command: cmd}, CurrentCommand: cmd})
	require.NoError(t, err)

	spec := checkSpec(t, repoURL, string(input))
	spec.GitToken = token

	var transcript bytes.Buffer

	d := Run(context.Background(), spec, nil, events.NewEmitter(io.Discard, &transcript))
	require.Equal(t, protocol.StopCauseDone, d.Cause)
	require.NotNil(t, d.Result)
	assert.Equal(t, protocol.TrialFinished, d.Result.Check.Trial)

	result, err := json.Marshal(d.Result)
	require.NoError(t, err)
	assert.NotContains(t, string(result), token)

	var ev struct {
		Kind string            `json:"kind"`
		Data map[string]string `json:"data"`
	}

	require.NoError(t, json.Unmarshal(bytes.TrimSpace(transcript.Bytes()), &ev))
	require.Equal(t, string(trialKind), ev.Kind)
	assert.Contains(t, ev.Data["output"], "token ", "the script ran")
	assert.NotContains(t, ev.Data["output"], token)
	assert.Contains(t, ev.Data["command"], "./ci/verify")
	assert.NotContains(t, ev.Data["command"], token)
}

func TestWorkspaceDir(t *testing.T) {
	assert.Equal(t, "/w/alpha-001", workspaceDir(Spec{Workspace: "/w", CardID: "ALPHA-001"}))
	assert.Equal(t, "/w/guidance_pr", workspaceDir(Spec{Workspace: "/w", Kind: protocol.RunKindGuidancePR}))
}

func TestRunMasksModelResultsWithOperatorValues(t *testing.T) {
	const (
		token = "ghs_operatortoken123"
		long  = "op-secret-1"
		short = "short77"
	)

	text := "a " + long + " b " + short + " c " + token

	assertMasked := func(t *testing.T, s string) {
		t.Helper()
		assert.NotContains(t, s, long)
		assert.NotContains(t, s, token)
		assert.Contains(t, s, "[REDACTED]")
		assert.Contains(t, s, short, "a value under 8 bytes is not masked")
	}

	t.Run("investigate through run", func(t *testing.T) {
		spec := investigateSpec(t, bareRepo(t, nil), protocol.InvestigateInput{Card: "card\n"})
		spec.GitToken = token
		spec.OperatorValues = []string{long, short}

		args, err := json.Marshal(protocol.InvestigateResult{
			Diagnosis: text,
			Cause:     protocol.DiagnosisCard,
			Fix:       &protocol.CardFix{Section: text},
			Findings:  []protocol.FrictionFinding{{Kind: protocol.FindingCode, Title: "t", Text: text}},
		})
		require.NoError(t, err)

		ex := &fakeExecutor{res: reported(string(args))}
		d := run(t.Context(), spec, slog.Default(), redact.New([]string{token}), discardEmitter(), deps{model: built(endpointDeps(ex))})
		require.Equal(t, protocol.StopCauseDone, d.Cause)

		inv := d.Result.Investigate
		assertMasked(t, inv.Diagnosis)
		assertMasked(t, inv.Fix.Section)
		assertMasked(t, inv.Findings[0].Text)
	})

	t.Run("analyse through run", func(t *testing.T) {
		spec := analyseSpec(t, bareRepo(t, nil), protocol.AnalyseInput{Card: "card\n"})
		spec.GitToken = token
		spec.OperatorValues = []string{long, short}

		args, err := json.Marshal(protocol.AnalyseResult{
			Findings: []protocol.FrictionFinding{{Kind: protocol.FindingGuidance, Text: text, Evidence: "e"}},
		})
		require.NoError(t, err)

		ex := &fakeExecutor{res: reported(string(args))}
		d := run(t.Context(), spec, slog.Default(), redact.New([]string{token}), discardEmitter(), deps{model: built(endpointDeps(ex))})
		require.Equal(t, protocol.StopCauseDone, d.Cause)

		assertMasked(t, d.Result.Analyse.Findings[0].Text)
	})
}

func TestRunKeepsOperatorValuesInCheckResults(t *testing.T) {
	const value = "op-secret-1"

	spec := checkSpec(t, bareRepo(t, nil), `{}`)
	spec.OperatorValues = []string{value}

	cands := []orchestrator.VerifyCandidate{{Argv: []string{"./run", value}, Display: "./run " + value}}

	d := run(context.Background(), spec, slog.Default(), redact.New(nil), discardEmitter(), deps{check: fakeDeps(cands, &trialFake{})})
	require.Equal(t, protocol.StopCauseDone, d.Cause)
	require.Equal(t, []string{"./run " + value}, d.Result.Check.Candidates)
}
