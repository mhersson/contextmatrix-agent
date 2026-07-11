package coop

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Server hosts every internal seat behind one loopback JSON-RPC listener.
// The port lives inside the worker container's network namespace and is
// never published; a static bearer guards it anyway (cheap, uniform with
// guests). Task state is a2asrv's default in-memory store — discussions are
// ephemeral by design.
type Server struct {
	base string
	srv  *http.Server

	once     sync.Once
	closeErr error
}

// StartServer binds 127.0.0.1:0 and mounts, per seat:
//
//	/seats/<name>                             — a2a JSON-RPC endpoint
//	/seats/<name>/.well-known/agent-card.json — static agent card
//
// wrapped in constant-time bearer middleware. The caller owns the lifecycle:
// Close when the discussion ends.
func StartServer(seats []SeatConfig, runner SeatRunner, bearer string) (*Server, error) {
	if len(seats) == 0 {
		return nil, errors.New("coop: start server: no seats")
	}

	if bearer == "" {
		return nil, errors.New("coop: start server: empty bearer token")
	}

	if runner == nil {
		return nil, errors.New("coop: start server: nil seat runner")
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("coop: listen loopback: %w", err)
	}

	base := "http://" + ln.Addr().String()
	mux := http.NewServeMux()
	seen := make(map[string]bool, len(seats))

	for _, seat := range seats {
		if seat.Name == "" || seen[seat.Name] {
			_ = ln.Close() //nolint:errcheck // best-effort cleanup on the error path

			return nil, fmt.Errorf("coop: start server: empty or duplicate seat name %q", seat.Name)
		}

		seen[seat.Name] = true
		endpoint := base + "/seats/" + seat.Name

		card := &a2a.AgentCard{
			Name:    "cm-coop-" + seat.Name,
			Version: "1",
			SupportedInterfaces: []*a2a.AgentInterface{
				a2a.NewAgentInterface(endpoint, a2a.TransportProtocolJSONRPC),
			},
			DefaultInputModes:  []string{"text/plain"},
			DefaultOutputModes: []string{"text/plain"},
		}

		handler := a2asrv.NewHandler(newSeatExecutor(seat, runner))
		mux.Handle("/seats/"+seat.Name, a2asrv.NewJSONRPCHandler(handler))
		mux.Handle("/seats/"+seat.Name+a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	}

	s := &Server{
		base: base,
		srv: &http.Server{
			Handler:           bearerMiddleware(bearer, mux),
			ReadHeaderTimeout: 10 * time.Second,
		},
	}

	go func() {
		_ = s.srv.Serve(ln) //nolint:errcheck // returns ErrServerClosed after Shutdown
	}()

	return s, nil
}

// SeatEndpoint returns the JSON-RPC endpoint for one seat name.
func (s *Server) SeatEndpoint(name string) string {
	return s.base + "/seats/" + name
}

// Close shuts the listener down. Idempotent — repeated calls return the
// first result.
func (s *Server) Close() error {
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		s.closeErr = s.srv.Shutdown(ctx)
	})

	return s.closeErr
}

// bearerMiddleware rejects any request whose Authorization header is not
// exactly "Bearer <token>", comparing in constant time.
func bearerMiddleware(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}

		next.ServeHTTP(w, r)
	})
}
