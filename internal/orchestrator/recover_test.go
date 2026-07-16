package orchestrator

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modelAwareLLM is a scripted llm.LLM that branches on the request model: any
// model in incapable returns a single bad-JSON tool call every turn, which the
// harness counts as unproductive and — after IncapableThreshold turns — reports
// as Reason "incapable" (the signal runModel turns into *IncapableError). An
// incapable call does NOT consume the response queue, so the script stays aligned
// to the CAPABLE calls only.
//
// Capable calls return responses in order: from the queued responses slice when
// set (the review path needs distinct specialist/verdict payloads), else a
// single canned finish call carrying a commit message (the execute path only
// needs one clean coder turn). Every call's model is recorded so tests can assert
// the retry switched to a different slug.
type modelAwareLLM struct {
	mu        sync.Mutex
	incapable map[string]bool
	responses []llm.Response // capable-call script; empty -> canned coder finish
	i         int
	models    []string
}

func (m *modelAwareLLM) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	return m.next(req), nil
}

func (m *modelAwareLLM) SendStream(_ context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return m.next(req), nil
}

func (m *modelAwareLLM) next(req llm.Request) llm.Response {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.models = append(m.models, req.Model)

	if m.incapable[req.Model] {
		// Unparseable tool-call arguments: the harness never marks the turn
		// capable, so consecutive turns drive it to ReasonIncapable. No queue
		// consumption — the script tracks only capable calls.
		return llm.Response{ToolCalls: []llm.ToolCall{{
			ID:       "bad-1",
			Type:     "function",
			Function: llm.FunctionCall{Name: "read", Arguments: `{ this is not json`},
		}}}
	}

	if len(m.responses) == 0 {
		return finishResp("feat(x): add y", 0.01)
	}

	if m.i >= len(m.responses) {
		return llm.Response{FinishReason: "stop"}
	}

	r := m.responses[m.i]
	m.i++

	return r
}

// recordedModels returns a copy of the per-call model log.
func (m *modelAwareLLM) recordedModels() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]string, len(m.models))
	copy(out, m.models)

	return out
}

// twoCoderRegistry builds a registry with two tools-capable coder candidates of
// equal price and distinct quality (alpha > beta), so SelectByComplexity picks
// alpha first; excluding alpha forces beta. capableDefault is a third, distinct
// model that is NOT a scored candidate, so it is only reached when every
// candidate is excluded (the cap path) — letting the cap test starve the loop.
func twoCoderRegistry() *registry.Registry {
	catalog := llm.Catalog{
		{ID: "alpha/coder", ContextLength: 200000, SupportedParameters: []string{"tools"}, PromptPricePerTok: 1e-6, CompletionPricePerTok: 1e-6},
		{ID: "beta/coder", ContextLength: 200000, SupportedParameters: []string{"tools"}, PromptPricePerTok: 1e-6, CompletionPricePerTok: 1e-6},
		{ID: "capable/default", ContextLength: 200000, SupportedParameters: []string{"tools"}},
	}

	alpha, beta := 0.90, 0.80
	priors := registry.Priors{
		Models: map[string]registry.PriorEntry{
			"alpha/coder": {Coder: &alpha, Reviewer: &alpha},
			"beta/coder":  {Coder: &beta, Reviewer: &beta},
		},
	}

	return registry.NewRegistryFromParts(catalog, priors, nil, nil, "capable/default")
}

// TestExecuteRecoversFromIncapableCoder pins the in-run recovery: the first
// selected coder model is harness-incapable, so the orchestrator blacklists it,
// excludes it, re-selects a DIFFERENT model, and re-runs the same subtask, which
// then succeeds. One recovery -> reselects == 1.
func TestExecuteRecoversFromIncapableCoder(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	llmFake := &modelAwareLLM{incapable: map[string]bool{"alpha/coder": true}}

	d := execTestDeps(ops, git, llmFake)
	d.Registry = twoCoderRegistry()

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "moderate"}}, 0)
	require.NoError(t, runExecute(context.Background(), o),
		"the run must recover via the second model, not error out")

	// The failed (first-selected) model was blacklisted on CM.
	assert.Contains(t, ops.recorded(), "BlacklistModel:CARD-1/alpha/coder",
		"the incapable model must be reported to CM")

	// Recovery happened exactly once and recorded the exclusion.
	assert.Equal(t, 1, o.reselects, "exactly one re-selection after a single incapable model")
	assert.True(t, o.excluded["alpha/coder"], "the failed model must be in the per-card exclude set")

	// The retry ran on a DIFFERENT model than the failed one, and the subtask
	// completed on that model.
	models := llmFake.recordedModels()
	require.NotEmpty(t, models)
	assert.Equal(t, "alpha/coder", models[0], "the first attempt uses the best-value pick")

	last := models[len(models)-1]
	assert.NotEqual(t, "alpha/coder", last, "the retry must switch off the incapable model")
	assert.Equal(t, "beta/coder", last, "the retry must use the next-best candidate")

	// The subtask completed (via the second model).
	assert.Contains(t, ops.recorded(), "CompleteTask:SUB-1")
}

// TestExecuteIncapableCapParks pins the cap: a coder model that is ALWAYS
// incapable (every candidate AND the capable default) drives reselects to 3 and
// then the run parks with an error instead of looping forever.
func TestExecuteIncapableCapParks(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true}
	// Every model the selector can return is incapable, so no re-selection ever
	// yields a working model — the cap is the only thing that stops the loop.
	llmFake := &modelAwareLLM{incapable: map[string]bool{
		"alpha/coder":     true,
		"beta/coder":      true,
		"capable/default": true,
	}}

	d := execTestDeps(ops, git, llmFake)
	d.Registry = twoCoderRegistry()

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "First", Tier: "moderate"}}, 0)
	err := runExecute(context.Background(), o)

	require.Error(t, err, "an always-incapable model must eventually park")

	var ie *IncapableError
	require.ErrorAs(t, err, &ie, "the park error must wrap the IncapableError")

	assert.Equal(t, 3, o.reselects, "the re-selection cap is 3")
	assert.Contains(t, err.Error(), "re-selection cap", "the park error names the cap")

	// The subtask never completes when the cap is exhausted.
	assert.NotContains(t, ops.recorded(), "CompleteTask:SUB-1")
}

