package worker

import (
	"context"
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

// writeFile writes content to dir/rel, creating parent dirs, with the given
// mode. Helper for staging tests.
func writeFile(t *testing.T, dir, rel string, content []byte, mode os.FileMode) {
	t.Helper()

	full := filepath.Join(dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, content, mode))
}

func TestStageForCommit_SkipsBuildArtifacts(t *testing.T) {
	remote := setupBareRemote(t)
	ws := t.TempDir()

	g := NewGit(ws, "", "", "")
	require.NoError(t, g.Clone(context.Background(), remote, "main"))
	require.NoError(t, g.CreateBranch(context.Background(), "cm/cmx-001"))
	g.SetBranchPolicy("cm/cmx-001", "main", "main")

	// Isolate our artifact guard from the developer's global gitignore: a
	// machine that globally ignores e.g. *.pyc would hide cache.pyc from git
	// entirely, so the test would not exercise our extension denylist.
	runGit(t, ws, "config", "core.excludesFile", "/dev/null")

	// A legitimate source edit (tracked: the seeded README).
	writeFile(t, ws, "README.md", []byte("# updated\n"), 0o644)
	// A new source file (untracked, must be staged).
	writeFile(t, ws, "main.go", []byte("package main\n"), 0o644)
	// A compiled ELF binary (untracked, must be skipped by magic).
	elf := append([]byte{0x7f, 'E', 'L', 'F'}, make([]byte, 64)...)
	writeFile(t, ws, "app", elf, 0o755)
	// Python bytecode by extension (untracked, must be skipped: no executable
	// magic, so the extension denylist is what catches it).
	writeFile(t, ws, "cache.pyc", []byte("not a real binary"), 0o644)
	// A golden-output fixture (untracked, MUST be staged): a ".out" data file
	// is legitimate source, not a build artifact. Real a.out binaries are
	// caught by magic, not by extension.
	writeFile(t, ws, "golden.out", []byte("expected output\n"), 0o644)

	committed, err := g.CommitWithMessage(context.Background(), "feat: real work")
	require.NoError(t, err)
	require.True(t, committed)

	// HEAD must contain the source files (incl. the golden fixture) but NOT the
	// compiled binary or the bytecode.
	out, err := g.run(context.Background(), "show", "--name-only", "--format=", "HEAD")
	require.NoError(t, err)
	assert.Contains(t, out, "README.md")
	assert.Contains(t, out, "main.go")
	assert.Contains(t, out, "golden.out")
	assert.NotContains(t, out, "app")
	assert.NotContains(t, out, "cache.pyc")

	// The skipped artifacts remain untracked, not lost.
	status, err := g.run(context.Background(), "status", "--porcelain")
	require.NoError(t, err)
	assert.Contains(t, status, "?? app")
	assert.Contains(t, status, "?? cache.pyc")
}

// A filename containing a space must still be staged. git status C-quotes such
// paths unless -z is used; the quoted form would fail `git add` and abort the
// whole commit. Regression guard for the -z parsing in stageForCommit (a
// blanket `git add -A` handled spaced names transparently).
func TestStageForCommit_HandlesSpacedFilenames(t *testing.T) {
	remote := setupBareRemote(t)
	ws := t.TempDir()

	g := NewGit(ws, "", "", "")
	require.NoError(t, g.Clone(context.Background(), remote, "main"))
	require.NoError(t, g.CreateBranch(context.Background(), "cm/cmx-001"))
	g.SetBranchPolicy("cm/cmx-001", "main", "main")

	writeFile(t, ws, "my fixture.go", []byte("package x\n"), 0o644)

	committed, err := g.CommitWithMessage(context.Background(), "feat: spaced filename")
	require.NoError(t, err)
	require.True(t, committed)

	out, err := g.run(context.Background(), "show", "--name-only", "--format=", "HEAD")
	require.NoError(t, err)
	assert.Contains(t, out, "my fixture.go")

	status, err := g.run(context.Background(), "status", "--porcelain")
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(status), "the spaced file must be committed, leaving a clean tree")
}

