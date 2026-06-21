package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/events"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/redact"
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

// secretTool is a fake tool that embeds a known secret in its output.
type secretTool struct{ secret string }

func (s *secretTool) Name() string { return "secret" }
func (s *secretTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: "secret"}}
}

func (s *secretTool) Execute(_ context.Context, _ map[string]any) (string, error) {
	return "output contains " + s.secret + " end", nil
}

func TestRunRedactToolOutput(t *testing.T) {
	const seedSecret = "sk-or-v1-supersecretkey99"

	st := &secretTool{secret: seedSecret}
	reg := tools.NewRegistry(st)

	capt := &capturingLLMSeq{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("1", "secret", `{}`)}},
		{Content: "done", FinishReason: "stop"},
	}}

	var transcript bytes.Buffer

	emit := events.NewEmitter(nil, &transcript)

	r := redact.New([]string{seedSecret})

	_, err := Run(context.Background(), capt, reg, emit, "task", Config{
		MaxTurns:         10,
		RedactToolOutput: r.Apply,
	})
	require.NoError(t, err)

	// The second request must carry the tool-result message with redacted content.
	require.Len(t, capt.requests, 2)

	var toolResultContent string

	for _, m := range capt.requests[1].Messages {
		if m.Role == "tool" && m.ToolCallID == "1" {
			toolResultContent = m.Content

			break
		}
	}

	require.NotEmpty(t, toolResultContent, "tool-result message not found in second request")
	assert.Contains(t, toolResultContent, "[REDACTED]", "secret must be masked in tool message")
	assert.NotContains(t, toolResultContent, seedSecret, "raw secret must not appear in tool message")

	// No event in the JSONL transcript may contain the raw secret.
	assert.NotContains(t, transcript.String(), seedSecret, "raw secret must not appear in any event")
}

// erroringTool is a fake tool that fails with an error carrying a large,
// secret-bearing message — models a subprocess error that dumps full output.
type erroringTool struct{ msg string }

func (e *erroringTool) Name() string { return "boom" }
func (e *erroringTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: "boom"}}
}

func (e *erroringTool) Execute(_ context.Context, _ map[string]any) (string, error) {
	return "", errors.New(e.msg)
}

func TestRunToolErrorOutputCappedAndRedacted(t *testing.T) {
	const (
		maxBytes   = 1000
		seedSecret = "sk-or-v1-supersecretkey99"
	)

	// Error message far exceeds the cap and embeds a secret.
	big := strings.Repeat("E", 50000) + " " + seedSecret + " " + strings.Repeat("F", 50000)
	et := &erroringTool{msg: big}
	reg := tools.NewRegistry(et)

	capt := &capturingLLMSeq{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("1", "boom", `{}`)}},
		{Content: "done", FinishReason: "stop"},
	}}

	r := redact.New([]string{seedSecret})

	_, err := Run(context.Background(), capt, reg, newEmitter(), "task", Config{
		MaxTurns:           10,
		ToolOutputMaxBytes: maxBytes,
		RedactToolOutput:   r.Apply,
	})
	require.NoError(t, err)

	require.Len(t, capt.requests, 2)

	var toolResultContent string

	for _, m := range capt.requests[1].Messages {
		if m.Role == "tool" && m.ToolCallID == "1" {
			toolResultContent = m.Content

			break
		}
	}

	require.NotEmpty(t, toolResultContent, "tool-result message not found in second request")
	assert.Contains(t, toolResultContent, "bytes truncated", "oversized tool error must be size-capped")
	assert.LessOrEqual(t, len(toolResultContent), maxBytes+80, "tool error must be bounded by the cap") // marker allowance
	assert.NotContains(t, toolResultContent, seedSecret, "secret must be redacted on the error path")
}

// scriptedInbox: queue of messages; Drain pops all pending; Wait pops one or
// returns closeErr when the queue is empty. When deliverViaWaitOnly is set,
// Drain returns nothing so the pending queue is delivered exclusively through
// Wait (models a message that only arrives while the loop is parked at Wait).
type scriptedInbox struct {
	mu                 sync.Mutex
	pending            []UserMessage
	closeErr           error // ErrInboxClosed once exhausted, or nil to block forever
	deliverViaWaitOnly bool
}

func (s *scriptedInbox) Drain() []UserMessage {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.deliverViaWaitOnly || len(s.pending) == 0 {
		return nil
	}

	out := s.pending
	s.pending = nil

	return out
}

func (s *scriptedInbox) Wait(ctx context.Context) (UserMessage, error) {
	s.mu.Lock()

	if len(s.pending) > 0 {
		um := s.pending[0]
		s.pending = s.pending[1:]
		s.mu.Unlock()

		return um, nil
	}

	closeErr := s.closeErr
	s.mu.Unlock()

	if closeErr != nil {
		return UserMessage{}, closeErr
	}

	// Block forever unless ctx fires.
	<-ctx.Done()

	return UserMessage{}, ctx.Err()
}

