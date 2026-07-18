package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/mob"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedEngine stubs run.mobEngine: it records every EngineConfig+Topic it
// received and pops queued (Outcome, error) results in order. Shared by the
// plan and review mob session tests.
type scriptedEngine struct {
	mu       sync.Mutex
	cfgs     []mob.EngineConfig
	topics   []mob.Topic
	outcomes []mob.Outcome
	errs     []error
	i        int
}

func (s *scriptedEngine) run(_ context.Context, cfg mob.EngineConfig, t mob.Topic) (mob.Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cfgs = append(s.cfgs, cfg)
	s.topics = append(s.topics, t)

	if s.i >= len(s.outcomes) {
		return mob.Outcome{}, errors.New("scriptedEngine: no outcome queued")
	}

	out := s.outcomes[s.i]

	var err error
	if s.i < len(s.errs) {
		err = s.errs[s.i]
	}

	s.i++

	return out, err
}

// mobTestRun builds a run with the reviewer-qualifying registry (seat
// selection needs reviewer priors) and the given mob session config.
func mobTestRun(ops *fakeOps, mobCfg MobConfig, maxCost float64) *run {
	d := Deps{
		Ops:       ops,
		Git:       &fakeGit{},
		Client:    &planLLM{},
		Emit:      events.NewEmitter(nil, nil),
		Registry:  reviewerRegistry(),
		ReadTools: tools.NewRegistry(tools.NewReadTool(".")),
		Cfg: Config{
			Project: "proj", CardID: "CARD-1",
			PayloadModel: "payload/model", DefaultModel: "default/model",
			MaxTurns: 20, MaxCardCost: maxCost, Mob: mobCfg,
		},
	}

	return newRun(d, cmclient.TaskContext{Title: "Parent", Description: "body"})
}

func TestMobConfigEnabled(t *testing.T) {
	tests := []struct {
		participants int
		want         bool
	}{
		{0, false},
		{1, false},
		{2, true},
		{5, true},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, MobConfig{Participants: tt.participants}.enabled(),
			"participants=%d", tt.participants)
	}
}

func TestBuildEngineConfigPlanTopic(t *testing.T) {
	ops := &fakeOps{}
	o := mobTestRun(ops, MobConfig{
		Participants: 3, Plan: true, Rounds: 2, BudgetFactor: 0.75,
		Guests: []MobGuest{{Name: "laptop", URL: "http://10.0.0.5:8484", Token: "tok"}},
	}, 2.0)

	topic := mob.Topic{Kind: "plan", Lenses: planLenses[:3], Rounds: 2, Blind: true}

	cfg := buildEngineConfig(o, topic, "test-bearer")

	require.Len(t, cfg.Seats, 3)

	seen := map[string]bool{}

	for i, s := range cfg.Seats {
		assert.Equal(t, "seat-"+string(rune('1'+i)), s.Name)
		assert.Equal(t, planLenses[i], s.Lens)
		assert.NotEmpty(t, s.Model)
		assert.False(t, seen[s.Model], "seat models must be distinct while the pool lasts")
		seen[s.Model] = true
	}

	require.Len(t, cfg.Guests, 1)
	assert.Equal(t, mob.GuestSeat{Name: "laptop", URL: "http://10.0.0.5:8484", Token: "tok"}, cfg.Guests[0])

	assert.InDelta(t, 1.5, cfg.BudgetUSD, 1e-9, "budget = factor x MaxCardCost")
	assert.Equal(t, "test-bearer", cfg.Bearer)
	assert.Nil(t, cfg.SeatEndpoint, "no server yet")
	assert.NotNil(t, cfg.Runner)
	assert.NotNil(t, cfg.Moderate)
	assert.NotNil(t, cfg.Emit)
}

func TestBuildEngineConfigReviewExcludesCoderModels(t *testing.T) {
	ops := &fakeOps{}
	o := mobTestRun(ops, MobConfig{Participants: 3, Review: true, Rounds: 2, BudgetFactor: 0.75}, 2.0)
	o.coderModels = map[string]bool{"rev/alpha": true}

	topic := mob.Topic{Kind: "review", Lenses: reviewLenses[:3], Rounds: 1, Blind: true}

	cfg := buildEngineConfig(o, topic, "b")

	require.Len(t, cfg.Seats, 3)

	for _, s := range cfg.Seats {
		assert.NotEqual(t, "rev/alpha", s.Model, "review seats must exclude the models that coded the card")
	}
}

