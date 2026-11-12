package orchestrator

import (
	"context"
	"errors"
	"fmt"
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

// fixedGateModel hands the gate an explicitly supplied test-literal model
// slug, standing in for whatever gateModel would resolve at runtime. Test
// suites for other model-bearing phases (runBrainstorm, runDiagnose) also use
// it as a static gateModel value. It resolves nothing and emits nothing on
// its own.
func fixedGateModel(model string) gateModel {
	return func(context.Context) string { return model }
}

// gateRun builds a *run wired for gate tests: scripted ops, a planLLM client for
// the classification call, an injected inbox, the interactive flag, and a
// transcript writer so awaiting-human emission is observable.
func gateRun(ops *fakeOps, inbox *fakeInbox, interactive bool, client llm.LLM, transcript io.Writer, maxCost, reported float64) *run {
	d := Deps{
		Ops:       ops,
		Client:    client,
		Executor:  harnessExec{},
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

	return newRun(context.Background(), d, cmclient.TaskContext{Title: "T", Description: "body", ReportedCostUSD: reported})
}

func TestGateAutonomousPassesThrough(t *testing.T) {
	ops := &fakeOps{}
	// Human is nil and Interactive is false: gate must not touch the inbox.
	o := gateRun(ops, nil, false, &planLLM{}, nil, 0, 0)

	outcome, fb, err := o.gate(context.Background(), gatePlanApproval, fixedGateModel("payload/model"), "plan?")
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

	outcome, fb, err := o.gate(context.Background(), gatePlanApproval, fixedGateModel("payload/model"), "plan?")
	require.NoError(t, err)
	assert.Equal(t, gateApprove, outcome)
	assert.Empty(t, fb)
}

func TestGateHITLAdjustCarriesFeedback(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "split subtask 2"}}}
	client := &planLLM{responses: []llm.Response{stopResp(`{"verdict":"adjust","feedback":"split subtask 2 into two"}`, 0.001)}}
	o := gateRun(ops, inbox, true, client, nil, 0, 0)

	outcome, fb, err := o.gate(context.Background(), gatePlanApproval, fixedGateModel("payload/model"), "plan?")
	require.NoError(t, err)
	assert.Equal(t, gateAdjust, outcome)
	assert.Equal(t, "split subtask 2 into two", fb)
}

func TestGateHITLPromoteReturnsPromoted(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{} // no messages, not blocking -> Wait returns ErrInboxClosed (promote)
	o := gateRun(ops, inbox, true, &planLLM{}, nil, 0, 0)

	outcome, _, err := o.gate(context.Background(), gatePlanApproval, fixedGateModel("payload/model"), "plan?")
	require.NoError(t, err)
	assert.Equal(t, gatePromoted, outcome, "promotion is passthrough, not approval")
	assert.True(t, ops.loggedContains("promoted"), "promote logged; logs=%v", ops.logs)
}

func TestGateHITLEndSessionReturnsError(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{block: true}
	o := gateRun(ops, inbox, true, &planLLM{}, nil, 0, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // end_session: the parked Wait returns ctx.Err immediately

	_, _, err := o.gate(ctx, gatePlanApproval, fixedGateModel("payload/model"), "plan?")
	require.Error(t, err, "end_session surfaces as an error the worker maps to a park")
}

func TestGateClassificationParseFailureIsAdjust(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "hmm, not sure"}}}
	client := &planLLM{responses: []llm.Response{stopResp("not json", 0.001)}}
	o := gateRun(ops, inbox, true, client, nil, 0, 0)

	outcome, fb, err := o.gate(context.Background(), gatePlanApproval, fixedGateModel("payload/model"), "plan?")
	require.NoError(t, err)
	assert.Equal(t, gateAdjust, outcome, "an unparseable verdict is never an approval")
	assert.Equal(t, "hmm, not sure", fb, "feedback falls back to the raw reply")
}

func TestGateClassificationModelErrorIsAdjust(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "approve"}}}
	// The scripted reply would approve; the failing call must keep it from counting.
	client := &planLLM{responses: []llm.Response{stopResp(`{"verdict":"approve"}`, 0.001)}, errAfter: 1}
	o := gateRun(ops, inbox, true, client, nil, 0, 0)

	outcome, fb, err := o.gate(context.Background(), gatePlanApproval, fixedGateModel("payload/model"), "plan?")
	require.NoError(t, err)
	assert.Equal(t, gateAdjust, outcome, "a classification error is never an approval")
	assert.Equal(t, "approve", fb, "feedback falls back to the raw reply")
}

