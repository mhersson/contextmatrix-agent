package worker

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSecrets writes a secrets env file carrying CM_GIT_TOKEN and returns its
// path, so PRCreator resolves GH_TOKEN from it at call time.
func writeSecrets(t *testing.T, token string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "env")
	require.NoError(t, os.WriteFile(path, []byte("CM_GIT_TOKEN="+token+"\n"), 0o600))

	return path
}

// TestPRCreatorCommand verifies the gh invocation the PRCreator builds: the
// argv shape, the workspace dir, the body on stdin, and an env that carries
// GH_TOKEN (read fresh from the secrets file) over the scrubbed allowlist and
// nothing else.
func TestPRCreatorCommand(t *testing.T) {
	t.Parallel()

	pc := NewPRCreator("/work/space", writeSecrets(t, "ghs_secrettoken"), "", "")

	cmd, err := pc.buildCmd(t.Context(), "Add the widget", "the body\nwith detail", "main", "cm/cmx-001")
	require.NoError(t, err)

	// argv: gh pr create --title <t> --body-file - --base <b> --head <h>
	require.GreaterOrEqual(t, len(cmd.Args), 9)
	assert.Equal(t, "gh", cmd.Args[0])
	assert.Equal(t, []string{
		"gh", "pr", "create",
		"--title", "Add the widget",
		"--body-file", "-",
		"--base", "main",
		"--head", "cm/cmx-001",
	}, cmd.Args)

	// Runs in the workspace.
	assert.Equal(t, "/work/space", cmd.Dir)

	// Body is fed on stdin.
	require.NotNil(t, cmd.Stdin)
	body, readErr := io.ReadAll(cmd.Stdin)
	require.NoError(t, readErr)
	assert.Equal(t, "the body\nwith detail", string(body))

	// Env carries GH_TOKEN exactly once and the scrubbed allowlist - no secrets
	// leak from the parent process.
	var ghToken string

	tokenCount := 0

	for _, kv := range cmd.Env {
		if after, ok := strings.CutPrefix(kv, "GH_TOKEN="); ok {
			ghToken = after
			tokenCount++
		}
	}

	assert.Equal(t, 1, tokenCount, "GH_TOKEN present exactly once")
	assert.Equal(t, "ghs_secrettoken", ghToken)

	// The env is the scrub helper + GH_TOKEN, nothing more: every non-GH_TOKEN
	// entry must be an allowlisted key (assert against the env-scrub helper).
	want := tools.ScrubbedEnv([]string{"GH_TOKEN=ghs_secrettoken"})
	assert.ElementsMatch(t, want, cmd.Env, "env must be exactly ScrubbedEnv + GH_TOKEN")
}

// TestPRCreatorTokenRotation pins that the PR path resolves GH_TOKEN fresh from
// the secrets file at call time: after the file is rewritten, the next buildCmd
// carries the NEW token - an end-of-run PR uses the current token, not one
// cached at startup.
func TestPRCreatorTokenRotation(t *testing.T) {
	t.Parallel()

	secretsPath := writeSecrets(t, "ghs_initial")
	pc := NewPRCreator("/work/space", secretsPath, "", "")

	first, err := pc.buildCmd(t.Context(), "t", "b", "main", "cm/x-1")
	require.NoError(t, err)
	assert.Contains(t, first.Env, "GH_TOKEN=ghs_initial")

	// Rotate the secrets file the way the host refresh loop does.
	require.NoError(t, os.WriteFile(secretsPath, []byte("CM_GIT_TOKEN=ghs_rotated\n"), 0o600))

	second, err := pc.buildCmd(t.Context(), "t", "b", "main", "cm/x-1")
	require.NoError(t, err)
	assert.Contains(t, second.Env, "GH_TOKEN=ghs_rotated")
	assert.NotContains(t, strings.Join(second.Env, "\n"), "ghs_initial")
}

// TestPRCreatorMissingSecretsFileErrors pins the fail-loud path: a configured
// but unreadable secrets file must fail buildCmd with a clear error instead of
// producing an unauthenticated gh command that fails generically later.
func TestPRCreatorMissingSecretsFileErrors(t *testing.T) {
	t.Parallel()

	pc := NewPRCreator("/work/space", filepath.Join(t.TempDir(), "missing-env"), "", "")

	cmd, err := pc.buildCmd(t.Context(), "t", "b", "main", "cm/x-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read git token")
	assert.Nil(t, cmd)
}

