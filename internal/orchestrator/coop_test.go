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

func TestSeatDebugWriterRewritesKinds(t *testing.T) {
	var buf bytes.Buffer

	w := &seatDebugWriter{w: &buf}

	emit := events.NewEmitter(io.Discard, w)
	emit.Emit(events.ToolCallKind, map[string]any{"name": "read"})

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Equal(t, "seat_debug", got["kind"], "bridged kinds must be rewritten off the live stream")
	assert.Equal(t, "tool_call", got["seat_kind"], "the original kind is preserved for debugging")

	data, isMap := got["data"].(map[string]any)
	require.True(t, isMap)
	assert.Equal(t, "read", data["name"], "event payload passes through untouched")

	// Non-JSON lines pass through verbatim (defensive; the emitter only
	// writes JSON lines).
	buf.Reset()

	n, err := w.Write([]byte("not json\n"))
	require.NoError(t, err)
	assert.Equal(t, len("not json\n"), n)
	assert.Equal(t, "not json\n", buf.String())
}
