package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reviewTestDeps builds Deps wired for the review phase: scripted ops + git, the
// supplied LLM (specialist fan-out + synthesis), read+write tools, and the
// supplied registry. The workspace is a temp dir so the verify gate, when it
// runs, has a real (empty) root.
func reviewTestDeps(t *testing.T, ops *fakeOps, git *fakeGit, client llm.LLM, reg *registry.Registry) Deps {
	t.Helper()

	return Deps{
		Ops:        ops,
		Git:        git,
		Client:     client,
		Emit:       events.NewEmitter(nil, nil),
		Registry:   reg,
		WriteTools: tools.NewRegistry(tools.NewReadTool(".")),
		ReadTools:  tools.NewRegistry(tools.NewReadTool(".")),
		Cfg: Config{
			Project:           "proj",
			CardID:            "CARD-1",
			Branch:            "cm/card-1",
			BaseBranch:        "main",
			Workspace:         t.TempDir(),
			PayloadModel:      "payload/model",
			DefaultModel:      "default/model",
			MaxTurns:          5,
			ReviewAttemptsCap: 5,
		},
	}
}

// newReviewRun builds a review run with a parent task context and the configured
// ledger cap. The verify-command runner is stubbed to "no command detected" by
// default so tests that don't care about the gate skip it; tests that exercise
// the gate override runVerify.
func newReviewRun(d Deps, tc cmclient.TaskContext, maxCost float64) *run {
	d.Cfg.MaxCardCost = maxCost
	o := newRun(d, tc)
	o.cardTier = "moderate"
	// Default: no verify command, so the gate never runs in tests that ignore it.
	o.runVerify = func(context.Context, []string) (string, bool) { return "", true }

	return o
}

// reviewerCatalog seeds a catalog of reviewer-qualifying models plus the
// synthesis/coder fallback default.
func reviewerCatalog() llm.Catalog {
	return llm.Catalog{
		{ID: "rev/alpha", ContextLength: 200000, SupportedParameters: []string{"tools"}},
		{ID: "rev/beta", ContextLength: 200000, SupportedParameters: []string{"tools"}},
		{ID: "rev/gamma", ContextLength: 200000, SupportedParameters: []string{"tools"}},
		{ID: "rev/delta", ContextLength: 200000, SupportedParameters: []string{"tools"}},
		{ID: "default/model", ContextLength: 131072, SupportedParameters: []string{"tools"}},
		{ID: "pinned/model", ContextLength: 131072, SupportedParameters: []string{"tools"}},
	}
}

func reviewerRegistry() *registry.Registry {
	return registry.NewRegistryWithCapabilities(nil, "default/model", reviewerCatalog(),
		map[string]map[registry.Role]float64{
			"rev/alpha": {registry.RoleReviewer: 0.90},
			"rev/beta":  {registry.RoleReviewer: 0.88},
			"rev/gamma": {registry.RoleReviewer: 0.86},
			"rev/delta": {registry.RoleReviewer: 0.84},
		})
}

func TestDetectVerifyCommand(t *testing.T) {
	t.Run("Makefile with test target -> make test", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "Makefile", "build:\n\tgo build ./...\ntest:\n\tgo test ./...\n")

		got := detectVerifyCommand(dir)
		assert.Equal(t, []string{"make", "test"}, got)
	})

	t.Run("go.mod only -> go test ./...", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "go.mod", "module example.com/x\n\ngo 1.26\n")

		got := detectVerifyCommand(dir)
		assert.Equal(t, []string{"go", "test", "./..."}, got)
	})

	t.Run("package.json with scripts.test -> npm test", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{"name":"x","scripts":{"test":"jest"}}`)

		got := detectVerifyCommand(dir)
		assert.Equal(t, []string{"npm", "test"}, got)
	})

	t.Run("Makefile without test target falls through to go.mod", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "Makefile", "build:\n\tgo build ./...\n")
		writeFile(t, dir, "go.mod", "module example.com/x\n")

		got := detectVerifyCommand(dir)
		assert.Equal(t, []string{"go", "test", "./..."}, got)
	})

	t.Run("indented test: in a recipe is not a target", func(t *testing.T) {
		// Targets are column-0 in Make; an indented "test:" (recipe text, comment)
		// must not be detected as a test target.
		dir := t.TempDir()
		writeFile(t, dir, "Makefile", "build:\n\techo 'test: skipped'\n\ttest: foo\n")

		got := detectVerifyCommand(dir)
		assert.Nil(t, got)
	})

	t.Run("package.json without test script is not detected", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{"name":"x","scripts":{"build":"vite"}}`)

		got := detectVerifyCommand(dir)
		assert.Nil(t, got)
	})

	t.Run("none -> nil", func(t *testing.T) {
		dir := t.TempDir()
		got := detectVerifyCommand(dir)
		assert.Nil(t, got)
	})
}

