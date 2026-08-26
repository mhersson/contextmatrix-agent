package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/mob"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modelSelection mirrors one model_selected event's data. The field names are
// the wire contract a transcript consumer parses, so the tags are the thing
// under test, not incidental.
type modelSelection struct {
	Phase         string `json:"phase"`
	Subtask       string `json:"subtask"`
	Model         string `json:"model"`
	Source        string `json:"source"`
	TierRequested string `json:"tier_requested"`
	MetTier       string `json:"met_tier"`
}

// modelSelections decodes every model_selected event from a JSON-lines
// transcript, in emission order.
func modelSelections(t *testing.T, transcript *bytes.Buffer) []modelSelection {
	t.Helper()

	var out []modelSelection

	for line := range strings.SplitSeq(strings.TrimSpace(transcript.String()), "\n") {
		if line == "" {
			continue
		}

		var ev struct {
			Kind string         `json:"kind"`
			Data modelSelection `json:"data"`
		}

		require.NoError(t, json.Unmarshal([]byte(line), &ev))

		if ev.Kind == modelSelectedKind {
			out = append(out, ev.Data)
		}
	}

	return out
}

// selectionsForPhase filters decoded selections down to one phase label.
func selectionsForPhase(sels []modelSelection, phase string) []modelSelection {
	var out []modelSelection

	for _, s := range sels {
		if s.Phase == phase {
			out = append(out, s)
		}
	}

	return out
}

// coderPriorRegistry has one catalogued coder model whose prior clears every
// bar up to complex, so a coder selection is a real ladder pick rather than
// the capable default.
func coderPriorRegistry() *registry.Registry {
	strong := 0.90
	priors := registry.Priors{
		Models: map[string]registry.PriorEntry{"coder/strong": {Coder: &strong, Reviewer: &strong}},
	}
	catalog := llm.Catalog{
		{ID: "coder/strong", ContextLength: 200000, SupportedParameters: []string{"tools"}, PromptPricePerTok: 1e-6, CompletionPricePerTok: 1e-6},
		{ID: "pinned/model", ContextLength: 200000, SupportedParameters: []string{"tools"}},
	}

	return registry.NewRegistryFromParts(catalog, priors, nil, nil, "default/model")
}

// offCatalogDefaultRegistry gives the operator a capable default the live
// catalog does not carry, and a catalogue with no priors, so every selection
// walks the whole ladder dry and lands on that default.
func offCatalogDefaultRegistry(capable string) *registry.Registry {
	return registry.NewRegistry(capable, llm.Catalog{
		{ID: "cat/model", ContextLength: 131072, SupportedParameters: []string{"tools"}},
	})
}

// TestCoderSelectionReachesTheTranscript is the core of this event: a subtask
// coder run must record, on the transcript alone, which model ran, for which
// subtask, at which tier, what that model was worth, and where it came from.
func TestCoderSelectionReachesTheTranscript(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{}
	git := &fakeGit{committed: true}

	d := execTestDeps(ops, git, &planLLM{})
	d.Registry = coderPriorRegistry()
	d.Emit = events.NewEmitter(nil, &transcript)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	require.NoError(t, runExecute(context.Background(), o))

	sels := selectionsForPhase(modelSelections(t, &transcript), "coder")
	require.Len(t, sels, 1, "one coder attempt records exactly one selection")

	assert.Equal(t, modelSelection{
		Phase:         "coder",
		Subtask:       "SUB-1",
		Model:         "coder/strong",
		Source:        "auto",
		TierRequested: "simple",
		MetTier:       "simple",
	}, sels[0])
}

// TestCoderSelectionKeepsTheCardLogEntry proves the transcript event is an
// addition: the activity-log line the operator reads on the board is still
// written, unchanged.
func TestCoderSelectionKeepsTheCardLogEntry(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{}
	git := &fakeGit{committed: true}

	d := execTestDeps(ops, git, &planLLM{})
	d.Registry = coderPriorRegistry()
	d.Emit = events.NewEmitter(nil, &transcript)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)

	require.NoError(t, runExecute(context.Background(), o))

	assert.Contains(t, strings.Join(ops.logs, "\n"),
		`coder model coder/strong selected for subtask "Only" (tier=simple)`)
	assert.NotEmpty(t, modelSelections(t, &transcript), "and the transcript event is there too")
}

