package worker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"

	"github.com/mhersson/contextmatrix-harness/tools"
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
	workspace  string
	token      string
	caCertFile string // optional in-container extra CA PEM path; empty disables it
	host       string // repo host for GH_HOST (e.g. acme.ghe.com); empty leaves gh on its github.com default
}

// NewPRCreator builds a PRCreator for the given workspace and GitHub token (the
// same minted token the worker's Git uses to push). caCertFile is an optional
// in-container path to an extra CA PEM; empty disables the extra trust. repoURL
// is the clone URL (CM_REPO_URL); its host is exported as GH_HOST so gh
// recognizes a GitHub Enterprise host that it cannot infer from the git remote.
func NewPRCreator(workspace, token, caCertFile, repoURL string) *PRCreator {
	return &PRCreator{
		workspace:  workspace,
		token:      token,
		caCertFile: caCertFile,
		host:       hostFromRepoURL(repoURL),
	}
}

// hostFromRepoURL returns the host[:port] of an https repo URL, or "" when
// repoURL is empty or not a parseable URL with a host (e.g. an scp-style
// remote). Mirrors the runner entrypoint, which slices GIT_HOST off CM_REPO_URL.
func hostFromRepoURL(repoURL string) string {
	if repoURL == "" {
		return ""
	}

	u, err := url.Parse(repoURL)
	if err != nil {
		return ""
	}

	return u.Host
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

	extra := []string{"GH_TOKEN=" + p.token}
	if p.host != "" {
		// gh does not treat a GitHub Enterprise host (e.g. acme.ghe.com) as a
		// known host from the git remote alone and refuses to open the PR; GH_HOST
		// names it explicitly. Harmless for github.com. Mirrors the runner
		// entrypoint, which exports GH_HOST alongside GH_TOKEN.
		extra = append(extra, "GH_HOST="+p.host)
	}

	if p.caCertFile != "" {
		// gh is a Go binary; crypto/x509 on Linux honours SSL_CERT_FILE for the
		// system pool. GH_CA_BUNDLE is set defensively in case gh grows custom
		// handling. Both REPLACE the trust store for this invocation, which is
		// correct for the target deployments (see Git.credEnv). The container env
		// is scrubbed for subprocesses, so these are injected here.
		extra = append(extra, "SSL_CERT_FILE="+p.caCertFile, "GH_CA_BUNDLE="+p.caCertFile)
	}

	cmd.Env = tools.ScrubbedEnv(extra)

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
