package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/executor"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// stopRequest is one way ContextMatrix stops a card mid-admission.
type stopRequest struct {
	name string
	path string
	body any
}

func stopRequests(cardID string) []stopRequest {
	return []stopRequest{
		{name: "kill", path: "/kill", body: protocol.KillPayload{CardID: cardID, Project: "proj"}},
		{name: "stop-all for the project", path: "/stop-all", body: protocol.StopAllPayload{Project: "proj"}},
		{name: "stop-all for every project", path: "/stop-all", body: protocol.StopAllPayload{}},
	}
}

// gatedLaunch is a launch hook that signals entered, then waits for the
// launch context to end or for release. onCancel is what the launch returns
// when the stop's cancel reaches it first.
func gatedLaunch(entered chan<- struct{}, release <-chan struct{}, onCancel func(ctx context.Context) error) func(context.Context, executor.LaunchSpec) error {
	return func(ctx context.Context, _ executor.LaunchSpec) error {
		entered <- struct{}{}

		select {
		case <-ctx.Done():
			return onCancel(ctx)
		case <-release:
			return nil
		}
	}
}

// TestStopDuringAdmissionReportsNothing: ContextMatrix records the card as
// killed once /kill or /stop-all answers, and writes any status that arrives
// later over it. A launch the stop catches therefore reports neither running
// nor a failure, and leaves no tracked run or live proxy token behind.
func TestStopDuringAdmissionReportsNothing(t *testing.T) {
	launches := []struct {
		name     string
		onCancel func(ctx context.Context) error
	}{
		{name: "launch fails on the cancel", onCancel: func(ctx context.Context) error { return ctx.Err() }},
		{name: "launch completes despite the cancel", onCancel: func(context.Context) error { return nil }},
	}

	for _, launch := range launches {
		for _, stop := range stopRequests("PROJ-STOP") {
			t.Run(launch.name+"/"+stop.name, func(t *testing.T) {
				h := newHarness(t, 4)
				entered := make(chan struct{}, 1)
				release := make(chan struct{})
				h.exec.launchHook = gatedLaunch(entered, release, launch.onCancel)

				require.Equal(t, http.StatusAccepted, h.do(t, http.MethodPost, "/trigger", provisionedPayload("PROJ-STOP")).Code)
				<-entered

				w := h.do(t, http.MethodPost, stop.path, stop.body)

				close(release)

				require.Equal(t, http.StatusOK, w.Code)
				assertStoppedAdmission(t, h, "PROJ-STOP")
			})
		}
	}
}

// stopRequested reports whether a stop has reached an admission in flight for
// the card.
func (s *Server) stopRequested(project, cardID string) bool {
	s.admissionsMu.Lock()
	defer s.admissionsMu.Unlock()

	for adm := range s.admissions {
		if adm.project == project && adm.cardID == cardID && adm.stopped {
			return true
		}
	}

	return false
}

// TestStopBeforeLaunchSkipsTheLaunch: a stop that lands while the run is
// still being provisioned keeps the executor from ever being called.
func TestStopBeforeLaunchSkipsTheLaunch(t *testing.T) {
	for _, stop := range stopRequests("PROJ-STOP") {
		t.Run(stop.name, func(t *testing.T) {
			h := newHarness(t, 4)
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			h.creds.provisionHook = func() {
				entered <- struct{}{}

				<-release
			}

			var launches atomic.Int32

			h.exec.launchHook = func(context.Context, executor.LaunchSpec) error {
				launches.Add(1)

				return nil
			}

			require.Equal(t, http.StatusAccepted, h.do(t, http.MethodPost, "/trigger", provisionedPayload("PROJ-STOP")).Code)
			<-entered

			answered := make(chan *httptest.ResponseRecorder, 1)

			stopRequest := h.request(t, http.MethodPost, stop.path, stop.body, nowTS())

			go func() { answered <- h.serve(stopRequest) }()

			require.Eventually(t, func() bool { return h.server.stopRequested("proj", "PROJ-STOP") },
				time.Second, time.Millisecond)
			close(release)

			require.Equal(t, http.StatusOK, (<-answered).Code)
			assert.Zero(t, launches.Load(), "the executor is never asked to launch")
			assertStoppedAdmission(t, h, "PROJ-STOP")
		})
	}
}

