package webhook

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/logbridge"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogs_SSEStream(t *testing.T) {
	hub := logbridge.NewHub()

	srv := NewServer(Config{
		APIKey:            testAPIKey,
		Skew:              protocol.DefaultMaxClockSkew,
		Hub:               hub,
		KeepaliveInterval: 40 * time.Millisecond, // shrunk so the test sees a keepalive fast
	})

	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// Signed GET WITH the query string — the query is part of the base string.
	target := ts.URL + "/logs?project=proj"

	req, err := http.NewRequest(http.MethodGet, target, nil)
	require.NoError(t, err)
	require.Equal(t, "/logs?project=proj", req.URL.RequestURI())

	now := nowTS()
	sig := protocol.SignPayloadWithTimestamp(testAPIKey, http.MethodGet, "/logs?project=proj", nil, now)
	req.Header.Set(protocol.SignatureHeader, "sha256="+sig)
	req.Header.Set(protocol.TimestampHeader, now)

	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed below
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	defer func() { _ = resp.Body.Close() }()

	type line struct {
		text string
		err  error
	}

	lines := make(chan line, 32)

	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- line{text: sc.Text()}
		}

		lines <- line{err: sc.Err()}
	}()

	readUntil := func(pred func(string) bool, what string) {
		t.Helper()

		deadline := time.After(3 * time.Second)

		for {
			select {
			case l := <-lines:
				require.NoError(t, l.err, "stream ended before "+what)

				if pred(l.text) {
					return
				}
			case <-deadline:
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	// 1. The connected comment must arrive immediately.
	readUntil(func(s string) bool { return s == ": connected" }, "connected comment")

	// Give the subscription a beat, then publish a frame.
	time.Sleep(10 * time.Millisecond)
	hub.Publish(protocol.LogEntry{
		Timestamp: time.Now(),
		Project:   "proj",
		CardID:    "PROJ-001",
		Type:      "text",
		Content:   "hello from worker",
	})

	// 2. The published frame must arrive as a data: line.
	readUntil(func(s string) bool {
		return strings.HasPrefix(s, "data:") && strings.Contains(s, "hello from worker")
	}, "data frame")

	// 3. A keepalive comment must arrive on the shrunken interval.
	readUntil(func(s string) bool { return s == ": keepalive" }, "keepalive comment")
}

func TestLogs_ProjectFilter(t *testing.T) {
	hub := logbridge.NewHub()

	srv := NewServer(Config{
		APIKey:            testAPIKey,
		Skew:              protocol.DefaultMaxClockSkew,
		Hub:               hub,
		KeepaliveInterval: time.Hour, // no keepalive noise
	})

	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	target := ts.URL + "/logs?project=proj"

	req, err := http.NewRequest(http.MethodGet, target, nil)
	require.NoError(t, err)

	now := nowTS()
	sig := protocol.SignPayloadWithTimestamp(testAPIKey, http.MethodGet, "/logs?project=proj", nil, now)
	req.Header.Set(protocol.SignatureHeader, "sha256="+sig)
	req.Header.Set(protocol.TimestampHeader, now)

	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed below
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	defer func() { _ = resp.Body.Close() }()

	lines := make(chan string, 32)

	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	// Wait for the connected comment.
	require.Eventually(t, func() bool {
		select {
		case l := <-lines:
			return l == ": connected"
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond)

	time.Sleep(10 * time.Millisecond)

	// A frame for a different project must NOT be delivered.
	hub.Publish(protocol.LogEntry{Project: "other", Type: "text", Content: "other-project"})
	// A frame for our project MUST be delivered.
	hub.Publish(protocol.LogEntry{Project: "proj", Type: "text", Content: "our-project"})

	deadline := time.After(2 * time.Second)

	for {
		select {
		case l := <-lines:
			if strings.Contains(l, "other-project") {
				t.Fatal("received a frame for a filtered-out project")
			}

			if strings.Contains(l, "our-project") {
				return // success: our frame arrived, the other did not precede it
			}
		case <-deadline:
			t.Fatal("timed out waiting for our-project frame")
		}
	}
}
