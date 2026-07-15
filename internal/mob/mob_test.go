package mob

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpecFixedConstants(t *testing.T) {
	assert.Equal(t, 16*1024, utteranceCap)
	assert.Equal(t, "\n[truncated by moderator]", truncationMarker)
	assert.Equal(t, 8, maxSeatTurns)
	assert.Equal(t, 240*time.Second, internalTurnDeadline)
	assert.Equal(t, 300*time.Second, guestTurnDeadline)
}

func TestNewEngineDefaults(t *testing.T) {
	e := NewEngine(EngineConfig{})

	assert.Equal(t, 240*time.Second, e.cfg.InternalDeadline)
	assert.Equal(t, 300*time.Second, e.cfg.GuestDeadline)
	require.NotNil(t, e.cfg.Emit)
	e.cfg.Emit("moderator", "", "", -1, "must not panic")
}

func TestNewEngineKeepsCustomDeadlines(t *testing.T) {
	e := NewEngine(EngineConfig{
		InternalDeadline: time.Second,
		GuestDeadline:    2 * time.Second,
	})

	assert.Equal(t, time.Second, e.cfg.InternalDeadline)
	assert.Equal(t, 2*time.Second, e.cfg.GuestDeadline)
}
