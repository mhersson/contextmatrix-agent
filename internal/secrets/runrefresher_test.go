package secrets

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// discardLogger silences slog output so tests that exercise error/warn paths
// keep pristine output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubCM is a fake ContextMatrix git-credentials endpoint. It verifies the
// inbound HMAC signature for real (a bad signature fails the test), records the
// query it was called with, and mints an incrementing token on each call.
type stubCM struct {
	apiKey string

	// respExpiry is how far in the future the minted token's expiry lies. Zero
	// means 10s — comfortably beyond the test window, so the loop does not fire
	// again. The leak test sets it short so every live loop keeps calling.
	respExpiry time.Duration

	mu     sync.Mutex
	calls  int
	lastQ  string
	verify int32 // atomic: number of requests that passed HMAC verification
}

func (s *stubCM) handler(t *testing.T) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		// Verify the signature exactly like CM does: over METHOD, the full
		// request URI (path + query), and the timestamp, with an empty body.
		sig := strings.TrimPrefix(r.Header.Get(protocol.SignatureHeader), "sha256=")
		ts := r.Header.Get(protocol.TimestampHeader)

		if !protocol.VerifySignatureWithTimestamp(
			s.apiKey, r.Method, r.URL.RequestURI(), sig, ts, nil,
			protocol.DefaultMaxClockSkew, nil,
		) {
			t.Errorf("git-credentials request failed HMAC verification: uri=%q", r.URL.RequestURI())
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		atomic.AddInt32(&s.verify, 1)

		s.mu.Lock()
		s.calls++
		n := s.calls
		s.lastQ = r.URL.RawQuery
		s.mu.Unlock()

		expiry := s.respExpiry
		if expiry == 0 {
			expiry = 10 * time.Second
		}

		// Response keys match CM's documented git-credentials shape
		// ({token, expires_at}) — deliberately NOT TriggerPayload's names.
		resp := map[string]string{
			"token":      "refreshed-token-" + strconv.Itoa(n),
			"expires_at": time.Now().Add(expiry).UTC().Format(time.RFC3339Nano),
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func (s *stubCM) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

func (s *stubCM) lastQuery() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lastQ
}

// newTestManager builds a RunCredentials with tightened timing knobs so the
// refresh loop fires within test time.
func newTestManager(t *testing.T, cmURL, apiKey string) *RunCredentials {
	t.Helper()

	dir := t.TempDir()
	m := NewRunCredentials(dir, cmURL, apiKey, discardLogger())
	m.refreshBefore = 10 * time.Millisecond
	m.minSleep = 5 * time.Millisecond
	m.retryBackoff = 5 * time.Millisecond

	return m
}

// TestRunCredentialsInitialWrite verifies Provision writes the per-run env file
// with the payload token and LLM values at HostDir(project, cardID)/env.
func TestRunCredentialsInitialWrite(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, "http://cm.invalid", "key")

	err := m.Provision("proj", "CARD-1", "payload-token", "", EndpointSecrets{
		APIKey:  "llm-key",
		BaseURL: "https://llm.example/v1",
		Type:    "openai",
	})
	require.NoError(t, err)

	t.Cleanup(func() { m.Teardown("proj", "CARD-1") })

	path := filepath.Join(m.HostDir("proj", "CARD-1"), "env")
	src, err := Open(path)
	require.NoError(t, err)

	assert.Equal(t, "payload-token", src.Get("CM_GIT_TOKEN"))
	assert.Equal(t, "llm-key", src.Get("LLM_API_KEY"))
	assert.Equal(t, "https://llm.example/v1", src.Get("LLM_BASE_URL"))
	assert.Equal(t, "openai", src.Get("LLM_TYPE"))

	// The file must be 0600 (secret material).
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

// TestRunCredentialsRefreshesFromCM checks that with an expiring token the loop
// calls the CM git-credentials endpoint (HMAC-signed, with project/card_id
// query) and rewrites the env file with the refreshed token.
func TestRunCredentialsRefreshesFromCM(t *testing.T) {
	t.Parallel()

	stub := &stubCM{apiKey: "test-key"}
	srv := httptest.NewServer(stub.handler(t))
	t.Cleanup(srv.Close)

	m := newTestManager(t, srv.URL, "test-key")

	// A token that expires very soon so the loop fetches promptly.
	expiry := time.Now().Add(40 * time.Millisecond).UTC().Format(time.RFC3339Nano)

	require.NoError(t, m.Provision("proj", "CARD-1", "payload-token", expiry, EndpointSecrets{APIKey: "llm-key"}))
	t.Cleanup(func() { m.Teardown("proj", "CARD-1") })

	path := filepath.Join(m.HostDir("proj", "CARD-1"), "env")

	// Initial write carries the payload token.
	src, err := Open(path)
	require.NoError(t, err)
	assert.Equal(t, "payload-token", src.Get("CM_GIT_TOKEN"))

	// The refresh loop rewrites the file with the CM-minted token.
	require.Eventually(t, func() bool {
		s, err := Open(path)
		if err != nil {
			return false
		}

		return strings.HasPrefix(s.Get("CM_GIT_TOKEN"), "refreshed-token-")
	}, 3*time.Second, 10*time.Millisecond, "expected the refreshed token in the env file")

	// The static LLM value must persist across the rewrite.
	s, err := Open(path)
	require.NoError(t, err)
	assert.Equal(t, "llm-key", s.Get("LLM_API_KEY"))

	// The CM request carried the project and card_id query parameters.
	assert.Contains(t, stub.lastQuery(), "project=proj")
	assert.Contains(t, stub.lastQuery(), "card_id=CARD-1")
	assert.Positive(t, int(atomic.LoadInt32(&stub.verify)), "at least one request must pass HMAC verification")
}

// TestRunCredentialsPATNoRefresh verifies that a token without an expiry (PAT)
// is written once and never triggers a CM call.
func TestRunCredentialsPATNoRefresh(t *testing.T) {
	t.Parallel()

	stub := &stubCM{apiKey: "test-key"}
	srv := httptest.NewServer(stub.handler(t))
	t.Cleanup(srv.Close)

	m := newTestManager(t, srv.URL, "test-key")

	require.NoError(t, m.Provision("proj", "CARD-1", "pat-token", "", EndpointSecrets{}))
	t.Cleanup(func() { m.Teardown("proj", "CARD-1") })

	path := filepath.Join(m.HostDir("proj", "CARD-1"), "env")
	src, err := Open(path)
	require.NoError(t, err)
	assert.Equal(t, "pat-token", src.Get("CM_GIT_TOKEN"))

	// Give any (erroneously started) loop time to fire, then assert no calls.
	time.Sleep(80 * time.Millisecond)
	assert.Zero(t, stub.callCount(), "a PAT token (no expiry) must not trigger a refresh")
}

// TestRunCredentialsTeardownStopsLoopAndRemovesDir verifies Teardown cancels the
// refresh loop and removes the run directory.
func TestRunCredentialsTeardownStopsLoopAndRemovesDir(t *testing.T) {
	t.Parallel()

	stub := &stubCM{apiKey: "test-key"}
	srv := httptest.NewServer(stub.handler(t))
	t.Cleanup(srv.Close)

	m := newTestManager(t, srv.URL, "test-key")

	expiry := time.Now().Add(40 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	require.NoError(t, m.Provision("proj", "CARD-1", "payload-token", expiry, EndpointSecrets{}))

	dir := m.HostDir("proj", "CARD-1")

	// Wait for at least one refresh so the loop is definitely running.
	require.Eventually(t, func() bool {
		return stub.callCount() >= 1
	}, 3*time.Second, 10*time.Millisecond, "expected the loop to call CM at least once")

	m.Teardown("proj", "CARD-1")

	// The run directory must be gone.
	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err), "run dir must be removed on teardown")

	// After teardown the loop must be stopped: the call count stays put.
	before := stub.callCount()

	time.Sleep(80 * time.Millisecond)
	assert.Equal(t, before, stub.callCount(), "no CM calls must happen after teardown")
}