func TestBuildEngineConfigZeroCostDisablesBudget(t *testing.T) {
	ops := &fakeOps{}
	o := mobTestRun(ops, MobConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 0)

	cfg := buildEngineConfig(o, mob.Topic{Kind: "plan", Lenses: planLenses[:2], Rounds: 2}, "b")

	assert.Zero(t, cfg.BudgetUSD, "MaxCardCost 0 disables the mob session budget term")
}

func TestMobDiscussQuorumFallback(t *testing.T) {
	ops := &fakeOps{}
	o := mobTestRun(ops, MobConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 2.0)

	eng := &scriptedEngine{
		outcomes: []mob.Outcome{{}},
		errs:     []error{mob.ErrNoQuorum},
	}
	o.mobEngine = eng.run

	out, ok := o.mobDiscuss(context.Background(), mob.Topic{
		Kind: "plan", Lenses: planLenses[:2], Rounds: 2, Blind: true, Briefing: "b",
	})

	assert.False(t, ok, "quorum failure must degrade, never error")
	assert.Empty(t, out.Synthesis)
	require.Len(t, eng.cfgs, 1)
	assert.NotNil(t, eng.cfgs[0].SeatEndpoint, "the real server endpoint is wired before Discuss")

	joined := strings.Join(ops.logs, "\n")
	assert.Contains(t, joined, "continuing solo", "the fallback is recorded on the card")
}

func TestMobDiscussBudgetExhaustedRunsSolo(t *testing.T) {
	ops := &fakeOps{}
	o := mobTestRun(ops, MobConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 2.0)

	// Pre-spend the run ledger up to the whole mob session envelope: no headroom left.
	o.ledger.Spend(effectiveCeiling(o.d.Cfg))

	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: "SHOULD-NOT-RUN"}}}
	o.mobEngine = eng.run

	out, ok := o.mobDiscuss(context.Background(), mob.Topic{
		Kind: "plan", Lenses: planLenses[:2], Rounds: 2, Blind: true, Briefing: "b",
	})

	assert.False(t, ok, "an exhausted mob session budget must degrade to solo")
	assert.Empty(t, out.Synthesis)
	assert.Empty(t, eng.cfgs, "the engine must not be invoked once the budget is exhausted")

	joined := strings.Join(ops.logs, "\n")
	assert.Contains(t, joined, "continuing solo")
	assert.Contains(t, joined, "exhausted")
}

func TestMobDiscussClampsBudgetToHeadroom(t *testing.T) {
	ops := &fakeOps{}
	o := mobTestRun(ops, MobConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 2.0)

	// effectiveCeiling = MaxCardCost + BudgetFactor*MaxCardCost = 2.0 + 1.5 = 3.5.
	// Pre-spend 2.5 → headroom 1.0, below the full mob session term (1.5): the
	// engine gets the clamped 1.0.
	o.ledger.Spend(2.5)

	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: "SYNTH"}}}
	o.mobEngine = eng.run

	out, ok := o.mobDiscuss(context.Background(), mob.Topic{
		Kind: "plan", Lenses: planLenses[:2], Rounds: 2, Blind: true, Briefing: "b",
	})

	require.True(t, ok)
	assert.Equal(t, "SYNTH", out.Synthesis)
	require.Len(t, eng.cfgs, 1)
	assert.InDelta(t, 1.0, eng.cfgs[0].BudgetUSD, 1e-9,
		"budget clamped to remaining mob session headroom, not the full term")
}

func TestMobDiscussUnlimitedCeilingKeepsUnbounded(t *testing.T) {
	ops := &fakeOps{}
	o := mobTestRun(ops, MobConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 0)

	// Even with prior spend, MaxCardCost 0 means an unlimited ceiling: the mob
	// session term stays unbounded (0) and the discussion is never treated as
	// exhausted.
	o.ledger.Spend(10.0)

	eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: "SYNTH"}}}
	o.mobEngine = eng.run

	out, ok := o.mobDiscuss(context.Background(), mob.Topic{
		Kind: "plan", Lenses: planLenses[:2], Rounds: 2, Blind: true, Briefing: "b",
	})

	require.True(t, ok)
	assert.Equal(t, "SYNTH", out.Synthesis)
	require.Len(t, eng.cfgs, 1)
	assert.Zero(t, eng.cfgs[0].BudgetUSD, "unlimited ceiling keeps the mob session budget unbounded")
}

