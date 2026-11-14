package runkind

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/redact"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testBranch  = "cm/sentinel/alpha"
	testSection = "Run the tests before pushing."
	testPRURL   = "https://github.com/o/r/pull/7"
)

// tgit runs git in dir and returns its trimmed output.
func tgit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	//nolint:gosec // G204: test-controlled arguments against temp repos
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	return strings.TrimSpace(string(out))
}

// guidanceRepo makes a bare repo whose main holds files and symlinks (path to
// target) and returns its file URL.
func guidanceRepo(t *testing.T, files, links map[string]string) string {
	t.Helper()

	root := t.TempDir()
	src := filepath.Join(root, "src")
	bare := filepath.Join(root, "repo.git")

	require.NoError(t, os.MkdirAll(src, 0o755))
	tgit(t, src, "init", "-q", "-b", "main")

	for name, body := range files {
		path := filepath.Join(src, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}

	for name, target := range links {
		require.NoError(t, os.Symlink(target, filepath.Join(src, name)))
	}

	tgit(t, src, "add", ".")
	tgit(t, src, "commit", "-q", "-m", "init")
	tgit(t, root, "clone", "-q", "--bare", src, bare)

	return "file://" + bare
}

func standardRepo(t *testing.T) string {
	t.Helper()

	return guidanceRepo(t, map[string]string{"AGENTS.md": "# Repo\n"}, nil)
}

func bareDir(repoURL string) string { return strings.TrimPrefix(repoURL, "file://") }

func remoteTip(t *testing.T, repoURL string) string {
	t.Helper()

	return tgit(t, bareDir(repoURL), "rev-parse", "--verify", "-q", "refs/heads/"+testBranch)
}

func remoteHasBranch(t *testing.T, repoURL string) bool {
	t.Helper()

	return tgit(t, bareDir(repoURL), "branch", "--list", testBranch) != ""
}

// pushCommit force-pushes a commit adding a file onto branch, cut from start,
// the way a human edits the branch, and returns its SHA.
func pushCommit(t *testing.T, repoURL, branch, start string) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "human")
	tgit(t, filepath.Dir(dir), "clone", "-q", repoURL, dir)
	tgit(t, dir, "checkout", "-q", "-B", branch, start)
	require.NoError(t, os.WriteFile(filepath.Join(dir, strings.ReplaceAll(start, "/", "_")+".txt"), []byte("human\n"), 0o644))
	tgit(t, dir, "add", ".")
	tgit(t, dir, "commit", "-q", "-m", "human")
	tgit(t, dir, "push", "-q", "--force", "origin", "HEAD:refs/heads/"+branch)

	return tgit(t, dir, "rev-parse", "HEAD")
}

type prCall struct{ url, title, body, base, head string }

// fakePRs is the PR host: it returns a set open PR, records writes and runs an
// optional hook inside OpenPRForHead.
type fakePRs struct {
	openURL, openHead string
	hook              func()
	created, edited   []prCall
}

func (f *fakePRs) OpenPRForHead(context.Context, string) (string, string, error) {
	if f.hook != nil {
		f.hook()
	}

	return f.openURL, f.openHead, nil
}

func (f *fakePRs) Create(_ context.Context, title, body, base, head string) (string, error) {
	f.created = append(f.created, prCall{title: title, body: body, base: base, head: head})

	return testPRURL, nil
}

func (f *fakePRs) EditPR(_ context.Context, url, title, body string) error {
	f.edited = append(f.edited, prCall{url: url, title: title, body: body})

	return nil
}

func guidanceSpec(t *testing.T, repoURL string) Spec {
	t.Helper()

	return Spec{
		Kind:       protocol.RunKindGuidancePR,
		Project:    "alpha",
		RepoURL:    repoURL,
		BaseBranch: "main",
		Workspace:  filepath.Join(t.TempDir(), "workspace"),
	}
}

func guidanceIn(expectHead string) protocol.GuidancePRInput {
	return protocol.GuidancePRInput{ExpectHead: expectHead, Section: testSection, Title: "Guidance", Body: "Body"}
}

func guidanceDepsFor(spec Spec, f *fakePRs) guidanceDeps {
	return guidanceDeps{prs: f, candidates: orchestrator.VerifyCandidates, absent: absentFromRepo, publish: operatorMask(spec)}
}

