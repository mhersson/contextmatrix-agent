package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
	// parks exits on end_session, the way the worker does after pushing WIP,
	// and its exit callback reports completed.
	parks workerBehavior = iota
	// ignores keeps running, so only the kill sweep stops it.
	ignores
	// unreachable has a stdin that refuses every write.
	unreachable
	// callsThenParks finishes its turn after end_session: it calls proxyURL,
	// the way a worker reaches the LLM proxy on serve's listener, then parks.
	callsThenParks
	// exitsSilently exits on end_session, but its exit callback never reports
	// before the process exits.
	exitsSilently
	// admitsAnotherThenParks parks, and another card's run is admitted first.
	admitsAnotherThenParks
	// parksAndIsReplaced parks, and a successor run of the same card is
	// admitted at once under the same key.
	parksAndIsReplaced
)

// proxyCall is the outcome of a callsThenParks worker's request.
type proxyCall struct {
	status int
	err    error
}

// fleet is what the fake workers share with the drain: the tracker the
// executor keeps, the status callbacks their exits report, and the claims
// that keep those reports to one per run.
type fleet struct {
	tracker  *executor.Tracker
	reporter *statusRecorder
	reports  *exitReports
}

// admit tracks a run for cardID under correlationID, answering end_session
// as behavior says.
func (f *fleet) admit(cardID, correlationID string, behavior workerBehavior) (*workerStdin, bool) {
	stdin := &workerStdin{behavior: behavior, fleet: f, calls: make(chan proxyCall, 1)}
	stdin.run = &executor.Run{
		ContainerID:   "ctr-" + correlationID,
		CardID:        cardID,
		Project:       "proj",
		CorrelationID: correlationID,
		StartedAt:     time.Now(),
		Stdin:         stdin,
	}

	return stdin, f.tracker.AddIfUnderLimit(stdin.run)
}

// exit is a run's own exit path: the executor drops the run, then its exit
// callback reports.
func (f *fleet) exit(run *executor.Run, status string) {
	f.tracker.Remove(run.Project, run.CardID)
	f.report(run, status)
}

// report is the run's exit callback reporting status, as onContainerExit
// does: not at all when the sweep has reported the run.
func (f *fleet) report(run *executor.Run, status string) {
	f.reports.report(run.CorrelationID, func() {
		_ = f.reporter.ReportStatus(context.Background(), run.CardID, run.Project, status, "")
	})
}

// workerStdin is a tracked run's stdin. It records end_session frames and acts
// out the run's behavior.
type workerStdin struct {
	mu       sync.Mutex
	ends     int
	behavior workerBehavior
	fleet    *fleet
	run      *executor.Run
	proxyURL string
	calls    chan proxyCall
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

	switch w.behavior {
	case parks:
		go w.fleet.exit(w.run, "completed")
	case callsThenParks:
		go w.callThenPark()
	case exitsSilently:
		go w.fleet.tracker.Remove(w.run.Project, w.run.CardID)
	case admitsAnotherThenParks:
		w.fleet.admit("CARD-9", "run-9", ignores)

		go w.fleet.exit(w.run, "completed")
	case parksAndIsReplaced:
		go func() {
			w.fleet.tracker.Remove(w.run.Project, w.run.CardID)
			w.fleet.admit(w.run.CardID, w.run.CorrelationID+"-successor", ignores)
			w.fleet.report(w.run, "completed")
		}()
	}

	return len(p), nil
}

func (w *workerStdin) callThenPark() {
	time.Sleep(50 * time.Millisecond)

	var call proxyCall

	client := &http.Client{Timeout: time.Second}

	resp, err := client.Get(w.proxyURL)
	if err != nil {
		call.err = err
	} else {
		call.status = resp.StatusCode
		_ = resp.Body.Close()
	}

	w.calls <- call

	w.fleet.exit(w.run, "completed")
}

func (w *workerStdin) Close() error { return nil }

func (w *workerStdin) endSessions() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.ends
}

// barrier holds every caller until n have arrived or the caller's context
// ends, so a test can tell concurrent calls from serial ones.
type barrier struct {
	n       int32
	arrived atomic.Int32
	all     chan struct{}
}

func newBarrier(n int) *barrier {
	return &barrier{n: int32(n), all: make(chan struct{})}
}

