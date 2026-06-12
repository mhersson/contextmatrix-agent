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
	g.SetBranchPolicy("cm/cmx-001", "main", "main")

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

func TestPushGuard(t *testing.T) {
	t.Parallel()

	g := NewGit(t.TempDir(), "tok")
	g.SetBranchPolicy("cm/test-001", "main", "main") // cardBranch, baseBranch, remoteDefault

	tests := []struct {
		name    string
		branch  string
		force   bool
		wantErr string
	}{
		{"force to own branch allowed", "cm/test-001", true, ""}, // fails later at network, not at guard
		{"plain push to own branch allowed", "cm/test-001", false, ""},
		{"force to main refused", "main", true, "refusing"},
		{"force to master refused", "master", true, "refusing"},
		{"force to other cm branch refused", "cm/other-002", true, "refusing"},
		{"plain push to main refused", "main", false, "refusing"},
		{"plain push to other branch refused", "feature/x", false, "refusing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := g.guardPush(tt.branch, tt.force)
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

// TestPushGuardDenylistRegardless pins the "denylist regardless" rule: even
// when the policy itself names a protected ref as the card branch, a force
// push to it is still refused. Card-ID validation makes this state
// unconstructable from env, but the guard must not depend on call order or on
// who set the policy.
func TestPushGuardDenylistRegardless(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                                  string
		cardBranch, baseBranch, remoteDefault string
		wantErr                               string
	}{
		{"card branch is main", "main", "main", "main", "refusing"},
		{"card branch is master, policy names neither", "master", "dev", "develop", "refusing"},
		{"card branch is base branch", "release/1.0", "release/1.0", "main", "refusing"},
		{"card branch outside cm namespace", "feature/x", "dev", "develop", "refusing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			g := NewGit(t.TempDir(), "tok")
			g.SetBranchPolicy(tt.cardBranch, tt.baseBranch, tt.remoteDefault)

			err := g.guardPush(tt.cardBranch, true)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestForcePushRequiresLeaseTip pins that force-with-lease without an explicit
// expected tip is rejected BEFORE git is ever invoked. The temp dir is not a
// repo, so a real git push would fail with a different (git-level) message; the
// exact lease error proves the guard fired first.
func TestForcePushRequiresLeaseTip(t *testing.T) {
	t.Parallel()

	g := NewGit(t.TempDir(), "tok")
	g.SetBranchPolicy("cm/test-001", "main", "main")

	err := g.ForcePushWithLease(context.Background(), "cm/test-001", "")
	require.Error(t, err)
	assert.EqualError(t, err, "force-with-lease: expected tip required")
}

// TestGuardZeroValueFailClosed pins the fail-closed posture: with no branch
// policy set (zero-value Git), EVERY push must refuse — including a push to a
// cm/ branch that would otherwise be legal.
func TestGuardZeroValueFailClosed(t *testing.T) {
	t.Parallel()

	g := NewGit(t.TempDir(), "tok") // SetBranchPolicy never called

	for _, branch := range []string{"cm/test-001", "main", "master", "feature/x", ""} {
		for _, force := range []bool{false, true} {
			err := g.guardPush(branch, force)
			require.Error(t, err, "branch=%q force=%v must refuse when policy unset", branch, force)
		}
	}
}