// TestPRCreatorEmptySecretsPathBuildsUnauthenticated pins the public-remote
// path: no secrets file configured means no GH_TOKEN and no error.
func TestPRCreatorEmptySecretsPathBuildsUnauthenticated(t *testing.T) {
	t.Parallel()

	pc := NewPRCreator("/work/space", "", "", "")

	cmd, err := pc.buildCmd(t.Context(), "t", "b", "main", "cm/x-1")
	require.NoError(t, err)
	assert.NotContains(t, strings.Join(cmd.Env, "\n"), "GH_TOKEN")
}

// TestPRCreatorCACert verifies the gh command's env carries the extra-CA vars
// only when a cert path is configured.
func TestPRCreatorCACert(t *testing.T) {
	t.Parallel()

	t.Run("cert set injects SSL_CERT_FILE and GH_CA_BUNDLE", func(t *testing.T) {
		t.Parallel()

		pc := NewPRCreator("/work/space", writeSecrets(t, "ghs_tok"), "/run/cm-ca/ca.crt", "")
		cmd, err := pc.buildCmd(t.Context(), "t", "b", "main", "cm/x-1")
		require.NoError(t, err)

		assert.Contains(t, cmd.Env, "SSL_CERT_FILE=/run/cm-ca/ca.crt")
		assert.Contains(t, cmd.Env, "GH_CA_BUNDLE=/run/cm-ca/ca.crt")
		assert.Contains(t, cmd.Env, "GH_TOKEN=ghs_tok")
	})

	t.Run("no cert omits the CA vars", func(t *testing.T) {
		t.Parallel()

		pc := NewPRCreator("/work/space", writeSecrets(t, "ghs_tok"), "", "")
		cmd, err := pc.buildCmd(t.Context(), "t", "b", "main", "cm/x-1")
		require.NoError(t, err)

		joined := strings.Join(cmd.Env, "\n")
		assert.NotContains(t, joined, "SSL_CERT_FILE")
		assert.NotContains(t, joined, "GH_CA_BUNDLE")
	})
}

// TestPRCreatorGHHost verifies GH_HOST is exported for a GitHub Enterprise repo
// URL (gh cannot infer such a host from the git remote) and omitted when the
// repo URL yields no host, leaving gh on its github.com default.
func TestPRCreatorGHHost(t *testing.T) {
	t.Parallel()

	t.Run("enterprise host sets GH_HOST", func(t *testing.T) {
		t.Parallel()

		pc := NewPRCreator("/work/space", writeSecrets(t, "ghs_tok"), "", "https://acme.ghe.com/org/repo.git")
		cmd, err := pc.buildCmd(t.Context(), "t", "b", "main", "cm/x-1")
		require.NoError(t, err)

		assert.Contains(t, cmd.Env, "GH_HOST=acme.ghe.com")
	})

	t.Run("empty repo URL omits GH_HOST", func(t *testing.T) {
		t.Parallel()

		pc := NewPRCreator("/work/space", writeSecrets(t, "ghs_tok"), "", "")
		cmd, err := pc.buildCmd(t.Context(), "t", "b", "main", "cm/x-1")
		require.NoError(t, err)

		assert.NotContains(t, strings.Join(cmd.Env, "\n"), "GH_HOST")
	})
}

// TestParsePRURL pulls the first http(s) URL gh prints to stdout.
func TestParsePRURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
		want string
	}{
		{"plain url", "https://github.com/org/repo/pull/42\n", "https://github.com/org/repo/pull/42"},
		{"with preamble", "Creating pull request...\nhttps://github.com/org/repo/pull/7\n", "https://github.com/org/repo/pull/7"},
		{"no url", "some unexpected output", ""},
		{"trailing space", "  https://github.com/org/repo/pull/9  \n", "https://github.com/org/repo/pull/9"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, parsePRURL(tc.out))
		})
	}
}

