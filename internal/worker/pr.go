package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-agent/internal/secrets"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// prURLPattern matches the http(s) PR URL gh prints on success. gh writes a
// preamble line ("Creating pull request...") before the URL, so we scan for the
// first URL anywhere in stdout.
var prURLPattern = regexp.MustCompile(`https?://\S+`)

// prPathPattern extracts owner, repo, and PR number from a PR URL. Matching
// only the path after the host works for github.com and a GitHub Enterprise
// host alike.
var prPathPattern = regexp.MustCompile(`^https?://[^/]+/([^/]+)/([^/]+)/pull/(\d+)`)

// runIDPattern extracts the numeric run ID from an Actions job/run link.
var runIDPattern = regexp.MustCompile(`/actions/runs/(\d+)`)

// noChecksReportedPattern matches gh pr checks' "no checks reported on the 'X'
// branch" failure. runGH embeds stdout and stderr in the error it builds, so
// matching the error text covers the whole failure (non-zero exit with a
// non-JSON report - a JSON report would not have errored at all).
var noChecksReportedPattern = regexp.MustCompile(`(?i)no checks reported`)

// checksInaccessiblePattern matches the refusal GitHub returns when the
// token cannot read the Checks API. Fine-grained PATs can never carry the
// Checks permission (GitHub grants it to Apps only), so on a private repo
// this failure is deterministic for the whole run - Checks flips to the
// Actions-runs fallback on first sight of it.
var checksInaccessiblePattern = regexp.MustCompile(`(?i)resource not accessible by personal access token`)

// unknownFlagPattern matches gh rejecting a flag it does not know - a gh too
// old for the fallback poll's run list arguments. The same gh answers the same
// way on every poll.
var unknownFlagPattern = regexp.MustCompile(`(?i)unknown flag`)

// noPRForBranchPattern matches gh pr view's "no pull requests found for
// branch 'X'" failure - the expected outcome when probing a branch that has
// no PR, not an error.
var noPRForBranchPattern = regexp.MustCompile(`(?i)no pull requests? found`)

// copilotReviewerLogin is the GitHub login gh adds as a reviewer and the login
// GitHub's REST API reports as the author of a Copilot review.
const copilotReviewerLogin = "copilot-pull-request-reviewer[bot]"

// Failure-log digest caps: bounded so a noisy CI run cannot blow the prompt
// budget of the pr_gates phase that consumes FailureLogs' output.
const (
	failureLogPerCheckCap = 8 * 1024
	failureLogTotalCap    = 24 * 1024
)

// PRCreator opens a pull request via the gh CLI. It satisfies
// orchestrator.PRCreator and orchestrator.PRGates. GH_TOKEN is resolved fresh
// from the secrets file at call time and injected over a scrubbed env so gh
// authenticates to GitHub without inheriting any other secret from the
// process; gh runs in the workspace so it resolves the repo from origin.
type PRCreator struct {
	workspace string

	// secretsEnvPath is the KEY=value secrets file CM_GIT_TOKEN is re-read from
	// per gh invocation, so an end-of-run PR uses the current token even after the
	// host rotated it. Empty means no auth (public/file:// remotes).
	secretsEnvPath string

	caCertFile string // optional in-container extra CA PEM path; empty disables it
	host       string // repo host for GH_HOST (e.g. acme.ghe.com); empty leaves gh on its github.com default

	// checksFallback, once set, routes every subsequent Checks poll through
	// the Actions-runs + commit-status APIs: the rollup read failed with the
	// fine-grained-PAT Checks refusal, which never heals within a run.
	checksFallback bool
}

var _ orchestrator.PRGates = (*PRCreator)(nil)

// NewPRCreator builds a PRCreator for the given workspace. secretsEnvPath is the
// secrets file gh re-reads CM_GIT_TOKEN from per invocation (empty disables
// auth). caCertFile is an optional in-container path to an extra CA PEM; empty
// disables the extra trust. repoURL is the clone URL (CM_REPO_URL); its host is
// exported as GH_HOST so gh recognizes a GitHub Enterprise host that it cannot
// infer from the git remote.
func NewPRCreator(workspace, secretsEnvPath, caCertFile, repoURL string) *PRCreator {
	return &PRCreator{
		workspace:      workspace,
		secretsEnvPath: secretsEnvPath,
		caCertFile:     caCertFile,
		host:           hostFromRepoURL(repoURL),
	}
}

