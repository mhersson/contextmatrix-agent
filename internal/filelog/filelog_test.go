package filelog

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBeginWriteEnd(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	l.Begin("proj", "CARD-1", "abcdef0123456789")
	l.Write("proj", "CARD-1", []byte(`{"kind":"model_response"}`), false)
	l.Write("proj", "CARD-1", []byte("time=T level=INFO msg=claimed"), true)
	l.End("proj", "CARD-1", 0)

	data, err := os.ReadFile(filepath.Join(dir, "proj", "card-1.log"))
	require.NoError(t, err)

	s := string(data)

	assert.Contains(t, s, "==== run started ")
	assert.Contains(t, s, "container=abcdef012345") // truncated to 12
	assert.Contains(t, s, `{"kind":"model_response"}`+"\n")
	assert.Contains(t, s, "time=T level=INFO msg=claimed\n")
	assert.Contains(t, s, "==== run ended ")
	assert.Contains(t, s, "exit=0")
}

func TestResumeAppends(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	l.Begin("p", "C-1", "cid-run-1")
	l.Write("p", "C-1", []byte("run one line"), false)
	l.End("p", "C-1", 0)

	l.Begin("p", "C-1", "cid-run-2")
	l.Write("p", "C-1", []byte("run two line"), false)
	l.End("p", "C-1", 1)

	data, err := os.ReadFile(filepath.Join(dir, "p", "c-1.log"))
	require.NoError(t, err)

	s := string(data)

	assert.Equal(t, 2, strings.Count(s, "==== run started "))
	assert.Less(t, strings.Index(s, "run one line"), strings.Index(s, "run two line"))
	assert.Contains(t, s, "exit=1")
}

func TestConcurrentCardsSeparateFiles(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	var wg sync.WaitGroup

	for i := range 8 {
		card := fmt.Sprintf("CARD-%d", i)

		wg.Add(1)
		go func() {
			defer wg.Done()

			l.Begin("p", card, "cid")

			for range 50 {
				l.Write("p", card, []byte(card+" line"), false)
			}

			l.End("p", card, 0)
		}()
	}

	wg.Wait()

	for i := range 8 {
		data, err := os.ReadFile(filepath.Join(dir, "p", fmt.Sprintf("card-%d.log", i)))
		require.NoError(t, err)

		s := string(data)
		assert.Equal(t, 50, strings.Count(s, fmt.Sprintf("CARD-%d line", i)))

		for k := range 8 {
			if k == i {
				continue
			}

			assert.NotContains(t, s, fmt.Sprintf("CARD-%d line", k))
		}
	}
}

func TestDisabledAndNilAreNoops(t *testing.T) {
	dir := t.TempDir()

	disabled := New("", testLogger())

	assert.NotPanics(t, func() {
		disabled.Begin("p", "C", "cid")
		disabled.Write("p", "C", []byte("x"), false)
		disabled.End("p", "C", 0)
	})

	var nilLogger *Logger

	assert.NotPanics(t, func() {
		nilLogger.Begin("p", "C", "cid")
		nilLogger.Write("p", "C", []byte("x"), false)
		nilLogger.End("p", "C", 0)
	})

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestUnwritableRootSwallowed(t *testing.T) {
	base := t.TempDir()
	notADir := filepath.Join(base, "root")
	require.NoError(t, os.WriteFile(notADir, []byte("x"), 0o600)) // a file, not a dir

	l := New(notADir, testLogger())

	assert.NotPanics(t, func() {
		l.Begin("proj", "C-1", "cid")
		l.Write("proj", "C-1", []byte("line"), false)
		l.End("proj", "C-1", 0)
	})
}

func TestSanitizePreventsTraversal(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	l.Begin("..", "..", "cid")
	l.Write("..", "..", []byte("evil"), false)
	l.End("..", "..", 0)

	// ".." sanitizes to "--"; the file stays inside dir.
	_, err := os.Stat(filepath.Join(dir, "--", "--.log"))
	require.NoError(t, err)
}

func TestSanitize(t *testing.T) {
	assert.Equal(t, "ctxagent-015", sanitize("CTXAGENT-015"))
	assert.Equal(t, "contextmatrix-agent", sanitize("contextmatrix-agent"))
	assert.Equal(t, "--", sanitize(".."))
	assert.Equal(t, "a-b-c", sanitize("a/b\\c"))
}
