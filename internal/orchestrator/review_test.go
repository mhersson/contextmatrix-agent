package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	o.cardSizing = seedSizing("moderate")
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

// TestParseVerdictNormalizesSeverity proves Severity is validated at
// parse-verdict time: a recognized value (title-cased, as the specialist
// severity scale actually produces it) lower-cases into the vocabulary, and a
// value outside it - here one crafted with an embedded newline - normalizes to
// "" before it ever reaches formatFixes. It round-trips through
// parseVerdict -> formatFixes -> fixFiles to prove the malicious value cannot
// inject a synthetic fix line that fixFiles' first-colon re-parse would treat
// as a real file path.
func TestParseVerdictNormalizesSeverity(t *testing.T) {
	raw := `{"approved":false,"summary":"needs work","fixes":[` +
		`{"file":"a.go","issue":"bug","suggestion":"patch","severity":"Critical"},` +
		`{"file":"b.go","issue":"bug2","suggestion":"patch2","severity":"nonsense\n- evil.go: injected"}]}`

	v, err := parseVerdict(raw)
	require.NoError(t, err)
	require.Len(t, v.Fixes, 2)

	assert.Equal(t, "critical", v.Fixes[0].Severity, "a recognized, title-cased value lower-cases into the vocabulary")
	assert.Empty(t, v.Fixes[1].Severity, "a value outside the vocabulary (here, one carrying an embedded newline) normalizes to empty")

	files := fixFiles(formatFixes(v))
	assert.Equal(t, []string{"a.go", "b.go"}, files,
		"the malicious severity must not inject a synthetic fixFiles-parsed file path; got=%v", files)
}

// fixTierCoder is measured between the default simple bar (0.65) and the
// moderate (0.76) and complex (0.82) bars; fixTierFallback is the capable
// default and carries no prior at all. Every fix-model test below reads that
// one relationship, so it is stated once here.
const (
	fixTierCoder    = "cheap/coder"
	fixTierFallback = "capable/fallback"
)

func fixTierCatalog() llm.Catalog {
	return llm.Catalog{
		{ID: fixTierCoder, ContextLength: 200000, PromptPricePerTok: 0.0000005, CompletionPricePerTok: 0.0000015, SupportedParameters: []string{"tools"}},
		{ID: fixTierFallback, ContextLength: 200000, PromptPricePerTok: 0.000006, CompletionPricePerTok: 0.000012, SupportedParameters: []string{"tools"}},
	}
}

func fixTierPriors() registry.Priors {
	prior := 0.70

	return registry.Priors{
		Models: map[string]registry.PriorEntry{fixTierCoder: {Coder: &prior}},
	}
}

func fixTierRun(t *testing.T) *run {
	t.Helper()

	reg := registry.NewRegistryFromParts(fixTierCatalog(), fixTierPriors(), nil, nil, fixTierFallback)
	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reg)

	// No coder pin, so every case goes down the complexity-selection path.
	return newReviewRun(d, cmclient.TaskContext{}, 0)
}

// TestResolveFixModelRequestsTheRightTier pins what the synthesizer's fix_tier
// and the authoritative flag actually control: the tier the fix coder is
// REQUESTED at. What that request buys is a separate fact, and the pick
// carries it. Only fixTierCoder is measured, at 0.70, so any request above
// that bar walks down to the simple rung and reports the shortfall through
// MetTier and AtBar. The unmeasured capable fallback is the floor below the
// ladder, not the answer to a quality question.
func TestResolveFixModelRequestsTheRightTier(t *testing.T) {
	tests := []struct {
		name          string
		fixTier       string
		authoritative bool
		wantRequested registry.Tier
		wantMet       registry.Tier
		wantAtBar     bool
	}{
		{
			name:          "simple fix_tier clears the cheap coder's bar",
			fixTier:       "simple",
			wantRequested: registry.TierSimple,
			wantMet:       registry.TierSimple,
			wantAtBar:     true,
		},
		{
			name:          "complex fix_tier clamps to the rung the coder clears",
			fixTier:       "complex",
			wantRequested: registry.TierComplex,
			wantMet:       registry.TierSimple,
			wantAtBar:     false,
		},
		{
			name:          "authoritative overrides fix_tier and requests complex",
			fixTier:       "simple",
			authoritative: true,
			wantRequested: registry.TierComplex,
			wantMet:       registry.TierSimple,
			wantAtBar:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := fixTierRun(t)

			got, err := o.resolveFixModel(context.Background(), fixRequest{FixTier: tt.fixTier, Authoritative: tt.authoritative})
			require.NoError(t, err)

			assert.Equal(t, fixTierCoder, got.Model,
				"the measured coder is the pick at every rung it clears; an unmeasured model never outranks it on quality")
			assert.Equal(t, registry.SourceAuto, got.Source,
				"a clamped pick is a real selection at its rung, not the capable default")
			assert.Equal(t, tt.wantRequested, got.RequestedTier,
				"fix_tier and the authoritative flag decide what is asked for")
			assert.Equal(t, tt.wantMet, got.MetTier,
				"the met tier is measured from the prior, never copied from the request")
			assert.Equal(t, tt.wantAtBar, got.AtBar())

			again, err := o.resolveFixModel(context.Background(), fixRequest{FixTier: tt.fixTier, Authoritative: tt.authoritative})
			require.NoError(t, err)
			assert.Equal(t, got, again, "resolving the same round twice is stable")
		})
	}
}

// TestAuthoritativeFixGateLivesInAtBarNotInTheModel pins WHERE the
// authoritative pass's complex floor is enforced. It is not enforced by which
// model comes back: nothing in this catalog clears 0.82, so the authoritative
// pass gets the same coder the plain pass got. The two picks differ in that
// the authoritative one reports it did not reach the tier it asked for, and
// that flag is what the escalated fix round and the shortfall advisory branch
// on.
func TestAuthoritativeFixGateLivesInAtBarNotInTheModel(t *testing.T) {
	o := fixTierRun(t)

	nonAuth, err := o.resolveFixModel(context.Background(), fixRequest{FixTier: "simple"})
	require.NoError(t, err)

	auth, err := o.resolveFixModel(context.Background(), fixRequest{FixTier: "simple", Authoritative: true})
	require.NoError(t, err)

	assert.Equal(t, nonAuth.Model, auth.Model, "the catalog offers the authoritative pass nothing stronger")
	assert.True(t, nonAuth.AtBar(), "a simple request served at simple got what it asked for")
	assert.False(t, auth.AtBar(), "an authoritative pass that could not reach complex must say so")
	assert.Equal(t, registry.TierComplex, auth.RequestedTier)
}

// TestFormatFixesFixFilesRoundTrip pins the line-shape contract between
// formatFixes (writer) and fixFiles (parser): every fix file path must survive
// the format -> parse round trip, deduplicated, in order.
func TestFormatFixesFixFilesRoundTrip(t *testing.T) {
	raw := `{"approved":false,"summary":"needs work","fixes":[` +
		`{"file":"internal/api/health.go","issue":"missing error wrap","suggestion":"wrap with fmt.Errorf","severity":"important"},` +
		`{"file":"web/src/App.tsx","issue":"stale prop"},` +
		`{"file":"internal/api/health.go","issue":"second issue, same file","suggestion":"dedupe me"}]}`

	v, err := parseVerdict(raw)
	require.NoError(t, err)

	rendered := formatFixes(v)
	assert.Contains(t, rendered, "- internal/api/health.go: [important] missing error wrap - wrap with fmt.Errorf",
		"severity parses through parseVerdict and renders in brackets after the colon")

	got := fixFiles(rendered)
	assert.Equal(t, []string{"internal/api/health.go", "web/src/App.tsx"}, got,
		"file paths must survive the formatFixes -> fixFiles round trip, severity included")
}

// TestFormatFixesEmptySeverityByteIdentical pins that an empty Severity emits
// no bracket, so checkpoint.go's unmodified use of formatFixes (a verdict
// whose fixes never carry a severity) keeps rendering exactly as before.
func TestFormatFixesEmptySeverityByteIdentical(t *testing.T) {
	v := verdict{
		Summary: "needs work",
		Fixes: []fix{
			{File: "a.go", Issue: "bug", Suggestion: "patch"},
		},
	}

	assert.Equal(t, "needs work\n- a.go: bug - patch\n", formatFixes(v),
		"an empty severity must render byte-identically to the pre-severity line shape")
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

	// No surviving findings -> no cleanup fix pass, no fixup.
	assert.Equal(t, -1, indexOfPrefix(git.recorded(), "CommitFixup:"),
		"an approval with an empty fixes array must not run a cleanup pass; git=%v", git.recorded())
}

// TestReviewApprovedWithFixesRunsOneFixPass proves an approved verdict that
// still carries surviving findings runs exactly one non-escalating cleanup
// fix pass instead of discarding v.Fixes: the fixup lands, and the pass never
// touches the review-attempts counter (it is not a review round).
func TestReviewApprovedWithFixesRunsOneFixPass(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}
	// Three specialist fan-out responses, one synthesis verdict (approved but
	// carrying a surviving finding), then exactly ONE fix-coder response. If
	// the run erroneously looped back into another review round, the queue
	// would starve and the malformed fallback response would fail synthesis
	// parsing, failing this test via a non-nil runReview error.
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: minor nit", 0.01),
		stopResp("Design: looks fine", 0.01),
		stopResp("Security: looks fine", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[{"file":"a.go","issue":"nit","suggestion":"tidy","severity":"minor"}]}`, 0.02),
		stopResp("coder: tidied", 0.05),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	assert.GreaterOrEqual(t, indexOfPrefix(git.recorded(), "CommitFixup:"), 0,
		"the surviving finding must land as a fixup; git=%v", git.recorded())
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"),
		"a non-escalating cleanup pass on an approved round must not increment attempts; calls=%v", ops.recorded())
	assert.Contains(t, o.reviewSummary, "were applied in a follow-up cleanup pass",
		"the PR body must say the surviving findings were fixed, not leave them reading as open")
	assert.Contains(t, o.reviewSummary, "a.go", "the finding rides the summary")
}

// TestReviewApprovedFixCleanupNoOpKeepsTheCounters proves that when the
// cleanup pass lands no commit, the approved run still finishes cleanly: the
// fix-coder call actually ran (client.tasks), it was built from the surviving
// finding, the no-op was logged, and no attempts increment happened.
//
// The counters are NOT cleared on the way out. Clearing them was the defect:
// runFix is shared with pr_gates, which runs AFTER approval, so an approving
// verdict handed the first gate round the model that had already failed, at the
// un-escalated bar. The cleanup pass declines to escalate per-call instead.
func TestReviewApprovedFixCleanupNoOpKeepsTheCounters(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: false}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: minor nit", 0.01),
		stopResp("Design: looks fine", 0.01),
		stopResp("Security: looks fine", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[{"file":"a.go","issue":"nit","suggestion":"tidy","severity":"minor"}]}`, 0.02),
		stopResp("coder: could not locate the issue", 0.05),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	// Enter already escalated, so the assertion below reads a value an approval
	// could clear rather than the zero value it would report anyway.
	o.fixBarSteps = 1

	require.NoError(t, runReview(context.Background(), o))

	require.Len(t, client.tasks, 5,
		"three specialists, one synthesis, and the cleanup pass's fix-coder call; tasks=%v", client.tasks)
	assert.Contains(t, client.tasks[4], "a.go",
		"the cleanup pass's fix prompt must be built from the surviving finding")
	assert.Contains(t, o.reviewSummary, "were not fixed",
		"a cleanup pass that landed nothing must not leave the PR model free to narrate the findings as fixed")
	assert.True(t, ops.loggedContains("produced no change"),
		"the no-op cleanup pass must be logged; logs=%v", ops.logs)

	assert.Equal(t, -1, indexOfCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"),
		"a no-op cleanup pass must not increment attempts; calls=%v", ops.recorded())
	assert.Equal(t, 1, o.fixBarSteps,
		"an approving verdict must not clear the escalation the gate rounds after it read")
}

// TestReviewApprovedFixCleanupErrorHandling covers both arms of the cleanup
// pass's ferr != nil branch (review.go): a budget error from the fix run must
// propagate out of runReview so the worker can park, while every other error
// class (here, transport) is logged and swallowed, leaving the approved
// verdict standing.
func TestReviewApprovedFixCleanupErrorHandling(t *testing.T) {
	verdictResponses := func() []llm.Response {
		return []llm.Response{
			stopResp("Correctness: minor nit", 0.01),
			stopResp("Design: looks fine", 0.01),
			stopResp("Security: looks fine", 0.01),
			stopResp(`{"approved":true,"summary":"clean","fixes":[{"file":"a.go","issue":"nit","suggestion":"tidy","severity":"minor"}]}`, 0.02),
		}
	}

	tests := []struct {
		name       string
		maxCost    float64
		wantBudget bool
	}{
		{
			name: "budget error propagates",
			// Strictly between the ledger spend after the three specialists
			// (0.03) and the spend after synthesis (0.05): reviewRound's own two
			// ledger.Check() calls pass, and runFix's check trips.
			maxCost:    0.04,
			wantBudget: true,
		},
		{
			name: "transport error is logged and swallowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{}
			git := &fakeGit{committed: true, lastCommitTarget: "abc123"}

			errAfter := 0
			if !tt.wantBudget {
				errAfter = 5 // the 5th call is the cleanup pass's fix-coder run
			}

			client := &planLLM{responses: verdictResponses(), errAfter: errAfter}
			d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

			tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
			o := newReviewRun(d, tc, tt.maxCost)

			err := runReview(context.Background(), o)

			if tt.wantBudget {
				var be *BudgetExceededError
				require.ErrorAs(t, err, &be, "a budget error from the cleanup pass must propagate out of runReview")
				require.Len(t, client.tasks, 4, "the fix coder must never run once the ledger trips; tasks=%v", client.tasks)
			} else {
				require.NoError(t, err, "a non-budget cleanup-pass error must be swallowed, not returned")
				assert.True(t, ops.loggedContains("cleanup fix pass failed"),
					"the swallowed failure must be logged on the card; logs=%v", ops.logs)
				assert.Contains(t, git.hardResetRefs, "HEAD",
					"a swallowed cleanup-pass error must reset the worktree, or the failed coder's "+
						"uncommitted edits carry a dirty tree into integrate's autosquash rebase")
			}

			assert.Equal(t, -1, indexOfCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"),
				"the cleanup pass must never increment review attempts; calls=%v", ops.recorded())
		})
	}
}

