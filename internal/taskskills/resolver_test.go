package taskskills

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	r := NewResolver(srv.URL, "key", t.TempDir(), fakeGen{}, nil)
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

	r := NewResolver(srv.URL, "key", t.TempDir(), fakeGen{}, nil)
	r.cloner = func(context.Context, string, string, string, string) error { return nil }

	_, err := r.Resolve(context.Background())
	require.Error(t, err)

	_, err = r.Resolve(context.Background())
	require.NoError(t, err, "a prior failure is not cached; the next call retries")
}
