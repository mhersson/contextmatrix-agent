package worker

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitEnv returns a minimal env for git subprocesses used in test setup.
// It includes identity variables so tests don't depend on the host git config.
func gitEnv() []string {
	path := os.Getenv("PATH")

	return []string{
		"PATH=" + path,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	}
}

// runGit runs a git command in dir with the test env.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", args[0], out)
}

// setupBareRemote creates a bare git repo seeded with a README on branch main.
// It returns the path to the bare repo (suitable as a file:// remote URL).
func setupBareRemote(t *testing.T) string {
	t.Helper()

	bare := t.TempDir()
	scratch := t.TempDir()

	runGit(t, bare, "init", "--bare", "-b", "main", bare)

	runGit(t, scratch, "init", "-b", "main", scratch)
	runGit(t, scratch, "config", "user.email", "test@example.com")
	runGit(t, scratch, "config", "user.name", "test")

	require.NoError(t, os.WriteFile(filepath.Join(scratch, "README.md"), []byte("seed\n"), 0o644))

	runGit(t, scratch, "add", "README.md")
	runGit(t, scratch, "commit", "-m", "init")
	runGit(t, scratch, "remote", "add", "origin", bare)
	runGit(t, scratch, "push", "-u", "origin", "main")

	return bare
}

func TestCloneBranchCommitPush(t *testing.T) {
	t.Parallel()

	remote := setupBareRemote(t)
	ws := filepath.Join(t.TempDir(), "ws")

	g := NewGit(ws, "") // no token: file:// remote needs none

	ctx := context.Background()

	require.NoError(t, g.Clone(ctx, remote, "main"))
	require.NoError(t, g.CreateBranch(ctx, "cm/cmx-001"))

	require.NoError(t, os.WriteFile(filepath.Join(ws, "new.txt"), []byte("x\n"), 0o644))

	dirty, err := g.CommitIfDirty(ctx, "Fix the thing", "CMX-001")
	require.NoError(t, err)
	assert.True(t, dirty)

	require.NoError(t, g.Push(ctx, "cm/cmx-001"))

	// Verify the bare remote now has the branch.
	cmd := exec.Command("git", "branch", "--list", "cm/cmx-001")
	cmd.Dir = remote
	cmd.Env = gitEnv()

	out, err := cmd.CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(out), "cm/cmx-001")
}

func TestCommitIfDirtyCleanTree(t *testing.T) {
	t.Parallel()

	remote := setupBareRemote(t)
	ws := filepath.Join(t.TempDir(), "ws")

	g := NewGit(ws, "")

	ctx := context.Background()

	require.NoError(t, g.Clone(ctx, remote, "main"))
	require.NoError(t, g.CreateBranch(ctx, "cm/cmx-002"))

	dirty, err := g.CommitIfDirty(ctx, "No changes", "CMX-002")
	require.NoError(t, err)
	assert.False(t, dirty)
}

func TestCredEnvShape(t *testing.T) {
	// t.Setenv mutates the process env; cannot use t.Parallel.
	t.Setenv("OPENROUTER_API_KEY", "secret-openrouter-value")

	g := NewGit(t.TempDir(), "ghs_tok123456")
	env := g.credEnv()

	assert.Contains(t, env, "GIT_CONFIG_COUNT=1")
	assert.Contains(t, env, "GIT_CONFIG_KEY_0=http.extraheader")

	joined := strings.Join(env, "\n")

	assert.NotContains(t, joined, "ghs_tok123456")
	assert.Contains(t, joined, base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_tok123456")))
	assert.NotContains(t, joined, "OPENROUTER")
}

func TestCredEnvNoToken(t *testing.T) {
	t.Parallel()

	g := NewGit(t.TempDir(), "")
	env := g.credEnv()

	joined := strings.Join(env, "\n")

	assert.NotContains(t, joined, "GIT_CONFIG_COUNT")
	assert.NotContains(t, joined, "http.extraheader")
}
