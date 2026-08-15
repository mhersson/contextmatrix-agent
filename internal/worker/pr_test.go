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
	assert.Equal(t, []string{"pr", "edit", prURL, "--add-reviewer", "copilot-pull-request-reviewer[bot]"}, addCopilotReviewerArgs(prURL))
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
