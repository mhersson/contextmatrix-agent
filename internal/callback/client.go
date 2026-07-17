// Package callback is the service's client for ContextMatrix's backend
// callback endpoints: status updates and the fail-closed promote
// verification.
package callback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-agent/internal/metrics"
)

const (
	maxAttempts    = 3
	requestTimeout = 10 * time.Second

	// backgroundBaseDelay and backgroundMaxDelay bound the exponential
	// backoff a TERMINAL (completed/failed) status callback falls back to
	// once the fast synchronous attempts above are exhausted: 5s, 10s, 20s,
	// 40s, 80s, then capped at 2m - retried indefinitely until it succeeds,
	// is rejected with a non-retryable 4xx, or the client is Closed (process
	// shutdown). A terminal callback is the only signal that clears a card's
	// parent claim / worker_status in ContextMatrix, so it must not be
	// silently dropped by a brief CM outage.
	backgroundBaseDelay = 5 * time.Second
	backgroundMaxDelay  = 2 * time.Minute
)

// defaultDelays holds the exponential-backoff delays for each retry attempt
// (0-indexed: delay before attempt 1, 2, …). The first attempt needs no
// delay, so index 0 is zero; only indices 1 and 2 are used by the retry
// loop (the loop exits after the final attempt without sleeping).
var defaultDelays = [maxAttempts]time.Duration{
	0,
	1 * time.Second,
	2 * time.Second,
}

// Client sends HMAC-signed callbacks to ContextMatrix.
type Client struct {
	baseURL string // no trailing slash
	apiKey  string // HMAC key - the agent backend entry's api_key
	http    *http.Client
	logger  *slog.Logger

	// delays holds the per-attempt backoff delays. Set to zeros in tests to
	// avoid sleeping. Index i is the delay before the (i+1)-th attempt; the
	// first attempt (i=0) never sleeps.
	delays [maxAttempts]time.Duration

	// metrics counts retry attempts. Nil disables counting.
	metrics *metrics.Metrics

	// bgCtx/bgCancel bound every persistent background retry goroutine;
	// Close cancels bgCtx and waits for them to exit so none survive past
	// process shutdown. bgMu serializes scheduleBackgroundRetry's
	// check-then-bgWG.Add against Close's cancel-then-bgWG.Wait so a retry
	// can never be scheduled after Close has started waiting (sync.WaitGroup
	// forbids racing Add against Wait when the counter may be zero).
	bgCtx    context.Context //nolint:containedctx // bounds background goroutines started after construction, not a request context
	bgCancel context.CancelFunc
	bgMu     sync.Mutex
	bgWG     sync.WaitGroup

	// backgroundDelay computes the delay before the (attempt+1)-th
	// background retry (0-indexed). Overridable in tests to avoid sleeping.
	backgroundDelay func(attempt int) time.Duration

	// bgAttempted, if non-nil, is signaled once per background retry attempt
	// - a test synchronization hook, never set in production.
	bgAttempted chan struct{}
}

// New creates a new Client. baseURL must not have a trailing slash; if it
// does, it is trimmed. A nil logger is replaced with slog.Default().
func New(baseURL, apiKey string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())

	return &Client{
		baseURL:         strings.TrimRight(baseURL, "/"),
		apiKey:          apiKey,
		http:            &http.Client{Timeout: requestTimeout},
		logger:          logger,
		delays:          defaultDelays,
		bgCtx:           bgCtx,
		bgCancel:        bgCancel,
		backgroundDelay: defaultBackgroundDelay,
	}
}

// Close cancels every in-flight persistent background retry and blocks until
// they exit. Callers must invoke it during graceful shutdown so no
// background-retry goroutine outlives the process.
func (c *Client) Close() {
	c.bgMu.Lock()
	c.bgCancel()
	c.bgMu.Unlock()

	c.bgWG.Wait()
}

// defaultBackgroundDelay is the production backoff schedule: doubles from
// backgroundBaseDelay, capped at backgroundMaxDelay.
func defaultBackgroundDelay(attempt int) time.Duration {
	if attempt < 0 || attempt > 6 { // 5s*2^6=320s already exceeds the cap
		return backgroundMaxDelay
	}

	d := backgroundBaseDelay * time.Duration(int64(1)<<uint(attempt))
	if d > backgroundMaxDelay {
		return backgroundMaxDelay
	}

	return d
}

