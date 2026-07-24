package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/mob"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-agent/internal/verifyexec"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
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
			Project:      "proj",
			CardID:       "CARD-1",
			Branch:       "cm/card-1",
			BaseBranch:   "main",
			Workspace:    t.TempDir(),
			PayloadModel: "payload/model",
			DefaultModel: "default/model",
			// Comfortably above wrapUpTurns (5): these single-turn fixtures must
			// finish before the one-shot nudge fires, or it becomes the captured
			// "last user message" instead of the real prompt. Tests that exercise
			// the turn cap or the nudge itself override this explicitly.
			MaxTurns:          20,
			ReviewAttemptsCap: 5,
		},
	}
}

// newReviewRun builds a review run with a parent task context and the configured
// ledger cap. The verify plan is pre-resolved to "skip" so tests that don't care
// about the gate never trigger resolution (which could otherwise propose a
// command via a model call); tests that exercise the gate set o.verify to a real
// plan and override runVerify.
func newReviewRun(d Deps, tc cmclient.TaskContext, maxCost float64) *run {
	d.Cfg.MaxCardCost = maxCost
	o := newRun(d, tc)
	o.cardTier = "moderate"
	// Default: pre-resolved skip, so ensureVerify is a cached no-op and no gate runs.
	isolateVerify(o)
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 0}
	}

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
	alpha, beta, gamma, delta := 0.90, 0.88, 0.86, 0.84
	priors := registry.Priors{
		Models: map[string]registry.PriorEntry{
			"rev/alpha": {Reviewer: &alpha},
			"rev/beta":  {Reviewer: &beta},
			"rev/gamma": {Reviewer: &gamma},
			"rev/delta": {Reviewer: &delta},
		},
	}

	return registry.NewRegistryFromParts(reviewerCatalog(), priors, nil, nil, "default/model")
}

