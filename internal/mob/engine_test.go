package mob

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const engineBearer = "engine-test-bearer"

// emitRecord is one captured EmitFn call.
type emitRecord struct {
	author  string
	lens    string
	model   string
	round   int
	content string
}

// emitLog collects EmitFn calls in order, thread-safe.
type emitLog struct {
	mu   sync.Mutex
	recs []emitRecord
}

func (l *emitLog) fn() EmitFn {
	return func(author, lens, model string, round int, content string) {
		l.mu.Lock()
		l.recs = append(l.recs, emitRecord{author: author, lens: lens, model: model, round: round, content: content})
		l.mu.Unlock()
	}
}

func (l *emitLog) snapshot() []emitRecord {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]emitRecord(nil), l.recs...)
}

// seatScript is a scripted SeatRunner recording prompts per seat name.
// reply gets the seat name and that seat's 0-based call index.
type seatScript struct {
	mu      sync.Mutex
	prompts map[string][]string
	reply   func(seat string, call int) (string, float64, error)
}

func (s *seatScript) run(_ context.Context, seat SeatConfig, _ []Turn, prompt string) (string, float64, error) {
	s.mu.Lock()

	if s.prompts == nil {
		s.prompts = map[string][]string{}
	}

	n := len(s.prompts[seat.Name])
	s.prompts[seat.Name] = append(s.prompts[seat.Name], prompt)
	s.mu.Unlock()

	return s.reply(seat.Name, n)
}

func (s *seatScript) promptsFor(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.prompts[name]...)
}

// modScript is a scripted ModeratorRunner: replies are consumed in order
// (the last one repeats); every call costs cost and reports running on model.
type modScript struct {
	mu      sync.Mutex
	prompts []string
	replies []string
	model   string
	cost    float64
}

func (m *modScript) run(_ context.Context, prompt string) (string, string, float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.prompts = append(m.prompts, prompt)

	i := len(m.prompts) - 1
	if i >= len(m.replies) {
		i = len(m.replies) - 1
	}

	if i < 0 {
		return "", "", m.cost, errors.New("modScript: no replies configured")
	}

	return m.replies[i], m.model, m.cost, nil
}

func (m *modScript) calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.prompts...)
}

// fakeInbox is a channel-backed harness.Inbox for interjection tests.
type fakeInbox struct{ ch chan harness.UserMessage }

func newFakeInbox(msgs ...string) *fakeInbox {
	f := &fakeInbox{ch: make(chan harness.UserMessage, len(msgs)+8)}

	for i, m := range msgs {
		f.ch <- harness.UserMessage{MessageID: fmt.Sprintf("m-%d", i), Content: m}
	}

	return f
}

func (f *fakeInbox) Drain() []harness.UserMessage {
	var out []harness.UserMessage

	for {
		select {
		case m := <-f.ch:
			out = append(out, m)
		default:
			return out
		}
	}
}

func (f *fakeInbox) Wait(ctx context.Context) (harness.UserMessage, error) {
	select {
	case m := <-f.ch:
		return m, nil
	case <-ctx.Done():
		return harness.UserMessage{}, ctx.Err()
	}
}

// startEngine spins a real loopback server for the seats and wires an engine
// to it, mirroring the orchestrator glue's construction.
func startEngine(t *testing.T, seats []SeatConfig, runner SeatRunner, moderate ModeratorRunner, mutate func(*EngineConfig)) (*Engine, *emitLog) {
	t.Helper()

	srv, err := StartServer(seats, runner, engineBearer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })

	log := &emitLog{}
	cfg := EngineConfig{
		Seats:            seats,
		Runner:           runner,
		Moderate:         moderate,
		Emit:             log.fn(),
		SeatEndpoint:     srv.SeatEndpoint,
		Bearer:           engineBearer,
		InternalDeadline: 30 * time.Second,
	}

	if mutate != nil {
		mutate(&cfg)
	}

	return NewEngine(cfg), log
}

