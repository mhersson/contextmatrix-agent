package logbridge

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMapEventSkipsBlankModelResponse(t *testing.T) {
	b := New(NewHub(), nil, nil)
	_, _, skip := b.mapEvent("model_response", map[string]any{"content": "", "model": "x"})
	assert.True(t, skip, "blank model_response must be skipped")

	entry, _, skip2 := b.mapEvent("model_response", map[string]any{"content": "hello", "model": "x"})
	assert.False(t, skip2)
	assert.Equal(t, "text", entry.Type)
	assert.Equal(t, "hello", entry.Content)
}
