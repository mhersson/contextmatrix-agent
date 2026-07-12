package mob

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seatCall records one SeatRunner invocation.
type seatCall struct {
	history []Turn
	prompt  string
}

// scriptedRunner is a thread-safe scripted SeatRunner: reply decides the
// n-th call's result (n is 0-based).
type scriptedRunner struct {
	mu    sync.Mutex
	calls []seatCall
	reply func(n int) (string, float64, error)
}

func (r *scriptedRunner) runner() SeatRunner {
	return func(_ context.Context, _ SeatConfig, history []Turn, prompt string) (string, float64, error) {
		r.mu.Lock()
		n := len(r.calls)
		r.calls = append(r.calls, seatCall{history: append([]Turn(nil), history...), prompt: prompt})
		r.mu.Unlock()

		return r.reply(n)
	}
}

func (r *scriptedRunner) snapshot() []seatCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]seatCall(nil), r.calls...)
}

// startSeatServer serves one seat executor over a real a2asrv JSON-RPC
// handler on an httptest server and returns a connected client.
func startSeatServer(t *testing.T, seat SeatConfig, runner SeatRunner) *a2aclient.Client {
	t.Helper()

	mux := http.NewServeMux()
	mux.Handle("/a2a", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(newSeatExecutor(seat, runner))))

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client, err := a2aclient.NewFromEndpoints(t.Context(), []*a2a.AgentInterface{
		a2a.NewAgentInterface(ts.URL+"/a2a", a2a.TransportProtocolJSONRPC),
	}, a2aclient.WithJSONRPCTransport(http.DefaultClient))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Destroy() })

	return client
}

// sendRound sends one round-control message and requires a *a2a.Task result.
func sendRound(t *testing.T, client *a2aclient.Client, taskID a2a.TaskID, round int, body string) *a2a.Task {
	t.Helper()

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(body))
	setControl(msg, control{Kind: controlRound, Round: round})

	if taskID != "" {
		msg.TaskID = taskID
	}

	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: msg})
	require.NoError(t, err)

	task, ok := res.(*a2a.Task)
	require.True(t, ok, "want *a2a.Task, got %T", res)

	return task
}

func TestSeatExecutorFirstTurnParksInputRequired(t *testing.T) {
	runner := &scriptedRunner{reply: func(int) (string, float64, error) { return "utterance-1", 0, nil }}
	client := startSeatServer(t, SeatConfig{Name: "seat-1", Lens: "correctness"}, runner.runner())

	task := sendRound(t, client, "", 0, "round-0 body")

	assert.NotEmpty(t, task.ID)
	assert.Equal(t, a2a.TaskStateInputRequired, task.Status.State)
	require.NotNil(t, task.Status.Message)
	assert.Equal(t, "utterance-1", messageText(task.Status.Message))

	calls := runner.snapshot()
	require.Len(t, calls, 1)
	assert.Empty(t, calls[0].history)
	assert.Equal(t, "round-0 body", calls[0].prompt)
}

func TestSeatExecutorStreamsProgression(t *testing.T) {
	runner := &scriptedRunner{reply: func(int) (string, float64, error) { return "done", 0, nil }}
	client := startSeatServer(t, SeatConfig{Name: "seat-1"}, runner.runner())

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"))
	setControl(msg, control{Kind: controlRound, Round: 0})

	var states []a2a.TaskState

	for event, err := range client.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: msg}) {
		require.NoError(t, err)

		switch ev := event.(type) {
		case *a2a.Task:
			states = append(states, ev.Status.State)
		case *a2a.TaskStatusUpdateEvent:
			states = append(states, ev.Status.State)
		}
	}

	assert.Equal(t, []a2a.TaskState{
		a2a.TaskStateSubmitted,
		a2a.TaskStateWorking,
		a2a.TaskStateInputRequired,
	}, states)
}

func TestSeatExecutorRebuildsHistoryAcrossTurns(t *testing.T) {
	runner := &scriptedRunner{reply: func(n int) (string, float64, error) {
		return fmt.Sprintf("utterance-%d", n+1), 0, nil
	}}
	client := startSeatServer(t, SeatConfig{Name: "seat-1"}, runner.runner())

	task := sendRound(t, client, "", 0, "round-0 body")
	assert.Equal(t, "utterance-1", messageText(task.Status.Message))

	task = sendRound(t, client, task.ID, 1, "round-1 body")
	assert.Equal(t, "utterance-2", messageText(task.Status.Message))

	task = sendRound(t, client, task.ID, 2, "round-2 body")
	assert.Equal(t, "utterance-3", messageText(task.Status.Message))

	calls := runner.snapshot()
	require.Len(t, calls, 3)

	assert.Empty(t, calls[0].history)
	assert.Equal(t, "round-0 body", calls[0].prompt)

	assert.Equal(t, []Turn{
		{Role: "user", Content: "round-0 body"},
		{Role: "assistant", Content: "utterance-1"},
	}, calls[1].history)
	assert.Equal(t, "round-1 body", calls[1].prompt)

	assert.Equal(t, []Turn{
		{Role: "user", Content: "round-0 body"},
		{Role: "assistant", Content: "utterance-1"},
		{Role: "user", Content: "round-1 body"},
		{Role: "assistant", Content: "utterance-2"},
	}, calls[2].history)
	assert.Equal(t, "round-2 body", calls[2].prompt)
}

func TestSeatExecutorCloseCompletesWithoutRunning(t *testing.T) {
	runner := &scriptedRunner{reply: func(int) (string, float64, error) { return "utterance-1", 0, nil }}
	client := startSeatServer(t, SeatConfig{Name: "seat-1"}, runner.runner())

	task := sendRound(t, client, "", 0, "round-0 body")

	closeMsg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("closing"))
	setControl(closeMsg, control{Kind: controlClose})
	closeMsg.TaskID = task.ID

	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: closeMsg})
	require.NoError(t, err)

	closed, ok := res.(*a2a.Task)
	require.True(t, ok, "want *a2a.Task, got %T", res)
	assert.Equal(t, a2a.TaskStateCompleted, closed.Status.State)

	assert.Len(t, runner.snapshot(), 1, "close must not invoke the runner")
}

func TestSeatExecutorRunnerErrorFailsTask(t *testing.T) {
	runner := &scriptedRunner{reply: func(int) (string, float64, error) {
		return "", 0, errors.New("boom: model exploded")
	}}
	client := startSeatServer(t, SeatConfig{Name: "seat-1"}, runner.runner())

	task := sendRound(t, client, "", 0, "round-0 body")

	assert.Equal(t, a2a.TaskStateFailed, task.Status.State)
	require.NotNil(t, task.Status.Message)
	assert.Contains(t, messageText(task.Status.Message), "boom: model exploded")
}

func TestSeatExecutorCostRidesUtteranceMetadata(t *testing.T) {
	runner := &scriptedRunner{reply: func(int) (string, float64, error) { return "priced", 0.0123, nil }}
	client := startSeatServer(t, SeatConfig{Name: "seat-1"}, runner.runner())

	task := sendRound(t, client, "", 0, "round-0 body")

	assert.InDelta(t, 0.0123, costFrom(task.Status.Message), 1e-12)
}
