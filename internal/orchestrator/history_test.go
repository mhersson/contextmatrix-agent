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
		{ID: "X-2", Title: "Sysinfo pkg", Sizing: seedSizing("simple"), Body: "Create go.mod and a sysinfo package.\n\nFiles: go.mod, sysinfo/sysinfo.go"},
		{ID: "X-3", Title: "HTTP server", Sizing: seedSizing("simple"), DependsOnIDs: []string{"X-2"}, Body: "Add main.go serving GET /."},
	}
	got := formatPlan(subs)
	assert.Contains(t, got, "### 1. X-2 - Sysinfo pkg")
	assert.Contains(t, got, "_Bar: simple · Turns: base · Depends on: none_")
	assert.Contains(t, got, "### 2. X-3 - HTTP server")
	assert.Contains(t, got, "_Bar: simple · Turns: base · Depends on: X-2_")
	// Body is its own block, not crammed onto a "Body:" line.
	assert.NotContains(t, got, "Body:")
	assert.Contains(t, got, "Create go.mod and a sysinfo package.")
}

func TestFormatPlannedPlanWithFollowups(t *testing.T) {
	p := plan{
		CardTier: "moderate",
		Subtasks: []planSubtask{{Title: "Wire the endpoint", Tier: "simple"}},
		FollowupCards: []planFollowup{
			{Title: "Extract config loader", DependsOnOriginal: true},
			{Title: "Add config docs", DependsOn: []int{0}},
		},
	}

	got := formatPlannedPlan(p)

	assert.Contains(t, got, "### Follow-up cards")
	assert.Contains(t, got, "inheriting this card's autonomous flag")
	assert.Contains(t, got, "Follow-up #1: Extract config loader")
	assert.Contains(t, got, "depends on: this card")
	assert.Contains(t, got, "Follow-up #2: Add config docs")
	assert.Contains(t, got, "depends on: follow-up #1")
}

func TestFormatPlannedPlanNoFollowupsOmitsSection(t *testing.T) {
	p := plan{CardTier: "simple", Subtasks: []planSubtask{{Title: "Only task", Tier: "simple"}}}

	got := formatPlannedPlan(p)

	assert.NotContains(t, got, "Follow-up")
}

// TestFormatPlannedPlanWithUnreachable proves the HITL plan-approval gate
// shows unreachable acceptance criteria before a human approves - without
// this the human cannot see which criteria review will exempt from the
// verdict.
func TestFormatPlannedPlanWithUnreachable(t *testing.T) {
	p := plan{
		CardTier: "moderate",
		Subtasks: []planSubtask{{Title: "Wire the endpoint", Tier: "simple"}},
		Unreachable: []planUnreachable{
			{Criterion: "staging deploy succeeds", Reason: "no staging access from this container"},
		},
	}

	got := formatPlannedPlan(p)

	assert.Contains(t, got, "### Unreachable acceptance criteria")
	assert.Contains(t, got, "staging deploy succeeds")
}

func TestFormatPlannedPlanNoUnreachableOmitsSection(t *testing.T) {
	p := plan{CardTier: "simple", Subtasks: []planSubtask{{Title: "Only task", Tier: "simple"}}}

	got := formatPlannedPlan(p)

	assert.NotContains(t, got, "Unreachable")
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

// TestLastSectionsWithPrefixWindows proves the windowed reader returns only the
// most recent n matching sections, keeping non-matching sections out entirely.
func TestLastSectionsWithPrefixWindows(t *testing.T) {
	body := "intro\n" +
		"## Review Findings\nround one text\n" +
		"## Plan\nplan text\n" +
		"## Review Findings (Round 2)\nround two text\n" +
		"## Review Findings (Round 3)\nround three text\n" +
		"## Review Findings (Round 4)\nround four text\n"

	got := lastSectionsWithPrefix(body, "Review Findings", 2)

	assert.Contains(t, got, "round three text")
	assert.Contains(t, got, "round four text")
	assert.NotContains(t, got, "round one text")
	assert.NotContains(t, got, "round two text")
	assert.NotContains(t, got, "plan text")
}

// TestLastSectionsWithPrefixUnderWindow proves a body with fewer sections than
// the window comes back whole - identical to the unwindowed reader.
func TestLastSectionsWithPrefixUnderWindow(t *testing.T) {
	body := "## Review Findings\nonly round\n"

	assert.Equal(t, sectionsWithPrefix(body, "Review Findings"),
		lastSectionsWithPrefix(body, "Review Findings", 3))
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
			body: "Intro.\n\n## Diagnosis\n\nd\n\n## Design\n\nds\n\n## Plan\n\np\n\n## Discussion\n\ndc\n\n## Execute Discussions\n\ned\n\n## Best-of-N Report\n\nb\n\n## Verify Command\n\nv\n\n## Review Findings\n\nr\n\n## PR Gates\n\ng\n",
			want: "Intro.",
		},
		{
			name: "strips numbered review rounds via prefix",
			body: "Intro.\n\n## Review Findings (Round 2)\n\nr2\n\n## Review Findings (Round 10)\n\nr10\n",
			want: "Intro.",
		},
		{
			name: "strips the bare copilot review heading",
			body: "Intro.\n\n## Copilot Review\n\nc1\n",
			want: "Intro.",
		},
		{
			name: "strips numbered copilot rounds via prefix",
			body: "Intro.\n\n## Copilot Review (Round 2)\n\nc2\n\n## Copilot Review (Round 10)\n\nc10\n",
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