// TestReviewSubagentsInheritRouting pins the harness v0.7.x propagation
// contract: the three review specialists (SubagentOpts) and the synthesis call
// (harnessConfig) all carry the parent run's provider and reasoning routing,
// derived from the same builder so they can never drift.
func TestReviewSubagentsInheritRouting(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: fine", 0.01),
		stopResp("Design: fine", 0.01),
		stopResp("Security: fine", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	d.Cfg.ReasoningEffort = "high"
	d.Cfg.Provider = json.RawMessage(`{"require_parameters":true}`)

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	client.mu.Lock()
	defer client.mu.Unlock()

	require.Len(t, client.providers, 4, "3 specialists + 1 synthesis")

	for i := range client.providers {
		assert.JSONEq(t, `{"require_parameters":true}`, string(client.providers[i]), "call %d must carry the provider routing", i)
		assert.JSONEq(t, `{"effort":"high"}`, string(client.reasonings[i]), "call %d must carry the reasoning config", i)
	}
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

	t.Run("bare JSON with in-string fence", func(t *testing.T) {
		raw := "{\"approved\":false,\"summary\":\"tighten the guard: ```go\\nfoo()\\n``` as discussed\",\"fixes\":[]}"
		v, err := parseVerdict(raw)
		require.NoError(t, err)
		assert.False(t, v.Approved)
		assert.Contains(t, v.Summary, "```go\nfoo()\n```")
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

func TestParseVerdictReadsFixTier(t *testing.T) {
	v, err := parseVerdict(`{"approved":false,"summary":"s","fix_tier":"moderate","fixes":[]}`)
	require.NoError(t, err)
	assert.Equal(t, "moderate", v.FixTier)
}

// TestResolveFixModelUsesFixTier proves the fix run sizes its coder on the
// synthesizer's fix_tier, not the card tier. The registry seeds one cheap coder
// model whose prior (0.70) clears the simple tier bar (0.65) but sits below the
// complex bar (0.82), with a DISTINCT capable fallback. So resolveFixModel("simple")
// must pick the cheap coder; resolveFixModel("complex") must fall back.
func TestResolveFixModelUsesFixTier(t *testing.T) {
	const (
		cheapCoder = "cheap/coder"
		fallback   = "capable/fallback"
	)

	catalog := llm.Catalog{
		{ID: cheapCoder, ContextLength: 200000, PromptPricePerTok: 0.0000005, CompletionPricePerTok: 0.0000015, SupportedParameters: []string{"tools"}},
		{ID: fallback, ContextLength: 200000, PromptPricePerTok: 0.000006, CompletionPricePerTok: 0.000012, SupportedParameters: []string{"tools"}},
	}
	// Prior between the default simple (0.65) and complex (0.82) bars.
	prior := 0.70
	priors := registry.Priors{
		Models: map[string]registry.PriorEntry{cheapCoder: {Coder: &prior}},
	}
	reg := registry.NewRegistryFromParts(catalog, priors, nil, nil, fallback)

	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reg)
	// No coder pin -> complexity selection path; card tier is moderate by default.
	o := newReviewRun(d, cmclient.TaskContext{}, 0)

	assert.Equal(t, cheapCoder, o.resolveFixModel("simple", false),
		"simple fix_tier clears the cheap coder's bar")
	assert.Equal(t, fallback, o.resolveFixModel("complex", false),
		"complex fix_tier excludes the cheap coder -> capable fallback")
	assert.NotEqual(t, cheapCoder, o.resolveFixModel("complex", false))
}

// TestResolveFixModelAuthoritativeForcesComplex proves the authoritative pass
// sizes the fix coder on the complex tier regardless of the synthesizer's
// fix_tier. The cheap coder clears the simple bar (0.65) but not the complex bar
// (0.82); so resolveFixModel("simple", false) picks it, but the authoritative
// resolveFixModel("simple", true) escalates to complex, gating the cheap coder
// out and falling back to the capable model.
func TestResolveFixModelAuthoritativeForcesComplex(t *testing.T) {
	const (
		cheapCoder = "cheap/coder"
		fallback   = "capable/fallback"
	)

	catalog := llm.Catalog{
		{ID: cheapCoder, ContextLength: 200000, PromptPricePerTok: 0.0000005, CompletionPricePerTok: 0.0000015, SupportedParameters: []string{"tools"}},
		{ID: fallback, ContextLength: 200000, PromptPricePerTok: 0.000006, CompletionPricePerTok: 0.000012, SupportedParameters: []string{"tools"}},
	}
	// Prior between the default simple (0.65) and complex (0.82) bars.
	prior := 0.70
	priors := registry.Priors{
		Models: map[string]registry.PriorEntry{cheapCoder: {Coder: &prior}},
	}
	reg := registry.NewRegistryFromParts(catalog, priors, nil, nil, fallback)

	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reg)
	o := newReviewRun(d, cmclient.TaskContext{}, 0)

	assert.Equal(t, cheapCoder, o.resolveFixModel("simple", false),
		"non-authoritative simple fix_tier clears the cheap coder's bar")
	assert.Equal(t, fallback, o.resolveFixModel("simple", true),
		"authoritative pass forces complex -> cheap coder excluded -> capable fallback")
	assert.NotEqual(t, cheapCoder, o.resolveFixModel("simple", true))
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

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
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

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "review"}
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

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
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

// TestReviewFixMaxTurnsAborts pins that a fix run truncated at the turn cap is
// NOT fixup-committed and pushed as if the findings were addressed.
func TestReviewFixMaxTurnsAborts(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}
	call := llm.ToolCall{
		ID:       "c1",
		Type:     "function",
		Function: llm.FunctionCall{Name: "read", Arguments: `{"path":"no-such-file.txt"}`},
	}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: bug found", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":false,"summary":"needs fix","fixes":[{"file":"a.go","issue":"bug"}]}`, 0.02),
		{ToolCalls: []llm.ToolCall{call}}, // fix coder burns its single turn -> max_turns
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	d.Cfg.MaxTurns = 1

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "review"}
	o := newReviewRun(d, tc, 0)

	err := runReview(context.Background(), o)
	require.Error(t, err)

	var mte *MaxTurnsError
	require.ErrorAs(t, err, &mte)

	for _, c := range git.recorded() {
		assert.False(t, strings.HasPrefix(c, "CommitFixup"), "truncated fix fixup-committed: %v", git.recorded())
		assert.False(t, strings.HasPrefix(c, "Push:"), "truncated fix pushed: %v", git.recorded())
	}
}

// TestFixRunTierScalesTurnBudget proves a complex fix tier lifts the fix coder
// turn budget above the flat base: 25 turns (more than the base of 20, fewer
// than the complex budget of 30 = 1.5x base) run to completion instead of
// capping mid-way.
func TestFixRunTierScalesTurnBudget(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: burnResps(25)}

	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	d.Cfg.MaxTurns = 20
	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "review"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, o.runFixModel(context.Background(), "fix prompt", 1, "complex", false),
		"a complex fix tier scales the budget above the base, so 25 turns do not cap")
}

// TestFixRunSimpleTierCapsAtBase proves a simple fix tier is NOT scaled: the
// same 25 turns cap at the flat base of 20.
func TestFixRunSimpleTierCapsAtBase(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: burnResps(25)}

	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	d.Cfg.MaxTurns = 20
	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "review"}
	o := newReviewRun(d, tc, 0)

	err := o.runFixModel(context.Background(), "fix prompt", 1, "simple", false)
	require.Error(t, err, "a simple fix tier keeps the flat base, so 25 turns cap")

	var mte *MaxTurnsError
	require.ErrorAs(t, err, &mte)
}

func TestReviewFixCoderSelectionLogged(t *testing.T) {
	// Round 1 is not approved -> fix coder run -> round 2 approves. The fix run
	// must announce the selected coder model, the round number, and the tier on
	// the activity log (mirrors the review panel-models log).
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}
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

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	// Find the fix-coder selection line for round 1: the message shape must match
	// the panel-models log style without hinging on a specific tier value.
	var selection string

	for _, m := range ops.logs {
		if strings.Contains(m, "fix coder ") &&
			strings.Contains(m, "selected for round 1 fixes") &&
			strings.Contains(m, "(tier=") {
			selection = m

			break
		}
	}

	require.NotEmpty(t, selection,
		"fix run must log the coder selection for round 1; logs=%v", ops.logs)
}

func TestReviewRoundTwoDiffsAgainstSnapshot(t *testing.T) {
	// Round 1 reviews the full branch (Diff base == BaseBranch). It is not
	// approved, a fix lands, and round 2 reviews only the change since round 1 by
	// diffing against the reviewed head captured at the end of round 1's review.
	ops := &fakeOps{}
	git := &fakeGit{committed: true, headSHA: "sha-reviewed-1"}
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

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	require.GreaterOrEqual(t, len(git.diffBases), 2,
		"two specialist rounds must each diff once; diffBases=%v", git.diffBases)
	assert.Equal(t, d.Cfg.BaseBranch, git.diffBases[0],
		"round 1 must diff the full branch against the base branch")
	assert.Equal(t, "sha-reviewed-1", git.diffBases[1],
		"round 2 must diff the delta against the round-1 reviewed head")
}

func TestReviewNoOpFixWidensNextRoundToBaseBranch(t *testing.T) {
	// A fix round that commits nothing (the cheap coder made no edits) must not
	// leave the stale reviewed-head snapshot as the next round's diff base - that
	// makes round 2 diff HEAD...HEAD (empty), hiding the unresolved finding and
	// letting an empty-delta panel spuriously approve. The next round must
	// re-widen to the full base-branch diff.
	ops := &fakeOps{}
	git := &fakeGit{committed: false, headSHA: "sha-reviewed-1"}
	client := &planLLM{responses: []llm.Response{
		// Round 1: specialists flag a bug, synthesis returns a fix.
		stopResp("Correctness: bug", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":false,"summary":"fix it","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`, 0.02),
		// Fix coder run makes no edits (git.committed == false).
		stopResp("coder: could not locate the issue", 0.05),
		// Round 2: specialists + synthesis.
		stopResp("Correctness: ok", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	require.GreaterOrEqual(t, len(git.diffBases), 2,
		"two specialist rounds must each diff once; diffBases=%v", git.diffBases)
	assert.Equal(t, d.Cfg.BaseBranch, git.diffBases[1],
		"after a no-op fix, round 2 must re-widen to the base branch, not the stale reviewed-head snapshot")
}

