package orchestrator

import (
	"context"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHasDesignSection(t *testing.T) {
	assert.True(t, hasDesignSection("intro\n\n## Design\n\nstuff"))
	assert.False(t, hasDesignSection("intro\n\n## Plan\n\nstuff"))
	assert.False(t, hasDesignSection(""))
}

func TestExtractDesign(t *testing.T) {
	t.Run("no marker", func(t *testing.T) {
		_, done := extractDesign("Let's discuss. What palette sizes do you need?")
		assert.False(t, done)
	})

	t.Run("marker with design heading", func(t *testing.T) {
		out := "Great.\n\n## Design\n\nA palette config with N slots.\n\nDESIGN_COMPLETE"
		design, done := extractDesign(out)
		assert.True(t, done)
		assert.True(t, hasDesignSection(design), "the captured text is the design section")
		assert.NotContains(t, design, "DESIGN_COMPLETE", "the marker line is stripped")
		assert.Contains(t, design, "A palette config")
	})

	t.Run("marker without heading keeps the body", func(t *testing.T) {
		design, done := extractDesign("Final design: just add a flag.\nDESIGN_COMPLETE")
		assert.True(t, done)
		assert.Contains(t, design, "just add a flag")
		assert.NotContains(t, design, "DESIGN_COMPLETE")
	})
}

// brainstormRun builds a *run wired for brainstorm tests: scripted ops, a planLLM
// serving the model turns, and an injected inbox serving the human turns.
func brainstormRun(ops *fakeOps, inbox *fakeInbox, client llm.LLM, maxCost, reported float64) *run {
	d := Deps{
		Ops:       ops,
		Client:    client,
		Emit:      events.NewEmitter(nil, nil),
		Registry:  planTestRegistry(),
		ReadTools: tools.NewRegistry(tools.NewReadTool(".")),
		Human:     inbox,
		Cfg: Config{
			Project: "proj", CardID: "CARD-1",
			PayloadModel: "payload/model", DefaultModel: "default/model",
			MaxTurns: 5, MaxCardCost: maxCost, Interactive: true,
		},
	}

	return newRun(d, cmclient.TaskContext{CardID: "CARD-1", Title: "Add a palette", Description: "body", ReportedCostUSD: reported})
}

func TestBrainstormRecordsDesignOnMarker(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "approach A, please"}}}
	client := &planLLM{responses: []llm.Response{
		stopResp("Which approach: A or B?", 0.01),
		stopResp("## Design\n\nApproach A: a palette config.\n\nDESIGN_COMPLETE", 0.01),
	}}
	o := brainstormRun(ops, inbox, client, 0, 0)

	design, err := o.runBrainstorm(context.Background(), "payload/model")
	require.NoError(t, err)
	assert.True(t, hasDesignSection(o.body), "## Design recorded on the card body; body=%q", o.body)
	assert.Contains(t, ops.lastBody(), "Approach A")
	assert.Contains(t, design, "Approach A", "returned design carries the recorded design text")
}

func TestBrainstormPromoteEndsWithoutDesign(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{} // no messages, not blocking -> ErrInboxClosed after turn 1 (promote)
	client := &planLLM{responses: []llm.Response{stopResp("What sizes do you need?", 0.01)}}
	o := brainstormRun(ops, inbox, client, 0, 0)

	design, err := o.runBrainstorm(context.Background(), "payload/model")
	require.NoError(t, err)
	assert.Empty(t, design, "no design returned on a mid-dialogue promote")
	assert.False(t, hasDesignSection(o.body), "no design recorded on a mid-dialogue promote")
	assert.True(t, ops.loggedContains("promoted"), "promote logged; logs=%v", ops.logs)
}

func TestBrainstormEndSessionParks(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{block: true}
	client := &planLLM{responses: []llm.Response{stopResp("What sizes do you need?", 0.01)}}
	o := brainstormRun(ops, inbox, client, 0, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := o.runBrainstorm(ctx, "payload/model")
	require.Error(t, err, "end_session surfaces as an error")
}

func TestBrainstormBudgetParks(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{}
	o := brainstormRun(ops, inbox, &planLLM{}, 1.0, 2.0) // already over budget

	_, err := o.runBrainstorm(context.Background(), "payload/model")

	var be *BudgetExceededError
	require.ErrorAs(t, err, &be, "an over-budget brainstorm parks before any model call")
}
