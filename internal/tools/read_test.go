package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadToolWholeFileAndSlice(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("a\nb\nc\nd\n"), 0o644))
	rt := NewReadTool(root)

	out, err := rt.Execute(context.Background(), map[string]any{"path": "f.txt"})
	require.NoError(t, err)
	assert.Equal(t, "a\nb\nc\nd\n", out)

	out, err = rt.Execute(context.Background(), map[string]any{"path": "f.txt", "offset": 2.0, "limit": 2.0})
	require.NoError(t, err)
	assert.Equal(t, "b\nc\n", out)

	_, err = rt.Execute(context.Background(), map[string]any{})
	require.Error(t, err) // missing required path
}

func TestReadToolEmptyFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "__init__.py"), []byte{}, 0o644))
	rt := NewReadTool(root)

	out, err := rt.Execute(context.Background(), map[string]any{"path": "__init__.py"})
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestReadToolBinaryFileReturnsDescription(t *testing.T) {
	root := t.TempDir()
	// ELF-like header with embedded NUL byte — clearly binary
	binaryContent := []byte{0x7f, 'E', 'L', 'F', 0x00, 0x01, 0x02, 0x03, 0x04, 0x05}
	require.NoError(t, os.WriteFile(filepath.Join(root, "program"), binaryContent, 0o755))
	rt := NewReadTool(root)

	out, err := rt.Execute(context.Background(), map[string]any{"path": "program"})
	require.NoError(t, err) // must NOT return an error
	assert.Contains(t, out, "[binary file:")
	assert.Contains(t, out, "program")
	assert.Contains(t, out, "not shown")
	// summary must report the byte size
	assert.Contains(t, out, "bytes")
	assert.Contains(t, out, fmt.Sprintf("%d", len(binaryContent)))
	// must not contain raw binary bytes
	assert.NotContains(t, out, string([]byte{0x7f, 'E', 'L', 'F', 0x00}))

	// Also test a file that simply contains a NUL inline
	mixedContent := []byte("text before\x00more text after")
	require.NoError(t, os.WriteFile(filepath.Join(root, "mixed.bin"), mixedContent, 0o644))

	out, err = rt.Execute(context.Background(), map[string]any{"path": "mixed.bin"})
	require.NoError(t, err)
	assert.Contains(t, out, "[binary file:")
	assert.Contains(t, out, "mixed.bin")
}

func TestReadToolLargeFilePaginates(t *testing.T) {
	root := t.TempDir()

	// Write 3000 numbered lines
	var sb strings.Builder
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}

	require.NoError(t, os.WriteFile(filepath.Join(root, "big.txt"), []byte(sb.String()), 0o644))
	rt := NewReadTool(root)

	// First call: no offset/limit — should return first readMaxLines lines + hint
	out, err := rt.Execute(context.Background(), map[string]any{"path": "big.txt"})
	require.NoError(t, err)
	assert.Contains(t, out, "line 1\n")
	assert.Contains(t, out, fmt.Sprintf("line %d\n", readMaxLines))
	// Must NOT contain a line beyond readMaxLines
	assert.NotContains(t, out, fmt.Sprintf("line %d\n", readMaxLines+1))
	// Must contain a pagination hint telling the model what offset to use
	assert.Contains(t, out, fmt.Sprintf("offset=%d", readMaxLines+1))

	// Second call: offset=readMaxLines+1 — should return remaining lines, NO hint
	out, err = rt.Execute(context.Background(), map[string]any{"path": "big.txt", "offset": float64(readMaxLines + 1)})
	require.NoError(t, err)
	assert.Contains(t, out, fmt.Sprintf("line %d\n", readMaxLines+1))
	assert.Contains(t, out, "line 3000\n")
	// No hint when we've consumed all lines
	assert.NotContains(t, out, "offset=")
}

