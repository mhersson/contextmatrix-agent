package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
)

// gitCredentialsPath is the ContextMatrix endpoint that mints a fresh git token
// for a running card. The agent signs the GET exactly like the other
// /api/agent/* callbacks.
const gitCredentialsPath = "/api/agent/git-credentials" //nolint:gosec // path, not a credential

// runsSubdir is the directory under the secrets dir that holds one env file per
// active run: <secrets_dir>/runs/<project>/<card_id>/env. The project component
// scopes the run dir per project so the same card ID reused across projects
// never collides on one env file.
const runsSubdir = "runs"

// credentialsRequestTimeout bounds each git-credentials GET to CM.
const credentialsRequestTimeout = 15 * time.Second

// pathSanitize replaces every character outside a conservative filesystem-safe
// set so a hostile project or card ID can never escape the runs directory (no
// '/', no '.'). Applied to each path component independently.
var pathSanitize = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// RunCredentials stages per-run credential files and refreshes the git token
// from ContextMatrix for the run's lifetime. One instance is shared
// process-wide; it fans out one refresh goroutine per active run whose token
// carries an expiry. A PAT-style token (no expiry) is written once and never
// refreshed. Safe for concurrent use.
type RunCredentials struct {
	runsDir string
	cmURL   string
	apiKey  string
	http    *http.Client
	logger  *slog.Logger

	// Timing knobs. Defaults come from the default* consts in secrets.go; tests
	// shrink them.
	refreshBefore time.Duration // rewrite this far ahead of expiry
	minSleep      time.Duration // floor on the sleep between refreshes
	retryBackoff  time.Duration // fast retry after a transient failure

	mu   sync.Mutex
	runs map[string]*runHandle // keyed by project/cardID
}

// runHandle tracks one provisioned run so Teardown can stop its refresh loop
// (when it has one) and remove its directory.
type runHandle struct {
	dir    string
	cancel context.CancelFunc // nil for PAT runs (no refresh loop)
	done   chan struct{}      // closed when the refresh goroutine returns; nil for PAT runs
}

// stop cancels the handle's refresh goroutine and waits for it to exit. Safe on
// a PAT handle (no loop). The refresh loop takes no locks and its in-flight CM
// request aborts on cancel, so the join is prompt even under the manager lock.
func (h *runHandle) stop() {
	if h.cancel != nil {
		h.cancel()
		<-h.done
	}
}

// NewRunCredentials builds a RunCredentials rooted at <secretsDir>/runs. cmURL is
// ContextMatrix's base URL and apiKey the agent backend HMAC key used to sign the
// git-credentials GET. Pass nil for logger to use the default.
func NewRunCredentials(secretsDir, cmURL, apiKey string, logger *slog.Logger) *RunCredentials {
	if logger == nil {
		logger = slog.Default()
	}

	return &RunCredentials{
		runsDir:       filepath.Join(secretsDir, runsSubdir),
		cmURL:         strings.TrimRight(cmURL, "/"),
		apiKey:        apiKey,
		http:          &http.Client{Timeout: credentialsRequestTimeout},
		logger:        logger,
		refreshBefore: defaultRefreshBefore,
		minSleep:      defaultMinSleep,
		retryBackoff:  defaultRetryBackoff,
		runs:          make(map[string]*runHandle),
	}
}

// HostDir returns the per-run directory bind-mounted read-only at
// /run/cm-secrets. Project and card ID are each sanitized and joined as separate
// path components (<runs>/<project>/<card_id>) so two projects reusing the same
// card ID map to distinct dirs and neither can escape the runs dir.
func (m *RunCredentials) HostDir(project, cardID string) string {
	return filepath.Join(m.runsDir,
		pathSanitize.ReplaceAllString(project, "-"),
		pathSanitize.ReplaceAllString(cardID, "-"))
}

// Provision writes the per-run env file (git token + LLM endpoint values) and,
// when expiresAt parses to a real expiry, starts a goroutine that rewrites the
// file ahead of each expiry by minting a fresh token from CM. A PAT-style token
// (empty or unparseable expiresAt) is written once with no refresh loop.
//
// A re-provision of the same run replaces any existing refresh loop. The initial
// write is synchronous so the file exists before the container mounts it; a
// write error is returned and nothing is registered.
//
// The whole provision runs under the manager lock: displacing the previous
// handle, joining its goroutine, writing the file, and storing the new handle
// are one atomic step. Releasing the lock mid-way would let a concurrent
// Provision overwrite a handle whose goroutine is then never joined — a leaked
// refresh loop no Teardown can reach. The refresh loop takes no locks, so
// holding the lock across the join cannot deadlock, and the join is prompt
// (cancel aborts the loop's in-flight CM request).
func (m *RunCredentials) Provision(project, cardID, token, expiresAt string, endpoint EndpointSecrets) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := runKey(project, cardID)

	// Displace and join any pre-existing loop for this run before rewriting the
	// file so two goroutines never race on the same path.
	if old, ok := m.runs[k]; ok {
		delete(m.runs, k)
		old.stop()
	}

	dir := m.HostDir(project, cardID)
	path := filepath.Join(dir, "env")

	if err := WriteEnvFile(path, endpointVals(token, endpoint)); err != nil {
		return fmt.Errorf("write per-run env file: %w", err)
	}

	h := &runHandle{dir: dir}

	if expiry, ok := parseExpiry(expiresAt); ok {
		//nolint:gosec // G118: cancel is stored on the run handle and called by Teardown or a displacing Provision; the loop must outlive Provision
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		h.cancel = cancel
		h.done = done

		go func() {
			defer close(done)

			m.refreshLoop(ctx, project, cardID, path, endpoint, expiry)
		}()
	}

	m.runs[k] = h

	return nil
}

