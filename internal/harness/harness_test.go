package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

type capturingLLM struct{ last llm.Request }

func (c *capturingLLM) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	c.last = req

	return llm.Response{FinishReason: "stop"}, nil
}

func (c *capturingLLM) SendStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	c.last = req

	return llm.Response{FinishReason: "stop"}, nil
}

func TestRunForwardsProviderAndReasoning(t *testing.T) {
	reg := tools.NewRegistry(tools.NewReadTool(t.TempDir()))
	capt := &capturingLLM{}
	_, err := Run(context.Background(), capt, reg, newEmitter(), "task", Config{
		MaxTurns:  1,
		Models:    []string{"primary/m", "fallback/m"},
		Provider:  json.RawMessage(`{"sort":"price"}`),
		Reasoning: json.RawMessage(`{"effort":"high"}`),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"primary/m", "fallback/m"}, capt.last.Models) // models[] failover forwarded
	assert.JSONEq(t, `{"sort":"price"}`, string(capt.last.Provider))
	assert.JSONEq(t, `{"effort":"high"}`, string(capt.last.Reasoning))
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

func TestRunContextLimitReturnsIncomplete(t *testing.T) {
	reg := tools.NewRegistry(tools.NewReadTool(t.TempDir()))
	f := &fakeLLM{responses: []llm.Response{
		{
			Content: "thinking", Usage: llm.Usage{PromptTokens: 900},
			ToolCalls: []llm.ToolCall{toolCall("1", "read", `{"path":"x"}`)},
		},
	}}
	res, err := Run(context.Background(), f, reg, newEmitter(), "task", Config{MaxTurns: 10, ContextWindow: 1000})
	require.NoError(t, err)
	assert.False(t, res.Completed)
	assert.Equal(t, "context_limit", res.Reason)
	assert.Equal(t, 1, res.Turns)
}

func TestRunContextLimitDisabledWhenWindowZero(t *testing.T) {
	reg := tools.NewRegistry(tools.NewReadTool(t.TempDir()))
	f := &fakeLLM{responses: []llm.Response{
		{Content: "done", FinishReason: "stop", Usage: llm.Usage{PromptTokens: 999999}},
	}}
	res, err := Run(context.Background(), f, reg, newEmitter(), "task", Config{MaxTurns: 10}) // window 0 = disabled
	require.NoError(t, err)
	assert.True(t, res.Completed)
	assert.Equal(t, "done", res.Reason)
}

// bigTool is a fake tool that returns a large string with distinct head/tail content.
type bigTool struct{ output string }

func (b *bigTool) Name() string { return "big" }
func (b *bigTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: "big"}}
}

func (b *bigTool) Execute(_ context.Context, _ map[string]any) (string, error) {
	return b.output, nil
}

// capturingLLMSeq records all requests; scripted responses are returned in order.
type capturingLLMSeq struct {
	responses []llm.Response
	requests  []llm.Request
	i         int
}

func (c *capturingLLMSeq) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	return c.next(req), nil
}

func (c *capturingLLMSeq) SendStream(_ context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return c.next(req), nil
}

func (c *capturingLLMSeq) next(req llm.Request) llm.Response {
	c.requests = append(c.requests, req)
	if c.i >= len(c.responses) {
		return llm.Response{FinishReason: "stop"}
	}

	r := c.responses[c.i]
	c.i++

	return r
}

func TestRunToolOutputCapTruncates(t *testing.T) {
	const maxBytes = 1000

	head := strings.Repeat("H", 60000)
	tail := strings.Repeat("T", 40000)
	large := head + tail // 100 KiB, clearly distinct head/tail

	bt := &bigTool{output: large}
	reg := tools.NewRegistry(bt)

	capt := &capturingLLMSeq{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("1", "big", `{}`)}},
		{Content: "done", FinishReason: "stop"},
	}}

	_, err := Run(context.Background(), capt, reg, newEmitter(), "task", Config{
		MaxTurns:           10,
		ToolOutputMaxBytes: maxBytes,
	})
	require.NoError(t, err)

	// The second request carries the tool-result message from the first turn.
	require.Len(t, capt.requests, 2)
	secondReq := capt.requests[1]

	// Find the tool-result message.
	var toolResultContent string

	for _, m := range secondReq.Messages {
		if m.Role == "tool" && m.ToolCallID == "1" {
			toolResultContent = m.Content

			break
		}
	}

	require.NotEmpty(t, toolResultContent, "tool-result message not found in second request")

	assert.Contains(t, toolResultContent, "bytes truncated")
	assert.True(t, strings.HasPrefix(toolResultContent, "HH"), "head content preserved")
	assert.True(t, strings.HasSuffix(toolResultContent, "TT"), "tail content preserved")
	assert.LessOrEqual(t, len(toolResultContent), maxBytes+80) // marker allowance
}
