package orchestrator

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureReadRootsLog swaps slog.Default for a text handler writing to buf and
// restores it on cleanup - the pattern already used in logcard_test.go.
func captureReadRootsLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	prev := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return &buf
}

// TestLogReadRootsOutcomeNoRootsNoLog: an empty outcome (no roots declared)
// must produce nothing, matching the pre-v0.19.0 behavior of the function this
// replaces.
func TestLogReadRootsOutcomeNoRootsNoLog(t *testing.T) {
	buf := captureReadRootsLog(t)

	LogReadRootsOutcome("CMX-001", t.TempDir(), tools.ReadRoots{})

	assert.Empty(t, buf.String(), "no declared roots means nothing widened or dropped - nothing to log")
}

// TestLogReadRootsOutcomeReportsEffectiveAndDroppedWithReason: an operator
// whose declared prefix is wrong must be able to see the dropped root AND its
// category, not just silence; a surviving root must show as effective. Level
// escalates to warn only when something was actually dropped.
func TestLogReadRootsOutcomeReportsEffectiveAndDroppedWithReason(t *testing.T) {
	buf := captureReadRootsLog(t)

	rr := tools.ReadRoots{
		Effective: []string{filepath.FromSlash("/srv/deps")},
		Dropped: []tools.DroppedReadRoot{
			{Root: "relative/path", Reason: tools.DropReasonRelative},
		},
	}

	LogReadRootsOutcome("CMX-001", "/workspace", rr)

	logged := buf.String()
	assert.Contains(t, logged, "level=WARN", "a drop must be reported at warning level")
	assert.Contains(t, logged, "CMX-001")
	assert.Contains(t, logged, "/srv/deps", "the surviving root must appear as effective")
	assert.Contains(t, logged, "relative/path", "the dropped root must be named")
	assert.Contains(t, logged, string(tools.DropReasonRelative), "the drop reason must be named")
}

// TestLogReadRootsOutcomeNoDropsStaysInfo: an outcome with only effective
// roots and nothing dropped is not warning-worthy.
func TestLogReadRootsOutcomeNoDropsStaysInfo(t *testing.T) {
	buf := captureReadRootsLog(t)

	LogReadRootsOutcome("CMX-001", "/workspace", tools.ReadRoots{Effective: []string{"/srv/deps"}})

	logged := buf.String()
	require.NotEmpty(t, logged)
	assert.Contains(t, logged, "level=INFO")
	assert.NotContains(t, logged, "level=WARN")
}

// TestLogReadRootsOutcomeDedupesIdenticalOutcome: readOnlyToolsWithRoots is
// invoked from two Deps fields built from the same declaration, and a
// PlanTools/WriteToolsForDir closure can rebuild the same tools again later in
// the same run - so an identical (identity, outcome) pair must log once, not
// once per construction. A genuinely different workspace is a different
// outcome and must still get its own line.
func TestLogReadRootsOutcomeDedupesIdenticalOutcome(t *testing.T) {
	buf := captureReadRootsLog(t)

	ws := t.TempDir()
	rr := tools.ReadRoots{Effective: []string{filepath.Join(ws, "vendor")}}

	LogReadRootsOutcome("CMX-100", ws, rr)

	first := buf.String()
	require.NotEmpty(t, first, "the first construction with this outcome must log")

	buf.Reset()
	LogReadRootsOutcome("CMX-100", ws, rr)
	assert.Empty(t, buf.String(), "an identical (identity, outcome) pair must not log again")

	buf.Reset()
	LogReadRootsOutcome("CMX-100", t.TempDir(), rr)
	assert.NotEmpty(t, buf.String(), "a different workspace is a different outcome and must still log")
}