// isTerminalStatus reports whether status is a terminal worker-status
// (completed/failed) - the only callbacks that clear a card's parent claim
// and worker_status in ContextMatrix. "running" is superseded by the next
// status update, so it keeps the fast, bounded retry only.
func isTerminalStatus(status string) bool {
	return status == "completed" || status == "failed"
}

// WithMetrics attaches a metrics bundle so retry attempts are counted. Returns
// the client for chaining. A nil bundle leaves counting disabled.
func (c *Client) WithMetrics(m *metrics.Metrics) *Client {
	c.metrics = m

	return c
}

// ReportStatus posts worker_status ∈ running|completed|failed to
// /api/agent/status (the path ContextMatrix mounts for the agent backend).
func (c *Client) ReportStatus(ctx context.Context, cardID, project, status, message string) error {
	payload := protocol.StatusCallbackPayload{
		CardID:       cardID,
		Project:      project,
		WorkerStatus: status,
		Message:      message,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal status payload: %w", err)
	}

	const path = "/api/agent/status"

	uri, err := deriveURI(c.baseURL + path)
	if err != nil {
		return err
	}

	var lastErr error

	for attempt := range maxAttempts {
		if attempt > 0 {
			if err := ctxSleep(ctx, c.delays[attempt]); err != nil {
				return err
			}

			if c.metrics != nil {
				c.metrics.CallbackRetriesTotal.WithLabelValues("status").Inc()
			}
		}

		lastErr = c.sendStatusOnce(ctx, uri, path, body)
		if lastErr == nil {
			return nil
		}

		if isClientError(lastErr) {
			return lastErr
		}

		c.logger.Warn("status callback failed, retrying",
			"attempt", attempt+1,
			"card_id", cardID,
			"error", lastErr,
		)
	}

	if isTerminalStatus(status) {
		c.scheduleBackgroundRetry(cardID, project, status, uri, path, body)
	}

	return fmt.Errorf("status callback failed after %d attempts: %w", maxAttempts, lastErr)
}

// sendStatusOnce executes a single signed POST attempt for the status
// payload. A fresh timestamp (and therefore a fresh signature) is generated
// on every call so the receiver's replay cache treats every attempt -
// synchronous or background - as distinct.
func (c *Client) sendStatusOnce(ctx context.Context, uri, path string, body []byte) error {
	sig, ts := protocol.SignRequestHeaders(c.apiKey, http.MethodPost, uri, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create status request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocol.SignatureHeader, sig)
	req.Header.Set(protocol.TimestampHeader, ts)

	return c.doRoundTrip(req)
}

// scheduleBackgroundRetry starts a goroutine that keeps retrying a failed
// terminal status callback with exponential backoff until it succeeds, is
// rejected with a non-retryable 4xx, or the client is Closed. CM's
// worker-status handler safely re-applies the same idempotent field writes
// for a duplicate terminal status, so replaying the same payload is safe.
func (c *Client) scheduleBackgroundRetry(cardID, project, status, uri, path string, body []byte) {
	c.bgMu.Lock()

	select {
	case <-c.bgCtx.Done():
		c.bgMu.Unlock()

		return
	default:
	}

	c.bgWG.Add(1)
	c.bgMu.Unlock()

	go func() {
		defer c.bgWG.Done()

		for attempt := 0; ; attempt++ {
			if err := ctxSleep(c.bgCtx, c.backgroundDelay(attempt)); err != nil {
				return // client Closed / process shutting down
			}

			err := c.sendStatusOnce(c.bgCtx, uri, path, body)

			if c.metrics != nil {
				c.metrics.CallbackRetriesTotal.WithLabelValues("status-background").Inc()
			}

			if c.bgAttempted != nil {
				select {
				case c.bgAttempted <- struct{}{}:
				case <-c.bgCtx.Done():
					return
				}
			}

			if err == nil {
				c.logger.Info("terminal status callback recovered via background retry",
					"card_id", cardID, "project", project, "status", status, "attempt", attempt+1)

				return
			}

			if isClientError(err) {
				c.logger.Error("terminal status callback rejected, giving up background retry",
					"card_id", cardID, "project", project, "status", status, "error", err)

				return
			}

			c.logger.Warn("terminal status callback still failing in background",
				"card_id", cardID, "project", project, "status", status, "attempt", attempt+1, "error", err)
		}
	}()
}

