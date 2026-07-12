package secrets

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWriteEnvFileNeutralKeys verifies WriteEnvFile round-trips the four neutral
// worker env keys.
func TestWriteEnvFileNeutralKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	require.NoError(t, WriteEnvFile(path, map[string]string{
		"LLM_API_KEY":  "k",
		"LLM_BASE_URL": "https://your-llm-endpoint.example/v1",
		"LLM_TYPE":     "openai",
		"CM_GIT_TOKEN": "t",
	}))

	src, err := Open(path)
	require.NoError(t, err)
	assert.Equal(t, "k", src.Get("LLM_API_KEY"))
	assert.Equal(t, "https://your-llm-endpoint.example/v1", src.Get("LLM_BASE_URL"))
	assert.Equal(t, "openai", src.Get("LLM_TYPE"))
	assert.Equal(t, "t", src.Get("CM_GIT_TOKEN"))
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
		"CM_GIT_TOKEN": "tok123",
		"LLM_API_KEY":  "llm-key",
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
	assert.Equal(t, "llm-key", s.Get("LLM_API_KEY"))
}

// TestWriteEnvFileDeterministic asserts byte-identical output across rewrites
// even with keys beyond the fixed-order set.
func TestWriteEnvFileDeterministic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "env")

	vals := map[string]string{
		"CM_GIT_TOKEN": "tok123",
		"LLM_API_KEY":  "llm-key",
		"LLM_BASE_URL": "https://your-llm-endpoint.example/v1",
		"LLM_TYPE":     "openai",
		"EXTRA_SECRET": "extra-val",
		"ANOTHER_KEY":  "another-val",
	}

	require.NoError(t, WriteEnvFile(path, vals))
	first, err := os.ReadFile(path)
	require.NoError(t, err)

	require.NoError(t, WriteEnvFile(path, vals))
	second, err := os.ReadFile(path)
	require.NoError(t, err)

	assert.Equal(t, string(first), string(second))
	assert.Equal(t,
		"LLM_API_KEY=llm-key\nLLM_BASE_URL=https://your-llm-endpoint.example/v1\nLLM_TYPE=openai\nCM_GIT_TOKEN=tok123\nANOTHER_KEY=another-val\nEXTRA_SECRET=extra-val\n",
		string(first))
}

// TestEnvFileCarriesMobGuests pins the refresh-safe guest delivery: guests
// are part of endpointVals, so BOTH the initial Provision write and every
// token-refresh rewrite emit the CM_MOB_GUESTS line, and the worker-side
// Source reads it back verbatim.
func TestEnvFileCarriesMobGuests(t *testing.T) {
	t.Parallel()

	guestJSON := `[{"name":"laptop","url":"http://10.0.0.5:8484","token":"guest-secret"}]`

	vals := endpointVals("git-tok", EndpointSecrets{
		APIKey:    "k",
		MobGuests: guestJSON,
	})
	//nolint:testifylint // byte-exact round-trip check (verbatim passthrough), not semantic JSON equality
	assert.Equal(t, guestJSON, vals["CM_MOB_GUESTS"])

	path := filepath.Join(t.TempDir(), "env")
	require.NoError(t, WriteEnvFile(path, vals))

	src, err := Open(path)
	require.NoError(t, err)
	//nolint:testifylint // byte-exact round-trip check (verbatim passthrough), not semantic JSON equality
	assert.Equal(t, guestJSON, src.Get("CM_MOB_GUESTS"))
	assert.Equal(t, "git-tok", src.Get("CM_GIT_TOKEN"))

	// Empty guests: the key must be absent from the file entirely.
	valsNone := endpointVals("git-tok", EndpointSecrets{APIKey: "k"})
	_, present := valsNone["CM_MOB_GUESTS"]
	assert.False(t, present, "empty guests must not write an empty CM_MOB_GUESTS line")
}
