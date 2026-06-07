package harness

import (
	"context"
	"os"
	"path/filepath"
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