// TestGatesArgBuilders pins the exact argv for each gh gates invocation.
func TestGatesArgBuilders(t *testing.T) {
	t.Parallel()

	prURL := "https://github.com/org/repo/pull/7"

	assert.Equal(t, []string{"pr", "checks", prURL, "--json", "name,bucket,link,description"}, checksArgs(prURL))
	assert.Equal(t, []string{"pr", "view", prURL, "--json", "headRefOid"}, headSHAArgs(prURL))
	assert.Equal(t, []string{"pr", "view", prURL, "--json", "reviewRequests"}, reviewRequestsArgs(prURL))

	// addCopilotReviewerArgs now issues a REST API request to the
	// requested_reviewers endpoint, bypassing gh pr edit's GraphQL login
	// resolution which cannot handle the [bot] suffix. The reviewers field uses
	// the [] array suffix because the REST endpoint expects an array.
	args, err := addCopilotReviewerArgs(prURL)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"api", "repos/org/repo/pulls/7/requested_reviewers",
		"--method", "POST",
		"-f", "reviewers[]=" + copilotReviewerLogin,
	}, args)

	// A GitHub Enterprise URL also derives the correct path.
	gheURL := "https://acme.ghe.com/team/project/pull/42"
	args, err = addCopilotReviewerArgs(gheURL)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"api", "repos/team/project/pulls/42/requested_reviewers",
		"--method", "POST",
		"-f", "reviewers[]=" + copilotReviewerLogin,
	}, args)

	// An invalid URL returns an error.
	_, err = addCopilotReviewerArgs("https://github.com/org/repo/issues/7")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add copilot reviewer")

	assert.Equal(t, []string{
		"run", "list", "-R", "org/repo", "--commit", "abc123",
		"--limit", "100", "--json", "name,event,status,conclusion,databaseId,workflowDatabaseId,url",
	}, runListArgs("org", "repo", "abc123"))
	assert.Equal(t, []string{"api", "repos/org/repo/commits/abc123/status"}, combinedStatusArgs("org", "repo", "abc123"))
}

// TestParsePRPath pulls owner, repo, and PR number out of a PR URL, on both
// github.com and a GitHub Enterprise host, and errors on a non-PR URL.
func TestParsePRPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantNum   int
		wantErr   bool
	}{
		{"github.com", "https://github.com/org/repo/pull/7", "org", "repo", 7, false},
		{"GHE host", "https://acme.ghe.com/org/repo/pull/42", "org", "repo", 42, false},
		{"not a PR URL", "https://github.com/org/repo/issues/7", "", "", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			owner, repo, num, err := parsePRPath(tc.url)
			if tc.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantOwner, owner)
			assert.Equal(t, tc.wantRepo, repo)
			assert.Equal(t, tc.wantNum, num)
		})
	}
}

// TestParseChecks unmarshals a gh pr checks --json fixture: one passing check,
// one failing check with an Actions link, one pending check.
func TestParseChecks(t *testing.T) {
	t.Parallel()

	fixture := `[
		{"name":"build","bucket":"pass","link":"https://github.com/org/repo/actions/runs/111/job/1","description":""},
		{"name":"test","bucket":"fail","link":"https://github.com/org/repo/actions/runs/222/job/2","description":"exit status 1"},
		{"name":"lint","bucket":"pending","link":"","description":""}
	]`

	got, err := parseChecks(fixture)
	require.NoError(t, err)
	require.Len(t, got, 3)

	assert.Equal(t, orchestrator.CheckResult{
		Name: "build", Bucket: "pass",
		Link: "https://github.com/org/repo/actions/runs/111/job/1", Description: "",
	}, got[0])
	assert.Equal(t, orchestrator.CheckResult{
		Name: "test", Bucket: "fail",
		Link: "https://github.com/org/repo/actions/runs/222/job/2", Description: "exit status 1",
	}, got[1])
	assert.Equal(t, orchestrator.CheckResult{
		Name: "lint", Bucket: "pending", Link: "", Description: "",
	}, got[2])
}

// TestRunBucket maps an Actions run's status/conclusion pair onto gh's
// statusCheckRollup bucket vocabulary.
func TestRunBucket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status, conclusion, want string
	}{
		{"queued", "", "pending"},
		{"in_progress", "", "pending"},
		{"waiting", "", "pending"},
		{"completed", "success", "pass"},
		{"completed", "failure", "fail"},
		{"completed", "timed_out", "fail"},
		{"completed", "cancelled", "cancel"},
		{"completed", "skipped", "skipping"},
		{"completed", "neutral", "skipping"},
		{"completed", "action_required", "pending"},
		{"completed", "stale", "pending"},
		{"completed", "somethingnew", "pending"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, runBucket(tt.status, tt.conclusion),
			"status=%s conclusion=%s", tt.status, tt.conclusion)
	}
}

