package worker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
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

// CommitIfDirty stages all changes and commits them. It reports whether a
// commit was made. A clean tree (no changes) returns (false, nil).
//
// git add -A is intentional here: the workspace is the agent's ephemeral
// container directory for this card only; staging everything is the correct
// product behavior.
func (g *Git) CommitIfDirty(ctx context.Context, title, cardID string) (bool, error) {
	dirty, err := g.isDirty(ctx)
	if err != nil {
		return false, err
	}

	if !dirty {
		return false, nil
	}

	if _, err := g.run(ctx, "add", "-A"); err != nil {
		return false, fmt.Errorf("stage changes: %w", err)
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

// CommitWithMessage stages all changes and commits with the given message. It
// reports whether a commit was made. A clean tree returns (false, nil).
//
// git add -A is intentional: this operates inside the ephemeral container
// workspace for this card; staging everything is the correct product behavior.
func (g *Git) CommitWithMessage(ctx context.Context, message string) (bool, error) {
	dirty, err := g.isDirty(ctx)
	if err != nil {
		return false, err
	}

	if !dirty {
		return false, nil
	}

	if _, err := g.run(ctx, "add", "-A"); err != nil {
		return false, fmt.Errorf("stage changes: %w", err)
	}

	if _, err := g.run(ctx, "commit", "-m", message); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}

	return true, nil
}

// CommitFixup stages all changes — including untracked files, since fix runs
// can legitimately create new ones — and creates a "fixup! <subject>" commit
// targeting target (the hash of the commit to fix up). It reports whether a
// commit was made. A clean tree returns (false, nil).
func (g *Git) CommitFixup(ctx context.Context, target string) (bool, error) {
	dirty, err := g.isDirty(ctx)
	if err != nil {
		return false, err
	}

	if !dirty {
		return false, nil
	}

	if _, err := g.run(ctx, "add", "-A"); err != nil {
		return false, fmt.Errorf("stage changes: %w", err)
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