func TestDiscussBlindRoundThenCritiqueDeltas(t *testing.T) {
	seats := []SeatConfig{
		{Name: "seat-1", Lens: "feasibility"},
		{Name: "seat-2", Lens: "risk"},
		{Name: "seat-3", Lens: "performance"},
	}
	script := &seatScript{reply: func(seat string, call int) (string, float64, error) {
		return fmt.Sprintf("position-%s-round-%d", seat, call), 0.01, nil
	}}
	mod := &modScript{replies: []string{"productive_disagreement", "SYNTH"}, cost: 0.002}

	eng, log := startEngine(t, seats, script.run, mod.run, nil)

	out, err := eng.Discuss(t.Context(), Topic{
		Kind:            "plan",
		Briefing:        "BRIEFING: solve X",
		Lenses:          []string{"feasibility", "risk", "performance"},
		Rounds:          1,
		Blind:           true,
		SynthesisPrompt: "Synthesize the plan.",
	})
	require.NoError(t, err)

	names := []string{"seat-1", "seat-2", "seat-3"}

	for _, name := range names {
		prompts := script.promptsFor(name)
		require.Len(t, prompts, 2, "seat %s: blind + one critique round", name)

		// Round 0 is blind: briefing only, zero peer text.
		assert.Equal(t, "BRIEFING: solve X", prompts[0])

		// Round 1: peers' round-0 positions, never the seat's own.
		for _, other := range names {
			frag := fmt.Sprintf("position-%s-round-0", other)
			if other == name {
				assert.NotContains(t, prompts[1], frag, "seat %s must not see its own text", name)
			} else {
				assert.Contains(t, prompts[1], frag)
			}
		}

		assert.Contains(t, prompts[1], "Critique round 1")
		assert.NotContains(t, prompts[1], "BRIEFING: solve X",
			"an established task gets the delta, not the snapshot")
	}

	assert.Equal(t, "SYNTH", out.Synthesis)
	assert.False(t, out.Consensus)

	// Transcript: briefing + 3 round-0 + 3 round-1 entries.
	require.Len(t, out.Transcript, 7)
	assert.Equal(t, Entry{Author: "moderator", Round: 0, Content: "BRIEFING: solve X"}, out.Transcript[0])

	for _, e := range out.Transcript[1:4] {
		assert.Equal(t, 0, e.Round)
	}

	for _, e := range out.Transcript[4:7] {
		assert.Equal(t, 1, e.Round)
	}

	// Costs: 6 seat turns x 0.01 + classify 0.002 + synthesis 0.002.
	assert.InDelta(t, 0.064, out.CostUSD, 1e-9)

	// Emit saw everything in order: briefing, 3x round 0, 3x round 1, synthesis.
	recs := log.snapshot()
	require.Len(t, recs, 8)
	assert.Equal(t, emitRecord{author: "moderator", lens: "", round: -1, content: "BRIEFING: solve X"}, recs[0])

	for i, name := range names {
		assert.Equal(t, name, recs[1+i].author)
		assert.Equal(t, 0, recs[1+i].round)
		assert.Equal(t, name, recs[4+i].author)
		assert.Equal(t, 1, recs[4+i].round)
	}

	assert.Equal(t, emitRecord{author: "moderator", lens: "", round: -1, content: "SYNTH"}, recs[7])

	// Synthesis prompt = caller instruction + full transcript.
	calls := mod.calls()
	require.Len(t, calls, 2)
	assert.True(t, strings.HasPrefix(calls[1], "Synthesize the plan."), calls[1])
	assert.Contains(t, calls[1], "position-seat-1-round-0")
	assert.Contains(t, calls[1], "position-seat-3-round-1")
}

// TestDiscussEmitsSeatAndModeratorModels pins the model attribution on live
// transcript events: each seat utterance carries that seat's own model, the
// moderator's synthesis carries the decision model it ran on, and the briefing
// (engine-generated framing, not a model output) carries no model.
func TestDiscussEmitsSeatAndModeratorModels(t *testing.T) {
	seats := []SeatConfig{
		{Name: "seat-1", Lens: "feasibility", Model: "vendor/model-a"},
		{Name: "seat-2", Lens: "risk", Model: "vendor/model-b"},
	}
	script := &seatScript{reply: func(seat string, call int) (string, float64, error) {
		return fmt.Sprintf("position-%s-%d", seat, call), 0.01, nil
	}}
	mod := &modScript{replies: []string{"productive_disagreement", "SYNTH"}, model: "vendor/decision-model"}

	eng, log := startEngine(t, seats, script.run, mod.run, nil)

	_, err := eng.Discuss(t.Context(), Topic{
		Kind:            "review",
		Briefing:        "BRIEF",
		Lenses:          []string{"feasibility", "risk"},
		Rounds:          1,
		Blind:           true,
		SynthesisPrompt: "Synthesize.",
	})
	require.NoError(t, err)

	var seat1, seat2, briefings, synths int

	for _, r := range log.snapshot() {
		switch {
		case r.author == "seat-1":
			seat1++

			assert.Equal(t, "vendor/model-a", r.model, "seat-1 utterance must carry its own model")
		case r.author == "seat-2":
			seat2++

			assert.Equal(t, "vendor/model-b", r.model, "seat-2 utterance must carry its own model")
		case r.content == "BRIEF":
			briefings++

			assert.Empty(t, r.model, "the briefing carries no model pill")
		case r.content == "SYNTH":
			synths++

			assert.Equal(t, "vendor/decision-model", r.model, "synthesis must carry the moderator's decision model")
		}
	}

	// Guard against a vacuous pass: every attributed kind must have been seen.
	assert.Positive(t, seat1, "expected seat-1 utterances")
	assert.Positive(t, seat2, "expected seat-2 utterances")
	assert.Equal(t, 1, briefings, "expected exactly one briefing")
	assert.Equal(t, 1, synths, "expected exactly one synthesis")
}

