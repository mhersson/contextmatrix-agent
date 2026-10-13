package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-agent/internal/webhook"
	"github.com/mhersson/contextmatrix-backendkit/frames"
)

// workerBehavior is how a fake worker answers an end_session frame.
type workerBehavior int

const (
	// parks exits on end_session, the way the worker does after pushing WIP.
	parks workerBehavior = iota
	// ignores keeps running, so only the kill sweep stops it.
	ignores
	// unreachable has a stdin that refuses every write.
	unreachable
)

// workerStdin is a tracked run's stdin. It records end_session frames and, for
// a parking worker, drops the run from the tracker as the executor does once
// the container exits.
type workerStdin struct {
	mu       sync.Mutex
	ends     int
	behavior workerBehavior
	tracker  *executor.Tracker
	project  string
	cardID   string
}

func (w *workerStdin) Write(p []byte) (int, error) {
	if w.behavior == unreachable {
		return 0, errors.New("stdin closed")
	}

	var f frames.Frame
	if err := json.Unmarshal(p, &f); err != nil {
		return 0, err
	}

	if f.Type != frames.TypeEndSession {
		return len(p), nil
	}

	w.mu.Lock()
	w.ends++
	w.mu.Unlock()

	if w.behavior == parks {
		go w.tracker.Remove(w.project, w.cardID)
	}

	return len(p), nil
}

func (w *workerStdin) Close() error { return nil }

func (w *workerStdin) endSessions() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.ends
}

// killRecorder is an executor whose Kill records when each card was killed and
// removes the run, standing in for the SIGKILL and the exit cleanup.
type killRecorder struct {
	mu      sync.Mutex
	tracker *executor.Tracker
	killed  map[string]time.Time
}

func (k *killRecorder) Launch(context.Context, executor.LaunchSpec) error { return nil }

func (k *killRecorder) Kill(_ context.Context, project, cardID string) error {
	k.mu.Lock()
	k.killed[cardID] = time.Now()
	k.mu.Unlock()

	k.tracker.Remove(project, cardID)

	return nil
}

func (k *killRecorder) List(context.Context) ([]*executor.Run, error) { return k.tracker.List(), nil }

func (k *killRecorder) StopAll(context.Context, string) ([]executor.StopResult, error) {
	return nil, nil
}

func (k *killRecorder) CleanupOrphans(context.Context) error { return nil }

func (k *killRecorder) killedAt() map[string]time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()

	return maps.Clone(k.killed)
}

// statusRecorder records every status callback by card.
type statusRecorder struct {
	mu       sync.Mutex
	statuses map[string][]string
}

func (s *statusRecorder) ReportStatus(_ context.Context, cardID, _, status, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.statuses[cardID] = append(s.statuses[cardID], status)

	return nil
}

type shutdownHarness struct {
	drain    workerDrain
	exec     *killRecorder
	reporter *statusRecorder
	stdins   map[string]*workerStdin
}

// newShutdownHarness tracks one run per card, each answering end_session as
// its behavior says, and wires the real webhook end-session path.
func newShutdownHarness(t *testing.T, wait time.Duration, workers map[string]workerBehavior) *shutdownHarness {
	t.Helper()

	tracker := executor.NewTracker(len(workers))
	exec := &killRecorder{tracker: tracker, killed: map[string]time.Time{}}
	reporter := &statusRecorder{statuses: map[string][]string{}}
	stdins := make(map[string]*workerStdin, len(workers))

	for cardID, behavior := range workers {
		stdin := &workerStdin{behavior: behavior, tracker: tracker, project: "proj", cardID: cardID}
		stdins[cardID] = stdin

		require.True(t, tracker.AddIfUnderLimit(&executor.Run{
			ContainerID: "ctr-" + cardID,
			CardID:      cardID,
			Project:     "proj",
			StartedAt:   time.Now(),
			Stdin:       stdin,
		}))
	}

	return &shutdownHarness{
		drain: workerDrain{
			tracker:  tracker,
			sessions: webhook.NewServer(webhook.Config{Tracker: tracker, Logger: testLogger()}),
			exec:     exec,
			reporter: reporter,
			logger:   testLogger(),
			wait:     wait,
			poll:     time.Millisecond,
		},
		exec:     exec,
		reporter: reporter,
		stdins:   stdins,
	}
}