// TestParseRunListDedupesReruns pins parseRunList's newest-run-wins-per
// (workflow, event) dedupe: a rerun of the same workflow file supersedes its
// predecessor, the same workflow triggered by different events stays two
// results, and two distinct workflow files that merely share a display name
// stay two results too - the failing one must not be masked by the
// same-named passing one.
func TestParseRunListDedupesReruns(t *testing.T) {
	t.Parallel()

	out := `[
	 {"name":"ci","event":"pull_request","status":"completed","conclusion":"failure","databaseId":1,"workflowDatabaseId":100,"url":"https://github.com/o/r/actions/runs/1"},
	 {"name":"ci","event":"pull_request","status":"completed","conclusion":"success","databaseId":2,"workflowDatabaseId":100,"url":"https://github.com/o/r/actions/runs/2"},
	 {"name":"ci","event":"push","status":"in_progress","conclusion":"","databaseId":3,"workflowDatabaseId":100,"url":"https://github.com/o/r/actions/runs/3"},
	 {"name":"ci","event":"pull_request","status":"completed","conclusion":"failure","databaseId":4,"workflowDatabaseId":500,"url":"https://github.com/o/r/actions/runs/4"},
	 {"name":"ci","event":"pull_request","status":"completed","conclusion":"success","databaseId":5,"workflowDatabaseId":600,"url":"https://github.com/o/r/actions/runs/5"}
	]`
	checks, err := parseRunList(out)
	require.NoError(t, err)
	require.Len(t, checks, 4,
		"newest run wins per (workflow, event); distinct events and distinct same-named workflows all kept")
	// order-independent asserts: find by link
	byLink := map[string]orchestrator.CheckResult{}
	for _, c := range checks {
		byLink[c.Link] = c
	}

	assert.Equal(t, "pass", byLink["https://github.com/o/r/actions/runs/2"].Bucket, "rerun collapse keeps the newer run")
	assert.Equal(t, "pending", byLink["https://github.com/o/r/actions/runs/3"].Bucket, "different event stays separate")
	assert.Equal(t, "fail", byLink["https://github.com/o/r/actions/runs/4"].Bucket,
		"a same-named workflow's red run must not vanish behind another workflow's green run")
	assert.Equal(t, "pass", byLink["https://github.com/o/r/actions/runs/5"].Bucket, "the other same-named workflow's run also survives")
}

// TestParseCombinedStatus unmarshals the legacy combined-status document's
// contexts into CheckResults.
func TestParseCombinedStatus(t *testing.T) {
	t.Parallel()

	out := `{"state":"failure","statuses":[
	 {"context":"jenkins/build","state":"success","target_url":"https://ci.example/1","description":"ok"},
	 {"context":"jenkins/deploy","state":"error","target_url":"https://ci.example/2","description":"boom"},
	 {"context":"jenkins/lint","state":"pending","target_url":"","description":""}
	]}`
	checks, err := parseCombinedStatus(out)
	require.NoError(t, err)
	require.Len(t, checks, 3)
	assert.Equal(t, orchestrator.CheckResult{Name: "jenkins/build", Bucket: "pass", Link: "https://ci.example/1", Description: "ok"}, checks[0])
	assert.Equal(t, "fail", checks[1].Bucket)
	assert.Equal(t, "pending", checks[2].Bucket)
}

// TestParseReviewRequests detects a Copilot reviewer request in a gh pr view
// --json reviewRequests document, case-insensitively and regardless of where
// the login appears in the document shape.
func TestParseReviewRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		json string
		want bool
	}{
		{"copilot bot requested", `{"reviewRequests":[{"login":"copilot-pull-request-reviewer[bot]"}]}`, true},
		{"human requested", `{"reviewRequests":[{"login":"alice"}]}`, false},
		{"bare copilot login", `{"login":"Copilot"}`, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseReviewRequests(tc.json)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParseCopilotReview picks the Copilot bot's review out of a GitHub REST
// /pulls/{n}/reviews fixture, and returns nil when no bot review exists.
func TestParseCopilotReview(t *testing.T) {
	t.Parallel()

	t.Run("picks the bot review", func(t *testing.T) {
		t.Parallel()

		fixture := `[
			{"id":1,"user":{"login":"alice"},"body":"LGTM","commit_id":"aaa111","submitted_at":"2026-08-10T10:00:00Z"},
			{"id":2,"user":{"login":"copilot-pull-request-reviewer[bot]"},"body":"Found an issue","commit_id":"bbb222","submitted_at":"2026-08-10T11:00:00Z"}
		]`

		review, reviewID, err := parseCopilotReview(fixture)
		require.NoError(t, err)
		require.NotNil(t, review)
		assert.Equal(t, "bbb222", review.CommitID)
		assert.Equal(t, "Found an issue", review.Body)
		assert.Equal(t, int64(2), reviewID)
	})

	t.Run("no bot review returns nil", func(t *testing.T) {
		t.Parallel()

		fixture := `[{"id":1,"user":{"login":"alice"},"body":"LGTM","commit_id":"aaa111","submitted_at":"2026-08-10T10:00:00Z"}]`

		review, _, err := parseCopilotReview(fixture)
		require.NoError(t, err)
		assert.Nil(t, review)
	})
}

// TestParseReviewComments filters a GitHub REST /pulls/{n}/comments fixture
// down to the comments attached to one review ID.
func TestParseReviewComments(t *testing.T) {
	t.Parallel()

	fixture := `[
		{"path":"main.go","body":"nit: rename this","pull_request_review_id":2},
		{"path":"README.md","body":"unrelated comment","pull_request_review_id":9}
	]`

	got, err := parseReviewComments(fixture, 2)
	require.NoError(t, err)
	assert.Equal(t, []orchestrator.ReviewComment{{Path: "main.go", Body: "nit: rename this"}}, got)
}

// TestParseRunID pulls the numeric run ID out of an Actions job link, and
// returns "" for a non-Actions link.
func TestParseRunID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		link string
		want string
	}{
		{"actions run link", "https://github.com/org/repo/actions/runs/123456/job/789", "123456"},
		{"non-actions link", "https://github.com/org/repo/pull/7", ""},
		{"empty link", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, parseRunID(tc.link))
		})
	}
}