// cleanupVerifyResponses is the LLM script every cleanup-verify test below
// shares: three specialists, an approving verdict that still carries one
// actionable finding, and the cleanup pass's fix-coder run.
func cleanupVerifyResponses() []llm.Response {
	return []llm.Response{
		stopResp("Correctness: minor issue", 0.01),
		stopResp("Design: looks fine", 0.01),
		stopResp("Security: looks fine", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[{"file":"a.go","issue":"leak","suggestion":"close it","severity":"minor"}]}`, 0.02),
		stopResp("coder: tidied", 0.05),
	}
}

// TestReviewCleanupFixupVerified proves the post-approval cleanup fixup is
// verified in its own right: the resolved plan runs again after the fixup
// lands, and the run-level result - the one the PR body and the completion note
// render - is the CLEANUP run's, not the approving round's. Without the re-run
// the PASSED trailer would describe the pre-cleanup tree.
func TestReviewCleanupFixupVerified(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{
		committed:        true,
		lastCommitTarget: "abc123",
		headSHAs:         []string{"snapshot-sha", "pre-cleanup-sha"},
	}
	client := &planLLM{responses: cleanupVerifyResponses()}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.verify = &verifyPlan{Argv: []string{"verify"}, Display: "verify", Source: verifySourceDetected, Timeout: time.Minute}

	runs := 0
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		runs++
		if runs == 1 {
			return verifyexec.Outcome{ExitCode: 0, Output: "round gate"}
		}

		return verifyexec.Outcome{ExitCode: 0, Output: "cleanup gate"}
	}

	require.NoError(t, runReview(context.Background(), o))

	assert.Equal(t, 2, runs, "the committed cleanup fixup must be verified, not shipped on the round's earlier gate")
	assert.Equal(t, verifyPassed, o.lastVerify.Status)
	assert.Equal(t, "cleanup gate", o.lastVerify.Output,
		"the run-level result must come from the run that saw the fixup")
	assert.Contains(t, verifyStatusLine(o.lastVerify, o.resolvedVerifyPlan()), "PASSED",
		"a cleanup the gate proved keeps the PASSED trailer")
	assert.Empty(t, git.hardResetRefs, "a proven cleanup fixup must not be reset away; git=%v", git.recorded())
	assert.Contains(t, o.reviewSummary, "were applied in a follow-up cleanup pass")
}

// TestReviewCleanupFixupDiscardedOnRedVerify proves a cleanup fixup that breaks
// the verify is dropped rather than shipped: the branch returns to the commit
// the pass started from, the discard is on the card, the approved run still
// finishes, the run-level result is the approving round's (which describes the
// tree that survived), and the summary stops claiming the findings were fixed.
func TestReviewCleanupFixupDiscardedOnRedVerify(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{
		committed:        true,
		lastCommitTarget: "abc123",
		// The snapshot head, the head the cleanup pass commits onto, and a head
		// only a capture taken AFTER the fix run could read.
		headSHAs: []string{"snapshot-sha", "pre-cleanup-sha", "post-cleanup-sha"},
	}
	client := &planLLM{responses: cleanupVerifyResponses()}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.verify = &verifyPlan{Argv: []string{"verify"}, Display: "verify", Source: verifySourceDetected, Timeout: time.Minute}

	runs := 0
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		runs++
		if runs == 1 {
			return verifyexec.Outcome{ExitCode: 0, Output: "round gate"}
		}

		return verifyexec.Outcome{ExitCode: 1, Output: "FAIL: the cleanup broke the build"}
	}

	require.NoError(t, runReview(context.Background(), o),
		"discarding the cleanup is not a review failure - the approved change still ships")

	assert.Equal(t, []string{"pre-cleanup-sha"}, git.hardResetRefs,
		"the branch must return to the commit the cleanup pass started from; git=%v", git.recorded())
	assert.True(t, ops.loggedContains(cleanupDiscardPrefix),
		"the discard must be recorded on the card; logs=%v", ops.logs)
	assert.Equal(t, "round gate", o.lastVerify.Output,
		"the tree that ships is the one the approving round verified, so its result stands")
	assert.Contains(t, o.reviewSummary, "were not fixed",
		"findings whose fixup was discarded must not read as applied")
}

// TestReviewCleanupFixupRedAndUndiscardable proves the one path that keeps a
// red fixup: the discard could not be performed. Both ways that happens - the
// pre-cleanup commit was never recorded, and the reset itself failed - must
// leave the run finishing on the RED result, so the PR body and the completion
// note stop claiming the approving round's PASSED for a tree that no longer
// matches it.
func TestReviewCleanupFixupRedAndUndiscardable(t *testing.T) {
	tests := []struct {
		name         string
		git          *fakeGit
		wantResetRef []string
	}{
		{
			// An empty Head read leaves the pass no commit to fall back to.
			name: "the pre-cleanup commit was never recorded",
			git: &fakeGit{
				committed:        true,
				lastCommitTarget: "abc123",
				headSHAs:         []string{"snapshot-sha", ""},
			},
		},
		{
			name: "the reset failed",
			git: &fakeGit{
				committed:        true,
				lastCommitTarget: "abc123",
				headSHAs:         []string{"snapshot-sha", "pre-cleanup-sha"},
				hardResetErr:     assertErr("detached worktree"),
			},
			wantResetRef: []string{"pre-cleanup-sha"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{}
			client := &planLLM{responses: cleanupVerifyResponses()}
			d := reviewTestDeps(t, ops, tt.git, client, reviewerRegistry())

			tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
			o := newReviewRun(d, tc, 0)
			o.verify = &verifyPlan{Argv: []string{"verify"}, Display: "verify", Source: verifySourceDetected, Timeout: time.Minute}

			runs := 0
			o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
				runs++
				if runs == 1 {
					return verifyexec.Outcome{ExitCode: 0, Output: "round gate"}
				}

				return verifyexec.Outcome{ExitCode: 1, Output: "FAIL: the cleanup broke the build"}
			}

			require.NoError(t, runReview(context.Background(), o))

			assert.Equal(t, tt.wantResetRef, tt.git.hardResetRefs)
			assert.Equal(t, verifyFailed, o.lastVerify.Status,
				"a fixup that stayed on the branch must be reported on its own gate result")
			assert.Contains(t, verifyStatusLine(o.lastVerify, o.resolvedVerifyPlan()), "FAILED",
				"the trailer must not carry the approving round's PASSED over a tree it never measured")

			// The round section was recorded before the cleanup pass ran. The
			// branch it described is gone, so the card body must not keep
			// claiming a gate that passed on it.
			body := ops.lastBody()
			assert.NotContains(t, body, "**Verify:** PASSED",
				"the recorded round must not claim PASSED for a tree already known red; body=%q", body)
			assert.Contains(t, body, "**Verify:** FAILED")
			assert.Contains(t, body, cleanupVerifyCorrection, "the correction must say why the round's gate no longer holds")
			assert.Contains(t, body, "### Recommendation\n\napprove",
				"correcting the verify line must not rewrite the verdict the reviewers reached")
		})
	}
}

// TestReviewCleanupFixupInconclusiveIsKeptNotDiscarded proves an inconclusive
// cleanup gate (here a timeout) is not a red one: the fixup stays, mirroring
// the pre-commit gate's skip arm. Both surfaces stop claiming the round's
// PASSED, because the tree that ships now carries a fixup nothing measured.
func TestReviewCleanupFixupInconclusiveIsKeptNotDiscarded(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{
		committed:        true,
		lastCommitTarget: "abc123",
		headSHAs:         []string{"snapshot-sha", "pre-cleanup-sha"},
	}
	client := &planLLM{responses: cleanupVerifyResponses()}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.verify = &verifyPlan{Argv: []string{"verify"}, Display: "verify", Source: verifySourceDetected, Timeout: time.Minute}

	runs := 0
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		runs++
		if runs == 1 {
			return verifyexec.Outcome{ExitCode: 0, Output: "round gate"}
		}

		return verifyexec.Outcome{TimedOut: true, ExitCode: -1}
	}

	require.NoError(t, runReview(context.Background(), o))

	assert.Empty(t, git.hardResetRefs, "an inconclusive gate is not proof of a defect, so the fixup stays; git=%v", git.recorded())
	assert.Equal(t, verifySkipped, o.lastVerify.Status)
	assert.Contains(t, verifyStatusLine(o.lastVerify, o.resolvedVerifyPlan()), "NOT VERIFIED",
		"a kept fixup nothing could measure must not ship under the round's PASSED")

	body := ops.lastBody()
	assert.NotContains(t, body, "**Verify:** PASSED", "body=%q", body)
	assert.Contains(t, body, cleanupVerifyCorrection)
}

// TestReviewCleanupNoCommitSkipsTheVerify proves the re-run is keyed on a
// cleanup pass that actually COMMITTED: a pass that landed nothing leaves HEAD
// where the round's own gate already measured it, and re-running the suite
// there buys nothing.
func TestReviewCleanupNoCommitSkipsTheVerify(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: false, headSHAs: []string{"snapshot-sha", "pre-cleanup-sha"}}
	client := &planLLM{responses: cleanupVerifyResponses()}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.verify = &verifyPlan{Argv: []string{"verify"}, Display: "verify", Source: verifySourceDetected, Timeout: time.Minute}

	runs := 0
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		runs++

		return verifyexec.Outcome{ExitCode: 0, Output: "round gate"}
	}

	require.NoError(t, runReview(context.Background(), o))

	assert.Equal(t, 1, runs, "a cleanup pass that committed nothing must not re-run the verify")
	assert.Empty(t, git.hardResetRefs, "nothing landed, so there is nothing to discard; git=%v", git.recorded())
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

// TestReviewFixMaxTurnsKeepsItsPartialWork pins that a fix run truncated at the
// turn cap still lands what it wrote: with no wider round left to buy the cap
// propagates - so nothing reads the findings as addressed - but the edits are
// committed and pushed rather than left in the tree for the next round to start
// from a workspace it did not write.
//
// "No wider round left" has two shapes, and the retry must recognise both: the
// step counter spent, and a card whose bar already opens the budget at the top
// rung so the width cannot move however many steps remain. Both rows land on
// the same 3-turn window, which is why one burn script covers them.
func TestReviewFixMaxTurnsKeepsItsPartialWork(t *testing.T) {
	tests := []struct {
		name       string
		seed       func(o *run)
		wantBudget int
	}{
		{
			name:       "the step counter is spent",
			seed:       func(o *run) { o.fixBudgetSteps = maxBudgetStep },
			wantBudget: maxBudgetStep,
		},
		{
			name: "the width is already at the top rung",
			seed: func(o *run) {
				o.cardSizing = sizing{Bar: registry.TierCritical, Budget: seedBudgetStep(registry.TierCritical)}
				o.fixBudgetSteps = 1
			},
			wantBudget: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{}
			git := &fakeGit{committed: true, lastCommitTarget: "abc123"}

			responses := []llm.Response{
				stopResp("Correctness: bug found", 0.01),
				stopResp("Design: ok", 0.01),
				stopResp("Security: ok", 0.01),
				stopResp(`{"approved":false,"summary":"needs fix","fixes":[{"file":"a.go","issue":"bug"}]}`, 0.02),
			}
			// The top rung of a 1-turn base is a 3-turn window; the coder burns
			// all of it -> max_turns.
			responses = append(responses, burnResps(3)...)

			d := reviewTestDeps(t, ops, git, &planLLM{responses: responses}, reviewerRegistry())
			d.Cfg.MaxTurns = 1

			tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "review"}
			o := newReviewRun(d, tc, 0)
			tt.seed(o)

			err := runReview(context.Background(), o)

			// Checked before the error, because a retry the width cannot serve
			// derails the run in ways that would otherwise mask this claim.
			assert.Equal(t, tt.wantBudget, o.fixBudgetSteps,
				"a round that could not run wider is not recorded as a widening")

			require.Error(t, err)

			var mte *MaxTurnsError
			require.ErrorAs(t, err, &mte)

			assert.Equal(t, []string{"cm/card-1"}, git.pushBranches,
				"a truncated fix round's edits are pushed, not abandoned; git=%v", git.recorded())
		})
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

	_, err := o.runFixModel(context.Background(), "fix prompt", fixRequest{Round: 1, FixTier: "complex"})
	require.NoError(t, err, "a complex fix tier scales the budget above the base, so 25 turns do not cap")
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

	_, err := o.runFixModel(context.Background(), "fix prompt", fixRequest{Round: 1, FixTier: "simple"})
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
	// the panel-models log style, and it must name BOTH sizing axes without
	// hinging on a specific value for either.
	var selection string

	for _, m := range ops.logs {
		if strings.Contains(m, "fix coder ") &&
			strings.Contains(m, "selected for round 1 fixes") &&
			strings.Contains(m, "bar=") && strings.Contains(m, "turns=") {
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

// TestReviewLoopParksOnLowDeadline proves reviewLoop checks the container
// deadline before spending any review round: a deadline inside the reserve
// parks before any model call and names the reserve and the resume round in
// the card log, while a deadline outside the reserve - or unset - lets the
// round run to completion.
func TestReviewLoopParksOnLowDeadline(t *testing.T) {
	tests := []struct {
		name     string
		deadline time.Time
		wantPark bool
	}{
		{
			name:     "deadline inside the reserve parks before any model call",
			deadline: time.Now().Add(5 * time.Minute),
			wantPark: true,
		},
		{
			name:     "deadline outside the reserve runs the round",
			deadline: time.Now().Add(time.Hour),
			wantPark: false,
		},
		{
			name:     "zero deadline runs the round",
			deadline: time.Time{},
			wantPark: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{}
			git := &fakeGit{committed: true}
			client := &planLLM{responses: []llm.Response{
				stopResp("Correctness: fine", 0.01),
				stopResp("Design: fine", 0.01),
				stopResp("Security: fine", 0.01),
				stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
			}}
			d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
			d.Cfg.Deadline = tt.deadline

			tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
			o := newReviewRun(d, tc, 0)

			err := runReview(context.Background(), o)

			if tt.wantPark {
				var parked *ReviewParkedError

				require.ErrorAs(t, err, &parked, "a short deadline must park before any model call")
				assert.Zero(t, modelCallCount(client), "no model call before the park")
				assert.True(t, ops.loggedContains("20m0s"), "log names the reserve; logs=%v", ops.logs)
				assert.True(t, ops.loggedContains("resumes at round 1"), "log names the resume round; logs=%v", ops.logs)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, 4, modelCallCount(client), "3 specialists + 1 synthesis")
		})
	}
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

	specs := o.reviewPanel(context.Background(), estimateTokens("diff"), false)
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

	specs := o.reviewPanel(context.Background(), estimateTokens("diff"), false)
	require.Len(t, specs, 3)

	for _, s := range specs {
		assert.Equal(t, "pinned/model", s.Model, "reviewer pin must override the whole panel")
	}
}

func TestReviewUnresolvableReviewerPinEmitsAdvisory(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	client := &planLLM{}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{
		Title: "Parent", Description: "body", State: "in_progress",
		ModelReviewer: "pinned/missing",
	}
	o := newReviewRun(d, tc, 0)

	_ = o.reviewPanel(context.Background(), estimateTokens("diff"), false)

	// Exactly one log entry for the unresolvable pin.
	require.Len(t, ops.logs, 1, "unresolvable reviewer pin must produce exactly one advisory")
	assert.Contains(t, ops.logs[0], "pinned/missing")
}

func TestReviewResolvableReviewerPinNoAdvisory(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	client := &planLLM{}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{
		Title: "Parent", Description: "body", State: "in_progress",
		ModelReviewer: "pinned/model",
	}
	o := newReviewRun(d, tc, 0)

	_ = o.reviewPanel(context.Background(), estimateTokens("diff"), false)

	assert.Empty(t, ops.logs, "resolvable reviewer pin must produce no advisory")
}

func TestResolveFixModelUnresolvablePinEmitsAdvisory(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	d := reviewTestDeps(t, ops, git, &planLLM{}, reviewerRegistry())

	tc := cmclient.TaskContext{
		Title: "Parent", Description: "body", State: "in_progress",
		ModelCoder: "pinned/missing",
	}
	o := newReviewRun(d, tc, 0)

	_, _ = o.resolveFixModel(context.Background(), fixRequest{FixTier: "simple"})

	require.Len(t, ops.logs, 1, "unresolvable coder pin must produce exactly one advisory")
	assert.Contains(t, ops.logs[0], "pinned/missing")
}

func TestResolveFixModelResolvablePinNoAdvisory(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	d := reviewTestDeps(t, ops, git, &planLLM{}, reviewerRegistry())

	tc := cmclient.TaskContext{
		Title: "Parent", Description: "body", State: "in_progress",
		ModelCoder: "pinned/model",
	}
	o := newReviewRun(d, tc, 0)

	_, _ = o.resolveFixModel(context.Background(), fixRequest{FixTier: "simple"})

	assert.Empty(t, ops.logs, "resolvable coder pin must produce no advisory")
}

func TestResolveFixModelUnresolvablePinDeduplicates(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	d := reviewTestDeps(t, ops, git, &planLLM{}, reviewerRegistry())

	tc := cmclient.TaskContext{
		Title: "Parent", Description: "body", State: "in_progress",
		ModelCoder: "pinned/missing",
	}
	o := newReviewRun(d, tc, 0)

	// First call warns.
	_, _ = o.resolveFixModel(context.Background(), fixRequest{FixTier: "simple"})

	require.Len(t, ops.logs, 1)

	// Second call with the same pin must NOT produce another entry (once-per-run).
	_, _ = o.resolveFixModel(context.Background(), fixRequest{FixTier: "simple"})

	require.Len(t, ops.logs, 1, "resolvedFixModel must deduplicate with the coderPinWarned guard")
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
	o.cardSizing = seedSizing("moderate") // no reviewer pin -> selection path

	est := estimateTokens("diff")

	moderatePanel := o.reviewPanel(context.Background(), est, false)
	require.Len(t, moderatePanel, 3)

	complexPanel := o.reviewPanel(context.Background(), est, true)
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

// TestRunSpecialistsNoReviewerParksInstead pins that an empty review panel
// parks the card for a human rather than indexing into it. No prior clears
// any bar and there is no capable default, so the panel is empty on the very
// first seat.
func TestRunSpecialistsNoReviewerParksInstead(t *testing.T) {
	reg := registry.NewRegistryFromParts(reviewerCatalog(), registry.Priors{}, nil, nil, "")
	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reg)

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	_, err := o.runSpecialists(context.Background(), false)

	var parked *ReviewParkedError

	require.ErrorAs(t, err, &parked, "an empty panel must park rather than panic")
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

// TestRunFixRoutesByFindingsOrigin proves runFix picks verifyFixPrompt (title-only
// parent, explicit SCOPE) when the findings came from a failed verify gate, and
// keeps using the full fixPrompt (with the parent description) for panel-round
// findings.
func TestRunFixRoutesByFindingsOrigin(t *testing.T) {
	t.Run("verify-failed round", func(t *testing.T) {
		ops := &fakeOps{}
		git := &fakeGit{committed: true}
		client := &planLLM{responses: []llm.Response{
			stopResp("coder: fixed", 0.05),
		}}
		d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

		tc := cmclient.TaskContext{Title: "Parent", Description: "the distinctive parent description", State: "in_progress"}
		o := newReviewRun(d, tc, 0)
		o.verify = &verifyPlan{Argv: []string{"verify"}, Display: "verify", Source: verifySourceDetected, Timeout: time.Minute}
		o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
			return verifyexec.Outcome{ExitCode: 1, Output: "FAIL: tests broke"}
		}

		findings, fixTier, approved, _, _, err := o.reviewRound(context.Background(), *o.verify, 1, false)
		require.NoError(t, err)
		assert.False(t, approved)

		committed, err := o.runFix(context.Background(), fixRequest{Findings: findings, Round: 1, FixTier: fixTier})
		require.NoError(t, err)
		assert.True(t, committed)

		prompt := promptOfCall(client, 0)
		assert.Contains(t, prompt, "The ONLY item in scope is the failure below")
		assert.NotContains(t, prompt, "the distinctive parent description")
	})

	t.Run("panel round", func(t *testing.T) {
		ops := &fakeOps{}
		git := &fakeGit{committed: true}
		client := &planLLM{responses: []llm.Response{
			stopResp("Correctness: bug", 0.01),
			stopResp("Design: ok", 0.01),
			stopResp("Security: ok", 0.01),
			stopResp(`{"approved":false,"summary":"fix it","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`, 0.02),
			stopResp("coder: fixed", 0.05),
		}}
		d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

		tc := cmclient.TaskContext{Title: "Parent", Description: "the distinctive parent description", State: "in_progress"}
		o := newReviewRun(d, tc, 0)

		findings, fixTier, approved, _, _, err := o.reviewRound(context.Background(), verifyPlan{}, 1, false)
		require.NoError(t, err)
		assert.False(t, approved)

		committed, err := o.runFix(context.Background(), fixRequest{Findings: findings, Round: 1, FixTier: fixTier})
		require.NoError(t, err)
		assert.True(t, committed)

		prompt := promptOfCall(client, 4)
		assert.Contains(t, prompt, "the distinctive parent description")
	})
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

	findings, _, approved, vres, _, err := o.reviewRound(context.Background(), *o.verify, 1, false)
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

	findings, _, approved, vres, _, err := o.reviewRound(context.Background(), *o.verify, 1, false)
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

