package orchestrator

import (
	"context"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWarnUnresolvablePin_CoderDeduplicated(t *testing.T) {
	ops := &fakeOps{}
	d := execTestDeps(ops, &fakeGit{}, &planLLM{})
	o := newExecRun(d, nil, 5)

	ctx := context.Background()

	// First call should produce the advisory.
	o.warnUnresolvablePin(ctx, "coder", "pinned/missing")
	require.Len(t, ops.logs, 1, "first call must produce exactly one log entry")
	assert.Contains(t, ops.logs[0], "pinned/missing")

	// Second call with the same pinType must be a no-op.
	o.warnUnresolvablePin(ctx, "coder", "pinned/missing")
	require.Len(t, ops.logs, 1, "second call must not produce an additional log entry")
	assert.Contains(t, ops.logs[0], "pinned/missing")
}

func TestWarnUnresolvablePin_ReviewerDeduplicated(t *testing.T) {
	ops := &fakeOps{}
	d := execTestDeps(ops, &fakeGit{}, &planLLM{})
	o := newExecRun(d, nil, 5)

	ctx := context.Background()

	// First call should produce the advisory.
	o.warnUnresolvablePin(ctx, "reviewer", "pinned/reviewer")
	require.Len(t, ops.logs, 1, "first call must produce exactly one log entry")
	assert.Contains(t, ops.logs[0], "pinned/reviewer")

	// Second call with the same pinType must be a no-op.
	o.warnUnresolvablePin(ctx, "reviewer", "pinned/reviewer")
	require.Len(t, ops.logs, 1, "second call must not produce an additional log entry")
	assert.Contains(t, ops.logs[0], "pinned/reviewer")
}

func TestWarnUnresolvablePin_CoderAndReviewerAreIndependent(t *testing.T) {
	ops := &fakeOps{}
	d := execTestDeps(ops, &fakeGit{}, &planLLM{})
	o := newExecRun(d, nil, 5)

	ctx := context.Background()

	// Coder call.
	o.warnUnresolvablePin(ctx, "coder", "pinned/coder")
	require.Len(t, ops.logs, 1, "coder call must produce exactly one log entry")
	assert.Contains(t, ops.logs[0], "pinned/coder")

	// Reviewer call with a different slug must NOT be suppressed by the coder guard.
	o.warnUnresolvablePin(ctx, "reviewer", "pinned/reviewer")
	require.Len(t, ops.logs, 2, "reviewer call must produce a second log entry")
	assert.Contains(t, ops.logs[0], "pinned/coder")
	assert.Contains(t, ops.logs[1], "pinned/reviewer")

	// Second coder call still suppressed.
	o.warnUnresolvablePin(ctx, "coder", "some/other")
	require.Len(t, ops.logs, 2, "repeated coder call must not produce additional log entry")

	// Second reviewer call still suppressed.
	o.warnUnresolvablePin(ctx, "reviewer", "some/other")
	require.Len(t, ops.logs, 2, "repeated reviewer call must not produce additional log entry")
}

func claudePinRegistry(t *testing.T) *registry.Registry {
	t.Helper()

	reg, faults := registry.FromSelection(&protocol.SelectionContext{
		Candidates: []protocol.CandidateModel{
			{Slug: "claude-opus-5-5", ContextWindow: 200000, CoderPrior: 0.9, ReviewerPrior: 0.9},
			{Slug: "claude-sonnet-5-5", ContextWindow: 200000, CoderPrior: 0.8, ReviewerPrior: 0.8},
		},
	}, "claude-opus-5-5", false)
	require.Empty(t, faults)

	return reg
}

func TestClaudeCodePins(t *testing.T) {
	reg := claudePinRegistry(t)

	tests := []struct {
		name   string
		pin    func(tc *cmclient.TaskContext) *string
		in     string
		want   string
		logged string
	}{
		{"mapped coder", func(tc *cmclient.TaskContext) *string { return &tc.ModelCoder }, "anthropic/claude-opus-5.5", "claude-opus-5-5", ""},
		{"native reviewer", func(tc *cmclient.TaskContext) *string { return &tc.ModelReviewer }, "claude-sonnet-5-5", "claude-sonnet-5-5", ""},
		{"foreign orchestrator", func(tc *cmclient.TaskContext) *string { return &tc.ModelOrchestrator }, "openai/gpt-6", "", "openai/gpt-6"},
		{"non-candidate coder", func(tc *cmclient.TaskContext) *string { return &tc.ModelCoder }, "anthropic/claude-haiku-5.5", "", "anthropic/claude-haiku-5.5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &fakeOps{}
			o := mobTestRun(ops, MobConfig{}, 0)
			o.d.Registry = reg
			o.d.Cfg.ClaudeCode = true
			*tt.pin(&o.tc) = tt.in

			o.claudeCodePins(t.Context())

			assert.Equal(t, tt.want, *tt.pin(&o.tc))

			if tt.logged == "" {
				assert.Empty(t, ops.logs)

				return
			}

			require.Len(t, ops.logs, 1)
			assert.Contains(t, ops.logs[0], tt.logged)
		})
	}
}

func TestNewRunMapsPinsOnlyInClaudeCodeMode(t *testing.T) {
	tc := cmclient.TaskContext{ModelCoder: "anthropic/claude-opus-5.5", ModelReviewer: "openai/gpt-6"}

	t.Run("claude code", func(t *testing.T) {
		ops := &fakeOps{}
		d := execTestDeps(ops, &fakeGit{}, &planLLM{})
		d.Registry = claudePinRegistry(t)
		d.Cfg.ClaudeCode = true

		o := newRun(t.Context(), d, tc)
		assert.Equal(t, "claude-opus-5-5", o.tc.ModelCoder)
		assert.Empty(t, o.tc.ModelReviewer)
		assert.True(t, ops.loggedContains("openai/gpt-6"), "the cleared pin gets a card note; logs=%v", ops.logs)
	})

	t.Run("endpoint", func(t *testing.T) {
		ops := &fakeOps{}
		d := execTestDeps(ops, &fakeGit{}, &planLLM{})
		d.Registry = claudePinRegistry(t)

		o := newRun(t.Context(), d, tc)
		assert.Equal(t, tc.ModelCoder, o.tc.ModelCoder)
		assert.Equal(t, tc.ModelReviewer, o.tc.ModelReviewer)
		assert.False(t, ops.loggedContains("openai/gpt-6"), "logs=%v", ops.logs)
	})
}