// TestRunCredentialsTeardownUnknownRunIsNoop verifies Teardown on a run that was
// never provisioned does not panic and does nothing.
func TestRunCredentialsTeardownUnknownRunIsNoop(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, "http://cm.invalid", "key")
	assert.NotPanics(t, func() { m.Teardown("proj", "nope") })
}

// TestRunCredentialsHostDirSanitizesCardID verifies HostDir keeps the run
// directory under the runs base dir even for a hostile project or card ID: both
// path components are sanitized with the same traversal-safe rules.
func TestRunCredentialsHostDirSanitizesCardID(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, "http://cm.invalid", "key")

	dir := m.HostDir("../evil", "../escape")
	assert.NotContains(t, dir, "..")

	rel, err := filepath.Rel(m.runsDir, dir)
	require.NoError(t, err)
	assert.NotContains(t, rel, "..", "the run dir must stay under the runs base dir")
	assert.False(t, filepath.IsAbs(rel), "the run dir must be a descendant of the runs base dir")
}

// TestRunCredentialsProjectScopedIsolation pins the multi-user isolation
// invariant: the same card ID reused across two projects must map to distinct
// run directories with each project's own token, and tearing one down must
// leave the other's live credentials intact. A card-ID-only path would collide
// both projects on one env file and let either teardown delete the other's
// credentials.
func TestRunCredentialsProjectScopedIsolation(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, "http://cm.invalid", "key")

	require.NoError(t, m.Provision("projA", "CARD-1", "token-A", "", EndpointSecrets{APIKey: "key-A"}))
	require.NoError(t, m.Provision("projB", "CARD-1", "token-B", "", EndpointSecrets{APIKey: "key-B"}))

	t.Cleanup(func() { m.Teardown("projB", "CARD-1") })

	dirA := m.HostDir("projA", "CARD-1")
	dirB := m.HostDir("projB", "CARD-1")
	assert.NotEqual(t, dirA, dirB, "the same card ID in two projects must map to distinct run dirs")

	srcA, err := Open(filepath.Join(dirA, "env"))
	require.NoError(t, err)
	assert.Equal(t, "token-A", srcA.Get("CM_GIT_TOKEN"), "project A keeps its own token")

	srcB, err := Open(filepath.Join(dirB, "env"))
	require.NoError(t, err)
	assert.Equal(t, "token-B", srcB.Get("CM_GIT_TOKEN"), "project B keeps its own token")

	// Tearing down project A must not touch project B's still-live credentials.
	m.Teardown("projA", "CARD-1")

	_, err = os.Stat(dirA)
	assert.True(t, os.IsNotExist(err), "the torn-down run dir must be gone")

	srcB2, err := Open(filepath.Join(dirB, "env"))
	require.NoError(t, err)
	assert.Equal(t, "token-B", srcB2.Get("CM_GIT_TOKEN"),
		"the other project's credentials must survive a sibling teardown")
}

