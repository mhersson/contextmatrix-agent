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
	"github.com/mhersson/contextmatrix-agent/internal/coop"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedEngine stubs run.coopEngine: it records every EngineConfig+Topic it
// received and pops queued (Outcome, error) results in order. Shared by the
// plan and review co-op tests.
type scriptedEngine struct {
	mu       sync.Mutex
	cfgs     []coop.EngineConfig
	topics   []coop.Topic
	outcomes []coop.Outcome
	errs     []error
	i        int
}

func (s *scriptedEngine) run(_ context.Context, cfg coop.EngineConfig, t coop.Topic) (coop.Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cfgs = append(s.cfgs, cfg)
	s.topics = append(s.topics, t)

	if s.i >= len(s.outcomes) {
		return coop.Outcome{}, errors.New("scriptedEngine: no outcome queued")
	}

	out := s.outcomes[s.i]

	var err error
	if s.i < len(s.errs) {
		err = s.errs[s.i]
	}

	s.i++

	return out, err
}

// coopTestRun builds a run with the reviewer-qualifying registry (seat
// selection needs reviewer priors) and the given co-op config.
func coopTestRun(ops *fakeOps, coopCfg CoopConfig, maxCost float64) *run {
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
			MaxTurns: 20, MaxCardCost: maxCost, Coop: coopCfg,
		},
	}

	return newRun(d, cmclient.TaskContext{CardID: "CARD-1", Title: "Parent", Description: "body"})
}

func TestCoopConfigEnabled(t *testing.T) {
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
		assert.Equal(t, tt.want, CoopConfig{Participants: tt.participants}.enabled(),
			"participants=%d", tt.participants)
	}
}

func TestBuildEngineConfigPlanTopic(t *testing.T) {
	ops := &fakeOps{}
	o := coopTestRun(ops, CoopConfig{
		Participants: 3, Plan: true, Rounds: 2, BudgetFactor: 0.75,
		Guests: []CoopGuest{{Name: "laptop", URL: "http://10.0.0.5:8484", Token: "tok"}},
	}, 2.0)

	topic := coop.Topic{Kind: "plan", Lenses: planLenses[:3], Rounds: 2, Blind: true}

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
	assert.Equal(t, coop.GuestSeat{Name: "laptop", URL: "http://10.0.0.5:8484", Token: "tok"}, cfg.Guests[0])

	assert.InDelta(t, 1.5, cfg.BudgetUSD, 1e-9, "budget = factor x MaxCardCost")
	assert.Equal(t, "test-bearer", cfg.Bearer)
	assert.Nil(t, cfg.SeatEndpoint, "no server yet")
	assert.NotNil(t, cfg.Runner)
	assert.NotNil(t, cfg.Moderate)
	assert.NotNil(t, cfg.Emit)
}

func TestBuildEngineConfigReviewExcludesCoderModels(t *testing.T) {
	ops := &fakeOps{}
	o := coopTestRun(ops, CoopConfig{Participants: 3, Review: true, Rounds: 2, BudgetFactor: 0.75}, 2.0)
	o.coderModels = map[string]bool{"rev/alpha": true}

	topic := coop.Topic{Kind: "review", Lenses: reviewLenses[:3], Rounds: 1, Blind: true}

	cfg := buildEngineConfig(o, topic, "b")

	require.Len(t, cfg.Seats, 3)

	for _, s := range cfg.Seats {
		assert.NotEqual(t, "rev/alpha", s.Model, "review seats must exclude the models that coded the card")
	}
}

func TestBuildEngineConfigZeroCostDisablesBudget(t *testing.T) {
	ops := &fakeOps{}
	o := coopTestRun(ops, CoopConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 0)

	cfg := buildEngineConfig(o, coop.Topic{Kind: "plan", Lenses: planLenses[:2], Rounds: 2}, "b")

	assert.Zero(t, cfg.BudgetUSD, "MaxCardCost 0 disables the co-op budget term")
}

func TestCoopDiscussQuorumFallback(t *testing.T) {
	ops := &fakeOps{}
	o := coopTestRun(ops, CoopConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 2.0)

	eng := &scriptedEngine{
		outcomes: []coop.Outcome{{}},
		errs:     []error{coop.ErrNoQuorum},
	}
	o.coopEngine = eng.run

	out, ok := o.coopDiscuss(context.Background(), coop.Topic{
		Kind: "plan", Lenses: planLenses[:2], Rounds: 2, Blind: true, Briefing: "b",
	})

	assert.False(t, ok, "quorum failure must degrade, never error")
	assert.Empty(t, out.Synthesis)
	require.Len(t, eng.cfgs, 1)
	assert.NotNil(t, eng.cfgs[0].SeatEndpoint, "the real server endpoint is wired before Discuss")

	joined := strings.Join(ops.logs, "\n")
	assert.Contains(t, joined, "continuing solo", "the fallback is recorded on the card")
}