// When only artifacts are dirty, the commit helpers report no commit (no empty
// commit, no failed `git commit`).
func TestCommitWithMessage_OnlyArtifactsIsNoOp(t *testing.T) {
	remote := setupBareRemote(t)
	ws := t.TempDir()

	g := NewGit(ws, "", "", "")
	require.NoError(t, g.Clone(context.Background(), remote, "main"))
	require.NoError(t, g.CreateBranch(context.Background(), "cm/cmx-001"))
	g.SetBranchPolicy("cm/cmx-001", "main", "main")

	elf := append([]byte{0x7f, 'E', 'L', 'F'}, make([]byte, 64)...)
	writeFile(t, ws, "app", elf, 0o755)

	committed, err := g.CommitWithMessage(context.Background(), "feat: nothing real")
	require.NoError(t, err)
	assert.False(t, committed, "a tree dirty only with artifacts must not produce a commit")
}

// TestAddInfoExcludeHidesWorktrees proves the clone-local exclude keeps a
// candidate-worktree path out of `git status` (and thus out of any staging
// path), and that a re-run is idempotent — the pattern is appended exactly once.
func TestAddInfoExcludeHidesWorktrees(t *testing.T) {
	remote := setupBareRemote(t)
	ws := t.TempDir()

	g := NewGit(ws, "", "", "")
	require.NoError(t, g.Clone(context.Background(), remote, "main"))

	// Isolate from any developer global gitignore that might already hide the dir.
	runGit(t, ws, "config", "core.excludesFile", "/dev/null")

	// An untracked file under .worktrees/ (the fan-out's candidate-worktree root).
	writeFile(t, ws, ".worktrees/c1/candidate.txt", []byte("wip\n"), 0o644)

	status, err := g.run(context.Background(), "status", "--porcelain")
	require.NoError(t, err)
	require.Contains(t, status, ".worktrees", "sanity: the worktree dir is untracked before the exclude")

	require.NoError(t, g.AddInfoExclude(context.Background(), ".worktrees/"))

	status, err = g.run(context.Background(), "status", "--porcelain")
	require.NoError(t, err)
	assert.NotContains(t, status, ".worktrees", "the excluded worktree dir must not appear in status")

	// Idempotent: a second call is a no-op — the pattern is present exactly once.
	require.NoError(t, g.AddInfoExclude(context.Background(), ".worktrees/"))

	data, err := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(data), ".worktrees/"), "pattern appended exactly once")
}

// TestStageForCommit_SkipsCandidateWorktrees proves the second layer of the
// defense: even with NO clone-local exclude, the staging path never stages a
// candidate worktree. A real linked worktree at .worktrees/c1 is exactly the
// mode-160000 gitlink a bare `git add .worktrees/` would land on the card branch.
func TestStageForCommit_SkipsCandidateWorktrees(t *testing.T) {
	remote := setupBareRemote(t)
	ws := t.TempDir()

	g := NewGit(ws, "", "", "")
	require.NoError(t, g.Clone(context.Background(), remote, "main"))
	require.NoError(t, g.CreateBranch(context.Background(), "cm/cmx-001"))
	g.SetBranchPolicy("cm/cmx-001", "main", "main")

	runGit(t, ws, "config", "core.excludesFile", "/dev/null")

	// Cut a real candidate worktree (the fan-out shape). NO AddInfoExclude here —
	// this exercises the stageForCommit prefix skip directly.
	wt := filepath.Join(ws, ".worktrees", "c1")
	require.NoError(t, g.AddWorktree(context.Background(), wt, "cm/cmx-001-c1", "cm/cmx-001"))

	// A legitimate source edit on the parent branch so the commit is non-empty.
	writeFile(t, ws, "main.go", []byte("package main\n"), 0o644)

	committed, err := g.CommitWithMessage(context.Background(), "feat: real work")
	require.NoError(t, err)
	require.True(t, committed)

	out, err := g.run(context.Background(), "show", "--name-only", "--format=", "HEAD")
	require.NoError(t, err)
	assert.Contains(t, out, "main.go")
	assert.NotContains(t, out, ".worktrees", "a candidate worktree must never be staged onto the card branch")
}

