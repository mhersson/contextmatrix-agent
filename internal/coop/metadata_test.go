package coop

import (
	"encoding/json"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControlRoundTripInProcess(t *testing.T) {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("body"))
	setControl(msg, control{Kind: controlClose, Round: 3})

	assert.Equal(t, control{Kind: controlClose, Round: 3}, parseControl(msg))
}

func TestControlRoundTripOverJSON(t *testing.T) {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("body"))
	setControl(msg, control{Kind: controlRound, Round: 2})

	// Simulate the wire: metadata numbers decode as float64.
	raw, err := json.Marshal(msg.Metadata)
	require.NoError(t, err)

	var meta map[string]any
	require.NoError(t, json.Unmarshal(raw, &meta))

	wire := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("body"))
	wire.Metadata = meta

	assert.Equal(t, control{Kind: controlRound, Round: 2}, parseControl(wire))
}

func TestParseControlDefaults(t *testing.T) {
	tests := []struct {
		name string
		msg  *a2a.Message
	}{
		{name: "nil message", msg: nil},
		{name: "no metadata", msg: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("x"))},
		{
			name: "garbage metadata value",
			msg: func() *a2a.Message {
				m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("x"))
				m.SetMeta(metadataKey, "not a map")

				return m
			}(),
		},
		{
			name: "unknown control kind",
			msg: func() *a2a.Message {
				m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("x"))
				m.SetMeta(metadataKey, map[string]any{"control": "reset"})

				return m
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, control{Kind: controlRound}, parseControl(tt.msg))
		})
	}
}

func TestCostMetadataRoundTrip(t *testing.T) {
	msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("utterance"))
	setCost(msg, 0.0123)

	assert.InDelta(t, 0.0123, costFrom(msg), 1e-12)
}

func TestCostFromDefaultsToZero(t *testing.T) {
	assert.Zero(t, costFrom(nil))
	assert.Zero(t, costFrom(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("x"))))

	zeroed := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("x"))
	setCost(zeroed, 0)
	assert.Zero(t, costFrom(zeroed))
}