func (b *barrier) wait(ctx context.Context) error {
	if b.arrived.Add(1) == b.n {
		close(b.all)
	}

	select {
	case <-b.all:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// killRecorder is an executor whose Kill records when each card was killed
// and drops the run, standing in for the SIGKILL and the exit cleanup. When
// exitReports is set, the killed run's exit callback then races the sweep to
// report it.
type killRecorder struct {
	mu          sync.Mutex
	fleet       *fleet
	killed      map[string]time.Time
	exitReports bool

	// gate, when set, holds each kill until it lets the kill through or the
	// kill's context ends.
	gate func(ctx context.Context) error
}

func (k *killRecorder) Launch(context.Context, executor.LaunchSpec) error { return nil }

func (k *killRecorder) Kill(ctx context.Context, project, cardID string) error {
	if k.gate != nil {
		if err := k.gate(ctx); err != nil {
			return err
		}
	}

	k.mu.Lock()
	k.killed[cardID] = time.Now()
	k.mu.Unlock()

	run, ok := k.fleet.tracker.Get(project, cardID)
	if !ok {
		return executor.ErrNotFound
	}

	if k.exitReports {
		go k.fleet.exit(run, "failed")
	} else {
		k.fleet.tracker.Remove(project, cardID)
	}

	return nil
}

func (k *killRecorder) List(context.Context) ([]*executor.Run, error) {
	return k.fleet.tracker.List(), nil
}

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

	// gate, when set, holds each callback until it lets it through or the
	// callback's context ends.
	gate func(ctx context.Context) error
}

func (s *statusRecorder) ReportStatus(ctx context.Context, cardID, _, status, _ string) error {
	if s.gate != nil {
		if err := s.gate(ctx); err != nil {
			return err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.statuses[cardID] = append(s.statuses[cardID], status)

	return nil
}

type shutdownHarness struct {
	drain    workerDrain
	fleet    *fleet
	exec     *killRecorder
	reporter *statusRecorder
	revoker  *recordingRevoker
	stdins   map[string]*workerStdin
	server   *http.Server
}

// newShutdownHarness tracks one run per card, each answering end_session as
// its behavior says, and wires the real webhook end-session path. A card
// CARD-n runs under correlation id run-n. The tracker has room for one more
// run than workers.
func newShutdownHarness(t *testing.T, wait time.Duration, workers map[string]workerBehavior) *shutdownHarness {
	t.Helper()

	reporter := &statusRecorder{statuses: map[string][]string{}}
	f := &fleet{tracker: executor.NewTracker(len(workers) + 1), reporter: reporter, reports: newExitReports()}
	exec := &killRecorder{fleet: f, killed: map[string]time.Time{}, exitReports: true}
	revoker := &recordingRevoker{}
	stdins := make(map[string]*workerStdin, len(workers))

	for cardID, behavior := range workers {
		stdin, admitted := f.admit(cardID, "run-"+strings.TrimPrefix(cardID, "CARD-"), behavior)
		require.True(t, admitted)

		stdins[cardID] = stdin
	}

	return &shutdownHarness{
		drain: workerDrain{
			tracker:      f.tracker,
			sessions:     webhook.NewServer(webhook.Config{Tracker: f.tracker, Logger: testLogger()}),
			exec:         exec,
			reporter:     reporter,
			reports:      f.reports,
			revoker:      revoker,
			logger:       testLogger(),
			wait:         wait,
			poll:         time.Millisecond,
			sweepTimeout: 5 * time.Second,
		},
		fleet:    f,
		exec:     exec,
		reporter: reporter,
		revoker:  revoker,
		stdins:   stdins,
		server:   &http.Server{ReadHeaderTimeout: time.Second},
	}
}

// shutdown runs gracefulShutdown against h.server (unstarted unless a test
// starts one) and fails the test when it does not return within limit. Its
// return orders every write the shutdown made before the caller's reads.
func (h *shutdownHarness) shutdown(ctx context.Context, t *testing.T, limit time.Duration) {
	t.Helper()

	done := make(chan struct{})

	go func() {
		defer close(done)

		gracefulShutdown(ctx, h.server, nil, h.drain, &atomic.Bool{}, testLogger())
	}()

	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("gracefulShutdown did not return within %s", limit)
	}
}

// statuses returns the status callbacks each card received.
func (h *shutdownHarness) statuses() map[string][]string {
	h.reporter.mu.Lock()
	defer h.reporter.mu.Unlock()

	return maps.Clone(h.reporter.statuses)
}

// TestGracefulShutdownReportsEachRunOnce: a run reports one terminal status.
// A parked run keeps the completed its own exit callback reports, a killed
// straggler is reported failed once whichever of the sweep and its exit
// callback gets there first, and a run whose exit callback never reports is
// reported failed by the sweep.
func TestGracefulShutdownReportsEachRunOnce(t *testing.T) {
	tests := []struct {
		name    string
		wait    time.Duration
		workers map[string]workerBehavior
		want    map[string][]string
	}{
		{
			name:    "every run parks within the wait",
			wait:    time.Hour,
			workers: map[string]workerBehavior{"CARD-1": parks, "CARD-2": parks},
			want:    map[string][]string{"CARD-1": {"completed"}, "CARD-2": {"completed"}},
		},
		{
			name:    "no run parks",
			wait:    50 * time.Millisecond,
			workers: map[string]workerBehavior{"CARD-1": ignores, "CARD-2": ignores},
			want:    map[string][]string{"CARD-1": {"failed"}, "CARD-2": {"failed"}},
		},
		{
			name:    "a straggler among parked runs",
			wait:    50 * time.Millisecond,
			workers: map[string]workerBehavior{"CARD-1": parks, "CARD-2": ignores},
			want:    map[string][]string{"CARD-1": {"completed"}, "CARD-2": {"failed"}},
		},
		{
			name:    "an unreachable run is killed without holding the wait",
			wait:    time.Hour,
			workers: map[string]workerBehavior{"CARD-1": parks, "CARD-2": unreachable},
			want:    map[string][]string{"CARD-1": {"completed"}, "CARD-2": {"failed"}},
		},
		{
			name:    "a run whose exit callback never reports",
			wait:    50 * time.Millisecond,
			workers: map[string]workerBehavior{"CARD-1": parks, "CARD-2": exitsSilently},
			want:    map[string][]string{"CARD-1": {"completed"}, "CARD-2": {"failed"}},
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

			var wantKilled []string

			for cardID, behavior := range tt.workers {
				if behavior == ignores || behavior == unreachable {
					wantKilled = append(wantKilled, cardID)
				}
			}

			assert.ElementsMatch(t, wantKilled, slices.Collect(maps.Keys(h.exec.killedAt())))
			assert.Equal(t, tt.want, h.statuses())
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

// TestGracefulShutdownSweepsConcurrently: every straggler's kill and every
// status callback overlap, so the sweep's length does not grow with the run
// count. Each kill and each callback here returns only once all of them have
// started.
func TestGracefulShutdownSweepsConcurrently(t *testing.T) {
	workers := map[string]workerBehavior{"CARD-1": ignores, "CARD-2": ignores, "CARD-3": ignores}

	h := newShutdownHarness(t, time.Millisecond, workers)
	h.exec.exitReports = false
	h.exec.gate = newBarrier(len(workers)).wait
	h.reporter.gate = newBarrier(len(workers)).wait

	h.shutdown(context.Background(), t, 5*time.Second)

	assert.Len(t, h.exec.killedAt(), len(workers))
	assert.Equal(t, map[string][]string{"CARD-1": {"failed"}, "CARD-2": {"failed"}, "CARD-3": {"failed"}}, h.statuses())
}

// TestGracefulShutdownSweepEndsAtItsDeadline: kills that never return cannot
// hold the shutdown past the one deadline the sweep runs under.
func TestGracefulShutdownSweepEndsAtItsDeadline(t *testing.T) {
	h := newShutdownHarness(t, time.Millisecond, map[string]workerBehavior{"CARD-1": ignores, "CARD-2": ignores})
	h.drain.sweepTimeout = 50 * time.Millisecond
	h.exec.gate = func(ctx context.Context) error {
		<-ctx.Done()

		return ctx.Err()
	}

	h.shutdown(context.Background(), t, 5*time.Second)

	assert.Empty(t, h.exec.killedAt())
}

// TestGracefulShutdownSweepsARunAdmittedAfterTheSnapshot: a run that enters
// the tracker after shutdown listed the live runs is still killed, revoked
// and reported.
func TestGracefulShutdownSweepsARunAdmittedAfterTheSnapshot(t *testing.T) {
	h := newShutdownHarness(t, 50*time.Millisecond, map[string]workerBehavior{"CARD-1": admitsAnotherThenParks})

	h.shutdown(context.Background(), t, 5*time.Second)

	assert.Contains(t, h.exec.killedAt(), "CARD-9")
	assert.Contains(t, h.revoker.revoked, "run-9")
	assert.Equal(t, []string{"failed"}, h.statuses()["CARD-9"])
}

// TestGracefulShutdownParkWaitIgnoresASuccessor: a run of the same card
// admitted after the parked one exited is not mistaken for it, so the park
// wait ends, and the successor is swept like any other run.
func TestGracefulShutdownParkWaitIgnoresASuccessor(t *testing.T) {
	h := newShutdownHarness(t, time.Hour, map[string]workerBehavior{"CARD-1": parksAndIsReplaced})

	h.shutdown(context.Background(), t, 5*time.Second)

	assert.Contains(t, h.exec.killedAt(), "CARD-1", "the successor is killed")
	assert.ElementsMatch(t, []string{"completed", "failed"}, h.statuses()["CARD-1"])
	assert.ElementsMatch(t, []string{"run-1", "run-1-successor"}, h.revoker.revoked)
}

// TestGracefulShutdownServesTheProxyUntilWorkersPark: the LLM proxy shares the
// webhook listener, so a worker finishing its turn while it parks must still
// reach it, and the listener closes only once the worker has exited.
func TestGracefulShutdownServesTheProxyUntilWorkersPark(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		ReadHeaderTimeout: time.Second,
	}

	go func() { _ = server.Serve(ln) }()

	t.Cleanup(func() { _ = server.Close() })

	h := newShutdownHarness(t, time.Hour, map[string]workerBehavior{"CARD-1": callsThenParks})
	h.server = server
	h.stdins["CARD-1"].proxyURL = "http://" + ln.Addr().String()

	trackedAtShutdown := make(chan int, 1)

	server.RegisterOnShutdown(func() { trackedAtShutdown <- h.drain.tracker.Count() })

	h.shutdown(context.Background(), t, 5*time.Second)

	select {
	case call := <-h.stdins["CARD-1"].calls:
		require.NoError(t, call.err, "the parking worker reaches the listener")
		assert.Equal(t, http.StatusOK, call.status)
	case <-time.After(5 * time.Second):
		t.Fatal("the worker never made its call")
	}

	select {
	case count := <-trackedAtShutdown:
		assert.Zero(t, count, "the listener shuts down only after every worker has exited")
	case <-time.After(5 * time.Second):
		t.Fatal("the listener was never shut down")
	}
}

// TestGracefulShutdownRevokesEverySweptRun: a killed straggler and a parked run
// whose own exit callback may not have fired both lose their proxy token.
func TestGracefulShutdownRevokesEverySweptRun(t *testing.T) {
	h := newShutdownHarness(t, 50*time.Millisecond, map[string]workerBehavior{"CARD-1": ignores, "CARD-2": parks})

	h.shutdown(context.Background(), t, 5*time.Second)

	assert.ElementsMatch(t, []string{"run-1", "run-2"}, h.revoker.revoked)
}

func TestEndSessionsWarnsOnlyForARunItCouldNotReach(t *testing.T) {
	tests := []struct {
		name     string
		behavior workerBehavior
		exited   bool
		wantWarn bool
		wantWait bool
	}{
		{name: "run exited before its end_session", behavior: ignores, exited: true, wantWait: true},
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

			waitOn := h.drain.endSessions(live)
			if tt.wantWait {
				assert.Equal(t, live, waitOn, "an exited run is still waited on for its report")
			} else {
				assert.Empty(t, waitOn, "an unreachable run is killed, not waited on")
			}

			assert.Equal(t, tt.wantWarn, strings.Contains(buf.String(), `"level":"WARN"`))
		})
	}
}

// TestParkWaitCoversARunThatExitedBeforeItsEndSession: a run gone from the
// tracker whose exit callback has not claimed its report yet must hold the park
// wait, or the sweep reports "failed" over the run's own status.
func TestParkWaitCoversARunThatExitedBeforeItsEndSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newShutdownHarness(t, time.Hour, map[string]workerBehavior{"CARD-1": ignores})
		live := h.drain.tracker.List()
		require.Len(t, live, 1)

		h.drain.reports.beginShutdown()
		h.drain.tracker.Remove("proj", "CARD-1")

		waitOn := h.drain.endSessions(live)

		parked := make(chan struct{})

		go func() {
			defer close(parked)

			h.drain.awaitPark(context.Background(), waitOn)
		}()

		synctest.Wait()

		select {
		case <-parked:
			t.Fatal("the park wait returned before the exited run's exit callback claimed its report")
		default:
		}

		h.fleet.report(live[0], "completed")
		<-parked

		h.drain.sweep(live)

		assert.Equal(t, []string{"completed"}, h.statuses()["CARD-1"])
	})
}