// gitToken reads CM_GIT_TOKEN fresh from the secrets file so each gh invocation
// authenticates with the current token, not one cached at startup. It returns
// ("", nil) when no secrets file is configured (public/file:// remotes - no
// auth needed); an unreadable file is an error so the caller surfaces a clear
// authentication-setup failure instead of a generic unauthenticated gh one.
func (p *PRCreator) gitToken() (string, error) {
	if p.secretsEnvPath == "" {
		return "", nil
	}

	src, err := secrets.Open(p.secretsEnvPath)
	if err != nil {
		return "", fmt.Errorf("read git token for gh: %w", err)
	}

	return src.Get("CM_GIT_TOKEN"), nil
}

// hostFromRepoURL returns the host[:port] of an https repo URL, or "" when
// repoURL is empty or not a parseable URL with a host (e.g. an scp-style
// remote). GIT_HOST is sliced off CM_REPO_URL this way.
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

// ghEnv assembles the extra "KEY=VALUE" environment entries every gh
// invocation carries: a fresh GH_TOKEN read from the secrets file, GH_HOST for
// a GitHub Enterprise remote, and CA overrides when configured. Shared by
// buildCmd and ghCmd so every gh invocation authenticates identically.
func (p *PRCreator) ghEnv() ([]string, error) {
	var extra []string

	token, err := p.gitToken()
	if err != nil {
		return nil, err
	}

	if token != "" {
		extra = append(extra, "GH_TOKEN="+token)
	}

	if p.host != "" {
		// gh does not treat a GitHub Enterprise host (e.g. acme.ghe.com) as a
		// known host from the git remote alone and refuses to open the PR; GH_HOST
		// names it explicitly. Harmless for github.com. GH_HOST is exported
		// alongside GH_TOKEN.
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

	return extra, nil
}

// buildCmd constructs the gh pr create invocation without running it: argv,
// workspace dir, body on stdin, and the scrubbed env carrying GH_TOKEN. Split
// out so tests assert command construction without shelling out to gh. An
// unreadable secrets file is an error - gh must not run unauthenticated on a
// stale or broken credential mount.
func (p *PRCreator) buildCmd(ctx context.Context, title, body, base, head string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(
		ctx, "gh", "pr", "create",
		"--title", title,
		"--body-file", "-",
		"--base", base,
		"--head", head,
	)
	cmd.Dir = p.workspace
	cmd.Stdin = strings.NewReader(body)

	extra, err := p.ghEnv()
	if err != nil {
		return nil, err
	}

	cmd.Env = tools.ScrubbedEnv(extra)

	return cmd, nil
}

// ghCmd builds a gh invocation with the same auth/CA/host env as buildCmd:
// scrubbed env + fresh GH_TOKEN, GH_HOST, CA overrides; runs in the workspace.
// A non-empty stdin is fed to the command; an empty stdin leaves cmd.Stdin nil.
func (p *PRCreator) ghCmd(ctx context.Context, stdin string, args ...string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = p.workspace

	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	extra, err := p.ghEnv()
	if err != nil {
		return nil, err
	}

	cmd.Env = tools.ScrubbedEnv(extra)

	return cmd, nil
}

// ghSucceeded reports whether a gh invocation should be treated as
// successful: a nil error always is; jsonTolerant also accepts a non-nil
// error when stdout parses as JSON - gh pr checks exits 1 on failing checks
// and 8 on pending while still printing the JSON report.
func ghSucceeded(err error, stdout string, jsonTolerant bool) bool {
	if err == nil {
		return true
	}

	return jsonTolerant && json.Valid([]byte(stdout))
}

// runGH executes a gh invocation and returns trimmed stdout. Exit errors carry
// stderr in the message. jsonTolerant, when true, returns stdout despite a
// non-zero exit if stdout parses as JSON - gh pr checks exits 1 on failing
// checks and 8 on pending while still printing the JSON report. stdin mirrors
// ghCmd's parameter for callers that need to feed a request body; no current
// PRGates call needs one.
func (p *PRCreator) runGH(ctx context.Context, stdin string, jsonTolerant bool, args ...string) (string, error) { //nolint:unparam // seam signature; see doc comment
	cmd, err := p.ghCmd(ctx, stdin, args...)
	if err != nil {
		return "", err
	}

	out, runErr := cmd.Output()
	trimmed := strings.TrimSpace(string(out))

	if ghSucceeded(runErr, trimmed, jsonTolerant) {
		return trimmed, nil
	}

	detail := trimmed

	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		detail = strings.TrimSpace(trimmed + "\n" + string(ee.Stderr))
	}

	return "", fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), runErr, detail)
}

