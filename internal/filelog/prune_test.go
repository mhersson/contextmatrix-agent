package filelog

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func touch(t *testing.T, path string, mtime time.Time) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
}

func TestPruneRemovesOnlyStaleLogs(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	dir := t.TempDir()

	stale := filepath.Join(dir, "p", "a.log")
	fresh := filepath.Join(dir, "p", "b.log")
	staleOther := filepath.Join(dir, "p", "c.check.log")
	notes := filepath.Join(dir, "p", "notes.txt")

	touch(t, stale, now.Add(-31*day))
	touch(t, fresh, now.Add(-29*day))
	touch(t, staleOther, now.Add(-31*day))
	touch(t, notes, now.Add(-40*day))

	assert.Equal(t, 2, New(dir, testLogger()).Prune(now, 30*day))

	assert.NoFileExists(t, stale)
	assert.NoFileExists(t, staleOther)
	assert.FileExists(t, fresh)
	assert.FileExists(t, notes)
}

func TestPruneNeverRemovesAnOpenLog(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	l := New(dir, testLogger())

	l.Begin("p", "CM-1", "cid", "corr-1")

	path := filepath.Join(dir, "p", "cm-1.log")
	old := now.Add(-40 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))

	assert.Zero(t, l.Prune(now, 30*24*time.Hour))

	l.Write("p", "CM-1", "corr-1", []byte("after prune"), false)
	l.End("p", "CM-1", "corr-1", 0, "normal")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "after prune\n")
}