func TestCloneBranchCommitPush(t *testing.T) {
	t.Parallel()

	remote := setupBareRemote(t)
	ws := filepath.Join(t.TempDir(), "ws")

	g := NewGit(ws, "", "", "") // no token: file:// remote needs none

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

	g := NewGit(ws, "", "", "")

	ctx := context.Background()

	require.NoError(t, g.Clone(ctx, remote, "main"))
	require.NoError(t, g.CreateBranch(ctx, "cm/cmx-002"))

	dirty, err := g.CommitIfDirty(ctx, "No changes", "CMX-002")
	require.NoError(t, err)
	assert.False(t, dirty)
}

func TestCredEnvShape(t *testing.T) {
	// t.Setenv mutates the process env; cannot use t.Parallel.
	t.Setenv("LLM_API_KEY", "secret-llm-value")
	// os.TempDir honors TMPDIR; redirect it so the helper script writes into a
	// per-test temp dir instead of the shared /tmp.
	t.Setenv("TMPDIR", t.TempDir())

	secretsPath := filepath.Join(t.TempDir(), "env")
	require.NoError(t, os.WriteFile(secretsPath, []byte("CM_GIT_TOKEN=ghs_tok123456\n"), 0o600))

	g := NewGit(t.TempDir(), secretsPath, "github.com", "")

	env, err := g.credEnv()
	require.NoError(t, err)

	scriptPath := filepath.Join(os.TempDir(), "cm-git-credential-helper.sh")

	// The helper is host-scoped and points at the script; no baked token.
	assert.Contains(t, env, "GIT_CONFIG_COUNT=2")
	assert.Contains(t, env, "GIT_CONFIG_KEY_0=credential.https://github.com.helper")
	assert.Contains(t, env, "GIT_CONFIG_VALUE_0="+scriptPath)
	assert.Contains(t, env, "GIT_CONFIG_KEY_1=credential.https://github.com.useHttpPath")
	assert.Contains(t, env, "GIT_CONFIG_VALUE_1=false")

	joined := strings.Join(env, "\n")

	// The token VALUE never lands in the env — only the path to the helper does.
	assert.NotContains(t, joined, "ghs_tok123456")
	assert.NotContains(t, joined, "http.extraheader")
	assert.NotContains(t, joined, "LLM_API_KEY")
}

func TestCredEnvNoToken(t *testing.T) {
	t.Parallel()

	g := NewGit(t.TempDir(), "", "", "")

	env, err := g.credEnv()
	require.NoError(t, err)

	joined := strings.Join(env, "\n")

	assert.NotContains(t, joined, "GIT_CONFIG_COUNT")
	assert.NotContains(t, joined, "credential.https://")
	assert.NotContains(t, joined, "http.extraheader")
}

func TestCredEnvCACert(t *testing.T) {
	t.Parallel()

	t.Run("cert set injects GIT_SSL_CAINFO", func(t *testing.T) {
		t.Parallel()

		g := NewGit(t.TempDir(), "", "", "/run/cm-ca/ca.crt")

		env, err := g.credEnv()
		require.NoError(t, err)
		assert.Contains(t, env, "GIT_SSL_CAINFO=/run/cm-ca/ca.crt")
	})

	t.Run("no cert omits GIT_SSL_CAINFO", func(t *testing.T) {
		t.Parallel()

		g := NewGit(t.TempDir(), "", "", "")

		env, err := g.credEnv()
		require.NoError(t, err)
		assert.NotContains(t, strings.Join(env, "\n"), "GIT_SSL_CAINFO")
	})
}

