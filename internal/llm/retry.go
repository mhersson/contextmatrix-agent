package llm

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryPolicy controls client-side retry of transient failures. Zero value =
// no retries (one attempt).
type RetryPolicy struct {
	MaxRetries int           // additional attempts after the first
	BaseDelay  time.Duration // first backoff (doubles each retry)
	MaxDelay   time.Duration // cap per delay (0 = uncapped)
	Jitter     bool          // spread delays to avoid thundering herds
}

// DefaultRetryPolicy is the production default the CLI opts into via WithRetry.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 4, BaseDelay: time.Second, MaxDelay: 30 * time.Second, Jitter: true}
}

func ctxSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	default:
		return false
	}
}

// retryAfter parses a Retry-After header (integer seconds or HTTP-date).
func retryAfter(resp *http.Response) (time.Duration, bool) {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0, false
	}

	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second, true
	}

	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d, true
		}

		return 0, true
	}

	return 0, false
}

func capDelay(d, ceiling time.Duration) time.Duration {
	if d < 0 {
		return 0
	}

	if ceiling > 0 && d > ceiling {
		return ceiling
	}

	return d
}

// backoff computes the delay before retry `attempt` (1-based), honoring a
// Retry-After header on the prior response when present.
func (c *Client) backoff(attempt int, lastResp *http.Response) time.Duration {
	if lastResp != nil {
		if d, ok := retryAfter(lastResp); ok {
			return capDelay(d, c.retry.MaxDelay)
		}
	}

	d := capDelay(c.retry.BaseDelay<<(attempt-1), c.retry.MaxDelay)
	if c.retry.Jitter && d > 1 {
		half := d / 2
		d = half + time.Duration(rand.Int64N(int64(half)+1)) //nolint:gosec // jitter, not security
	}

	return d
}
