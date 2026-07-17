// Package metrics adapts the shared backendkit metrics bundle to the agent
// service. It embeds the shared bundle (namespaced "cm_agent") and adds the
// agent-only callback-retries counter; the endpoint allowlist and the
// container-outcome enum stay agent-owned.
//
// Label cardinality is bounded on purpose: no card_id / project labels;
// endpoint labels pass through NormalizeEndpoint; container outcome is a fixed
// enum; broadcaster drops are unlabeled.
package metrics

import (
	backendkitmetrics "github.com/mhersson/contextmatrix-backendkit/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Container-exit outcomes for cm_agent_container_duration_seconds.
const (
	OutcomeSuccess     = "success"
	OutcomeFailure     = "failure"
	OutcomeTimeout     = "timeout"
	OutcomeKilled      = "killed"
	OutcomeIdleTimeout = "idle_timeout"
)

// endpointAllowlist enumerates the request paths the agent serves. Any inbound
// path outside this set collapses to "other" via NormalizeEndpoint so a stray
// probe cannot inflate metric label cardinality. Keep this in lockstep with
// webhook.Server.Routes() (the main mux) plus the admin /metrics path.
var endpointAllowlist = []string{
	"/trigger",
	"/kill",
	"/stop-all",
	"/message",
	"/promote",
	"/end-session",
	"/containers",
	"/logs",
	"/health",
	"/readyz",
	"/metrics",
}

// Metrics wraps the shared backendkit bundle, adding the agent-only
// callback-retries counter. The embedded *backendkitmetrics.Metrics promotes
// Registry, the shared collectors, and NormalizeEndpoint, so existing call
// sites keep working through embedding.
type Metrics struct {
	*backendkitmetrics.Metrics
	CallbackRetriesTotal *prometheus.CounterVec
}

// New registers the agent metrics on a fresh registry and returns the bundle.
// The dedicated registry also carries the standard Go runtime + Process
// collectors so /metrics exposes go_* / process_* alongside the cm_agent_*
// series - the dedicated-registry shape would otherwise drop them.
func New() *Metrics {
	base := backendkitmetrics.New("cm_agent", endpointAllowlist)

	return &Metrics{
		Metrics: base,
		CallbackRetriesTotal: promauto.With(base.Registry).NewCounterVec(prometheus.CounterOpts{
			Name: "cm_agent_callback_retries_total",
			Help: "Total ContextMatrix callback retry attempts, labelled by endpoint.",
		}, []string{"endpoint"}),
	}
}
