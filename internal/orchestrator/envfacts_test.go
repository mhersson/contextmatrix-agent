package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubBin writes an executable shell stub named name into dir that echoes out.
func stubBin(t *testing.T, dir, name, out string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name),
		[]byte("#!/bin/sh\necho '"+out+"'\n"), 0o755))
}

func TestEnvironmentFacts(t *testing.T) {
	today := "Date: " + time.Now().UTC().Format("2006-01-02")

	t.Run("no markers yields header and date only", func(t *testing.T) {
		got := environmentFacts(t.TempDir())
		assert.Contains(t, got, "ENVIRONMENT (authoritative; verified on this container — do not dispute from memory)")
		assert.Contains(t, got, today)
		assert.Len(t, strings.Split(got, "\n"), 2)
	})

	t.Run("go marker probes go version", func(t *testing.T) {
		ws, bin := t.TempDir(), t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module x\n"), 0o644))
		stubBin(t, bin, "go", "go version go9.9.9 linux/amd64")
		t.Setenv("PATH", bin)

		got := environmentFacts(ws)
		assert.Contains(t, got, "go version go9.9.9 linux/amd64")
	})

	t.Run("missing binary omits the line silently", func(t *testing.T) {
		ws, bin := t.TempDir(), t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(ws, "package.json"), []byte("{}"), 0o644))
		t.Setenv("PATH", bin) // no node stub

		got := environmentFacts(ws)
		assert.NotContains(t, got, "node")
		assert.Contains(t, got, today)
	})

	t.Run("pyproject and requirements dedupe to one python probe", func(t *testing.T) {
		ws, bin := t.TempDir(), t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(ws, "pyproject.toml"), []byte(""), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(ws, "requirements.txt"), []byte(""), 0o644))
		stubBin(t, bin, "python3", "Python 9.9.9")
		t.Setenv("PATH", bin)

		got := environmentFacts(ws)
		assert.Equal(t, 1, strings.Count(got, "Python 9.9.9"))
	})

	t.Run("multiline probe output keeps first line only", func(t *testing.T) {
		ws, bin := t.TempDir(), t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(ws, "Cargo.toml"), []byte(""), 0o644))
		stubBin(t, bin, "rustc", "rustc 9.9.9\nextra noise")
		t.Setenv("PATH", bin)

		got := environmentFacts(ws)
		assert.Contains(t, got, "rustc 9.9.9")
		assert.NotContains(t, got, "extra noise")
	})
}
