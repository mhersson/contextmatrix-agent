package callback

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/metrics"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newFastClient returns a Client with zero backoff delays so retry tests run
// without sleeping.
func newFastClient(baseURL, apiKey string) *Client {
	c := New(baseURL, apiKey, testLogger())
	c.delays = [maxAttempts]time.Duration{}

	return c
}

// TestReportStatus_Success verifies that ReportStatus POSTs to
// /api/agent/status with a valid HMAC signature and correct payload fields.
func TestReportStatus_Success(t *testing.T) {
	apiKey := "test-secret-key-that-is-long-enough"

	var (
		receivedMethod          string
		receivedPath            string
		receivedSig, receivedTS string
		receivedPayload         protocol.StatusCallbackPayload
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.Path
		receivedSig = r.Header.Get(protocol.SignatureHeader)
		receivedTS = r.Header.Get(protocol.TimestampHeader)

		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		if !assert.NoError(t, json.Unmarshal(body, &receivedPayload)) {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		// Verify signature using the protocol helper — strip the "sha256=" prefix.
		sig := strings.TrimPrefix(receivedSig, "sha256=")
		ok := protocol.VerifySignatureWithTimestamp(
			apiKey, r.Method, r.URL.RequestURI(),
			sig, receivedTS, body, protocol.DefaultMaxClockSkew, nil,
		)
		assert.True(t, ok, "HMAC signature verification failed")

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newFastClient(srv.URL, apiKey)

	err := c.ReportStatus(context.Background(), "CMX-001", "alpha", "running", "task started")
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, receivedMethod)
	assert.Equal(t, "/api/agent/status", receivedPath)
	assert.NotEmpty(t, receivedSig)
	assert.NotEmpty(t, receivedTS)
	assert.Equal(t, "CMX-001", receivedPayload.CardID)
	assert.Equal(t, "alpha", receivedPayload.Project)
	assert.Equal(t, "running", receivedPayload.RunnerStatus)
	assert.Equal(t, "task started", receivedPayload.Message)
}

// TestReportStatus_Retries verifies that 5xx responses are retried (up to
// maxAttempts) while 4xx responses are not retried (fail immediately).
func TestReportStatus_Retries(t *testing.T) {
	t.Run("five_xx_retried_three_times", func(t *testing.T) {
		apiKey := "retry-test-key"

		var attempts int32

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := atomic.AddInt32(&attempts, 1)
			if n < 3 {
				w.WriteHeader(http.StatusInternalServerError)

				return
			}

			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := newFastClient(srv.URL, apiKey)

		err := c.ReportStatus(context.Background(), "CMX-001", "proj", "running", "")
		require.NoError(t, err)

		assert.Equal(t, int32(3), atomic.LoadInt32(&attempts), "expected exactly 3 attempts")
	})

	t.Run("four_xx_not_retried", func(t *testing.T) {
		apiKey := "no-retry-key"

		var attempts int32

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			w.WriteHeader(http.StatusBadRequest)
		}))
		defer srv.Close()

		c := newFastClient(srv.URL, apiKey)

		err := c.ReportStatus(context.Background(), "CMX-001", "proj", "running", "")
		require.Error(t, err)

		assert.Equal(t, int32(1), atomic.LoadInt32(&attempts), "4xx must not be retried")
	})
}

// TestVerifyAutonomous_True verifies that a card with autonomous=true is
// decoded correctly.
func TestVerifyAutonomous_True(t *testing.T) {
	apiKey := "autonomous-test-key"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/cards/alpha/CMX-001/autonomous", r.URL.Path)

		// Verify HMAC signature over an empty body.
		sig := strings.TrimPrefix(r.Header.Get(protocol.SignatureHeader), "sha256=")
		ts := r.Header.Get(protocol.TimestampHeader)
		ok := protocol.VerifySignatureWithTimestamp(
			apiKey, r.Method, r.URL.RequestURI(),
			sig, ts, nil, protocol.DefaultMaxClockSkew, nil,
		)
		assert.True(t, ok, "HMAC signature verification failed for GET")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"autonomous": true}`))
	}))
	defer srv.Close()

	c := newFastClient(srv.URL, apiKey)

	auto, err := c.VerifyAutonomous(context.Background(), "alpha", "CMX-001")
	require.NoError(t, err)
	assert.True(t, auto)
}

// TestVerifyAutonomous_NonOK verifies that a non-200 response from CM results
// in an error (fail-closed).
func TestVerifyAutonomous_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newFastClient(srv.URL, "any-key")

	auto, err := c.VerifyAutonomous(context.Background(), "proj", "CMX-001")
	require.Error(t, err, "non-200 must return an error (fail-closed)")
	assert.False(t, auto)
}

// TestVerifyAutonomous_MalformedBody verifies that a malformed JSON body
// results in an error.
func TestVerifyAutonomous_MalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := newFastClient(srv.URL, "any-key")

	auto, err := c.VerifyAutonomous(context.Background(), "proj", "CMX-001")
	require.Error(t, err, "malformed body must return an error")
	assert.False(t, auto)
}

func TestReportStatus_CountsRetries(t *testing.T) {
	apiKey := "test-secret-key-that-is-long-enough"

	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)

		w.WriteHeader(http.StatusInternalServerError) // 5xx is retryable
	}))
	defer srv.Close()

	m := metrics.New()
	c := newFastClient(srv.URL, apiKey).WithMetrics(m)

	err := c.ReportStatus(context.Background(), "C-1", "proj", "running", "")
	require.Error(t, err, "all attempts fail")

	assert.Equal(t, int32(3), hits.Load(), "three attempts were made")
	assert.InEpsilon(t, float64(2), testutil.ToFloat64(m.CallbackRetriesTotal.WithLabelValues("status")), 1e-9,
		"two retries follow the first attempt")
}
