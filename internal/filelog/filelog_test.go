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

	l.Begin("proj", "CARD-1", "abcdef0123456789", "corr-1")
	l.Write("proj", "CARD-1", "corr-1", []byte(`{"kind":"model_response"}`), false)
	l.Write("proj", "CARD-1", "corr-1", []byte("time=T level=INFO msg=claimed"), true)
	l.End("proj", "CARD-1", "corr-1", 0, "normal")

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

	l.Begin("p", "C-1", "cid-run-1", "corr-1")
	l.Write("p", "C-1", "corr-1", []byte("run one line"), false)
	l.End("p", "C-1", "corr-1", 0, "normal")

	l.Begin("p", "C-1", "cid-run-2", "corr-2")
	l.Write("p", "C-1", "corr-2", []byte("run two line"), false)
	l.End("p", "C-1", "corr-2", 1, "normal")

	data, err := os.ReadFile(filepath.Join(dir, "p", "c-1.log"))
	require.NoError(t, err)

	s := string(data)

	assert.Equal(t, 2, strings.Count(s, "==== run started "))
	assert.Less(t, strings.Index(s, "run one line"), strings.Index(s, "run two line"))
	assert.Contains(t, s, "exit=1")
}

func TestBeginSupersedesUnclosedRun(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	l.Begin("p", "C-1", "run-one", "corr-1")
	l.Write("p", "C-1", "corr-1", []byte("first run line"), false)
	// No End for the first run - its entry is still open.
	l.Begin("p", "C-1", "run-two", "corr-1")
	l.Write("p", "C-1", "corr-1", []byte("second run line"), false)
	l.End("p", "C-1", "corr-1", 0, "normal")

	data, err := os.ReadFile(filepath.Join(dir, "p", "c-1.log"))
	require.NoError(t, err)

	s := string(data)

	assert.Equal(t, 2, strings.Count(s, "==== run started "))
	assert.Equal(t, 2, strings.Count(s, "==== run ended "))
	assert.Contains(t, s, "exit=-1") // superseded first run
	assert.Contains(t, s, "exit=0")  // clean second run
	assert.Less(t, strings.Index(s, "first run line"), strings.Index(s, "second run line"))
}

func TestConcurrentCardsSeparateFiles(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	var wg sync.WaitGroup

	for i := range 8 {
		card := fmt.Sprintf("CARD-%d", i)

		wg.Go(func() {
			l.Begin("p", card, "cid", "corr-1")

			for range 50 {
				l.Write("p", card, "corr-1", []byte(card+" line"), false)
			}

			l.End("p", card, "corr-1", 0, "normal")
		})
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
		disabled.Begin("p", "C", "cid", "corr-1")
		disabled.Write("p", "C", "corr-1", []byte("x"), false)
		disabled.End("p", "C", "corr-1", 0, "normal")
	})

	var nilLogger *Logger

	assert.NotPanics(t, func() {
		nilLogger.Begin("p", "C", "cid", "corr-1")
		nilLogger.Write("p", "C", "corr-1", []byte("x"), false)
		nilLogger.End("p", "C", "corr-1", 0, "normal")
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
		l.Begin("proj", "C-1", "cid", "corr-1")
		l.Write("proj", "C-1", "corr-1", []byte("line"), false)
		l.End("proj", "C-1", "corr-1", 0, "normal")
	})
}

func TestSanitizePreventsTraversal(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	l.Begin("..", "..", "cid", "corr-1")
	l.Write("..", "..", "corr-1", []byte("evil"), false)
	l.End("..", "..", "corr-1", 0, "normal")

	// ".." sanitizes to "--"; the file stays inside dir.
	_, err := os.Stat(filepath.Join(dir, "--", "--.log"))
	require.NoError(t, err)
}

func TestSanitize(t *testing.T) {
	assert.Equal(t, "ctxagent-015", sanitize("CTXAGENT-015"))
	assert.Equal(t, "contextmatrix-agent", sanitize("contextmatrix-agent"))
	assert.Equal(t, "--", sanitize(".."))
	assert.Equal(t, "a-b-c", sanitize("a/b\\c"))
	assert.Equal(t, "-", sanitize(""))
}

