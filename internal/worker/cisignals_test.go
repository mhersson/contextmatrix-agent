package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ciTestPR = "https://github.com/org/repo/pull/1"

const ciPRViewReply = `{"headRefOid":"abc123","baseRefName":"playbook/x","baseRefOid":"def456"}`

// ghRule is one canned gh reply: the first rule whose match is a substring of
// the joined argv answers, writing stdout and stderr and exiting with code.
type ghRule struct {
	match  string
	stdout string
	stderr string
	code   int
}

// stubGHRules fakes gh from ordered rules, logging every argv to gh.log in the
// workspace. An argv no rule matches fails the call.
func stubGHRules(t *testing.T, rules ...ghRule) {
	t.Helper()

	var sb strings.Builder

	sb.WriteString("echo \"$*\" >> gh.log\ncase \"$*\" in\n")

	for _, r := range rules {
		fmt.Fprintf(&sb, "*'%s'*)\n\tprintf '%%s' '%s'\n\tprintf '%%s' '%s' 1>&2\n\texit %d\n\t;;\n",
			r.match, r.stdout, r.stderr, r.code)
	}

	sb.WriteString("esac\necho unmatched 1>&2\nexit 1\n")
	stubGH(t, sb.String())
}

func readGHLog(t *testing.T, workspace string) string {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(workspace, "gh.log"))
	if os.IsNotExist(err) {
		return ""
	}

	require.NoError(t, err)

	return string(b)
}

func treeJSON(entries ...string) string {
	return `{"tree":[` + strings.Join(entries, ",") + `],"truncated":false}`
}

func blob(path string) string {
	return fmt.Sprintf(`{"path":%q,"type":"blob","sha":"b-%s"}`, path, path)
}

func subtree(path, sha string) string {
	return fmt.Sprintf(`{"path":%q,"type":"tree","sha":%q}`, path, sha)
}

