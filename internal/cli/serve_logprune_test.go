package cli

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/filelog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogPrunerRunsAtStartAndOnEachTick(t *testing.T) {
	dir := t.TempDir()
	files := filelog.New(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	old := time.Now().Add(-48 * time.Hour)

	writeStale := func(name string) string {
		p := filepath.Join(dir, "p", name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
		require.NoError(t, os.Chtimes(p, old, old))

		return p
	}

	first := writeStale("first.log")

	stop := startLogPruner(files, 24*time.Hour, 10*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))

	assert.NoFileExists(t, first, "the start-up sweep runs before startLogPruner returns")

	second := writeStale("second.log")

	assert.Eventually(t, func() bool {
		_, err := os.Stat(second)

		return os.IsNotExist(err)
	}, 5*time.Second, 5*time.Millisecond)

	stop()
	stop()
}
