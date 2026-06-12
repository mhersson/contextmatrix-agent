package webhook

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAPIKey = "test-secret-key"

// signReq stamps r with a valid HMAC signature for the given key over
// METHOD\nURI\nTS.BODY using the supplied timestamp.
func signReq(t *testing.T, r *http.Request, key string, body []byte, ts string) {
	t.Helper()

	sig := protocol.SignPayloadWithTimestamp(key, r.Method, r.URL.RequestURI(), body, ts)
	r.Header.Set(protocol.SignatureHeader, "sha256="+sig)
	r.Header.Set(protocol.TimestampHeader, ts)
}

// nowTS returns the current Unix second as a string.
func nowTS() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

// echoHandler is a trivial next-handler that records it ran and echoes the
// re-injected body so tests can assert body re-injection.
func echoHandler(ran *bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*ran = true
		body, _ := io.ReadAll(r.Body)

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

func newAuthServer(t *testing.T) *Server {
	t.Helper()

	return NewServer(Config{APIKey: testAPIKey, Skew: protocol.DefaultMaxClockSkew})
}

func TestAuth_ValidPOSTPasses(t *testing.T) {
	s := newAuthServer(t)
	body := []byte(`{"hello":"world"}`)

	var ran bool

	h := s.auth(echoHandler(&ran))

	r := httptest.NewRequest(http.MethodPost, "/trigger", strings.NewReader(string(body)))
	signReq(t, r, testAPIKey, body, nowTS())

	w := httptest.NewRecorder()

	h(w, r)

	require.True(t, ran, "next handler should run")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, string(body), w.Body.String(), "body should be re-injected intact")
}

func TestAuth_BadKey401(t *testing.T) {
	s := newAuthServer(t)
	body := []byte(`{}`)

	var ran bool

	h := s.auth(echoHandler(&ran))

	r := httptest.NewRequest(http.MethodPost, "/trigger", strings.NewReader(string(body)))
	signReq(t, r, "wrong-key", body, nowTS())

	w := httptest.NewRecorder()

	h(w, r)

	assert.False(t, ran)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), protocol.CodeUnauthorized)
}

func TestAuth_StaleTimestamp401(t *testing.T) {
	s := newAuthServer(t)
	body := []byte(`{}`)

	var ran bool

	h := s.auth(echoHandler(&ran))

	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)

	r := httptest.NewRequest(http.MethodPost, "/trigger", strings.NewReader(string(body)))
	signReq(t, r, testAPIKey, body, stale)

	w := httptest.NewRecorder()

	h(w, r)

	assert.False(t, ran)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuth_MissingHeaders401(t *testing.T) {
	s := newAuthServer(t)

	var ran bool

	h := s.auth(echoHandler(&ran))

	r := httptest.NewRequest(http.MethodPost, "/trigger", strings.NewReader("{}"))
	w := httptest.NewRecorder()

	h(w, r)

	assert.False(t, ran)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestAuth_ReplayRejected401(t *testing.T) {
	s := newAuthServer(t)
	body := []byte(`{}`)

	var ran bool

	h := s.auth(echoHandler(&ran))

	ts := nowTS()
	sig := "sha256=" + protocol.SignPayloadWithTimestamp(testAPIKey, http.MethodPost, "/trigger", body, ts)

	// First request: accepted.
	r1 := httptest.NewRequest(http.MethodPost, "/trigger", strings.NewReader(string(body)))
	r1.Header.Set(protocol.SignatureHeader, sig)
	r1.Header.Set(protocol.TimestampHeader, ts)

	w1 := httptest.NewRecorder()
	h(w1, r1)
	require.Equal(t, http.StatusOK, w1.Code)

	// Identical signed request replayed: rejected by the replay cache.
	ran = false
	r2 := httptest.NewRequest(http.MethodPost, "/trigger", strings.NewReader(string(body)))
	r2.Header.Set(protocol.SignatureHeader, sig)
	r2.Header.Set(protocol.TimestampHeader, ts)

	w2 := httptest.NewRecorder()
	h(w2, r2)

	assert.False(t, ran)
	assert.Equal(t, http.StatusUnauthorized, w2.Code)
}

func TestAuth_SignedGETWithQueryPasses(t *testing.T) {
	s := newAuthServer(t)

	var ran bool

	h := s.auth(echoHandler(&ran))

	// Query is part of the signed base string: sign /logs?project=x exactly.
	r := httptest.NewRequest(http.MethodGet, "/logs?project=x", nil)
	require.Equal(t, "/logs?project=x", r.URL.RequestURI())
	signReq(t, r, testAPIKey, nil, nowTS())

	w := httptest.NewRecorder()

	h(w, r)

	require.True(t, ran)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAuth_BodyTooLarge413(t *testing.T) {
	s := newAuthServer(t)

	var ran bool

	h := s.auth(echoHandler(&ran))

	big := strings.Repeat("a", (1<<20)+1)
	r := httptest.NewRequest(http.MethodPost, "/trigger", strings.NewReader(big))
	signReq(t, r, testAPIKey, []byte(big), nowTS())

	w := httptest.NewRecorder()

	h(w, r)

	assert.False(t, ran)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestDrainGate_503WhenDraining(t *testing.T) {
	var draining atomic.Bool

	s := NewServer(Config{APIKey: testAPIKey, Draining: &draining})

	var ran bool

	h := s.drainGate(echoHandler(&ran))

	// Not draining: passes through.
	r1 := httptest.NewRequest(http.MethodPost, "/message", strings.NewReader("{}"))
	w1 := httptest.NewRecorder()
	h(w1, r1)
	require.True(t, ran)
	require.Equal(t, http.StatusOK, w1.Code)

	// Draining: 503 with the draining code.
	draining.Store(true)

	ran = false
	r2 := httptest.NewRequest(http.MethodPost, "/message", strings.NewReader("{}"))
	w2 := httptest.NewRecorder()
	h(w2, r2)

	assert.False(t, ran)
	assert.Equal(t, http.StatusServiceUnavailable, w2.Code)
	assert.Contains(t, w2.Body.String(), protocol.CodeDraining)
}