func TestDiscussConvergenceStops(t *testing.T) {
	tests := []struct {
		name          string
		verdict       string
		wantConsensus bool
	}{
		{name: "consensus stops early", verdict: "consensus", wantConsensus: true},
		{name: "stalled stops early", verdict: "stalled", wantConsensus: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seats := []SeatConfig{{Name: "seat-1", Lens: "a"}, {Name: "seat-2", Lens: "b"}}
			script := &seatScript{reply: func(seat string, call int) (string, float64, error) {
				return fmt.Sprintf("%s-%d", seat, call), 0, nil
			}}
			mod := &modScript{replies: []string{tt.verdict, "SYNTH"}}

			eng, _ := startEngine(t, seats, script.run, mod.run, nil)

			out, err := eng.Discuss(t.Context(), Topic{
				Briefing:        "brief",
				Rounds:          3,
				Blind:           true,
				SynthesisPrompt: "synthesize",
			})
			require.NoError(t, err)

			assert.Len(t, script.promptsFor("seat-1"), 2, "stop after round 1, not 3")
			assert.Len(t, script.promptsFor("seat-2"), 2)
			assert.Equal(t, tt.wantConsensus, out.Consensus)
			assert.Equal(t, "SYNTH", out.Synthesis)
		})
	}
}

func TestDiscussQuorumFailure(t *testing.T) {
	seats := []SeatConfig{
		{Name: "seat-1", Lens: "a"},
		{Name: "seat-2", Lens: "b"},
		{Name: "seat-3", Lens: "c"},
	}
	script := &seatScript{reply: func(seat string, _ int) (string, float64, error) {
		if seat != "seat-1" {
			return "", 0, errors.New("model down")
		}

		return "lonely position", 0.01, nil
	}}
	mod := &modScript{replies: []string{"SYNTH"}}

	eng, log := startEngine(t, seats, script.run, mod.run, nil)

	out, err := eng.Discuss(t.Context(), Topic{
		Briefing:        "brief",
		Rounds:          2,
		Blind:           true,
		SynthesisPrompt: "synthesize",
	})

	require.ErrorIs(t, err, ErrNoQuorum)
	assert.Empty(t, out.Synthesis)
	assert.InDelta(t, 0.01, out.CostUSD, 1e-9)

	// Transcript: briefing + the one responder.
	require.Len(t, out.Transcript, 2)
	assert.Equal(t, "seat-1", out.Transcript[1].Author)

	assert.Empty(t, mod.calls(), "no classify or synthesis on quorum failure")

	// Absences were surfaced.
	var absences int

	for _, r := range log.snapshot() {
		if r.author == "moderator" && r.round == 0 && strings.Contains(r.content, "absent") {
			absences++
		}
	}

	assert.Equal(t, 2, absences)
}

func TestDiscussBudgetExhaustionSynthesizesEarly(t *testing.T) {
	seats := []SeatConfig{{Name: "seat-1", Lens: "a"}, {Name: "seat-2", Lens: "b"}}
	script := &seatScript{reply: func(seat string, _ int) (string, float64, error) {
		return seat + " position", 0.05, nil
	}}
	mod := &modScript{replies: []string{"SYNTH-B"}, cost: 0.001}

	eng, log := startEngine(t, seats, script.run, mod.run, func(cfg *EngineConfig) {
		cfg.BudgetUSD = 0.05 // blind round spends 0.10 >= budget
	})

	out, err := eng.Discuss(t.Context(), Topic{
		Briefing:        "brief",
		Rounds:          3,
		Blind:           true,
		SynthesisPrompt: "synthesize",
	})
	require.NoError(t, err)

	assert.Len(t, script.promptsFor("seat-1"), 1, "no critique round after budget break")
	assert.Len(t, script.promptsFor("seat-2"), 1)

	require.Len(t, mod.calls(), 1, "synthesis only - no classify")
	assert.Equal(t, "SYNTH-B", out.Synthesis)
	assert.False(t, out.Consensus)
	assert.InDelta(t, 0.101, out.CostUSD, 1e-9)

	var sawBudgetNotice bool

	for _, r := range log.snapshot() {
		if r.author == "moderator" && strings.Contains(r.content, "budget") {
			sawBudgetNotice = true
		}
	}

	assert.True(t, sawBudgetNotice)
}