// TestRunCredentialsCleanupOrphans verifies the boot-time sweep removes any
// leftover run directories.
func TestRunCredentialsCleanupOrphans(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, "http://cm.invalid", "key")

	require.NoError(t, m.Provision("proj", "CARD-1", "tok", "", EndpointSecrets{}))
	path := filepath.Join(m.HostDir("proj", "CARD-1"), "env")
	_, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, m.CleanupOrphans())

	_, err = os.Stat(m.HostDir("proj", "CARD-1"))
	assert.True(t, os.IsNotExist(err), "orphan run dirs must be swept at boot")
}

// TestRunCredentialsConcurrentProvisionNoLeakedLoop pins the map-overwrite
// window: displacing the previous handle, joining its goroutine, and storing
// the new handle must be one atomic step under the manager lock. Before the
// fix, Provision released the lock between starting the goroutine and storing
// the handle, so an interleaved Provision could overwrite a handle whose
// goroutine was never joined — a leaked refresh loop that kept hitting CM
// after Teardown. The stub mints short-lived tokens, so every live loop keeps
// calling CM on the minSleep floor: after Teardown the call count must freeze,
// which a leaked loop violates.
func TestRunCredentialsConcurrentProvisionNoLeakedLoop(t *testing.T) {
	t.Parallel()

	// Short minted expiry keeps every live (including leaked) loop calling CM
	// continuously instead of sleeping past the observation window.
	stub := &stubCM{apiKey: "test-key", respExpiry: 20 * time.Millisecond}
	srv := httptest.NewServer(stub.handler(t))
	t.Cleanup(srv.Close)

	m := newTestManager(t, srv.URL, "test-key")

	// Many barrier-released waves of concurrent Provisions maximise the chance
	// of hitting the displace/store interleaving the lock must exclude.
	const (
		waves     = 20
		perWave   = 4
		runToken  = "payload-token"
		runExpiry = 25 * time.Millisecond
	)

	for range waves {
		start := make(chan struct{})
		errCh := make(chan error, perWave)

		var wg sync.WaitGroup

		for range perWave {
			wg.Add(1)

			go func() {
				defer wg.Done()

				<-start

				expiry := time.Now().Add(runExpiry).UTC().Format(time.RFC3339Nano)
				errCh <- m.Provision("proj", "CARD-1", runToken, expiry, EndpointSecrets{})
			}()
		}

		close(start)
		wg.Wait()
		close(errCh)

		for err := range errCh {
			require.NoError(t, err)
		}
	}

	// Let the surviving loop run at least once so a leaked sibling would be
	// observably alive too.
	require.Eventually(t, func() bool {
		return stub.callCount() >= 1
	}, 3*time.Second, 5*time.Millisecond, "expected the surviving loop to call CM")

	m.Teardown("proj", "CARD-1")

	// The loops poll every ~5-15ms here; 150ms of post-teardown silence proves
	// nothing survived.
	before := stub.callCount()

	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, before, stub.callCount(),
		"a leaked refresh loop kept calling CM after teardown")

	_, err := os.Stat(m.HostDir("proj", "CARD-1"))
	assert.True(t, os.IsNotExist(err), "run dir must be removed on teardown")
}
