package orchestrator

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gateRun builds a *run wired for gate tests: scripted ops, a planLLM client for
// the classification call, an injected inbox, the interactive flag, and a
// transcript writer so awaiting-human emission is observable.
func gateRun(ops *fakeOps, inbox *fakeInbox, interactive bool, client llm.LLM, transcript io.Writer, maxCost, reported float64) *run {
	d := Deps{
		Ops:       ops,
		Client:    client,
		Emit:      events.NewEmitter(nil, transcript),
		Registry:  planTestRegistry(),
		ReadTools: tools.NewRegistry(tools.NewReadTool(".")),
		Human:     inbox,
		Cfg: Config{
			Project: "proj", CardID: "CARD-1",
			PayloadModel: "payload/model", DefaultModel: "default/model",
			MaxTurns: 5, MaxCardCost: maxCost, Interactive: interactive,
		},
	}

	return newRun(d, cmclient.TaskContext{CardID: "CARD-1", Title: "T", Description: "body", ReportedCostUSD: reported})
}

func TestGateAutonomousPassesThrough(t *testing.T) {
	ops := &fakeOps{}
	// Human is nil and Interactive is false: gate must not touch the inbox.
	o := gateRun(ops, nil, false, &planLLM{}, nil, 0, 0)

	outcome, fb, err := o.gate(context.Background(), gatePlanApproval, "payload/model", "plan?")
	require.NoError(t, err)
	assert.Equal(t, gateApprove, outcome)
	assert.Empty(t, fb)
	assert.Empty(t, ops.recorded(), "autonomous gate makes no calls")
}

func TestGateHITLApprove(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "looks good, ship it"}}}
	client := &planLLM{responses: []llm.Response{stopResp(`{"verdict":"approve","feedback":""}`, 0.001)}}
	o := gateRun(ops, inbox, true, client, nil, 0, 0)

	outcome, fb, err := o.gate(context.Background(), gatePlanApproval, "payload/model", "plan?")
	require.NoError(t, err)
	assert.Equal(t, gateApprove, outcome)
	assert.Empty(t, fb)
}

func TestGateHITLAdjustCarriesFeedback(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "split subtask 2"}}}
	client := &planLLM{responses: []llm.Response{stopResp(`{"verdict":"adjust","feedback":"split subtask 2 into two"}`, 0.001)}}
	o := gateRun(ops, inbox, true, client, nil, 0, 0)

	outcome, fb, err := o.gate(context.Background(), gatePlanApproval, "payload/model", "plan?")
	require.NoError(t, err)
	assert.Equal(t, gateAdjust, outcome)
	assert.Equal(t, "split subtask 2 into two", fb)
}

func TestGateHITLPromoteApprovesAndLogs(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{} // no messages, not blocking -> Wait returns ErrInboxClosed (promote)
	o := gateRun(ops, inbox, true, &planLLM{}, nil, 0, 0)

	outcome, _, err := o.gate(context.Background(), gatePlanApproval, "payload/model", "plan?")
	require.NoError(t, err)
	assert.Equal(t, gateApprove, outcome, "a promoted card passes the gate")
	assert.True(t, ops.loggedContains("promoted"), "promote logged; logs=%v", ops.logs)
}

func TestGateHITLEndSessionReturnsError(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{block: true}
	o := gateRun(ops, inbox, true, &planLLM{}, nil, 0, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // end_session: the parked Wait returns ctx.Err immediately

	_, _, err := o.gate(ctx, gatePlanApproval, "payload/model", "plan?")
	require.Error(t, err, "end_session surfaces as an error the worker maps to a park")
}

func TestGateClassificationParseFailureIsAdjust(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "hmm, not sure"}}}
	client := &planLLM{responses: []llm.Response{stopResp("not json", 0.001)}}
	o := gateRun(ops, inbox, true, client, nil, 0, 0)

	outcome, fb, err := o.gate(context.Background(), gatePlanApproval, "payload/model", "plan?")
	require.NoError(t, err)
	assert.Equal(t, gateAdjust, outcome, "an unparseable verdict is never an approval")
	assert.Equal(t, "hmm, not sure", fb, "feedback falls back to the raw reply")
}

func TestGateEmitsAwaitingHuman(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "approve"}}}
	client := &planLLM{responses: []llm.Response{stopResp(`{"verdict":"approve"}`, 0.001)}}

	var transcript syncBuf

	o := gateRun(ops, inbox, true, client, &transcript, 0, 0)

	_, _, err := o.gate(context.Background(), gateReviewDecision, "payload/model", "findings?")
	require.NoError(t, err)
	assert.Contains(t, transcript.String(), `"state":"awaiting_human"`)
}

func TestGateBudgetParkDuringClassification(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "approve"}}}
	// Already over budget: the classification ledger.Check parks before the model call.
	o := gateRun(ops, inbox, true, &planLLM{}, nil, 1.0, 2.0)

	_, _, err := o.gate(context.Background(), gatePlanApproval, "payload/model", "plan?")

	var be *BudgetExceededError
	require.ErrorAs(t, err, &be, "a budget breach in classification parks the run")
}

// syncBuf is a mutex-guarded buffer the emitter can write to while the test reads.
type syncBuf struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buf = append(b.buf, p...)

	return len(p), nil
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return string(b.buf)
}
