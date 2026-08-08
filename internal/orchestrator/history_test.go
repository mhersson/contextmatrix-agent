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

func TestFormatPlanReadable(t *testing.T) {
	subs := []subtaskRef{
		{ID: "X-2", Title: "Sysinfo pkg", Tier: "simple", Body: "Create go.mod and a sysinfo package.\n\nFiles: go.mod, sysinfo/sysinfo.go"},
		{ID: "X-3", Title: "HTTP server", Tier: "simple", DependsOnIDs: []string{"X-2"}, Body: "Add main.go serving GET /."},
	}
	got := formatPlan(subs)
	assert.Contains(t, got, "### 1. X-2 - Sysinfo pkg")
	assert.Contains(t, got, "_Tier: simple · Depends on: none_")
	assert.Contains(t, got, "### 2. X-3 - HTTP server")
	assert.Contains(t, got, "_Tier: simple · Depends on: X-2_")
	// Body is its own block, not crammed onto a "Body:" line.
	assert.NotContains(t, got, "Body:")
	assert.Contains(t, got, "Create go.mod and a sysinfo package.")
}

func TestRecordReview_RoundHeadingsAndVerdict(t *testing.T) {
	o := &run{d: Deps{Ops: &fakeOps{}, Cfg: Config{CardID: "CARD-1"}}, body: "Task."}
	ops := o.d.Ops.(*fakeOps)

	o.recordReview(t.Context(), 1, "first round findings", false, verifyResult{Status: verifyPassed})
	o.recordReview(t.Context(), 2, "second round findings", true, verifyResult{Status: verifySkipped, Note: "tool missing"})

	body := ops.lastBody()
	// Round 1 uses the bare heading; round 2 is numbered. Both preserved.
	assert.Contains(t, body, "## Review Findings\n")
	assert.Contains(t, body, "## Review Findings (Round 2)")
	assert.Contains(t, body, "first round findings")
	assert.Contains(t, body, "second round findings")
	assert.Contains(t, body, "### Recommendation\n\nrevise")
	assert.Contains(t, body, "### Recommendation\n\napprove")
	// Each round leads with its verify status, recorded by code.
	assert.Contains(t, body, "**Verify:** PASSED")
	assert.Contains(t, body, "**Verify:** SKIPPED - tool missing")
}

func TestExtractSection_ReturnsBlockOrEmpty(t *testing.T) {
	body := "Desc.\n\n## Execute Discussions\n\n### SUB-1 - a\nx\n\n## Plan\n\n1. y"

	got := extractSection(body, "Execute Discussions")

	assert.Equal(t, "## Execute Discussions\n\n### SUB-1 - a\nx", got)
	assert.Empty(t, extractSection(body, "Nope"))
	assert.Empty(t, extractSection("", "Execute Discussions"))
}

func TestExtractSection_RunsToEndWhenLast(t *testing.T) {
	body := "Desc.\n\n## Execute Discussions\n\n### SUB-1 - a\nx\n"

	got := extractSection(body, "Execute Discussions")

	assert.Equal(t, "## Execute Discussions\n\n### SUB-1 - a\nx", got)
}

func TestStripAgentSections(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "strips every recorded heading",
			body: "Intro.\n\n## Diagnosis\n\nd\n\n## Design\n\nds\n\n## Plan\n\np\n\n## Discussion\n\ndc\n\n## Execute Discussions\n\ned\n\n## Best-of-N Report\n\nb\n\n## Verify Command\n\nv\n\n## Review Findings\n\nr\n",
			want: "Intro.",
		},
		{
			name: "strips numbered review rounds via prefix",
			body: "Intro.\n\n## Review Findings (Round 2)\n\nr2\n\n## Review Findings (Round 10)\n\nr10\n",
			want: "Intro.",
		},
		{
			name: "keeps human heading that extends an exact-match name",
			body: "Intro.\n\n## Planning notes\n\nhuman notes\n\n## Plan\n\nagent plan\n",
			want: "Intro.\n\n## Planning notes\n\nhuman notes",
		},
		{
			name: "prefix match strips any review findings variant",
			body: "Intro.\n\n## Review Findings from QA\n\nqa notes\n",
			want: "Intro.",
		},
		{
			name: "section at body start",
			body: "## Plan\n\np\n\n## Context\n\nhuman context\n",
			want: "## Context\n\nhuman context",
		},
		{
			name: "whole body is agent sections",
			body: "## Plan\n\np\n\n## Review Findings\n\nr\n",
			want: "",
		},
		{
			name: "interleaved human sections survive",
			body: "Intro.\n\n## Requirements\n\nreq\n\n## Plan\n\np\n\n## Acceptance\n\nacc\n",
			want: "Intro.\n\n## Requirements\n\nreq\n\n## Acceptance\n\nacc",
		},
		{
			name: "consecutive agent sections",
			body: "Intro.\n\n## Diagnosis\n\nd\n\n## Plan\n\np\n\n## Follow-up\n\nf\n",
			want: "Intro.\n\n## Follow-up\n\nf",
		},
		{
			name: "h3 heading does not end a stripped block",
			body: "Intro.\n\n## Plan\n\n### Detail\n\nx\n\n## Keep\n\nk\n",
			want: "Intro.\n\n## Keep\n\nk",
		},
		{
			name: "indented agent heading starts a strip",
			body: "Intro.\n\n  ## Plan\n\np\n\n## Keep\n\nk\n",
			want: "Intro.\n\n## Keep\n\nk",
		},
		{
			name: "no agent sections returns input byte identical",
			body: "Intro.\n\n\n## Human\n\n\nweird   spacing\n\n\n",
			want: "Intro.\n\n\n## Human\n\n\nweird   spacing\n\n\n",
		},
		{
			name: "empty body",
			body: "",
			want: "",
		},
		{
			name: "fenced agent heading is stripped like the writer would",
			body: "Intro.\n\n## Keep\n\n```markdown\n## Plan\n\ninside\n```\n\ntail\n",
			want: "Intro.\n\n## Keep\n\n```markdown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, stripAgentSections(tt.body))
		})
	}
}
