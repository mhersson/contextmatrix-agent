package executor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

// stubResolver returns canned results so buildExtraHosts is testable without
// touching real DNS.
type stubResolver struct {
	addrs []string
	err   error
}

func (s stubResolver) LookupHost(_ context.Context, _ string) ([]string, error) {
	return s.addrs, s.err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBuildExtraHosts(t *testing.T) {
	const gateway = "host.docker.internal:host-gateway"

	tests := []struct {
		name     string
		resolver hostResolver
		mcpURL   string
		want     []string
	}{
		{
			name:     "resolvable hostname is pinned",
			resolver: stubResolver{addrs: []string{"192.168.1.50"}},
			mcpURL:   "https://cm.lan.example/mcp",
			want:     []string{gateway, "cm.lan.example:192.168.1.50"},
		},
		{
			name:     "unresolvable hostname falls back to gateway only",
			resolver: stubResolver{err: errors.New("no such host")},
			mcpURL:   "https://cm.lan.example/mcp",
			want:     []string{gateway},
		},
		{
			name:     "zero addresses falls back to gateway only",
			resolver: stubResolver{addrs: nil},
			mcpURL:   "https://cm.lan.example/mcp",
			want:     []string{gateway},
		},
		{
			name:     "ip host needs no mapping",
			resolver: stubResolver{addrs: []string{"10.0.0.1"}},
			mcpURL:   "https://10.0.0.9:8080/mcp",
			want:     []string{gateway},
		},
		{
			name:     "localhost needs no mapping",
			resolver: stubResolver{addrs: []string{"127.0.0.1"}},
			mcpURL:   "http://localhost:8080/mcp",
			want:     []string{gateway},
		},
		{
			name:     "host.docker.internal needs no extra mapping",
			resolver: stubResolver{addrs: []string{"172.17.0.1"}},
			mcpURL:   "http://host.docker.internal:8080/mcp",
			want:     []string{gateway},
		},
		{
			name:     "empty url yields gateway only",
			resolver: stubResolver{addrs: []string{"1.2.3.4"}},
			mcpURL:   "",
			want:     []string{gateway},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildExtraHosts(tt.resolver, tt.mcpURL, discardLogger())
			assert.Equal(t, tt.want, got)
		})
	}
}