// VerifyAutonomous confirms the card's autonomous flag before a promote
// frame is written - fail closed: any error means "do not promote".
// The GET is signed with an empty body.
func (c *Client) VerifyAutonomous(ctx context.Context, project, cardID string) (bool, error) {
	path := fmt.Sprintf("/api/v1/cards/%s/%s/autonomous",
		url.PathEscape(project),
		url.PathEscape(cardID),
	)

	reqURL := c.baseURL + path

	uri, err := deriveURI(reqURL)
	if err != nil {
		return false, err
	}

	var lastErr error

	for attempt := range maxAttempts {
		if attempt > 0 {
			if err := ctxSleep(ctx, c.delays[attempt]); err != nil {
				return false, err
			}

			if c.metrics != nil {
				c.metrics.CallbackRetriesTotal.WithLabelValues("verify-autonomous").Inc()
			}
		}

		// Sign with empty body; each attempt uses a fresh timestamp.
		sig, ts := protocol.SignRequestHeaders(c.apiKey, http.MethodGet, uri, nil)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return false, fmt.Errorf("create verify-autonomous request: %w", err)
		}

		req.Header.Set(protocol.SignatureHeader, sig)
		req.Header.Set(protocol.TimestampHeader, ts)

		autonomous, err := c.doVerifyAutonomous(req)
		if err == nil {
			return autonomous, nil
		}

		lastErr = err

		if isClientError(err) {
			return false, err
		}

		c.logger.Warn("verify-autonomous callback failed, retrying",
			"attempt", attempt+1,
			"project", project,
			"card_id", cardID,
			"error", err,
		)
	}

	return false, fmt.Errorf("verify-autonomous callback failed after %d attempts: %w", maxAttempts, lastErr)
}

// doVerifyAutonomous executes a single signed GET request and decodes the
// autonomous boolean from the response body.
func (c *Client) doVerifyAutonomous(req *http.Request) (bool, error) {
	resp, err := c.http.Do(req) //nolint:gosec // G704: baseURL is operator-supplied config, not user input
	if err != nil {
		return false, fmt.Errorf("send verify-autonomous request: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, fmt.Errorf("read verify-autonomous response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return false, &callbackError{statusCode: resp.StatusCode}
	}

	var out struct {
		Autonomous bool `json:"autonomous"`
	}

	if err := json.Unmarshal(body, &out); err != nil {
		return false, fmt.Errorf("parse verify-autonomous response: %w", err)
	}

	return out.Autonomous, nil
}

// doRoundTrip executes a pre-built request and returns any error, mapping
// non-2xx responses to a callbackError.
func (c *Client) doRoundTrip(req *http.Request) error {
	resp, err := c.http.Do(req) //nolint:gosec // G704: baseURL is operator-supplied config, not user input
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)); err != nil {
		return fmt.Errorf("drain response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return &callbackError{statusCode: resp.StatusCode}
	}

	return nil
}

// callbackError is returned for non-2xx upstream responses.
type callbackError struct {
	statusCode int
}

func (e *callbackError) Error() string {
	return fmt.Sprintf("callback returned status %d", e.statusCode)
}

// isClientError reports whether err represents a 4xx response or a
// context cancellation/deadline. Both are non-retryable.
func isClientError(err error) bool {
	if err == nil {
		return false
	}

	if ce, ok := err.(*callbackError); ok { //nolint:errorlint
		return ce.statusCode >= 400 && ce.statusCode < 500
	}

	return false
}

// deriveURI returns the request-target (path + raw query) for fullURL.
// Sender and receiver must agree on the signed value.
func deriveURI(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse url %q: %w", rawURL, err)
	}

	if u.Path == "" {
		u.Path = "/"
	}

	return u.RequestURI(), nil
}

// ctxSleep sleeps for d or until ctx is cancelled. Cancellation is honored
// even for d <= 0 so a zero-delay retry loop can never spin past a cancelled
// context.
func ctxSleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if d <= 0 {
		return nil
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