// runGuidance clones into a fresh workspace and runs guidance_pr there.
func runGuidance(t *testing.T, spec Spec, in protocol.GuidancePRInput, f *fakePRs) (protocol.GuidancePRResult, error) {
	t.Helper()

	ws := workspaceDir(spec)
	require.NoError(t, clone(t.Context(), spec, ws))

	return guidancePR(t.Context(), ws, spec, in, guidanceDepsFor(spec, f))
}

func fileAt(t *testing.T, repoURL, rev, path string) string {
	t.Helper()

	return tgit(t, bareDir(repoURL), "show", rev+":"+path)
}

func TestStale(t *testing.T) {
	tests := []struct {
		name        string
		expect, tip string
		open        bool
		openHead    string
		want        bool
	}{
		{"no expectation, nothing open", "", "", false, "", false},
		{"no expectation, closed PR's branch", "", "abc", false, "", false},
		{"no expectation, open PR", "", "abc", true, "abc", true},
		{"all agree", "x", "x", true, "x", false},
		{"expected PR is gone", "x", "x", false, "", true},
		{"branch moved", "x", "y", true, "x", true},
		{"PR head moved", "x", "x", true, "y", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stale(tc.expect, tc.tip, tc.open, tc.openHead))
		})
	}
}

func TestNotAllowed(t *testing.T) {
	assert.Equal(t, []string{"a", "c"}, notAllowed([]string{"a", "b", "c"}, []string{"b"}))
	assert.Nil(t, notAllowed(nil, []string{"b"}))
	assert.Equal(t, []string{"B"}, notAllowed([]string{"B"}, []string{"b"}), "entries compare exactly")
}

func TestGuidancePROpensAFreshPR(t *testing.T) {
	repo := standardRepo(t)
	f := &fakePRs{}

	res, err := runGuidance(t, guidanceSpec(t, repo), guidanceIn(""), f)
	require.NoError(t, err)

	tip := remoteTip(t, repo)
	assert.Equal(t, protocol.GuidancePRResult{Outcome: protocol.GuidancePushed, PRURL: testPRURL, HeadSHA: tip, File: "AGENTS.md"}, res)
	assert.Equal(t, tgit(t, bareDir(repo), "rev-parse", "main"), tgit(t, bareDir(repo), "rev-parse", tip+"^"))
	assert.Equal(t, "AGENTS.md", tgit(t, bareDir(repo), "diff-tree", "--no-commit-id", "--name-only", "-r", tip))

	want, err := withGuidance("# Repo\n", "alpha", testSection)
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(want), fileAt(t, repo, tip, "AGENTS.md"))

	require.Len(t, f.created, 1)
	assert.Equal(t, prCall{title: "Guidance", body: "Body", base: "main", head: testBranch}, f.created[0])
	assert.Empty(t, f.edited)
}

func TestGuidancePRRevisesItsOwnPR(t *testing.T) {
	repo := standardRepo(t)

	first, err := runGuidance(t, guidanceSpec(t, repo), guidanceIn(""), &fakePRs{})
	require.NoError(t, err)

	f := &fakePRs{openURL: testPRURL, openHead: first.HeadSHA}
	in := guidanceIn(first.HeadSHA)
	in.Section, in.Title, in.Body = "Prefer small commits.", "Guidance 2", "Body 2"

	res, err := runGuidance(t, guidanceSpec(t, repo), in, f)
	require.NoError(t, err)

	tip := remoteTip(t, repo)
	assert.Equal(t, protocol.GuidancePushed, res.Outcome)
	assert.Equal(t, tip, res.HeadSHA)
	assert.NotEqual(t, first.HeadSHA, tip)
	assert.Equal(t, first.HeadSHA, tgit(t, bareDir(repo), "rev-parse", tip+"^"))
	assert.Equal(t, []prCall{{url: testPRURL, title: "Guidance 2", body: "Body 2"}}, f.edited)
	assert.Empty(t, f.created)

	text := fileAt(t, repo, tip, "AGENTS.md")
	assert.Contains(t, text, "Prefer small commits.")
	assert.NotContains(t, text, testSection, "a revision carries the new section, not the old one on top")
}