func TestCoopDiscussBudgetExhaustedRunsSolo(t *testing.T) {
	ops := &fakeOps{}
	o := coopTestRun(ops, CoopConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 2.0)

	// Pre-spend the run ledger up to the whole co-op envelope: no headroom left.
	o.ledger.Spend(effectiveCeiling(o.d.Cfg))

	eng := &scriptedEngine{outcomes: []coop.Outcome{{Synthesis: "SHOULD-NOT-RUN"}}}
	o.coopEngine = eng.run

	out, ok := o.coopDiscuss(context.Background(), coop.Topic{
		Kind: "plan", Lenses: planLenses[:2], Rounds: 2, Blind: true, Briefing: "b",
	})

	assert.False(t, ok, "an exhausted co-op budget must degrade to solo")
	assert.Empty(t, out.Synthesis)
	assert.Empty(t, eng.cfgs, "the engine must not be invoked once the budget is exhausted")

	joined := strings.Join(ops.logs, "\n")
	assert.Contains(t, joined, "continuing solo")
	assert.Contains(t, joined, "exhausted")
}

func TestCoopDiscussClampsBudgetToHeadroom(t *testing.T) {
	ops := &fakeOps{}
	o := coopTestRun(ops, CoopConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 2.0)

	// effectiveCeiling = MaxCardCost + BudgetFactor*MaxCardCost = 2.0 + 1.5 = 3.5.
	// Pre-spend 2.5 → headroom 1.0, below the full co-op term (1.5): the engine
	// gets the clamped 1.0.
	o.ledger.Spend(2.5)

	eng := &scriptedEngine{outcomes: []coop.Outcome{{Synthesis: "SYNTH"}}}
	o.coopEngine = eng.run

	out, ok := o.coopDiscuss(context.Background(), coop.Topic{
		Kind: "plan", Lenses: planLenses[:2], Rounds: 2, Blind: true, Briefing: "b",
	})

	require.True(t, ok)
	assert.Equal(t, "SYNTH", out.Synthesis)
	require.Len(t, eng.cfgs, 1)
	assert.InDelta(t, 1.0, eng.cfgs[0].BudgetUSD, 1e-9,
		"budget clamped to remaining co-op headroom, not the full term")
}

func TestCoopDiscussUnlimitedCeilingKeepsUnbounded(t *testing.T) {
	ops := &fakeOps{}
	o := coopTestRun(ops, CoopConfig{Participants: 2, Plan: true, Rounds: 2, BudgetFactor: 0.75}, 0)

	// Even with prior spend, MaxCardCost 0 means an unlimited ceiling: the co-op
	// term stays unbounded (0) and the discussion is never treated as exhausted.
	o.ledger.Spend(10.0)

	eng := &scriptedEngine{outcomes: []coop.Outcome{{Synthesis: "SYNTH"}}}
	o.coopEngine = eng.run

	out, ok := o.coopDiscuss(context.Background(), coop.Topic{
		Kind: "plan", Lenses: planLenses[:2], Rounds: 2, Blind: true, Briefing: "b",
	})

	require.True(t, ok)
	assert.Equal(t, "SYNTH", out.Synthesis)
	require.Len(t, eng.cfgs, 1)
	assert.Zero(t, eng.cfgs[0].BudgetUSD, "unlimited ceiling keeps the co-op budget unbounded")
}

func TestSeatConfigCapsToolOutput(t *testing.T) {
	base := harness.Config{ToolOutputMaxBytes: 131072, MaxTurns: 45}
	cfg := seatConfig(base, coop.SeatConfig{Name: "seat-1", Lens: "risk"}, 0.10, nil)

	assert.Equal(t, coopSeatToolOutputMaxBytes, cfg.ToolOutputMaxBytes)
	assert.Equal(t, coopSeatMaxTurns, cfg.MaxTurns)
	assert.InDelta(t, 0.10, cfg.MaxCostUSD, 1e-9)
}

func TestSeatConfigSetsWrapUpNudge(t *testing.T) {
	cfg := seatConfig(harness.Config{}, coop.SeatConfig{Name: "seat-1", Lens: "risk"}, 0.25, nil)

	assert.Equal(t, coopSeatWrapUpTurns, cfg.WrapUpTurns)
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

func TestCoopModeratorRunnerIsToolless(t *testing.T) {
	client := &planLLM{responses: []llm.Response{stopResp("VERDICT", 0.01)}}
	o := coopTestRun(&fakeOps{}, CoopConfig{Participants: 2}, 10)
	o.d.Client = client

	runner := o.coopModeratorRunner(&seatDebugSink{w: io.Discard})

	out, cost, err := runner(t.Context(), "synthesize this")
	require.NoError(t, err)
	assert.Equal(t, "VERDICT", out)
	assert.InDelta(t, 0.01, cost, 1e-9)

	for _, n := range client.toolCountsSeen() {
		assert.Zero(t, n, "moderator calls must offer no tools")
	}
}