// Create opens the pull request and returns its URL. It feeds the body on stdin
// (so arbitrary markdown is safe) and parses the URL gh prints to stdout.
func (p *PRCreator) Create(ctx context.Context, title, body, base, head string) (string, error) {
	cmd, err := p.buildCmd(ctx, title, body, base, head)
	if err != nil {
		return "", fmt.Errorf("gh pr create: %w", err)
	}

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

// parsePRViewURL unmarshals gh pr view --json url,state output, returning the
// URL only when the PR is OPEN. gh pr view falls back to the branch's most
// recent CLOSED or MERGED PR when it has none open, exiting zero rather than
// erroring the way it does for a branch with no PR at all - so the state
// check, not the caller's error handling, is what keeps this a probe for an
// open PR.
func parsePRViewURL(out string) (string, error) {
	var payload struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}

	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return "", fmt.Errorf("parse gh pr view output: %w", err)
	}

	if payload.State != "OPEN" {
		return "", nil
	}

	return payload.URL, nil
}

// checksArgs builds the gh pr checks invocation, requesting exactly the JSON
// fields parseChecks reads.
func checksArgs(prURL string) []string {
	return []string{"pr", "checks", prURL, "--json", "name,bucket,link,description"}
}

// headSHAArgs builds the gh pr view invocation that reports the PR's current
// head commit SHA.
func headSHAArgs(prURL string) []string {
	return []string{"pr", "view", prURL, "--json", "headRefOid"}
}

// runListArgs builds the gh run list invocation for the head SHA's workflow
// runs, requesting exactly the JSON fields parseRunList reads. --limit
// bounds a pathological rerun pile-up; 100 is far above any real per-commit
// run count.
func runListArgs(owner, repo, sha string) []string {
	return []string{
		"run", "list", "-R", owner + "/" + repo, "--commit", sha,
		"--limit", "100", "--json", "name,event,status,conclusion,databaseId,workflowDatabaseId,url",
	}
}

// combinedStatusArgs builds the gh api invocation for the legacy combined
// commit status - the surface non-Actions CI systems report to.
func combinedStatusArgs(owner, repo, sha string) []string {
	return []string{"api", fmt.Sprintf("repos/%s/%s/commits/%s/status", owner, repo, sha)}
}

// reviewRequestsArgs builds the gh api invocation that lists the PR's pending
// review requests through the REST requested_reviewers endpoint. gh's own
// `pr view --json reviewRequests` is NOT usable here: its JSON exporter emits
// only User and Team nodes and silently drops Bot-typed reviewers, and
// Copilot's reviewer is a Bot - the pre-check would never see it.
func reviewRequestsArgs(prURL string) ([]string, error) {
	owner, repo, number, err := parsePRPath(prURL)
	if err != nil {
		return nil, fmt.Errorf("review requests: %w", err)
	}

	return []string{"api", fmt.Sprintf("repos/%s/%s/pulls/%d/requested_reviewers", owner, repo, number)}, nil
}

// addCopilotReviewerArgs builds the gh api invocation that requests a Copilot
// review via the REST requested_reviewers endpoint. The REST endpoint accepts
// the bot login directly, unlike gh pr edit --add-reviewer which resolves
// through GraphQL's requestReviewsByLogin and cannot resolve the [bot] suffix.
func addCopilotReviewerArgs(prURL string) ([]string, error) {
	owner, repo, number, err := parsePRPath(prURL)
	if err != nil {
		return nil, fmt.Errorf("add copilot reviewer: %w", err)
	}

	return []string{
		"api",
		fmt.Sprintf("repos/%s/%s/pulls/%d/requested_reviewers", owner, repo, number),
		"--method", "POST",
		"-f", "reviewers[]=" + copilotReviewerLogin,
	}, nil
}

// prViewURLArgs builds the gh pr view invocation that resolves the branch's
// PR. No selector: gh's own repo/branch inference from the workspace cwd is
// what makes this a branch probe rather than a lookup by URL or number. state
// rides along with url because gh pr view resolves to the branch's most
// recent PR whether or not it is open - parsePRViewURL reads state to keep
// this a probe for an OPEN PR.
func prViewURLArgs() []string {
	return []string{"pr", "view", "--json", "url,state"}
}

// parsePRPath extracts owner, repo, and PR number from a PR URL. Works for
// github.com and GitHub Enterprise hosts alike, since only the path after the
// host is matched.
func parsePRPath(prURL string) (owner, repo string, number int, err error) {
	m := prPathPattern.FindStringSubmatch(prURL)
	if m == nil {
		return "", "", 0, fmt.Errorf("parse PR path: not a PR URL: %s", prURL)
	}

	number, err = strconv.Atoi(m[3])
	if err != nil {
		return "", "", 0, fmt.Errorf("parse PR path: %w", err)
	}

	return m[1], m[2], number, nil
}

// parseChecks unmarshals gh pr checks --json output into CheckResults.
func parseChecks(out string) ([]orchestrator.CheckResult, error) {
	var checks []orchestrator.CheckResult
	if err := json.Unmarshal([]byte(out), &checks); err != nil {
		return nil, fmt.Errorf("parse checks: %w", err)
	}

	return checks, nil
}