func TestReviewPriorFindingsFedToNextRound(t *testing.T) {
	// Round 1 is not approved with a recognizable finding (delta.go / nil deref);
	// round 2 approves. The round-2 specialist panel must receive the round-1
	// findings as a PRIOR FINDINGS block (cross-round memory), and round 1 - with
	// no prior - must not carry that block.
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}
	client := &planLLM{responses: []llm.Response{
		// Round 1: specialists + synthesis returns the distinctive finding.
		stopResp("Correctness: bug", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":false,"summary":"fix it","fixes":[{"file":"delta.go","issue":"nil deref","suggestion":"guard the pointer"}]}`, 0.02),
		// Fix run, then round 2 approves.
		stopResp("coder: fixed", 0.05),
		stopResp("Correctness: ok now", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":true,"summary":"clean now","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	// Partition the captured specialist prompts into round 1 (before the fix coder
	// run) and round 2 (after it). The fix coder task is the one addressing review
	// feedback; specialists are the "code-review specialist" prompts.
	fixIdx := -1

	for i, task := range client.tasks {
		if strings.Contains(task, "addressing review feedback") {
			fixIdx = i

			break
		}
	}

	require.GreaterOrEqual(t, fixIdx, 0, "fix coder run must appear in captured tasks; tasks=%v", client.tasks)

	var round1Specialists, round2Specialists []string

	for i, task := range client.tasks {
		if !strings.Contains(task, "code-review specialist") {
			continue
		}

		if i < fixIdx {
			round1Specialists = append(round1Specialists, task)
		} else {
			round2Specialists = append(round2Specialists, task)
		}
	}

	require.Len(t, round1Specialists, 3, "round 1 fans out three specialists")
	require.Len(t, round2Specialists, 3, "round 2 fans out three specialists")

	// Round 1 has no prior round: no PRIOR FINDINGS block.
	for _, task := range round1Specialists {
		assert.NotContains(t, task, "PRIOR FINDINGS",
			"round 1 has no prior findings; specialist prompt must not carry the block")
	}

	// Round 2 must carry the round-1 findings (delta.go / nil deref) framed as
	// PRIOR FINDINGS so the panel verifies resolution without re-raising scope.
	carried := false

	for _, task := range round2Specialists {
		if strings.Contains(task, "PRIOR FINDINGS") &&
			strings.Contains(task, "delta.go") &&
			strings.Contains(task, "nil deref") {
			carried = true
		}
	}

	assert.True(t, carried,
		"round 2 specialist prompt must carry the round-1 findings under PRIOR FINDINGS; round2=%v", round2Specialists)
}

func TestReviewCapParks(t *testing.T) {
	// At the review cliff the gated authoritative pass runs instead of parking on a
	// cheap verdict: one strong review (rejects), ONE strong fix, one strong
	// re-review (still rejects) -> park with the SECOND (strong) review's findings.
	// Seed tc.ReviewAttempts = cap-1 (4) so iter 0 is the authoritative round, and
	// ops.reviewAttempts = 4 so the running totals mirror the persisted count.
	ops := &fakeOps{reviewAttempts: 4}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		// Authoritative review 1: 3 specialists + synthesis (rejects).
		stopResp("Correctness: bug", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":false,"summary":"fix it","fixes":[{"file":"x.go","issue":"first","suggestion":"patch"}]}`, 0.02),
		// Gated strong fix run.
		stopResp("coder: attempted fix", 0.05),
		// Authoritative re-review: 3 specialists + synthesis (still rejects).
		stopResp("Correctness: still bug", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":false,"summary":"still broken","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{
		Title: "Parent", Description: "body",
		State: "in_progress", ReviewAttempts: 4,
	}
	o := newReviewRun(d, tc, 0)

	err := runReview(context.Background(), o)

	var parked *ReviewParkedError
	require.ErrorAs(t, err, &parked, "cap exhaustion must return ReviewParkedError")

	calls := ops.recorded()
	// AddLog recorded with the SECOND (strong) re-review's outstanding
	// findings, not the first: summary, fix file, and fix issue.
	logged := false

	for _, c := range calls {
		if strings.HasPrefix(c, "AddLog:") && strings.Contains(c, "still broken") &&
			strings.Contains(c, "a.go") && strings.Contains(c, "bug") {
			logged = true
		}
	}

	assert.True(t, logged, "AddLog must record the strong re-review's findings; calls=%v", calls)

	// Exactly ONE fix run (the gated strong fix) happened.
	fixupCount := 0

	for _, c := range git.recorded() {
		if strings.HasPrefix(c, "CommitFixup:") {
			fixupCount++
		}
	}

	assert.Equal(t, 1, fixupCount, "exactly one gated fix run before park; git=%v", git.recorded())
}

func TestReviewAuthoritativeApprovesNoFix(t *testing.T) {
	// At the cliff the authoritative pass runs and APPROVES on the first strong
	// review: runReview finishes nil and NO fix runs (the gated fix is reserved for
	// confirmed issues only).
	ops := &fakeOps{reviewAttempts: 4}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: clean", 0.01),
		stopResp("Design: clean", 0.01),
		stopResp("Security: clean", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{
		Title: "Parent", Description: "body",
		State: "in_progress", ReviewAttempts: 4,
	}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o),
		"authoritative approval must finish the card")

	assert.Equal(t, -1, indexOfPrefix(git.recorded(), "CommitFixup:"),
		"no fix when the authoritative review approves; git=%v", git.recorded())
}

func TestReviewAuthoritativeFullScope(t *testing.T) {
	// The cliff re-widens to full scope even when a delta snapshot is set. iter 0
	// is an INCREMENTAL round (cap-2 seed): it rejects, lands a fix, and captures
	// the reviewed head as the next round's delta base. iter 1 is authoritative and
	// must IGNORE that snapshot, diffing the full branch against the base again.
	ops := &fakeOps{reviewAttempts: 3}
	git := &fakeGit{committed: true, headSHA: "snap1"}
	client := &planLLM{responses: []llm.Response{
		// Incremental round (iter 0): 3 specialists + synthesis (rejects) -> fix.
		stopResp("Correctness: bug", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":false,"summary":"fix it","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`, 0.02),
		stopResp("coder: fixed", 0.05),
		// Authoritative round (iter 1): 3 specialists + synthesis (approves).
		stopResp("Correctness: ok", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{
		Title: "Parent", Description: "body",
		State: "in_progress", ReviewAttempts: 3,
	}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	require.GreaterOrEqual(t, len(git.diffBases), 2,
		"both rounds must each diff once; diffBases=%v", git.diffBases)
	assert.Equal(t, "main", git.diffBases[0],
		"incremental round 1 has no prior snapshot -> diffs the base branch")
	assert.Equal(t, "main", git.diffBases[1],
		"authoritative round must re-widen to the base branch despite lastReviewBase==snap1")
}

func TestReviewZeroCapDefaultsToConvention(t *testing.T) {
	// A mis-wired worker passing ReviewAttemptsCap 0 must NOT park the card on
	// the first non-approval (n=1 >= 0 would otherwise trip immediately); the
	// zero cap falls back to the convention (3), so the fix loop proceeds.
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

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
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

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	// The coder used rev/alpha on a subtask; the panel must exclude it.
	o.coderModels = map[string]bool{"rev/alpha": true}

	specs := o.reviewPanel(estimateTokens("diff"), false)
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
		Title: "Parent", Description: "body", State: "in_progress",
		ModelReviewer: "pinned/model",
	}
	o := newReviewRun(d, tc, 0)
	o.coderModels = map[string]bool{"rev/alpha": true}

	specs := o.reviewPanel(estimateTokens("diff"), false)
	require.Len(t, specs, 3)

	for _, s := range specs {
		assert.Equal(t, "pinned/model", s.Model, "reviewer pin must override the whole panel")
	}
}

// TestReviewPanelEscalatesWhenAuthoritative proves the authoritative pass sizes
// the panel on the complex tier, not the card tier. Three cheap-but-weak
// reviewers clear the moderate bar (0.6) but not the complex bar (0.8); one
// expensive strong reviewer clears both. At moderate the cheap trio fills the
// three slots (the strong model is priced out of the band), so it never appears.
// At complex the weak trio is gated out, forcing the strong model in - a model
// the moderate panel does not select.
func TestReviewPanelEscalatesWhenAuthoritative(t *testing.T) {
	const strong = "acme/strong-reviewer"

	catalog := llm.Catalog{
		{ID: "acme/weak-one", ContextLength: 200000, PromptPricePerTok: 0.0000004, CompletionPricePerTok: 0.0000006, SupportedParameters: []string{"tools"}},
		{ID: "acme/weak-two", ContextLength: 200000, PromptPricePerTok: 0.00000045, CompletionPricePerTok: 0.00000065, SupportedParameters: []string{"tools"}},
		{ID: "acme/weak-three", ContextLength: 200000, PromptPricePerTok: 0.0000005, CompletionPricePerTok: 0.0000007, SupportedParameters: []string{"tools"}},
		{ID: strong, ContextLength: 200000, PromptPricePerTok: 0.000005, CompletionPricePerTok: 0.000005, SupportedParameters: []string{"tools"}},
		{ID: "default/model", ContextLength: 131072, SupportedParameters: []string{"tools"}},
	}
	// The weak trio clears the default moderate bar (0.76) but not complex (0.82);
	// the strong model clears complex (0.82). So the moderate panel is the cheap
	// trio and the complex escalation forces the strong model in. A single
	// vendor prefix keeps the vendor-diversity preference out of the picture -
	// this test isolates tier escalation.
	w1, w2, w3, st := 0.77, 0.78, 0.79, 0.90
	priors := registry.Priors{
		Models: map[string]registry.PriorEntry{
			"acme/weak-one":   {Reviewer: &w1},
			"acme/weak-two":   {Reviewer: &w2},
			"acme/weak-three": {Reviewer: &w3},
			strong:            {Reviewer: &st},
		},
	}
	reg := registry.NewRegistryFromParts(catalog, priors, nil, nil, "default/model")

	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reg)
	o := newReviewRun(d, cmclient.TaskContext{}, 0)
	o.cardTier = "moderate" // no reviewer pin -> selection path

	est := estimateTokens("diff")

	moderatePanel := o.reviewPanel(est, false)
	require.Len(t, moderatePanel, 3)

	complexPanel := o.reviewPanel(est, true)
	require.Len(t, complexPanel, 3)

	moderateModels := map[string]bool{}
	for _, s := range moderatePanel {
		moderateModels[s.Model] = true
	}

	// The moderate panel never reaches the strong (expensive) model.
	assert.NotContains(t, moderateModels, strong,
		"moderate panel must be filled by the cheap trio; panel=%v", moderatePanel)

	// The complex escalation must select at least one model the moderate panel did
	// not - here the strong model, which only clears the complex bar.
	escalated := false

	for _, s := range complexPanel {
		if !moderateModels[s.Model] {
			escalated = true
		}
	}

	assert.True(t, escalated,
		"authoritative (complex) panel must pick a higher model the moderate panel does not; moderate=%v complex=%v",
		moderatePanel, complexPanel)
	assert.Contains(t, []string{complexPanel[0].Model, complexPanel[1].Model, complexPanel[2].Model}, strong,
		"complex bar gates out the weak trio, forcing the strong model in; complex=%v", complexPanel)
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

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	// Opt into a real gate: a detected command the stub below drives.
	o.verify = &verifyPlan{Argv: []string{"verify"}, Display: "verify", Source: verifySourceDetected, Timeout: time.Minute}

	// Gate fails on the first round, passes on every subsequent round.
	round := 0
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		round++
		if round == 1 {
			return verifyexec.Outcome{ExitCode: 1, Output: "FAIL: tests broke\nexit status 1"}
		}

		return verifyexec.Outcome{ExitCode: 0}
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

func TestReviewGateSkippedProceedsUnverified(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	// Gate skips -> specialists run and approve; no fix coder is invoked.
	client := &planLLM{responses: []llm.Response{
		stopResp("No concerns.", 0.01), stopResp("No concerns.", 0.01), stopResp("No concerns.", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.01),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "P", Description: "b", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.verify = &verifyPlan{Argv: []string{"verify"}, Display: "verify", Source: verifySourceDetected, Timeout: time.Minute}
	// The verify tool is missing -> a skipped (inconclusive) gate.
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		return verifyexec.Outcome{StartErr: true, ExitCode: -1}
	}

	findings, _, approved, vres, err := o.reviewRound(context.Background(), *o.verify, 1, false)
	require.NoError(t, err)
	assert.Equal(t, verifySkipped, vres.Status)
	assert.True(t, approved, "a skipped gate proceeds to the specialists, which approve")
	assert.NotEmpty(t, findings)
	assert.True(t, ops.loggedContains("verify skipped"), "the skip is logged loudly; logs=%v", ops.logs)
	assert.Len(t, client.tasks, 4, "a skipped gate runs the full panel (3 specialists + synthesis), not a fix loop")
}

func TestReviewGateFailureRedactsFindings(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	client := &planLLM{}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	d.Redact = func(s string) string { return strings.ReplaceAll(s, "SECRETTOKEN", "[MASKED]") }

	tc := cmclient.TaskContext{Title: "P", Description: "b", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.verify = &verifyPlan{Argv: []string{"verify"}, Display: "verify", Source: verifySourceDetected, Timeout: time.Minute}
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 1, Output: "auth error: SECRETTOKEN leaked in the log"}
	}

	findings, _, approved, vres, err := o.reviewRound(context.Background(), *o.verify, 1, false)
	require.NoError(t, err)
	assert.False(t, approved)
	assert.Equal(t, verifyFailed, vres.Status)
	assert.Contains(t, findings, "[MASKED]", "the verify output is redacted before it enters the findings")
	assert.NotContains(t, findings, "SECRETTOKEN", "a secret must never reach the fix prompt or the activity log")
	assert.Empty(t, client.tasks, "a gate failure short-circuits to the fix loop before any reviewer model call")
}

func TestReviewBudgetParkBeforeSpecialists(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	client := &planLLM{}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
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

// hitlReviewDeps builds Deps for HITL review tests with both tool registries and
// an injected inbox; the scripted client serves specialist + synthesis + gate
// classification turns.
func hitlReviewDeps(ops *fakeOps, git *fakeGit, inbox *fakeInbox, client llm.LLM) Deps {
	return Deps{
		Ops:        ops,
		Git:        git,
		Client:     client,
		Emit:       events.NewEmitter(nil, nil),
		Registry:   planTestRegistry(),
		WriteTools: tools.NewRegistry(tools.NewReadTool(".")),
		ReadTools:  tools.NewRegistry(tools.NewReadTool(".")),
		Human:      inbox,
		Cfg: Config{
			Project: "proj", CardID: "CARD-1", Branch: "cm/card-1", BaseBranch: "main",
			PayloadModel: "payload/model", DefaultModel: "default/model",
			// Comfortably above wrapUpTurns (5): these single-turn fixtures must
			// finish before the one-shot nudge fires, or it becomes the captured
			// "last user message" instead of the real prompt. Tests that exercise
			// the turn cap or the nudge itself override this explicitly.
			MaxTurns: 20, ReviewAttemptsCap: 3, Interactive: true,
		},
	}
}

func TestRunReviewHITLApproveProceeds(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{} // no go.mod in cwd -> no verify gate
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "approve"}}}
	// Three specialists (no-concern findings) + one synthesis (approved) + gate approve.
	client := &planLLM{responses: []llm.Response{
		stopResp("No concerns.", 0.001),
		stopResp("No concerns.", 0.001),
		stopResp("No concerns.", 0.001),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.001),
		stopResp(`{"verdict":"approve","feedback":""}`, 0.001),
	}}
	o := newRun(hitlReviewDeps(ops, git, inbox, client), cmclient.TaskContext{Title: "T", Description: "b", State: "review"})
	isolateVerify(o)

	require.NoError(t, runReview(context.Background(), o))
	assert.Equal(t, 0, countCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"), "approve does not increment attempts")
}

func TestRunReviewHITLAdjustFixesThenApproves(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	inbox := &fakeInbox{msgs: []harness.UserMessage{
		{Content: "tighten error handling in a.go"},
		{Content: "approve"},
	}}
	client := &planLLM{responses: []llm.Response{
		// Round 1: specialists + synthesis (approved, but the human adjusts anyway).
		stopResp("No concerns.", 0.001), stopResp("No concerns.", 0.001), stopResp("No concerns.", 0.001),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.001),
		stopResp(`{"verdict":"adjust","feedback":"tighten error handling in a.go"}`, 0.001), // gate -> adjust
		stopResp("Fixed.", 0.001), // fix coder
		// Round 2: specialists + synthesis + gate approve.
		stopResp("No concerns.", 0.001), stopResp("No concerns.", 0.001), stopResp("No concerns.", 0.001),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.001),
		stopResp(`{"verdict":"approve","feedback":""}`, 0.001),
	}}
	o := newRun(hitlReviewDeps(ops, git, inbox, client), cmclient.TaskContext{Title: "T", Description: "b", State: "review"})
	isolateVerify(o)

	require.NoError(t, runReview(context.Background(), o))
	assert.GreaterOrEqual(t, countCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"), 1, "an adjust increments attempts and runs a fix")
	assert.NotEmpty(t, git.pushBranches, "the fix round pushed a fixup")
}

// countCall counts how many entries in calls equal name.
func countCall(calls []string, name string) int {
	n := 0

	for _, c := range calls {
		if c == name {
			n++
		}
	}

	return n
}

// mobReviewRun builds a review run with mob session review enabled and a
// scripted engine.
func mobReviewRun(t *testing.T, ops *fakeOps, git *fakeGit, client llm.LLM, eng *scriptedEngine) *run {
	t.Helper()

	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	d.Cfg.Mob = MobConfig{Participants: 3, Review: true, Rounds: 2, BudgetFactor: 0.75}

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.mobEngine = eng.run

	return o
}

const mobApprovedVerdict = `{"approved":true,"summary":"clean","fix_tier":"","fixes":[]}`

func TestReviewMobApprovedFirstPass(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	llmFake := &planLLM{}
	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: mobApprovedVerdict, Consensus: true}}}

	o := mobReviewRun(t, ops, git, llmFake, eng)
	require.NoError(t, runReview(context.Background(), o))

	// The discussion replaced the whole specialist pass: zero LLM calls.
	assert.Empty(t, llmFake.tasks, "no specialist or synthesis model calls on the mob session path")
	assert.Equal(t, "clean", o.reviewSummary)
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"))

	// The review topic carried the review knobs and the diff-scoped briefing.
	require.Len(t, eng.topics, 1)
	topic := eng.topics[0]
	assert.Equal(t, "review", topic.Kind)
	assert.True(t, topic.Blind)
	assert.Equal(t, 1, topic.Rounds, "review discussions are one rebuttal round")
	assert.Equal(t, reviewLenses[:3], topic.Lenses)
	assert.Contains(t, topic.SynthesisPrompt, `"approved"`)
}

func TestReviewMobRejectWithFixesRunsFixLoop(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}
	// Only LLM call: the fix coder run between the two discussion rounds.
	llmFake := &planLLM{responses: []llm.Response{
		stopResp("coder: fixed the bug", 0.05),
	}}
	eng := &scriptedEngine{outcomes: []mob.Outcome{
		{Synthesis: `{"approved":false,"summary":"fix it","fix_tier":"simple",` +
			`"fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`},
		{Synthesis: mobApprovedVerdict, Consensus: true},
	}}

	o := mobReviewRun(t, ops, git, llmFake, eng)
	require.NoError(t, runReview(context.Background(), o))

	require.Len(t, eng.topics, 2, "round 2 re-convenes the discussion")

	incCount := 0

	for _, c := range ops.recorded() {
		if c == "IncrementReviewAttempts:CARD-1" {
			incCount++
		}
	}

	assert.Equal(t, 1, incCount, "exactly one fix round")
	assert.GreaterOrEqual(t, indexOfPrefix(git.recorded(), "CommitFixup:"), 0, "the fix landed as a fixup")
}

func TestReviewMobFallsBackToSpecialists(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	// Solo fallback path: 3 specialists + 1 synthesis.
	llmFake := &planLLM{responses: []llm.Response{
		stopResp("Correctness: looks fine", 0.01),
		stopResp("Design: looks fine", 0.01),
		stopResp("Security: looks fine", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
	}}
	eng := &scriptedEngine{outcomes: []mob.Outcome{{}}, errs: []error{mob.ErrNoQuorum}}

	o := mobReviewRun(t, ops, git, llmFake, eng)
	require.NoError(t, runReview(context.Background(), o))

	assert.Len(t, llmFake.tasks, 4, "the specialist pass ran after the discussion degraded")
}

func TestReviewMobPassesExclusionsAndDeltaScope(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	llmFake := &planLLM{}
	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: mobApprovedVerdict, Consensus: true}}}

	o := mobReviewRun(t, ops, git, llmFake, eng)
	o.coderModels = map[string]bool{"rev/alpha": true}
	o.lastReviewBase = "snapshot-sha"
	o.lastFindings = "prior finding about a.go"

	require.NoError(t, runReview(context.Background(), o))

	// The coder's model never takes a seat.
	require.Len(t, eng.cfgs, 1)

	for _, s := range eng.cfgs[0].Seats {
		assert.NotEqual(t, "rev/alpha", s.Model, "review seats must exclude the coder's model")
	}

	// Delta scoping feeds the briefing unchanged: diff against the snapshot,
	// prior findings included. fakeGit captures diff bases in diffBases.
	assert.Contains(t, git.diffBases, "snapshot-sha",
		"the briefing diff uses lastReviewBase; bases=%v", git.diffBases)
	assert.Contains(t, eng.topics[0].Briefing, "prior finding about a.go")
}
