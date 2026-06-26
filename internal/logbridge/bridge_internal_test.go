package logbridge

import (
	"testing"

	"github.com/stretchr/testify/assert"

	protocol "github.com/mhersson/contextmatrix-protocol"
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

type countingDropObserver struct{ n int }

func (c *countingDropObserver) ObserveDrop() { c.n++ }

func TestHubDropObserver_CountsOverflow(t *testing.T) {
	obs := &countingDropObserver{}
	hub := NewHubWithDropObserver(obs)

	// Subscribe but never drain, so the buffer fills.
	_, _ = hub.Subscribe("")

	// The first subBufSize entries fit; the next one overflows and drops.
	for range subBufSize + 1 {
		hub.Publish(protocol.LogEntry{Project: "p", Type: "system", Content: "x"})
	}

	assert.Equal(t, 1, obs.n, "exactly one entry should have been dropped")
}

func TestHubNilDropObserver_DoesNotPanic(t *testing.T) {
	hub := NewHub() // nil observer
	_, _ = hub.Subscribe("")

	for range subBufSize + 1 {
		hub.Publish(protocol.LogEntry{Project: "p", Type: "system", Content: "x"})
	}
	// Reaching here without a panic is the assertion.
}