// assertStoppedAdmission checks what a stopped admission leaves behind: no
// status callback, no tracked run, and its token and credentials gone.
func assertStoppedAdmission(t *testing.T, h *harness, cardID string) {
	t.Helper()

	assert.Never(t, func() bool { return len(h.reporter.statuses()) > 0 },
		50*time.Millisecond, 5*time.Millisecond, "a stopped admission reports nothing")

	_, tracked := h.tracker.Get("proj", cardID)
	assert.False(t, tracked, "a stopped admission leaves no tracked run")

	registers := h.proxy.registerCalls()
	require.Len(t, registers, 1)
	assert.Contains(t, h.proxy.revokeCalls(), registers[0].runKey)
	assert.Contains(t, h.creds.teardownCalls(), [3]string{"proj", cardID, registers[0].runKey})
}

// TestStopAllSparesOtherProjectsAdmissions: a stop-all scoped to one project
// leaves another project's launch to run.
func TestStopAllSparesOtherProjectsAdmissions(t *testing.T) {
	h := newHarness(t, 4)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	h.exec.launchHook = gatedLaunch(entered, release, func(ctx context.Context) error { return ctx.Err() })

	other := provisionedPayload("OTHER-1")
	other.Project = "other"

	require.Equal(t, http.StatusAccepted, h.do(t, http.MethodPost, "/trigger", provisionedPayload("PROJ-1")).Code)
	require.Equal(t, http.StatusAccepted, h.do(t, http.MethodPost, "/trigger", other).Code)
	<-entered
	<-entered

	require.Equal(t, http.StatusOK, h.do(t, http.MethodPost, "/stop-all", protocol.StopAllPayload{Project: "proj"}).Code)
	close(release)

	assert.Equal(t, [][3]string{{"OTHER-1", "running", ""}}, h.awaitStatuses(t, 1))
	assert.Never(t, func() bool { return len(h.reporter.statuses()) > 1 },
		50*time.Millisecond, 5*time.Millisecond, "the stopped project's launch reports nothing")

	_, tracked := h.tracker.Get("other", "OTHER-1")
	assert.True(t, tracked)
}

// TestLaunchTimeoutFailsAndFreesTheCard: a launch that never finishes, such as
// a stalled image pull, is failed at the launch timeout with its token and
// credentials torn down, and the card's admission lock is free for the next
// trigger.
func TestLaunchTimeoutFailsAndFreesTheCard(t *testing.T) {
	h := newHarness(t, 4)
	h.server.launchTimeout = 20 * time.Millisecond

	var calls atomic.Int32

	h.exec.launchHook = func(ctx context.Context, _ executor.LaunchSpec) error {
		if calls.Add(1) == 1 {
			<-ctx.Done()

			return ctx.Err()
		}

		return nil
	}

	ts := time.Now().Unix()
	payload := provisionedPayload("PROJ-STALL")

	require.Equal(t, http.StatusAccepted, h.doAt(t, http.MethodPost, "/trigger", payload, strconv.FormatInt(ts, 10)).Code)
	require.Equal(t, "failed", h.awaitStatuses(t, 1)[0][1])

	registers := h.proxy.registerCalls()
	require.Len(t, registers, 1)
	assert.Equal(t, []string{registers[0].runKey}, h.proxy.revokeCalls())
	assert.Equal(t, [][3]string{{"proj", "PROJ-STALL", registers[0].runKey}}, h.creds.teardownCalls())

	require.Equal(t, http.StatusAccepted, h.doAt(t, http.MethodPost, "/trigger", payload, strconv.FormatInt(ts+1, 10)).Code)
	assert.Equal(t, "running", h.awaitStatuses(t, 2)[1][1])
}