// TestGHSucceeded pins the jsonTolerant decision runGH delegates to: a nil
// error always succeeds; a non-nil error only succeeds when jsonTolerant is
// set and stdout parses as JSON - the gh pr checks exit-1/exit-8 case.
func TestGHSucceeded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          error
		stdout       string
		jsonTolerant bool
		want         bool
	}{
		{"nil error always succeeds", nil, "not json", false, true},
		{"error, not tolerant, fails", errors.New("exit status 1"), `[{"a":1}]`, false, false},
		{"error, tolerant, valid json succeeds", errors.New("exit status 1"), `[{"a":1}]`, true, true},
		{"error, tolerant, invalid json fails", errors.New("exit status 1"), "not json", true, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, ghSucceeded(tc.err, tc.stdout, tc.jsonTolerant))
		})
	}
}

// TestTruncateTail keeps the tail of a string, since the most relevant log
// lines are usually the last ones.
func TestTruncateTail(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "hello", truncateTail("hello", 10))
	assert.Equal(t, "llo", truncateTail("hello", 3))
	assert.Empty(t, truncateTail("hello", 0))
}

// TestBuildFailureSection formats a run-log body under the per-check cap, or
// falls back to "name: description" when there is no log body.
func TestBuildFailureSection(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "lint: syntax error", buildFailureSection("lint", "syntax error", "", 100))

	longLog := strings.Repeat("x", 20)
	got := buildFailureSection("test", "", longLog, 10)
	assert.Equal(t, "test:\n"+strings.Repeat("x", 10), got)
}

// TestJoinFailureDigestCaps pins the 24KB total cap: sections are kept and
// truncated in order until the cap is hit, then dropped.
func TestJoinFailureDigestCaps(t *testing.T) {
	t.Parallel()

	sections := []string{strings.Repeat("a", 5000), strings.Repeat("b", 5000), strings.Repeat("c", 5000)}

	got := joinFailureDigest(sections, 8000)

	assert.LessOrEqual(t, len(got), 8000+len("\n\n"))
	assert.Contains(t, got, strings.Repeat("a", 5000))
	assert.NotContains(t, got, strings.Repeat("c", 5000))
}

// TestGatesCmdEnv verifies ghCmd builds a gh invocation with the same
// auth/CA/host env as buildCmd - GH_TOKEN read fresh from the secrets file
// over the scrubbed allowlist - and runs it in the workspace.
func TestGatesCmdEnv(t *testing.T) {
	t.Parallel()

	pc := NewPRCreator("/work/space", writeSecrets(t, "ghs_secrettoken"), "", "")

	prURL := "https://github.com/org/repo/pull/7"

	cmd, err := pc.ghCmd(t.Context(), "", "pr", "checks", prURL)
	require.NoError(t, err)

	assert.Equal(t, []string{"gh", "pr", "checks", prURL}, cmd.Args)
	assert.Equal(t, "/work/space", cmd.Dir)
	assert.Nil(t, cmd.Stdin)

	var ghToken string

	tokenCount := 0

	for _, kv := range cmd.Env {
		if after, ok := strings.CutPrefix(kv, "GH_TOKEN="); ok {
			ghToken = after
			tokenCount++
		}
	}

	assert.Equal(t, 1, tokenCount, "GH_TOKEN present exactly once")
	assert.Equal(t, "ghs_secrettoken", ghToken)

	want := tools.ScrubbedEnv([]string{"GH_TOKEN=ghs_secrettoken"})
	assert.ElementsMatch(t, want, cmd.Env, "env must be exactly ScrubbedEnv + GH_TOKEN")
}

