package taskskills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeGen struct{}

func (fakeGen) GenerateToken(context.Context) (string, time.Time, error) {
	return "tok", time.Now().Add(time.Hour), nil
}

// recordingGen is a tokenGen that records how many times it was invoked, so
// tests can assert local minting was (or was not) reached.
type recordingGen struct {
	calls int32
}

func (g *recordingGen) GenerateToken(context.Context) (string, time.Time, error) {
	atomic.AddInt32(&g.calls, 1)

	return "self-minted", time.Now().Add(time.Hour), nil
}

func TestResolveFetchesPointerClonesAndCaches(t *testing.T) {
	var hits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		assert.Equal(t, "/api/agent/task-skills-source", r.URL.Path)
		assert.NotEmpty(t, r.Header.Get("X-Signature-256"), "the GET is HMAC-signed")

		_ = json.NewEncoder(w).Encode(map[string]string{
			"git_remote_url": "https://example.test/skills.git",
			"ref":            "abc123",
		})
	}))
	defer srv.Close()

	var gotURL, gotRef, gotDest, gotTok string

	cloner := func(_ context.Context, url, ref, dest, token string) error {
		gotURL, gotRef, gotDest, gotTok = url, ref, dest, token

		return nil
	}

	r := NewResolver(srv.URL, "key", t.TempDir(), fakeGen{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.cloner = cloner

	dir, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.NotEmpty(t, dir)
	assert.Equal(t, "https://example.test/skills.git", gotURL)
	assert.Equal(t, "abc123", gotRef)
	assert.Equal(t, dir, gotDest)
	assert.Equal(t, "tok", gotTok)

	// Second call is cached: no second pointer fetch, no second clone.
	dir2, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, dir, dir2)
	assert.Equal(t, int32(1), atomic.LoadInt32(&hits), "pointer fetched once; result cached")
}

func TestResolveEmptyPointerYieldsNoSkills(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"git_remote_url": "", "ref": ""})
	}))
	defer srv.Close()

	r := NewResolver(srv.URL, "key", t.TempDir(), fakeGen{}, nil)
	r.cloner = func(context.Context, string, string, string, string) error { return nil }

	_, err := r.Resolve(context.Background())
	require.Error(t, err, "an empty remote URL means there is no skills source")
}

// TestGitCloneRejectsDashLeadingRef ensures that a git URL or ref beginning
// with '-' (which git would interpret as a flag) is rejected before exec is
// reached. The cloner must never be called with such inputs.
func TestGitCloneRejectsDashLeadingRef(t *testing.T) {
	t.Run("dash-leading ref is rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"git_remote_url": "https://example.test/skills.git",
				"ref":            "-upload-pack=/tmp/pwn",
			})
		}))
		defer srv.Close()

		r := NewResolver(srv.URL, "key", t.TempDir(), fakeGen{}, nil)
		r.cloner = func(_ context.Context, _, _, _, _ string) error {
			t.Fatal("cloner must not be called with a dash-leading ref")

			return nil
		}

		_, err := r.Resolve(context.Background())
		require.Error(t, err, "dash-leading ref must be rejected before exec")
	})

	t.Run("dash-leading URL is rejected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"git_remote_url": "--upload-pack=/tmp/pwn",
				"ref":            "abc123",
			})
		}))
		defer srv.Close()

		r := NewResolver(srv.URL, "key", t.TempDir(), fakeGen{}, nil)
		r.cloner = func(_ context.Context, _, _, _, _ string) error {
			t.Fatal("cloner must not be called with a dash-leading URL")

			return nil
		}

		_, err := r.Resolve(context.Background())
		require.Error(t, err, "dash-leading URL must be rejected before exec")
	})
}