func (s *scriptedInbox) push(um UserMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pending = append(s.pending, um)
}

// parseEvents decodes JSONL transcript events for assertions.
func parseEvents(t *testing.T, transcript string) []events.Event {
	t.Helper()

	var out []events.Event

	for _, line := range strings.Split(strings.TrimSpace(transcript), "\n") {
		if line == "" {
			continue
		}

		var ev events.Event
		require.NoError(t, json.Unmarshal([]byte(line), &ev))
		out = append(out, ev)
	}

	return out
}

// findToolResult locates the tool-result message for a given tool-call id.
func findToolResult(msgs []llm.Message, id string) (string, bool) {
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == id {
			return m.Content, true
		}
	}

	return "", false
}

// firstUserMessageIndex returns the index of the first user message at-or-after
// offset, or -1.
func userMessageIndexAfter(msgs []llm.Message, offset int) int {
	for i := offset; i < len(msgs); i++ {
		if msgs[i].Role == "user" {
			return i
		}
	}

	return -1
}

// Case 2: a message arriving during turn 1's (single) tool call drains at the
// top of turn 2 and lands in request 2 AFTER turn 1's tool results.
func TestInboxTurnTopDrain(t *testing.T) {
	inbox := &scriptedInbox{closeErr: ErrInboxClosed}
	it := &interjectingTool{inbox: inbox, msg: UserMessage{Content: "human steers here", MessageID: "m1"}}
	reg := tools.NewRegistry(it)

	capt := &capturingLLMSeq{responses: []llm.Response{
		// single tool call → mid-batch skip does not apply; drain happens at turn 2 top.
		{ToolCalls: []llm.ToolCall{toolCall("1", "interject", `{}`)}},
		{Content: "done", FinishReason: "stop"},
	}}

	res, err := Run(context.Background(), capt, reg, newEmitter(), "do it", Config{MaxTurns: 10, Inbox: inbox})
	require.NoError(t, err)
	assert.True(t, res.Completed)

	require.Len(t, capt.requests, 2)
	second := capt.requests[1].Messages

	// Turn 1's tool result must be present and precede the injected user message.
	_, ok := findToolResult(second, "1")
	require.True(t, ok, "turn 1 tool result missing from request 2")

	// Locate the tool result index, then the user message after it.
	toolIdx := -1

	for i, m := range second {
		if m.Role == "tool" && m.ToolCallID == "1" {
			toolIdx = i

			break
		}
	}

	require.GreaterOrEqual(t, toolIdx, 0)

	uIdx := userMessageIndexAfter(second, toolIdx)
	require.GreaterOrEqual(t, uIdx, 0, "injected user message not found after tool result")
	assert.Equal(t, "human steers here", second[uIdx].Content)
}

// Case 3: natural stop blocks on Wait, gets one message, continues, then closed.
func TestInboxNaturalStopWaitThenContinue(t *testing.T) {
	reg := tools.NewRegistry(tools.NewReadTool(t.TempDir()))

	inbox := &scriptedInbox{
		pending:            []UserMessage{{Content: "keep going", MessageID: "m1"}},
		closeErr:           ErrInboxClosed,
		deliverViaWaitOnly: true, // message arrives only via Wait, not the turn-top drain
	}

	// Turn 1: no tool calls. Turn 2: no tool calls. Then inbox is empty → closed.
	f := &fakeLLM{responses: []llm.Response{
		{Content: "first", FinishReason: "stop"},
		{Content: "second", FinishReason: "stop"},
	}}

	var transcript bytes.Buffer

	emit := events.NewEmitter(nil, &transcript)

	res, err := Run(context.Background(), f, reg, emit, "task", Config{MaxTurns: 10, Inbox: inbox})
	require.NoError(t, err)
	assert.True(t, res.Completed)
	assert.Equal(t, "done", res.Reason)
	assert.Equal(t, 2, res.Turns)

	// An awaiting_human state change must precede the wait, carrying the
	// plural "turns" key like every other StateChange payload.
	evs := parseEvents(t, transcript.String())

	var sawAwaiting bool

	for _, ev := range evs {
		if ev.Kind == events.StateChange && ev.Data["state"] == "awaiting_human" {
			sawAwaiting = true

			assert.Contains(t, ev.Data, "turns", "awaiting_human payload must use plural turns key")

			break
		}
	}

	assert.True(t, sawAwaiting, "awaiting_human state change not emitted")
}

