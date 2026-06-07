package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestMarshalsOpenRouterExtras(t *testing.T) {
	req := Request{
		Model:    "primary/model",
		Models:   []string{"primary/model", "fallback/model"},
		Provider: json.RawMessage(`{"require_parameters":true}`),
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []Tool{{Type: "function", Function: ToolFunction{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		Stream:   true,
	}
	b, err := json.Marshal(req)
	require.NoError(t, err)
	s := string(b)
	assert.Contains(t, s, `"models":["primary/model","fallback/model"]`)
	assert.Contains(t, s, `"provider":{"require_parameters":true}`)
	assert.Contains(t, s, `"stream":true`)
}
