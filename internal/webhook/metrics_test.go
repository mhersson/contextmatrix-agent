package webhook

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-agent/internal/metrics"
)

func TestRecordMetrics_CountsAndLabels(t *testing.T) {
	m := metrics.New()
	s := NewServer(Config{APIKey: testAPIKey, Metrics: m})

	h := s.recordMetrics(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	h(httptest.NewRecorder(), r)

	got := testutil.ToFloat64(m.WebhookRequestsTotal.WithLabelValues("health", "200", "success"))
	assert.InEpsilon(t, float64(1), got, 1e-9)
	assert.Equal(t, 1, testutil.CollectAndCount(m.WebhookRequestDuration), "duration histogram should have a series")
}

func TestRecordMetrics_UnknownPathCollapses(t *testing.T) {
	m := metrics.New()
	s := NewServer(Config{APIKey: testAPIKey, Metrics: m})

	h := s.recordMetrics(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/secret-probe", nil))

	got := testutil.ToFloat64(m.WebhookRequestsTotal.WithLabelValues("other", "200", "success"))
	assert.InEpsilon(t, float64(1), got, 1e-9)
}

func TestRecordMetrics_RateLimitedCode(t *testing.T) {
	m := metrics.New()
	s := NewServer(Config{APIKey: testAPIKey, Metrics: m})

	h := s.recordMetrics(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/trigger", nil))

	got := testutil.ToFloat64(m.WebhookRequestsTotal.WithLabelValues("trigger", "429", "rate_limited"))
	assert.InEpsilon(t, float64(1), got, 1e-9)
}

func TestRecordMetrics_NilMetricsPassThrough(t *testing.T) {
	s := NewServer(Config{APIKey: testAPIKey}) // no Metrics

	var ran bool

	h := s.recordMetrics(func(w http.ResponseWriter, _ *http.Request) {
		ran = true

		w.WriteHeader(http.StatusOK)
	})

	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/health", nil))

	assert.True(t, ran, "nil metrics must pass through to the handler")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAdminAuth_RejectsUnauthenticatedAcceptsSigned(t *testing.T) {
	s := NewServer(Config{APIKey: testAPIKey, Skew: protocol.DefaultMaxClockSkew})

	h := s.AdminAuth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("# metrics"))
	})

	// Unauthenticated GET /metrics: rejected.
	w1 := httptest.NewRecorder()
	h(w1, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusUnauthorized, w1.Code)

	// Correctly signed GET /metrics: accepted.
	r2 := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	signReq(t, r2, testAPIKey, nil, nowTS())

	w2 := httptest.NewRecorder()
	h(w2, r2)
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Contains(t, w2.Body.String(), "# metrics")
}
