package executor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/metrics"
)

// TestResolveOutcome pins the container_duration label for every input,
// including the causes that outrank a recorded reason. The cause vocabulary is
// finer than the label vocabulary on purpose: the label must keep saying what
// it has always said, so the dashboards built on it do not shift underneath.
func TestResolveOutcome(t *testing.T) {
	tests := []struct {
		name     string
		cause    ExitCause
		reason   string
		exitCode int64
		want     string
	}{
		{"clean exit", ExitNormal, "", 0, metrics.OutcomeSuccess},
		{"nonzero exit", ExitNormal, "", 1, metrics.OutcomeFailure},
		{"container timeout wins", ExitTimeout, "", -1, metrics.OutcomeTimeout},
		{"idle reason", ExitIdleTimeout, metrics.OutcomeIdleTimeout, 137, metrics.OutcomeIdleTimeout},
		{"killed reason", ExitKilled, metrics.OutcomeKilled, 137, metrics.OutcomeKilled},
		{"timeout beats reason", ExitTimeout, metrics.OutcomeKilled, -1, metrics.OutcomeTimeout},
		{"wait failure leaves the reason as the label", ExitWaitFailure, metrics.OutcomeKilled, -1, metrics.OutcomeKilled},
		{"wait failure without a reason is a failure", ExitWaitFailure, "", -1, metrics.OutcomeFailure},
		{"a daemon-flagged wait does not change the label", ExitDaemonError, "", 0, metrics.OutcomeSuccess},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveOutcome(tc.cause, tc.reason, tc.exitCode))
		})
	}
}

// TestResolveCause pins the one precedence the cause vocabulary applies: a kill
// this goroutine performed itself outranks a reason recorded elsewhere, which
// in turn outranks whatever the wait result alone suggested.
func TestResolveCause(t *testing.T) {
	tests := []struct {
		name     string
		observed ExitCause
		reason   string
		want     ExitCause
	}{
		{"nothing recorded leaves the observation", ExitNormal, "", ExitNormal},
		{"idle watchdog kill", ExitNormal, metrics.OutcomeIdleTimeout, ExitIdleTimeout},
		{"requested kill", ExitNormal, metrics.OutcomeKilled, ExitKilled},
		{"a recorded kill outranks a daemon-flagged wait", ExitDaemonError, metrics.OutcomeKilled, ExitKilled},
		{"the container timeout outranks a recorded kill", ExitTimeout, metrics.OutcomeKilled, ExitTimeout},
		{"a failed wait outranks a recorded kill", ExitWaitFailure, metrics.OutcomeIdleTimeout, ExitWaitFailure},
		{"an unnamed reason leaves the observation", ExitNormal, "something-else", ExitNormal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveCause(tc.observed, tc.reason))
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