func TestGuidancePRUnchanged(t *testing.T) {
	repo := standardRepo(t)

	first, err := runGuidance(t, guidanceSpec(t, repo), guidanceIn(""), &fakePRs{})
	require.NoError(t, err)

	f := &fakePRs{openURL: testPRURL, openHead: first.HeadSHA}
	in := guidanceIn(first.HeadSHA)
	in.Title = "Retitled"

	res, err := runGuidance(t, guidanceSpec(t, repo), in, f)
	require.NoError(t, err)

	assert.Equal(t, protocol.GuidancePRResult{Outcome: protocol.GuidanceUnchanged, PRURL: testPRURL, HeadSHA: first.HeadSHA, File: "AGENTS.md"}, res)
	assert.Equal(t, first.HeadSHA, remoteTip(t, repo))
	assert.Equal(t, []prCall{{url: testPRURL, title: "Retitled", body: "Body"}}, f.edited)
}

func TestGuidancePRStale(t *testing.T) {
	t.Run("the branch moved past the expected head", func(t *testing.T) {
		repo := standardRepo(t)

		first, err := runGuidance(t, guidanceSpec(t, repo), guidanceIn(""), &fakePRs{})
		require.NoError(t, err)

		moved := pushCommit(t, repo, testBranch, "origin/"+testBranch)
		f := &fakePRs{openURL: testPRURL, openHead: first.HeadSHA}

		res, err := runGuidance(t, guidanceSpec(t, repo), guidanceIn(first.HeadSHA), f)
		require.NoError(t, err)

		assert.Equal(t, protocol.GuidanceStale, res.Outcome)
		assert.Equal(t, moved, remoteTip(t, repo))
		assert.Empty(t, f.created)
		assert.Empty(t, f.edited)
	})

	t.Run("an open PR when none was expected", func(t *testing.T) {
		repo := standardRepo(t)
		tip := pushCommit(t, repo, testBranch, "origin/main")
		f := &fakePRs{openURL: testPRURL, openHead: tip}

		res, err := runGuidance(t, guidanceSpec(t, repo), guidanceIn(""), f)
		require.NoError(t, err)

		assert.Equal(t, protocol.GuidanceStale, res.Outcome)
		assert.Equal(t, tip, remoteTip(t, repo))
		assert.Empty(t, f.created)
	})
}

func TestGuidancePRLosesARaceToAHuman(t *testing.T) {
	t.Run("fresh run over a closed PR's branch", func(t *testing.T) {
		repo := standardRepo(t)
		closed := pushCommit(t, repo, testBranch, "origin/main")

		var human string

		f := &fakePRs{hook: func() { human = pushCommit(t, repo, testBranch, "origin/"+testBranch) }}

		res, err := runGuidance(t, guidanceSpec(t, repo), guidanceIn(""), f)
		require.NoError(t, err)

		assert.Equal(t, protocol.GuidanceStale, res.Outcome)
		assert.NotEqual(t, closed, human)
		assert.Equal(t, human, remoteTip(t, repo))
		assert.Empty(t, f.created)
	})

	t.Run("revision of an open PR", func(t *testing.T) {
		repo := standardRepo(t)

		first, err := runGuidance(t, guidanceSpec(t, repo), guidanceIn(""), &fakePRs{})
		require.NoError(t, err)

		var human string

		f := &fakePRs{
			openURL: testPRURL, openHead: first.HeadSHA,
			hook: func() { human = pushCommit(t, repo, testBranch, "origin/"+testBranch) },
		}
		in := guidanceIn(first.HeadSHA)
		in.Section = "Prefer small commits."

		res, err := runGuidance(t, guidanceSpec(t, repo), in, f)
		require.NoError(t, err)

		assert.Equal(t, protocol.GuidanceStale, res.Outcome)
		assert.Equal(t, human, remoteTip(t, repo))
		assert.Empty(t, f.edited)
	})
}