func TestFilePermissions(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	l.Begin("proj", "CARD-1", "cid", "corr-1")
	l.Write("proj", "CARD-1", "corr-1", []byte("line"), false)
	l.End("proj", "CARD-1", "corr-1", 0, "normal")

	fi, err := os.Stat(filepath.Join(dir, "proj", "card-1.log"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	di, err := os.Stat(filepath.Join(dir, "proj"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), di.Mode().Perm())
}

func TestNextAttemptCountsRunHeaders(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	assert.Equal(t, 1, l.NextAttempt("proj", "CARD-1"), "a card with no log has never run")

	l.Begin("proj", "CARD-1", "aaaaaaaaaaaa", "corr-1")
	l.Write("proj", "CARD-1", "corr-1", []byte(`{"seq":1,"kind":"state_change"}`), false)
	l.End("proj", "CARD-1", "corr-1", 0, "normal")

	assert.Equal(t, 2, l.NextAttempt("proj", "CARD-1"))

	l.Begin("proj", "CARD-1", "bbbbbbbbbbbb", "corr-2")
	l.End("proj", "CARD-1", "corr-2", 137, "normal")

	assert.Equal(t, 3, l.NextAttempt("proj", "CARD-1"))
	assert.Equal(t, 1, l.NextAttempt("proj", "CARD-2"), "the count is per card")
	assert.Equal(t, 1, l.NextAttempt("other", "CARD-1"), "the count is per project")
}

// A container that dies leaves a header with no footer - exactly the restart
// case the ordinal exists for - so the count must include it.
func TestNextAttemptCountsAnUnclosedRun(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	l.Begin("proj", "CARD-1", "aaaaaaaaaaaa", "corr-1")

	assert.Equal(t, 2, l.NextAttempt("proj", "CARD-1"))
}

// Container output lands in the same file as the headers, so a line that
// merely quotes the header text must not be counted as a run.
func TestNextAttemptCountsOnlyLineLeadingHeaders(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	l.Begin("proj", "CARD-1", "aaaaaaaaaaaa", "corr-1")
	l.Write("proj", "CARD-1", "corr-1", []byte(`{"seq":1,"data":{"text":"==== run started 2026-08-25T00:00:00Z container=x ===="}}`), false)
	l.Write("proj", "CARD-1", "corr-1", []byte("tail of the log: ==== run started later ===="), true)
	l.End("proj", "CARD-1", "corr-1", 0, "normal")

	assert.Equal(t, 2, l.NextAttempt("proj", "CARD-1"))
}

// Transcript lines can be far larger than any fixed scan buffer (tool output
// alone defaults to 128 KB), so the count must not depend on line length.
func TestNextAttemptCountsPastVeryLongLines(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	l.Begin("proj", "CARD-1", "aaaaaaaaaaaa", "corr-1")
	l.Write("proj", "CARD-1", "corr-1", []byte(`{"data":"`+strings.Repeat("x", 256<<10)+`"}`), false)
	l.End("proj", "CARD-1", "corr-1", 0, "normal")

	l.Begin("proj", "CARD-1", "bbbbbbbbbbbb", "corr-2")
	l.Write("proj", "CARD-1", "corr-2", []byte(strings.Repeat("y", 300<<10)), true)
	l.End("proj", "CARD-1", "corr-2", 0, "normal")

	assert.Equal(t, 3, l.NextAttempt("proj", "CARD-1"))
}

func TestNextAttemptDisabledAndNilAreFirstAttempt(t *testing.T) {
	var nilLogger *Logger

	assert.Equal(t, 1, nilLogger.NextAttempt("proj", "CARD-1"))
	assert.Equal(t, 1, New("", testLogger()).NextAttempt("proj", "CARD-1"))
}

// TestFooterRecordsTheCause pins the datum the exit code cannot carry: both
// kill paths report -1, so only the cause says which one ended the run.
func TestFooterRecordsTheCause(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int64
		cause    string
	}{
		{"normal", 0, "normal"},
		{"timeout kill", -1, "timeout"},
		{"wait failure", -1, "wait_failure"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l := New(dir, testLogger())

			l.Begin("proj", "CARD-1", "abcdef012345", "corr-1")
			l.End("proj", "CARD-1", "corr-1", tc.exitCode, tc.cause)

			data, err := os.ReadFile(filepath.Join(dir, "proj", "card-1.log"))
			require.NoError(t, err)

			s := string(data)

			assert.Contains(t, s, fmt.Sprintf("exit=%d", tc.exitCode))
			assert.Contains(t, s, "cause="+tc.cause)
		})
	}
}