// TestRunReviewHITLPromotedRejectFixesThenApproves pins the incident
// regression: a card promoted to autonomous mid-run must not have a rejecting
// verdict silently approved at the review-decision gate. The promoted round's
// findings drive a fix, and the remaining rounds run with autonomous semantics.
func TestRunReviewHITLPromotedRejectFixesThenApproves(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}
	// Empty, non-blocking inbox: the first gate Wait reports ErrInboxClosed,
	// exactly what a promote frame produces.
	inbox := &fakeInbox{}
	client := &planLLM{responses: []llm.Response{
		// Round 1: specialists + synthesis (rejected). No gate classification -
		// promotion consumes no human turn and no model call.
		stopResp("Correctness: bug", 0.001), stopResp("Design: ok", 0.001), stopResp("Security: ok", 0.001),
		stopResp(`{"approved":false,"summary":"fix it","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`, 0.001),
		stopResp("Fixed.", 0.001), // fix coder
		// Round 2 (delegated autonomous loop): specialists + synthesis approve.
		stopResp("Correctness: ok now", 0.001), stopResp("Design: ok", 0.001), stopResp("Security: ok", 0.001),
		stopResp(`{"approved":true,"summary":"clean now","fixes":[]}`, 0.001),
	}}
	o := newRun(hitlReviewDeps(ops, git, inbox, client), cmclient.TaskContext{Title: "T", Description: "b", State: "review"})
	isolateVerify(o)

	require.NoError(t, runReview(context.Background(), o))

	assert.Equal(t, 1, countCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"),
		"the promoted rejecting round increments attempts and runs a fix; calls=%v", ops.recorded())

	gitCalls := git.recorded()
	fixupIdx := indexOfPrefix(gitCalls, "CommitFixup:")
	pushIdx := indexOfCall(gitCalls, "Push:cm/card-1")
	require.GreaterOrEqual(t, fixupIdx, 0, "the promoted round's fix must be fixup-committed; git=%v", gitCalls)
	require.GreaterOrEqual(t, pushIdx, 0, "the fixup must be pushed; git=%v", gitCalls)
	assert.Less(t, fixupIdx, pushIdx, "fixup before push")

	assert.Equal(t, "clean now", o.reviewSummary, "an approved delegated round keeps the plain verdict summary")

	body := ops.lastBody()
	assert.Contains(t, body, "## Review Findings", "round 1 recorded")
	assert.Contains(t, body, "## Review Findings (Round 2)", "delegated round numbering continues from the HITL round")
}

// TestRunReviewHITLPromotedRejectPersistsParks pins the cliff via delegation: a
// promoted run whose findings persist must reach the authoritative pass and
// park - never approve. Seeding review_attempts=1 puts the HITL round at 2, so
// the delegated loop lands directly on the cliff (round 3 >= cap 3).
func TestRunReviewHITLPromotedRejectPersistsParks(t *testing.T) {
	ops := &fakeOps{reviewAttempts: 1}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}
	inbox := &fakeInbox{}
	reject := `{"approved":false,"summary":"still broken","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`
	client := &planLLM{responses: []llm.Response{
		// HITL round 2: specialists + synthesis reject, then the promoted fix.
		stopResp("Correctness: bug", 0.001), stopResp("Design: ok", 0.001), stopResp("Security: ok", 0.001),
		stopResp(reject, 0.001),
		stopResp("Fixed.", 0.001),
		// Authoritative round 3: specialists + synthesis reject, then the strong fix.
		stopResp("Correctness: still bug", 0.001), stopResp("Design: ok", 0.001), stopResp("Security: ok", 0.001),
		stopResp(reject, 0.001),
		stopResp("Fixed again.", 0.001),
		// Strong re-review round 4: specialists + synthesis reject -> park.
		stopResp("Correctness: STILL bug", 0.001), stopResp("Design: ok", 0.001), stopResp("Security: ok", 0.001),
		stopResp(reject, 0.001),
	}}
	o := newRun(hitlReviewDeps(ops, git, inbox, client),
		cmclient.TaskContext{Title: "T", Description: "b", State: "review", ReviewAttempts: 1})
	isolateVerify(o)

	err := runReview(context.Background(), o)

	var parked *ReviewParkedError

	require.ErrorAs(t, err, &parked, "persistent findings on a promoted run must park, never approve")
	assert.Equal(t, 3, countCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"),
		"promoted round + authoritative fix + park each increment; calls=%v", ops.recorded())
	assert.Equal(t, 2, countPrefix(git.recorded(), "CommitFixup:"),
		"the promoted fix and the strong fix each land a fixup; git=%v", git.recorded())
	assert.True(t, ops.loggedContains("review parked"), "park logged; logs=%v", ops.logs)

	body := ops.lastBody()
	assert.Contains(t, body, "## Review Findings (Round 3)", "authoritative round numbering continues")
	assert.Contains(t, body, "## Review Findings (Round 4)", "strong re-review recorded")
}

// TestRunReviewHITLPromotedAutoApproveNoFix pins that a promotion with an
// approving verdict integrates without a fix round - the legitimate passthrough.
func TestRunReviewHITLPromotedAutoApproveNoFix(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	inbox := &fakeInbox{}
	client := &planLLM{responses: []llm.Response{
		stopResp("No concerns.", 0.001), stopResp("No concerns.", 0.001), stopResp("No concerns.", 0.001),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.001),
	}}
	o := newRun(hitlReviewDeps(ops, git, inbox, client), cmclient.TaskContext{Title: "T", Description: "b", State: "review"})
	isolateVerify(o)

	require.NoError(t, runReview(context.Background(), o))
	assert.Equal(t, 0, countCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"), "no fix round on an approving verdict")
	assert.Equal(t, -1, indexOfPrefix(git.recorded(), "CommitFixup:"), "no fixup; git=%v", git.recorded())
	assert.Equal(t, "clean", o.reviewSummary, "a genuine auto-approval keeps the plain verdict summary")
}

// TestRunReviewHITLHumanApproveDespiteFindingsFramesSummary pins the PR-body
// honesty contract: when the human approves while the automated verdict still
// has open findings, the review summary must say so instead of posing as an
// approved summary the PR model could narrate as fixed.
func TestRunReviewHITLHumanApproveDespiteFindingsFramesSummary(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "approve anyway"}}}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: bug", 0.001), stopResp("Design: ok", 0.001), stopResp("Security: ok", 0.001),
		stopResp(`{"approved":false,"summary":"needs work","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`, 0.001),
		stopResp(`{"verdict":"approve","feedback":""}`, 0.001), // the human overrides the revise recommendation
	}}
	o := newRun(hitlReviewDeps(ops, git, inbox, client), cmclient.TaskContext{Title: "T", Description: "b", State: "review"})
	isolateVerify(o)

	require.NoError(t, runReview(context.Background(), o))
	assert.Equal(t, 0, countCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"), "a human approval does not increment attempts")
	assert.Equal(t, -1, indexOfPrefix(git.recorded(), "CommitFixup:"), "no fix ran; git=%v", git.recorded())
	assert.Contains(t, o.reviewSummary, "approved integration despite", "the summary is framed as an override")
	assert.Contains(t, o.reviewSummary, "a.go", "the outstanding finding rides the summary")
	assert.Contains(t, o.reviewSummary, "bug", "the finding text rides the summary")
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

// countPrefix counts how many entries in calls start with prefix.
func countPrefix(calls []string, prefix string) int {
	n := 0

	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
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

// TestReviewMobApprovedWithFixesRunsOneFixPass mirrors
// TestReviewMobApprovedFirstPass with a non-empty fixes array in the scripted
// approved outcome, proving the non-escalating cleanup pass covers the mob
// discussion synthesis path exactly like the solo path: one discussion round
// (no re-convene), one fix-coder call, one fixup, no attempts increment.
func TestReviewMobApprovedWithFixesRunsOneFixPass(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}
	llmFake := &planLLM{responses: []llm.Response{
		stopResp("coder: tidied", 0.05),
	}}
	verdictJSON := `{"approved":true,"summary":"clean","fix_tier":"",` +
		`"fixes":[{"file":"a.go","issue":"nit","suggestion":"tidy","severity":"minor"}]}`
	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: verdictJSON, Consensus: true}}}

	o := mobReviewRun(t, ops, git, llmFake, eng)
	require.NoError(t, runReview(context.Background(), o))

	require.Len(t, eng.topics, 1, "an approved round - even with surviving findings - must not re-convene the discussion")
	assert.Contains(t, o.reviewSummary, "clean", "the plain verdict summary survives")
	assert.Contains(t, o.reviewSummary, "a.go", "the surviving finding rides the summary too")
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"))
	assert.GreaterOrEqual(t, indexOfPrefix(git.recorded(), "CommitFixup:"), 0,
		"the surviving finding must land as a fixup on the mob path too; git=%v", git.recorded())
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

