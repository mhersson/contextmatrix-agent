package metrics_test

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/metrics"
)

func TestNew_RegistersAllMetrics(t *testing.T) {
	m := metrics.New()
	require.NotNil(t, m)

	// Touch each collector so it appears in the registry output.
	m.WebhookRequestsTotal.WithLabelValues("trigger", "200", "success").Inc()
	m.WebhookRequestDuration.WithLabelValues("trigger").Observe(0.1)
	m.ContainerDuration.WithLabelValues(metrics.OutcomeSuccess).Observe(30)
	m.RunningContainers.Set(1)
	m.CallbackRetriesTotal.WithLabelValues("status").Inc()
	m.BroadcasterDropsTotal.Inc()

	families, err := m.Registry.Gather()
	require.NoError(t, err)

	got := make(map[string]bool, len(families))
	for _, f := range families {
		got[f.GetName()] = true
	}

	want := []string{
		"cm_agent_webhook_requests_total",
		"cm_agent_webhook_request_duration_seconds",
		"cm_agent_container_duration_seconds",
		"cm_agent_running_containers",
		"cm_agent_callback_retries_total",
		"cm_agent_broadcaster_drops_total",
	}

	for _, name := range want {
		assert.True(t, got[name], "metric %q not registered", name)
	}
}

func TestNew_RegistersGoAndProcessCollectors(t *testing.T) {
	m := metrics.New()
	require.NotNil(t, m)

	families, err := m.Registry.Gather()
	require.NoError(t, err)

	want := map[string]bool{
		"go_goroutines":           false,
		"go_memstats_alloc_bytes": false,
	}

	if runtime.GOOS == "linux" {
		want["process_cpu_seconds_total"] = false
		want["process_resident_memory_bytes"] = false
	}

	for _, f := range families {
		if _, ok := want[f.GetName()]; ok {
			want[f.GetName()] = true
		}
	}

	for name, seen := range want {
		assert.True(t, seen, "expected runtime/process series %q to be registered", name)
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	m := metrics.New()

	allowed := []string{
		"/trigger", "/kill", "/stop-all", "/message", "/promote",
		"/end-session", "/containers", "/logs", "/health", "/readyz", "/metrics",
	}
	for _, p := range allowed {
		assert.Equal(t, p, m.NormalizeEndpoint(p), "allowlisted path %q must round-trip", p)
	}

	unknown := []string{"/nonexistent", "/trigger/extra", "", "/", "/TRIGGER"}
	for _, p := range unknown {
		assert.Equal(t, "other", m.NormalizeEndpoint(p), "unknown path %q must collapse", p)
	}
}
