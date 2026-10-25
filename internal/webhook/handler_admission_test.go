package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

// admissionOrdering is where a stop lands in an admission: block installs a
// blocking point that signals entered once the admission reaches it, then
// waits for release, or for the stop's cancel when cancelled is set.
type admissionOrdering struct {
	name      string
	block     func(h *harness, entered chan<- struct{}, release <-chan struct{})
	cancelled bool  // the stop's cancel, not release, unblocks the admission
	launches  int32 // Launch calls the admission makes
	launched  bool  // whether a run started, which the admission must kill
}

func admissionOrderings() []admissionOrdering {
	return []admissionOrdering{
		{
			name: "before launch",
			block: func(h *harness, entered chan<- struct{}, release <-chan struct{}) {
				h.creds.provisionHook = func() {
					entered <- struct{}{}

					<-release
				}
			},
		},
		{
			name: "while launch blocks, which fails on the cancel",
			block: func(h *harness, entered chan<- struct{}, _ <-chan struct{}) {
				h.exec.launchHook = func(ctx context.Context, _ executor.LaunchSpec) error {
					entered <- struct{}{}

					<-ctx.Done()

					return ctx.Err()
				}
			},
			cancelled: true,
			launches:  1,
		},
		{
			name: "while launch blocks, which completes despite the cancel",
			block: func(h *harness, entered chan<- struct{}, _ <-chan struct{}) {
				h.exec.launchHook = func(ctx context.Context, _ executor.LaunchSpec) error {
					entered <- struct{}{}

					<-ctx.Done()

					return nil
				}
			},
			cancelled: true,
			launches:  1,
			launched:  true,
		},
		{
			name: "just after launch returns",
			block: func(h *harness, entered chan<- struct{}, release <-chan struct{}) {
				h.server.afterLaunch = func() {
					entered <- struct{}{}

					<-release
				}
			},
			launches: 1,
			launched: true,
		},
	}
}

// TestStopDuringAdmission: ContextMatrix records the card as killed once
// /kill or /stop-all answers, and writes any status that arrives later over
// it. Wherever the stop lands in the admission, the launch therefore reports
// neither running nor a failure, and by the time the stop answers its token is
// revoked, its credentials are torn down and no run is left tracked. A run the
// executor already started is killed; its session secrets stay registered for
// its exit path to remove once its output has drained, while a launch that
// never started removes them before the answer.
func TestStopDuringAdmission(t *testing.T) {
	for _, ordering := range admissionOrderings() {
		for _, stop := range stopRequests("PROJ-STOP") {
			t.Run(ordering.name+"/"+stop.name, func(t *testing.T) {
				h := newHarness(t, 4)
				registry := newFakeSessionSecretRegistry()
				h.server.sessionRegistry = registry

				entered := make(chan struct{}, 1)
				release := make(chan struct{})

				ordering.block(h, entered, release)

				var launches atomic.Int32

				blockLaunch := h.exec.launchHook
				h.exec.launchHook = func(ctx context.Context, spec executor.LaunchSpec) error {
					launches.Add(1)

					if blockLaunch != nil {
						return blockLaunch(ctx, spec)
					}

					return nil
				}

				require.Equal(t, http.StatusAccepted, h.do(t, http.MethodPost, "/trigger", provisionedPayload("PROJ-STOP")).Code)
				<-entered

				stopRequest := h.request(t, http.MethodPost, stop.path, stop.body, nowTS())
				answered := make(chan *httptest.ResponseRecorder, 1)

				go func() { answered <- h.serve(stopRequest) }()

				// An admission held on release waits for the stop to reach it.
				if !ordering.cancelled {
					require.Eventually(t, func() bool { return h.server.stopRequested("proj", "PROJ-STOP") },
						time.Second, time.Millisecond)
					close(release)
				}

				var w *httptest.ResponseRecorder

				select {
				case w = <-answered:
				case <-time.After(5 * time.Second):
					t.Fatal("the stop never answered")
				}

				require.Equal(t, http.StatusOK, w.Code)
				assert.Equal(t, ordering.launches, launches.Load(), "launch calls")

				registers := h.proxy.registerCalls()
				require.Len(t, registers, 1)

				runKey := registers[0].runKey
				session := SessionID("proj", "PROJ-STOP", runKey)

				assert.Contains(t, h.proxy.revokeCalls(), runKey, "the token is revoked before the answer")
				assert.Contains(t, h.creds.teardownCalls(), [3]string{"proj", "PROJ-STOP", runKey},
					"the credentials are torn down before the answer")

				_, tracked := h.tracker.Get("proj", "PROJ-STOP")
				assert.False(t, tracked, "no run is left tracked")

				if ordering.launched {
					h.exec.mu.Lock()
					assert.Contains(t, h.exec.killed, [2]string{"proj", "PROJ-STOP"}, "the started run is killed")
					h.exec.mu.Unlock()

					assert.NotEmpty(t, registry.keys(session), "the exit path removes a started run's secrets after its output drains")
				} else {
					assert.Contains(t, registry.removedIDs(), session, "the session secrets are removed before the answer")
				}

				assert.Never(t, func() bool { return len(h.reporter.statuses()) > 0 },
					50*time.Millisecond, 5*time.Millisecond, "a stopped admission reports nothing")
			})
		}
	}
}

