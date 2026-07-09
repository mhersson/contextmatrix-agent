package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// ErrRebaseConflict is the rebase-conflict sentinel. It aliases
// orchestrator.ErrRebaseConflict so the orchestrator's integrate phase can match
// it with errors.Is across the package boundary without importing worker (the
// import edge is one-way: worker -> orchestrator). RebaseAutosquash returns it
// with the repo left clean (rebase aborted).
var ErrRebaseConflict = orchestrator.ErrRebaseConflict

// Git runs code-driven git operations for one card's workspace. Credentials
// reach git through a credential helper that re-reads the token from the secrets
// file on every git auth, so a token the host rotates on disk mid-run is picked
// up on the next operation — the token value never lands in on-disk config, the
// injected env, or the model-facing tool env; only the path to the helper does.
//
// The branch-policy fields (cardBranch, baseBranch, remoteDefault) gate every
// push through guardPush. They are a hard safety invariant: no config or env
// can loosen them, and the zero value (cardBranch == "") is fail-closed — a Git
// whose policy was never set refuses every push.
type Git struct {
	dir string

	// secretsEnvPath is the KEY=value secrets file the credential helper re-reads
	// CM_GIT_TOKEN from on every git auth. Empty disables credential injection
	// (file:// remotes or public repos). host scopes the helper to that GitHub
	// host so a live token is never offered to any other host git may contact.
	secretsEnvPath string
	host           string

	// caCertFile is an optional in-container path to an extra CA PEM. When set,
	// git subprocesses trust it via GIT_SSL_CAINFO — needed for TLS interception
	// or a private-CA GitHub Enterprise. The container env is scrubbed for
	// subprocesses, so this must be injected here rather than inherited.
	caCertFile string

	// credHelper* memoize the one-time write of the credential-helper script so
	// concurrent git operations share a single write; the token itself is still
	// read fresh from secretsEnvPath by the script on every git auth.
	credHelperOnce sync.Once
	credHelperPath string
	credHelperErr  error

	cardBranch    string // the run's own branch; the only ref this Git may push
	baseBranch    string // the card's base branch; never a force-push target
	remoteDefault string // origin/HEAD short name; never a force-push target
}

// NewGit creates a Git for the given workspace directory. secretsEnvPath is the
// KEY=value secrets file (e.g. /run/cm-secrets/env) the credential helper
// re-reads CM_GIT_TOKEN from on every git auth; an empty secretsEnvPath means no
// credential injection (suitable for file:// remotes or public repos). host
// scopes the credential helper to that GitHub host, defaulting to github.com; it
// is ignored when secretsEnvPath is empty. caCertFile is an optional
// in-container path to an extra CA PEM; empty disables the extra trust.
func NewGit(workspace, secretsEnvPath, host, caCertFile string) *Git {
	if secretsEnvPath != "" && host == "" {
		host = "github.com"
	}

	return &Git{dir: workspace, secretsEnvPath: secretsEnvPath, host: host, caCertFile: caCertFile}
}

// SetBranchPolicy records the push policy for this run: cardBranch is the only
// branch any push may target; baseBranch and remoteDefault are additionally
// protected against force-push. Called once at run startup; until it is, the
// guard is fail-closed and refuses every push.
func (g *Git) SetBranchPolicy(cardBranch, baseBranch, remoteDefault string) {
	g.cardBranch = cardBranch
	g.baseBranch = baseBranch
	g.remoteDefault = remoteDefault
}

