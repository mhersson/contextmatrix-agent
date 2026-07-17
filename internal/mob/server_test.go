package mob

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBearer = "server-test-bearer"

// bearerCtx attaches the Authorization service param the way the moderator
// does - the JSON-RPC transport turns it into an HTTP header.
func bearerCtx(ctx context.Context, token string) context.Context {
	return a2aclient.AttachServiceParams(ctx, a2aclient.ServiceParams{
		"Authorization": {"Bearer " + token},
	})
}

// dialEndpoint connects a raw a2a client to one seat endpoint. It returns the
// dial error rather than asserting on it directly - callers may invoke it
// from a spawned goroutine, where require/assert.FailNow-style calls are
// unsafe.
func dialEndpoint(t *testing.T, endpoint string) (*a2aclient.Client, error) {
	t.Helper()

	client, err := a2aclient.NewFromEndpoints(t.Context(), []*a2a.AgentInterface{
		a2a.NewAgentInterface(endpoint, a2a.TransportProtocolJSONRPC),
	}, a2aclient.WithJSONRPCTransport(http.DefaultClient))
	if err != nil {
		return nil, err
	}

	t.Cleanup(func() { _ = client.Destroy() })

	return client, nil
}

func echoSeatRunner(_ context.Context, seat SeatConfig, _ []Turn, _ string) (string, float64, error) {
	return "hello from " + seat.Name, 0, nil
}

func startTestServer(t *testing.T, seats ...SeatConfig) *Server {
	t.Helper()

	srv, err := StartServer(seats, echoSeatRunner, testBearer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })

	return srv
}

func TestStartServerServesTwoSeatsConcurrently(t *testing.T) {
	srv := startTestServer(t,
		SeatConfig{Name: "seat-1", Lens: "feasibility"},
		SeatConfig{Name: "seat-2", Lens: "risk"},
	)

	ep1 := srv.SeatEndpoint("seat-1")
	ep2 := srv.SeatEndpoint("seat-2")

	assert.NotEqual(t, ep1, ep2)
	assert.True(t, strings.HasPrefix(ep1, "http://127.0.0.1:"), ep1)
	assert.True(t, strings.HasSuffix(ep1, "/seats/seat-1"), ep1)
	assert.True(t, strings.HasSuffix(ep2, "/seats/seat-2"), ep2)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		replies = map[string]string{}
	)

	for _, name := range []string{"seat-1", "seat-2"} {
		wg.Add(1)

		go func(name string) {
			defer wg.Done()

			client, err := dialEndpoint(t, srv.SeatEndpoint(name))
			if err != nil {
				t.Errorf("dial %s: %v", name, err)

				return
			}

			msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("speak"))
			setControl(msg, control{Kind: controlRound, Round: 0})

			res, err := client.SendMessage(bearerCtx(t.Context(), testBearer),
				&a2a.SendMessageRequest{Message: msg})
			if err != nil {
				t.Errorf("send to %s: %v", name, err)

				return
			}

			task, ok := res.(*a2a.Task)
			if !ok {
				t.Errorf("send to %s: got %T, want *a2a.Task", name, res)

				return
			}

			mu.Lock()
			replies[name] = messageText(task.Status.Message)
			mu.Unlock()
		}(name)
	}

	wg.Wait()

	assert.Equal(t, map[string]string{
		"seat-1": "hello from seat-1",
		"seat-2": "hello from seat-2",
	}, replies)
}

func TestSeatAgentCardResolvable(t *testing.T) {
	srv := startTestServer(t, SeatConfig{Name: "seat-1"}, SeatConfig{Name: "seat-2"})

	resolver := agentcard.NewResolver(http.DefaultClient)

	// Default path resolution against the seat endpoint as base URL.
	card, err := resolver.Resolve(t.Context(), srv.SeatEndpoint("seat-1"),
		agentcard.WithRequestHeader("Authorization", "Bearer "+testBearer))
	require.NoError(t, err)

	assert.Equal(t, "cm-mob-seat-1", card.Name)
	assert.Equal(t, "1", card.Version)
	require.Len(t, card.SupportedInterfaces, 1)
	assert.Equal(t, srv.SeatEndpoint("seat-1"), card.SupportedInterfaces[0].URL)
	assert.Equal(t, a2a.TransportProtocolJSONRPC, card.SupportedInterfaces[0].ProtocolBinding)
	assert.Equal(t, []string{"text/plain"}, card.DefaultInputModes)
	assert.Equal(t, []string{"text/plain"}, card.DefaultOutputModes)

	// Explicit WithPath resolution from the server root.
	base := strings.TrimSuffix(srv.SeatEndpoint("seat-2"), "/seats/seat-2")
	card2, err := resolver.Resolve(t.Context(), base,
		agentcard.WithPath("/seats/seat-2"+a2asrv.WellKnownAgentCardPath),
		agentcard.WithRequestHeader("Authorization", "Bearer "+testBearer))
	require.NoError(t, err)
	assert.Equal(t, "cm-mob-seat-2", card2.Name)
}

func TestBearerMiddlewareRejects(t *testing.T) {
	srv := startTestServer(t, SeatConfig{Name: "seat-1"})

	cardURL := srv.SeatEndpoint("seat-1") + a2asrv.WellKnownAgentCardPath

	tests := []struct {
		name   string
		header string
	}{
		{name: "missing authorization", header: ""},
		{name: "wrong token", header: "Bearer wrong-token"},
		{name: "not bearer", header: "Basic " + testBearer},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, cardURL, nil)
			require.NoError(t, err)

			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)

			defer resp.Body.Close()

			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		})
	}

	t.Run("json-rpc endpoint without auth", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			srv.SeatEndpoint("seat-1"), strings.NewReader(`{}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		defer resp.Body.Close()

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("correct token passes", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, cardURL, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+testBearer)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

func TestServerCloseIdempotent(t *testing.T) {
	srv, err := StartServer([]SeatConfig{{Name: "seat-1"}}, echoSeatRunner, testBearer)
	require.NoError(t, err)

	require.NoError(t, srv.Close())
	require.NoError(t, srv.Close())

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		srv.SeatEndpoint("seat-1")+a2asrv.WellKnownAgentCardPath, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testBearer)

	_, err = http.DefaultClient.Do(req) //nolint:bodyclose // request must fail before a body exists
	assert.Error(t, err, "listener must be down after Close")
}

func TestStartServerValidation(t *testing.T) {
	_, err := StartServer(nil, echoSeatRunner, testBearer)
	require.Error(t, err, "no seats")

	_, err = StartServer([]SeatConfig{{Name: "seat-1"}}, echoSeatRunner, "")
	require.Error(t, err, "empty bearer")

	_, err = StartServer([]SeatConfig{{Name: "seat-1"}}, nil, testBearer)
	require.Error(t, err, "nil runner")

	_, err = StartServer([]SeatConfig{{Name: "seat-1"}, {Name: "seat-1"}}, echoSeatRunner, testBearer)
	require.Error(t, err, "duplicate seat name")
}
