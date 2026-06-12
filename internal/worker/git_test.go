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

// setupClonedRepo creates a bare remote with an initial commit on main and a
// cloned workspace. It returns (bareDir, cloneDir, initialCommitHash).
func setupClonedRepo(t *testing.T) (string, string, string) {
	t.Helper()

	bare := setupBareRemote(t)
	ws := filepath.Join(t.TempDir(), "ws")

	g := NewGit(ws, "")
	require.NoError(t, g.Clone(context.Background(), bare, "main"))

	// Get the initial commit hash.
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = ws
	cmd.Env = gitEnv()

	out, err := cmd.CombinedOutput()
	require.NoError(t, err)

	return bare, ws, strings.TrimSpace(string(out))
}

// pushFileToBranch commits filename in a fresh clone of bare and pushes the
// result to branch on the remote — simulating work that lands after our clone.
func pushFileToBranch(t *testing.T, bare, filename, branch string) {
	t.Helper()

	scratch := t.TempDir()
	runGit(t, scratch, "clone", bare, scratch)
	runGit(t, scratch, "config", "user.email", "test@example.com")
	runGit(t, scratch, "config", "user.name", "test")

	require.NoError(t, os.WriteFile(filepath.Join(scratch, filename), []byte(filename+"\n"), 0o644))

	runGit(t, scratch, "add", filename)
	runGit(t, scratch, "commit", "-m", "add "+filename)
	runGit(t, scratch, "push", "origin", "HEAD:refs/heads/"+branch)
}

func TestRemoteTip(t *testing.T) {
	t.Parallel()

	bare, ws, _ := setupClonedRepo(t)
	g := NewGit(ws, "")
	ctx := context.Background()

	// Existing branch returns a non-empty hash.
	tip, err := g.RemoteTip(ctx, "main")
	require.NoError(t, err)
	assert.NotEmpty(t, tip)
	assert.Len(t, tip, 40)

	// Missing branch returns ("", nil).
	tip, err = g.RemoteTip(ctx, "does-not-exist")
	require.NoError(t, err)
	assert.Empty(t, tip)

	_ = bare
}

func TestFetch(t *testing.T) {
	t.Parallel()

	bare, ws, _ := setupClonedRepo(t)
	ctx := context.Background()

	// Create a new branch on the remote after the clone.
	pushFileToBranch(t, bare, "extra.txt", "feature/new")

	g := NewGit(ws, "")

	// Before fetch the branch is unknown to our clone.
	_, err := g.run(ctx, "rev-parse", "--verify", "origin/feature/new")
	require.Error(t, err)

	require.NoError(t, g.Fetch(ctx, "feature/new"))

	// After fetch the remote ref exists.
	out, err := g.run(ctx, "rev-parse", "--verify", "origin/feature/new")
	require.NoError(t, err)
	assert.NotEmpty(t, strings.TrimSpace(out))
}

func TestMergeBase(t *testing.T) {
	t.Parallel()

	_, ws, initHash := setupClonedRepo(t)
	g := NewGit(ws, "")
	ctx := context.Background()

	// Create a branch off main and add a commit.
	runGit(t, ws, "checkout", "-b", "cm/test")
	require.NoError(t, os.WriteFile(filepath.Join(ws, "branch.txt"), []byte("b\n"), 0o644))
	runGit(t, ws, "add", "branch.txt")
	runGit(t, ws, "commit", "-m", "branch commit")

	base, err := g.MergeBase(ctx, "main", "cm/test")
	require.NoError(t, err)
	assert.Equal(t, initHash, base)
}