// credEnv builds the env for git subprocesses: the scrubbed allowlist base plus
// fixed identity variables, an optional extra-CA path, and — when a secrets file
// is configured — a git credential helper that re-reads CM_GIT_TOKEN from that
// file on every git auth. The token value is never baked into git config or the
// env; only the path to the helper (which reads the file) is, so a token the
// host rotates on disk is picked up on the next git operation.
func (g *Git) credEnv() ([]string, error) {
	env := tools.ScrubbedEnv([]string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=contextmatrix-agent",
		"GIT_AUTHOR_EMAIL=agent@contextmatrix.local",
		"GIT_COMMITTER_NAME=contextmatrix-agent",
		"GIT_COMMITTER_EMAIL=agent@contextmatrix.local",
	})

	if g.caCertFile != "" {
		// GIT_SSL_CAINFO REPLACES the CA bundle for this invocation (unlike the
		// in-process Go client, which appends to the system pool). That is
		// correct for the target deployments: a TLS-intercepting proxy re-signs
		// every cert from this root, and a private-CA GHES issues its host cert
		// from it, so trusting only this file suffices.
		env = append(env, "GIT_SSL_CAINFO="+g.caCertFile)
	}

	if g.secretsEnvPath == "" {
		return env, nil
	}

	scriptPath, err := g.ensureCredentialHelper()
	if err != nil {
		return nil, err
	}

	// Scope the helper to the repo host only. An unscoped credential.helper would
	// offer the token to ANY https host git contacts (a malicious submodule URL or
	// redirect), leaking a live installation token off-platform. useHttpPath=false
	// makes the host alone the credential context, so the scope matches every repo
	// path on that host. Injected via GIT_CONFIG_* (not global git config) so it
	// applies only to the worker's own code-driven git, never the model's tools.
	scope := "credential.https://" + g.host

	return append(env,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0="+scope+".helper",
		"GIT_CONFIG_VALUE_0="+scriptPath,
		"GIT_CONFIG_KEY_1="+scope+".useHttpPath",
		"GIT_CONFIG_VALUE_1=false",
	), nil
}

// ensureCredentialHelper writes the credential-helper script once (memoized) and
// returns its path; concurrent git operations share the single write.
func (g *Git) ensureCredentialHelper() (string, error) {
	g.credHelperOnce.Do(func() {
		g.credHelperPath, g.credHelperErr = writeGitCredentialHelper(g.secretsEnvPath)
	})

	return g.credHelperPath, g.credHelperErr
}

// writeGitCredentialHelper writes a git credential-helper script to os.TempDir()
// that reads CM_GIT_TOKEN from secretsEnvPath on every `get` call and echoes it
// as the password. Only the path is baked into the script; the token is read
// fresh each call, so host-side rotation is transparent. It is written to
// os.TempDir() (not alongside secretsEnvPath) because the secrets mount is
// read-only in the container. Ported from the chat backend's credential helper.
func writeGitCredentialHelper(secretsEnvPath string) (string, error) {
	scriptPath := filepath.Join(os.TempDir(), "cm-git-credential-helper.sh")

	// Path is baked in; the token is read fresh on each git auth call.
	script := fmt.Sprintf(`#!/bin/sh
SECRETS_ENV='%s'

case "$1" in
    get)
        token=$(grep '^CM_GIT_TOKEN=' "$SECRETS_ENV" | cut -d= -f2-)
        echo "username=x-access-token"
        echo "password=$token"
        ;;
esac
`, secretsEnvPath)

	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		return "", fmt.Errorf("write git credential helper: %w", err)
	}

	return scriptPath, nil
}

func (g *Git) run(ctx context.Context, args ...string) (string, error) {
	env, err := g.credEnv()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.dir
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}

	return string(out), nil
}

// Clone clones url into g.dir, checking out baseBranch. An empty baseBranch
// uses the remote's default. The workspace directory must not exist yet.
func (g *Git) Clone(ctx context.Context, url, baseBranch string) error {
	args := []string{"clone"}
	if baseBranch != "" {
		args = append(args, "--branch", baseBranch)
	}

	args = append(args, "--", url, g.dir)

	env, err := g.credEnv()
	if err != nil {
		return fmt.Errorf("git clone: %w", err)
	}

	// Clone targets a not-yet-existing path, so Dir must be the parent.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = filepath.Dir(g.dir)
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}

	return nil
}

// CreateBranch creates and checks out a new branch from the current HEAD.
func (g *Git) CreateBranch(ctx context.Context, name string) error {
	_, err := g.run(ctx, "checkout", "-b", name)

	return err
}

// isDirty reports whether the working tree has any changes, including
// untracked files.
func (g *Git) isDirty(ctx context.Context) (bool, error) {
	out, err := g.run(ctx, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check status: %w", err)
	}

	return strings.TrimSpace(out) != "", nil
}

// buildArtifactExts are extensions that are essentially always compiled
// outputs, never first-party source. Untracked files with these extensions are
// not staged.
var buildArtifactExts = map[string]bool{
	".exe": true, ".dll": true, ".so": true, ".dylib": true,
	".o": true, ".a": true,
	".pyc": true, ".pyo": true, ".class": true,
}

// buildArtifactDirs are path segments whose contents are build outputs or
// vendored dependencies, never source the agent should author.
var buildArtifactDirs = map[string]bool{
	"node_modules": true, "__pycache__": true, "target": true,
}