func TestSeatConfigCapsToolOutput(t *testing.T) {
	base := harness.Config{ToolOutputMaxBytes: 131072, MaxTurns: 45}
	cfg := seatConfig(base, mob.SeatConfig{Name: "seat-1", Lens: "risk"}, 0.10, nil)

	assert.Equal(t, mobSeatToolOutputMaxBytes, cfg.ToolOutputMaxBytes)
	assert.Equal(t, mobSeatMaxTurns, cfg.MaxTurns)
	assert.InDelta(t, 0.10, cfg.MaxCostUSD, 1e-9)
}

func TestSeatConfigSetsWrapUpNudge(t *testing.T) {
	cfg := seatConfig(harness.Config{}, mob.SeatConfig{Name: "seat-1", Lens: "risk"}, 0.25, nil)

	assert.Equal(t, mobSeatWrapUpTurns, cfg.WrapUpTurns)
	assert.Equal(t, seatWrapUpMessage, cfg.WrapUpMessage)
}

func TestSeatDebugWriterRewritesKinds(t *testing.T) {
	var buf bytes.Buffer

	sink := &seatDebugSink{w: &buf}
	w := sink.named("seat-2")

	_, err := w.Write([]byte(`{"kind":"tool_call","data":{"name":"read"}}` + "\n"))
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	assert.Equal(t, "seat_debug", m["kind"])
	assert.Equal(t, "tool_call", m["seat_kind"])
	assert.Equal(t, "seat-2", m["seat"])
}

func TestSeatDebugWriterPassesNonJSONThrough(t *testing.T) {
	var buf bytes.Buffer

	w := (&seatDebugSink{w: &buf}).named("moderator")

	_, err := w.Write([]byte("plain log line\n"))
	require.NoError(t, err)
	assert.Equal(t, "plain log line\n", buf.String())
}

func TestMobModeratorRunnerIsToolless(t *testing.T) {
	client := &planLLM{responses: []llm.Response{stopResp("VERDICT", 0.01)}}
	o := mobTestRun(&fakeOps{}, MobConfig{Participants: 2}, 10)
	o.d.Client = client

	runner := o.mobModeratorRunner(&seatDebugSink{w: io.Discard}, "mob_moderator")

	out, model, cost, err := runner(t.Context(), "synthesize this")
	require.NoError(t, err)
	assert.Equal(t, "VERDICT", out)
	assert.NotEmpty(t, model, "runner reports the resolved decision model it ran on")
	assert.InDelta(t, 0.01, cost, 1e-9)

	for _, n := range client.toolCountsSeen() {
		assert.Zero(t, n, "moderator calls must offer no tools")
	}
}

func TestMobSeatRunnerPersistsContextAcrossRounds(t *testing.T) {
	client := &planLLM{responses: []llm.Response{
		stopResp("position A", 0.01),
		stopResp("critique B", 0.01),
	}}
	o := mobTestRun(&fakeOps{}, MobConfig{Participants: 2}, 10)
	o.d.Client = client

	runner := o.mobSeatRunner(&seatDebugSink{w: io.Discard}, 0, "mob_seat")
	seat := mob.SeatConfig{Name: "seat-1", Lens: "risk", Model: "m/1"}

	out1, _, err := runner(t.Context(), seat, nil, "round 0 briefing")
	require.NoError(t, err)
	assert.Equal(t, "position A", out1)

	out2, _, err := runner(t.Context(), seat, nil, "round 1 delta")
	require.NoError(t, err)
	assert.Equal(t, "critique B", out2)

	// The second call must carry the first round's full exchange and
	// exactly one system message (seatConfig re-adds it each run).
	msgs := client.messagesOf(1)
	require.NotNil(t, msgs)

	var sys int

	var sawBriefing, sawPosition bool

	for _, m := range msgs {
		if m.Role == "system" {
			sys++
		}

		if strings.Contains(m.Content, "round 0 briefing") {
			sawBriefing = true
		}

		if m.Role == "assistant" && strings.Contains(m.Content, "position A") {
			sawPosition = true
		}
	}

	assert.Equal(t, 1, sys)
	assert.True(t, sawBriefing)
	assert.True(t, sawPosition)
}

