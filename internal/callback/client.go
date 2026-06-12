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
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
)

const (
	maxAttempts    = 3
	requestTimeout = 10 * time.Second
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
	apiKey  string // HMAC key — the agent backend entry's api_key
	http    *http.Client
	logger  *slog.Logger

	// delays holds the per-attempt backoff delays. Set to zeros in tests to
	// avoid sleeping. Index i is the delay before the (i+1)-th attempt; the
	// first attempt (i=0) never sleeps.
	delays [maxAttempts]time.Duration
}

// New creates a new Client. baseURL must not have a trailing slash; if it
// does, it is trimmed. A nil logger is replaced with slog.Default().
func New(baseURL, apiKey string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}

	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: requestTimeout},
		logger:  logger,
		delays:  defaultDelays,
	}
}

// ReportStatus posts runner_status ∈ running|completed|failed to
// /api/agent/status (the path ContextMatrix mounts for the agent backend).
func (c *Client) ReportStatus(ctx context.Context, cardID, project, status, message string) error {
	payload := protocol.StatusCallbackPayload{
		CardID:       cardID,
		Project:      project,
		RunnerStatus: status,
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
		}

		// Each attempt uses a fresh timestamp (and therefore a fresh signature)
		// so the receiver's replay cache treats every attempt as distinct.
		sig, ts := protocol.SignRequestHeaders(c.apiKey, http.MethodPost, uri, body)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create status request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(protocol.SignatureHeader, sig)
		req.Header.Set(protocol.TimestampHeader, ts)

		lastErr = c.doRoundTrip(req)
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

	return fmt.Errorf("status callback failed after %d attempts: %w", maxAttempts, lastErr)
}

// VerifyAutonomous confirms the card's autonomous flag before a promote
// frame is written — fail closed: any error means "do not promote".
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

// ctxSleep sleeps for d or until ctx is cancelled.
func ctxSleep(ctx context.Context, d time.Duration) error {
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