// TestSupersededRunFooterIsNotAnObservedCause covers the one footer no exit
// path writes: a prior run still open when a new one begins. Nothing observed
// how it ended, so it must not read as a timeout or a wait failure.
func TestSupersededRunFooterIsNotAnObservedCause(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	l.Begin("proj", "CARD-1", "aaaaaaaaaaaa", "corr-1")
	l.Begin("proj", "CARD-1", "bbbbbbbbbbbb", "corr-1") // no End: the first run is superseded

	data, err := os.ReadFile(filepath.Join(dir, "proj", "card-1.log"))
	require.NoError(t, err)

	s := string(data)

	require.Equal(t, 1, strings.Count(s, "==== run ended "), "the superseded run is footered")
	assert.Contains(t, s, "cause="+causeSuperseded)
	assert.NotContains(t, s, "cause=timeout")
	assert.NotContains(t, s, "cause=wait_failure")
	assert.NotContains(t, s, "cause=normal")
}

// TestStaleEndCannotCloseFreshRunWriter pins the failure keying by
// (project, cardID) alone used to allow: run 1's late End - an exit callback
// still draining during the pump-drain window while run 2 is already live -
// closed run 2's just-opened writer, so run 2's transcript was silently
// dropped. With open writers keyed by correlation id, run 1's stale End is a
// no-op on run 2's state; only run 2's own End footers and closes.
func TestStaleEndCannotCloseFreshRunWriter(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	l.Begin("proj", "CARD-1", "aaaaaaaaaaaa", "corr-2")
	l.Write("proj", "CARD-1", "corr-2", []byte("run two line"), false)

	// Run 1's exit callback fires late, carrying run 1's id.
	l.End("proj", "CARD-1", "corr-1", 0, "normal")

	// Run 2's writer survived: a further Write still lands in the file.
	l.Write("proj", "CARD-1", "corr-2", []byte("run two keeps writing"), false)

	data, err := os.ReadFile(filepath.Join(dir, "proj", "card-1.log"))
	require.NoError(t, err)

	s := string(data)
	assert.NotContains(t, s, "==== run ended ", "a stale End must not footer the live run")
	assert.Contains(t, s, "run two line\n")
	assert.Contains(t, s, "run two keeps writing\n", "the live run's writer is still open after the stale End")

	// Run 2's own End still footers and closes.
	l.End("proj", "CARD-1", "corr-2", 0, "normal")

	data, err = os.ReadFile(filepath.Join(dir, "proj", "card-1.log"))
	require.NoError(t, err)

	s = string(data)
	assert.Contains(t, s, "==== run ended ")
	assert.NotContains(t, s, "run two dropped after close\n")

	l.Write("proj", "CARD-1", "corr-2", []byte("run two dropped after close"), false)

	data, err = os.ReadFile(filepath.Join(dir, "proj", "card-1.log"))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "run two dropped after close")
}

// TestStaleWriteCannotReachFreshRunWriter is the Write-side twin of the stale
// End pin: run 1's trailing pump output must not land in run 2's transcript.
func TestStaleWriteCannotReachFreshRunWriter(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	l.Begin("proj", "CARD-1", "aaaaaaaaaaaa", "corr-2")

	l.Write("proj", "CARD-1", "corr-1", []byte("run one trailing line"), false)
	l.Write("proj", "CARD-1", "corr-2", []byte("run two line"), false)

	data, err := os.ReadFile(filepath.Join(dir, "proj", "card-1.log"))
	require.NoError(t, err)

	s := string(data)
	assert.Contains(t, s, "run two line\n")
	assert.NotContains(t, s, "run one trailing line", "a stale run's Write must not reach another run's writer")
}

func TestTwoRunsOnOneCardEachFooterTheirOwnWriter(t *testing.T) {
	dir := t.TempDir()
	l := New(dir, testLogger())

	// Run 1 is still draining when CM re-triggers the card as run 2.
	l.Begin("proj", "CARD-1", "aaaaaaaaaaaa", "corr-1")
	l.Begin("proj", "CARD-1", "bbbbbbbbbbbb", "corr-2")

	l.Write("proj", "CARD-1", "corr-1", []byte("run one tail"), false)
	l.End("proj", "CARD-1", "corr-1", 0, "normal")

	l.Write("proj", "CARD-1", "corr-2", []byte("run two line"), false)
	l.End("proj", "CARD-1", "corr-2", 0, "normal")

	data, err := os.ReadFile(filepath.Join(dir, "proj", "card-1.log"))
	require.NoError(t, err)

	s := string(data)
	assert.Equal(t, 2, strings.Count(s, "==== run started "))
	assert.Equal(t, 2, strings.Count(s, "cause=normal"), "each run footers its own writer with its real cause")
	assert.NotContains(t, s, "cause=superseded")
	assert.Contains(t, s, "run one tail\n", "the draining run's tail reaches the durable log")
	assert.Contains(t, s, "run two line\n")
}
