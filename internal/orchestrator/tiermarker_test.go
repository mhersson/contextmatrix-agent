package orchestrator

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTierMarkerRoundTrip(t *testing.T) {
	body := withTierMarker("Implement the API.\n\nAcceptance: tests pass.", "complex")
	assert.Contains(t, body, "<!-- cm:tier=complex -->")

	tier, clean := parseTierMarker(body)
	assert.Equal(t, "complex", tier)
	assert.Equal(t, "Implement the API.\n\nAcceptance: tests pass.", clean)
}

func TestParseTierMarkerAbsentAndInvalid(t *testing.T) {
	tier, clean := parseTierMarker("plain body")
	assert.Empty(t, tier)
	assert.Equal(t, "plain body", clean)

	tier, _ = parseTierMarker("body\n<!-- cm:tier=galactic -->")
	assert.Empty(t, tier, "unknown tiers are rejected, caller falls back")
}

func TestNextTier(t *testing.T) {
	tests := []struct {
		tier string
		want string
	}{
		{"simple", "moderate"},
		{"moderate", "complex"},
		{"complex", "critical"},
		{"critical", "critical"},
		{"", "complex"}, // absent marker parses as the moderate default
	}

	for _, tt := range tests {
		t.Run(tt.tier, func(t *testing.T) {
			assert.Equal(t, tt.want, nextTier(tt.tier))
		})
	}
}
