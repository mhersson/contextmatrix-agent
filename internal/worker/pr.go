package worker

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/tools"
)

// prURLPattern matches the http(s) PR URL gh prints on success. gh writes a
// preamble line ("Creating pull request...") before the URL, so we scan for the
// first URL anywhere in stdout.
var prURLPattern = regexp.MustCompile(`https?://\S+`)

// PRCreator opens a pull request via the gh CLI. It satisfies
// orchestrator.PRCreator. The git token is injected as GH_TOKEN over a scrubbed
// env so gh authenticates to GitHub without inheriting any other secret from the
// process; gh runs in the workspace so it resolves the repo from origin.
type PRCreator struct {
	workspace string
	token     string
}

// NewPRCreator builds a PRCreator for the given workspace and GitHub token (the
// same minted token the worker's Git uses to push).
func NewPRCreator(workspace, token string) *PRCreator {
	return &PRCreator{workspace: workspace, token: token}
}

// buildCmd constructs the gh invocation without running it: argv, workspace
// dir, body on stdin, and the scrubbed env carrying GH_TOKEN. Split out so tests
// assert command construction without shelling out to gh.
func (p *PRCreator) buildCmd(ctx context.Context, title, body, base, head string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "gh", "pr", "create",
		"--title", title,
		"--body-file", "-",
		"--base", base,
		"--head", head,
	)
	cmd.Dir = p.workspace
	cmd.Stdin = strings.NewReader(body)
	cmd.Env = tools.ScrubbedEnv([]string{"GH_TOKEN=" + p.token})

	return cmd
}

// Create opens the pull request and returns its URL. It feeds the body on stdin
// (so arbitrary markdown is safe) and parses the URL gh prints to stdout.
func (p *PRCreator) Create(ctx context.Context, title, body, base, head string) (string, error) {
	cmd := p.buildCmd(ctx, title, body, base, head)

	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(string(out))

		var ee *exec.ExitError
		if errors.As(err, &ee) {
			detail = strings.TrimSpace(detail + "\n" + string(ee.Stderr))
		}

		return "", fmt.Errorf("gh pr create: %w: %s", err, detail)
	}

	url := parsePRURL(string(out))
	if url == "" {
		return "", fmt.Errorf("gh pr create: no PR URL in output: %s", strings.TrimSpace(string(out)))
	}

	return url, nil
}

// parsePRURL returns the first http(s) URL in gh's stdout, or "" if none.
func parsePRURL(out string) string {
	return prURLPattern.FindString(out)
}
