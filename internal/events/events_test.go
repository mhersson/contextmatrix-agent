package events

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmitterSeqAndDualOutput(t *testing.T) {
	var human, transcript bytes.Buffer

	e := NewEmitter(&human, &transcript)
	e.now = func() time.Time { return time.Unix(0, 0).UTC() }

	e.Emit(ModelRequest, map[string]any{"model": "x"})
	e.Emit(ToolCallKind, map[string]any{"name": "read"})

	lines := strings.Split(strings.TrimSpace(transcript.String()), "\n")
	require.Len(t, lines, 2)

	var ev1 Event
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &ev1))
	assert.Equal(t, 1, ev1.Seq)
	assert.Equal(t, ModelRequest, ev1.Kind)

	var ev2 Event
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &ev2))
	assert.Equal(t, 2, ev2.Seq)
	assert.Equal(t, ToolCallKind, ev2.Kind)

	assert.Contains(t, human.String(), "model_request")
	assert.Contains(t, human.String(), "tool_call")
}