// isLikelyBuildArtifact reports whether the untracked file at absPath (relative
// path rel) looks like a compiled build output that must not be committed: a
// known build-output extension, a build-output path segment, or a native
// executable magic number. Read errors are treated as "not an artifact" — the
// guard never blocks on its own failure, only on positive evidence.
func isLikelyBuildArtifact(absPath, rel string) bool {
	if buildArtifactExts[strings.ToLower(filepath.Ext(rel))] {
		return true
	}

	for seg := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
		if buildArtifactDirs[seg] {
			return true
		}
	}

	return hasExecutableMagic(absPath)
}

// hasExecutableMagic reports whether the file begins with a known native
// executable magic number (ELF, Mach-O, or PE/COFF) — a compiled binary the
// agent never legitimately authors as source.
func hasExecutableMagic(absPath string) bool {
	f, err := os.Open(absPath) //nolint:gosec // path is a workspace-relative entry from git status
	if err != nil {
		return false
	}
	defer f.Close() //nolint:errcheck // read-only sniff

	var magic [4]byte
	if n, _ := io.ReadFull(f, magic[:]); n < 4 {
		return false
	}

	switch {
	case magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F':
		return true // ELF (Linux/BSD)
	case magic[0] == 0xcf && magic[1] == 0xfa && magic[2] == 0xed && magic[3] == 0xfe:
		return true // Mach-O 64-bit
	case magic[0] == 0xce && magic[1] == 0xfa && magic[2] == 0xed && magic[3] == 0xfe:
		return true // Mach-O 32-bit
	case magic[0] == 0xca && magic[1] == 0xfe && magic[2] == 0xba && magic[3] == 0xbe:
		return true // Mach-O universal
	case magic[0] == 'M' && magic[1] == 'Z':
		return true // PE/COFF (Windows)
	}

	return false
}

// stageForCommit stages the working tree while refusing to add untracked build
// artifacts. It stages all TRACKED modifications/deletions (`git add -u`, which
// never adds untracked files), then adds each UNTRACKED file individually
// unless it looks like a build artifact, in which case the file is skipped with
// a warning. This replaces a blanket `git add -A`: a build step's output can no
// longer be committed just because the target repo's .gitignore failed to cover
// it. Untracked files that look like build artifacts are skipped with a
// warning rather than counted back to the caller.
func (g *Git) stageForCommit(ctx context.Context) error {
	if _, err := g.run(ctx, "add", "-u"); err != nil {
		return fmt.Errorf("stage tracked changes: %w", err)
	}

	// -z NUL-delimits records and disables path quoting, so filenames with
	// spaces or non-ASCII bytes reach `git add` verbatim. Without it git
	// C-quotes such paths and the quoted string fails to match, aborting the
	// whole commit — a regression from the blanket `git add -A` this replaced.
	out, err := g.run(ctx, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("list untracked: %w", err)
	}

	for record := range strings.SplitSeq(out, "\x00") {
		// Under -z each record is "XY <path>" with no quoting; untracked is
		// "?? <path>". The trailing NUL yields a final empty record (skipped).
		if !strings.HasPrefix(record, "?? ") {
			continue
		}

		rel := strings.TrimPrefix(record, "?? ")
		if rel == "" {
			continue
		}

		// Defense in depth for the Best-of-N fan-out: a candidate worktree lives at
		// .worktrees/cK and shows up untracked in the parent clone. Staging it adds
		// a mode-160000 gitlink to the card branch. The fan-out also writes a clone-
		// local exclude for this, but never stage it regardless of that exclude.
		if rel == ".worktrees" || strings.HasPrefix(rel, ".worktrees/") {
			slog.Warn("refusing to stage candidate worktree path", "path", rel)

			continue
		}

		if isLikelyBuildArtifact(filepath.Join(g.dir, rel), rel) {
			slog.Warn("refusing to stage likely build artifact", "path", rel)

			continue
		}

		if _, err := g.run(ctx, "add", "--", rel); err != nil {
			return fmt.Errorf("stage %q: %w", rel, err)
		}
	}

	return nil
}

// hasStagedChanges reports whether the index has changes to commit. It runs
// `git diff --cached --quiet` directly to read the exit code: 0 = nothing
// staged, 1 = staged changes present.
func (g *Git) hasStagedChanges(ctx context.Context) (bool, error) {
	env, err := g.credEnv()
	if err != nil {
		return false, fmt.Errorf("check staged changes: %w", err)
	}

	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--quiet")
	cmd.Dir = g.dir
	cmd.Env = env

	err = cmd.Run()
	if err == nil {
		return false, nil
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return true, nil
	}

	return false, fmt.Errorf("check staged changes: %w", err)
}