func TestResolveDoesNotCacheFailure(t *testing.T) {
	var hits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]string{"git_remote_url": "https://example.test/s.git", "ref": "r"})
	}))
	defer srv.Close()

	r := NewResolver(srv.URL, "key", t.TempDir(), fakeGen{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.cloner = func(context.Context, string, string, string, string) error { return nil }

	_, err := r.Resolve(context.Background())
	require.Error(t, err)

	_, err = r.Resolve(context.Background())
	require.NoError(t, err, "a prior failure is not cached; the next call retries")
}

// ---- CM-provisioned clone token (compat window) ------------------------------

// TestResolveUsesCMProvisionedTokenWhenPresent pins the compat-window
// override: when the task-skills-source response carries a token, the
// resolver must clone with THAT token and must never reach local minting.
func TestResolveUsesCMProvisionedTokenWhenPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"git_remote_url":   "https://example.test/skills.git",
			"ref":              "abc123",
			"token":            "cm-provisioned-token",
			"token_expires_at": "2026-07-03T18:00:00Z",
		})
	}))
	defer srv.Close()

	gen := &recordingGen{}

	var gotTok string

	cloner := func(_ context.Context, _, _, _, token string) error {
		gotTok = token

		return nil
	}

	r := NewResolver(srv.URL, "key", t.TempDir(), gen, nil)
	r.cloner = cloner

	_, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "cm-provisioned-token", gotTok)
	assert.Equal(t, int32(0), atomic.LoadInt32(&gen.calls), "a CM-provisioned token must skip local minting")
}

// TestResolveSelfMintsWhenTokenAbsent mirrors the pre-existing behavior: when
// the task-skills-source response carries no token, the resolver falls back
// to minting one via the local generator.
func TestResolveSelfMintsWhenTokenAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"git_remote_url": "https://example.test/skills.git",
			"ref":            "abc123",
		})
	}))
	defer srv.Close()

	gen := &recordingGen{}

	var gotTok string

	cloner := func(_ context.Context, _, _, _, token string) error {
		gotTok = token

		return nil
	}

	r := NewResolver(srv.URL, "key", t.TempDir(), gen, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.cloner = cloner

	_, err := r.Resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "self-minted", gotTok)
	assert.Equal(t, int32(1), atomic.LoadInt32(&gen.calls), "an absent CM token must fall back to local minting")
}

// TestResolveWarnsOncePerProcessNotPerAttempt pins the once-per-process (not
// once per resolve attempt) deprecation warning, mirroring the webhook
// handler's credentialFallbackWarnOnce pattern: a first attempt that reaches
// local minting and then fails downstream (clone failure) must not cause a
// retried attempt to log the warning a second time.
func TestResolveWarnsOncePerProcessNotPerAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"git_remote_url": "https://example.test/skills.git",
			"ref":            "abc123",
		})
	}))
	defer srv.Close()

	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, nil))

	var attempt int32

	r := NewResolver(srv.URL, "key", t.TempDir(), fakeGen{}, logger)
	r.cloner = func(context.Context, string, string, string, string) error {
		if atomic.AddInt32(&attempt, 1) == 1 {
			return errors.New("boom")
		}

		return nil
	}

	_, err := r.Resolve(context.Background())
	require.Error(t, err)

	_, err = r.Resolve(context.Background())
	require.NoError(t, err)

	warnCount := strings.Count(buf.String(),
		"CM did not provision a task-skills clone token; self-minting via local github config is deprecated")
	assert.Equal(t, 1, warnCount, "deprecation warning must fire once per process, not per resolve attempt")
}

// TestResolveNoWarnWhenTokenProvisioned asserts the deprecation warning stays
// silent once CM provisions a clone token.
func TestResolveNoWarnWhenTokenProvisioned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"git_remote_url": "https://example.test/skills.git",
			"ref":            "abc123",
			"token":          "cm-provisioned-token",
		})
	}))
	defer srv.Close()

	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, nil))

	r := NewResolver(srv.URL, "key", t.TempDir(), fakeGen{}, logger)
	r.cloner = func(context.Context, string, string, string, string) error { return nil }

	_, err := r.Resolve(context.Background())
	require.NoError(t, err)

	assert.Empty(t, buf.String(), "no deprecation warning expected when CM provisions the clone token")
}