func TestGateClassificationLimitErrorParks(t *testing.T) {
	for name, limit := range map[string]error{
		"usage limit":   &UsageLimitError{Window: "five_hour"},
		"wrapped limit": fmt.Errorf("model: %w", &UsageLimitError{Window: "seven_day"}),
		"budget breach": &BudgetExceededError{Spent: 2, Max: 1},
	} {
		t.Run(name, func(t *testing.T) {
			ops := &fakeOps{}
			inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "approve"}}}
			client := &planLLM{errAfter: 1, errWith: limit}
			o := gateRun(ops, inbox, true, client, nil, 0, 0)

			_, _, err := o.gate(context.Background(), gatePlanApproval, fixedGateModel("payload/model"), "plan?")
			require.Error(t, err, "a limit stopping the classification must surface, not become adjust")
			assert.True(t, isBudgetOrUsageLimit(err), "err=%v", err)
		})
	}
}

// gateClaudeRun is a gate run on the Claude Code executor with a 1.00 ceiling
// and 0.20 already spent, so a classification run is capped at 0.80.
func gateClaudeRun(ops *fakeOps, rec *recordingExec) *run {
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "looks good"}}}
	o := gateRun(ops, inbox, true, &planLLM{}, nil, 1.0, 0.2)
	o.d.Executor = rec
	o.d.Cfg.ClaudeCode = true

	return o
}

func TestGateClassificationStoppedAtTheBudgetCapParks(t *testing.T) {
	ops := &fakeOps{}
	rec := &recordingExec{results: []execResult{{res: harness.Result{Reason: "max_cost", TotalCostUSD: 0.8}}}}
	o := gateClaudeRun(ops, rec)

	outcome, _, err := o.gate(t.Context(), gateReviewDecision, fixedGateModel("payload/model"), "findings?")

	_, ok := errors.AsType[*BudgetExceededError](err)
	require.True(t, ok, "err=%v", err)
	assert.NotEqual(t, gateAdjust, outcome, "a truncated classification never becomes an adjust")
}

func TestGateClassificationDonePastTheCapIsApplied(t *testing.T) {
	ops := &fakeOps{}
	rec := &recordingExec{results: []execResult{{res: harness.Result{
		Completed: true, Reason: "done", Output: `{"verdict":"approve"}`, TotalCostUSD: 0.9,
	}}}}
	o := gateClaudeRun(ops, rec)

	outcome, _, err := o.gate(t.Context(), gateReviewDecision, fixedGateModel("payload/model"), "findings?")
	require.NoError(t, err)
	assert.Equal(t, gateApprove, outcome, "an answer that crossed the cap is acted on as finished")
	assert.InDelta(t, 1.1, o.ledger.Spent(), 1e-9, "the overspend is recorded")

	_, err = o.execRun(t.Context(), o.ledger, RunRequest{Task: "next"})
	_, ok := errors.AsType[*BudgetExceededError](err)
	assert.True(t, ok, "the next run refuses on the exhausted ledger")
	assert.Len(t, rec.runs, 1)
}

func TestGateEmitsAwaitingHuman(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "approve"}}}
	client := &planLLM{responses: []llm.Response{stopResp(`{"verdict":"approve"}`, 0.001)}}

	var transcript syncBuf

	o := gateRun(ops, inbox, true, client, &transcript, 0, 0)

	_, _, err := o.gate(context.Background(), gateReviewDecision, fixedGateModel("payload/model"), "findings?")
	require.NoError(t, err)
	assert.Contains(t, transcript.String(), `"state":"awaiting_human"`)
}

func TestGateBudgetParkDuringClassification(t *testing.T) {
	ops := &fakeOps{}
	inbox := &fakeInbox{msgs: []harness.UserMessage{{Content: "approve"}}}
	// Already over budget: the classification ledger.Check parks before the model call.
	o := gateRun(ops, inbox, true, &planLLM{}, nil, 1.0, 2.0)

	_, _, err := o.gate(context.Background(), gatePlanApproval, fixedGateModel("payload/model"), "plan?")

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

// TestParseGateVerdictBareJSONWithInStringFence: a bare, valid gate verdict
// whose feedback embeds a fenced snippet must not be mangled into ("", "") -
// that silently downgrades an approve to adjust.
func TestParseGateVerdictBareJSONWithInStringFence(t *testing.T) {
	raw := "{\"verdict\":\"Approve\",\"feedback\":\"keep the ```go\\nfunc x() {}\\n``` example\"}"

	verdict, feedback := parseGateVerdict(raw)
	assert.Equal(t, "approve", verdict)
	assert.Contains(t, feedback, "```go\nfunc x() {}\n```")
}