func TestGuidancePRReplacesAClosedPRsBranch(t *testing.T) {
	repo := standardRepo(t)
	closed := pushCommit(t, repo, testBranch, "origin/main")

	res, err := runGuidance(t, guidanceSpec(t, repo), guidanceIn(""), &fakePRs{})
	require.NoError(t, err)

	tip := remoteTip(t, repo)
	assert.Equal(t, protocol.GuidancePushed, res.Outcome)
	assert.NotEqual(t, closed, tip)
	assert.Equal(t, tgit(t, bareDir(repo), "rev-parse", "main"), tgit(t, bareDir(repo), "rev-parse", tip+"^"))
}

func TestGuidancePRRefusesTextTheRepoLacks(t *testing.T) {
	in := guidanceIn("")
	in.Section = "See https://new.example.net/guide"

	t.Run("refused", func(t *testing.T) {
		repo := standardRepo(t)
		f := &fakePRs{}

		res, err := runGuidance(t, guidanceSpec(t, repo), in, f)
		require.NoError(t, err)

		assert.Equal(t, protocol.GuidanceRefused, res.Outcome)
		assert.Contains(t, res.AbsentFromRepo, "https://new.example.net/guide")
		assert.Contains(t, res.AbsentFromRepo, "new.example.net")
		assert.False(t, remoteHasBranch(t, repo))
		assert.Empty(t, f.created)
	})

	t.Run("allowed", func(t *testing.T) {
		repo := standardRepo(t)
		in := in
		in.Allowed = []string{"https://new.example.net/guide", "new.example.net"}

		res, err := runGuidance(t, guidanceSpec(t, repo), in, &fakePRs{})
		require.NoError(t, err)

		assert.Equal(t, protocol.GuidancePushed, res.Outcome)
	})
}

func TestGuidancePRPicksTheFileGroundingReads(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		links     map[string]string
		wantFile  string
		wantLink  string // a path that must stay a symlink to this target
		wantError bool
	}{
		{name: "CLAUDE.md only", files: map[string]string{"CLAUDE.md": "# C\n"}, wantFile: "CLAUDE.md"},
		{name: "neither file", files: map[string]string{"README.md": "r\n"}, wantFile: "AGENTS.md"},
		{
			name: "AGENTS.md links to CLAUDE.md", files: map[string]string{"CLAUDE.md": "# C\n"},
			links: map[string]string{"AGENTS.md": "CLAUDE.md"}, wantFile: "CLAUDE.md", wantLink: "CLAUDE.md",
		},
		{
			name: "AGENTS.md links into docs", files: map[string]string{"docs/AGENTS.md": "# D\n"},
			links: map[string]string{"AGENTS.md": "docs/AGENTS.md"}, wantFile: "docs/AGENTS.md", wantLink: "docs/AGENTS.md",
		},
		{name: "escaping link", links: map[string]string{"AGENTS.md": "../outside.md"}, wantError: true},
		{
			name: "chained link", files: map[string]string{"CLAUDE.md": "# C\n"},
			links: map[string]string{"AGENTS.md": "link.md", "link.md": "CLAUDE.md"}, wantError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := guidanceRepo(t, tc.files, tc.links)

			res, err := runGuidance(t, guidanceSpec(t, repo), guidanceIn(""), &fakePRs{})
			if tc.wantError {
				require.Error(t, err)
				assert.False(t, remoteHasBranch(t, repo))

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantFile, res.File)

			tip := remoteTip(t, repo)
			assert.Equal(t, tc.wantFile, tgit(t, bareDir(repo), "diff-tree", "--no-commit-id", "--name-only", "-r", tip))
			assert.Contains(t, fileAt(t, repo, tip, tc.wantFile), testSection)

			if tc.wantLink != "" {
				assert.Equal(t, tc.wantLink, fileAt(t, repo, tip, "AGENTS.md"), "the link stays a link")
				assert.True(t, strings.HasPrefix(tgit(t, bareDir(repo), "ls-tree", tip, "AGENTS.md"), "120000"))
			}
		})
	}
}

