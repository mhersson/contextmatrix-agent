package worker

import (
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/tools"
)

// Git runs code-driven git operations for one card's workspace. Credentials
// are injected per invocation via an http.extraheader — they never land in
// on-disk config or the model-facing tool env.
type Git struct {
	dir   string
	token string
}

// NewGit creates a Git for the given workspace directory and optional GitHub
// installation token. An empty token means no credential injection (suitable
// for file:// remotes or public repos).
func NewGit(workspace, gitToken string) *Git {
	return &Git{dir: workspace, token: gitToken}
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

// CommitIfDirty stages all changes and commits them. It reports whether a
// commit was made. A clean tree (no changes) returns (false, nil).
//
// git add -A is intentional here: the workspace is the agent's ephemeral
// container directory for this card only; staging everything is the correct
// product behavior.
func (g *Git) CommitIfDirty(ctx context.Context, title, cardID string) (bool, error) {
	out, err := g.run(ctx, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check status: %w", err)
	}

	if strings.TrimSpace(out) == "" {
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

// Push pushes branch to origin. A diverged pre-existing remote branch fails
// by design — no force push.
func (g *Git) Push(ctx context.Context, branch string) error {
	_, err := g.run(ctx, "push", "-u", "origin", branch)

	return err
}