func TestDiscussInboxInterjections(t *testing.T) {
	seats := []SeatConfig{{Name: "seat-1", Lens: "a"}, {Name: "seat-2", Lens: "b"}}
	script := &seatScript{reply: func(seat string, call int) (string, float64, error) {
		return fmt.Sprintf("%s-%d", seat, call), 0, nil
	}}
	mod := &modScript{replies: []string{"productive_disagreement", "SYNTH"}}
	inbox := newFakeInbox("please consider Y", "   ")

	eng, log := startEngine(t, seats, script.run, mod.run, func(cfg *EngineConfig) {
		cfg.Inbox = inbox
	})

	out, err := eng.Discuss(t.Context(), Topic{
		Briefing:        "brief",
		Rounds:          1,
		Blind:           true,
		SynthesisPrompt: "synthesize",
	})
	require.NoError(t, err)

	// The interjection entered the round-1 delta for both seats.
	for _, name := range []string{"seat-1", "seat-2"} {
		prompts := script.promptsFor(name)
		require.Len(t, prompts, 2)
		assert.Contains(t, prompts[1], "[round 1] human: please consider Y")
	}

	// It is a transcript entry and was emitted; the blank one was dropped.
	var humanEntries []Entry

	for _, e := range out.Transcript {
		if e.Author == "human" {
			humanEntries = append(humanEntries, e)
		}
	}

	require.Len(t, humanEntries, 1)
	assert.Equal(t, Entry{Author: "human", Round: 1, Content: "please consider Y"}, humanEntries[0])

	var emittedHuman bool

	for _, r := range log.snapshot() {
		if r.author == "human" && r.content == "please consider Y" {
			emittedHuman = true
		}
	}

	assert.True(t, emittedHuman)
}

func TestDiscussGuestDialBoundedByDeadline(t *testing.T) {
	// A guest endpoint that accepts the connection but never answers must not
	// hang the discussion: open() bounds the dial by GuestDeadline, the guest
	// is surfaced as unreachable (name only - never the URL), and the internal
	// seats discuss through to synthesis.
	release := make(chan struct{})
	guestSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(guestSrv.Close)
	t.Cleanup(func() { close(release) })

	seats := []SeatConfig{{Name: "seat-1", Lens: "a"}, {Name: "seat-2", Lens: "b"}}
	script := &seatScript{reply: func(seat string, call int) (string, float64, error) {
		return fmt.Sprintf("%s-%d", seat, call), 0.01, nil
	}}
	mod := &modScript{replies: []string{"consensus", "SYNTH"}}

	eng, log := startEngine(t, seats, script.run, mod.run, func(cfg *EngineConfig) {
		cfg.Guests = []GuestSeat{{Name: "laptop", URL: guestSrv.URL}}
		cfg.GuestDeadline = 200 * time.Millisecond
	})

	done := make(chan struct{})

	var (
		out    Outcome
		runErr error
	)

	go func() {
		defer close(done)

		out, runErr = eng.Discuss(t.Context(), Topic{
			Briefing:        "brief",
			Rounds:          1,
			Blind:           true,
			SynthesisPrompt: "synthesize",
		})
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Discuss hung on an unresponsive guest dial")
	}

	require.NoError(t, runErr, "internal-seat discussion proceeds despite the dead guest")
	assert.Equal(t, "SYNTH", out.Synthesis)

	var sawUnreachable bool

	for _, r := range log.snapshot() {
		if r.author == "moderator" && strings.Contains(r.content, "laptop") &&
			strings.Contains(r.content, "unreachable") {
			sawUnreachable = true
		}

		assert.NotContains(t, r.content, guestSrv.URL, "guest URL must not leak into the transcript")
	}

	assert.True(t, sawUnreachable, "the unresponsive guest was surfaced as unreachable")
}

