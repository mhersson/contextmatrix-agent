package worker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
// are injected per invocation via an http.extraheader — they never land in
// on-disk config or the model-facing tool env.
//
// The branch-policy fields (cardBranch, baseBranch, remoteDefault) gate every
// push through guardPush. They are a hard safety invariant: no config or env
// can loosen them, and the zero value (cardBranch == "") is fail-closed — a Git
// whose policy was never set refuses every push.
type Git struct {
	dir   string
	token string

	cardBranch    string // the run's own branch; the only ref this Git may push
	baseBranch    string // the card's base branch; never a force-push target
	remoteDefault string // origin/HEAD short name; never a force-push target
}

// NewGit creates a Git for the given workspace directory and optional GitHub
// installation token. An empty token means no credential injection (suitable
// for file:// remotes or public repos).
func NewGit(workspace, gitToken string) *Git {
	return &Git{dir: workspace, token: gitToken}
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

// credEnv builds the env for git subprocesses: the scrubbed allowlist base
// plus fixed identity variables, plus per-invocation credentials as an
// http.extraheader. The token is base64-encoded and never stored on disk.
func (g *Git) credEnv() []string {
	env := tools.ScrubbedEnv([]string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=contextmatrix-agent",
		"GIT_AUTHOR_EMAIL=agent@contextmatrix.local",
		"GIT_COMMITTER_NAME=contextmatrix-agent",
		"GIT_COMMITTER_EMAIL=agent@contextmatrix.local",
	})

	if g.token == "" {
		return env
	}

	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + g.token))

	return append(env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+auth,
	)
}

func (g *Git) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.dir
	cmd.Env = g.credEnv()

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

	args = append(args, url, g.dir)

	// Clone targets a not-yet-existing path, so Dir must be the parent.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = filepath.Dir(g.dir)
	cmd.Env = g.credEnv()

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

	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
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

	for _, record := range strings.Split(out, "\x00") {
		// Under -z each record is "XY <path>" with no quoting; untracked is
		// "?? <path>". The trailing NUL yields a final empty record (skipped).
		if !strings.HasPrefix(record, "?? ") {
			continue
		}

		rel := strings.TrimPrefix(record, "?? ")
		if rel == "" {
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
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--quiet")
	cmd.Dir = g.dir
	cmd.Env = g.credEnv()

	err := cmd.Run()
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
	cmd := exec.CommandContext(ctx, "git", "rebase", "-i", "--autosquash", onto)
	cmd.Dir = g.dir
	cmd.Env = append(g.credEnv(), "GIT_SEQUENCE_EDITOR=true")

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
