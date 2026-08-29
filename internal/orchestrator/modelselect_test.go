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
	"github.com/mhersson/contextmatrix-harness/harness"
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

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Sizing: seedSizing("simple")}}, 0)

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

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Sizing: seedSizing("simple")}}, 0)

	require.NoError(t, runExecute(context.Background(), o))

	// The line is operator prose that nothing parses, so this pins the two
	// facts it must carry rather than its wording: rewording it is not a
	// behaviour change, deleting it is.
	var logged bool

	for _, l := range ops.logs {
		if strings.Contains(l, "coder/strong") && strings.Contains(l, "bar=simple") {
			logged = true
		}
	}

	assert.True(t, logged,
		"one card-log entry still names the selected model and its bar; logs=%v", ops.logs)
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

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Sizing: seedSizing("simple")}}, 0)
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

	o := newExecRun(d, []subtaskRef{{ID: "SUB-1", Title: "Only", Sizing: seedSizing("simple")}}, 0)
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

	_, err := o.runFixModel(context.Background(), "fix prompt", fixRequest{Round: 1, FixTier: "complex"})
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

	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, reviewerRegistry())
	d.Emit = events.NewEmitter(nil, &transcript)
	o := newReviewRun(d, cmclient.TaskContext{}, 0)

	pick, rep := resolveDecisionModel(context.Background(), d.Registry, d.Emit, d.Ops,
		"CARD-1", "", "payload/model", "default/model", nil)
	require.True(t, pick.OK)
	o.noteShortfall(context.Background(), "plan decision", "", pick, rep)

	sels := selectionsForPhase(modelSelections(t, &transcript), "plan decision")
	require.Len(t, sels, 1)
	assert.Equal(t, pick.Model, sels[0].Model, "the transcript names the model that actually ran")
	assert.Equal(t, "complex", sels[0].TierRequested, "decision phases are floored to complex")
	assert.Equal(t, "auto", sels[0].Source,
		"a floor pick that cleared the bar came off the ladder, not from a pin or the default")
}

// TestDecisionModelBelowBarReportsTheFallback proves the event names the model
// that ran, not the one the selector proposed: a floor pick that misses the bar
// is discarded and the operator's configured default runs instead.
func TestDecisionModelBelowBarReportsTheFallback(t *testing.T) {
	var transcript bytes.Buffer

	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, nil)
	d.Emit = events.NewEmitter(nil, &transcript)
	o := newReviewRun(d, cmclient.TaskContext{}, 0)

	// No priors anywhere, so no candidate clears the complex bar and base wins.
	reg := offCatalogDefaultRegistry("capable/default")

	pick, rep := resolveDecisionModel(context.Background(), reg, d.Emit, d.Ops,
		"CARD-1", "", "payload/model", "default/model", nil)
	require.Equal(t, "payload/model", pick.Model)
	o.noteShortfall(context.Background(), "review synthesis", "", pick, rep)

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

// TestReviewGateSelectionIsRecordedWhenTheGateModelRuns pairs with the promoted
// case below: a human turn reaches the classification model, so the phase
// records the one selection that ran.
func TestReviewGateSelectionIsRecordedWhenTheGateModelRuns(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "approve"}}}
	client := &planLLM{responses: []llm.Response{
		stopResp("No concerns.", 0.001),
		stopResp("No concerns.", 0.001),
		stopResp("No concerns.", 0.001),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.001),
		stopResp(`{"verdict":"approve","feedback":""}`, 0.001),
	}}

	d := hitlReviewDeps(ops, &fakeGit{}, inbox, client)
	d.Emit = events.NewEmitter(nil, &transcript)
	o := newRun(d, cmclient.TaskContext{Title: "T", Description: "b", State: "review"})
	isolateVerify(o)

	require.NoError(t, runReview(context.Background(), o))

	sels := selectionsForPhase(modelSelections(t, &transcript), "review gate")
	require.Len(t, sels, 1, "the gate model ran, so its selection is on the transcript")
	assert.NotEmpty(t, sels[0].Model)
}