// Case 4: closed inbox behaves like autonomous — single turn, done.
func TestInboxClosedIsAutonomous(t *testing.T) {
	reg := tools.NewRegistry(tools.NewReadTool(t.TempDir()))

	inbox := &scriptedInbox{closeErr: ErrInboxClosed}

	f := &fakeLLM{responses: []llm.Response{
		{Content: "all done", FinishReason: "stop"},
	}}

	res, err := Run(context.Background(), f, reg, newEmitter(), "task", Config{MaxTurns: 10, Inbox: inbox})
	require.NoError(t, err)
	assert.True(t, res.Completed)
	assert.Equal(t, "done", res.Reason)
	assert.Equal(t, 1, res.Turns)
}

// interjectingTool pushes a message into the inbox the first time it executes.
type interjectingTool struct {
	inbox *scriptedInbox
	msg   UserMessage
	calls int
}

func (i *interjectingTool) Name() string { return "interject" }
func (i *interjectingTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: "interject"}}
}

func (i *interjectingTool) Execute(_ context.Context, _ map[string]any) (string, error) {
	i.calls++
	if i.calls == 1 {
		i.inbox.push(i.msg)
	}

	return "ok", nil
}

// Case 5: mid-batch interrupt. Turn 1 has three tool calls; the first pushes a
// message; calls 2 and 3 are skipped with synthesized results; the user message
// follows after all tool results.
func TestInboxMidBatchInterrupt(t *testing.T) {
	inbox := &scriptedInbox{closeErr: ErrInboxClosed}
	it := &interjectingTool{inbox: inbox, msg: UserMessage{Content: "stop and listen", MessageID: "m1"}}
	reg := tools.NewRegistry(it)

	capt := &capturingLLMSeq{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{
			toolCall("1", "interject", `{}`),
			toolCall("2", "interject", `{}`),
			toolCall("3", "interject", `{}`),
		}},
		{Content: "done", FinishReason: "stop"},
	}}

	res, err := Run(context.Background(), capt, reg, newEmitter(), "task", Config{MaxTurns: 10, Inbox: inbox})
	require.NoError(t, err)
	assert.True(t, res.Completed)

	// Only the first call executed.
	assert.Equal(t, 1, it.calls, "calls 2 and 3 must not execute")

	require.Len(t, capt.requests, 2)
	second := capt.requests[1].Messages

	// Call 1 executed result.
	c1, ok := findToolResult(second, "1")
	require.True(t, ok)
	assert.Equal(t, "ok", c1)

	// Calls 2 and 3 carry synthesized skip results.
	c2, ok := findToolResult(second, "2")
	require.True(t, ok)
	assert.Equal(t, "skipped: user interjected", c2)

	c3, ok := findToolResult(second, "3")
	require.True(t, ok)
	assert.Equal(t, "skipped: user interjected", c3)

	// The user message must appear AFTER all three tool results.
	lastToolIdx := -1

	for i, m := range second {
		if m.Role == "tool" && (m.ToolCallID == "1" || m.ToolCallID == "2" || m.ToolCallID == "3") {
			lastToolIdx = i
		}
	}

	require.GreaterOrEqual(t, lastToolIdx, 0)

	uIdx := userMessageIndexAfter(second, lastToolIdx)
	require.GreaterOrEqual(t, uIdx, 0, "user message must follow the tool results")
	assert.Equal(t, "stop and listen", second[uIdx].Content)
}

func TestRunDetectsIncapability(t *testing.T) {
	reg := tools.NewRegistry(tools.NewReadTool(t.TempDir()))

	// Every turn: one tool call whose Arguments is not valid JSON → never executes.
	badCall := toolCall("1", "read", `{ this is not json`)
	responses := make([]llm.Response, 20)

	for i := range responses {
		responses[i] = llm.Response{ToolCalls: []llm.ToolCall{badCall}}
	}

	f := &fakeLLM{responses: responses}

	res, err := Run(context.Background(), f, reg, newEmitter(), "do x",
		Config{Model: "weak/m", MaxTurns: 20, IncapableThreshold: 3})
	require.NoError(t, err)
	assert.Equal(t, "incapable", res.Reason)
	assert.Less(t, res.Turns, 20, "incapable must fire before MaxTurns")
}

// Case 6: ctx cancel during Wait → Run returns ctx.Err() with Reason canceled.
func TestInboxCtxCancelDuringWait(t *testing.T) {
	reg := tools.NewRegistry(tools.NewReadTool(t.TempDir()))

	// Inbox never closes and has no pending messages → Wait blocks until ctx.
	inbox := &scriptedInbox{}

	f := &fakeLLM{responses: []llm.Response{
		{Content: "first", FinishReason: "stop"},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel up front: the fakeLLM ignores ctx, so turn 1 still completes; the
	// loop then reaches the natural-stop Wait, which returns ctx.Err()
	// immediately because Done is already closed.
	cancel()

	res, err := Run(ctx, f, reg, newEmitter(), "task", Config{MaxTurns: 10, Inbox: inbox})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "canceled", res.Reason)
	assert.False(t, res.Completed)
}