func TestGuidancePRMasksWhatItWrites(t *testing.T) {
	const (
		token = "ghs_test_token_1"
		long  = "op-secret-1"
		short = "short77"
	)

	text := "a " + token + " b " + long + " c " + short
	repo := standardRepo(t)
	spec := guidanceSpec(t, repo)
	spec.GitToken = token
	spec.OperatorValues = []string{long, short}
	f := &fakePRs{}

	_, err := runGuidance(t, spec, protocol.GuidancePRInput{Section: text, Title: text, Body: text}, f)
	require.NoError(t, err)

	tip := remoteTip(t, repo)
	for name, got := range map[string]string{
		"file":    fileAt(t, repo, tip, "AGENTS.md"),
		"commit":  tgit(t, bareDir(repo), "log", "-1", "--format=%B", tip),
		"title":   f.created[0].title,
		"PR body": f.created[0].body,
	} {
		assert.NotContains(t, got, token, name)
		assert.NotContains(t, got, long, name)
		assert.Contains(t, got, "[REDACTED]", name)
		assert.Contains(t, got, short, name+": a value under 8 bytes is not masked")
	}
}

func TestGuidancePRKeepsTheResultExactAndMasksTheInput(t *testing.T) {
	const host = "github.com"

	repo := standardRepo(t)
	spec := guidanceSpec(t, repo)
	spec.OperatorValues = []string{host}
	in := guidanceIn("")
	in.Section = "Docs live on " + host
	in.Allowed = []string{host}

	input, err := json.Marshal(in)
	require.NoError(t, err)

	spec.Input = input

	d := run(context.Background(), spec, slog.Default(), redact.New(nil), discardEmitter(), deps{
		guidance: func(string) guidanceDeps { return guidanceDepsFor(spec, &fakePRs{}) },
	})
	require.Equal(t, protocol.StopCauseDone, d.Cause)

	assert.Equal(t, testPRURL, d.Result.GuidancePR.PRURL)

	text := fileAt(t, repo, remoteTip(t, repo), "AGENTS.md")
	assert.Contains(t, text, "[REDACTED]")
	assert.NotContains(t, text, host)
}

func TestGuidancePRRevisionRefusesALinkTheBaseNoLongerHas(t *testing.T) {
	repo := guidanceRepo(t, map[string]string{"CLAUDE.md": "# C\n"}, map[string]string{"AGENTS.md": "CLAUDE.md"})

	first, err := runGuidance(t, guidanceSpec(t, repo), guidanceIn(""), &fakePRs{})
	require.NoError(t, err)

	dir := filepath.Join(t.TempDir(), "human")
	tgit(t, filepath.Dir(dir), "clone", "-q", repo, dir)
	require.NoError(t, os.Remove(filepath.Join(dir, "AGENTS.md")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# A\n"), 0o644))
	tgit(t, dir, "commit", "-q", "-am", "unlink")
	tgit(t, dir, "push", "-q", "origin", "main")

	f := &fakePRs{openURL: testPRURL, openHead: first.HeadSHA}

	_, err = runGuidance(t, guidanceSpec(t, repo), guidanceIn(first.HeadSHA), f)
	require.Error(t, err)

	assert.Equal(t, first.HeadSHA, remoteTip(t, repo))
	assert.Empty(t, f.created)
	assert.Empty(t, f.edited)
}

func TestGuidancePRMalformedBlockFails(t *testing.T) {
	start, _ := blockMarkers("alpha")
	repo := guidanceRepo(t, map[string]string{"AGENTS.md": start + "\n" + start + "\n"}, nil)

	_, err := runGuidance(t, guidanceSpec(t, repo), guidanceIn(""), &fakePRs{})
	require.ErrorIs(t, err, errMalformedBlock)
	assert.False(t, remoteHasBranch(t, repo))
}

func TestRunGuidancePREndToEnd(t *testing.T) {
	repo := standardRepo(t)
	spec := guidanceSpec(t, repo)

	input, err := json.Marshal(guidanceIn(""))
	require.NoError(t, err)

	spec.Input = input
	f := &fakePRs{}

	d := run(context.Background(), spec, slog.Default(), redact.New(nil), discardEmitter(), deps{
		guidance: func(string) guidanceDeps { return guidanceDepsFor(spec, f) },
	})

	require.Equal(t, protocol.StopCauseDone, d.Cause)
	require.NotNil(t, d.Result.GuidancePR)
	assert.Equal(t, protocol.GuidancePushed, d.Result.GuidancePR.Outcome)
	assert.Empty(t, d.Usage)
}