// CommitIfDirty stages tracked changes plus non-artifact untracked files via
// stageForCommit (see its doc) and commits them. It reports whether a commit
// was made. A clean tree (no changes) returns (false, nil).
func (g *Git) CommitIfDirty(ctx context.Context, title, cardID string) (bool, error) {
	dirty, err := g.isDirty(ctx)
	if err != nil {
		return false, err
	}

	if !dirty {
		return false, nil
	}

	if err := g.stageForCommit(ctx); err != nil {
		return false, err
	}

	staged, err := g.hasStagedChanges(ctx)
	if err != nil {
		return false, err
	}

	if !staged {
		// The tree was dirty only with skipped artifacts — nothing to commit.
		return false, nil
	}

	body := "Automated commit by contextmatrix-agent for " + cardID + "."

	if _, err := g.run(ctx, "commit", "-m", title, "-m", body); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}

	return true, nil
}

// RemoteDefaultBranch returns the remote's default branch short name (from
// origin/HEAD), or "" if it cannot be determined. Best-effort: used only to
// widen the force-push denylist, never to permit a push.
func (g *Git) RemoteDefaultBranch(ctx context.Context) string {
	out, err := g.run(ctx, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return ""
	}

	return strings.TrimPrefix(strings.TrimSpace(out), "refs/remotes/origin/")
}

// guardPush is the single hard-safety chokepoint every push must pass before
// any network call. The rules are hardcoded — no config or env may loosen
// them, and the zero value is fail-closed:
//
//  1. Only the run's own card branch may be pushed at all (force or not). The
//     zero-value cardBranch ("") makes this refuse every push.
//  2. A force push is additionally refused to the base branch, main, master,
//     or the remote default — protected refs that must never be rewritten.
//  3. A force push is refused to anything outside the cm/ namespace.
func (g *Git) guardPush(branch string, force bool) error {
	if g.cardBranch == "" {
		return fmt.Errorf("refusing to push %q: branch policy not set", branch)
	}

	if branch != g.cardBranch {
		return fmt.Errorf("refusing to push %q: only the run's own card branch %q may be pushed", branch, g.cardBranch)
	}

	if force {
		for _, forbidden := range []string{g.baseBranch, "main", "master", g.remoteDefault} {
			if forbidden != "" && branch == forbidden {
				return fmt.Errorf("refusing to force-push %q: protected ref", branch)
			}
		}

		if !strings.HasPrefix(branch, "cm/") {
			return fmt.Errorf("refusing to force-push %q: outside the cm/ namespace", branch)
		}
	}

	return nil
}

// Push pushes branch to origin via an explicit refspec — never a bare
// git push, so no push.default, upstream config, or "matching" can redirect
// it. Guarded by guardPush. A diverged pre-existing remote branch fails by
// design; force pushes go through ForcePushWithLease.
func (g *Git) Push(ctx context.Context, branch string) error {
	if err := g.guardPush(branch, false); err != nil {
		return err
	}

	_, err := g.run(ctx, "push", "origin", "HEAD:refs/heads/"+branch)

	return err
}

// ForcePushWithLease force-pushes the card branch, guarded, with an explicit
// expected remote tip — never bare --force, never valueless --force-with-lease.
func (g *Git) ForcePushWithLease(ctx context.Context, branch, expectedTip string) error {
	if expectedTip == "" {
		return errors.New("force-with-lease: expected tip required")
	}

	if err := g.guardPush(branch, true); err != nil {
		return err
	}

	_, err := g.run(ctx, "push", "--force-with-lease="+branch+":"+expectedTip,
		"origin", "HEAD:refs/heads/"+branch)

	return err
}

// Fetch fetches ref from origin into the local FETCH_HEAD / remote-tracking ref.
func (g *Git) Fetch(ctx context.Context, ref string) error {
	_, err := g.run(ctx, "fetch", "origin", ref)

	return err
}

