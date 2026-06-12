package llm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractHarmonyToolCalls(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []ToolCall
		rest    string
	}{
		{
			name:    "single call commentary channel",
			content: `<|channel|>commentary to=functions.read_file <|constrain|>json<|message|>{"path":"main.go"}`,
			want: []ToolCall{{
				ID: "harmony-0", Type: "function",
				Function: FunctionCall{Name: "read_file", Arguments: `{"path":"main.go"}`},
			}},
			rest: "",
		},
		{
			name:    "prose plus call",
			content: "Let me look.<|start|>assistant<|channel|>commentary to=functions.bash <|constrain|>json<|message|>{\"command\":\"ls\"}<|end|>",
			want: []ToolCall{{
				ID: "harmony-0", Type: "function",
				Function: FunctionCall{Name: "bash", Arguments: `{"command":"ls"}`},
			}},
			rest: "Let me look.",
		},
		{
			name:    "no harmony markers",
			content: "plain answer",
			want:    nil,
			rest:    "plain answer",
		},
		{
			name:    "final channel only is not a tool call",
			content: `<|channel|>final<|message|>done`,
			want:    nil,
			rest:    "done",
		},
		{
			name:    "multiple calls",
			content: `<|channel|>commentary to=functions.read_file <|constrain|>json<|message|>{"path":"a.go"}<|channel|>commentary to=functions.bash <|constrain|>json<|message|>{"command":"pwd"}`,
			want: []ToolCall{
				{
					ID: "harmony-0", Type: "function",
					Function: FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`},
				},
				{
					ID: "harmony-1", Type: "function",
					Function: FunctionCall{Name: "bash", Arguments: `{"command":"pwd"}`},
				},
			},
			rest: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, rest := extractHarmonyToolCalls(tt.content)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.rest, rest)
		})
	}
}

func TestNormalizeHarmony(t *testing.T) {
	tests := []struct {
		name string
		in   Response
		want Response
	}{
		{
			name: "final channel only: prose preserved, no tool calls",
			in:   Response{Content: `<|channel|>final<|message|>All done here.`, FinishReason: "stop"},
			want: Response{Content: "All done here.", FinishReason: "stop"},
		},
		{
			name: "structured tool calls already present: untouched",
			in: Response{
				Content:      "<|channel|>commentary to=functions.bash <|message|>{}",
				ToolCalls:    []ToolCall{{ID: "call_1", Type: "function", Function: FunctionCall{Name: "read", Arguments: "{}"}}},
				FinishReason: "tool_calls",
			},
			want: Response{
				Content:      "<|channel|>commentary to=functions.bash <|message|>{}",
				ToolCalls:    []ToolCall{{ID: "call_1", Type: "function", Function: FunctionCall{Name: "read", Arguments: "{}"}}},
				FinishReason: "tool_calls",
			},
		},
		{
			name: "no harmony markers: untouched",
			in:   Response{Content: "plain answer", FinishReason: "stop"},
			want: Response{Content: "plain answer", FinishReason: "stop"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, normalizeHarmony(tt.in))
		})
	}
}

func TestParseStreamHarmonyContent(t *testing.T) {
	// SSE fixture where deltas accumulate Harmony-format tool call in content.
	// The model emits content with Harmony markers instead of structured tool_calls.
	sse := strings.Join([]string{
		`data: {"model":"gpt-oss","choices":[{"delta":{"content":"<|start|>assistant<|channel|>commentary to=functions.bash <|constrain|>json<|message|>"}}]}`,
		`data: {"choices":[{"delta":{"content":"{\"command\":\"ls\"}"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n") + "\n"

	resp, err := parseStream(strings.NewReader(sse), nil)
	require.NoError(t, err)
	require.NotEmpty(t, resp.ToolCalls, "expected harmony tool calls to be extracted from content")
	assert.Equal(t, "harmony-0", resp.ToolCalls[0].ID)
	assert.Equal(t, "bash", resp.ToolCalls[0].Function.Name)
	assert.JSONEq(t, `{"command":"ls"}`, resp.ToolCalls[0].Function.Arguments)
	assert.Equal(t, "tool_calls", resp.FinishReason)
	assert.Equal(t, "gpt-oss", resp.Model)
}
