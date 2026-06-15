package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegressionBinaryAndLargeTextBounded is the end-to-end regression proof
// for the 8.6 MB binary read crash. A model that reads a large binary artifact
// and then a large text file must receive bounded tool results, and the run
// must complete without error.
//
// The real read tool is used — no mocking. The harness cap (ToolOutputMaxBytes)
// and the read tool's binary refusal and pagination are exercised together.
func TestRegressionBinaryAndLargeTextBounded(t *testing.T) {
	root := t.TempDir()

	// Plant a large "binary" artifact — 400 KB of NUL bytes (ELF-like), well
	// above both the harness cap (131072) and the read tool's byte ceiling.
	const binarySize = 400 * 1024

	binary := make([]byte, binarySize)
	binary[0] = 0x7F // ELF magic prefix for realism
	binary[1] = 'E'
	binary[2] = 'L'
	binary[3] = 'F'
	// Remaining bytes are 0x00 (NUL) — satisfies the binary detector.
	require.NoError(t, os.WriteFile(filepath.Join(root, "sysinfo"), binary, 0o755))

	// Plant a large text file — 3000 numbered lines, exceeding the 2000-line
	// default page size.
	var sb strings.Builder
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&sb, "line %d: the quick brown fox jumps over the lazy dog\n", i)
	}

	require.NoError(t, os.WriteFile(filepath.Join(root, "output.log"), []byte(sb.String()), 0o644))

	reg := tools.NewRegistry(tools.NewReadTool(root))

	// capturingLLMSeq records all requests AND plays scripted responses:
	//   turn 1 — tool_call read on the binary file
	//   turn 2 — tool_call read on the large text file
	//   turn 3 — no tool calls (finish/done)
	capt := &capturingLLMSeq{
		responses: []llm.Response{
			{ToolCalls: []llm.ToolCall{toolCall("bin1", "read", `{"path":"sysinfo"}`)}},
			{ToolCalls: []llm.ToolCall{toolCall("txt2", "read", `{"path":"output.log"}`)}},
			{Content: "all done", FinishReason: "stop"},
		},
	}

	// Use a generous context window so we assert on boundedness, not a
	// context_limit trip.
	res, err := Run(context.Background(), capt, reg, newEmitter(), "analyse workspace", Config{
		MaxTurns:           10,
		ToolOutputMaxBytes: 131072,
		ContextWindow:      1_000_000,
		Model:              "fake",
	})

	// The run must complete — no error, no overflow.
	require.NoError(t, err)
	assert.True(t, res.Completed, "run must complete; reason: %s", res.Reason)
	assert.Equal(t, "done", res.Reason)

	// We expect 3 requests:
	//   req[0] — initial user message (no prior tool results)
	//   req[1] — carries binary tool-result from turn 1
	//   req[2] — carries large-text tool-result from turn 2
	require.GreaterOrEqual(t, len(capt.requests), 3, "expected at least 3 LLM requests")

	// --- binary tool-result assertion ---
	// The second request carries the result of reading the binary file.
	binResult, ok := findToolResult(capt.requests[1].Messages, "bin1")
	require.True(t, ok, "binary tool-result message not found in request 2")

	// Must be the short summary, not the raw bytes.
	assert.Contains(t, binResult, "binary file:", "binary result must contain the refusal summary")
	assert.Contains(t, binResult, "not shown", "binary result must say content is not shown")

	// Must NOT contain NUL bytes — the whole point of the fix.
	assert.NotContains(t, binResult, "\x00", "binary result must not contain NUL bytes")

	// Must be tiny — well under 1 KB.
	assert.Less(t, len(binResult), 1024, "binary result must be small (< 1 KB), got %d bytes", len(binResult))

	// --- large text tool-result assertion ---
	// The third request carries the result of reading the large text file.
	txtResult, ok := findToolResult(capt.requests[2].Messages, "txt2")
	require.True(t, ok, "large-text tool-result message not found in request 3")

	// Must be bounded by the harness cap.
	const capBytes = 131072
	assert.LessOrEqual(t, len(txtResult), capBytes+200,
		"large-text result must be bounded by ToolOutputMaxBytes (+ marker allowance), got %d bytes", len(txtResult))

	// Must contain a pagination or truncation hint indicating more content exists.
	hasPaginationHint := strings.Contains(txtResult, "offset=")
	hasTruncationHint := strings.Contains(txtResult, "truncated")
	assert.True(t, hasPaginationHint || hasTruncationHint,
		"large-text result must contain a pagination or truncation hint")

	// Sanity: tool call count covers both reads.
	assert.GreaterOrEqual(t, res.ToolCallCount, 2, "both tool calls must have been dispatched")
}