// reviewFixRegistry builds a registry whose reviewer pool (rev/judge) drives the
// specialists and synthesis, and whose coder pool has two candidates (fix/alpha
// best-value, fix/beta the fallback) so the FIX coder can re-select after one is
// excluded. rev/judge has no coder prior, so it is never picked as a fix model;
// the fix candidates have no reviewer prior, so they never join the panel.
func reviewFixRegistry() *registry.Registry {
	catalog := llm.Catalog{
		{ID: "rev/judge", ContextLength: 200000, SupportedParameters: []string{"tools"}},
		{ID: "fix/alpha", ContextLength: 200000, SupportedParameters: []string{"tools"}, PromptPricePerTok: 1e-6, CompletionPricePerTok: 1e-6},
		{ID: "fix/beta", ContextLength: 200000, SupportedParameters: []string{"tools"}, PromptPricePerTok: 1e-6, CompletionPricePerTok: 1e-6},
		{ID: "capable/default", ContextLength: 200000, SupportedParameters: []string{"tools"}},
	}

	judge, alpha, beta := 0.92, 0.90, 0.80
	priors := registry.Priors{
		Models: map[string]registry.PriorEntry{
			"rev/judge": {Reviewer: &judge},
			"fix/alpha": {Coder: &alpha},
			"fix/beta":  {Coder: &beta},
		},
	}

	return registry.NewRegistryFromParts(catalog, priors, nil, nil, "capable/default")
}

// TestReviewRecoversFromIncapableFixCoder pins that the review fix run shares the
// same in-run recovery: round 1's verdict requests a fix, the first-selected fix
// coder is harness-incapable, so the orchestrator blacklists it, re-selects a
// different fix coder, re-runs the fix, and round 2 approves.
func TestReviewRecoversFromIncapableFixCoder(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{committed: true, lastCommitTarget: "abc123"}

	// rev/judge (specialists + synthesis) and fix/beta (the recovered fix coder)
	// are capable; fix/alpha (the first fix pick) is incapable. The capable-call
	// script (incapable attempts don't consume it): round 1's three specialists +
	// a not-approved verdict, the recovered fix coder's turn, then round 2's three
	// specialists + an approved verdict.
	client := &modelAwareLLM{
		incapable: map[string]bool{"fix/alpha": true},
		responses: []llm.Response{
			stopResp("Correctness: bug", 0.01),
			stopResp("Design: ok", 0.01),
			stopResp("Security: ok", 0.01),
			stopResp(`{"approved":false,"summary":"fix it","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`, 0.02),
			stopResp("coder: fixed the bug", 0.05),
			stopResp("Correctness: ok now", 0.01),
			stopResp("Design: ok", 0.01),
			stopResp("Security: ok", 0.01),
			stopResp(`{"approved":true,"summary":"clean now","fixes":[]}`, 0.02),
		},
	}

	d := reviewTestDeps(t, ops, git, client, reviewFixRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)

	require.NoError(t, runReview(context.Background(), o),
		"the review must recover via a second fix coder, not error out")

	// The incapable fix coder was blacklisted and excluded.
	assert.Contains(t, ops.recorded(), "BlacklistModel:CARD-1/fix/alpha",
		"the incapable fix coder must be reported to CM")
	assert.Equal(t, 1, o.reselects, "exactly one re-selection")
	assert.True(t, o.excluded["fix/alpha"])

	// The fixup landed and was pushed: the recovered fix coder actually ran.
	gitCalls := git.recorded()
	assert.GreaterOrEqual(t, indexOfPrefix(gitCalls, "CommitFixup:"), 0,
		"the recovered fix coder must commit a fixup; git=%v", gitCalls)

	// The fix coder was first attempted on the incapable model, then the recovery
	// re-ran it on a different model.
	assert.True(t, modelsUsed(client, "fix/alpha"), "the incapable fix coder must be attempted first")
	assert.True(t, modelsUsed(client, "fix/beta"), "the recovered fix coder fix/beta must run")
}

// modelsUsed reports whether the model-aware stub ran the given model at least
// once.
func modelsUsed(m *modelAwareLLM, model string) bool {
	return slices.Contains(m.recordedModels(), model)
}

// TestRecoverIncapableCapsAtThree is a focused unit test of the helper: the
// fourth call (reselects already 3) returns an error wrapping the IncapableError
// instead of incrementing further.
func TestRecoverIncapableCapsAtThree(t *testing.T) {
	ops := &fakeOps{}
	d := execTestDeps(ops, &fakeGit{}, &planLLM{})
	o := newRun(d, cmclient.TaskContext{})

	for i := 1; i <= 3; i++ {
		err := o.recoverIncapable(context.Background(), &IncapableError{Model: "m", Reason: "cannot drive the tool loop"})
		require.NoError(t, err, "recovery %d within the cap must succeed", i)
		assert.Equal(t, i, o.reselects)
	}

	// Fourth recovery is over the cap.
	err := o.recoverIncapable(context.Background(), &IncapableError{Model: "m", Reason: "cannot drive the tool loop"})
	require.Error(t, err)

	var ie *IncapableError
	require.ErrorAs(t, err, &ie)
	assert.Equal(t, 3, o.reselects, "the counter must not advance past the cap")
}
