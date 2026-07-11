package coop

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bearerCtx and dialEndpoint are shared package test helpers from
// server_test.go (Task 4).

const modBearer = "moderator-test-bearer"

// historyRecorder is a thread-safe SeatRunner wrapper recording histories.
type historyRecorder struct {
	mu        sync.Mutex
	histories [][]Turn
}

func (h *historyRecorder) record(history []Turn) {
	h.mu.Lock()
	h.histories = append(h.histories, append([]Turn(nil), history...))
	h.mu.Unlock()
}

func (h *historyRecorder) get(i int) []Turn {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.histories[i]
}

func TestSendTurnHappyPathPersistsTask(t *testing.T) {
	rec := &historyRecorder{}

	var (
		mu    sync.Mutex
		calls int
	)

	runner := func(_ context.Context, _ SeatConfig, history []Turn, _ string) (string, float64, error) {
		rec.record(history)

		mu.Lock()
		calls++
		n := calls
		mu.Unlock()

		return fmt.Sprintf("reply-%d", n), 0.01, nil
	}

	srv, err := StartServer([]SeatConfig{{Name: "seat-1", Lens: "risk"}}, runner, modBearer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })

	h, err := dialSeat(t.Context(), "seat-1", "risk", srv.SeatEndpoint("seat-1"), modBearer)
	require.NoError(t, err)
	t.Cleanup(func() { h.closeSeat(context.Background()) })

	assert.Equal(t, internalTurnDeadline, h.deadline)
	assert.False(t, h.guest)

	u1, err := h.sendTurn(t.Context(), 0, "body-0")
	require.NoError(t, err)
	assert.Equal(t, "reply-1", u1)
	assert.False(t, h.absent)
	assert.InDelta(t, 0.01, h.lastCost, 1e-12)
	require.NotEmpty(t, h.taskID)

	tid := h.taskID

	u2, err := h.sendTurn(t.Context(), 1, "body-1")
	require.NoError(t, err)
	assert.Equal(t, "reply-2", u2)
	assert.Equal(t, tid, h.taskID, "task must persist across rounds")

	// The second turn's history proves the same task carried the exchange.
	assert.Equal(t, []Turn{
		{Role: "user", Content: "body-0"},
		{Role: "assistant", Content: "reply-1"},
	}, rec.get(1))
}

func TestSendTurnTimeoutReplacesTask(t *testing.T) {
	rec := &historyRecorder{}
	canceled := make(chan struct{})

	var (
		mu    sync.Mutex
		calls int
	)

	runner := func(ctx context.Context, _ SeatConfig, history []Turn, _ string) (string, float64, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()

		rec.record(history)

		if n == 2 {
			<-ctx.Done() // hang until CancelTask lands
			close(canceled)

			return "", 0, ctx.Err()
		}

		return fmt.Sprintf("reply-%d", n), 0, nil
	}

	srv, err := StartServer([]SeatConfig{{Name: "seat-1"}}, runner, modBearer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })

	h, err := dialSeat(t.Context(), "seat-1", "", srv.SeatEndpoint("seat-1"), modBearer)
	require.NoError(t, err)
	t.Cleanup(func() { h.closeSeat(context.Background()) })

	_, err = h.sendTurn(t.Context(), 0, "body-0")
	require.NoError(t, err)

	tid := h.taskID
	require.NotEmpty(t, tid)

	h.deadline = 500 * time.Millisecond

	_, err = h.sendTurn(t.Context(), 1, "body-1")
	require.ErrorIs(t, err, errTurnTimeout)
	assert.True(t, h.absent)
	assert.Empty(t, h.taskID, "timed-out task must be cleared for replacement")

	select {
	case <-canceled:
		// CancelTask reached the running executor: its ctx was canceled.
	case <-time.After(5 * time.Second):
		t.Fatal("runner context was never canceled after CancelTask")
	}

	// Seat rejoins on a fresh task carrying the full snapshot.
	h.deadline = 30 * time.Second

	u3, err := h.sendTurn(t.Context(), 2, "full snapshot body")
	require.NoError(t, err)
	assert.Equal(t, "reply-3", u3)
	assert.False(t, h.absent)
	require.NotEmpty(t, h.taskID)
	assert.NotEqual(t, tid, h.taskID, "replacement must be a new task")
	assert.Empty(t, rec.get(2), "fresh task starts with empty history")
}

func TestCloseSeatCompletesTask(t *testing.T) {
	var calls atomic.Int32

	runner := func(_ context.Context, _ SeatConfig, _ []Turn, _ string) (string, float64, error) {
		calls.Add(1)

		return "reply", 0, nil
	}

	srv, err := StartServer([]SeatConfig{{Name: "seat-1"}}, runner, modBearer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })

	h, err := dialSeat(t.Context(), "seat-1", "", srv.SeatEndpoint("seat-1"), modBearer)
	require.NoError(t, err)

	_, err = h.sendTurn(t.Context(), 0, "body-0")
	require.NoError(t, err)

	tid := h.taskID

	h.closeSeat(t.Context())

	assert.Equal(t, int32(1), calls.Load(), "close must not invoke the runner")

	// Verify with a fresh client that the task is completed.
	verify, err := dialEndpoint(t, srv.SeatEndpoint("seat-1"))
	require.NoError(t, err)

	task, err := verify.GetTask(bearerCtx(t.Context(), modBearer), &a2a.GetTaskRequest{ID: tid})
	require.NoError(t, err)
	assert.Equal(t, a2a.TaskStateCompleted, task.Status.State)
}

// startGuestServer runs a second, standalone A2A server the way an
// operator's shim would: own bearer, agent card at the well-known root path.
func startGuestServer(t *testing.T, token string, runner SeatRunner) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	ts := httptest.NewServer(bearerMiddleware(token, mux))
	t.Cleanup(ts.Close)

	card := &a2a.AgentCard{
		Name:    "guest-shim",
		Version: "1",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(ts.URL+"/a2a", a2a.TransportProtocolJSONRPC),
		},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
	}

	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("/a2a", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(newSeatExecutor(SeatConfig{Name: "guest"}, runner))))

	return ts
}

func TestDialGuestResolvesCardAndTalks(t *testing.T) {
	runner := func(_ context.Context, _ SeatConfig, _ []Turn, _ string) (string, float64, error) {
		return "guest says hi", 0, nil
	}

	ts := startGuestServer(t, "guest-token", runner)

	h, err := dialGuest(t.Context(), GuestSeat{Name: "laptop", URL: ts.URL, Token: "guest-token"})
	require.NoError(t, err)
	t.Cleanup(func() { h.closeSeat(context.Background()) })

	assert.Equal(t, "guest-laptop", h.name)
	assert.True(t, h.guest)
	assert.Equal(t, guestTurnDeadline, h.deadline)

	u, err := h.sendTurn(t.Context(), 0, "briefing")
	require.NoError(t, err)
	assert.Equal(t, "guest says hi", u)
	assert.Zero(t, h.lastCost, "guest compute is the guest's own")
}

func TestDialGuestWrongTokenFails(t *testing.T) {
	runner := func(_ context.Context, _ SeatConfig, _ []Turn, _ string) (string, float64, error) {
		return "unreachable", 0, nil
	}

	ts := startGuestServer(t, "guest-token", runner)

	_, err := dialGuest(t.Context(), GuestSeat{Name: "laptop", URL: ts.URL, Token: "wrong"})
	require.Error(t, err, "card resolution must fail on 401")
	assert.NotErrorIs(t, err, errTurnTimeout)
}