// TestStopReachesEveryAdmissionOfTheCard: a second trigger for the card waits
// on the card's launch lock while the first holds it. The stop cancels both
// and waits for both without holding any lock they need, so the first
// releases the lock, the second gives up before it provisions, and the stop
// answers.
func TestStopReachesEveryAdmissionOfTheCard(t *testing.T) {
	h := newHarness(t, 4)
	entered := make(chan struct{}, 2)

	var launches atomic.Int32

	h.exec.launchHook = func(ctx context.Context, _ executor.LaunchSpec) error {
		launches.Add(1)

		entered <- struct{}{}

		<-ctx.Done()

		return ctx.Err()
	}

	ts := time.Now().Unix()
	payload := provisionedPayload("PROJ-TWICE")

	require.Equal(t, http.StatusAccepted, h.doAt(t, http.MethodPost, "/trigger", payload, strconv.FormatInt(ts, 10)).Code)
	<-entered
	require.Equal(t, http.StatusAccepted, h.doAt(t, http.MethodPost, "/trigger", payload, strconv.FormatInt(ts+1, 10)).Code)

	stopRequest := h.request(t, http.MethodPost, "/kill", protocol.KillPayload{CardID: "PROJ-TWICE", Project: "proj"}, nowTS())
	answered := make(chan *httptest.ResponseRecorder, 1)

	go func() { answered <- h.serve(stopRequest) }()

	select {
	case w := <-answered:
		require.Equal(t, http.StatusOK, w.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("the stop never answered")
	}

	assert.Equal(t, int32(1), launches.Load(), "the waiting admission never launches")
	assert.Len(t, h.proxy.registerCalls(), 1, "the waiting admission never provisions")
	assert.Never(t, func() bool { return len(h.reporter.statuses()) > 0 },
		50*time.Millisecond, 5*time.Millisecond, "neither admission reports")
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
// a stalled image pull, holds until the launch timeout, is then failed with
// its token and credentials torn down, and leaves the card's admission lock
// free for the next trigger. Virtual time makes the timeout the production
// one and lets it fire only once the launch is blocked on it.
func TestLaunchTimeoutFailsAndFreesTheCard(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 4)

		var calls atomic.Int32

		h.exec.launchHook = func(ctx context.Context, _ executor.LaunchSpec) error {
			if calls.Add(1) == 1 {
				<-ctx.Done()

				return ctx.Err()
			}

			return nil
		}

		payload := provisionedPayload("PROJ-STALL")

		require.Equal(t, http.StatusAccepted, h.do(t, http.MethodPost, "/trigger", payload).Code)

		time.Sleep(launchTimeout - time.Second)
		synctest.Wait()
		assert.Empty(t, h.reporter.statuses(), "the stalled launch holds until its timeout")

		time.Sleep(2 * time.Second)
		synctest.Wait()

		statuses := h.reporter.statuses()
		require.Len(t, statuses, 1)
		assert.Equal(t, "failed", statuses[0][1])

		registers := h.proxy.registerCalls()
		require.Len(t, registers, 1)
		assert.Equal(t, []string{registers[0].runKey}, h.proxy.revokeCalls())
		assert.Equal(t, [][3]string{{"proj", "PROJ-STALL", registers[0].runKey}}, h.creds.teardownCalls())

		require.Equal(t, http.StatusAccepted, h.do(t, http.MethodPost, "/trigger", payload).Code)
		synctest.Wait()

		statuses = h.reporter.statuses()
		require.Len(t, statuses, 2)
		assert.Equal(t, "running", statuses[1][1])
	})
}