func TestDiscussNotBlindFirstRoundCarriesBriefing(t *testing.T) {
	seats := []SeatConfig{{Name: "seat-1", Lens: "a"}, {Name: "seat-2", Lens: "b"}}
	script := &seatScript{reply: func(seat string, call int) (string, float64, error) {
		return fmt.Sprintf("%s-%d", seat, call), 0, nil
	}}
	mod := &modScript{replies: []string{"consensus", "SYNTH"}}

	eng, _ := startEngine(t, seats, script.run, mod.run, nil)

	out, err := eng.Discuss(t.Context(), Topic{
		Briefing:        "CHECKPOINT BRIEFING",
		Rounds:          1,
		Blind:           false,
		SynthesisPrompt: "synthesize",
	})
	require.NoError(t, err)

	for _, name := range []string{"seat-1", "seat-2"} {
		prompts := script.promptsFor(name)
		require.Len(t, prompts, 1, "no blind round")
		assert.Contains(t, prompts[0], "[round 0] moderator: CHECKPOINT BRIEFING",
			"first critique round carries the briefing as a full snapshot")
	}

	assert.True(t, out.Consensus)
}

func TestClassifyRendersOnlyCurrentRound(t *testing.T) {
	var gotPrompt string

	e := NewEngine(EngineConfig{
		Moderate: func(_ context.Context, prompt string) (string, string, float64, error) {
			gotPrompt = prompt

			return "productive_disagreement", "", 0, nil
		},
	})

	entries := []Entry{
		{Author: "seat-a", Round: 1, Content: "EARLY-ROUND-ONE-MARKER"},
		{Author: "seat-b", Round: 1, Content: "EARLY-ROUND-ONE-MARKER-B"},
		{Author: "seat-a", Round: 2, Content: "LATEST-ROUND-TWO-MARKER"},
		{Author: "seat-b", Round: 2, Content: "LATEST-ROUND-TWO-MARKER-B"},
	}

	_, _, err := e.classify(context.Background(), entries, 2) // round 2 starts at index 2
	require.NoError(t, err)

	assert.Contains(t, gotPrompt, "LATEST-ROUND-TWO-MARKER")
	assert.NotContains(t, gotPrompt, "EARLY-ROUND-ONE-MARKER",
		"the convergence check must not re-send earlier rounds")
}

func TestDiscussAllEmptyBlindRoundFailsQuorum(t *testing.T) {
	seats := []SeatConfig{{Name: "seat-1", Lens: "a"}, {Name: "seat-2", Lens: "b"}}
	script := &seatScript{reply: func(string, int) (string, float64, error) {
		return "   ", 0.01, nil // budget-exhausted seats return blank text with nil error
	}}
	mod := &modScript{replies: []string{"SYNTH"}}

	eng, log := startEngine(t, seats, script.run, mod.run, nil)

	out, err := eng.Discuss(t.Context(), Topic{
		Briefing:        "brief",
		Rounds:          2,
		Blind:           true,
		SynthesisPrompt: "synthesize",
	})

	require.ErrorIs(t, err, ErrNoQuorum)
	require.Len(t, out.Transcript, 1, "briefing only - no empty entries")
	assert.Empty(t, mod.calls(), "no classify or synthesis after quorum failure")

	var notices int

	for _, r := range log.snapshot() {
		if r.author == "moderator" && strings.Contains(r.content, "produced no position") {
			notices++
		}

		if r.author == "seat-1" || r.author == "seat-2" {
			assert.NotEmpty(t, strings.TrimSpace(r.content), "no empty seat bubbles on the live stream")
		}
	}

	assert.Equal(t, 2, notices)
}

func TestDiscussEmptySeatBecomesAbsenceNotice(t *testing.T) {
	seats := []SeatConfig{
		{Name: "seat-1", Lens: "a"},
		{Name: "seat-2", Lens: "b"},
		{Name: "seat-3", Lens: "c"},
	}
	script := &seatScript{reply: func(seat string, _ int) (string, float64, error) {
		if seat == "seat-2" {
			return "", 0.01, nil // silent in every round
		}

		return seat + " position", 0.01, nil
	}}
	mod := &modScript{replies: []string{"consensus", "SYNTH"}}

	eng, log := startEngine(t, seats, script.run, mod.run, nil)

	out, err := eng.Discuss(t.Context(), Topic{
		Briefing:        "brief",
		Rounds:          1,
		Blind:           true,
		SynthesisPrompt: "synthesize",
	})

	require.NoError(t, err)
	assert.Equal(t, "SYNTH", out.Synthesis)

	for _, e := range out.Transcript {
		assert.NotEqual(t, "seat-2", e.Author, "silent seat must not enter the transcript")
	}

	var noticed bool

	for _, r := range log.snapshot() {
		if r.author == "moderator" && strings.Contains(r.content, "seat-2") &&
			strings.Contains(r.content, "produced no position") {
			noticed = true
		}
	}

	assert.True(t, noticed, "absence notice for seat-2 expected")
}
