package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedLLM is deterministic under parallelism: it decides its reply from the
// conversation shape (not a shared counter), so multiple children can share it.
type scriptedLLM struct {
	tool  string
	args  string
	final string
}

func (s *scriptedLLM) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	return s.SendStream(ctx, req, nil)
}

func (s *scriptedLLM) SendStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	last := req.Messages[len(req.Messages)-1]
	if last.Role == "tool" {
		return llm.Response{Content: s.final, FinishReason: "stop"}, nil
	}

	return llm.Response{ToolCalls: []llm.ToolCall{toolCall("1", s.tool, s.args)}}, nil
}

func TestSpawnSubagentsParallelReadOnly(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("hello"), 0o644))

	scripted := &scriptedLLM{tool: "read", args: `{"path":"f.txt"}`, final: "the file says hello"}
	specs := []SubagentSpec{
		{Role: "reader-a", Prompt: "summarize f.txt"},
		{Role: "reader-b", Prompt: "summarize f.txt"},
	}
	results, err := SpawnSubagents(context.Background(), scripted, root, newEmitter(), specs,
		SubagentOpts{Depth: 0, MaxDepth: 2, DefaultModel: "test/model"})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "reader-a", results[0].Role)
	assert.Equal(t, "reader-b", results[1].Role)

	for _, r := range results {
		require.NoError(t, r.Err)
		assert.True(t, r.Result.Completed)
		assert.Equal(t, "the file says hello", r.Output)
	}
}

func TestSpawnSubagentsDepthCap(t *testing.T) {
	_, err := SpawnSubagents(context.Background(), &scriptedLLM{}, t.TempDir(), newEmitter(),
		[]SubagentSpec{{Role: "x", Prompt: "y"}},
		SubagentOpts{Depth: 2, MaxDepth: 2, DefaultModel: "m"})
	require.Error(t, err)
}

func TestSpawnSubagentsChildrenAreReadOnly(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("orig"), 0o644))
	// Child tries to edit; the read-only registry has no "edit" tool.
	scripted := &scriptedLLM{tool: "edit", args: `{"path":"f.txt","old_string":"orig","new_string":"hacked"}`, final: "tried"}
	results, err := SpawnSubagents(context.Background(), scripted, root, newEmitter(),
		[]SubagentSpec{{Role: "writer", Prompt: "edit it"}},
		SubagentOpts{MaxDepth: 2, DefaultModel: "m"})
	require.NoError(t, err)
	assert.Equal(t, 1, results[0].Result.ToolCallFailures) // "edit" is unknown in a read-only registry

	b, _ := os.ReadFile(filepath.Join(root, "f.txt"))
	assert.Equal(t, "orig", string(b)) // untouched
}

// capturingScriptedLLM uses the same per-role decision as scriptedLLM (last
// message role) and records all requests. The mutex guards only the requests
// slice; tool/args/final are set once before fan-out and read-only thereafter.
// The recorded order is therefore only meaningful for single-spec fan-outs.
type capturingScriptedLLM struct {
	mu       sync.Mutex
	requests []llm.Request
	tool     string
	args     string
	final    string
}

func (c *capturingScriptedLLM) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	return c.SendStream(ctx, req, nil)
}

func (c *capturingScriptedLLM) SendStream(_ context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()

	last := req.Messages[len(req.Messages)-1]
	if last.Role == "tool" {
		return llm.Response{Content: c.final, FinishReason: "stop"}, nil
	}

	return llm.Response{ToolCalls: []llm.ToolCall{toolCall("1", c.tool, c.args)}}, nil
}

// TestSpawnSubagentsRedactPropagates proves that SubagentOpts.RedactToolOutput
// is forwarded into the child Config. A secret planted in a file must be masked
// in the tool-result message the child sends back to the model.
func TestSpawnSubagentsRedactPropagates(t *testing.T) {
	const secret = "TOKEN-ABC123"

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret.txt"), []byte(secret), 0o644))

	capt := &capturingScriptedLLM{
		tool:  "read",
		args:  `{"path":"secret.txt"}`,
		final: "all done",
	}

	redact := func(s string) string {
		return strings.ReplaceAll(s, secret, "[REDACTED]")
	}

	_, err := SpawnSubagents(context.Background(), capt, root, newEmitter(),
		[]SubagentSpec{{Role: "reviewer", Prompt: "read the secret file"}},
		SubagentOpts{
			MaxDepth:         2,
			DefaultModel:     "test/model",
			RedactToolOutput: redact,
		},
	)
	require.NoError(t, err)

	// The second request carries the tool-result message produced after the read.
	capt.mu.Lock()
	reqs := capt.requests
	capt.mu.Unlock()

	require.GreaterOrEqual(t, len(reqs), 2, "expected at least 2 requests (tool call + final)")
	secondReq := reqs[1]

	var toolResultContent string

	for _, m := range secondReq.Messages {
		if m.Role == "tool" && m.ToolCallID == "1" {
			toolResultContent = m.Content

			break
		}
	}

	require.NotEmpty(t, toolResultContent, "tool-result message not found in second request")
	assert.Contains(t, toolResultContent, "[REDACTED]", "secret must be masked in child tool-result message")
	assert.NotContains(t, toolResultContent, secret, "raw secret must not reach the child model")
}

// TestSpawnSubagentsToolOutputCapPropagates proves that
// SubagentOpts.ToolOutputMaxBytes is forwarded into the child Config. A large
// file read by a child must be size-capped before it reaches the child model.
func TestSpawnSubagentsToolOutputCapPropagates(t *testing.T) {
	const maxBytes = 1000

	root := t.TempDir()
	large := strings.Repeat("X", 100000) // 100 KB, far above the cap
	require.NoError(t, os.WriteFile(filepath.Join(root, "big.txt"), []byte(large), 0o644))

	capt := &capturingScriptedLLM{
		tool:  "read",
		args:  `{"path":"big.txt"}`,
		final: "all done",
	}

	_, err := SpawnSubagents(context.Background(), capt, root, newEmitter(),
		[]SubagentSpec{{Role: "reviewer", Prompt: "read the big file"}},
		SubagentOpts{
			MaxDepth:           2,
			DefaultModel:       "test/model",
			ToolOutputMaxBytes: maxBytes,
		},
	)
	require.NoError(t, err)

	capt.mu.Lock()
	reqs := capt.requests
	capt.mu.Unlock()

	require.GreaterOrEqual(t, len(reqs), 2, "expected at least 2 requests (tool call + final)")

	var toolResultContent string

	for _, m := range reqs[1].Messages {
		if m.Role == "tool" && m.ToolCallID == "1" {
			toolResultContent = m.Content

			break
		}
	}

	require.NotEmpty(t, toolResultContent, "tool-result message not found in second request")
	assert.Contains(t, toolResultContent, "bytes truncated", "child tool output must be size-capped")
	assert.LessOrEqual(t, len(toolResultContent), maxBytes+80, "child tool output must be bounded by the cap") // marker allowance
}