func TestMobSeatRunnerForcesFinalAnswerOnEmptyOutput(t *testing.T) {
	// The first turn tool-calls at a cost above the per-turn cap, so the
	// run stops max_cost with empty output (deterministic - the cap is
	// checked at the top of the next turn). The backstop call must then
	// produce the position, toolless, despite the exhausted cap.
	burn := llm.Response{
		FinishReason: "tool_calls",
		Usage:        llm.Usage{Cost: 0.30},
		ToolCalls: []llm.ToolCall{{
			ID: "c1", Type: "function",
			Function: llm.FunctionCall{Name: "nope", Arguments: "{}"},
		}},
	}
	client := &planLLM{responses: []llm.Response{burn, stopResp("forced position", 0.01)}}
	o := mobTestRun(&fakeOps{}, MobConfig{Participants: 2}, 10)
	o.d.Client = client

	runner := o.mobSeatRunner(&seatDebugSink{w: io.Discard}, 0.25, "mob_seat")

	out, _, err := runner(t.Context(), mob.SeatConfig{Name: "seat-1", Lens: "risk", Model: "m/1"}, nil, "briefing")
	require.NoError(t, err)
	assert.Equal(t, "forced position", out)

	counts := client.toolCountsSeen()
	require.NotEmpty(t, counts)
	assert.Zero(t, counts[len(counts)-1], "backstop call must offer no tools")
}

// TestMobSeatRunnerBackstopFailureFallsBackToAbsence drives the ferr != nil
// branch of the empty-output backstop: same primary shape as
// TestMobSeatRunnerForcesFinalAnswerOnEmptyOutput (stops max_cost with
// empty output), but this time the backstop call itself errors (e.g. a
// transport failure). The runner must still degrade to an absent position
// rather than failing the whole discussion.
func TestMobSeatRunnerBackstopFailureFallsBackToAbsence(t *testing.T) {
	burn := llm.Response{
		FinishReason: "tool_calls",
		Usage:        llm.Usage{Cost: 0.30},
		ToolCalls: []llm.ToolCall{{
			ID: "c1", Type: "function",
			Function: llm.FunctionCall{Name: "nope", Arguments: "{}"},
		}},
	}
	// Only the primary turn is scripted; errAfter: 2 makes the second call
	// (the backstop) return errPlanLLM instead of a response.
	client := &planLLM{responses: []llm.Response{burn}, errAfter: 2}
	o := mobTestRun(&fakeOps{}, MobConfig{Participants: 2}, 10)
	o.d.Client = client

	runner := o.mobSeatRunner(&seatDebugSink{w: io.Discard}, 0.25, "mob_seat")

	out, cost, err := runner(t.Context(), mob.SeatConfig{Name: "seat-1", Lens: "risk", Model: "m/1"}, nil, "briefing")
	require.NoError(t, err, "a failed backstop must degrade to absence, not fail the discussion")
	assert.Empty(t, out)

	// The harness only adds Usage.Cost on a successful response (see
	// harness.Run), so an erroring call can never carry cost through
	// fres.TotalCostUSD - the returned discussion cost is exactly the
	// primary run's billed spend either way. Fix 1's unconditional
	// `res.TotalCostUSD += fres.TotalCostUSD` is validated by inspection
	// and the other cost-carrying paths; this test's essential assertions
	// are the graceful degrade above (out=="", err==nil).
	assert.InDelta(t, 0.30, cost, 1e-9)

	require.Len(t, client.toolCountsSeen(), 2, "primary turn plus one backstop attempt")
}

func TestTrimSeatContext(t *testing.T) {
	big := strings.Repeat("x", seatContextMaxBytes)
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "briefing"},
		{Role: "tool", Content: big},
		{Role: "tool", Content: "small"},
		{Role: "assistant", Content: "position"},
	}

	got := trimSeatContext(msgs)

	require.Len(t, got, 4, "system message dropped")
	assert.Equal(t, "briefing", got[0].Content)
	assert.Equal(t, trimmedToolMarker, got[1].Content, "oldest oversized tool output blanked")
	assert.Equal(t, "small", got[2].Content, "later tool output kept once under the cap")
	assert.Equal(t, "position", got[3].Content)
}

func TestTrimSeatContextUnderCapUntouched(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "briefing"},
		{Role: "tool", Content: "output"},
		{Role: "assistant", Content: "position"},
	}

	got := trimSeatContext(msgs)

	require.Len(t, got, 3)
	assert.Equal(t, "output", got[1].Content)
}
