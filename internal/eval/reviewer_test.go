package eval

import (
	"context"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVerdict(t *testing.T) {
	a, d, ok := parseVerdict("blah\nVERDICT: REQUEST_CHANGES\nDEFECT: last.go:Last - off by one")
	assert.True(t, ok)
	assert.False(t, a)
	assert.Contains(t, d, "Last")

	a, _, ok = parseVerdict("looks fine\nVERDICT: APPROVE")
	assert.True(t, ok)
	assert.True(t, a)

	_, _, ok = parseVerdict("no verdict here")
	assert.False(t, ok)
}

func TestReviewerCheckMutant(t *testing.T) {
	task := ReviewerTask{name: "offbyone", plantedSymbol: "Last"}
	// Caught: requests changes citing Last.
	v, err := task.Check(context.Background(), "", harness.Result{Output: "VERDICT: REQUEST_CHANGES\nDEFECT: last.go:Last - index out of range"})
	require.NoError(t, err)
	assert.True(t, v.OK)
	// Missed: approves the buggy diff.
	v, _ = task.Check(context.Background(), "", harness.Result{Output: "VERDICT: APPROVE"})
	assert.False(t, v.OK)
	// Wrong location: requests changes but doesn't cite the symbol.
	v, _ = task.Check(context.Background(), "", harness.Result{Output: "VERDICT: REQUEST_CHANGES\nDEFECT: other.go:Foo - style"})
	assert.False(t, v.OK)
}

func TestReviewerCheckClean(t *testing.T) {
	task := ReviewerTask{name: "clean_guard", wantApprove: true}
	v, _ := task.Check(context.Background(), "", harness.Result{Output: "VERDICT: APPROVE"})
	assert.True(t, v.OK)
	v, _ = task.Check(context.Background(), "", harness.Result{Output: "VERDICT: REQUEST_CHANGES\nDEFECT: guard.go:First - nit"})
	assert.False(t, v.OK, "over-flagging a clean diff fails specificity")
}
