package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatApproval_RendersSection(t *testing.T) {
	a := approval{
		HeadSHA: "abc123def456",
		Summary: "looks good with minor nits",
		Fixes: []fix{
			{File: "a.go", Issue: "nit", Severity: "nit", Suggestion: "tidy"},
		},
	}

	got := formatApproval(a)

	assert.Contains(t, got, "## Review Approval")
	assert.Contains(t, got, "Commit: abc123def456")
	assert.Contains(t, got, "```json")
	assert.Contains(t, got, `"head_sha":"abc123def456"`)
	assert.Contains(t, got, `"summary":"looks good with minor nits"`)
	assert.Contains(t, got, `"fixes"`)
	assert.Contains(t, got, `"file":"a.go"`)
}

func TestFormatApproval_NoFixes(t *testing.T) {
	a := approval{
		HeadSHA: "abc123",
		Summary: "clean",
		Fixes:   nil,
	}

	got := formatApproval(a)

	assert.Contains(t, got, "## Review Approval")
	assert.Contains(t, got, "Commit: abc123")
	assert.Contains(t, got, `"fixes":null`)
}

func TestExtractApproval_ValidRoundTrip(t *testing.T) {
	a := approval{
		HeadSHA: "abc123def456",
		Summary: "looks good with minor nits",
		Fixes: []fix{
			{File: "a.go", Issue: "nit", Severity: "nit", Suggestion: "tidy"},
			{File: "b.go", Issue: "dead code", Severity: "minor", Suggestion: "remove"},
		},
	}

	section := formatApproval(a)
	body := "Intro.\n\n" + section + "\n\n## Keep\n\nhuman text"

	got, ok := extractApproval(body)
	require.True(t, ok)
	assert.Equal(t, "abc123def456", got.HeadSHA)
	assert.Equal(t, "looks good with minor nits", got.Summary)
	require.Len(t, got.Fixes, 2)
	assert.Equal(t, "a.go", got.Fixes[0].File)
	assert.Equal(t, "nit", got.Fixes[0].Severity)
	assert.Equal(t, "b.go", got.Fixes[1].File)
	assert.Equal(t, "minor", got.Fixes[1].Severity)
}

func TestExtractApproval_EmptyBody(t *testing.T) {
	_, ok := extractApproval("")
	assert.False(t, ok)
}

func TestExtractApproval_NoSection(t *testing.T) {
	body := "## Plan\n\njust a plan"

	_, ok := extractApproval(body)
	assert.False(t, ok)
}

func TestExtractApproval_MalformedJSON(t *testing.T) {
	body := "## Review Approval\n\nCommit: abc123\n\n```json\n{broken\n```"

	_, ok := extractApproval(body)
	assert.False(t, ok)
}

func TestExtractApproval_EmptySHA(t *testing.T) {
	body := "## Review Approval\n\nCommit: \n\n```json\n{\"head_sha\":\"\",\"summary\":\"x\",\"fixes\":null}\n```"

	_, ok := extractApproval(body)
	assert.False(t, ok)
}