// TestPinnedCoderReportsPinSource pins the provenance of an operator override:
// the selector never ran, and the transcript must say so.
func TestPinnedCoderReportsPinSource(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{}
	git := &fakeGit{committed: true}

	d := execTestDeps(ops, git, &planLLM{})
	d.Registry = coderPriorRegistry()
	d.Emit = events.NewEmitter(nil, &transcript)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)
	o.tc.ModelCoder = "pinned/model"

	require.NoError(t, runExecute(context.Background(), o))

	sels := selectionsForPhase(modelSelections(t, &transcript), "coder")
	require.Len(t, sels, 1)
	assert.Equal(t, "pinned/model", sels[0].Model)
	assert.Equal(t, "pinned", sels[0].Source)
	assert.Equal(t, "simple", sels[0].TierRequested)
	assert.Empty(t, sels[0].MetTier, "a synthesized pin carries no measured prior, so it clears no bar")
}

// TestFallbackEqualToThePinIsNotReportedAsAPin is the guard against deriving
// the source by comparing the chosen model to the pin string. The operator's
// capable default and an unresolvable pin are the same slug here - exactly the
// collision that made the recorded runs unreadable - and the truth is that the
// pin was rejected and the off-ladder default ran.
//
// The ladder variant of this collision is unconstructible: a slug the ladder
// can select is by definition in the catalog, which is what makes a pin
// resolvable, so a catalogued pin never reaches the selector at all.
func TestFallbackEqualToThePinIsNotReportedAsAPin(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{}
	git := &fakeGit{committed: true}

	d := execTestDeps(ops, git, &planLLM{})
	d.Registry = offCatalogDefaultRegistry("fallback/model")
	d.Emit = events.NewEmitter(nil, &transcript)

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Tier: "simple"}}, 0)
	o.tc.ModelCoder = "fallback/model"

	require.NoError(t, runExecute(context.Background(), o))

	sels := selectionsForPhase(modelSelections(t, &transcript), "coder")
	require.Len(t, sels, 1)
	assert.Equal(t, "fallback/model", sels[0].Model, "the model that ran is the same slug as the pin")
	assert.Equal(t, "capable-default", sels[0].Source,
		"the source is read off the pick, not inferred from the model name")
}

// TestReviewPanelSelectionsReachTheTranscript records one event per seat, so a
// panel that collapsed onto one model is visible from the transcript.
func TestReviewPanelSelectionsReachTheTranscript(t *testing.T) {
	var transcript bytes.Buffer

	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reviewerRegistry())
	d.Emit = events.NewEmitter(nil, &transcript)
	o := newReviewRun(d, cmclient.TaskContext{}, 0)

	require.Len(t, o.reviewPanel(context.Background(), 1000, false), reviewPanelSize)

	sels := selectionsForPhase(modelSelections(t, &transcript), "review panel")
	require.Len(t, sels, reviewPanelSize, "every seat records its own selection")

	for _, s := range sels {
		assert.Equal(t, "moderate", s.TierRequested, "the panel sizes on the card tier")
		assert.NotEmpty(t, s.Model)
		assert.Empty(t, s.Subtask, "a card-level phase has no subtask")
	}
}

// TestAuthoritativeReviewPanelRecordsItsEscalatedTier proves the requested tier
// on the event is the one actually asked of the selector, not the card's.
func TestAuthoritativeReviewPanelRecordsItsEscalatedTier(t *testing.T) {
	var transcript bytes.Buffer

	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reviewerRegistry())
	d.Emit = events.NewEmitter(nil, &transcript)
	o := newReviewRun(d, cmclient.TaskContext{}, 0)

	o.reviewPanel(context.Background(), 1000, true)

	sels := selectionsForPhase(modelSelections(t, &transcript), "review panel")
	require.NotEmpty(t, sels)

	for _, s := range sels {
		assert.Equal(t, "complex", s.TierRequested)
	}
}

// TestPinnedReviewPanelReachesTheTranscript covers the panel's pin path, which
// returns three synthesized seats without consulting the selector.
func TestPinnedReviewPanelReachesTheTranscript(t *testing.T) {
	var transcript bytes.Buffer

	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reviewerRegistry())
	d.Emit = events.NewEmitter(nil, &transcript)
	o := newReviewRun(d, cmclient.TaskContext{}, 0)
	o.tc.ModelReviewer = "pinned/model"

	o.reviewPanel(context.Background(), 1000, false)

	sels := selectionsForPhase(modelSelections(t, &transcript), "review panel")
	require.Len(t, sels, reviewPanelSize)

	for _, s := range sels {
		assert.Equal(t, "pinned/model", s.Model)
		assert.Equal(t, "pinned", s.Source)
	}
}

