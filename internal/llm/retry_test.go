package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetryRecoversAfter429HonoringRetryAfter(t *testing.T) {
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"rate limited"}}`) //nolint:errcheck

			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\ndata: [DONE]\n") //nolint:errcheck
	}))
	defer srv.Close()

	var delays []time.Duration

	c := NewClient("k", WithBaseURL(srv.URL),
		WithRetry(RetryPolicy{MaxRetries: 4, BaseDelay: time.Second, MaxDelay: 30 * time.Second, Jitter: false}))
	c.sleep = func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)

		return nil
	}

	resp, err := c.SendStream(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}}, nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls))
	require.Len(t, delays, 2)
	assert.Equal(t, 2*time.Second, delays[0])
	assert.Equal(t, 2*time.Second, delays[1])
}

func TestRetryExponentialBackoffWithoutRetryAfter(t *testing.T) {
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "boom") //nolint:errcheck
	}))
	defer srv.Close()

	var delays []time.Duration

	c := NewClient("k", WithBaseURL(srv.URL),
		WithRetry(RetryPolicy{MaxRetries: 3, BaseDelay: time.Second, MaxDelay: 30 * time.Second, Jitter: false}))
	c.sleep = func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)

		return nil
	}

	_, err := c.Send(context.Background(), Request{Messages: []Message{{Role: "user"}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.Equal(t, int32(4), atomic.LoadInt32(&calls)) // 1 + 3 retries
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, delays)
}

func TestNoRetryByDefault(t *testing.T) {
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "rate limited") //nolint:errcheck
	}))
	defer srv.Close()

	c := NewClient("k", WithBaseURL(srv.URL)) // default: no retry
	_, err := c.SendStream(context.Background(), Request{Messages: []Message{{Role: "user"}}}, nil)
	require.Error(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls))
}