func TestCIConfigFiles(t *testing.T) {
	prView := ghRule{match: "pr view", stdout: ciPRViewReply}
	root := func(entries ...string) ghRule {
		return ghRule{match: "git/trees/abc123", stdout: treeJSON(entries...)}
	}

	cases := []struct {
		name    string
		rules   []ghRule
		want    []string
		wantErr string
	}{
		{"jenkinsfile", []ghRule{prView, root(blob("Jenkinsfile"))}, []string{"Jenkinsfile"}, ""},
		{"travis", []ghRule{prView, root(blob(".travis.yml"))}, []string{".travis.yml"}, ""},
		{"azure", []ghRule{prView, root(blob("azure-pipelines.yml"))}, []string{"azure-pipelines.yml"}, ""},
		{"drone", []ghRule{prView, root(blob(".drone.yml"))}, []string{".drone.yml"}, ""},
		{"woodpecker file", []ghRule{prView, root(blob(".woodpecker.yml"))}, []string{".woodpecker.yml"}, ""},
		{"appveyor", []ghRule{prView, root(blob("appveyor.yml"))}, []string{"appveyor.yml"}, ""},
		{"dot appveyor", []ghRule{prView, root(blob(".appveyor.yml"))}, []string{".appveyor.yml"}, ""},
		{"cirrus", []ghRule{prView, root(blob(".cirrus.yml"))}, []string{".cirrus.yml"}, ""},
		{"woodpecker yaml", []ghRule{prView, root(blob(".woodpecker.yaml"))}, []string{".woodpecker.yaml"}, ""},
		{"cloudbuild yaml", []ghRule{prView, root(blob("cloudbuild.yaml"))}, []string{"cloudbuild.yaml"}, ""},
		{"cloudbuild yml", []ghRule{prView, root(blob("cloudbuild.yml"))}, []string{"cloudbuild.yml"}, ""},
		{"buildspec", []ghRule{prView, root(blob("buildspec.yml"))}, []string{"buildspec.yml"}, ""},
		{"semaphore dir", []ghRule{prView, root(subtree(".semaphore", "t1"))}, []string{".semaphore/"}, ""},
		{"teamcity dir", []ghRule{prView, root(subtree(".teamcity", "t1"))}, []string{".teamcity/"}, ""},
		{"near-miss backup blob", []ghRule{prView, root(blob("appveyor.yml.bak"))}, nil, ""},
		{"buildkite dir", []ghRule{prView, root(subtree(".buildkite", "t1"))}, []string{".buildkite/"}, ""},
		{"woodpecker dir", []ghRule{prView, root(subtree(".woodpecker", "t1"))}, []string{".woodpecker/"}, ""},
		{
			"circleci",
			[]ghRule{
				prView, root(subtree(".circleci", "t1")),
				{match: "git/trees/t1", stdout: treeJSON(blob("config.yml"))},
			},
			[]string{".circleci/config.yml"},
			"",
		},
		{
			"github workflow yml",
			[]ghRule{
				prView, root(subtree(".github", "t1")),
				{match: "git/trees/t1", stdout: treeJSON(subtree("workflows", "t2"))},
				{match: "git/trees/t2", stdout: treeJSON(blob("ci.yml"))},
			},
			[]string{".github/workflows/ci.yml"},
			"",
		},
		{
			"github workflow yaml",
			[]ghRule{
				prView, root(subtree(".github", "t1")),
				{match: "git/trees/t1", stdout: treeJSON(subtree("workflows", "t2"))},
				{match: "git/trees/t2", stdout: treeJSON(blob("release.yaml"))},
			},
			[]string{".github/workflows/release.yaml"},
			"",
		},
		{"empty root", []ghRule{prView, root()}, nil, ""},
		{
			"workflows only readme",
			[]ghRule{
				prView, root(subtree(".github", "t1")),
				{match: "git/trees/t1", stdout: treeJSON(subtree("workflows", "t2"))},
				{match: "git/trees/t2", stdout: treeJSON(blob("README.md"))},
			},
			nil, "",
		},
		{
			"github without workflows",
			[]ghRule{
				prView, root(subtree(".github", "t1")),
				{match: "git/trees/t1", stdout: treeJSON(blob("CODEOWNERS"))},
			},
			nil, "",
		},
		{
			"circleci other file",
			[]ghRule{
				prView, root(subtree(".circleci", "t1")),
				{match: "git/trees/t1", stdout: treeJSON(blob("other.yml"))},
			},
			nil, "",
		},
		{"lookalike blob", []ghRule{prView, root(blob("jenkinsfile.txt"))}, nil, ""},
		{
			"root 404",
			[]ghRule{prView, {match: "git/trees/abc123", stderr: "gh: Not Found (HTTP 404)", code: 1}},
			nil, "Not Found",
		},
		{
			"root rate limit",
			[]ghRule{prView, {match: "git/trees/abc123", stderr: "HTTP 403: API rate limit exceeded", code: 1}},
			nil, "rate limit",
		},
		{
			"root auth",
			[]ghRule{prView, {match: "git/trees/abc123", stderr: "gh: authentication required", code: 1}},
			nil, "authentication required",
		},
		{
			"subtree failure",
			[]ghRule{
				prView, root(subtree(".circleci", "t1")),
				{match: "git/trees/t1", stderr: "HTTP 502", code: 1},
			},
			nil, "502",
		},
		{
			"truncated",
			[]ghRule{prView, {match: "git/trees/abc123", stdout: `{"tree":[],"truncated":true}`}},
			nil, "truncated",
		},
		{"pr view fails", []ghRule{{match: "pr view", stderr: "boom", code: 1}}, nil, "boom"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubGHRules(t, tc.rules...)

			pc := NewPRCreator(t.TempDir(), "", "", "")

			got, err := pc.CIConfigFiles(t.Context(), ciTestPR)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.ElementsMatch(t, tc.want, got)
		})
	}
}

