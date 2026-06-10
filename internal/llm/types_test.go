package llm

import (
	"encoding/json"
	"strings"
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

func TestResponseToleratesUnknownFields(t *testing.T) {
	// Unknown top-level + nested fields must not break decoding.
	raw := `{"model":"m","brand_new_field":42,"usage":{"prompt_tokens":3,"completion_tokens":5,"cost":0.0001,"surprise":true}}`

	var nr nonStreamResponse
	require.NoError(t, json.NewDecoder(strings.NewReader(raw)).Decode(&nr))
	assert.Equal(t, "m", nr.Model)
	assert.InEpsilon(t, 0.0001, nr.Usage.Cost, 1e-9)
}
