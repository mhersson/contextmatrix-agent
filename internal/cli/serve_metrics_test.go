package cli

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/mhersson/contextmatrix-agent/internal/executor"
	"github.com/mhersson/contextmatrix-agent/internal/metrics"
	"github.com/mhersson/contextmatrix-agent/internal/webhook"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBuildAdminServer_DisabledWhenZero(t *testing.T) {
	cfg := &config.ServiceConfig{AdminPort: 0}
	srv := webhook.NewServer(webhook.Config{APIKey: "k"})

	assert.Nil(t, buildAdminServer(cfg, srv, metrics.New(), testLogger()))
}

func TestBuildAdminServer_LoopbackAddr(t *testing.T) {
	cfg := &config.ServiceConfig{AdminPort: 9093}
	srv := webhook.NewServer(webhook.Config{APIKey: "k"})

	admin := buildAdminServer(cfg, srv, metrics.New(), testLogger())
	require.NotNil(t, admin)
	assert.Equal(t, "127.0.0.1:9093", admin.Addr, "empty bind addr defaults to loopback")
}

func TestBuildAdminServer_CustomBindAddr(t *testing.T) {
	cfg := &config.ServiceConfig{AdminPort: 9093, AdminBindAddr: "0.0.0.0"}
	srv := webhook.NewServer(webhook.Config{APIKey: "k"})

	admin := buildAdminServer(cfg, srv, metrics.New(), testLogger())
	require.NotNil(t, admin)
	assert.Equal(t, "0.0.0.0:9093", admin.Addr)
}

func TestStartRunningContainersGauge_TracksCount(t *testing.T) {
	tracker := executor.NewTracker(4)
	require.True(t, tracker.AddIfUnderLimit(&executor.Run{Project: "p", CardID: "C-1"}))

	m := metrics.New()

	stop := startRunningContainersGauge(tracker, m, testLogger(), 5*time.Millisecond)
	defer stop()

	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(m.RunningContainers) == 1
	}, time.Second, 5*time.Millisecond, "gauge should reflect the tracked count")
}
