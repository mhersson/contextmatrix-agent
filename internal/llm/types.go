// Package llm is a raw-HTTP, OpenAI-compatible client for OpenRouter behind a
// narrow Send/SendStream interface. No SDK: the streaming/tool-call path is
// owned end-to-end so weak-model quirks are handled explicitly.
package llm

import (
	"context"
	"encoding/json"
)

// Message is one chat message (OpenAI-compatible).
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name string `json:"name"`
	// Arguments is a JSON-encoded string; weak models may emit malformed JSON.
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Request is the /chat/completions body. Provider/Models are OpenRouter extras.
type Request struct {
	Model     string          `json:"model,omitempty"`
	Models    []string        `json:"models,omitempty"`
	Messages  []Message       `json:"messages"`
	Tools     []Tool          `json:"tools,omitempty"`
	Stream    bool            `json:"stream,omitempty"`
	Provider  json.RawMessage `json:"provider,omitempty"`
	Reasoning json.RawMessage `json:"reasoning,omitempty"`
	Usage     *UsageOpt       `json:"usage,omitempty"`
}

// UsageOpt opts into OpenRouter usage accounting (token counts + the
// authoritative USD cost) on the response, including the final SSE frame for
// streamed calls. OpenRouter omits cost on streamed responses without this.
type UsageOpt struct {
	Include bool `json:"include"`
}

type Usage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Cost             float64 `json:"cost"` // OpenRouter authoritative USD for this call
}

// Response is the assembled result of one model call (stream or non-stream).
type Response struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Model        string // model actually used (after fallback)
	Usage        Usage
}

// Delta is an incremental streaming fragment for live display.
type Delta struct {
	Content   string
	Reasoning string
}

// LLM is the narrow interface the harness depends on.
type LLM interface {
	Send(ctx context.Context, req Request) (Response, error)
	SendStream(ctx context.Context, req Request, onDelta func(Delta)) (Response, error)
}
