package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// progressSentinel stands in for a secret a model could echo mid-stream. The
// marker must never carry it: fragments are raw and unredacted.
const progressSentinel = "sk-live-PROGRESS-SENTINEL-4242"

// stepClock is a manually advanced clock for the progress throttle.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func newStepClock() *stepClock {
	return &stepClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = c.t.Add(d)
}

// lockedBuffer is a transcript writer a test can snapshot while a model run
// is still writing to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// progressLines returns the transcript lines whose kind is the progress marker.
func progressLines(t *testing.T, transcript string) []map[string]any {
	t.Helper()

	var out []map[string]any

	for line := range strings.SplitSeq(strings.TrimSpace(transcript), "\n") {
		if line == "" {
			continue
		}

		var ev map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &ev), line)

		if ev["kind"] == modelProgressKind {
			out = append(out, ev)
		}
	}

	return out
}

func TestProgressMarkerThrottles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		gap   time.Duration
		count int
		want  int
	}{
		{name: "fragments inside one interval emit nothing", gap: time.Second, count: 29, want: 0},
		{name: "a long turn emits once per interval", gap: time.Second, count: 95, want: 3},
		{name: "fragments an interval apart each emit", gap: modelProgressInterval, count: 4, want: 4},
		{name: "fragments further apart than the interval each emit", gap: 2 * modelProgressInterval, count: 3, want: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out lockedBuffer

			clock := newStepClock()
			marker := newProgressMarker(events.NewEmitter(nil, &out), clock.now)

			for range tt.count {
				clock.advance(tt.gap)
				marker.onDelta(llm.Delta{Reasoning: "thinking"})
			}

			assert.Len(t, progressLines(t, out.String()), tt.want)
		})
	}
}

func TestProgressMarkerCarriesNoModelText(t *testing.T) {
	t.Parallel()

	var out lockedBuffer

	clock := newStepClock()
	marker := newProgressMarker(events.NewEmitter(nil, &out), clock.now)

	clock.advance(modelProgressInterval)
	marker.onDelta(llm.Delta{Reasoning: progressSentinel, Content: progressSentinel})

	transcript := out.String()
	assert.NotContains(t, transcript, progressSentinel)

	lines := progressLines(t, transcript)
	require.Len(t, lines, 1)
	assert.NotContains(t, lines[0], "data", "the marker is content-free: it carries no data at all")
}

// reasoningOnlyLLM streams reasoning fragments for one long turn and answers
// with a short final message and no tool calls. Mid-stream it snapshots the
// transcript, so a test can tell what the stream carried before the turn ended.
type reasoningOnlyLLM struct {
	clock     *stepClock
	gap       time.Duration
	fragments int
	out       *lockedBuffer
	midTurn   string
}

func (r *reasoningOnlyLLM) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	return r.SendStream(ctx, req, nil)
}

func (r *reasoningOnlyLLM) SendStream(_ context.Context, _ llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	for range r.fragments {
		r.clock.advance(r.gap)

		if onDelta != nil {
			onDelta(llm.Delta{Reasoning: progressSentinel})
		}
	}

	r.midTurn = r.out.String()

	return llm.Response{Content: "done", FinishReason: "stop"}, nil
}

func TestRunModelSignalsProgressDuringReasoningTurn(t *testing.T) {
	t.Parallel()

	var out lockedBuffer

	client := &reasoningOnlyLLM{gap: time.Second, fragments: 95, out: &out}

	ops := &fakeOps{taskContext: cmclient.TaskContext{}}
	d := planTestDeps(ops, client)
	d.Emit = events.NewEmitter(nil, &out)

	o := newRun(d, ops.taskContext)
	require.NotNil(t, o.progress, "newRun builds the marker from Deps.Emit")

	// Only the clock is swapped, so the run keeps the marker newRun built. It
	// starts at the real now because that marker seeded its throttle from it.
	clock := &stepClock{t: time.Now()}
	o.progress.now = clock.now
	client.clock = clock

	_, _, err := o.runModel(context.Background(), d.ReadTools, "think hard", "default/model")
	require.NoError(t, err)

	assert.Len(t, progressLines(t, client.midTurn), 3,
		"the watchdog-facing stream must advance while the reasoning turn is still running")
	assert.NotContains(t, out.String(), progressSentinel, "no raw fragment text reaches the transcript")
}

// fragmentLLM streams sentinel-carrying reasoning and content fragments on
// every streamed call, advancing clock between them. It is safe for the
// review panel's concurrent specialists.
type fragmentLLM struct {
	clock     *stepClock
	gap       time.Duration
	fragments int
}

func (f *fragmentLLM) Send(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{Content: "sent", FinishReason: "stop"}, nil
}

func (f *fragmentLLM) SendStream(_ context.Context, _ llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	for range f.fragments {
		f.clock.advance(f.gap)

		if onDelta != nil {
			onDelta(llm.Delta{Reasoning: progressSentinel})
			onDelta(llm.Delta{Content: progressSentinel})
		}
	}

	return llm.Response{Content: "no findings", FinishReason: "stop"}, nil
}

func TestReviewSpecialistsSignalProgress(t *testing.T) {
	var out lockedBuffer

	client := &fragmentLLM{gap: time.Second, fragments: 40}
	d := reviewTestDeps(t, &fakeOps{}, &fakeGit{}, client, reviewerRegistry())
	d.Emit = events.NewEmitter(nil, &out)

	o := newReviewRun(d, cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}, 0)
	require.NotNil(t, o.progress)

	clock := &stepClock{t: time.Now()}
	o.progress.now = clock.now
	client.clock = clock

	_, err := o.runSpecialists(context.Background(), false)
	require.NoError(t, err)

	assert.NotEmpty(t, progressLines(t, out.String()), "a streaming specialist must feed the idle watchdog")
	assert.NotContains(t, out.String(), progressSentinel, "no raw fragment text reaches the transcript")
}

func TestProgressClient(t *testing.T) {
	t.Parallel()

	newMarker := func(out *lockedBuffer) (*progressMarker, *stepClock) {
		clock := newStepClock()

		return newProgressMarker(events.NewEmitter(nil, out), clock.now), clock
	}

	t.Run("a caller's own callback is kept and the marker stays quiet", func(t *testing.T) {
		t.Parallel()

		var out lockedBuffer

		marker, clock := newMarker(&out)
		client := progressClient{LLM: &fragmentLLM{clock: clock, gap: time.Second, fragments: 95}, onDelta: marker.onDelta}

		seen := 0
		_, err := client.SendStream(context.Background(), llm.Request{}, func(llm.Delta) { seen++ })
		require.NoError(t, err)

		assert.Equal(t, 190, seen, "every fragment reaches the caller's callback")
		assert.Empty(t, progressLines(t, out.String()))
	})

	t.Run("Send passes through", func(t *testing.T) {
		t.Parallel()

		var out lockedBuffer

		marker, _ := newMarker(&out)
		inner := &planLLM{responses: []llm.Response{{Content: "sent", FinishReason: "stop"}}}
		client := progressClient{LLM: inner, onDelta: marker.onDelta}

		resp, err := client.Send(context.Background(), llm.Request{Model: "rev/alpha"})
		require.NoError(t, err)

		assert.Equal(t, "sent", resp.Content)
		assert.Equal(t, []string{"rev/alpha"}, inner.models)
		assert.Empty(t, out.String())
	})
}
