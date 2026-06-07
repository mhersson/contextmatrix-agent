package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fanoutFakeLLM decides replies from the conversation shape (parallel-safe).
type fanoutFakeLLM struct{}

func (fanoutFakeLLM) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	return fanoutFakeLLM{}.SendStream(ctx, req, nil)
}

func (fanoutFakeLLM) SendStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	if req.Messages[len(req.Messages)-1].Role == "tool" {
		return llm.Response{Content: "found it", FinishReason: "stop"}, nil
	}
	return llm.Response{ToolCalls: []llm.ToolCall{{
		ID: "1", Type: "function",
		Function: llm.FunctionCall{Name: "read", Arguments: `{"path":"f.txt"}`},
	}}}, nil
}

func TestRunFanout(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("hi"), 0o644))

	results, err := runFanout(context.Background(), fanoutFakeLLM{}, fanoutOpts{
		workspace: root, model: "m", maxDepth: 2, human: io.Discard,
		tasks: []harness.SubagentSpec{{Role: "a", Prompt: "read f.txt"}, {Role: "b", Prompt: "read f.txt"}},
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "found it", results[0].Output)
	assert.Equal(t, "found it", results[1].Output)
}

func TestParseTasks(t *testing.T) {
	specs, err := parseTasks([]string{"reviewer=check correctness", "docs=write notes"})
	require.NoError(t, err)
	require.Len(t, specs, 2)
	assert.Equal(t, "reviewer", specs[0].Role)
	assert.Equal(t, "check correctness", specs[0].Prompt)

	_, err = parseTasks([]string{"bad-no-equals"})
	require.Error(t, err)
}
