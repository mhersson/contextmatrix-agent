package orchestrator

import (
	"context"
	"testing"

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