func TestParseVerdict(t *testing.T) {
	t.Run("valid approved", func(t *testing.T) {
		v, err := parseVerdict(`{"approved":true,"summary":"all good","fixes":[]}`)
		require.NoError(t, err)
		assert.True(t, v.Approved)
		assert.Equal(t, "all good", v.Summary)
		assert.Empty(t, v.Fixes)
	})

	t.Run("junk-wrapped JSON", func(t *testing.T) {
		raw := "Here is my verdict:\n```json\n" +
			`{"approved":false,"summary":"needs work","fixes":[{"file":"a.go","issue":"bug","suggestion":"fix it"}]}` +
			"\n```\nThanks."
		v, err := parseVerdict(raw)
		require.NoError(t, err)
		assert.False(t, v.Approved)
		require.Len(t, v.Fixes, 1)
		assert.Equal(t, "a.go", v.Fixes[0].File)
	})

	t.Run("invalid JSON", func(t *testing.T) {
		_, err := parseVerdict("no json here at all")
		require.Error(t, err)
	})

	t.Run("malformed object", func(t *testing.T) {
		_, err := parseVerdict(`{"approved": "not-a-bool"`)
		require.Error(t, err)
	})
}

// TestFormatFixesFixFilesRoundTrip pins the line-shape contract between
// formatFixes (writer) and fixFiles (parser): every fix file path must survive
// the format -> parse round trip, deduplicated, in order.
func TestFormatFixesFixFilesRoundTrip(t *testing.T) {
	v := verdict{
		Summary: "needs work",
		Fixes: []fix{
			{File: "internal/api/health.go", Issue: "missing error wrap", Suggestion: "wrap with fmt.Errorf"},
			{File: "web/src/App.tsx", Issue: "stale prop", Suggestion: ""},
			{File: "internal/api/health.go", Issue: "second issue, same file", Suggestion: "dedupe me"},
		},
	}

	got := fixFiles(formatFixes(v))
	assert.Equal(t, []string{"internal/api/health.go", "web/src/App.tsx"}, got,
		"file paths must survive the formatFixes -> fixFiles round trip")
}

func TestReviewApprovedFirstPass(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	// Three specialist fan-out responses + one synthesis verdict (approved).
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: looks fine", 0.01),
		stopResp("Design: looks fine", 0.01),
		stopResp("Security: looks fine", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	calls := ops.recorded()
	// StartReview called exactly once.
	startCount := 0

	for _, c := range calls {
		if c == "StartReview:CARD-1" {
			startCount++
		}
	}

	assert.Equal(t, 1, startCount, "StartReview must be called exactly once; calls=%v", calls)

	// IncrementReviewAttempts NOT called on approval.
	assert.Equal(t, -1, indexOfCall(calls, "IncrementReviewAttempts:CARD-1"),
		"IncrementReviewAttempts must not be called on approval; calls=%v", calls)
}

func TestReviewSkipsStartReviewWhenAlreadyInReview(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness ok", 0.01),
		stopResp("Design ok", 0.01),
		stopResp("Security ok", 0.01),
		stopResp(`{"approved":true,"summary":"ok","fixes":[]}`, 0.01),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body", State: "review"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	assert.Equal(t, -1, indexOfCall(ops.recorded(), "StartReview:CARD-1"),
		"StartReview must be skipped when already in review")
}

