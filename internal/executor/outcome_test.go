package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/metrics"
)

func TestResolveOutcome(t *testing.T) {
	tests := []struct {
		name     string
		timedOut bool
		reason   string
		exitCode int64
		want     string
	}{
		{"clean exit", false, "", 0, metrics.OutcomeSuccess},
		{"nonzero exit", false, "", 1, metrics.OutcomeFailure},
		{"container timeout wins", true, "", -1, metrics.OutcomeTimeout},
		{"idle reason", false, metrics.OutcomeIdleTimeout, 137, metrics.OutcomeIdleTimeout},
		{"killed reason", false, metrics.OutcomeKilled, 137, metrics.OutcomeKilled},
		{"timeout beats reason", true, metrics.OutcomeKilled, -1, metrics.OutcomeTimeout},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveOutcome(tc.timedOut, tc.reason, tc.exitCode))
		})
	}
}

func TestTrackerReason(t *testing.T) {
	tr := NewTracker(2)
	run := &Run{Project: "p", CardID: "C-1"}
	require.True(t, tr.AddIfUnderLimit(run))

	assert.Empty(t, tr.Reason("p", "C-1"), "no reason recorded yet")

	tr.SetReason("p", "C-1", metrics.OutcomeKilled)
	assert.Equal(t, metrics.OutcomeKilled, tr.Reason("p", "C-1"))

	// Remove clears the reason.
	tr.Remove("p", "C-1")
	assert.Empty(t, tr.Reason("p", "C-1"))

	// SetReason on an absent run is a no-op (no dangling entry).
	tr.SetReason("p", "ghost", metrics.OutcomeIdleTimeout)
	assert.Empty(t, tr.Reason("p", "ghost"))
}