// shutdown runs gracefulShutdown against unstarted servers and fails the test
// when it does not return within limit.
func (h *shutdownHarness) shutdown(ctx context.Context, t *testing.T, limit time.Duration) {
	t.Helper()

	done := make(chan struct{})

	go func() {
		defer close(done)

		gracefulShutdown(ctx, &http.Server{ReadHeaderTimeout: time.Second}, nil, h.drain, &atomic.Bool{}, testLogger())
	}()

	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("gracefulShutdown did not return within %s", limit)
	}
}

func TestGracefulShutdownKillsOnlyRunsThatDoNotPark(t *testing.T) {
	tests := []struct {
		name       string
		wait       time.Duration
		workers    map[string]workerBehavior
		wantKilled []string
	}{
		{
			name:    "every run parks within the wait",
			wait:    time.Hour,
			workers: map[string]workerBehavior{"CARD-1": parks, "CARD-2": parks},
		},
		{
			name:       "no run parks",
			wait:       50 * time.Millisecond,
			workers:    map[string]workerBehavior{"CARD-1": ignores, "CARD-2": ignores},
			wantKilled: []string{"CARD-1", "CARD-2"},
		},
		{
			name:       "a straggler among parked runs",
			wait:       50 * time.Millisecond,
			workers:    map[string]workerBehavior{"CARD-1": parks, "CARD-2": ignores},
			wantKilled: []string{"CARD-2"},
		},
		{
			name:       "an unreachable run is killed without holding the wait",
			wait:       time.Hour,
			workers:    map[string]workerBehavior{"CARD-1": parks, "CARD-2": unreachable},
			wantKilled: []string{"CARD-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newShutdownHarness(t, tt.wait, tt.workers)

			h.shutdown(context.Background(), t, 5*time.Second)

			for cardID, behavior := range tt.workers {
				if behavior == unreachable {
					continue
				}

				assert.Equal(t, 1, h.stdins[cardID].endSessions(), "%s gets one end_session frame", cardID)
			}

			assert.ElementsMatch(t, tt.wantKilled, slices.Collect(maps.Keys(h.exec.killedAt())))

			for cardID := range tt.workers {
				assert.Equal(t, []string{"failed"}, h.reporter.statuses[cardID],
					"%s gets one terminal status even if its own exit callback never lands", cardID)
			}
		})
	}
}

func TestGracefulShutdownKillsStragglersOnlyAfterTheWait(t *testing.T) {
	const wait = 200 * time.Millisecond

	h := newShutdownHarness(t, wait, map[string]workerBehavior{"CARD-1": ignores})

	start := time.Now()

	h.shutdown(context.Background(), t, 5*time.Second)

	killedAt, ok := h.exec.killedAt()["CARD-1"]
	require.True(t, ok, "the straggler is killed")
	assert.GreaterOrEqual(t, killedAt.Sub(start), wait)
}

func TestGracefulShutdownWaitEndsWithTheShutdownContext(t *testing.T) {
	h := newShutdownHarness(t, time.Hour, map[string]workerBehavior{"CARD-1": ignores})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	time.AfterFunc(50*time.Millisecond, cancel)

	h.shutdown(ctx, t, 5*time.Second)

	assert.Contains(t, h.exec.killedAt(), "CARD-1", "a cancelled wait goes straight to the kill sweep")
	assert.Equal(t, 1, h.stdins["CARD-1"].endSessions())
}

func TestEndSessionsWarnsOnlyForARunItCouldNotReach(t *testing.T) {
	tests := []struct {
		name     string
		behavior workerBehavior
		exited   bool
		wantWarn bool
	}{
		{name: "run exited before its end_session", behavior: ignores, exited: true},
		{name: "run whose stdin refuses the frame", behavior: unreachable, wantWarn: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newShutdownHarness(t, time.Hour, map[string]workerBehavior{"CARD-1": tt.behavior})
			live := h.drain.tracker.List()

			if tt.exited {
				h.drain.tracker.Remove("proj", "CARD-1")
			}

			var buf bytes.Buffer

			h.drain.logger = slog.New(slog.NewJSONHandler(&buf, nil))

			assert.Empty(t, h.drain.endSessions(live), "an unreached run is not waited on")
			assert.Equal(t, tt.wantWarn, strings.Contains(buf.String(), `"level":"WARN"`))
		})
	}
}