// TestGatesCmdEnvStdin verifies ghCmd feeds a non-empty stdin argument to the
// command, mirroring buildCmd's body-on-stdin behavior.
func TestGatesCmdEnvStdin(t *testing.T) {
	t.Parallel()

	pc := NewPRCreator("/work/space", "", "", "")

	cmd, err := pc.ghCmd(t.Context(), "review body", "pr", "review", "--comment")
	require.NoError(t, err)
	require.NotNil(t, cmd.Stdin)

	body, err := io.ReadAll(cmd.Stdin)
	require.NoError(t, err)
	assert.Equal(t, "review body", string(body))
}

// stubGH writes an executable fake `gh` into a fresh bin dir with the given
// shell script body and points PATH at it, so gh-invoking methods truly shell
// out to a controlled fake instead of the real CLI. Uses t.Setenv, so callers
// cannot run in parallel.
func stubGH(t *testing.T, script string) {
	t.Helper()

	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\n"+script), 0o755))
	t.Setenv("PATH", bin)
}

// TestFailureLogsDegradesOnLogFetchError pins the fix for a per-check log
// fetch failure: it must not discard the digest sections already built for
// the other failed checks. One check's "gh run view --log-failed" errors,
// the other succeeds; the resulting digest must contain both - the errored
// check in its no-run-link fallback form, noting the fetch failure.
func TestFailureLogsDegradesOnLogFetchError(t *testing.T) {
	stubGH(t, `
if [ "$1" = "run" ] && [ "$2" = "view" ]; then
	if [ "$3" = "111" ]; then
		echo "transient failure" 1>&2
		exit 1
	fi
	echo "log line for run $3"
	exit 0
fi
exit 1
`)

	pc := NewPRCreator(t.TempDir(), "", "", "")

	failed := []orchestrator.CheckResult{
		{
			Name: "flaky-check", Bucket: "fail",
			Link: "https://github.com/org/repo/actions/runs/111/job/1", Description: "exit status 1",
		},
		{
			Name: "real-check", Bucket: "fail",
			Link: "https://github.com/org/repo/actions/runs/222/job/2", Description: "exit status 2",
		},
	}

	digest, err := pc.FailureLogs(t.Context(), "https://github.com/org/repo/pull/1", failed)
	require.NoError(t, err)

	assert.Contains(t, digest, "flaky-check")
	assert.Contains(t, digest, "log fetch failed")
	assert.Contains(t, digest, "real-check")
	assert.Contains(t, digest, "log line for run 222")
}

// TestChecksTreatsNoChecksReportedAsEmpty pins the seam translation for a repo
// without CI: gh pr checks exits non-zero and prints "no checks reported" on
// stderr rather than an empty JSON array, and Checks must render that as an
// explicit empty result. The CI gate's "this repo has no CI" conclusion is
// reachable only through this translation - an error there just means "keep
// waiting", so without it a CI-less repo would wait out the full gate.
func TestChecksTreatsNoChecksReportedAsEmpty(t *testing.T) {
	stubGH(t, `
echo "no checks reported on the 'cm/card-1' branch" 1>&2
exit 1
`)

	pc := NewPRCreator(t.TempDir(), "", "", "")

	checks, err := pc.Checks(t.Context(), "https://github.com/org/repo/pull/1")
	require.NoError(t, err, "no checks reported is an empty result, not a failure")
	assert.Empty(t, checks)
}

// TestChecksReportsRealFailures: any other gh failure stays an error - the gate
// must not read a broken gh (auth, network, rate limit) as "this repo has no CI"
// and pass a PR whose checks it never saw.
func TestChecksReportsRealFailures(t *testing.T) {
	stubGH(t, `
echo "gh: authentication required" 1>&2
exit 4
`)

	pc := NewPRCreator(t.TempDir(), "", "", "")

	_, err := pc.Checks(t.Context(), "https://github.com/org/repo/pull/1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authentication required")
}