func TestReviewFixLoop(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}
	// Round 1: 3 specialists + synthesis (fixes) -> fix coder run.
	// Round 2: 3 specialists + synthesis (approved).
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: bug", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":false,"summary":"fix it","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`, 0.02),
		stopResp("coder: fixed the bug", 0.05),
		stopResp("Correctness: ok now", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":true,"summary":"clean now","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	gitCalls := git.recorded()
	// Fixup committed and pushed.
	fixupIdx := indexOfPrefix(gitCalls, "CommitFixup:")
	pushIdx := indexOfCall(gitCalls, "Push:cm/card-1")
	require.GreaterOrEqual(t, fixupIdx, 0, "CommitFixup recorded; git=%v", gitCalls)
	require.GreaterOrEqual(t, pushIdx, 0, "Push recorded; git=%v", gitCalls)
	assert.Less(t, fixupIdx, pushIdx, "fixup before push")
	// LastCommitTouching consulted to find the fixup target, and it targeted the
	// commit it returned (abc123), not HEAD.
	assert.GreaterOrEqual(t, indexOfCall(gitCalls, "LastCommitTouching"), 0,
		"LastCommitTouching recorded; git=%v", gitCalls)
	assert.GreaterOrEqual(t, indexOfCall(gitCalls, "CommitFixup:abc123"), 0,
		"fixup must target the LastCommitTouching result; git=%v", gitCalls)
	// The fix file from the verdict reached LastCommitTouching.
	require.NotEmpty(t, git.lastCommitPaths)
	assert.Contains(t, git.lastCommitPaths[0], "a.go", "fix file must drive the fixup target lookup")

	// IncrementReviewAttempts called exactly once (one fix round).
	incCount := 0

	for _, c := range ops.recorded() {
		if c == "IncrementReviewAttempts:CARD-1" {
			incCount++
		}
	}

	assert.Equal(t, 1, incCount, "exactly one fix round; calls=%v", ops.recorded())
}

func TestReviewCapParks(t *testing.T) {
	// Seed the attempts counter one below the cap (5): the first non-approval
	// increments to exactly 5, pinning the >= boundary (n == cap parks).
	ops := &fakeOps{reviewAttempts: 4}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: bug", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":false,"summary":"still broken","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	err := runReview(context.Background(), o)

	var parked *ReviewParkedError
	require.ErrorAs(t, err, &parked, "cap exhaustion must return ReviewParkedError")
	// The findings must carry the actionable fix items, not just the summary.
	assert.Contains(t, parked.Findings, "still broken")
	assert.Contains(t, parked.Findings, "a.go", "findings must carry the fix file")
	assert.Contains(t, parked.Findings, "bug", "findings must carry the fix issue")

	calls := ops.recorded()
	// AddLog recorded with outstanding findings.
	logged := false

	for _, c := range calls {
		if strings.HasPrefix(c, "AddLog:") && strings.Contains(c, "still broken") {
			logged = true
		}
	}

	assert.True(t, logged, "AddLog must record outstanding findings; calls=%v", calls)

	// No fix run after the park: no CommitFixup.
	assert.Equal(t, -1, indexOfPrefix(git.recorded(), "CommitFixup:"),
		"no fix run after cap park; git=%v", git.recorded())
}

