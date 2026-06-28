package executor

import (
	"context"
	"log/slog"
	"net"
	"net/url"
	"time"
)

// dnsLookupTimeout bounds the host-side MCP hostname resolution in
// buildExtraHosts so a slow or unreachable resolver cannot stall a launch.
const dnsLookupTimeout = 2 * time.Second

// hostResolver is the subset of *net.Resolver that buildExtraHosts needs. It
// is an interface so tests can inject a deterministic resolver.
type hostResolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// buildExtraHosts returns the extra /etc/hosts entries for a worker container.
// It always maps host.docker.internal to the host gateway. When the MCP URL
// carries a DNS name that resolves on the host — e.g. via the host's own
// /etc/hosts — that name is pinned to its resolved address so the container,
// which does not inherit the host's /etc/hosts, can reach the MCP server too.
// This is what lets a worker connect to an MCP endpoint like
// https://cm.lan.example/mcp that only resolves through the host's /etc/hosts.
//
// The lookup is bounded by dnsLookupTimeout. Any failure (parse error, IP host,
// timeout, NXDOMAIN, zero addresses) returns the default entry alone — the
// container can still reach MCP if in-container DNS resolves the name itself.
func buildExtraHosts(resolver hostResolver, mcpURL string, log *slog.Logger) []string {
	hosts := []string{"host.docker.internal:host-gateway"}

	u, err := url.Parse(mcpURL)
	if err != nil || u.Hostname() == "" {
		return hosts
	}

	hostname := u.Hostname()
	// An IP needs no mapping; localhost and host.docker.internal are already
	// reachable (the latter via the host-gateway entry above).
	if net.ParseIP(hostname) != nil || hostname == "localhost" || hostname == "host.docker.internal" {
		return hosts
	}

	if resolver == nil {
		resolver = net.DefaultResolver
	}

	// Resolve under a fresh background context: a near-cancelled launch ctx must
	// not shorten the deadline below the already-tight cap.
	ctx, cancel := context.WithTimeout(context.Background(), dnsLookupTimeout)
	defer cancel()

	addrs, err := resolver.LookupHost(ctx, hostname)
	if err != nil || len(addrs) == 0 {
		log.Warn("could not resolve MCP hostname on the host; container will rely on in-container DNS",
			"hostname", hostname, "error", err)

		return hosts
	}

	hosts = append(hosts, hostname+":"+addrs[0])
	log.Info("pinned MCP host for container", "hostname", hostname, "ip", addrs[0])

	return hosts
}