// TestChecksFallsBackOnInaccessibleChecksAPI pins the sticky fallback: the
// first poll hits gh pr checks, gets the fine-grained-PAT refusal, and the
// same call transparently answers from gh run list + the commit-status API;
// the second poll never invokes gh pr checks again.
func TestChecksFallsBackOnInaccessibleChecksAPI(t *testing.T) {
	stubGH(t, `
echo "$1 $2" >> gh.log
if [ "$1" = "pr" ] && [ "$2" = "checks" ]; then
	echo "Resource not accessible by personal access token" 1>&2
	exit 1
fi
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
	echo '{"headRefOid":"abc123"}'
	exit 0
fi
if [ "$1" = "run" ] && [ "$2" = "list" ]; then
	echo '[{"name":"ci","event":"pull_request","status":"completed","conclusion":"success","databaseId":9,"url":"https://github.com/o/r/actions/runs/9"}]'
	exit 0
fi
if [ "$1" = "api" ]; then
	echo '{"state":"success","statuses":[]}'
	exit 0
fi
exit 1
`)

	workspace := t.TempDir()
	pc := NewPRCreator(workspace, "", "", "")
	prURL := "https://github.com/org/repo/pull/1"

	checks, err := pc.Checks(t.Context(), prURL)
	require.NoError(t, err)
	require.Len(t, checks, 1)
	assert.Equal(t, "pass", checks[0].Bucket)
	assert.True(t, pc.checksFallback, "fallback armed after the inaccessible-Checks-API refusal")

	_, err = pc.Checks(t.Context(), prURL)
	require.NoError(t, err)

	log, readErr := os.ReadFile(filepath.Join(workspace, "gh.log"))
	require.NoError(t, readErr)
	assert.Equal(t, 1, strings.Count(string(log), "pr checks\n"),
		"pr checks invoked exactly once across both polls")
}

// TestChecksDoesNotFallBackOnOtherErrors pins that a non-permission failure
// (network hiccup, rate limit) surfaces as an error and does NOT arm the
// fallback: the next poll tries gh pr checks again.
func TestChecksDoesNotFallBackOnOtherErrors(t *testing.T) {
	stubGH(t, `
echo "$1 $2" >> gh.log
echo "connect: connection refused" 1>&2
exit 1
`)

	workspace := t.TempDir()
	pc := NewPRCreator(workspace, "", "", "")
	prURL := "https://github.com/org/repo/pull/1"

	_, err := pc.Checks(t.Context(), prURL)
	require.Error(t, err)
	assert.False(t, pc.checksFallback, "a non-permission failure must not arm the fallback")

	_, err = pc.Checks(t.Context(), prURL)
	require.Error(t, err)

	log, readErr := os.ReadFile(filepath.Join(workspace, "gh.log"))
	require.NoError(t, readErr)
	assert.Equal(t, 2, strings.Count(string(log), "pr checks\n"),
		"pr checks retried on the next poll since the fallback never armed")
}

// TestChecksViaRunsEmptyMeansNoCI pins the fallback path's own no-CI
// contract: no Actions runs and no commit statuses map to an empty result and
// a nil error - the same "this repo has no CI" semantics gh pr checks' own
// no-checks-reported failure translates to on the rollup path.
func TestChecksViaRunsEmptyMeansNoCI(t *testing.T) {
	stubGH(t, `
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
	echo '{"headRefOid":"abc123"}'
	exit 0
fi
if [ "$1" = "run" ] && [ "$2" = "list" ]; then
	echo '[]'
	exit 0
fi
if [ "$1" = "api" ]; then
	echo '{"state":"pending","statuses":[]}'
	exit 0
fi
exit 1
`)

	pc := NewPRCreator(t.TempDir(), "", "", "")

	checks, err := pc.checksViaRuns(t.Context(), "https://github.com/org/repo/pull/1")
	require.NoError(t, err)
	assert.Empty(t, checks)
}

// TestPRViewURLArgs pins the exact argv for the branch-PR probe: url and
// state, so parsePRViewURL can reject a non-OPEN result.
func TestPRViewURLArgs(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{"pr", "view", "--json", "url,state"}, prViewURLArgs())
}