// Teardown stops the run's refresh loop (if any), waits for the goroutine to
// exit, and removes the run directory. Idempotent: a run that was never
// provisioned is a no-op. It holds the manager lock throughout so the dir
// removal cannot interleave with a concurrent Provision's write to the same
// path.
func (m *RunCredentials) Teardown(project, cardID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := runKey(project, cardID)

	h, ok := m.runs[k]
	if !ok {
		return
	}

	delete(m.runs, k)
	h.stop()

	if err := os.RemoveAll(h.dir); err != nil {
		m.logger.Warn("remove per-run secrets dir failed",
			"project", project, "card_id", cardID, "error", err)
	}
}

// CleanupOrphans removes the entire runs directory. Call it once at boot: a
// fresh process tracks no runs, so any run dir on disk is a leftover from a
// previous process (stale secret material). Mirrors the executor's orphan sweep.
func (m *RunCredentials) CleanupOrphans() error {
	if err := os.RemoveAll(m.runsDir); err != nil {
		return fmt.Errorf("sweep orphan run dirs: %w", err)
	}

	return nil
}

// refreshLoop rewrites the env file ahead of each token expiry by minting a
// fresh token from CM. It blocks until ctx is cancelled (Teardown).
func (m *RunCredentials) refreshLoop(
	ctx context.Context,
	project, cardID, path string,
	endpoint EndpointSecrets,
	expiresAt time.Time,
) {
	for {
		sleep := max(time.Until(expiresAt)-m.refreshBefore, m.minSleep)

		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}

		token, expiry, err := m.fetchGitCredentials(ctx, project, cardID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			m.logger.Error("refresh git credentials failed; retrying on backoff",
				"project", project, "card_id", cardID, "error", err, "backoff", m.retryBackoff)

			select {
			case <-ctx.Done():
				return
			case <-time.After(m.retryBackoff):
			}

			continue
		}

		if err := WriteEnvFile(path, endpointVals(token, endpoint)); err != nil {
			m.logger.Error("rewrite per-run env file failed; retrying on backoff",
				"project", project, "card_id", cardID, "error", err, "backoff", m.retryBackoff)

			select {
			case <-ctx.Done():
				return
			case <-time.After(m.retryBackoff):
			}

			continue
		}

		m.logger.Info("per-run env file refreshed",
			"project", project, "card_id", cardID, "expires_at", expiry)

		// A refreshed token with no expiry (CM switched the binding to a PAT) has
		// nothing left to refresh — stop rather than busy-poll on the minSleep floor.
		if expiry.IsZero() {
			m.logger.Info("refreshed token has no expiry; stopping refresh loop",
				"project", project, "card_id", cardID)

			return
		}

		expiresAt = expiry
	}
}

// gitCredentials is the decode target for CM's git-credentials response:
// {"token": "...", "expires_at": "..."} per docs/api-reference.md in the CM
// repo. Note the keys deliberately differ from TriggerPayload's git_token /
// git_token_expires_at — different endpoint, different shape; expires_at is
// absent for PAT-backed credentials (no refresh needed).
type gitCredentials struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// fetchGitCredentials does a signed GET to CM's git-credentials endpoint for the
// run and returns the freshly minted token and its expiry.
func (m *RunCredentials) fetchGitCredentials(ctx context.Context, project, cardID string) (string, time.Time, error) {
	u, err := url.Parse(m.cmURL + gitCredentialsPath)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parse git-credentials url: %w", err)
	}

	q := u.Query()
	q.Set("project", project)
	q.Set("card_id", cardID)
	u.RawQuery = q.Encode()

	// Sign over the exact request URI (path + query) CM will verify against.
	sig, ts := protocol.SignRequestHeaders(m.apiKey, http.MethodGet, u.RequestURI(), nil)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create git-credentials request: %w", err)
	}

	req.Header.Set(protocol.SignatureHeader, sig)
	req.Header.Set(protocol.TimestampHeader, ts)

	resp, err := m.http.Do(req) //nolint:gosec // cmURL is operator config, not user input
	if err != nil {
		return "", time.Time{}, fmt.Errorf("fetch git-credentials: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", time.Time{}, fmt.Errorf("git-credentials returned %d", resp.StatusCode)
	}

	var gc gitCredentials
	if err := json.Unmarshal(body, &gc); err != nil {
		return "", time.Time{}, fmt.Errorf("parse git-credentials: %w", err)
	}

	if gc.Token == "" {
		return "", time.Time{}, fmt.Errorf("git-credentials response carried no token")
	}

	expiry, _ := parseExpiry(gc.ExpiresAt)

	return gc.Token, expiry, nil
}

// runKey is the map key for a run: project and card ID together disambiguate the
// same card ID reused across projects.
func runKey(project, cardID string) string {
	return project + "/" + cardID
}

// parseExpiry parses an RFC3339 expiry. It returns ok=false for an empty,
// unparseable, or sentinel (year 9999, the PAT convention) timestamp — those all
// mean "no expiry", so no refresh loop.
func parseExpiry(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}

	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return time.Time{}, false
		}
	}

	if t.Year() >= 9999 {
		return time.Time{}, false
	}

	return t, true
}