// decodeJSONArrays decodes one or more JSON arrays printed back to back - the
// shape `gh api --paginate` produces for array endpoints - into one slice. A
// single array and empty input both decode cleanly.
func decodeJSONArrays[T any](out string) ([]T, error) {
	var all []T

	dec := json.NewDecoder(strings.NewReader(out))

	var pageNum int

	for {
		pageNum++

		var page []T

		err := dec.Decode(&page)
		if errors.Is(err, io.EOF) {
			return all, nil
		}

		if err != nil {
			return nil, fmt.Errorf("decode JSON array page %d: %w", pageNum, err)
		}

		all = append(all, page...)
	}
}

// paginatedGetArgs builds a gh api GET that follows pagination with 100-item
// pages; --method GET keeps gh from flipping to POST when a field is given.
func paginatedGetArgs(path string) []string {
	return []string{"api", "--method", "GET", "--paginate", "-f", "per_page=100", path}
}

// parseReviewRequests reports whether any reviewer request in a REST
// requested_reviewers document names Copilot's PR review bot. gh's schema
// nests review-request logins differently for users, teams, and bots, so this
// walks the decoded JSON for any "login" value rather than binding to one
// fixed shape.
func parseReviewRequests(out string) (bool, error) {
	var doc any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return false, fmt.Errorf("parse review requests: %w", err)
	}

	return containsCopilotLogin(doc), nil
}

// parseRequestedReviewersResponse reports whether the PR object a
// requested_reviewers POST returns lists Copilot's review bot as a pending
// reviewer. GitHub accepts the POST with a 2xx even when it discards the
// request (a requesting identity without Copilot access, or a reviewer that
// already reviewed), so the response body - not the exit status - is the only
// proof the request took effect. Only requested_reviewers is searched: a
// copilot-ish login elsewhere in the PR object must not confirm, and team
// entries carry no login at all - the bot is always a user-shaped reviewer.
func parseRequestedReviewersResponse(out string) (bool, error) {
	var doc struct {
		RequestedReviewers []any `json:"requested_reviewers"`
	}

	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return false, fmt.Errorf("parse requested reviewers response: %w", err)
	}

	return containsCopilotLogin(doc.RequestedReviewers), nil
}

// containsCopilotLogin recursively searches a decoded JSON value for any
// "login" field whose value contains "copilot", case-insensitively.
func containsCopilotLogin(v any) bool {
	switch val := v.(type) {
	case map[string]any:
		if login, ok := val["login"].(string); ok && strings.Contains(strings.ToLower(login), "copilot") {
			return true
		}

		for _, child := range val {
			if containsCopilotLogin(child) {
				return true
			}
		}
	case []any:
		if slices.ContainsFunc(val, containsCopilotLogin) {
			return true
		}
	}

	return false
}

// ghReview is one entry of a GitHub REST /pulls/{n}/reviews response.
type ghReview struct {
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Body        string `json:"body"`
	CommitID    string `json:"commit_id"`
	SubmittedAt string `json:"submitted_at"`
	ID          int64  `json:"id"`
}

// parseCopilotReview picks the latest review (by submitted_at) authored by
// Copilot's PR review bot out of a GitHub REST /pulls/{n}/reviews array. It
// also returns the review's ID, used to fetch that review's line comments; nil
// and a zero ID when no bot review exists.
func parseCopilotReview(out string) (*orchestrator.CopilotReview, int64, error) {
	reviews, err := decodeJSONArrays[ghReview](out)
	if err != nil {
		return nil, 0, fmt.Errorf("parse copilot review: %w", err)
	}

	var latest *ghReview

	for i := range reviews {
		r := &reviews[i]
		if r.User.Login != copilotReviewerLogin {
			continue
		}

		if latest == nil || r.SubmittedAt > latest.SubmittedAt {
			latest = r
		}
	}

	if latest == nil {
		return nil, 0, nil
	}

	return &orchestrator.CopilotReview{
		CommitID: latest.CommitID,
		Body:     latest.Body,
	}, latest.ID, nil
}

// ghReviewComment is one entry of a GitHub REST /pulls/{n}/comments response.
type ghReviewComment struct {
	ID                  int64  `json:"id"`
	Path                string `json:"path"`
	Body                string `json:"body"`
	PullRequestReviewID int64  `json:"pull_request_review_id"`
}