// TestFixCoderSelectionReachesTheTranscript covers the fix round, whose tier is
// the synthesizer's rather than the card's.
func TestFixCoderSelectionReachesTheTranscript(t *testing.T) {
	var transcript bytes.Buffer

	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{committed: true}, &planLLM{}, reviewerRegistry())
	d.Emit = events.NewEmitter(nil, &transcript)
	o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", State: "review"}, 0)

	_, err := o.runFixModel(context.Background(), "fix prompt", 1, "complex", false)
	require.NoError(t, err)

	sels := selectionsForPhase(modelSelections(t, &transcript), "fix coder")
	require.Len(t, sels, 1)
	assert.Equal(t, "complex", sels[0].TierRequested)
	assert.NotEmpty(t, sels[0].Model)
}

// TestDecisionModelSelectionReachesTheTranscript covers the orchestrator's
// decision phases, which floor to a fixed tier regardless of the card.
func TestDecisionModelSelectionReachesTheTranscript(t *testing.T) {
	var transcript bytes.Buffer

	emit := events.NewEmitter(nil, &transcript)
	ops := &fakeOps{}

	model := resolveDecisionModel(context.Background(), reviewerRegistry(), emit, ops,
		"CARD-1", "", "payload/model", "default/model", nil, "plan decision")

	sels := selectionsForPhase(modelSelections(t, &transcript), "plan decision")
	require.Len(t, sels, 1)
	assert.Equal(t, model, sels[0].Model, "the transcript names the model that actually ran")
	assert.Equal(t, "complex", sels[0].TierRequested, "decision phases are floored to complex")
}

// TestDecisionModelBelowBarReportsTheFallback proves the event names the model
// that ran, not the one the selector proposed: a floor pick that misses the bar
// is discarded and the operator's configured default runs instead.
func TestDecisionModelBelowBarReportsTheFallback(t *testing.T) {
	var transcript bytes.Buffer

	emit := events.NewEmitter(nil, &transcript)
	ops := &fakeOps{}

	// No priors anywhere, so no candidate clears the complex bar and base wins.
	reg := offCatalogDefaultRegistry("capable/default")

	model := resolveDecisionModel(context.Background(), reg, emit, ops,
		"CARD-1", "", "payload/model", "default/model", nil, "review synthesis")
	require.Equal(t, "payload/model", model)

	sels := selectionsForPhase(modelSelections(t, &transcript), "review synthesis")
	require.Len(t, sels, 1)
	assert.Equal(t, "payload/model", sels[0].Model)
	assert.Equal(t, "capable-default", sels[0].Source, "an off-ladder configured model ran")
}

// TestDiscussionPanelSelectionsReachTheTranscript covers the mob session seats.
func TestDiscussionPanelSelectionsReachTheTranscript(t *testing.T) {
	var transcript bytes.Buffer

	o := mobTestRun(&fakeOps{}, MobConfig{Participants: 3, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 2.0)
	o.d.Emit = events.NewEmitter(nil, &transcript)

	_, ok := buildEngineConfig(context.Background(), o,
		mob.Topic{Kind: "plan", Lenses: planLenses[:3], Rounds: 2, Blind: true}, "b")
	require.True(t, ok)

	sels := selectionsForPhase(modelSelections(t, &transcript), "mob plan")
	require.Len(t, sels, 3, "every discussion seat records its own selection")

	for _, s := range sels {
		assert.Equal(t, "complex", s.TierRequested)
	}
}

// TestJudgeSelectionReachesTheTranscript covers the Best-of-N judge, the pick
// that decides which branch ships.
func TestJudgeSelectionReachesTheTranscript(t *testing.T) {
	var transcript bytes.Buffer

	client := &planLLM{responses: []llm.Response{
		stopResp(`{"winner": 1, "ranking": [1, 2], "rationale": "c1 wins.", "notes": []}`, 0.02),
	}}
	cands := []*candidate{
		judgeCandidate(1, "alpha/coder", "dir-c1", "DIFF_ONE"),
		judgeCandidate(2, "beta/coder", "dir-c2", "DIFF_TWO"),
	}
	o := newJudgeRun(t, &fakeOps{}, &fakeGit{}, client, cands, map[string]bool{"dir-c1": true, "dir-c2": true})
	o.d.Registry = twoCoderRegistry()
	o.d.Emit = events.NewEmitter(nil, &transcript)

	require.NoError(t, runJudge(context.Background(), o))

	sels := selectionsForPhase(modelSelections(t, &transcript), "best-of-n judge")
	require.Len(t, sels, 1)
	assert.Equal(t, o.judgeModel, sels[0].Model)
	assert.Equal(t, "complex", sels[0].TierRequested)
}
