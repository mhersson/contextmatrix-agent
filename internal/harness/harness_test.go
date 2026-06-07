package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLLM returns scripted responses in order; after they run out it returns an
// empty response (no tool calls → loop treats it as "done").
type fakeLLM struct {
	responses []llm.Response
	i         int
}

func (f *fakeLLM) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	return f.next(), nil
}

func (f *fakeLLM) SendStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	return f.next(), nil
}

func (f *fakeLLM) next() llm.Response {
	if f.i >= len(f.responses) {
		return llm.Response{FinishReason: "stop"}
	}
	r := f.responses[f.i]
	f.i++
	return r
}

func newEmitter() *events.Emitter { return events.NewEmitter(nil, nil) }

func toolCall(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: args}}
}

func TestRunExecutesToolThenStops(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("data"), 0o644))
	reg := tools.NewRegistry(tools.NewReadTool(root))

	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("1", "read", `{"path":"f.txt"}`)}, Usage: llm.Usage{Cost: 0.001}},
		{Content: "all done", FinishReason: "stop", Usage: llm.Usage{Cost: 0.001}},
	}}
	res, err := Run(context.Background(), f, reg, newEmitter(), "do it", Config{MaxTurns: 10})
	require.NoError(t, err)
	assert.True(t, res.Completed)
	assert.Equal(t, "done", res.Reason)
	assert.Equal(t, 1, res.ToolCallCount)
	assert.Equal(t, 0, res.RepairCount)
	assert.InEpsilon(t, 0.002, res.TotalCostUSD, 1e-9)
}

func TestRunRepairsMalformedArgs(t *testing.T) {
	reg := tools.NewRegistry(tools.NewReadTool(t.TempDir()))
	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("1", "read", `{"path":`)}}, // malformed
		{Content: "ok", FinishReason: "stop"},
	}}
	res, err := Run(context.Background(), f, reg, newEmitter(), "task", Config{MaxTurns: 10})
	require.NoError(t, err)
	assert.Equal(t, 1, res.RepairCount)
	assert.Equal(t, 1, res.ToolCallCount)
}

func TestRunToolCallsBeatLyingFinishReason(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))
	reg := tools.NewRegistry(tools.NewReadTool(root))
	f := &fakeLLM{responses: []llm.Response{
		// finish_reason "stop" but tool_calls present — must still execute.
		{FinishReason: "stop", ToolCalls: []llm.ToolCall{toolCall("1", "read", `{"path":"f.txt"}`)}},
		{Content: "fin", FinishReason: "stop"},
	}}
	res, err := Run(context.Background(), f, reg, newEmitter(), "task", Config{MaxTurns: 10})
	require.NoError(t, err)
	assert.Equal(t, 1, res.ToolCallCount)
	assert.True(t, res.Completed)
}

func TestRunMaxTurns(t *testing.T) {
	reg := tools.NewRegistry(tools.NewReadTool(t.TempDir()))
	// Always asks for an (unknown-path) read → never stops on its own.
	loop := []llm.Response{{ToolCalls: []llm.ToolCall{toolCall("1", "read", `{"path":"missing"}`)}}}
	f := &fakeLLM{responses: append(append(append([]llm.Response{}, loop...), loop...), loop...)}
	res, err := Run(context.Background(), f, reg, newEmitter(), "task", Config{MaxTurns: 3})
	require.NoError(t, err)
	assert.False(t, res.Completed)
	assert.Equal(t, "max_turns", res.Reason)
	assert.Equal(t, 3, res.Turns)
}

func TestToolResultMsgNeverEmptyContent(t *testing.T) {
	m := toolResultMsg("call_1", "")
	assert.Equal(t, "tool", m.Role)
	assert.Equal(t, "call_1", m.ToolCallID)
	assert.NotEmpty(t, m.Content) // empty tool output must not drop the wire `content` field

	m2 := toolResultMsg("call_2", "hello")
	assert.Equal(t, "hello", m2.Content)
}

func TestRunMaxCost(t *testing.T) {
	reg := tools.NewRegistry(tools.NewReadTool(t.TempDir()))
	f := &fakeLLM{responses: []llm.Response{
		{Content: "thinking", Usage: llm.Usage{Cost: 0.6}, ToolCalls: []llm.ToolCall{toolCall("1", "read", `{"path":"x"}`)}},
		{Content: "more", Usage: llm.Usage{Cost: 0.6}, ToolCalls: []llm.ToolCall{toolCall("2", "read", `{"path":"x"}`)}},
	}}
	res, err := Run(context.Background(), f, reg, newEmitter(), "task", Config{MaxTurns: 10, MaxCostUSD: 0.5})
	require.NoError(t, err)
	assert.Equal(t, "max_cost", res.Reason)
	assert.False(t, res.Completed)
}

func TestRunMaxTurnsZeroUsesDefault(t *testing.T) {
	reg := tools.NewRegistry(tools.NewReadTool(t.TempDir()))
	// Always asks for a (missing-path) read → never stops on its own.
	resp := llm.Response{ToolCalls: []llm.ToolCall{toolCall("1", "read", `{"path":"missing"}`)}}
	many := make([]llm.Response, 0, defaultMaxTurns)
	for i := 0; i < defaultMaxTurns; i++ {
		many = append(many, resp)
	}
	f := &fakeLLM{responses: many}

	// MaxTurns 0 must NOT mean "run zero turns and silently complete".
	res, err := Run(context.Background(), f, reg, newEmitter(), "task", Config{MaxTurns: 0})
	require.NoError(t, err)
	assert.False(t, res.Completed)
	assert.Equal(t, "max_turns", res.Reason)
	assert.Equal(t, defaultMaxTurns, res.Turns)
}