func TestReadToolOverLongSingleLine(t *testing.T) {
	root := t.TempDir()

	// One very long line (~200 KB), no NUL — must not be misclassified as binary
	lineLen := 200 * 1024
	longLine := strings.Repeat("x", lineLen) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "minified.js"), []byte(longLine), 0o644))
	rt := NewReadTool(root)

	out, err := rt.Execute(context.Background(), map[string]any{"path": "minified.js"})
	require.NoError(t, err)
	// Must not classify as binary (no NUL present)
	assert.NotContains(t, out, "[binary file:")
	// Output should be capped to readMaxBytes worth of data
	assert.LessOrEqual(t, len(out), readMaxBytes+512) // allow for the marker line overhead
	// The marker must be concrete and honest about retrievability.
	assert.Contains(t, out, "bytes shown")
	assert.Contains(t, out, "not retrievable")
	// This is the last content — must NOT offer a continuation offset.
	assert.NotContains(t, out, "offset=")
	// Reports actual byte counts (shown / total).
	assert.Contains(t, out, fmt.Sprintf("%d", readMaxBytes))
	// lines[start] includes the trailing newline, so total is lineLen+1.
	assert.Contains(t, out, fmt.Sprintf("%d", lineLen+1))
}

func TestReadToolOverLongLineFollowedByNormalLines(t *testing.T) {
	root := t.TempDir()

	// An over-long first line, then several normal lines.
	lineLen := 200 * 1024

	var sb strings.Builder
	sb.WriteString(strings.Repeat("x", lineLen))
	sb.WriteString("\n")
	sb.WriteString("second line\n")
	sb.WriteString("third line\n")
	sb.WriteString("fourth line\n")
	require.NoError(t, os.WriteFile(filepath.Join(root, "mixed.txt"), []byte(sb.String()), 0o644))
	rt := NewReadTool(root)

	out, err := rt.Execute(context.Background(), map[string]any{"path": "mixed.txt"})
	require.NoError(t, err)
	assert.NotContains(t, out, "[binary file:")
	// Byte-capped: at most readMaxBytes of payload plus the marker.
	assert.LessOrEqual(t, len(out), readMaxBytes+512)
	// Honest marker: the over-long line's remainder is not retrievable, but the
	// following lines are reachable via the offered offset.
	assert.Contains(t, out, "not retrievable")
	assert.Contains(t, out, "following lines")
	// The over-long line is line 1; following lines start at line 2.
	assert.Contains(t, out, "offset=2")
	// Must NOT have leaked the trailing normal lines into this first page.
	assert.NotContains(t, out, "second line")

	// Reading at the offered offset returns the FOLLOWING lines (not the lost tail).
	out, err = rt.Execute(context.Background(), map[string]any{"path": "mixed.txt", "offset": 2.0})
	require.NoError(t, err)
	assert.Contains(t, out, "second line\n")
	assert.Contains(t, out, "third line\n")
	assert.Contains(t, out, "fourth line\n")
	// The remainder of the over-long line (a run of x's) must not reappear.
	assert.NotContains(t, out, strings.Repeat("x", 1000))
}

func TestReadToolOffsetPastEOF(t *testing.T) {
	root := t.TempDir()

	var sb strings.Builder
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}

	require.NoError(t, os.WriteFile(filepath.Join(root, "big.txt"), []byte(sb.String()), 0o644))
	rt := NewReadTool(root)

	out, err := rt.Execute(context.Background(), map[string]any{"path": "big.txt", "offset": 9999.0})
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.NotContains(t, out, "offset=")
}

func TestReadToolHugeLimitDoesNotEmpty(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("a\nb\nc\nd\n"), 0o644))
	rt := NewReadTool(root)

	// A pathological limit (1e19 as a JSON float) converts to a negative int
	// (math.MinInt64), which must NOT overflow start+limit negative and either
	// panic or silently empty the read.
	out, err := rt.Execute(context.Background(), map[string]any{"path": "f.txt", "limit": 1e19})
	require.NoError(t, err)
	assert.Equal(t, "a\nb\nc\nd\n", out)
}