// TestReviewMobPassesExclusionsAndPriorFindings keeps the two properties the
// old delta-scope test really covered. Its snapshot assertion went with the
// scoping: it set lastReviewBase by hand, a state round 1 cannot reach, and
// mobReviewBriefing no longer reads the field at all.
func TestReviewMobPassesExclusionsAndPriorFindings(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	llmFake := &planLLM{}
	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: mobApprovedVerdict, Consensus: true}}}

	o := mobReviewRun(t, ops, git, llmFake, eng)
	o.coderModels = map[string]bool{"rev/alpha": true}
	o.lastFindings = "prior finding about a.go"

	require.NoError(t, runReview(context.Background(), o))

	// The coder's model never takes a seat.
	require.Len(t, eng.cfgs, 1)

	for _, s := range eng.cfgs[0].Seats {
		assert.NotEqual(t, "rev/alpha", s.Model, "review seats must exclude the coder's model")
	}

	assert.Equal(t, []string{"main"}, git.diffBases, "the briefing diffs the base branch")
	assert.Contains(t, eng.topics[0].Briefing, "prior finding about a.go")
}

// TestReviewMobPostFixRoundFullScope proves the mob path re-widens its
// briefing diff to the base branch on a round that follows a fix, even
// though a snapshot from round 1 would otherwise scope it down: a fix can
// land code outside the delta it targeted, and a delta-scoped round would
// never examine that code.
func TestReviewMobPostFixRoundFullScope(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123", headSHA: "snap1"}
	// Only LLM call: the fix coder run between the two discussion rounds.
	llmFake := &planLLM{responses: []llm.Response{
		stopResp("coder: fixed", 0.05),
	}}
	eng := &scriptedEngine{outcomes: []mob.Outcome{
		{Synthesis: `{"approved":false,"summary":"fix it","fix_tier":"simple",` +
			`"fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`},
		{Synthesis: mobApprovedVerdict, Consensus: true},
	}}

	o := mobReviewRun(t, ops, git, llmFake, eng)
	require.NoError(t, runReview(context.Background(), o))

	require.Len(t, git.diffBases, 2, "one briefing diff per round, no specialist fan-out")
	assert.Equal(t, "main", git.diffBases[0],
		"round 1 has no snapshot yet, so it already diffs the base branch")
	assert.Equal(t, "main", git.diffBases[1],
		"round 2 follows a fix, so it re-widens despite lastReviewBase==snap1")

	require.Len(t, eng.topics, 2, "the flag adds no rounds")
	assert.Equal(t, reviewLenses[:3], eng.topics[0].Lenses, "the flag adds no seats")
	assert.Equal(t, reviewLenses[:3], eng.topics[1].Lenses, "the flag adds no seats")
}

func TestReviewPromptsUseFilteredDescriptionAndSeededFindings(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: ok", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: grownDescription, State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	specialist := client.tasks[0]
	assert.Contains(t, specialist, "Add a config flag to toggle the feature.")
	assert.NotContains(t, specialist, "1. SUBTASK: Add the flag", "plan stripped from the description slot")
	assert.NotContains(t, specialist, "Use a palette config", "design stripped from the description slot")
	assert.Contains(t, specialist, "PRIOR FINDINGS", "resumed findings arrive through the prior-findings framing")
	assert.Equal(t, 1, strings.Count(specialist, "naming could improve"),
		"the finding text rides the prior block exactly once, not the description too")

	synthesis := client.tasks[3]
	assert.NotContains(t, synthesis, "1. SUBTASK: Add the flag", "plan not re-imported into synthesis")
}

// TestReviewGateFailureFindingsKeepTheTail: a failing build's diagnostics sit at
// the END of its output. The finding handed to the fix coder must be the tail,
// not a head-weighted slice whose 2666-byte head lands in the build tool's
// banner and whose 1334-byte tail clips the top of the error block.
func TestReviewGateFailureFindingsKeepTheTail(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	client := &planLLM{}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	// ~10 KB of banner, then a ~2.5 KB error block: bigger than HeadTail's
	// 1334-byte tail, so a head-weighted slice clips the block's first lines -
	// exactly the lines naming the failing test.
	banner := strings.Repeat("[INFO] downloading a dependency\n", 320)
	failures := "[ERROR] Failures:\n[ERROR]   WidgetIT.shouldPersist:42 expected true\n" +
		strings.Repeat("[ERROR]   at com.example.Widget.persist(Widget.java:88)\n", 45)
	out := banner + failures + "\nBUILD FAILURE\n"

	require.Greater(t, len(failures), verifyOutputTail/3,
		"the error block must exceed HeadTail's tail share or this test proves nothing")

	tc := cmclient.TaskContext{Title: "P", Description: "b", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.verify = &verifyPlan{Argv: []string{"verify"}, Display: "mvn -q verify", Source: verifySourceDetected, Timeout: time.Minute}
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		return verifyexec.Outcome{ExitCode: 1, Output: out}
	}

	findings, _, approved, vres, _, err := o.reviewRound(context.Background(), *o.verify, 1, false)
	require.NoError(t, err)
	require.False(t, approved)
	require.Equal(t, verifyFailed, vres.Status)

	assert.Contains(t, findings, "WidgetIT.shouldPersist",
		"the failing test name must reach the fix coder; it sits at the TOP of the error block")
	assert.Contains(t, findings, "BUILD FAILURE", "the final line of the build must survive")
	assert.Empty(t, client.tasks, "a gate failure short-circuits before any reviewer model call")
}

// TestRunReviewResetsExhaustedCounterApprove proves that a card whose persisted
// ReviewAttempts exceeds the cap (cap+1, from a prior parked run) gets the
// counter reset to zero at the start of runReview. The first review round uses
// the normal cheap path (round 1, not authoritative) and, on approval, does not
// call IncrementReviewAttempts.
func TestRunReviewResetsExhaustedCounterApprove(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: looks fine", 0.01),
		stopResp("Design: looks fine", 0.01),
		stopResp("Security: looks fine", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	// cap is 5; seed ReviewAttempts at cap+1 (6) to simulate a card that
	// actually parked (authoritative pass increments past the cap).
	tc := cmclient.TaskContext{
		Title: "Parent", Description: "body",
		State: "in_progress", ReviewAttempts: 6,
	}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	// The counter was reset, so the first round is a normal cheap round, not
	// authoritative. On approval, IncrementReviewAttempts is never called.
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"),
		"exhausted counter reset: approval must not increment; calls=%v", ops.recorded())
	// The round 1 specialist prompt should NOT carry the authoritative marker.
	require.NotEmpty(t, client.tasks)
	assert.NotContains(t, client.tasks[0], "full scope",
		"round 1 must be a normal cheap round, not authoritative; task=%q", client.tasks[0])
}

// TestRunReviewResetsExhaustedCounterFixThenApprove proves that an exhausted
// counter (cap+1, from a prior parked run) reset followed by a reject-and-fix
// cycle correctly increments attempts from 1 (not 7) and the second round uses
// normal numbering.
func TestRunReviewResetsExhaustedCounterFixThenApprove(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}
	client := &planLLM{responses: []llm.Response{
		// Round 1: specialists + synthesis (rejects).
		stopResp("Correctness: bug", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":false,"summary":"fix it","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`, 0.02),
		// Fix coder run.
		stopResp("coder: fixed the bug", 0.05),
		// Round 2: specialists + synthesis (approves).
		stopResp("Correctness: ok now", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":true,"summary":"clean now","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	tc := cmclient.TaskContext{
		Title: "Parent", Description: "body",
		State: "in_progress", ReviewAttempts: 6,
	}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	// Exactly one fix round: IncrementReviewAttempts called once.
	incCount := 0

	for _, c := range ops.recorded() {
		if c == "IncrementReviewAttempts:CARD-1" {
			incCount++
		}
	}

	assert.Equal(t, 1, incCount, "exhausted counter reset: exactly one fix round; calls=%v", ops.recorded())

	// The fix round committed and pushed.
	gitCalls := git.recorded()
	fixupIdx := indexOfPrefix(gitCalls, "CommitFixup:")
	pushIdx := indexOfCall(gitCalls, "Push:cm/card-1")
	require.GreaterOrEqual(t, fixupIdx, 0, "fixup committed; git=%v", gitCalls)
	require.GreaterOrEqual(t, pushIdx, 0, "fixup pushed; git=%v", gitCalls)
	assert.Less(t, fixupIdx, pushIdx, "fixup before push")
}

// TestRunReviewDoesNotResetOnInterruptedAuthoritative proves that a card
// whose ReviewAttempts is exactly at the cap (5) with State "review" (an
// interrupted authoritative pass) is NOT reset: the counter stays at the cap,
// round numbering continues from where it left off, and the authoritative pass
// runs (round 6). This distinguishes crash-resume from a fresh rerun from todo.
func TestRunReviewDoesNotResetOnInterruptedAuthoritative(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	client := &planLLM{responses: []llm.Response{
		// Authoritative review: 3 specialists + synthesis (approves).
		stopResp("Correctness: clean", 0.01),
		stopResp("Design: clean", 0.01),
		stopResp("Security: clean", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	// cap is 5; seed ReviewAttempts at the cap (5) with State "review" to
	// simulate an interrupted authoritative pass.
	tc := cmclient.TaskContext{
		Title: "Parent", Description: "body",
		State: "review", ReviewAttempts: 5,
	}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	// StartReview must NOT be called (already in review).
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "StartReview:CARD-1"),
		"interrupted authoritative: StartReview must be skipped; calls=%v", ops.recorded())

	// The counter was NOT reset, so the round goes to authoritative (round 6).
	// On approval, no IncrementReviewAttempts is called.
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"),
		"interrupted authoritative: approval must not increment; calls=%v", ops.recorded())

	// The round 6 heading must be recorded (not round 1).
	body := ops.lastBody()
	assert.Contains(t, body, "## Review Findings (Round 6)", "interrupted authoritative at counter 5 must record round 6; body=%q", body)
}

// TestRunReviewPreservesCrashResumeCounter proves that a card whose review
// counter is below the cap (e.g. 1 of 5) is NOT reset: the round numbering
// continues from where it left off, so a crash-resume at round 2 (counter 1)
// starts the review at round 2.
func TestRunReviewPreservesCrashResumeCounter(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: looks fine", 0.01),
		stopResp("Design: looks fine", 0.01),
		stopResp("Security: looks fine", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	// Counter at 1 means one prior round already ran; cap is 5, so no reset.
	// The first round should be numbered 2.
	tc := cmclient.TaskContext{
		Title: "Parent", Description: "body",
		State: "in_progress", ReviewAttempts: 1,
	}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	// No IncrementReviewAttempts calls on approval.
	assert.Equal(t, -1, indexOfCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"),
		"crash-resume: approval must not increment; calls=%v", ops.recorded())

	// The round 2 heading must be recorded (not round 1).
	body := ops.lastBody()
	assert.Contains(t, body, "## Review Findings (Round 2)", "crash-resume at counter 1 must record round 2; body=%q", body)
}

// TestRunReviewHITLResetsExhaustedCounter proves that a HITL card with an
// exhausted review counter gets the counter reset, so the HITL round starts at
// round 1. On approval, no IncrementReviewAttempts is called.
func TestRunReviewHITLResetsExhaustedCounter(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "approve"}}}
	client := &planLLM{responses: []llm.Response{
		stopResp("No concerns.", 0.001),
		stopResp("No concerns.", 0.001),
		stopResp("No concerns.", 0.001),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.001),
		stopResp(`{"verdict":"approve","feedback":""}`, 0.001),
	}}
	d := hitlReviewDeps(ops, git, inbox, client)
	// hitlReviewDeps sets ReviewAttemptsCap: 3 and Interactive: true.
	// Seed at cap+1 (4) with State "in_progress" to simulate a card that
	// actually parked on a prior run and was moved back to an active state.
	tc := cmclient.TaskContext{
		Title: "T", Description: "b",
		State: "in_progress", ReviewAttempts: 4,
	}
	o := newRun(d, tc)
	isolateVerify(o)

	require.NoError(t, runReview(context.Background(), o))

	// No fix round, so no IncrementReviewAttempts.
	assert.Equal(t, 0, countCall(ops.recorded(), "IncrementReviewAttempts:CARD-1"),
		"HITL exhausted counter reset: approval must not increment; calls=%v", ops.recorded())

	// Round 1 (not round 4) was recorded.
	body := ops.lastBody()
	assert.Contains(t, body, "## Review Findings", "HITL must record the findings")
	assert.NotContains(t, body, "(Round 4)", "reset counter starts at round 1, not 4; body=%q", body)
}

// TestIncrementReviewAttempt covers the park-vs-fail split on the shared
// increment helper. The ceiling rejection is the one error the review loop must
// absorb: a card already at ContextMatrix's review_attempts ceiling has to park
// cleanly instead of hard-failing mid-review.
func TestIncrementReviewAttempt(t *testing.T) {
	t.Parallel()

	newRun := func(incErr error) (*run, *fakeOps) {
		ops := &fakeOps{incrementErr: incErr}

		return &run{d: Deps{Ops: ops, Cfg: Config{CardID: "CARD-1"}}}, ops
	}

	t.Run("success returns the running total", func(t *testing.T) {
		t.Parallel()

		o, ops := newRun(nil)

		n, err := o.incrementReviewAttempt(t.Context(), "findings")
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		assert.Equal(t, 1, ops.reviewAttempts)
	})

	t.Run("ceiling rejection parks", func(t *testing.T) {
		t.Parallel()

		capped := fmt.Errorf("increment review attempts: review attempts capped at 7: %w",
			cmclient.ErrReviewAttemptsCapped)

		o, _ := newRun(capped)

		_, err := o.incrementReviewAttempt(t.Context(), "findings")

		var parked *ReviewParkedError

		require.ErrorAs(t, err, &parked)
	})

	t.Run("unrelated error fails the run", func(t *testing.T) {
		t.Parallel()

		o, _ := newRun(errors.New("connection refused"))

		_, err := o.incrementReviewAttempt(t.Context(), "findings")
		require.Error(t, err)

		var parked *ReviewParkedError

		require.NotErrorAs(t, err, &parked, "a transport failure must not be mistaken for a park")
		assert.Contains(t, err.Error(), "increment review attempts")
	})
}

// TestReviewCheapRoundParksAtServerCap pins that ContextMatrix's review_attempts
// ceiling rejection is absorbed as a park in the cheap review loop, not only on
// the authoritative pass. CM's counter is monotonic for the card's lifetime
// while runReview resets only its local snapshot for round numbering, so a card
// resumed at the ceiling re-enters at round 1 and the cheap increment is the
// first call that can be rejected.
func TestReviewCheapRoundParksAtServerCap(t *testing.T) {
	capped := fmt.Errorf("increment review attempts: review attempts capped at 7: %w",
		cmclient.ErrReviewAttemptsCapped)
	ops := &fakeOps{incrementErr: capped}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: bug", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":false,"summary":"needs fix","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	// Cap 5 with a zero starting counter puts round 1 well below the cliff, so
	// this is a cheap round, not the authoritative pass.
	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	err := runReview(context.Background(), o)
	require.Error(t, err)

	var parked *ReviewParkedError

	require.ErrorAs(t, err, &parked,
		"a cheap-round ceiling rejection must park, not hard-fail; got %v", err)
}

// TestReviewAuthoritativeFirstIncrementParksAtServerCap guards the wiring of the
// FIRST authoritative increment. Cap 1 sends round 1 straight to the
// authoritative pass, so the first increment call is the one inside
// authoritativeReview.
func TestReviewAuthoritativeFirstIncrementParksAtServerCap(t *testing.T) {
	capped := fmt.Errorf("increment review attempts: review attempts capped at 7: %w",
		cmclient.ErrReviewAttemptsCapped)
	ops := &fakeOps{incrementErr: capped}
	git := &fakeGit{committed: true}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: bug", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":false,"summary":"fix it","fixes":[{"file":"x.go","issue":"first","suggestion":"patch"}]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	d.Cfg.ReviewAttemptsCap = 1

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	err := runReview(context.Background(), o)
	require.Error(t, err)

	var parked *ReviewParkedError

	require.ErrorAs(t, err, &parked,
		"the authoritative pass's first increment must park on the ceiling; got %v", err)
}

// TestReviewAuthoritativeSecondIncrementParksAtServerCap guards the wiring of the
// SECOND authoritative increment, the one immediately before the park. The first
// increment succeeds and the strong fix plus re-review run, so only the final
// call meets the ceiling.
func TestReviewAuthoritativeSecondIncrementParksAtServerCap(t *testing.T) {
	capped := fmt.Errorf("increment review attempts: review attempts capped at 7: %w",
		cmclient.ErrReviewAttemptsCapped)
	ops := &fakeOps{reviewAttempts: 4, incrementErr: capped, incrementErrAfter: 1}
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

	// Seed the snapshot at cap-1 (the test helper's cap is 5) so iter 0 is the cliff.
	tc := cmclient.TaskContext{
		Title: "Parent", Description: "body",
		State: "in_progress", ReviewAttempts: 4,
	}
	o := newReviewRun(d, tc, 0)

	err := runReview(context.Background(), o)
	require.Error(t, err)

	var parked *ReviewParkedError

	require.ErrorAs(t, err, &parked,
		"the authoritative pass's second increment must park on the ceiling; got %v", err)
	assert.Equal(t, 2, ops.incrementCalls, "the first increment must have succeeded")
}

// escalationRegistry seeds three coder candidates across two vendors at equal
// price: every one clears the moderate bar (0.76) and alpha/coder wins on
// quality; only alpha/second and beta/coder clear the complex bar (0.82), so an
// escalated pick that avoids vendor alpha lands on beta/coder.
func escalationRegistry() *registry.Registry {
	catalog := llm.Catalog{
		{ID: "alpha/coder", ContextLength: 200000, SupportedParameters: []string{"tools"}, PromptPricePerTok: 1e-6, CompletionPricePerTok: 1e-6},
		{ID: "alpha/second", ContextLength: 200000, SupportedParameters: []string{"tools"}, PromptPricePerTok: 1e-6, CompletionPricePerTok: 1e-6},
		{ID: "beta/coder", ContextLength: 200000, SupportedParameters: []string{"tools"}, PromptPricePerTok: 1e-6, CompletionPricePerTok: 1e-6},
		{ID: "capable/default", ContextLength: 200000, SupportedParameters: []string{"tools"}},
	}

	alpha, second, beta := 0.90, 0.88, 0.85
	priors := registry.Priors{
		Models: map[string]registry.PriorEntry{
			"alpha/coder":  {Coder: &alpha},
			"alpha/second": {Coder: &second},
			"beta/coder":   {Coder: &beta},
		},
	}

	return registry.NewRegistryFromParts(catalog, priors, nil, nil, "capable/default")
}

// panelRejects scripts one specialist round (three specialists + synthesis)
// that returns a single fix; panelApproves one that approves.
func panelRejects(issue string) []llm.Response {
	return []llm.Response{
		stopResp("Correctness: "+issue, 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":false,"summary":"fix it","fixes":[{"file":"a.go","issue":"`+issue+`","suggestion":"patch"}]}`, 0.02),
	}
}