// TestGitCredentialHelperScriptEchoesToken pins that the credential-helper script
// credEnv installs echoes the CM_GIT_TOKEN it reads from the secrets file on
// every `get` call, and re-reads a rotated file — the direct analog of the chat
// backend's credhelper test.
func TestGitCredentialHelperScriptEchoesToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// TMPDIR isolates the helper script (also forbids t.Parallel, as intended).
	t.Setenv("TMPDIR", t.TempDir())

	secretsPath := filepath.Join(t.TempDir(), "env")
	require.NoError(t, os.WriteFile(secretsPath, []byte("CM_GIT_TOKEN=tok1\n"), 0o600))

	g := NewGit(t.TempDir(), secretsPath, "github.com", "")

	// credEnv writes the helper script as a side effect.
	_, err := g.credEnv()
	require.NoError(t, err)

	scriptPath := filepath.Join(os.TempDir(), "cm-git-credential-helper.sh")

	runHelper := func(t *testing.T) string {
		t.Helper()

		out, err := exec.Command(scriptPath, "get").Output()
		require.NoError(t, err)

		return string(out)
	}

	out := runHelper(t)
	assert.Contains(t, out, "username=x-access-token")
	assert.Contains(t, out, "password=tok1")

	// Rotate the secrets file; the helper must re-read fresh on the next call.
	require.NoError(t, os.WriteFile(secretsPath, []byte("CM_GIT_TOKEN=tok2\n"), 0o600))

	out = runHelper(t)
	assert.Contains(t, out, "username=x-access-token")
	assert.Contains(t, out, "password=tok2")
}

// TestGitCredentialRotation is the credential-rotation contract: it drives git's
// real credential subsystem (`git credential fill`) with the env credEnv builds,
// so the injected helper resolves the token exactly as a clone/fetch/push would.
// It rewrites the secrets file between two fills and asserts the SECOND fill
// returned the NEW token — proving a git op after rotation uses the current
// token, not the one present when the worker started.
func TestGitCredentialRotation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// TMPDIR isolates the helper script (also forbids t.Parallel, as intended).
	t.Setenv("TMPDIR", t.TempDir())

	secretsPath := filepath.Join(t.TempDir(), "env")
	require.NoError(t, os.WriteFile(secretsPath, []byte("CM_GIT_TOKEN=tok-initial\n"), 0o600))

	g := NewGit(t.TempDir(), secretsPath, "github.com", "")

	gitconfig := filepath.Join(t.TempDir(), "gitconfig")

	// fill runs `git credential fill` for https://github.com with credEnv's env,
	// isolating global/system config so ONLY the injected helper answers.
	fill := func(t *testing.T) string {
		t.Helper()

		env, err := g.credEnv()
		require.NoError(t, err)

		env = append(env, "GIT_CONFIG_GLOBAL="+gitconfig, "GIT_CONFIG_NOSYSTEM=1")

		cmd := exec.Command("git", "credential", "fill")
		cmd.Env = env
		cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")

		out, err := cmd.Output()
		require.NoError(t, err)

		return string(out)
	}

	// First git op resolves the initial token.
	assert.Contains(t, fill(t), "password=tok-initial")

	// The host rewrites the secrets file ~10m before expiry.
	require.NoError(t, os.WriteFile(secretsPath, []byte("CM_GIT_TOKEN=tok-rotated\n"), 0o600))

	// The SECOND git op must observe the rotated token — the bug this fixes.
	second := fill(t)
	assert.Contains(t, second, "password=tok-rotated")
	assert.NotContains(t, second, "password=tok-initial")
}

func TestPushGuard(t *testing.T) {
	t.Parallel()

	g := NewGit(t.TempDir(), "", "", "")
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

			g := NewGit(t.TempDir(), "", "", "")
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

	g := NewGit(t.TempDir(), "", "", "")
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

	g := NewGit(t.TempDir(), "", "", "") // SetBranchPolicy never called

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

	g := NewGit(ws, "", "", "")
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
	g := NewGit(ws, "", "", "")
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

	g := NewGit(ws, "", "", "")

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
	g := NewGit(ws, "", "", "")
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
	g := NewGit(ws, "", "", "")
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
	g := NewGit(ws, "", "", "")
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
		g := NewGit(ws, "", "", "")
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
		g := NewGit(ws, "", "", "")
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
	g := NewGit(ws, "", "", "")
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
	g := NewGit(ws, "", "", "")
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
	g := NewGit(ws, "", "", "")
	ctx := context.Background()

	committed, err := g.CommitWithMessage(ctx, "no changes")
	require.NoError(t, err)
	assert.False(t, committed)
}

func TestLastCommitTouching(t *testing.T) {
	t.Parallel()

	_, ws, _ := setupClonedRepo(t)
	g := NewGit(ws, "", "", "")
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
	g := NewGit(ws, "", "", "")
	ctx := context.Background()

	h, err := g.Head(ctx)
	require.NoError(t, err)
	assert.Equal(t, initHash, h)
}

