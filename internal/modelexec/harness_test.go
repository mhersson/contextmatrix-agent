package modelexec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// echoLLM answers every request with a text reply naming the requested model.
type echoLLM struct{}

func (echoLLM) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	return llm.Response{Content: "reply " + req.Model, FinishReason: "stop"}, nil
}

func (e echoLLM) SendStream(ctx context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return e.Send(ctx, req)
}

func TestHarnessRunForwardsToTheHarnessLoop(t *testing.T) {
	res, err := Harness{}.Run(t.Context(), orchestrator.RunRequest{
		Client:   echoLLM{},
		Registry: tools.NewRegistry(),
		Emit:     events.NewEmitter(nil, nil),
		Task:     "task",
		Config:   harness.Config{Model: "m/1", MaxTurns: 3},
	})
	require.NoError(t, err)
	assert.Equal(t, "reply m/1", res.Output)
	assert.True(t, res.Completed)
}

func TestHarnessSpawnSubagentsKeepsSpecOrder(t *testing.T) {
	specs := []harness.SubagentSpec{
		{Role: "first", Prompt: "p", Model: "m/1", MaxTurns: 3},
		{Role: "second", Prompt: "p", Model: "m/2", MaxTurns: 3},
	}

	results, err := Harness{}.SpawnSubagents(t.Context(), orchestrator.FanoutRequest{
		Client: echoLLM{},
		Root:   t.TempDir(),
		Emit:   events.NewEmitter(nil, nil),
		Specs:  specs,
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "first", results[0].Role)
	assert.Equal(t, "reply m/1", results[0].Output)
	assert.Equal(t, "second", results[1].Role)
	assert.Equal(t, "reply m/2", results[1].Output)
}

// toolEchoLLM answers the first request with a read tool call and the next with
// the tool result it received, recording the model of every request.
type toolEchoLLM struct {
	mu     sync.Mutex
	models []string
}

func (l *toolEchoLLM) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	l.mu.Lock()
	l.models = append(l.models, req.Model)
	l.mu.Unlock()

	for _, m := range req.Messages {
		if m.Role == "tool" {
			return llm.Response{Content: m.Content, FinishReason: "stop"}, nil
		}
	}

	return llm.Response{
		ToolCalls: []llm.ToolCall{{
			ID:       "call-1",
			Type:     "function",
			Function: llm.FunctionCall{Name: "read", Arguments: `{"path":"secret.txt"}`},
		}},
		FinishReason: "tool_calls",
	}, nil
}

func (l *toolEchoLLM) SendStream(ctx context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return l.Send(ctx, req)
}

func TestHarnessSpawnSubagentsForwardsOpts(t *testing.T) {
	const marker = "SECRET-7c1e"

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret.txt"), []byte("token "+marker+"\n"), 0o600))

	client := &toolEchoLLM{}

	results, err := Harness{}.SpawnSubagents(t.Context(), orchestrator.FanoutRequest{
		Client: client,
		Root:   root,
		Emit:   events.NewEmitter(nil, nil),
		Specs:  []harness.SubagentSpec{{Role: "reader", Prompt: "p", MaxTurns: 4}},
		Opts: harness.SubagentOpts{
			DefaultModel:     "m/default",
			RedactToolOutput: func(s string) string { return strings.ReplaceAll(s, marker, "[REDACTED]") },
		},
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)

	client.mu.Lock()
	defer client.mu.Unlock()

	require.NotEmpty(t, client.models)

	for _, m := range client.models {
		assert.Equal(t, "m/default", m)
	}

	assert.Contains(t, results[0].Output, "[REDACTED]")
	assert.NotContains(t, results[0].Output, marker)
}