// TestPromotedReviewGateRecordsNoSelection keeps the phase's event count
// meaningful. A card promoted mid-run closes the inbox, so the gate returns
// without ever classifying a reply and no model runs. An event here would
// count a model into the review-gate distribution that never saw a prompt.
func TestPromotedReviewGateRecordsNoSelection(t *testing.T) {
	var transcript bytes.Buffer

	ops := &fakeOps{}
	// Empty, non-blocking inbox: the first gate Wait reports ErrInboxClosed,
	// exactly what a promote frame produces.
	inbox := &fakeInbox{}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: bug", 0.001), stopResp("Design: ok", 0.001), stopResp("Security: ok", 0.001),
		stopResp(`{"approved":false,"summary":"fix it","fixes":[{"file":"a.go","issue":"bug","suggestion":"patch"}]}`, 0.001),
		stopResp("Fixed.", 0.001),
		stopResp("Correctness: ok now", 0.001), stopResp("Design: ok", 0.001), stopResp("Security: ok", 0.001),
		stopResp(`{"approved":true,"summary":"clean now","fixes":[]}`, 0.001),
	}}

	d := hitlReviewDeps(ops, &fakeGit{committed: true, lastCommitTarget: "abc123"}, inbox, client)
	d.Emit = events.NewEmitter(nil, &transcript)
	o := newRun(d, cmclient.TaskContext{Title: "T", Description: "b", State: "review"})
	isolateVerify(o)

	require.NoError(t, runReview(context.Background(), o))

	assert.Empty(t, selectionsForPhase(modelSelections(t, &transcript), "review gate"),
		"no gate classification ran, so no gate model was selected")
}

// TestVerifyProposeSelectionReachesTheTranscript covers one of the three
// selecting sites the brief did not name.
func TestVerifyProposeSelectionReachesTheTranscript(t *testing.T) {
	var transcript bytes.Buffer

	client := &planLLM{responses: []llm.Response{stopResp(`{"command":"cargo test"}`, 0.01)}}
	o := newProposeRun(t, &fakeOps{}, client, t.TempDir())
	o.d.Emit = events.NewEmitter(nil, &transcript)

	_, err := o.proposeVerify(context.Background())
	require.NoError(t, err)

	sels := selectionsForPhase(modelSelections(t, &transcript), "verify propose")
	require.Len(t, sels, 1)
	assert.Equal(t, "simple", sels[0].TierRequested, "the proposal is a cheap read-only step")
	assert.NotEmpty(t, sels[0].Model)
	assert.Empty(t, sels[0].Subtask)
}

// TestFanoutCandidateSelectionsReachTheTranscript covers the Best-of-N seat
// selection: one event per candidate, so the fan-out's model spread is
// countable from the transcript.
func TestFanoutCandidateSelectionsReachTheTranscript(t *testing.T) {
	var transcript bytes.Buffer

	d, _, _ := fanoutDeps(t, &fakeOps{}, &fakeGit{}, &planLLM{}, 3)
	d.Emit = events.NewEmitter(nil, &transcript)

	o := newFanoutRun(t, d, []subtaskRef{{ID: "SUB-1", Title: "First", Sizing: seedSizing("simple")}}, 0)

	require.NoError(t, o.runFanout(context.Background()))

	sels := modelSelections(t, &transcript)
	for i := 1; i <= 3; i++ {
		seat := selectionsForPhase(sels, candidatePhase(i))
		require.Len(t, seat, 1, "candidate %d records its seat selection", i)
		assert.Equal(t, "moderate", seat[0].TierRequested, "the fan-out seats on the card tier")
		assert.Empty(t, seat[0].Subtask, "the seat is chosen before any subtask runs")
	}
}