func panelApproves() []llm.Response {
	return []llm.Response{
		stopResp("Correctness: ok", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
	}
}

// TestReviewFixZeroEditRoundEscalatesModel: a fix round that lands no commit is
// a failed round - the next fix runs on a different model, one tier up,
// preferring another vendor, and the failed model is excluded.
func TestReviewFixZeroEditRoundEscalatesModel(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: false} // every fix round lands nothing

	var responses []llm.Response

	responses = append(responses, panelRejects("bug")...)
	responses = append(responses, stopResp("coder: tried", 0.05))
	responses = append(responses, panelRejects("still bug")...)
	responses = append(responses, stopResp("coder: tried again", 0.05))
	responses = append(responses, panelApproves()...)

	client := &planLLM{responses: responses}
	d := reviewTestDeps(t, ops, git, client, escalationRegistry())
	o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}, 0)

	require.NoError(t, runReview(context.Background(), o))

	// Call order per round: three specialists, synthesis, fix coder.
	require.GreaterOrEqual(t, len(client.models), 10, "models=%v", client.models)
	assert.Equal(t, "alpha/coder", client.models[4], "the first fix runs on the card-tier pick; models=%v", client.models)
	assert.Equal(t, "beta/coder", client.models[9],
		"a zero-edit round escalates: one tier up, another vendor, the failed model excluded; models=%v", client.models)
	assert.True(t, ops.loggedContains("escalated"), "the escalation is card-logged; logs=%v", ops.recorded())
}

// TestReviewFixRedVerifyAfterFixEscalatesModel: a fix round that committed but
// left the next round's verify red failed just as surely - the next fix
// escalates. Round 1's red verify, with no fix run yet, does not: the first fix
// runs on the card-tier pick.
func TestReviewFixRedVerifyAfterFixEscalatesModel(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}

	var responses []llm.Response

	responses = append(responses, stopResp("coder: fix one", 0.05))
	responses = append(responses, stopResp("coder: fix two", 0.05))
	responses = append(responses, panelApproves()...)

	client := &planLLM{responses: responses}
	d := reviewTestDeps(t, ops, git, client, escalationRegistry())
	o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}, 0)
	o.verify = &verifyPlan{Argv: []string{"verify"}, Display: "verify", Source: verifySourceDetected, Timeout: time.Minute}

	round := 0
	o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
		round++
		if round <= 2 {
			return verifyexec.Outcome{ExitCode: 1, Output: "--- FAIL: TestX\nexit status 1"}
		}

		return verifyexec.Outcome{ExitCode: 0}
	}

	require.NoError(t, runReview(context.Background(), o))

	require.GreaterOrEqual(t, len(client.models), 2, "models=%v", client.models)
	assert.Equal(t, "alpha/coder", client.models[0], "round 1's red verify does not escalate; models=%v", client.models)
	assert.Equal(t, "beta/coder", client.models[1],
		"a fix that left the verify red escalates the next fix; models=%v", client.models)
}

// TestReviewFixNoAlternativeModelParks: after a failed fix round, when the
// escalated pick is the model that just failed - a single-model registry or an
// operator coder pin - the loop parks at once with the model named, instead of
// replaying the same fix round.
func TestReviewFixNoAlternativeModelParks(t *testing.T) {
	only := 0.90
	single := registry.NewRegistryFromParts(
		llm.Catalog{{ID: "only/coder", ContextLength: 200000, SupportedParameters: []string{"tools"}}},
		registry.Priors{Models: map[string]registry.PriorEntry{"only/coder": {Coder: &only}}},
		nil, nil, "only/coder")

	cases := []struct {
		name  string
		reg   *registry.Registry
		pin   string
		model string
	}{
		{"single-model registry", single, "", "only/coder"},
		{"operator coder pin", reviewerRegistry(), "pinned/model", "pinned/model"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops := &fakeOps{}
			git := &fakeGit{committed: false}

			var responses []llm.Response

			responses = append(responses, panelRejects("bug")...)
			responses = append(responses, stopResp("coder: tried", 0.05))
			responses = append(responses, panelRejects("still bug")...)

			client := &planLLM{responses: responses}
			d := reviewTestDeps(t, ops, git, client, tc.reg)
			o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress", ModelCoder: tc.pin}, 0)

			err := runReview(context.Background(), o)

			var parked *ReviewParkedError

			require.ErrorAs(t, err, &parked, "no alternative fix model parks; err=%v", err)
			assert.Equal(t, 1, countPrefix(git.recorded(), "CommitFixup:"), "exactly one fix round ran; git=%v", git.recorded())
			assert.True(t, ops.loggedContains("no other fix model"), "the park names the cause; logs=%v", ops.recorded())
			assert.True(t, ops.loggedContains(tc.model), "the park names the model that failed; logs=%v", ops.recorded())
		})
	}
}

// TestReviewApprovedNitOnlyFindingsSkipCleanupPass pins that severity is a
// decision, not decoration: an approved verdict carrying nothing but nits is
// not worth a fix-coder run, a fixup commit and a push. Exactly four responses
// are queued, so a cleanup pass would show up as a fifth call.
func TestReviewApprovedNitOnlyFindingsSkipCleanupPass(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: nothing blocking", 0.01),
		stopResp("Design: naming nit", 0.01),
		stopResp("Security: looks fine", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[`+
			`{"file":"a.go","issue":"name could be shorter","suggestion":"rename","severity":"nit"},`+
			`{"file":"b.go","issue":"stray blank line","suggestion":"drop it","severity":"nit"}]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	require.Len(t, client.tasks, 4, "three specialists and one synthesis - no fix coder; tasks=%v", client.tasks)
	assert.Equal(t, -1, indexOfPrefix(git.recorded(), "CommitFixup:"),
		"nit-only findings must not buy a fixup commit; git=%v", git.recorded())
	assert.Contains(t, o.reviewSummary, "were not fixed",
		"the nits still reach the PR body, framed as unfixed")
	assert.Contains(t, o.reviewSummary, "a.go", "the nits ride the summary rather than being discarded")
}

// TestReviewApprovedNonNitFindingRunsCleanupPass is the other half of the nit
// gate: one finding above nit is enough to earn the pass, even alongside nits.
func TestReviewApprovedNonNitFindingRunsCleanupPass(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: off-by-one", 0.01),
		stopResp("Design: naming nit", 0.01),
		stopResp("Security: looks fine", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[`+
			`{"file":"a.go","issue":"name could be shorter","suggestion":"rename","severity":"nit"},`+
			`{"file":"b.go","issue":"off-by-one in the loop bound","suggestion":"use <=","severity":"minor"}]}`, 0.02),
		stopResp("coder: fixed", 0.05),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	require.Len(t, client.tasks, 5, "the cleanup pass runs; tasks=%v", client.tasks)
	assert.GreaterOrEqual(t, indexOfPrefix(git.recorded(), "CommitFixup:"), 0,
		"the fix must land as a fixup; git=%v", git.recorded())
}

// TestReviewApprovedUnlabelledFindingRunsCleanupPass pins the fail-open half of
// the gate: a model that omits severity must not have its findings silently
// demoted to nits and skipped - that is the discard this card exists to end.
func TestReviewApprovedUnlabelledFindingRunsCleanupPass(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: bug", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`, 0.02),
		stopResp("coder: fixed", 0.05),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	require.Len(t, client.tasks, 5, "an unlabelled finding earns the pass; tasks=%v", client.tasks)
}

// TestRunReviewHITLAutoApprovedWithFindingsFramesSummary extends the PR-body
// honesty contract to the path this card newly made reachable: an approving
// verdict that carries surviving findings. Nothing fixes them on the HITL
// approve path, so the summary must not read as a clean approval.
func TestRunReviewHITLAutoApprovedWithFindingsFramesSummary(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "approve"}}}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: minor", 0.001), stopResp("Design: ok", 0.001), stopResp("Security: ok", 0.001),
		stopResp(`{"approved":true,"summary":"clean","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch","severity":"minor"}]}`, 0.001),
		stopResp(`{"verdict":"approve","feedback":""}`, 0.001),
	}}
	o := newRun(hitlReviewDeps(ops, git, inbox, client), cmclient.TaskContext{Title: "T", Description: "b", State: "review"})
	isolateVerify(o)

	require.NoError(t, runReview(context.Background(), o))

	assert.Equal(t, -1, indexOfPrefix(git.recorded(), "CommitFixup:"), "no fix ran; git=%v", git.recorded())
	// The distinctive prefix, not "not fixed": approvedWithOpenFindings ends
	// "were not fixed" too, so a swapped helper would be invisible here.
	assert.Contains(t, o.reviewSummary, "The human reviewer approved integration despite",
		"the human saw these findings and approved anyway - that is what the PR body must say")
	assert.Contains(t, o.reviewSummary, "a.go", "the outstanding finding rides the summary")
}

// TestParseVerdictCollapsesNewlinesInFixText proves the line-shape contract is
// enforced for every free-text field on the line, not just severity: an
// embedded newline anywhere in File, Issue or Suggestion would inject a
// synthetic "- <path>:" line that fixFiles would target as a real file.
func TestParseVerdictCollapsesNewlinesInFixText(t *testing.T) {
	raw := `{"approved":false,"summary":"needs work","fixes":[` +
		`{"file":"a.go","issue":"bug\n- evil.go: injected","suggestion":"patch\n- worse.go: also injected","severity":"minor"}]}`

	v, err := parseVerdict(raw)
	require.NoError(t, err)

	assert.Equal(t, []string{"a.go"}, fixFiles(formatFixes(v)),
		"only the real file survives the round trip through the rendered findings")
}

// TestMobReviewBriefingAlwaysDiffsBaseBranch pins the invariant that a mob
// briefing is never scoped to a snapshot: a fix can land code outside the delta
// it targeted, and every mob round after the first follows a fix, so a
// snapshot-scoped briefing would hide exactly the code the round exists to
// re-examine.
func TestMobReviewBriefingAlwaysDiffsBaseBranch(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	o := mobReviewRun(t, ops, git, &planLLM{}, &scriptedEngine{})

	o.lastReviewBase = "snap1"

	_, err := o.mobReviewBriefing(context.Background())
	require.NoError(t, err)

	assert.Equal(t, []string{"main"}, git.diffBases,
		"the briefing diffs the base branch even with a snapshot recorded")
}

