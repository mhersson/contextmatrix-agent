package runkind

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTextTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"a url and its host, trailing dot trimmed", "See https://docs.example.org/x.", []string{"https://docs.example.org/x", "docs.example.org"}},
		{"an inline span", "Run `./run lint` first.", []string{"./run lint"}},
		{"a fenced block", "```\n$ ./ci/verify --fast\n\n./run test\n```", []string{"./ci/verify --fast", "./run test"}},
		{"a version and a file name are not hosts", "bump to v1.2.3 in main.go", nil},
		{"a bare host", "call api.internal.example directly", []string{"api.internal.example"}},
		{"duplicates collapse", "`./x` and `./x` again", []string{"./x"}},
		{"a url in a span", "use `https://a.example.com/p` now", []string{"https://a.example.com/p", "a.example.com"}},
		{"first-seen order across kinds", "run `./run lint` against api.example.com then https://b.example.org", []string{"./run lint", "api.example.com", "https://b.example.org", "b.example.org"}},
		{"a host with a port, a path or a user", "api.example.com:8080 and api.example.com/x and git@git.example.com:org/repo", []string{"api.example.com", "git.example.com"}},
		{"a double-backtick span and a tilde fence", "``./a b`` then\n~~~\n./c\n~~~", []string{"./a b", "./c"}},
		{"hosts dedupe case-insensitively", "Api.Example.com and api.example.com", []string{"Api.Example.com"}},
		{"a markdown link", "[x](https://a.example.org/p)", []string{"https://a.example.org/p", "a.example.org"}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := textTokens(tt.in)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAbsentFromRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	ws := t.TempDir()
	gitIn := func(args ...string) {
		t.Helper()

		//nolint:gosec // G204: test-controlled arguments against a temp repo
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = ws

		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}

	gitIn("init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(ws, "README.md"), []byte("see https://ok.example.com\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "scripts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "scripts", "build.sh"), []byte("x\n"), 0o755))
	gitIn("add", ".")
	gitIn("commit", "-q", "-m", "init")

	tokens := []string{"https://ok.example.com", "https://new.example.net", "scripts/build.sh", "./x --y", "OK.EXAMPLE.COM", "--help"}

	got, err := absentFromRepo(context.Background(), ws, tokens, []string{"./x  --y"})
	require.NoError(t, err)
	assert.Equal(t, []string{"https://new.example.net", "--help"}, got)
}

func TestCheckFillsAbsentFromRepo(t *testing.T) {
	calls := 0
	d := fakeDeps(nil, &trialFake{})
	d.absent = func(_ context.Context, _ string, tokens, declared []string) ([]string, error) {
		calls++

		assert.Contains(t, tokens, "./run lint")
		assert.Equal(t, []string{"./ci/verify", "./run test"}, declared)

		return []string{"https://new.example.net"}, nil
	}
	d.candidates = func(string) []orchestrator.VerifyCandidate {
		return []orchestrator.VerifyCandidate{{Display: "./run test"}}
	}

	in := protocol.CheckInput{Text: "run `./run lint` at https://new.example.net", CurrentCommand: "./ci/verify"}

	res, err := check(context.Background(), t.TempDir(), in, d, discardEmitter())
	require.NoError(t, err)
	assert.Equal(t, []string{"https://new.example.net"}, res.AbsentFromRepo)

	res, err = check(context.Background(), t.TempDir(), protocol.CheckInput{}, d, discardEmitter())
	require.NoError(t, err)
	assert.Nil(t, res.AbsentFromRepo)
	assert.Equal(t, 1, calls)
}