// RemoteTip returns the commit hash at origin/branch via ls-remote. It returns
// ("", nil) when the branch does not exist on the remote.
func (g *Git) RemoteTip(ctx context.Context, branch string) (string, error) {
	out, err := g.run(ctx, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("remote tip %q: %w", branch, err)
	}

	line := strings.TrimSpace(out)
	if line == "" {
		return "", nil
	}

	// ls-remote output: "<hash>\trefs/heads/<branch>"
	return strings.Fields(line)[0], nil
}

// MergeBase returns the merge-base commit hash between a and b.
func (g *Git) MergeBase(ctx context.Context, a, b string) (string, error) {
	out, err := g.run(ctx, "merge-base", a, b)
	if err != nil {
		return "", fmt.Errorf("merge-base %q %q: %w", a, b, err)
	}

	return strings.TrimSpace(out), nil
}

// CommitWithMessage stages tracked changes plus non-artifact untracked files
// via stageForCommit (see its doc) and commits with the given message. It
// reports whether a commit was made. A clean tree returns (false, nil).
func (g *Git) CommitWithMessage(ctx context.Context, message string) (bool, error) {
	if err := g.stageForCommit(ctx); err != nil {
		return false, err
	}

	staged, err := g.hasStagedChanges(ctx)
	if err != nil {
		return false, err
	}

	if !staged {
		return false, nil
	}

	if _, err := g.run(ctx, "commit", "-m", message); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}

	return true, nil
}

// CommitFixup stages tracked changes plus non-artifact untracked files via
// stageForCommit (see its doc) — fix runs can legitimately create new source
// files — and creates a "fixup! <subject>" commit targeting target (the hash of
// the commit to fix up). It reports whether a commit was made. A clean tree
// returns (false, nil).
func (g *Git) CommitFixup(ctx context.Context, target string) (bool, error) {
	if err := g.stageForCommit(ctx); err != nil {
		return false, err
	}

	staged, err := g.hasStagedChanges(ctx)
	if err != nil {
		return false, err
	}

	if !staged {
		return false, nil
	}

	if _, err := g.run(ctx, "commit", "--fixup="+target); err != nil {
		return false, fmt.Errorf("commit fixup: %w", err)
	}

	return true, nil
}

// LastCommitTouching returns the hash of the most recent commit that touches
// any of the given paths. It returns ("", nil) when no commit touches them.
func (g *Git) LastCommitTouching(ctx context.Context, paths []string) (string, error) {
	args := append([]string{"log", "-1", "--format=%H", "--"}, paths...)

	out, err := g.run(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("last commit touching %v: %w", paths, err)
	}

	return strings.TrimSpace(out), nil
}

// RebaseAutosquash rebases the current branch onto onto with --autosquash,
// collapsing any fixup! commits. If the rebase encounters a conflict it aborts,
// leaving the repo clean, and returns ErrRebaseConflict.
//
// GIT_SEQUENCE_EDITOR=true skips the interactive editor prompt.
func (g *Git) RebaseAutosquash(ctx context.Context, onto string) error {
	env, err := g.credEnv()
	if err != nil {
		return fmt.Errorf("rebase: %w", err)
	}

	env = append(env, "GIT_SEQUENCE_EDITOR=true")

	cmd := exec.CommandContext(ctx, "git", "rebase", "-i", "--autosquash", onto)
	cmd.Dir = g.dir
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	// Rebase failed — abort to leave the repo clean.
	_, abortErr := g.run(ctx, "rebase", "--abort")
	if abortErr != nil {
		return fmt.Errorf("rebase abort after conflict: %w (original: %s)", abortErr, strings.TrimSpace(string(out)))
	}

	return fmt.Errorf("%w: %s", ErrRebaseConflict, strings.TrimSpace(string(out)))
}

// SoftReset moves HEAD to to, leaving the index and working tree unchanged.
func (g *Git) SoftReset(ctx context.Context, to string) error {
	_, err := g.run(ctx, "reset", "--soft", to)

	return err
}

// Head returns the current HEAD commit hash.
func (g *Git) Head(ctx context.Context) (string, error) {
	out, err := g.run(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("head: %w", err)
	}

	return strings.TrimSpace(out), nil
}

// Checkout checks out ref, creating a local tracking branch if it exists on
// origin but not locally.
func (g *Git) Checkout(ctx context.Context, ref string) error {
	_, err := g.run(ctx, "checkout", ref)

	return err
}