func TestCommitFixup(t *testing.T) {
	t.Parallel()

	_, ws, _ := setupClonedRepo(t)
	g := NewGit(ws, "")
	ctx := context.Background()

	// Make an initial commit to target with fixup.
	require.NoError(t, os.WriteFile(filepath.Join(ws, "foo.txt"), []byte("original\n"), 0o644))
	runGit(t, ws, "add", "foo.txt")
	runGit(t, ws, "commit", "-m", "add foo")

	// Get the hash of that commit.
	out, err := g.run(ctx, "rev-parse", "HEAD")
	require.NoError(t, err)

	target := strings.TrimSpace(out)

	// Make a change to fixup, plus a brand-new untracked file — fix runs can
	// legitimately create files (e.g. a missing test).
	require.NoError(t, os.WriteFile(filepath.Join(ws, "foo.txt"), []byte("fixed\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "foo_test.txt"), []byte("new test\n"), 0o644))

	dirty, err := g.CommitFixup(ctx, target)
	require.NoError(t, err)
	assert.True(t, dirty)

	// Fixup commit subject should be "fixup! add foo".
	out, err = g.run(ctx, "log", "--format=%s", "-1")
	require.NoError(t, err)
	assert.Equal(t, "fixup! add foo", strings.TrimSpace(out))

	// The untracked file must be part of the fixup commit.
	out, err = g.run(ctx, "show", "--format=", "--name-only", "HEAD")
	require.NoError(t, err)
	assert.Contains(t, out, "foo_test.txt")

	// Nothing left behind in the working tree.
	out, err = g.run(ctx, "status", "--porcelain")
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(out))
}

func TestCommitFixupCleanTree(t *testing.T) {
	t.Parallel()

	_, ws, _ := setupClonedRepo(t)
	g := NewGit(ws, "")
	ctx := context.Background()

	// No changes; fixup on HEAD should be a no-op.
	out, err := g.run(ctx, "rev-parse", "HEAD")
	require.NoError(t, err)

	target := strings.TrimSpace(out)

	dirty, err := g.CommitFixup(ctx, target)
	require.NoError(t, err)
	assert.False(t, dirty)
}

func TestRebaseAutosquash(t *testing.T) {
	t.Parallel()

	t.Run("squashes fixup commit", func(t *testing.T) {
		t.Parallel()

		_, ws, _ := setupClonedRepo(t)
		g := NewGit(ws, "")
		ctx := context.Background()

		// Capture the starting point (origin/main) before adding commits.
		ontoOut, err := g.run(ctx, "rev-parse", "origin/main")
		require.NoError(t, err)

		onto := strings.TrimSpace(ontoOut)

		// Create a commit to squash into.
		require.NoError(t, os.WriteFile(filepath.Join(ws, "foo.txt"), []byte("original\n"), 0o644))
		runGit(t, ws, "add", "foo.txt")
		runGit(t, ws, "commit", "-m", "add foo")

		targetOut, err := g.run(ctx, "rev-parse", "HEAD")
		require.NoError(t, err)

		target := strings.TrimSpace(targetOut)

		// Create fixup commit.
		require.NoError(t, os.WriteFile(filepath.Join(ws, "foo.txt"), []byte("fixed\n"), 0o644))

		_, err = g.CommitFixup(ctx, target)
		require.NoError(t, err)

		// Two commits on top of onto before rebase.
		out, err := g.run(ctx, "rev-list", onto+"..HEAD")
		require.NoError(t, err)
		assert.Len(t, strings.Fields(strings.TrimSpace(out)), 2)

		require.NoError(t, g.RebaseAutosquash(ctx, onto))

		// After autosquash rebase, exactly one commit on top of onto.
		out, err = g.run(ctx, "rev-list", onto+"..HEAD")
		require.NoError(t, err)
		assert.Len(t, strings.Fields(strings.TrimSpace(out)), 1)

		// Content should be the fixup version.
		content, err := os.ReadFile(filepath.Join(ws, "foo.txt"))
		require.NoError(t, err)
		assert.Equal(t, "fixed\n", string(content))
	})

	t.Run("conflict aborts and returns ErrRebaseConflict", func(t *testing.T) {
		t.Parallel()

		bare, ws, _ := setupClonedRepo(t)
		g := NewGit(ws, "")
		ctx := context.Background()

		// Create a commit on our branch.
		require.NoError(t, os.WriteFile(filepath.Join(ws, "conflict.txt"), []byte("ours\n"), 0o644))
		runGit(t, ws, "add", "conflict.txt")
		runGit(t, ws, "commit", "-m", "our change")

		// Create a diverged base: add conflicting content to the remote.
		scratch := t.TempDir()
		runGit(t, scratch, "clone", bare, scratch)
		runGit(t, scratch, "config", "user.email", "test@example.com")
		runGit(t, scratch, "config", "user.name", "test")
		require.NoError(t, os.WriteFile(filepath.Join(scratch, "conflict.txt"), []byte("theirs\n"), 0o644))
		runGit(t, scratch, "add", "conflict.txt")
		runGit(t, scratch, "commit", "-m", "their conflicting change")
		runGit(t, scratch, "push", "origin", "HEAD:main")

		// Fetch the new tip as our onto target.
		require.NoError(t, g.Fetch(ctx, "main"))
		ontoOut, err := g.run(ctx, "rev-parse", "origin/main")
		require.NoError(t, err)

		onto := strings.TrimSpace(ontoOut)

		err = g.RebaseAutosquash(ctx, onto)
		require.ErrorIs(t, err, ErrRebaseConflict)

		// Repo must be clean — no .git/rebase-merge leftover.
		_, statErr := os.Stat(filepath.Join(ws, ".git", "rebase-merge"))
		assert.True(t, os.IsNotExist(statErr), "rebase-merge dir should not exist after abort")
	})
}

func TestSoftResetSquash(t *testing.T) {
	t.Parallel()

	_, ws, _ := setupClonedRepo(t)
	g := NewGit(ws, "")
	ctx := context.Background()

	// Create two commits.
	require.NoError(t, os.WriteFile(filepath.Join(ws, "a.txt"), []byte("a\n"), 0o644))
	runGit(t, ws, "add", "a.txt")
	runGit(t, ws, "commit", "-m", "add a")

	require.NoError(t, os.WriteFile(filepath.Join(ws, "b.txt"), []byte("b\n"), 0o644))
	runGit(t, ws, "add", "b.txt")
	runGit(t, ws, "commit", "-m", "add b")

	// Record the tree hash at HEAD before squash.
	treeOut, err := g.run(ctx, "rev-parse", "HEAD^{tree}")
	require.NoError(t, err)

	preTree := strings.TrimSpace(treeOut)

	// Merge-base is origin/main (2 commits behind HEAD; we're on main locally).
	baseOut, err := g.run(ctx, "rev-parse", "origin/main")
	require.NoError(t, err)

	mergeBase := strings.TrimSpace(baseOut)

	require.NoError(t, g.SoftReset(ctx, mergeBase))

	// Everything should be staged (soft reset leaves index dirty).
	out, err := g.run(ctx, "status", "--porcelain")
	require.NoError(t, err)
	assert.NotEmpty(t, strings.TrimSpace(out))

	// Commit the squash.
	committed, err := g.CommitWithMessage(ctx, "squashed")
	require.NoError(t, err)
	assert.True(t, committed)

	// Only one commit on top of merge-base.
	listOut, err := g.run(ctx, "rev-list", mergeBase+"..HEAD")
	require.NoError(t, err)
	assert.Len(t, strings.Fields(strings.TrimSpace(listOut)), 1)

	// Tree hash must be identical to pre-squash HEAD.
	treeOut2, err := g.run(ctx, "rev-parse", "HEAD^{tree}")
	require.NoError(t, err)
	assert.Equal(t, preTree, strings.TrimSpace(treeOut2))
}

func TestCommitWithMessage(t *testing.T) {
	t.Parallel()

	_, ws, _ := setupClonedRepo(t)
	g := NewGit(ws, "")
	ctx := context.Background()

	require.NoError(t, os.WriteFile(filepath.Join(ws, "new.txt"), []byte("hello\n"), 0o644))

	committed, err := g.CommitWithMessage(ctx, "my exact message")
	require.NoError(t, err)
	assert.True(t, committed)

	out, err := g.run(ctx, "log", "--format=%s", "-1")
	require.NoError(t, err)
	assert.Equal(t, "my exact message", strings.TrimSpace(out))
}

func TestCommitWithMessageCleanTree(t *testing.T) {
	t.Parallel()

	_, ws, _ := setupClonedRepo(t)
	g := NewGit(ws, "")
	ctx := context.Background()

	committed, err := g.CommitWithMessage(ctx, "no changes")
	require.NoError(t, err)
	assert.False(t, committed)
}

func TestLastCommitTouching(t *testing.T) {
	t.Parallel()

	_, ws, _ := setupClonedRepo(t)
	g := NewGit(ws, "")
	ctx := context.Background()

	// Commit touching foo.txt.
	require.NoError(t, os.WriteFile(filepath.Join(ws, "foo.txt"), []byte("foo\n"), 0o644))
	runGit(t, ws, "add", "foo.txt")
	runGit(t, ws, "commit", "-m", "add foo")

	fooHash, err := g.run(ctx, "rev-parse", "HEAD")
	require.NoError(t, err)

	fooHash = strings.TrimSpace(fooHash)

	// Commit touching bar.txt.
	require.NoError(t, os.WriteFile(filepath.Join(ws, "bar.txt"), []byte("bar\n"), 0o644))
	runGit(t, ws, "add", "bar.txt")
	runGit(t, ws, "commit", "-m", "add bar")

	// Last commit touching foo.txt should be the first commit.
	hash, err := g.LastCommitTouching(ctx, []string{"foo.txt"})
	require.NoError(t, err)
	assert.Equal(t, fooHash, hash)

	// Last commit touching bar.txt should be HEAD.
	headOut, err := g.run(ctx, "rev-parse", "HEAD")
	require.NoError(t, err)

	head := strings.TrimSpace(headOut)

	hash, err = g.LastCommitTouching(ctx, []string{"bar.txt"})
	require.NoError(t, err)
	assert.Equal(t, head, hash)

	// No commits touch nonexistent path.
	hash, err = g.LastCommitTouching(ctx, []string{"nonexistent.txt"})
	require.NoError(t, err)
	assert.Empty(t, hash)
}

func TestHead(t *testing.T) {
	t.Parallel()

	_, ws, initHash := setupClonedRepo(t)
	g := NewGit(ws, "")
	ctx := context.Background()

	h, err := g.Head(ctx)
	require.NoError(t, err)
	assert.Equal(t, initHash, h)
}

func TestCheckout(t *testing.T) {
	t.Parallel()

	bare, ws, _ := setupClonedRepo(t)
	g := NewGit(ws, "")
	ctx := context.Background()

	// Create a branch on the remote.
	pushFileToBranch(t, bare, "feature.txt", "feature/resume")

	require.NoError(t, g.Fetch(ctx, "feature/resume"))
	require.NoError(t, g.Checkout(ctx, "feature/resume"))

	// Verify we're on the right branch.
	out, err := g.run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	require.NoError(t, err)
	assert.Equal(t, "feature/resume", strings.TrimSpace(out))
}

func TestDiff(t *testing.T) {
	t.Parallel()

	_, ws, _ := setupClonedRepo(t)
	g := NewGit(ws, "")
	ctx := context.Background()

	// Add a commit on main branch.
	require.NoError(t, os.WriteFile(filepath.Join(ws, "diff.txt"), []byte("added line\n"), 0o644))
	runGit(t, ws, "add", "diff.txt")
	runGit(t, ws, "commit", "-m", "add diff.txt")

	// Diff against origin/main (the merge-base...HEAD range).
	diff, err := g.Diff(ctx, "origin/main")
	require.NoError(t, err)
	assert.Contains(t, diff, "added line")
	assert.Contains(t, diff, "diff.txt")
}

func TestDiffNoChanges(t *testing.T) {
	t.Parallel()

	_, ws, _ := setupClonedRepo(t)
	g := NewGit(ws, "")
	ctx := context.Background()

	// No commits beyond origin/main, diff should be empty.
	diff, err := g.Diff(ctx, "origin/main")
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(diff))
}
