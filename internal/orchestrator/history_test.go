package orchestrator

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUpsertSection_AppendsWhenAbsent(t *testing.T) {
	body := "Original task description."

	got := upsertSection(body, "Diagnosis", "## Diagnosis\n\nRoot cause: X")

	assert.Contains(t, got, "Original task description.")
	assert.Contains(t, got, "## Diagnosis\n\nRoot cause: X")
}

func TestUpsertSection_ReplacesExisting(t *testing.T) {
	body := "Desc.\n\n## Diagnosis\n\nOld cause\n\n## Plan\n\n1. SUBTASK A"

	got := upsertSection(body, "Diagnosis", "## Diagnosis\n\nNew cause")

	assert.Contains(t, got, "New cause")
	assert.NotContains(t, got, "Old cause")
	// The following Plan section is preserved and neither section duplicated.
	assert.Equal(t, 1, strings.Count(got, "## Diagnosis"))
	assert.Equal(t, 1, strings.Count(got, "## Plan"))
	assert.Contains(t, got, "1. SUBTASK A")
}

func TestUpsertSection_ExactHeadingMatch(t *testing.T) {
	// "## Review Findings" must not match "## Review Findings (Round 2)".
	body := "## Review Findings\n\nround one\n\n## Review Findings (Round 2)\n\nround two"

	got := upsertSection(body, "Review Findings", "## Review Findings\n\nROUND ONE UPDATED")

	assert.Contains(t, got, "ROUND ONE UPDATED")
	assert.NotContains(t, got, "round one")
	assert.Contains(t, got, "## Review Findings (Round 2)")
	assert.Contains(t, got, "round two")
}

func TestSectionFrom(t *testing.T) {
	assert.Equal(t, "## Diagnosis\n\nbody", sectionFrom("Diagnosis", "body"))
	// Already-headed content is returned as-is (trimmed), not double-wrapped.
	assert.Equal(t, "## Diagnosis\nalready", sectionFrom("Diagnosis", "  ## Diagnosis\nalready  "))
}

func TestFormatPlan(t *testing.T) {
	subs := []subtaskRef{
		{ID: "C-1", Title: "First", Body: "do A", Tier: "moderate"},
		{ID: "C-2", Title: "Second", Body: "do B", Tier: "simple", DependsOnIDs: []string{"C-1"}},
	}

	got := formatPlan(subs)

	assert.Contains(t, got, "1. SUBTASK C-1: First")
	assert.Contains(t, got, "Tier: moderate | Depends on: (none)")
	assert.Contains(t, got, "2. SUBTASK C-2: Second")
	assert.Contains(t, got, "Depends on: C-1")
}

func TestRecordReview_RoundHeadingsAndVerdict(t *testing.T) {
	o := &run{d: Deps{Ops: &fakeOps{}, Cfg: Config{CardID: "CARD-1"}}, body: "Task."}
	ops := o.d.Ops.(*fakeOps)

	o.recordReview(t.Context(), 1, "first round findings", false)
	o.recordReview(t.Context(), 2, "second round findings", true)

	body := ops.lastBody()
	// Round 1 uses the bare heading; round 2 is numbered. Both preserved.
	assert.Contains(t, body, "## Review Findings\n")
	assert.Contains(t, body, "## Review Findings (Round 2)")
	assert.Contains(t, body, "first round findings")
	assert.Contains(t, body, "second round findings")
	assert.Contains(t, body, "### Recommendation\n\nrevise")
	assert.Contains(t, body, "### Recommendation\n\napprove")
}