func TestBaseCIActivity(t *testing.T) {
	prView := ghRule{match: "pr view", stdout: ciPRViewReply}
	denied := "Resource not accessible by personal access token (HTTP 403)"
	noStatus := ghRule{match: "/status", stdout: `{"statuses":[]}`}

	cases := []struct {
		name       string
		fallback   bool
		rules      []ghRule
		wantActive bool
		wantErr    string
		wantLog    []string
		wantAbsent []string
	}{
		{"check runs", false, []ghRule{prView, {match: "check-runs", stdout: `{"total_count":2}`}, noStatus}, true, "", nil, nil},
		{
			"status only", false,
			[]ghRule{prView, {match: "check-runs", stdout: `{"total_count":0}`}, {match: "/status", stdout: `{"statuses":[{"context":"ci","state":"success"}]}`}},
			true, "", nil, nil,
		},
		{
			"slashed base inactive", false,
			[]ghRule{prView, {match: "check-runs", stdout: `{"total_count":0}`}, noStatus},
			false, "",
			[]string{"commits/def456/check-runs", "commits/def456/status"},
			nil,
		},
		{
			"checks refused no runs", false,
			[]ghRule{prView, {match: "check-runs", stderr: denied, code: 1}, {match: "run list", stdout: `[]`}, noStatus},
			false, "", nil, nil,
		},
		{
			"checks refused with run", false,
			[]ghRule{
				prView,
				{match: "check-runs", stderr: denied, code: 1},
				{match: "run list", stdout: `[{"name":"ci","event":"push","status":"completed","conclusion":"success","databaseId":1,"workflowDatabaseId":2}]`},
				noStatus,
			},
			true, "", nil, nil,
		},
		{
			"sticky fallback skips check runs", true,
			[]ghRule{prView, {match: "run list", stdout: `[]`}, noStatus},
			false, "",
			[]string{"run list"},
			[]string{"check-runs"},
		},
		{"check runs 502", false, []ghRule{prView, {match: "check-runs", stderr: "HTTP 502: Bad Gateway", code: 1}}, false, "502", nil, nil},
		{
			"status 502", false,
			[]ghRule{prView, {match: "check-runs", stdout: `{"total_count":0}`}, {match: "/status", stderr: "HTTP 502", code: 1}},
			false, "502", nil, nil,
		},
		{"pr view fails", false, []ghRule{{match: "pr view", stderr: "boom", code: 1}}, false, "boom", nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubGHRules(t, tc.rules...)

			workspace := t.TempDir()
			pc := NewPRCreator(workspace, "", "", "")
			pc.checksFallback = tc.fallback

			base, active, err := pc.BaseCIActivity(t.Context(), ciTestPR)
			log := readGHLog(t, workspace)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, "playbook/x", base)
			assert.Equal(t, tc.wantActive, active)

			for _, s := range tc.wantLog {
				assert.Contains(t, log, s)
			}

			for _, s := range tc.wantAbsent {
				assert.NotContains(t, log, s)
			}

			for line := range strings.SplitSeq(log, "\n") {
				if strings.HasPrefix(line, "api ") {
					assert.NotContains(t, line, "playbook/x", "the base name never rides in an api path")
				}
			}

			assert.Equal(t, tc.fallback, pc.checksFallback, "the signal read never arms the sticky fallback")
		})
	}
}