func TestReviewZeroCapDefaultsToConvention(t *testing.T) {
	// A mis-wired worker passing ReviewAttemptsCap 0 must NOT park the card on
	// the first non-approval (n=1 >= 0 would otherwise trip immediately); the
	// zero cap falls back to the convention (5), so the fix loop proceeds.
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		// Round 1: specialists + synthesis returns fixes.
		stopResp("Correctness: bug", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":false,"summary":"fix it","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`, 0.02),
		// Fix run, then round 2 approves.
		stopResp("coder: fixed", 0.05),
		stopResp("Correctness: ok now", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	d.Cfg.ReviewAttemptsCap = 0

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o),
		"zero cap must behave as the default cap, not park on the first non-approval")

	// The fix round ran (one increment), proving the loop did not park early.
	incCount := 0

	for _, c := range ops.recorded() {
		if c == "IncrementReviewAttempts:CARD-1" {
			incCount++
		}
	}

	assert.Equal(t, 1, incCount, "one fix round under the defaulted cap; calls=%v", ops.recorded())
}

func TestReviewPanelDiversity(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness ok", 0.01),
		stopResp("Design ok", 0.01),
		stopResp("Security ok", 0.01),
		stopResp(`{"approved":true,"summary":"ok","fixes":[]}`, 0.01),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	// The coder used rev/alpha on a subtask; the panel must exclude it.
	o.coderModels = map[string]bool{"rev/alpha": true}

	specs := o.reviewPanel(estimateTokens("diff"))
	require.Len(t, specs, 3)

	for _, s := range specs {
		assert.NotEqual(t, "rev/alpha", s.Model, "panel must exclude the coder model")
	}
}

func TestReviewPinOverridesPanel(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	client := &planLLM{}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{
		CardID: "CARD-1", Title: "Parent", Description: "body", State: "in_progress",
		ModelReviewer: "pinned/model",
	}
	o := newReviewRun(d, tc, 0)
	o.coderModels = map[string]bool{"rev/alpha": true}

	specs := o.reviewPanel(estimateTokens("diff"))
	require.Len(t, specs, 3)

	for _, s := range specs {
		assert.Equal(t, "pinned/model", s.Model, "reviewer pin must override the whole panel")
	}
}

func TestReviewGateFailureSkipsSpecialists(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	// Only a fix-coder response + a second-round synthesis. If specialists ran in
	// round 1 they would consume these and the assertions on the coder model and
	// LLM call sequence would break. Round 1 must skip the fan-out entirely.
	client := &planLLM{responses: []llm.Response{
		stopResp("coder: fixed", 0.05),
		// Round 2 after the fix: gate now passes (overridden below), specialists run.
		stopResp("Correctness ok", 0.01),
		stopResp("Design ok", 0.01),
		stopResp("Security ok", 0.01),
		stopResp(`{"approved":true,"summary":"ok","fixes":[]}`, 0.01),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	// Seed a go.mod so detectVerifyCommand finds a real command (go test ./...);
	// the stub below controls whether that gate passes or fails.
	writeFile(t, d.Cfg.Workspace, "go.mod", "module example.com/x\n")

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	// Gate fails on the first round, passes on every subsequent round.
	round := 0
	o.runVerify = func(context.Context, []string) (string, bool) {
		round++
		if round == 1 {
			return "FAIL: tests broke\nexit status 1", false
		}

		return "", true
	}

	require.NoError(t, runReview(context.Background(), o))

	// Round 1 went straight to the fix run: the FIRST LLM call must be the coder
	// fix run (not a specialist). The synthesis on round 1 never happened.
	require.NotEmpty(t, client.tasks)
	assert.Contains(t, client.tasks[0], "fix",
		"gate failure must drive the coder fix run first, not specialists; first task=%q", client.tasks[0])

	// One fix round happened.
	incCount := 0

	for _, c := range ops.recorded() {
		if c == "IncrementReviewAttempts:CARD-1" {
			incCount++
		}
	}

	assert.Equal(t, 1, incCount, "gate failure increments the attempt counter via the fix path")
}

func TestReviewBudgetParkBeforeSpecialists(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	client := &planLLM{}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body", State: "in_progress"}
	// Seed the ledger already at the ceiling so Check trips immediately.
	o := newReviewRun(d, tc, 0.01)
	o.ledger.Spend(0.01)

	err := runReview(context.Background(), o)

	var be *BudgetExceededError
	require.ErrorAs(t, err, &be, "review must park on budget before any model call")
	assert.Empty(t, client.tasks, "no model call once the budget is exhausted")
}

// indexOfPrefix returns the position of the first call whose value has the given
// prefix, or -1. Used for recorded calls that carry an argument suffix (e.g.
// "CommitFixup:HEAD").
func indexOfPrefix(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}

	return -1
}