// TestRecordApprovalOnApproval records an approval through the run's
// recordSection path and checks that the body contains the approval section
// with a HEAD SHA. This proves the integration between recordApproval and
// the existing section-recording machinery.
func TestRecordApprovalOnApproval(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{headSHA: "abc123def456"}
	d := reviewTestDeps(t, ops, git, &planLLM{}, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.body = "Task intro."

	o.recordApproval(t.Context(), "approved with minor nits", []fix{
		{File: "x.go", Issue: "tidy", Severity: "nit"},
	})

	body := ops.bodyFor("CARD-1")
	require.NotEmpty(t, body)

	assert.Contains(t, body, "## Review Approval")
	assert.Contains(t, body, "abc123def456")
	assert.Contains(t, body, `"head_sha":"abc123def456"`)
	assert.Contains(t, body, "approved with minor nits")

	// The original body content is preserved.
	assert.Contains(t, body, "Task intro.")

	// The Head call happened before the UpdateCardBody call (verified by
	// sequential code flow - recordApproval calls Head, then recordSection
	// calls UpdateCardBody).
	opsCalls := ops.recorded()
	gitCalls := git.recorded()
	assert.Contains(t, gitCalls, "Head")
	assert.Contains(t, opsCalls, "UpdateCardBody:CARD-1")
}

// TestRecordApprovalNoFixes records an approval with an empty fix list.
func TestRecordApprovalNoFixes(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{headSHA: "abc123"}
	d := reviewTestDeps(t, ops, git, &planLLM{}, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.body = "Intro."

	o.recordApproval(t.Context(), "clean approval", nil)

	body := ops.bodyFor("CARD-1")
	require.NotEmpty(t, body)
	assert.Contains(t, body, "## Review Approval")
	assert.Contains(t, body, "abc123")
}

// TestRecordApprovalSkipsOnEmptyHead proves the record is silently skipped
// when HEAD is empty, so a broken repo cannot inject a malformed record.
func TestRecordApprovalSkipsOnEmptyHead(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{headSHA: ""}
	d := reviewTestDeps(t, ops, git, &planLLM{}, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.body = "Intro."

	o.recordApproval(t.Context(), "summary", nil)

	// The body must NOT have been written.
	assert.Empty(t, ops.bodyFor("CARD-1"))
}

// TestRecordApprovalSkipsOnHeadError proves the record is silently skipped
// when HEAD cannot be read.
func TestRecordApprovalSkipsOnHeadError(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{headErr: assertErr("no repo")}
	d := reviewTestDeps(t, ops, git, &planLLM{}, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.body = "Intro."

	o.recordApproval(t.Context(), "summary", nil)

	assert.Empty(t, ops.bodyFor("CARD-1"))
}

// TestStripAgentSections_RemovesReviewApproval proves the approval section
// is stripped from model-facing prompt text via stripAgentSections.
func TestStripAgentSections_RemovesReviewApproval(t *testing.T) {
	body := "Intro.\n\n## Review Approval\n\nCommit: abc123\n\n```json\n{\"head_sha\":\"abc123\"}\n```\n\n## Keep\n\nhuman text"

	got := stripAgentSections(body)

	assert.NotContains(t, got, "## Review Approval")
	assert.NotContains(t, got, "abc123")
	assert.Contains(t, got, "Intro.")
	assert.Contains(t, got, "## Keep")
	assert.Contains(t, got, "human text")
}

// TestRecentReviewFindingsHistory_ExcludesApproval proves that
// recentReviewFindingsHistory (which collects "Review Findings" sections)
// does NOT include the approval section, because the headings are distinct.
func TestRecentReviewFindingsHistory_ExcludesApproval(t *testing.T) {
	body := "Intro.\n\n## Review Findings\n\nRound 1 findings\n\n### Recommendation\n\nrevise\n\n## Review Approval\n\nCommit: abc\n\n```json\n{}\n```\n\n## Keep\n\nk"

	got := recentReviewFindingsHistory(body)

	assert.Contains(t, got, "Round 1 findings")
	assert.NotContains(t, got, "Review Approval")
	assert.NotContains(t, got, "Commit: abc")
}

// TestApprovalRecordedBeforeCleanupPass proves that when recordApproval
// is called during an approved review round, the approval section lands
// on the card body BEFORE any CommitFixup call - the HEAD SHA is captured
// at approval time, before the cleanup pass modifies the tree.
func TestApprovalRecordedBeforeCleanupPass(t *testing.T) {
	ops := &fakeOps{}
	// committed=true so the fix pass actually creates a fixup.
	git := &fakeGit{committed: true, headSHA: "pre-cleanup-sha", headAfterFixup: "post-cleanup-sha"}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: minor nit", 0.01),
		stopResp("Design: looks fine", 0.01),
		stopResp("Security: looks fine", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[{"file":"a.go","issue":"nit","suggestion":"tidy","severity":"minor"}]}`, 0.02),
		stopResp("coder: tidied", 0.05),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.body = "Task."

	require.NoError(t, runReview(context.Background(), o))

	// The body must contain the approval section.
	body := ops.bodyFor("CARD-1")
	require.NotEmpty(t, body)
	assert.Contains(t, body, "## Review Approval")
	assert.Contains(t, body, "pre-cleanup-sha")

	// CommitFixup moves HEAD; the recorded SHA must be the one BEFORE it.
	assert.NotContains(t, body, "post-cleanup-sha",
		"the approval must bind the commit the panel judged, not the fixup")
}

// TestReviewApprovalRecordedOnReviewLoopApproval proves that the approval
// section is recorded when reviewLoop's approval branch is taken (no
// surviving fixes).
func TestReviewApprovalRecordedOnReviewLoopApproval(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{headSHA: "abc123"}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: looks fine", 0.01),
		stopResp("Design: looks fine", 0.01),
		stopResp("Security: looks fine", 0.01),
		stopResp(`{"approved":true,"summary":"clean","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.body = "Task."

	require.NoError(t, runReview(context.Background(), o))

	body := ops.bodyFor("CARD-1")
	require.NotEmpty(t, body)
	assert.Contains(t, body, "## Review Approval")
	assert.Contains(t, body, "abc123")
}

// TestAuthoritativeReviewApprovalRecorded proves that the approval section
// is recorded when authoritativeReview's approval branch is taken.
func TestAuthoritativeReviewApprovalRecorded(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{headSHA: "auth-approve-sha"}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: looks fine", 0.01),
		stopResp("Design: looks fine", 0.01),
		stopResp("Security: looks fine", 0.01),
		// Authoritative verdict: approved.
		stopResp(`{"approved":true,"summary":"authoritative approval","fixes":[]}`, 0.02),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.body = "Task."

	// Send the run to the authoritative pass directly.
	plan := verifyPlan{}
	err := o.authoritativeReview(context.Background(), plan, 2)
	require.NoError(t, err)

	body := ops.bodyFor("CARD-1")
	require.NotEmpty(t, body)
	assert.Contains(t, body, "## Review Approval")
	assert.Contains(t, body, "auth-approve-sha")
}

// TestReviewRejectionNoApproval proves that a rejected review verdict never
// produces an approval section on the body.
func TestReviewRejectionNoApproval(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{headSHA: "some-sha", committed: true}
	client := &planLLM{responses: []llm.Response{
		stopResp("Correctness: has bugs", 0.01),
		stopResp("Design: needs work", 0.01),
		stopResp("Security: has issues", 0.01),
		// Verdict: not approved.
		stopResp(`{"approved":false,"summary":"needs work","fixes":[{"file":"a.go","issue":"bug","suggestion":"fix","severity":"important"}]}`, 0.02),
		// Fix coder response.
		stopResp("coder: fixed", 0.05),
	}}
	d := reviewTestDeps(t, ops, git, client, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.body = "Task."

	_ = runReview(context.Background(), o)

	body := ops.bodyFor("CARD-1")

	// The body may have review findings, increment attempts, etc. But it must
	// NOT have an approval section.
	assert.NotContains(t, body, "## Review Approval")
}

// TestRemoveSection_RemovesApprovalBlock proves removeApprovalSection strips a
// "## Review Approval" block from the body, including its JSON payload.
func TestRemoveSection_RemovesApprovalBlock(t *testing.T) {
	body := "Intro.\n\n## Review Approval\n\nCommit: abc123\n\n```json\n{\"head_sha\":\"abc123\"}\n```\n\n## Keep\n\nhuman text"

	got := removeApprovalSection(body)

	assert.NotContains(t, got, "## Review Approval")
	assert.NotContains(t, got, "abc123")
	assert.NotContains(t, got, "```json")
	assert.Contains(t, got, "Intro.")
	assert.Contains(t, got, "## Keep")
	assert.Contains(t, got, "human text")
}

// TestRemoveSection_AbsentHeading returns body unchanged.
func TestRemoveSection_AbsentHeading(t *testing.T) {
	body := "## Plan\n\nplain body"

	got := removeApprovalSection(body)

	assert.Equal(t, body, got, "removeApprovalSection on an absent heading must return the body unchanged")
}

// TestRemoveSection_LastSection proves removing the final section works.
func TestRemoveSection_LastSection(t *testing.T) {
	body := "## Review Approval\n\nCommit: abc\n\n```json\n{}\n```"

	got := removeApprovalSection(body)

	assert.Empty(t, strings.TrimSpace(got), "removing the only section must leave an empty body")
}

// TestRemoveSection_EmptyBody returns empty.
func TestRemoveSection_EmptyBody(t *testing.T) {
	got := removeApprovalSection("")

	assert.Empty(t, got)
}

// TestClearApproval_UpdatesBody proves clearApproval removes the approval
// section from the body and pushes via UpdateCardBody.
func TestClearApproval_UpdatesBody(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	d := reviewTestDeps(t, ops, git, &planLLM{}, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.body = "Intro.\n\n## Review Approval\n\nCommit: abc\n\n```json\n{\"head_sha\":\"abc\"}\n```\n\n## Keep\n\nhuman text"

	o.clearApproval(t.Context())

	body := ops.bodyFor("CARD-1")
	require.NotEmpty(t, body)

	assert.NotContains(t, body, "## Review Approval")
	assert.NotContains(t, body, "```json")
	assert.Contains(t, body, "Intro.")
	assert.Contains(t, body, "## Keep")
	assert.Contains(t, body, "human text")
}

// TestClearApproval_NoSectionSkips proves clearApproval on a body without an
// approval section still succeeds (no-op).
func TestClearApproval_NoSectionSkips(t *testing.T) {
	ops := &fakeOps{}
	git := &fakeGit{}
	d := reviewTestDeps(t, ops, git, &planLLM{}, reviewerRegistry())

	tc := cmclient.TaskContext{Title: "Parent", Description: "body", State: "in_progress"}
	o := newReviewRun(d, tc, 0)
	o.body = "## Plan\n\nplain"

	o.clearApproval(t.Context())

	body := ops.bodyFor("CARD-1")
	assert.Contains(t, body, "## Plan")
	assert.NotContains(t, body, "## Review Approval")
}