func TestLastMergedCIActivity(t *testing.T) {
	prView := ghRule{match: "pr view", stdout: ciPRViewReply}
	denied := "Resource not accessible by personal access token (HTTP 403)"
	noStatus := ghRule{match: "/status", stdout: `{"statuses":[]}`}
	pullsMixed := `[
{"number":3,"merged_at":null,"head":{"sha":"open333"}},
{"number":2,"merged_at":"2026-10-01T00:00:00Z","head":{"sha":"old222"}},
{"number":1,"merged_at":"2026-10-02T00:00:00Z","head":{"sha":"new111"}}]`
	pullsOne := `[{"number":1,"merged_at":"2026-10-02T00:00:00Z","head":{"sha":"new111"}}]`
	pulls := func(body string) ghRule { return ghRule{match: "/pulls", stdout: body} }
	defaultBranch := ghRule{match: "api repos/org/repo", stdout: `{"default_branch":"main"}`}
	basePulls := func(body string) ghRule { return ghRule{match: "base=playbook/x", stdout: body} }
	mainPulls := func(body string) ghRule { return ghRule{match: "base=main", stdout: body} }
	mainMerged := `[{"number":9,"merged_at":"2026-10-03T00:00:00Z","head":{"sha":"main111"}}]`
	zeroChecks := ghRule{match: "check-runs", stdout: `{"total_count":0}`}

	cases := []struct {
		name       string
		rules      []ghRule
		wantActive bool
		wantErr    string
		wantLog    []string
		wantAbsent []string
	}{
		{
			"fresh base falls back to default branch",
			[]ghRule{
				prView, basePulls(`[]`), mainPulls(mainMerged),
				{match: "commits/main111/check-runs", stdout: `{"total_count":1}`},
				defaultBranch,
			},
			true, "",
			[]string{"base=main", "commits/main111/check-runs"},
			nil,
		},
		{
			"default branch newest merged has no checks",
			[]ghRule{prView, basePulls(`[]`), mainPulls(mainMerged), zeroChecks, noStatus, defaultBranch},
			false, "",
			[]string{"commits/main111/check-runs"},
			nil,
		},
		{
			"default branch has no merged PR either",
			[]ghRule{prView, basePulls(`[]`), mainPulls(`[]`), defaultBranch},
			false, "", nil,
			[]string{"commits/"},
		},
		{
			"base with merged PR skips default branch read",
			[]ghRule{prView, basePulls(pullsOne), zeroChecks, noStatus},
			false, "", nil,
			[]string{"api repos/org/repo\n", "base=main"},
		},
		{
			"base equals default branch",
			[]ghRule{
				{match: "pr view", stdout: `{"baseRefName":"main","baseRefOid":"def456"}`},
				mainPulls(`[]`), defaultBranch,
			},
			false, "", nil,
			[]string{"commits/"},
		},
		{"default branch read fails", []ghRule{prView, basePulls(`[]`), {match: "api repos/org/repo", stderr: "HTTP 502", code: 1}}, false, "502", nil, nil},
		{"fallback pulls read fails", []ghRule{prView, basePulls(`[]`), {match: "base=main", stderr: "HTTP 502", code: 1}, defaultBranch}, false, "502", nil, nil},
		{
			"picks newest merged",
			[]ghRule{prView, pulls(pullsMixed), {match: "commits/new111/check-runs", stdout: `{"total_count":1}`}, {match: "check-runs", stdout: `{"total_count":0}`}, noStatus},
			true, "",
			[]string{"commits/new111/check-runs"},
			[]string{"old222", "open333"},
		},
		{
			"statuses only",
			[]ghRule{prView, pulls(pullsOne), {match: "check-runs", stdout: `{"total_count":0}`}, {match: "/status", stdout: `{"statuses":[{"context":"ci","state":"success"}]}`}},
			true, "", nil, nil,
		},
		{
			"nothing",
			[]ghRule{prView, pulls(pullsOne), {match: "check-runs", stdout: `{"total_count":0}`}, noStatus},
			false, "", nil, nil,
		},
		{"no pulls", []ghRule{prView, pulls(`[]`), defaultBranch}, false, "", nil, []string{"commits/"}},
		{
			"none merged",
			[]ghRule{prView, pulls(`[{"number":3,"merged_at":null,"head":{"sha":"open333"}}]`), defaultBranch},
			false, "", nil,
			[]string{"commits/"},
		},
		{
			"slashed base is a field",
			[]ghRule{prView, pulls(`[]`), defaultBranch},
			false, "",
			[]string{"-f base=playbook/x", "--method GET"},
			[]string{"pulls/playbook", "playbook/x/"},
		},
		{
			"checks refused uses run list",
			[]ghRule{
				prView, pulls(pullsOne),
				{match: "check-runs", stderr: denied, code: 1},
				{match: "run list", stdout: `[{"name":"ci","event":"push","status":"completed","conclusion":"success","databaseId":1,"workflowDatabaseId":2}]`},
				noStatus,
			},
			true, "",
			[]string{"run list"},
			nil,
		},
		{"pulls error", []ghRule{prView, {match: "/pulls", stderr: "HTTP 502", code: 1}}, false, "502", nil, nil},
		{
			"status error",
			[]ghRule{prView, pulls(pullsOne), {match: "check-runs", stdout: `{"total_count":0}`}, {match: "/status", stderr: "HTTP 502", code: 1}},
			false, "502", nil, nil,
		},
		{"pr view error", []ghRule{{match: "pr view", stderr: "boom", code: 1}}, false, "boom", nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubGHRules(t, tc.rules...)

			workspace := t.TempDir()
			pc := NewPRCreator(workspace, "", "", "")

			active, err := pc.LastMergedCIActivity(t.Context(), ciTestPR)
			log := readGHLog(t, workspace)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantActive, active)

			for _, s := range tc.wantLog {
				assert.Contains(t, log, s)
			}

			for _, s := range tc.wantAbsent {
				assert.NotContains(t, log, s)
			}
		})
	}
}

func TestLastMergedCIActivityBaseIsDefaultBranch(t *testing.T) {
	stubGHRules(t,
		ghRule{match: "pr view", stdout: `{"baseRefName":"main","baseRefOid":"def456"}`},
		ghRule{match: "base=main", stdout: `[]`},
		ghRule{match: "api repos/org/repo", stdout: `{"default_branch":"main"}`},
	)

	workspace := t.TempDir()
	pc := NewPRCreator(workspace, "", "", "")

	active, err := pc.LastMergedCIActivity(t.Context(), ciTestPR)
	require.NoError(t, err)
	assert.False(t, active)
	assert.Equal(t, 1, strings.Count(readGHLog(t, workspace), "base=main"), "the default branch is not listed twice")
}