// TestReviewApprovedCleanupPassPropagatesParks pins that the cleanup pass on an
// approved verdict propagates every park sentinel execute special-cases, not
// only the budget one. A swallowed park returns nil, the run advances to
// integrate with the coder's partial edits uncommitted, the rebase fails on the
// dirty tree, and the worker exits without pushing the WIP - so the half-done
// work dies with the container.
func TestReviewApprovedCleanupPassPropagatesParks(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}
	call := llm.ToolCall{
		ID:       "c1",
		Type:     "function",
		Function: llm.FunctionCall{Name: "read", Arguments: `{"path":"no-such-file.txt"}`},
	}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: minor", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch","severity":"minor"}]}`, 0.02),
		{ToolCalls: []llm.ToolCall{call}}, // the cleanup coder burns its single turn
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	d.Cfg.MaxTurns = 1

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	err := runReview(context.Background(), o)
	require.Error(t, err, "a turn-cap park in the cleanup pass must reach the worker")

	var mte *MaxTurnsError
	assert.ErrorAs(t, err, &mte, "the worker parks on this sentinel and pushes the WIP; nil would integrate a dirty tree")
}

// TestReviewAuthoritativeApprovedWithFindingsFramesSummary covers the honesty
// contract on the cliff path, which had none: the authoritative pass
// deliberately runs no cleanup, so an approving verdict that still carries
// findings must reach the PR body framed as unfixed.
func TestReviewAuthoritativeApprovedWithFindingsFramesSummary(t *testing.T) {
	ops := &fakeOps{reviewAttempts: 4}
	git := &fakeGit{committed: true, headSHA: "snap1"}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: minor", 0.01),
		stopResp("Design: ok", 0.01),
		stopResp("Security: ok", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch","severity":"minor"}]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	// Seed ReviewAttempts = cap-1 (the deps set the cap to 5) so the first round
	// is the authoritative one.
	tc := cmclient.TaskContext{
		Title: "Parent", Description: "body",
		State: "in_progress", ReviewAttempts: 4,
	}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o))

	assert.Equal(t, -1, indexOfPrefix(git.recorded(), "CommitFixup:"),
		"the cliff runs no cleanup pass; git=%v", git.recorded())
	assert.Contains(t, o.reviewSummary, "were not fixed",
		"findings surviving an approval at the cliff must not read as addressed")
	assert.Contains(t, o.reviewSummary, "a.go", "the finding rides the summary")
}

// reviewerPrior builds a PriorEntry with only the reviewer role scored.
func reviewerPrior(v float64) registry.PriorEntry {
	return registry.PriorEntry{Reviewer: &v}
}

// TestReviewPanelIsEmptyWhenNoReviewerIsSelectable pins the panel builder's own
// contract: an unservable reviewer role yields no panel at all. That empty
// panel is the condition the review phase turns into a park for a human, which
// TestRunSpecialistsNoReviewerParksInstead pins separately.
func TestReviewPanelIsEmptyWhenNoReviewerIsSelectable(t *testing.T) {
	reg := registry.NewRegistryFromParts(
		llm.Catalog{{ID: "weak/model", ContextLength: 200000, SupportedParameters: []string{"tools"}}},
		registry.Priors{Models: map[string]registry.PriorEntry{"weak/model": reviewerPrior(0.30)}},
		nil, nil, "") // no capable default: nothing is employable at all
	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reg)
	o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", State: "in_progress"}, 0)

	assert.Empty(t, o.reviewPanel(context.Background(), estimateTokens("diff"), false),
		"an unservable reviewer role yields no panel at all")
}

// TestRefusalSentinelsDifferByPhase pins the deliberate asymmetry a future
// reader will otherwise "fix" into consistency: no coder means there is no work
// (blocked park), no fix model means the code is already written and pushed, so
// a human taking it from review is the right destination.
func TestRefusalSentinelsDifferByPhase(t *testing.T) {
	reg := registry.NewRegistryFromParts(llm.Catalog{}, registry.Priors{}, nil, nil, "")

	t.Run("coder refusal parks the card as blocked", func(t *testing.T) {
		d := execTestDeps(&fakeOps{}, &fakeGit{}, &planLLM{})
		d.Registry = reg
		o := newExecRun(d, nil, 5)

		_, err := o.resolveCoderModel(context.Background(),
			subtaskRef{ID: "SUB-1", Sizing: seedSizing("moderate")}, "prompt")

		var nme *NoModelError

		require.ErrorAs(t, err, &nme)

		var rp *ReviewParkedError

		assert.NotErrorAs(t, err, &rp,
			"there is no work without a coder, so this must not be a review park")
	})

	t.Run("fix refusal parks the card in review", func(t *testing.T) {
		d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reg)
		o := newReviewRun(d, cmclient.TaskContext{}, 0)

		_, err := o.resolveFixModel(context.Background(), fixRequest{FixTier: "moderate"})

		var rp *ReviewParkedError

		require.ErrorAs(t, err, &rp)

		var nme *NoModelError

		assert.NotErrorAs(t, err, &nme,
			"the code is written and pushed, so a human takes it from review, not from blocked")
	})
}

// TestFixEscalationThatBuysNothingIsRecorded pins the no-op the card currently
// cannot see: the escalation climbs moderate -> complex, the complex rung is
// dry, and the walk lands back on the same model the un-escalated round would
// have used. The round still runs - the pick is fresh - but the card says the
// climb bought nothing.
func TestFixEscalationThatBuysNothingIsRecorded(t *testing.T) {
	ops := &fakeOps{}
	reg := registry.NewRegistryFromParts(
		llm.Catalog{{ID: "mid/coder", ContextLength: 200000, SupportedParameters: []string{"tools"}}},
		registry.Priors{Models: map[string]registry.PriorEntry{"mid/coder": coderPrior(0.78)}},
		nil, nil, "capable/default")
	client := &planLLM{responses: []llm.Response{stopResp("Applied the fix.", 0.01)}}
	d := reviewTestDeps(t, ops, &fakeGit{}, client, reg)
	o := newReviewRun(d, cmclient.TaskContext{}, 0)

	plain, err := o.resolveFixModel(context.Background(), fixRequest{FixTier: "moderate"})
	require.NoError(t, err)
	require.True(t, plain.AtBar())

	o.fixBarSteps = 1
	o.fixFailReason = "landed no commit"

	climbed, err := o.resolveFixModel(context.Background(), fixRequest{FixTier: "moderate"})
	require.NoError(t, err)

	assert.Equal(t, plain.Model, climbed.Model, "the climb reached the same model")
	assert.False(t, climbed.AtBar(), "and the caller can see the climb bought nothing")
	assert.Equal(t, registry.TierComplex, climbed.RequestedTier)
	assert.Equal(t, registry.TierModerate, climbed.MetTier)

	model, err := o.runFixModel(context.Background(), "fix it", fixRequest{Round: 2, FixTier: "moderate"})
	require.NoError(t, err)
	assert.Equal(t, "mid/coder", model)
	assert.True(t, ops.loggedContains("bought nothing"),
		"an escalation that reached no stronger model must say so; logs=%v", ops.logs)
}

// belowComplexReviewerRegistry seeds three reviewers that clear the moderate
// bar (0.76) but none that clears the complex bar (0.82) the authoritative
// pass asks for. One vendor prefix keeps the diversity preference out of it.
func belowComplexReviewerRegistry() *registry.Registry {
	return registry.NewRegistryFromParts(
		llm.Catalog{
			{ID: "acme/one", ContextLength: 200000, PromptPricePerTok: 4e-7, CompletionPricePerTok: 6e-7, SupportedParameters: []string{"tools"}},
			{ID: "acme/two", ContextLength: 200000, PromptPricePerTok: 4e-7, CompletionPricePerTok: 6e-7, SupportedParameters: []string{"tools"}},
			{ID: "acme/three", ContextLength: 200000, PromptPricePerTok: 4e-7, CompletionPricePerTok: 6e-7, SupportedParameters: []string{"tools"}},
			{ID: "default/model", ContextLength: 131072, SupportedParameters: []string{"tools"}},
		},
		registry.Priors{Models: map[string]registry.PriorEntry{
			"acme/one":   reviewerPrior(0.77),
			"acme/two":   reviewerPrior(0.78),
			"acme/three": reviewerPrior(0.79),
		}},
		nil, nil, "default/model")
}

// TestAuthoritativeReviewBelowItsBarRunsAndRecords pins the escalation gate on
// the last pass before a human park: refusing would spend a whole run to say
// nothing, so the pass RUNS and its verdict stands - but the card records that
// it ran below the bar it asked for, which is what turns an invisible collapse
// into a fact a human reading the card can weigh.
func TestAuthoritativeReviewBelowItsBarRunsAndRecords(t *testing.T) {
	panelResponses := func() []llm.Response {
		return []llm.Response{
			stopResp("Correctness ok", 0.01),
			stopResp("Design ok", 0.01),
			stopResp("Security ok", 0.01),
		}
	}

	t.Run("no seat clears the bar", func(t *testing.T) {
		ops := &fakeOps{}
		d := reviewTestDeps(t, ops, &fakeGit{}, &planLLM{responses: panelResponses()}, belowComplexReviewerRegistry())
		o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", State: "in_progress"}, 0)

		_, err := o.runSpecialists(context.Background(), true)
		require.NoError(t, err, "the authoritative pass still runs below its bar")

		assert.True(t, ops.loggedContains("authoritative review ran below its bar"),
			"the card must record that the last pass before a human park was under-powered; logs=%v", ops.logs)
	})

	t.Run("a seat clears the bar", func(t *testing.T) {
		ops := &fakeOps{}
		d := reviewTestDeps(t, ops, &fakeGit{}, &planLLM{responses: panelResponses()}, reviewerRegistry())
		o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", State: "in_progress"}, 0)

		_, err := o.runSpecialists(context.Background(), true)
		require.NoError(t, err)

		assert.False(t, ops.loggedContains("authoritative review ran below its bar"),
			"a panel that met its bar has nothing to record; logs=%v", ops.logs)
	})
}

// TestReviewPinnedPanelRendersRepeatedSeats pins that three copies of one
// pinned model do not read as three independent judgements: the synthesizer
// reads agreement as signal, so the seat count and the distinct-model count
// must stay separable in the card log.
func TestReviewPinnedPanelRendersRepeatedSeats(t *testing.T) {
	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reviewerRegistry())
	o := newReviewRun(d, cmclient.TaskContext{ModelReviewer: "pinned/model"}, 0)

	panel := o.reviewPanel(context.Background(), estimateTokens("diff"), false)
	require.Len(t, panel, reviewPanelSize)
	assert.Equal(t, 1, registry.DistinctModels(panel), "a pin fills every seat with the one model")

	summary := panelSummary(panel)
	assert.Equal(t, reviewPanelSize-1, strings.Count(summary, "repeat"),
		"every seat after the first repeats the pin; summary=%s", summary)
	assert.Contains(t, summary, "pinned", "and the summary says the panel came from an operator pin")
}

// TestPinnedFixModelReportsNoFabricatedShortfall pins that a selection the
// operator made by hand never produces an advisory about a bar it was never
// measured against. The pins this package synthesizes carry no prior, so a
// shortfall line for one would state a number nothing measured.
func TestPinnedFixModelReportsNoFabricatedShortfall(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{stopResp("Applied the fix.", 0.01)}}
	d := reviewTestDeps(t, ops, &fakeGit{}, client, reviewerRegistry())
	o := newReviewRun(d, cmclient.TaskContext{ModelCoder: "pinned/model"}, 0)
	o.fixBarSteps = 1
	o.fixFailReason = "landed no commit"

	model, err := o.runFixModel(context.Background(), "fix it", fixRequest{Round: 2, FixTier: "moderate"})
	require.NoError(t, err)
	assert.Equal(t, "pinned/model", model)

	for _, l := range ops.logs {
		assert.NotContains(t, l, "prior 0.00",
			"a pin this package synthesizes carries no measured prior, so no line may state one; logs=%v", ops.logs)
		assert.NotContains(t, l, "does not clear",
			"nothing measured this model against a bar, so no line may say it missed one; logs=%v", ops.logs)
		assert.NotContains(t, l, "no model clears",
			"a pinned model was chosen by hand, so no bar was searched; logs=%v", ops.logs)
		assert.NotContains(t, l, "bought nothing",
			"an escalation cannot be scored against a pin that has no prior; logs=%v", ops.logs)
	}
}

// TestFixEscalationNoOpReportsNoPriorRatherThanZero is the same guard on the
// other card line that prints a prior: an escalated round that landed on the
// operator's unrated fallback must not report it as rated zero.
func TestFixEscalationNoOpReportsNoPriorRatherThanZero(t *testing.T) {
	ops := &fakeOps{}
	client := &planLLM{responses: []llm.Response{stopResp("Applied the fix.", 0.01)}}
	// No priors: the escalated pick can only be the capable default.
	reg := registry.NewRegistryFromParts(reviewerCatalog(), registry.Priors{}, nil, nil, "default/model")
	d := reviewTestDeps(t, ops, &fakeGit{}, client, reg)
	o := newReviewRun(d, cmclient.TaskContext{}, 0)
	o.fixBarSteps = 1
	o.fixFailReason = "landed no commit"

	_, err := o.runFixModel(context.Background(), "fix it", fixRequest{Round: 2, FixTier: "moderate"})
	require.NoError(t, err)

	require.True(t, ops.loggedContains("bought nothing"), "logs=%v", ops.logs)

	for _, l := range ops.logs {
		assert.NotContains(t, l, "prior 0.00",
			"the fallback is unrated, not rated worst possible; logs=%v", ops.logs)
	}
}

// TestReviewParkedErrorNamesItsCause pins that the sentinel reports the reason
// it was actually constructed with, and that a bare construction still means
// what it always meant.
func TestReviewParkedErrorNamesItsCause(t *testing.T) {
	assert.Equal(t, "review parked: attempts cap exhausted without approval",
		(&ReviewParkedError{}).Error(),
		"a bare construction keeps the meaning every existing site gave it")

	assert.Equal(t, "review parked: no fix model is selectable",
		(&ReviewParkedError{Reason: reviewParkedNoFixModel}).Error())
}

// TestFixModelRefusalNamesItsRealCause covers the one park path whose Error()
// text IS the card line: the cleanup fix pass renders the error verbatim into
// its own log entry, with no accurate line of its own ahead of it. A fixed
// attempts-cap string there would tell the operator the cap was exhausted when
// it was not, and hide the real cause entirely.
func TestFixModelRefusalNamesItsRealCause(t *testing.T) {
	reg := registry.NewRegistryFromParts(llm.Catalog{}, registry.Priors{}, nil, nil, "")
	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reg)
	o := newReviewRun(d, cmclient.TaskContext{Title: "Parent"}, 0)

	committed, err := o.runFix(context.Background(), fixRequest{Findings: "- a.go: something", Round: 2, FixTier: "moderate"})
	require.Error(t, err)
	assert.False(t, committed)

	var parked *ReviewParkedError

	require.ErrorAs(t, err, &parked, "the fix refusal still parks the card in review")

	assert.Contains(t, err.Error(), "no fix model is selectable",
		"the card must read the true cause; err=%v", err)
	assert.NotContains(t, err.Error(), "attempts cap",
		"no attempts cap was reached on this path; err=%v", err)
}

// The terminal, last-chance fix before a park runs at TierComplex
// unconditionally, which today buys it 1.5x the base turns. Sourcing the fix
// budget from the CARD's budget would silently cut a third of the turns off the
// one run where running out ends the run rather than deferring it - and would
// re-conflate exactly what the two-axis split exists to separate.
func TestFixBudgetComesFromTheFixBarNotTheCard(t *testing.T) {
	ops := &fakeOps{}
	o := newReviewRun(reviewTestDeps(t, ops, &fakeGit{}, &planLLM{}, reviewerRegistry()),
		cmclient.TaskContext{Title: "Card"}, 0)
	// A card budget ABOVE every rung these rows expect: sourcing the fix budget
	// from it would show up as 135 on all three, where a card budget at or below
	// the bar's own seed would be re-seeded back to the right answer and hide
	// the mutation entirely.
	o.cardSizing = sizing{Bar: registry.TierModerate, Budget: 3}

	auth := o.fixSizing(fixRequest{Round: 3, Authoritative: true})
	assert.Equal(t, registry.TierComplex, auth.Bar)
	assert.Equal(t, 68, turnBudget(45, auth.Budget), "the authoritative pass keeps its 1.5x")

	hot := o.fixSizing(fixRequest{Round: 1, FixTier: "critical"})
	assert.Equal(t, 90, turnBudget(45, hot.Budget), "a critical fix_tier still buys 2x on a moderate card")

	plain := o.fixSizing(fixRequest{Round: 1})
	assert.Equal(t, registry.TierModerate, plain.Bar, "an empty fix_tier falls back to the card bar")
	assert.Equal(t, 45, turnBudget(45, plain.Budget))
}

// runFix is SHARED with pr_gates, which runs AFTER approval - so resetting the
// escalation state on an approving verdict made the first Copilot fix round
// re-pick the exact model that had already failed twice, at the un-escalated
// bar. The counters must be monotone across an approval.
func TestFixEscalationSurvivesApproval(t *testing.T) {
	ops := &fakeOps{}
	o := newReviewRun(reviewTestDeps(t, ops, &fakeGit{}, &planLLM{}, reviewerRegistry()),
		cmclient.TaskContext{Title: "Card"}, 0)
	o.cardSizing = sizing{Bar: registry.TierModerate, Budget: 0}
	o.lastFixModel = "vendor/weak"

	o.markFixFailed("produced no change")
	o.markFixFailed("left the verify red")
	o.markFixCapped()

	gate := o.fixSizing(fixRequest{Round: 1}) // a pr_gates round, after approval
	assert.Equal(t, registry.TierCritical, gate.Bar, "two failed rounds climb two bar rungs")
	assert.Equal(t, maxBudgetStep, gate.Budget,
		"the critical bar re-seeds the budget at the ladder's ceiling, and the capped round has nothing left to widen")
	assert.True(t, o.fixFailed["vendor/weak"], "the failed fixer stays excluded after approval")
}

// A failed round climbs the bar, and the round that runs at the CLIMBED bar
// must get the window that bar opens - which is what the pre-split code did,
// since it sized every fix run on the escalated tier. Seeding from the bar the
// round started at instead silently hands the harder bar the base window, on
// the most common escalation path there is: round 1 lands no commit, round 2
// runs climbed.
func TestEscalatedFixRoundKeepsThePreSplitTurnBudget(t *testing.T) {
	tests := []struct {
		cardBar registry.Tier
		wantBar registry.Tier
		// The cap the harness receives against an operator base of 20.
		wantTurns int
	}{
		{registry.TierSimple, registry.TierModerate, 20},
		{registry.TierModerate, registry.TierComplex, 30},
		{registry.TierComplex, registry.TierCritical, 40},
		{registry.TierCritical, registry.TierCritical, 40},
	}

	for _, tt := range tests {
		t.Run(string(tt.cardBar), func(t *testing.T) {
			o := newReviewRun(reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reviewerRegistry()),
				cmclient.TaskContext{Title: "Card"}, 0)
			o.cardSizing = sizing{Bar: tt.cardBar, Budget: seedBudgetStep(tt.cardBar)}
			o.lastFixModel = "vendor/weak"
			o.markFixFailed("produced no change")

			got := o.fixSizing(fixRequest{Round: 2})
			assert.Equal(t, tt.wantBar, got.Bar)
			assert.Equal(t, tt.wantTurns, turnBudget(20, got.Budget),
				"the climbed bar must run on the window that bar opens")
		})
	}
}

// A turn cap widens the fix budget WITHOUT blaming the model: it ran out of
// room, it was not shown to be too weak, and re-running it wider is the fix.
func TestFixCapWidensBudgetWithoutBlamingTheModel(t *testing.T) {
	ops := &fakeOps{}
	o := newReviewRun(reviewTestDeps(t, ops, &fakeGit{}, &planLLM{}, reviewerRegistry()),
		cmclient.TaskContext{Title: "Card"}, 0)
	o.cardSizing = sizing{Bar: registry.TierModerate, Budget: 0}
	o.lastFixModel = "vendor/fine"

	o.markFixCapped()

	next := o.fixSizing(fixRequest{Round: 2})
	assert.Equal(t, 1, next.Budget)
	assert.Equal(t, registry.TierModerate, next.Bar, "a cap must not raise the bar")
	assert.False(t, o.fixFailed["vendor/fine"], "a capped model is still eligible, with more room")
}

// The post-approval cleanup pass must not escalate - but as a property of that
// call site, not by LOWERING shared state pr_gates later reads. NoEscalate must
// gate the park guard too, or the cleanup pass silently stops running on every
// card whose earlier rounds failed.
func TestCleanupPassNeitherEscalatesNorLowers(t *testing.T) {
	ops := &fakeOps{}
	o := newReviewRun(reviewTestDeps(t, ops, &fakeGit{}, &planLLM{}, reviewerRegistry()),
		cmclient.TaskContext{Title: "Card"}, 0)
	o.cardSizing = sizing{Bar: registry.TierModerate, Budget: 1}
	o.lastFixModel = "vendor/weak"
	o.markFixFailed("produced no change")

	cleanup := o.fixSizing(fixRequest{Round: 3, NoEscalate: true})
	assert.Equal(t, registry.TierModerate, cleanup.Bar, "the cleanup pass runs at the un-escalated bar")
	assert.Equal(t, 0, cleanup.Budget, "and at the un-escalated budget")
	assert.Equal(t, 1, o.fixBarSteps, "but it must not LOWER the counters pr_gates reads later")
}

// NoEscalate gates the exhausted-fixers park as well as the sizing. The
// cleanup pass runs on a card review has ALREADY approved, so a card whose
// earlier rounds failed must still get its pass - while an ordinary round on
// the same state parks.
func TestCleanupPassRunsDespiteExhaustedFixers(t *testing.T) {
	// A pinned fix coder is returned by resolveFixModel even when it is
	// excluded, so it is the one live shape in which the park guard sees a pick
	// that already failed.
	newRunWithFailedPin := func(t *testing.T, ops *fakeOps, git *fakeGit) *run {
		t.Helper()

		client := &planLLM{responses: []llm.Response{stopResp("Applied the fix.", 0.01)}}
		d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
		o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", ModelCoder: "pinned/model"}, 0)
		o.lastFixModel = "pinned/model"
		o.markFixFailed("produced no change")

		return o
	}

	t.Run("an ordinary round parks", func(t *testing.T) {
		o := newRunWithFailedPin(t, &fakeOps{}, &fakeGit{committed: true})

		committed, err := o.runFix(context.Background(), fixRequest{Findings: "- a.go: something", Round: 2})
		assert.False(t, committed)

		var parked *ReviewParkedError

		require.ErrorAs(t, err, &parked)
		assert.Equal(t, reviewParkedFixExhausted, parked.Reason)
	})

	t.Run("the cleanup pass still runs", func(t *testing.T) {
		git := &fakeGit{committed: true}
		o := newRunWithFailedPin(t, &fakeOps{}, git)

		committed, err := o.runFix(context.Background(), fixRequest{Findings: "- a.go: something", Round: 3, NoEscalate: true})
		require.NoError(t, err, "the cleanup pass must not be refused by the exhausted-fixers park")
		assert.True(t, committed)
	})
}

// A capped round widens the next one; it is never evidence that a fixer
// FAILED, so it must not turn a "no model is selectable" refusal into the
// exhausted-fixers park - which would put a cause on the card that nothing
// observed.
func TestCappedRoundDoesNotClaimTheFixersAreExhausted(t *testing.T) {
	client := &planLLM{responses: []llm.Response{stopResp("Applied the fix.", 0.01)}}
	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{committed: true}, client, reviewerRegistry())
	o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", ModelCoder: "pinned/model"}, 0)
	// Set directly rather than through markFixFailed, which would climb the BAR
	// counter the guard is legitimately keyed on. A pin is returned by
	// resolveFixModel even when excluded, so this is the one live shape in which
	// the guard sees a pick that already failed.
	o.fixFailed = map[string]bool{"pinned/model": true}
	o.lastFixModel = "pinned/model"
	o.markFixCapped()

	committed, err := o.runFix(context.Background(), fixRequest{Findings: "- a.go: something", Round: 2})
	require.NoError(t, err, "a cap is not evidence that any fixer was too weak")
	assert.True(t, committed, "the widened round runs the same model again instead of parking")
}

// NoEscalate opts a call site out of the WHOLE correction, not just the bar.
// The post-approval cleanup pass runs on a card review has already approved, so
// it must take the plain pick - the failed-vendor preference is part of the
// escalation the request declined, and applying it there would quietly steer
// the pass away from a vendor it has no reason to avoid.
func TestCleanupPassTakesThePlainPick(t *testing.T) {
	reg := registry.NewRegistryFromParts(
		llm.Catalog{
			{ID: "acme/failed", ContextLength: 200000, PromptPricePerTok: 4e-7, CompletionPricePerTok: 6e-7, SupportedParameters: []string{"tools"}},
			{ID: "acme/cheap", ContextLength: 200000, PromptPricePerTok: 4e-7, CompletionPricePerTok: 6e-7, SupportedParameters: []string{"tools"}},
			{ID: "bravo/dear", ContextLength: 200000, PromptPricePerTok: 4e-6, CompletionPricePerTok: 6e-6, SupportedParameters: []string{"tools"}},
		},
		registry.Priors{Models: map[string]registry.PriorEntry{
			"acme/failed": coderPrior(0.90),
			"acme/cheap":  coderPrior(0.80),
			"bravo/dear":  coderPrior(0.80),
		}},
		nil, nil, "")
	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reg)
	o := newReviewRun(d, cmclient.TaskContext{Title: "Parent"}, 0)
	o.lastFixModel = "acme/failed"
	o.markFixFailed("produced no change")

	ordinary, err := o.resolveFixModel(context.Background(), fixRequest{Round: 2})
	require.NoError(t, err)
	assert.Equal(t, "bravo/dear", ordinary.Model,
		"an escalating round prefers a vendor that has not failed this card")

	cleanup, err := o.resolveFixModel(context.Background(), fixRequest{Round: 3, NoEscalate: true})
	require.NoError(t, err)
	assert.Equal(t, "acme/cheap", cleanup.Model,
		"the cleanup pass declined the escalation, so it declines its vendor preference too")
}

// runFixModel returns ("", err) on a cap. Assigning that unconditionally wipes
// lastFixModel, so the NEXT round's markFixFailed records nothing, fixFailed
// stays empty, the exclusion never fires and the "no other fix model" park can
// never trip. And a capped round's partial edits are the only evidence it
// produced - leaving them uncommitted makes the caller's retry unsound.
func TestCappedFixRoundKeepsTheModelAndCommitsItsWork(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	// Burn the whole (small) budget so the harness returns max_turns. A base of
	// 8 keeps the fixture clear of wrapUpTurns, so the nudge cannot become the
	// captured last user message and turn this into a test about the nudge.
	client := &planLLM{responses: burnResps(8)}

	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	d.Cfg.MaxTurns = 8

	o := newReviewRun(d, cmclient.TaskContext{Title: "Card"}, 0)
	o.cardSizing = sizing{Bar: registry.TierModerate, Budget: 0}
	o.lastFixModel = "vendor/prior"

	committed, err := o.runFix(context.Background(), fixRequest{Findings: "- a.go: something", Round: 1})

	var mte *MaxTurnsError

	require.ErrorAs(t, err, &mte, "the cap must still reach the caller")
	assert.True(t, committed, "a capped fix's partial edits are the only evidence it produced")
	assert.Contains(t, git.pushBranches, "cm/card-1",
		"an uncommitted, unpushed capped fix makes the caller's retry unsound")
	assert.NotEqual(t, "vendor/prior", o.lastFixModel, "lastFixModel must never be wiped or left stale")
	assert.NotEmpty(t, o.lastFixModel)
}

// --- read-only roots on the review panel ----------------------------------

// readProbeLLM drives every review specialist to read one path and records the
// tool result it got back; the synthesizer gets a clean approval. It routes on
// message shape, not a call counter, because SpawnSubagents runs the three
// specialists concurrently.
type readProbeLLM struct {
	mu          sync.Mutex
	path        string
	toolResults []string
}

func (c *readProbeLLM) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	return c.next(req)
}

func (c *readProbeLLM) SendStream(_ context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return c.next(req)
}

func (c *readProbeLLM) next(req llm.Request) (llm.Response, error) {
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "tool" {
		c.mu.Lock()
		c.toolResults = append(c.toolResults, req.Messages[n-1].Content)
		c.mu.Unlock()

		return llm.Response{Content: "no findings", FinishReason: "stop"}, nil
	}

	specialist := false

	for _, m := range req.Messages {
		if strings.Contains(m.Content, "You are a code-review specialist") {
			specialist = true

			break
		}
	}

	if !specialist {
		return llm.Response{Content: `{"approved":true,"summary":"clean","fixes":[]}`, FinishReason: "stop"}, nil
	}

	args, _ := json.Marshal(map[string]string{"path": c.path})

	return llm.Response{
		ToolCalls: []llm.ToolCall{{
			ID:       "probe-1",
			Type:     "function",
			Function: llm.FunctionCall{Name: "read", Arguments: string(args)},
		}},
		FinishReason: "tool_calls",
	}, nil
}

// TestReviewSubagentReadToolsCarryExtraRoots drives the whole chain the review
// panel depends on: Deps.ReadRoots reaches SubagentOpts.ExtraReadOnlyTools, and
// tools.NewRegistry's last-registration-wins means the widened read replaces the
// workspace-confined one SpawnSubagents registers first.
func TestReviewSubagentReadToolsCarryExtraRoots(t *testing.T) {
	dep := t.TempDir()
	depFile := filepath.Join(dep, "api.go")
	require.NoError(t, os.WriteFile(depFile, []byte("package dep // WIDGET\n"), 0o600))

	tests := []struct {
		name  string
		roots []string
		want  string
	}{
		{"a declared root resolves", []string{dep}, "WIDGET"},
		{"no declaration keeps the confinement", nil, "tool error"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &readProbeLLM{path: depFile}
			d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, client, reviewerRegistry())
			d.ReadRoots = tc.roots

			o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}, 0)
			require.NoError(t, runReview(context.Background(), o))

			client.mu.Lock()
			defer client.mu.Unlock()

			require.Len(t, client.toolResults, 3, "each of the three specialists must have probed once")

			for i, got := range client.toolResults {
				assert.Contains(t, got, tc.want, "specialist %d tool result: %q", i, got)
			}
		})
	}
}

// TestReviewSubagentToolsKeepTheSkillTool: widening the panel's read tools must
// not cost it the Skill tool - both ride the same ExtraReadOnlyTools slice.
func TestReviewSubagentToolsKeepTheSkillTool(t *testing.T) {
	t.Parallel()

	skillsDir := filepath.Join(t.TempDir(), "skills")
	require.NoError(t, os.MkdirAll(filepath.Join(skillsDir, "go-development"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillsDir, "go-development", "SKILL.md"),
		[]byte("---\nname: go-development\ndescription: Use for Go.\n---\nbody"), 0o644))

	skill, ok := tools.NewSkillTool(skillsDir, nil, false, nil)
	require.True(t, ok)

	ws := t.TempDir()
	dep := t.TempDir()

	var names []string
	for _, tl := range reviewSubagentTools(ws, []string{dep}, skill) {
		names = append(names, tl.Name())
	}

	assert.Subset(t, names, []string{"skill", "read", "grep", "glob"})

	// No declaration: the panel gets exactly what it got before, the Skill tool alone.
	names = nil
	for _, tl := range reviewSubagentTools(ws, nil, skill) {
		names = append(names, tl.Name())
	}

	assert.Equal(t, []string{"skill"}, names)
	assert.Empty(t, reviewSubagentTools(ws, nil, nil))
}

// TestReviewSubagentToolsLogsTheDeclarationOutcome: the panel's widened tools
// go through the same sanitizeReadRoots gate as the worker's - a declared
// root the harness drops must not vanish silently here either.
func TestReviewSubagentToolsLogsTheDeclarationOutcome(t *testing.T) {
	buf := captureReadRootsLog(t)

	ws := t.TempDir()
	present := t.TempDir()
	absent := filepath.Join(t.TempDir(), "never-created")

	reviewSubagentTools(ws, []string{present, absent}, nil)

	logged := buf.String()
	assert.Contains(t, logged, present, "the surviving root must be logged as effective")
	assert.Contains(t, logged, absent, "the dropped root must be named")
	assert.Contains(t, logged, string(tools.DropReasonNonexistent), "the drop reason must be named")

	// No declaration: nothing to log, matching the early-return that also
	// skips building the widened tools.
	buf.Reset()
	reviewSubagentTools(ws, nil, nil)
	assert.Empty(t, buf.String())
}

// TestExtraReadOnlyToolsOverrideConfinedDefaults pins the harness mechanism the
// panel wiring rides on, mirroring how SpawnSubagents builds its child registry:
// a duplicate name keeps the LAST registration.
func TestExtraReadOnlyToolsOverrideConfinedDefaults(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	dep := t.TempDir()
	depFile := filepath.Join(dep, "api.go")
	require.NoError(t, os.WriteFile(depFile, []byte("package dep // WIDGET\n"), 0o600))

	reg := tools.NewRegistry(append(tools.ReadOnlyTools(ws), reviewSubagentTools(ws, []string{dep}, nil)...)...)

	read, ok := reg.Get("read")
	require.True(t, ok)

	res, err := read.Execute(context.Background(), map[string]any{"path": depFile})
	require.NoError(t, err, "the widened read must win over the confined default")
	assert.Contains(t, res.Text, "WIDGET")
}

// TestReadRootsReachTheSpecialistPrompt: the review panel reads through the same
// widened tools, so it needs the same declaration in its prompt.
func TestReadRootsReachTheSpecialistPrompt(t *testing.T) {
	tests := []struct {
		name  string
		roots []string
	}{
		{"declared roots are named", []string{"/declared/dep-source", "/declared/other"}},
		{"no declaration leaves the prompt alone", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &planLLM{responses: []llm.Response{
				stopResp("Correctness: fine", 0.01),
				stopResp("Design: fine", 0.01),
				stopResp("Security: fine", 0.01),
				stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
			}}

			d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, client, reviewerRegistry())
			d.ReadRoots = tc.roots

			o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}, 0)
			require.NoError(t, runReview(context.Background(), o))

			client.mu.Lock()
			defer client.mu.Unlock()

			var specialists []string

			for _, task := range client.tasks {
				if strings.Contains(task, "code-review specialist") {
					specialists = append(specialists, task)
				}
			}

			require.Len(t, specialists, 3)

			for i, task := range specialists {
				for _, r := range tc.roots {
					assert.Contains(t, task, r, "specialist %d must be told about %s", i, r)
				}

				if len(tc.roots) == 0 {
					assert.NotContains(t, task, "outside the workspace",
						"specialist %d must carry no roots line when nothing is declared", i)
				}
			}
		})
	}
}

// A capped review fix round must not park the whole run. The review loop is the
// one place a turn-cap correction is free: the round is already counted against
// attemptsCap, so the loop stays bounded and the next round simply runs wider.
//
// Response script: round 1's panel and synthesis (four one-turn calls) revise;
// the round-1 fix coder then burns the whole 5-turn budget and returns
// max_turns; round 2's panel and synthesis approve. The panel and synthesis
// calls all finish on their first turn, so the small MaxTurns caps only the fix
// coder.
func TestCappedReviewFixRoundRetriesWiderInsteadOfParking(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}

	responses := []llm.Response{
		stopResp("Correctness: broken", 0.01),
		stopResp("Design: broken", 0.01),
		stopResp("Security: fine", 0.01),
		stopResp(`{"approved":false,"summary":"needs work","fixes":[{"file":"a.go","issue":"bug","suggestion":"fix","severity":"major"}]}`, 0.02),
	}
	responses = append(responses, burnResps(5)...)
	responses = append(responses,
		stopResp("Correctness: fine", 0.01),
		stopResp("Design: fine", 0.01),
		stopResp("Security: fine", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
	)

	d := reviewTestDeps(t, ops, git, &planLLM{responses: responses}, reviewerRegistry())
	d.Cfg.MaxTurns = 5

	o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}, 0)

	require.NoError(t, runReview(context.Background(), o),
		"a capped fix round must not park the whole run")

	assert.Equal(t, 1, o.fixBudgetSteps, "the cap widened the next round by one rung")
	assert.Zero(t, o.fixBarSteps, "the retried round approved, so nothing climbed the bar")
	assert.True(t, ops.loggedContains("hit its turn cap"), "logs=%v", ops.logs)
	assert.GreaterOrEqual(t, indexOfPrefix(git.recorded(), "CommitFixup:"), 0,
		"the capped round's partial work is still committed; git=%v", git.recorded())
}

// A capped review fix round that committed NOTHING must not retry wider: HEAD
// is unchanged, so a second panel would critique the exact diff round 1
// already reviewed - a full extra 3-reviewer panel bought for no new evidence.
// The CI gate's capped arm already requires a push before retrying (see
// gates.go's ciFixRound); this is the same rule ported to the review loop.
//
// Response script: round 1's panel and synthesis (four one-turn calls) revise;
// the round-1 fix coder then burns the whole 5-turn budget and returns
// max_turns, landing no commit. Only five responses beyond round 1 are
// scripted - a retried round 2 would run out of scripted panel responses and
// either hang the assertions on stale content or misreport findings, so
// exhausting the script is itself evidence the retry did not happen.
func TestCappedReviewFixRoundWithNoCommitParksInsteadOfRetrying(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: false}

	responses := []llm.Response{
		stopResp("Correctness: broken", 0.01),
		stopResp("Design: broken", 0.01),
		stopResp("Security: fine", 0.01),
		stopResp(`{"approved":false,"summary":"needs work","fixes":[{"file":"a.go","issue":"bug","suggestion":"fix","severity":"major"}]}`, 0.02),
	}
	responses = append(responses, burnResps(5)...)

	client := &planLLM{responses: responses}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())
	d.Cfg.MaxTurns = 5

	o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}, 0)

	err := runReview(context.Background(), o)

	var mte *MaxTurnsError
	require.ErrorAs(t, err, &mte, "a capped round that committed nothing must park, never retry")

	client.mu.Lock()
	specialists := 0

	for _, task := range client.tasks {
		if strings.Contains(task, "code-review specialist") {
			specialists++
		}
	}

	client.mu.Unlock()

	assert.Equal(t, 3, specialists, "no second panel - the capped, uncommitted round bought no new evidence")
	assert.Zero(t, o.fixBudgetSteps, "nothing widened; the round never retried")
	assert.Equal(t, -1, indexOfPrefix(git.recorded(), "Push:"), "an uncommitted round has nothing to push")
}

// A turn cap widens the budget and says nothing about the fixer's quality, so
// the round after a cap must not be reported to the operator as an escalation -
// and a cap that follows a genuine quality failure must not overwrite the
// reason that failure gave the escalation. Sharing one reason string between
// the two corrections told the operator that a turn cap had bought a stronger
// model.
func TestFixRoundReportsTheCorrectionItActuallyMade(t *testing.T) {
	tests := []struct {
		name         string
		mark         func(o *run)
		wantReason   string
		unwantReason string
		wantWord     string
		unwantWord   string
	}{
		{
			name:         "capped only",
			mark:         func(o *run) { o.markFixCapped() },
			wantReason:   "hit its turn cap",
			unwantReason: "produced no change",
			wantWord:     "widened",
			unwantWord:   "escalated",
		},
		{
			name: "escalated then capped",
			mark: func(o *run) {
				o.markFixFailed("produced no change")
				o.markFixCapped()
			},
			wantReason:   "produced no change",
			unwantReason: "hit its turn cap",
			wantWord:     "escalated",
			unwantWord:   "widened",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{}
			o := newReviewRun(reviewTestDeps(t, ops, &fakeGit{committed: true},
				&planLLM{responses: []llm.Response{stopResp("coder: fixed", 0.01)}}, reviewerRegistry()),
				cmclient.TaskContext{Title: "Card"}, 0)
			o.lastFixModel = "vendor/weak"
			tt.mark(o)

			_, err := o.runFixModel(context.Background(), "fix prompt", fixRequest{Round: 2})
			require.NoError(t, err)

			assert.True(t, ops.loggedContains(tt.wantWord), "logs=%v", ops.logs)
			assert.False(t, ops.loggedContains(tt.unwantWord), "logs=%v", ops.logs)
			assert.True(t, ops.loggedContains(tt.wantReason), "logs=%v", ops.logs)
			assert.False(t, ops.loggedContains(tt.unwantReason), "logs=%v", ops.logs)
		})
	}
}

// One fix round produces one outcome and is charged for it exactly once. The
// carrier is fixRan, which is true only when the IMMEDIATELY PRECEDING round
// both ran and committed, so a round the next iteration finds behind a red
// verify is one that has not already been judged.
//
// Three shapes, all of which charged twice or on the wrong axis at some point:
//
//   - A round truncated at its turn cap is the likeliest of all to leave the
//     verify red - it ran out of turns mid-work, and runFix committed the
//     partial result. That red verify is the cap showing through, already
//     charged on the budget axis, and a turn cap is zero evidence about
//     capability.
//   - The flag is per-round, not per-run, so omitting the write on the cap path
//     covers only a FIRST-round cap; once an earlier round has completed, the
//     cap must actively clear it.
//   - A round that committed nothing is charged on the spot. Its HEAD is
//     unchanged, so the next round's verify is the same subprocess against the
//     same tree and carries no new information - charging that too would jump
//     the bar two rungs on the evidence of one round.
//
// Every exclusion shrinks the pool toward the exhausted-fixers park, which the
// third row reaches for real.
func TestOneFixRoundOutcomeIsChargedOnce(t *testing.T) {
	panelRejects := func() []llm.Response {
		return []llm.Response{
			stopResp("Correctness: broken", 0.01),
			stopResp("Design: ok", 0.01),
			stopResp("Security: ok", 0.01),
			stopResp(`{"approved":false,"summary":"needs work","fixes":[{"file":"a.go","issue":"bug","suggestion":"fix","severity":"major"}]}`, 0.02),
		}
	}
	panelApproves := func() []llm.Response {
		return []llm.Response{
			stopResp("Correctness: fine", 0.01),
			stopResp("Design: fine", 0.01),
			stopResp("Security: fine", 0.01),
			stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
		}
	}
	// A red gate short-circuits the panel entirely, so a red round's only model
	// call is its own fix coder's.
	fixCompletes := stopResp("coder: fixed it this time", 0.05)

	tests := []struct {
		name string
		// script is the model conversation; redGate is the gate run that fails;
		// wantGates is how many rounds ran before the loop ended.
		script    []llm.Response
		redGate   int
		wantGates int
		// committed is what every fixup in the row lands. wantParked marks the
		// row that ends on the exhausted-fixers park instead of an approval.
		committed   bool
		wantParked  bool
		wantBar     int
		wantBudget  int
		wantFailers int
	}{
		{
			name: "the cap is the first fix round",
			// pass/reject/cap, red/fix, pass/approve.
			script: slices.Concat(panelRejects(), burnResps(5),
				[]llm.Response{fixCompletes}, panelApproves()),
			redGate:    2,
			wantGates:  3,
			committed:  true,
			wantBudget: 1,
		},
		{
			name: "a completed fix round precedes the cap",
			// pass/reject/fix, pass/reject/cap, red/fix, pass/approve. The first
			// round completing is what leaves the per-round flag set going into
			// the cap.
			script: slices.Concat(panelRejects(), []llm.Response{fixCompletes},
				panelRejects(), burnResps(5),
				[]llm.Response{fixCompletes}, panelApproves()),
			redGate:    3,
			wantGates:  4,
			committed:  true,
			wantBudget: 1,
		},
		{
			name: "the round before the red verify committed nothing",
			// pass/reject/no-op fix, red. The no-op is charged on the spot, so
			// the red verify behind it adds nothing; round 2's escalated pick
			// then finds the pool empty and parks, which is where the run ends.
			script:      slices.Concat(panelRejects(), []llm.Response{fixCompletes}),
			redGate:     2,
			wantGates:   2,
			committed:   false,
			wantParked:  true,
			wantBar:     1,
			wantFailers: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{}
			git := &fakeGit{committed: tt.committed, lastCommitTarget: "abc123"}

			d := reviewTestDeps(t, ops, git, &planLLM{responses: tt.script}, reviewerRegistry())
			d.Cfg.MaxTurns = 5

			o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}, 0)
			o.verify = &verifyPlan{Argv: []string{"verify"}, Display: "verify", Source: verifySourceDetected, Timeout: time.Minute}

			gateRuns := 0
			o.runVerify = func(context.Context, string, []string, time.Duration, []string) verifyexec.Outcome {
				gateRuns++
				if gateRuns == tt.redGate {
					return verifyexec.Outcome{ExitCode: 1, Output: "still failing"}
				}

				return verifyexec.Outcome{ExitCode: 0}
			}

			err := runReview(context.Background(), o)

			// Asserted first: a double charge derails the run in ways that would
			// otherwise mask the count this test exists for.
			assert.Equal(t, tt.wantBar, o.fixBarSteps, "one round, one charge on the bar axis")
			assert.Equal(t, tt.wantBudget, o.fixBudgetSteps, "one round, one charge on the budget axis")
			assert.Len(t, o.fixFailed, tt.wantFailers,
				"only a round judged too weak excludes its fixer; fixFailed=%v", o.fixFailed)

			if tt.wantParked {
				var parked *ReviewParkedError
				require.ErrorAs(t, err, &parked, "the row ends on the exhausted-fixers park")
			} else {
				require.NoError(t, err, "the fixer must still be selectable for the round that follows")
			}

			assert.Equal(t, tt.wantGates, gateRuns, "every scripted round ran")
		})
	}
}