// Diff returns the unified diff of all commits between merge-base(base, HEAD)
// and HEAD. This is the three-dot diff: changes introduced on this branch
// relative to base, ignoring divergence in base itself.
func (g *Git) Diff(ctx context.Context, base string) (string, error) {
	mergeBase, err := g.MergeBase(ctx, base, "HEAD")
	if err != nil {
		return "", fmt.Errorf("diff: %w", err)
	}

	out, err := g.run(ctx, "diff", mergeBase+"...HEAD")
	if err != nil {
		return "", fmt.Errorf("diff %q...HEAD: %w", mergeBase, err)
	}

	return out, nil
}

// AddWorktree creates a linked worktree at path on a new branch cut from
// startRef. Candidate worktrees share the clone's object store.
func (g *Git) AddWorktree(ctx context.Context, path, branch, startRef string) error {
	_, err := g.run(ctx, "worktree", "add", "-b", branch, path, startRef)

	return err
}

// RemoveWorktree force-removes a linked worktree (dirty trees included —
// losing candidates are discarded wholesale).
func (g *Git) RemoveWorktree(ctx context.Context, path string) error {
	_, err := g.run(ctx, "worktree", "remove", "--force", path)

	return err
}

// DeleteBranch deletes a local candidate branch. Only cm/-namespaced branches
// other than the run's own card branch may be deleted.
func (g *Git) DeleteBranch(ctx context.Context, name string) error {
	if !strings.HasPrefix(name, "cm/") {
		return fmt.Errorf("refusing to delete %q: outside the cm/ namespace", name)
	}

	if g.cardBranch != "" && name == g.cardBranch {
		return fmt.Errorf("refusing to delete %q: the run's own card branch", name)
	}

	_, err := g.run(ctx, "branch", "-D", name)

	return err
}

// HardReset moves the checked-out branch to ref, discarding the work tree.
// Used once, to adopt the Best-of-N winner onto the card branch.
func (g *Git) HardReset(ctx context.Context, ref string) error {
	_, err := g.run(ctx, "reset", "--hard", ref)

	return err
}

// DiffStat is Diff's --stat form, for judge prompts where the full diff
// exceeds the token cap.
func (g *Git) DiffStat(ctx context.Context, base string) (string, error) {
	mergeBase, err := g.MergeBase(ctx, base, "HEAD")
	if err != nil {
		return "", fmt.Errorf("diffstat: %w", err)
	}

	out, err := g.run(ctx, "diff", "--stat", mergeBase+"...HEAD")
	if err != nil {
		return "", fmt.Errorf("diffstat %q...HEAD: %w", mergeBase, err)
	}

	return out, nil
}

// DisableAutoGC turns off auto-gc for the clone: candidate worktrees share
// the object store, and a background gc racing N writers is the one shared
// failure mode worktrees introduce.
func (g *Git) DisableAutoGC(ctx context.Context) error {
	_, err := g.run(ctx, "config", "gc.auto", "0")

	return err
}

// AddInfoExclude appends pattern to the clone's local exclude file
// ($GIT_COMMON_DIR/info/exclude) unless it is already listed, so untracked
// paths matching it never surface to `git status` and can never be staged.
// Unlike .gitignore this is clone-local (never committed) and unlike the model
// tool env it needs no repo write the coder could observe. Idempotent: a re-run
// that finds the pattern already present is a no-op. The fan-out uses it to hide
// candidate worktrees (.worktrees/) from the parent clone's staging path so a
// WIP push on a park cannot stage them as gitlinks. This is a file write, not a
// git command: rev-parse resolves the git dir, then os handles the append.
func (g *Git) AddInfoExclude(ctx context.Context, pattern string) error {
	// A worktree's --git-common-dir points at the main clone's .git, which is
	// where info/exclude lives. The path may be relative to the clone, so anchor
	// it under g.dir.
	out, err := g.run(ctx, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("resolve git-common-dir: %w", err)
	}

	gitDir := strings.TrimSpace(out)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(g.dir, gitDir)
	}

	excludePath := filepath.Join(gitDir, "info", "exclude")

	if existing, err := os.ReadFile(excludePath); err == nil {
		for line := range strings.SplitSeq(string(existing), "\n") {
			if strings.TrimSpace(line) == pattern {
				return nil // already excluded — idempotent no-op
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("ensure info dir: %w", err)
	}

	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open exclude file: %w", err)
	}

	if _, err := f.WriteString(pattern + "\n"); err != nil {
		_ = f.Close() //nolint:errcheck // write already failed; report that error

		return fmt.Errorf("append exclude pattern: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close exclude file: %w", err)
	}

	return nil
}
