package runkind

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
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
		{"anything else", errors.New("x"), protocol.StopCauseError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, causeOf(tt.err))
		})
	}
}

// bareRepo makes a bare repo with one commit on main and returns its file URL.
func bareRepo(t *testing.T) string {
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
	gitIn(src, "add", "README.md")
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
	spec := checkSpec(t, bareRepo(t), `{}`)

	d := Run(context.Background(), spec, nil, discardEmitter())
	require.Equal(t, protocol.StopCauseDone, d.Cause)
	require.NotNil(t, d.Result)
	require.NotNil(t, d.Result.Check)
	assert.NotNil(t, d.Result.Check.Candidates)
	assert.Nil(t, d.Result.Investigate)
	assert.DirExists(t, filepath.Join(spec.Workspace, "abc-012", ".git"), "the base branch is cloned under the card's lower-case id")
}

func TestRunEndsInError(t *testing.T) {
	repoURL := bareRepo(t)

	tests := []struct {
		name string
		edit func(*Spec)
	}{
		{"missing repo", func(s *Spec) { s.RepoURL = "file://" + filepath.Join(t.TempDir(), "nope.git") }},
		{"missing branch", func(s *Spec) { s.BaseBranch = "no-such-branch" }},
		{"bad input", func(s *Spec) { s.Input = []byte(`{"verify":`) }},
		{"unknown kind", func(s *Spec) { s.Kind = "unknown" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := checkSpec(t, repoURL, `{}`)
			tt.edit(&spec)

			d := Run(context.Background(), spec, nil, discardEmitter())
			assert.Equal(t, protocol.StopCauseError, d.Cause)
			assert.Nil(t, d.Result)
		})
	}
}

func TestRunMasksTheResult(t *testing.T) {
	const token = "ghs_secrettoken123"

	spec := checkSpec(t, bareRepo(t), `{}`)
	spec.GitToken = token

	cands := []orchestrator.VerifyCandidate{{Argv: []string{"./run", token}, Display: "./run " + token}}

	red := redact.New([]string{spec.GitToken})
	d := run(context.Background(), spec, slog.Default(), red, discardEmitter(), fakeDeps(cands, &trialFake{}))
	require.Equal(t, protocol.StopCauseDone, d.Cause)
	require.NotNil(t, d.Result)
	require.Len(t, d.Result.Check.Candidates, 1)
	assert.NotContains(t, d.Result.Check.Candidates[0], token)
}

func TestRunMasksBeforeItLimits(t *testing.T) {
	// A candidate that is over the limit only because of the secret it holds is
	// kept: masking shortens it before the limit applies.
	token := "ghs_" + strings.Repeat("x9", 300)

	spec := checkSpec(t, bareRepo(t), `{}`)
	spec.GitToken = token

	cands := []orchestrator.VerifyCandidate{{Argv: []string{"./run", token}, Display: "./run " + token}}

	d := run(context.Background(), spec, slog.Default(), redact.New([]string{token}), discardEmitter(), fakeDeps(cands, &trialFake{}))
	require.Equal(t, protocol.StopCauseDone, d.Cause)
	require.Len(t, d.Result.Check.Candidates, 1)
	assert.NotContains(t, d.Result.Check.Candidates[0], token)
}