// TestCandidateRepickRecordsTheSubtaskItRepickedFor covers the third unnamed
// site. A candidate whose model proves harness-incapable re-picks mid-run, and
// the replacement selection names the subtask it will run: without it the
// transcript shows a second model for the candidate with no way to place it.
func TestCandidateRepickRecordsTheSubtaskItRepickedFor(t *testing.T) {
	var transcript bytes.Buffer

	client := &modelAwareLLM{incapable: map[string]bool{"solo/coder": true}}
	d, _, _ := fanoutDeps(t, &fakeOps{}, &fakeGit{}, client, 1)
	d.Registry = registry.NewRegistryFromParts(
		llm.Catalog{
			{ID: "solo/coder", ContextLength: 200000, SupportedParameters: []string{"tools"}, PromptPricePerTok: 1e-6},
			{ID: "capgood/default", ContextLength: 200000, SupportedParameters: []string{"tools"}},
		},
		registry.Priors{Models: map[string]registry.PriorEntry{"solo/coder": coderPrior(0.9)}},
		nil, nil, "capgood/default",
	)
	d.Emit = events.NewEmitter(nil, &transcript)

	o := newFanoutRun(t, d, []subtaskRef{{ID: "SUB-1", Title: "First", Sizing: seedSizing("moderate")}}, 0)

	require.NoError(t, o.runFanout(context.Background()))

	sels := selectionsForPhase(modelSelections(t, &transcript), candidatePhase(1))
	require.Len(t, sels, 2, "the seat selection, then the re-pick after the incapable model")

	assert.Equal(t, "solo/coder", sels[0].Model)
	assert.Empty(t, sels[0].Subtask, "the seat is chosen before any subtask runs")

	assert.Equal(t, "capgood/default", sels[1].Model, "the re-pick names the replacement")
	assert.Equal(t, "SUB-1", sels[1].Subtask, "and the subtask that replacement runs")
}

// TestWalkedDownExcludesOffLadderPicksByDesign states the rule in one place,
// over the whole source space, rather than leaving it inferable only from the
// two recording sites that consume it.
//
// The name is the point: pins and the capable default are not exceptions
// carved out of a below-bar test, they are picks the ladder never walked. Both
// report BelowBar, so a reader who assumes the two are the same thing will
// "simplify" this predicate into a silent stop on outcome recording for every
// pinned and every fallback run.
func TestWalkedDownExcludesOffLadderPicksByDesign(t *testing.T) {
	pick := func(src registry.PickSource, met registry.Tier) registry.Pick {
		return registry.Pick{
			Role:          registry.RoleCoder,
			RequestedTier: registry.TierComplex,
			MetTier:       met,
			Source:        src,
			OK:            true,
		}
	}

	tests := []struct {
		name string
		p    registry.Pick
		want bool
	}{
		{"a ladder pick that got its rung was not walked down", pick(registry.SourceAuto, registry.TierComplex), false},
		{"a ladder pick served a rung lower was walked down", pick(registry.SourceAuto, registry.TierSimple), true},
		{"a ladder pick that cleared nothing was walked down", pick(registry.SourceAuto, ""), true},
		{"a favorite at its rung was not walked down", pick(registry.SourceFavorite, registry.TierComplex), false},
		{"a favorite served a rung lower was walked down", pick(registry.SourceFavorite, registry.TierSimple), true},
		// The two off-ladder sources. Both report BelowBar; neither was
		// walked down, because the ladder is not what chose them.
		{"a pin is intent, so the selector never walked", pick(registry.SourcePinned, ""), false},
		{"the capable default sits in no rung to be walked down from", pick(registry.SourceDefault, ""), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, walkedDown(tt.p))
		})
	}

	// A refusal (OK false) never reaches a recording site - there is no model
	// to charge - but it must not read as a walk-down if one ever does.
	assert.False(t, walkedDown(registry.Pick{RequestedTier: registry.TierComplex, Source: registry.SourceAuto}))
}