// parseReviewComments filters a GitHub REST /pulls/{n}/comments array down to
// the line comments attached to one review ID.
func parseReviewComments(out string, reviewID int64) ([]orchestrator.ReviewComment, error) {
	raw, err := decodeJSONArrays[ghReviewComment](out)
	if err != nil {
		return nil, fmt.Errorf("parse review comments: %w", err)
	}

	var comments []orchestrator.ReviewComment

	for _, c := range raw {
		if c.PullRequestReviewID != reviewID {
			continue
		}

		comments = append(comments, orchestrator.ReviewComment{ID: c.ID, Path: c.Path, Body: c.Body})
	}

	return comments, nil
}

// reviewThreadsQuery lists the PR's review threads with the fields the
// thread write-back needs. The first 100 threads with 50 comments each is
// far above any observed Copilot review; a PR beyond that loses write-back
// on the tail, which is acceptable for a best-effort feature.
const reviewThreadsQuery = `query($owner:String!,$repo:String!,$number:Int!){
  repository(owner:$owner,name:$repo){ pullRequest(number:$number){
    reviewThreads(first:100){ nodes{
      id isResolved comments(first:50){ nodes{ databaseId path body } } } } } } }`

// ReviewThreads returns the PR's review-comment threads via GraphQL - the
// only API surface that exposes thread node IDs and resolved state; the REST
// comments endpoint carries neither.
func (p *PRCreator) ReviewThreads(ctx context.Context, prURL string) ([]orchestrator.ReviewThread, error) {
	owner, repo, number, err := parsePRPath(prURL)
	if err != nil {
		return nil, fmt.Errorf("review threads: %w", err)
	}

	out, err := p.runGH(ctx, "", false, "api", "graphql",
		"-f", "query="+reviewThreadsQuery,
		"-f", "owner="+owner, "-f", "repo="+repo, "-F", "number="+strconv.Itoa(number))
	if err != nil {
		return nil, fmt.Errorf("review threads: %w", err)
	}

	threads, err := parseReviewThreads(out)
	if err != nil {
		return nil, fmt.Errorf("review threads: %w", err)
	}

	return threads, nil
}