// TestParsePRViewURL unmarshals gh pr view's --json url,state output,
// returning the URL only for an OPEN PR - gh pr view falls back to the
// branch's most recent CLOSED or MERGED PR when none is open, so the state
// check is what keeps this read to "open PR" rather than "any PR that ever
// existed on the branch" - and errors on unparsable output.
func TestParsePRViewURL(t *testing.T) {
	t.Parallel()

	url, err := parsePRViewURL(`{"url": "https://github.com/org/repo/pull/7", "state": "OPEN"}`)
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/org/repo/pull/7", url)

	url, err = parsePRViewURL(`{"url": "https://github.com/org/repo/pull/3", "state": "MERGED"}`)
	require.NoError(t, err)
	assert.Empty(t, url, "a merged PR must not be read as the branch's open PR")

	url, err = parsePRViewURL(`{"url": "https://github.com/org/repo/pull/5", "state": "CLOSED"}`)
	require.NoError(t, err)
	assert.Empty(t, url, "a closed PR must not be read as the branch's open PR")

	_, err = parsePRViewURL("not json")
	require.Error(t, err)
}

// TestFindPRURLTranslatesNoPRToEmpty pins the seam translation for a branch
// with no open PR: gh pr view exits non-zero with "no pull requests found",
// and FindPRURL must render that as an empty result, not an error - it is the
// expected outcome of a recovery probe, not a broken gh.
func TestFindPRURLTranslatesNoPRToEmpty(t *testing.T) {
	stubGH(t, `
echo "no pull requests found for branch \"cm/card-1\"" 1>&2
exit 1
`)

	pc := NewPRCreator(t.TempDir(), "", "", "")

	url, err := pc.FindPRURL(t.Context())
	require.NoError(t, err, "no PR on the branch is an empty result, not a failure")
	assert.Empty(t, url)
}

// TestFindPRURLKeepsRealErrors: any other gh failure stays an error - a broken
// gh (auth, network, rate limit) must never read as "no PR exists".
func TestFindPRURLKeepsRealErrors(t *testing.T) {
	stubGH(t, `
echo "HTTP 401: Bad credentials" 1>&2
exit 1
`)

	pc := NewPRCreator(t.TempDir(), "", "", "")

	_, err := pc.FindPRURL(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

// TestFindPRURLRejectsNonOpenPR pins the fix for the probe's most important
// defect: gh pr view exits zero and falls back to the branch's most recent
// CLOSED or MERGED PR when it has no open one, rather than erroring like the
// no-PR case does. FindPRURL must not adopt that PR - e.g. one a human closed
// to reject the work - as if it were the open PR the fail-closed park is
// recovering.
func TestFindPRURLRejectsNonOpenPR(t *testing.T) {
	stubGH(t, `
echo '{"url": "https://github.com/org/repo/pull/3", "state": "MERGED"}'
exit 0
`)

	pc := NewPRCreator(t.TempDir(), "", "", "")

	url, err := pc.FindPRURL(t.Context())
	require.NoError(t, err)
	assert.Empty(t, url, "a merged PR must not be recovered as the branch's open PR")
}

// TestRequestCopilotReviewIssuesRESTRequest pins the fix for the Copilot
// reviewer request: it calls the REST requested_reviewers endpoint via gh api
// (with the bot login and POST method) instead of gh pr edit --add-reviewer,
// which cannot resolve the bot login through GraphQL.
func TestRequestCopilotReviewIssuesRESTRequest(t *testing.T) {
	stubGH(t, `
echo "$@" > args.log
echo "{}"
exit 0
`)

	workspace := t.TempDir()
	pc := NewPRCreator(workspace, "", "", "")

	prURL := "https://github.com/org/repo/pull/7"
	err := pc.RequestCopilotReview(t.Context(), prURL)
	require.NoError(t, err)

	log, err := os.ReadFile(filepath.Join(workspace, "args.log"))
	require.NoError(t, err)
	assert.Equal(t, "api repos/org/repo/pulls/7/requested_reviewers --method POST -f reviewers[]=copilot-pull-request-reviewer[bot]",
		strings.TrimSpace(string(log)))
}

// TestRequestCopilotReviewErrorSurfacesWrappedError verifies that a non-zero
// gh exit from the REST endpoint returns the wrapped error, so the
// orchestrator can log it verbatim on the card.
func TestRequestCopilotReviewErrorSurfacesWrappedError(t *testing.T) {
	stubGH(t, `
echo "HTTP 422: Unprocessable Entity" 1>&2
exit 1
`)

	pc := NewPRCreator(t.TempDir(), "", "", "")

	prURL := "https://github.com/org/repo/pull/7"
	err := pc.RequestCopilotReview(t.Context(), prURL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gh api request copilot reviewer")
	assert.Contains(t, err.Error(), "HTTP 422")
}