func TestCheckout(t *testing.T) {
	t.Parallel()

	bare, ws, _ := setupClonedRepo(t)
	g := NewGit(ws, "", "", "")
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
	g := NewGit(ws, "", "", "")
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
	g := NewGit(ws, "", "", "")
	ctx := context.Background()

	// No commits beyond origin/main, diff should be empty.
	diff, err := g.Diff(ctx, "origin/main")
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(diff))
}

// TestWorktreeLifecycle exercises the full candidate-worktree cycle a
// Best-of-N candidate goes through: add a linked worktree on a new branch cut
// from main, commit inside it via a Git handle rooted at the worktree,
// hard-reset the main checkout onto that commit (the winner-adoption path),
// then remove the worktree and delete its branch.
func TestWorktreeLifecycle(t *testing.T) {
	t.Parallel()

	_, ws, _ := setupClonedRepo(t)
	ctx := context.Background()

	g := NewGit(ws, "", "", "")
	require.NoError(t, g.DisableAutoGC(ctx))

	wt := filepath.Join(ws, ".worktrees", "c1")
	branch := "cm/x-1-c1"

	require.NoError(t, g.AddWorktree(ctx, wt, branch, "main"))

	info, err := os.Stat(wt)
	require.NoError(t, err)
	assert.True(t, info.IsDir(), "worktree directory must exist")

	// A Git handle rooted at the worktree sees the new branch checked out.
	wtGit := NewGit(wt, "", "", "")

	out, err := wtGit.run(ctx, "branch", "--show-current")
	require.NoError(t, err)
	assert.Equal(t, branch, strings.TrimSpace(out))

	// Commit a file inside the worktree via that handle.
	require.NoError(t, os.WriteFile(filepath.Join(wt, "candidate.txt"), []byte("c1\n"), 0o644))

	committed, err := wtGit.CommitWithMessage(ctx, "candidate c1 change")
	require.NoError(t, err)
	require.True(t, committed)

	wtHead, err := wtGit.Head(ctx)
	require.NoError(t, err)

	// Adopt the candidate: hard-reset the main checkout onto the worktree HEAD.
	require.NoError(t, g.HardReset(ctx, wtHead))

	mainHead, err := g.Head(ctx)
	require.NoError(t, err)
	assert.Equal(t, wtHead, mainHead)

	require.NoError(t, g.RemoveWorktree(ctx, wt))

	_, err = os.Stat(wt)
	assert.True(t, os.IsNotExist(err), "worktree directory should be gone after RemoveWorktree")

	require.NoError(t, g.DeleteBranch(ctx, branch))

	branchList, err := g.run(ctx, "branch", "--list", branch)
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(branchList), "branch should be gone after DeleteBranch")
}

// TestDeleteBranchGuards pins DeleteBranch's two refusals: anything outside
// the cm/ namespace, and — once a branch policy is set — the run's own card
// branch, which must survive even though it IS cm/-namespaced.
func TestDeleteBranchGuards(t *testing.T) {
	t.Parallel()

	_, ws, _ := setupClonedRepo(t)
	ctx := context.Background()

	g := NewGit(ws, "", "", "")

	err := g.DeleteBranch(ctx, "main")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside the cm/ namespace")

	require.NoError(t, g.CreateBranch(ctx, "cm/x-1"))
	g.SetBranchPolicy("cm/x-1", "main", "main")

	err = g.DeleteBranch(ctx, "cm/x-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the run's own card branch")
}

// TestCandidateGitCannotPush pins the structural safety property GitForDir
// relies on: a Git handle whose SetBranchPolicy was never called — exactly
// what a candidate worktree handle looks like — refuses every push, even to a
// cm/-namespaced branch, because guardPush is fail-closed on the zero value.
func TestCandidateGitCannotPush(t *testing.T) {
	t.Parallel()

	g := NewGit(t.TempDir(), "", "", "") // no SetBranchPolicy call

	err := g.Push(context.Background(), "cm/x-1-c1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "branch policy not set")
}