func parseReviewThreads(out string) ([]orchestrator.ReviewThread, error) {
	var doc struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									DatabaseID int64  `json:"databaseId"`
									Path       string `json:"path"`
									Body       string `json:"body"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}

	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, fmt.Errorf("parse review threads: %w", err)
	}

	var threads []orchestrator.ReviewThread

	for _, n := range doc.Data.Repository.PullRequest.ReviewThreads.Nodes {
		t := orchestrator.ReviewThread{ThreadID: n.ID, IsResolved: n.IsResolved}

		for i, c := range n.Comments.Nodes {
			t.CommentIDs = append(t.CommentIDs, c.DatabaseID)

			if i == 0 {
				t.RootPath = c.Path
				t.RootBody = c.Body
			}
		}

		if len(t.CommentIDs) > 1 {
			t.ReplyCount = len(t.CommentIDs) - 1
		}

		threads = append(threads, t)
	}

	return threads, nil
}

// ReplyToReviewComment posts a reply into the review thread rooted at the
// given REST comment id, carrying the gate's triage verdict to the one place
// a PR reviewer actually looks.
func (p *PRCreator) ReplyToReviewComment(ctx context.Context, prURL string, commentID int64, body string) error {
	owner, repo, number, err := parsePRPath(prURL)
	if err != nil {
		return fmt.Errorf("reply to review comment: %w", err)
	}

	path := fmt.Sprintf("repos/%s/%s/pulls/%d/comments/%d/replies", owner, repo, number, commentID)
	if _, err := p.runGH(ctx, "", false, "api", path, "--method", "POST", "-f", "body="+body); err != nil {
		return fmt.Errorf("reply to review comment: %w", err)
	}

	return nil
}

// resolveThreadMutation marks one review thread resolved. GraphQL is the only
// surface with a resolve concept - REST has none.
const resolveThreadMutation = `mutation($threadId:ID!){
  resolveReviewThread(input:{threadId:$threadId}){ thread{ id } } }`

// ResolveReviewThread marks one review thread resolved by its node id.
func (p *PRCreator) ResolveReviewThread(ctx context.Context, threadID string) error {
	if _, err := p.runGH(ctx, "", false, "api", "graphql",
		"-f", "query="+resolveThreadMutation, "-f", "threadId="+threadID); err != nil {
		return fmt.Errorf("resolve review thread: %w", err)
	}

	return nil
}

// parseRunID extracts the numeric run ID from an Actions job/run link, or ""
// when the link isn't an Actions URL.
func parseRunID(link string) string {
	m := runIDPattern.FindStringSubmatch(link)
	if m == nil {
		return ""
	}

	return m[1]
}

// truncateTail keeps at most max bytes from the end of s, so a capped section
// preserves the most relevant (final) log lines instead of the earliest ones.
func truncateTail(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}

	return s[len(s)-maxLen:]
}

// buildFailureSection formats one failed check's contribution to the failure
// digest: the tail of its run log capped at perCheckCap when logBody is
// non-empty, or "name: description" when there is no run log to show.
func buildFailureSection(name, description, logBody string, perCheckCap int) string {
	if logBody == "" {
		return name + ": " + description
	}

	return name + ":\n" + truncateTail(logBody, perCheckCap)
}

// joinFailureDigest joins failure sections into one digest, truncating the
// last section that would cross totalCap and dropping any sections after it -
// so the digest never exceeds totalCap even when a caller passes uncapped
// sections.
func joinFailureDigest(sections []string, totalCap int) string {
	var (
		kept  []string
		total int
	)

	for _, s := range sections {
		if total >= totalCap {
			break
		}

		if remaining := totalCap - total; len(s) > remaining {
			s = truncateTail(s, remaining)
		}

		kept = append(kept, s)
		total += len(s)
	}

	return strings.Join(kept, "\n\n")
}

// Checks returns the PR's CI results. It reads gh pr checks' rollup first -
// the richest view, covering check runs and commit statuses alike - and,
// when the token cannot read the Checks API (fine-grained PATs on private
// repos; GitHub offers the Checks permission to Apps only), falls back for
// the rest of the run to the Actions-runs and commit-status APIs, which the
// gate's documented PAT permission set (Actions: read, Commit statuses:
// read) covers. Fallback mode loses third-party Checks-API-only
// integrations; GitHub Actions and status-API CI are fully visible.
func (p *PRCreator) Checks(ctx context.Context, prURL string) ([]orchestrator.CheckResult, error) {
	if !p.checksFallback {
		checks, err := p.checksViaRollup(ctx, prURL)
		if err == nil || !checksInaccessiblePattern.MatchString(err.Error()) {
			return checks, err
		}

		p.checksFallback = true

		slog.Info("pr_gates: checks API inaccessible to this token; polling the Actions runs API instead",
			"pr_url", prURL)
	}

	return p.checksViaRuns(ctx, prURL)
}

// checksViaRollup runs gh pr checks and returns the CI check results.
// jsonTolerant is set: gh exits non-zero when any check fails or is pending,
// while still printing the JSON report on stdout.
//
// A head commit with no checks at all is gh's one non-JSON failure mode: it
// exits non-zero and writes "no checks reported" to stderr instead of printing
// an empty array. That is a real, empty answer, not a failed poll, so it is
// translated into an empty result - callers distinguish "this repo has no CI"
// from "the poll failed" only by the error being nil.
func (p *PRCreator) checksViaRollup(ctx context.Context, prURL string) ([]orchestrator.CheckResult, error) {
	out, err := p.runGH(ctx, "", true, checksArgs(prURL)...)
	if err != nil {
		if noChecksReportedPattern.MatchString(err.Error()) {
			return nil, nil
		}

		return nil, fmt.Errorf("gh pr checks: %w", err)
	}

	checks, err := parseChecks(out)
	if err != nil {
		return nil, fmt.Errorf("gh pr checks: %w", err)
	}

	return checks, nil
}

// checksViaRuns polls CI without the Checks API: workflow runs for the PR's
// current head SHA plus legacy commit statuses, mapped into the same bucket
// vocabulary classifyChecks reads. The head SHA is re-resolved every poll
// because fix rounds push new commits. Both sources empty means the same
// thing as gh's "no checks reported": an empty result with a nil error.
func (p *PRCreator) checksViaRuns(ctx context.Context, prURL string) ([]orchestrator.CheckResult, error) {
	owner, repo, _, err := parsePRPath(prURL)
	if err != nil {
		return nil, fmt.Errorf("checks via runs: %w", err)
	}

	sha, err := p.HeadSHA(ctx, prURL)
	if err != nil {
		return nil, fmt.Errorf("checks via runs: %w", err)
	}

	runsOut, err := p.runGH(ctx, "", false, runListArgs(owner, repo, sha)...)
	if err != nil {
		// A gh without the run list flags, or a token without Actions: read,
		// fails this way on every poll: tell the gate to stop waiting.
		if unknownFlagPattern.MatchString(err.Error()) || checksInaccessiblePattern.MatchString(err.Error()) {
			return nil, &orchestrator.PermanentPollError{Err: err.Error()}
		}

		return nil, fmt.Errorf("gh run list: %w", err)
	}

	checks, err := parseRunList(runsOut)
	if err != nil {
		return nil, fmt.Errorf("gh run list: %w", err)
	}

	statusOut, err := p.runGH(ctx, "", false, combinedStatusArgs(owner, repo, sha)...)
	if err != nil {
		// Commit statuses: read is missing and there is nothing left to fall
		// back to. Say so now rather than on every poll until the deadline - and
		// never let the Actions runs alone stand in for checks the token cannot
		// see, which would read a status-only CI as "no CI".
		if checksInaccessiblePattern.MatchString(err.Error()) {
			return nil, &orchestrator.PermanentPollError{Err: err.Error()}
		}

		return nil, fmt.Errorf("gh api commit status: %w", err)
	}

	statuses, err := parseCombinedStatus(statusOut)
	if err != nil {
		return nil, fmt.Errorf("gh api commit status: %w", err)
	}

	return append(checks, statuses...), nil
}

// parseRunList maps gh run list --json output into CheckResults, keeping
// only the newest run (highest databaseId) per (workflow, event), where
// workflow identity is the workflow's database id, not its display name:
// a rerun supersedes its predecessor, the same workflow triggered by
// different events (push and pull_request) stays two results, and two
// distinct workflow files that happen to share a display name stay
// distinct - same-named workflow files never mask each other's runs.
func parseRunList(out string) ([]orchestrator.CheckResult, error) {
	var runs []struct {
		Name               string `json:"name"`
		Event              string `json:"event"`
		Status             string `json:"status"`
		Conclusion         string `json:"conclusion"`
		DatabaseID         int64  `json:"databaseId"`
		WorkflowDatabaseID int64  `json:"workflowDatabaseId"`
		URL                string `json:"url"`
	}

	if err := json.Unmarshal([]byte(out), &runs); err != nil {
		return nil, fmt.Errorf("parse run list: %w", err)
	}

	type key struct {
		workflowID int64
		event      string
	}

	newest := map[key]int{}

	for i, r := range runs {
		k := key{r.WorkflowDatabaseID, r.Event}
		if j, ok := newest[k]; !ok || r.DatabaseID > runs[j].DatabaseID {
			newest[k] = i
		}
	}

	checks := make([]orchestrator.CheckResult, 0, len(newest))

	for _, i := range newest {
		r := runs[i]
		checks = append(checks, orchestrator.CheckResult{
			Name:   r.Name,
			Bucket: runBucket(r.Status, r.Conclusion),
			Link:   r.URL,
		})
	}

	return checks, nil
}

// runBucket maps an Actions run's status/conclusion pair onto gh's
// statusCheckRollup bucket vocabulary, mirroring gh's own mapping so
// classifyChecks treats both poll paths identically. Anything unsettled or
// unrecognized is pending - the orchestrator's conservative default.
func runBucket(status, conclusion string) string {
	if status != "completed" {
		return "pending"
	}

	switch conclusion {
	case "success":
		return "pass"
	case "failure", "timed_out":
		return "fail"
	case "cancelled":
		return "cancel"
	case "skipped", "neutral":
		return "skipping"
	default: // action_required, stale, future states
		return "pending"
	}
}

// parseCombinedStatus maps the legacy combined-status document's contexts
// into CheckResults. GitHub Actions never reports here; these are
// third-party status-API integrations.
func parseCombinedStatus(out string) ([]orchestrator.CheckResult, error) {
	var doc struct {
		Statuses []struct {
			Context     string `json:"context"`
			State       string `json:"state"`
			TargetURL   string `json:"target_url"`
			Description string `json:"description"`
		} `json:"statuses"`
	}

	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, fmt.Errorf("parse combined status: %w", err)
	}

	checks := make([]orchestrator.CheckResult, 0, len(doc.Statuses))

	for _, s := range doc.Statuses {
		checks = append(checks, orchestrator.CheckResult{
			Name:        s.Context,
			Bucket:      statusBucket(s.State),
			Link:        s.TargetURL,
			Description: s.Description,
		})
	}

	return checks, nil
}

// statusBucket maps a commit-status state onto the bucket vocabulary.
func statusBucket(state string) string {
	switch state {
	case "success":
		return "pass"
	case "failure", "error":
		return "fail"
	default: // pending, future states
		return "pending"
	}
}

// HeadSHA returns the PR's current head commit SHA.
func (p *PRCreator) HeadSHA(ctx context.Context, prURL string) (string, error) {
	out, err := p.runGH(ctx, "", false, headSHAArgs(prURL)...)
	if err != nil {
		return "", fmt.Errorf("gh pr view head sha: %w", err)
	}

	var doc struct {
		HeadRefOid string `json:"headRefOid"`
	}

	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return "", fmt.Errorf("gh pr view head sha: parse: %w", err)
	}

	return doc.HeadRefOid, nil
}

// CopilotRequested reports whether Copilot's PR review bot is among the
// pending review requests (REST requested_reviewers: users[] + teams[]).
func (p *PRCreator) CopilotRequested(ctx context.Context, prURL string) (bool, error) {
	args, err := reviewRequestsArgs(prURL)
	if err != nil {
		return false, fmt.Errorf("gh api review requests: %w", err)
	}

	out, err := p.runGH(ctx, "", false, args...)
	if err != nil {
		return false, fmt.Errorf("gh api review requests: %w", err)
	}

	requested, err := parseReviewRequests(out)
	if err != nil {
		return false, fmt.Errorf("gh api review requests: %w", err)
	}

	return requested, nil
}

// RequestCopilotReview adds Copilot's PR review bot as a reviewer and reports
// whether the request actually took: confirmed is true only when the POST's
// response body lists the bot as a requested reviewer. GitHub is known to
// accept the request with a 2xx and silently discard it. The error, when
// non-nil, is returned verbatim - the orchestrator logs it on the card.
func (p *PRCreator) RequestCopilotReview(ctx context.Context, prURL string) (bool, error) {
	args, err := addCopilotReviewerArgs(prURL)
	if err != nil {
		return false, fmt.Errorf("gh api request copilot reviewer: %w", err)
	}

	out, err := p.runGH(ctx, "", false, args...)
	if err != nil {
		return false, fmt.Errorf("gh api request copilot reviewer: %w", err)
	}

	confirmed, perr := parseRequestedReviewersResponse(out)
	if perr != nil {
		// A 2xx whose body does not parse may still be a request that took;
		// report unconfirmed rather than failing the gate over parsing.
		slog.Warn("pr: copilot reviewer response unparseable; treating the request as unconfirmed",
			"pr_url", prURL, "error", perr)

		return false, nil
	}

	return confirmed, nil
}

// CopilotReview returns the latest completed Copilot review on the PR,
// including its line comments, or nil when Copilot has not reviewed it yet.
func (p *PRCreator) CopilotReview(ctx context.Context, prURL string) (*orchestrator.CopilotReview, error) {
	owner, repo, number, err := parsePRPath(prURL)
	if err != nil {
		return nil, fmt.Errorf("copilot review: %w", err)
	}

	base := "repos/" + owner + "/" + repo + "/pulls/" + strconv.Itoa(number)

	out, err := p.runGH(ctx, "", false, paginatedGetArgs(base+"/reviews")...)
	if err != nil {
		return nil, fmt.Errorf("copilot review: %w", err)
	}

	review, reviewID, err := parseCopilotReview(out)
	if err != nil {
		return nil, fmt.Errorf("copilot review: %w", err)
	}

	if review == nil {
		return nil, nil
	}

	commentsOut, err := p.runGH(ctx, "", false,
		paginatedGetArgs(base+"/reviews/"+strconv.FormatInt(reviewID, 10)+"/comments")...)
	if err != nil {
		return nil, fmt.Errorf("copilot review comments: %w", err)
	}

	comments, err := parseReviewComments(commentsOut, reviewID)
	if err != nil {
		return nil, fmt.Errorf("copilot review comments: %w", err)
	}

	review.Comments = comments

	return review, nil
}

// FailureLogs builds a digest of failure output for the given failed checks:
// the tail of gh run view --log-failed for checks with an Actions run link,
// or "name: description" for checks with no run link, capped per check and in
// total so a noisy CI run cannot blow the prompt budget. prURL is part of the
// PRGates interface but unused here - each check's own Link carries the
// Actions run to read. A per-check log fetch failure (transient gh/API error,
// rate limit, expired logs) degrades that check to the no-run-link fallback
// instead of aborting the whole digest - one bad fetch must not discard the
// sections already built for the other failed checks, exactly when the
// pr_gates fix loop most needs whatever failure context is available.
func (p *PRCreator) FailureLogs(ctx context.Context, _ string, failed []orchestrator.CheckResult) (string, error) {
	sections := make([]string, 0, len(failed))

	for _, check := range failed {
		description := check.Description
		logBody := ""

		if runID := parseRunID(check.Link); runID != "" {
			out, err := p.runGH(ctx, "", false, "run", "view", runID, "--log-failed")
			if err != nil {
				description += " (log fetch failed: " + err.Error() + ")"
			} else {
				logBody = out
			}
		}

		sections = append(sections, buildFailureSection(check.Name, description, logBody, failureLogPerCheckCap))
	}

	return joinFailureDigest(sections, failureLogTotalCap), nil
}

// FindPRURL probes for an open PR on the workspace's current branch.
func (p *PRCreator) FindPRURL(ctx context.Context) (string, error) {
	out, err := p.runGH(ctx, "", false, prViewURLArgs()...)
	if err != nil {
		if noPRForBranchPattern.MatchString(err.Error()) {
			return "", nil
		}

		return "", fmt.Errorf("gh pr view: %w", err)
	}

	return parsePRViewURL(out)
}
