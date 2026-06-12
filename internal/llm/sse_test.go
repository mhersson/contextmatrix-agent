package llm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStreamContentAndUsage(t *testing.T) {
	sse := strings.Join([]string{
		`: OPENROUTER PROCESSING`,
		`data: {"model":"m","choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"cost":0.0003}}`,
		`data: [DONE]`,
	}, "\n") + "\n"

	var streamed strings.Builder

	resp, err := parseStream(strings.NewReader(sse), func(d Delta) { streamed.WriteString(d.Content) })
	require.NoError(t, err)
	assert.Equal(t, "Hello", resp.Content)
	assert.Equal(t, "Hello", streamed.String())
	assert.Equal(t, "stop", resp.FinishReason)
	assert.Equal(t, "m", resp.Model)
	assert.InEpsilon(t, 0.0003, resp.Usage.Cost, 1e-9)
}

func TestParseStreamToolCallByIndexWithLateName(t *testing.T) {
	// Name arrives in a LATER delta than id/arguments; args split across frames.
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"arguments":"{\"pa"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"x\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n") + "\n"

	resp, err := parseStream(strings.NewReader(sse), nil)
	require.NoError(t, err)
	require.Len(t, resp.ToolCalls, 1)
	assert.Equal(t, "call_1", resp.ToolCalls[0].ID)
	assert.Equal(t, "read", resp.ToolCalls[0].Function.Name)
	assert.JSONEq(t, `{"path":"x"}`, resp.ToolCalls[0].Function.Arguments)
	assert.Equal(t, "function", resp.ToolCalls[0].Type)
}

func TestParseStreamParallelToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"read","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"grep","arguments":"{}"}}]}}]}`,
		`data: [DONE]`,
	}, "\n") + "\n"

	resp, err := parseStream(strings.NewReader(sse), nil)
	require.NoError(t, err)
	require.Len(t, resp.ToolCalls, 2)
	assert.Equal(t, "read", resp.ToolCalls[0].Function.Name)
	assert.Equal(t, "grep", resp.ToolCalls[1].Function.Name)
}

func TestParseStreamMidStreamError(t *testing.T) {
	sse := `data: {"error":{"code":"server_error","message":"boom"},"choices":[{"delta":{},"finish_reason":"error"}]}` + "\n"
	_, err := parseStream(strings.NewReader(sse), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

func TestParseStreamRejectsOverlongLineClearly(t *testing.T) {
	// A data line longer than the (tiny, test-injected) cap must yield a clear error.
	long := `data: {"choices":[{"delta":{"content":"` + strings.Repeat("x", 256) + `"}}]}` + "\n"
	_, err := parseStreamWithLimit(strings.NewReader(long), nil, 64)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

func TestParseStreamTruncationDetection(t *testing.T) {
	chunk := `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`
	noFinish := `data: {"choices":[{"delta":{"content":"hi"}}]}`

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"done marker, no finish_reason", noFinish + "\n\ndata: [DONE]\n\n", false},
		{"finish_reason, no done marker", chunk + "\n\n", false},
		{"both", chunk + "\n\ndata: [DONE]\n\n", false},
		{"neither: truncated", noFinish + "\n\n", true},
		{"empty stream", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseStream(strings.NewReader(tt.body), nil)
			if tt.wantErr {
				require.ErrorContains(t, err, "truncated")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
