package secrets

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGenerator simulates a TokenGenerator that returns pre-configured tokens.
type fakeGenerator struct {
	calls []fakeCall
	idx   int
}

type fakeCall struct {
	token     string
	expiresAt time.Time
}

func (f *fakeGenerator) GenerateToken(_ context.Context) (string, time.Time, error) {
	if f.idx >= len(f.calls) {
		return "", time.Time{}, nil
	}

	c := f.calls[f.idx]
	f.idx++

	return c.token, c.expiresAt, nil
}

// TestOpen parses KEY=value lines, skips blanks and comments.
func TestOpen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")

	content := "# comment\n\nFOO=bar\nBAZ=qux quux\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	s, err := Open(path)
	require.NoError(t, err)

	assert.Equal(t, "bar", s.Get("FOO"))
	assert.Equal(t, "qux quux", s.Get("BAZ"))
	assert.Empty(t, s.Get("MISSING"))
}

// TestOpenMissingFile returns an error.
func TestOpenMissingFile(t *testing.T) {
	t.Parallel()

	_, err := Open("/nonexistent/path/env")
	assert.Error(t, err)
}

// TestWriteEnvFile checks atomic write, modes, and round-trip.
func TestWriteEnvFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "env")

	vals := map[string]string{
		"CM_GIT_TOKEN":       "tok123",
		"OPENROUTER_API_KEY": "or-key",
	}

	require.NoError(t, WriteEnvFile(path, vals))

	// File mode must be 0600.
	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	// Dir mode must be 0700.
	di, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), di.Mode().Perm())

	// Round-trip: Open must return the same values.
	s, err := Open(path)
	require.NoError(t, err)
	assert.Equal(t, "tok123", s.Get("CM_GIT_TOKEN"))
	assert.Equal(t, "or-key", s.Get("OPENROUTER_API_KEY"))
}

// TestWriteEnvFileDeterministic asserts byte-identical output across rewrites
// even with keys beyond the fixed-order pair.
func TestWriteEnvFileDeterministic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")

	vals := map[string]string{
		"CM_GIT_TOKEN":       "tok123",
		"OPENROUTER_API_KEY": "or-key",
		"EXTRA_SECRET":       "extra-val",
		"ANOTHER_KEY":        "another-val",
	}

	require.NoError(t, WriteEnvFile(path, vals))
	first, err := os.ReadFile(path)
	require.NoError(t, err)

	require.NoError(t, WriteEnvFile(path, vals))
	second, err := os.ReadFile(path)
	require.NoError(t, err)

	assert.Equal(t, string(first), string(second))
	assert.Equal(t,
		"OPENROUTER_API_KEY=or-key\nCM_GIT_TOKEN=tok123\nANOTHER_KEY=another-val\nEXTRA_SECRET=extra-val\n",
		string(first))
}

// TestForReturnsSource verifies the per-run seam returns self.
func TestForReturnsSource(t *testing.T) {
	t.Parallel()

	s := &Source{vals: map[string]string{"K": "v"}}
	got := s.For("some-run-id")

	assert.Same(t, s, got)
}

// TestRefresherWritesAndRefreshes exercises the Refresher end-to-end.
func TestRefresherWritesAndRefreshes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")

	now := time.Now()
	firstExpiry := now.Add(60 * time.Millisecond)
	secondExpiry := now.Add(10 * time.Second)

	gen := &fakeGenerator{
		calls: []fakeCall{
			{token: "tok1", expiresAt: firstExpiry},
			{token: "tok2", expiresAt: secondExpiry},
		},
	}

	r := NewRefresher(path, "or-static-key", gen, nil)
	// Shrink timing constants so the test completes quickly.
	r.refreshBefore = 20 * time.Millisecond
	r.minSleep = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)

	go func() { errCh <- r.Run(ctx) }()

	// After Run starts, the file must contain tok1.
	require.Eventually(t, func() bool {
		s, err := Open(path)
		if err != nil {
			return false
		}

		return s.Get("CM_GIT_TOKEN") == "tok1"
	}, 2*time.Second, 10*time.Millisecond, "expected tok1 in env file")

	// After the refresh window passes, the file must contain tok2.
	require.Eventually(t, func() bool {
		s, err := Open(path)
		if err != nil {
			return false
		}

		return s.Get("CM_GIT_TOKEN") == "tok2"
	}, 2*time.Second, 10*time.Millisecond, "expected tok2 in env file")

	// OPENROUTER_API_KEY must persist across rewrites.
	s, err := Open(path)
	require.NoError(t, err)
	assert.Equal(t, "or-static-key", s.Get("OPENROUTER_API_KEY"))

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
