package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/mob"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyCommandBlock(t *testing.T) {
	assert.Empty(t, verifyCommandBlock(verifyPlan{}), "an empty plan yields no block (prompt unchanged)")
	assert.Empty(t, verifyCommandBlock(verifyPlan{Source: verifySourceNone}), "a skip plan yields no block")

	out := verifyCommandBlock(verifyPlan{Argv: []string{"go", "test", "./..."}, Display: "go test ./...", Source: verifySourceDetected})
	assert.Contains(t, out, "`verify` tool", "the coder must be sent to the tool, not to a bare command string")
	assert.Contains(t, out, "`go test ./...` (detected)")
}

// prohibitionSentence returns the sentence of s that forbids something, so a
// test can assert on the scope of a prohibition without pinning the rest of the
// prose around it.
func prohibitionSentence(t *testing.T, s string) string {
	t.Helper()

	for _, sentence := range strings.Split(s, ". ") {
		if strings.Contains(sentence, "not run") {
			return sentence
		}
	}

	t.Fatalf("no prohibition sentence in %q", s)

	return ""
}

// The bash prohibition must name the command the tool runs. The tool reaches
// exactly one command, so a blanket "do not run the checks yourself" forbids the
// format, lint and build rungs it cannot reach without covering them.
func TestVerifyCommandBlockScopesTheBashProhibition(t *testing.T) {
	out := verifyCommandBlock(verifyPlan{Argv: []string{"make", "check"}, Display: "make check", Source: verifySourceDeclared})

	assert.Contains(t, prohibitionSentence(t, out), "make check",
		"the prohibition must be scoped to the command the tool runs")
}

func TestFixVerifyLine(t *testing.T) {
	// Empty plan keeps the generic wording (line break mended: no embedded newline).
	generic := fixVerifyLine(verifyPlan{})
	assert.Equal(t, "Run the project's tests after your changes to confirm they pass.", generic)

	out := fixVerifyLine(verifyPlan{Argv: []string{"cargo", "test"}, Display: "cargo test", Source: verifySourceProposed})
	assert.Contains(t, out, "`cargo test` (model-proposed)")
}

// TestCoderPromptEmptyVerifyByteIdentical proves the empty-verify coder prompt is
// byte-identical to the pre-verify wording, so no-gate runs are unaffected.
func TestCoderPromptEmptyVerifyByteIdentical(t *testing.T) {
	withEmpty := fmt.Sprintf(coderPrompt, "", "", "/ws", verifyCommandBlock(verifyPlan{}), "st", "sb", "pt", "pb")
	assert.Contains(t, withEmpty, "that already passed.\n\n"+buildHygieneNote,
		"an empty verify block leaves the coder prompt spacing unchanged")
}

func TestFeedbackBlock(t *testing.T) {
	assert.Empty(t, feedbackBlock("   "), "empty feedback collapses to nothing")
	out := feedbackBlock("split subtask 2")
	assert.Contains(t, out, "REQUESTED CHANGES")
	assert.Contains(t, out, "split subtask 2")
}

func TestDesignBlock(t *testing.T) {
	assert.Empty(t, designBlock(""), "empty design collapses to nothing")
	assert.Empty(t, designBlock("   "), "whitespace-only design collapses to nothing")
	out := designBlock("## Design\n\nUse option A.")
	assert.Contains(t, out, "AGREED DESIGN")
	assert.Contains(t, out, "## Design\n\nUse option A.")
}

// TestFencedDiff pins the markdown fencing of diffs relayed to the board
// chat: one ```diff block, trailing newline normalized, and a fence that
// grows past any backtick run embedded in the diff so it cannot break out.
func TestFencedDiff(t *testing.T) {
	plain := "diff --git a/f.go b/f.go\n-old\n+new\n"
	assert.Equal(t, "```diff\ndiff --git a/f.go b/f.go\n-old\n+new\n```", fencedDiff(plain))

	// A diff touching a markdown file can itself contain a ``` fence; the
	// wrapper must be strictly longer so renderers keep it one block.
	embedded := "diff --git a/README.md b/README.md\n+```go\n+fmt.Println()\n+```\n"
	out := fencedDiff(embedded)
	assert.True(t, strings.HasPrefix(out, "````diff\n"), "fence must outgrow the embedded ``` run: %q", out)
	assert.True(t, strings.HasSuffix(out, "\n````"), "closing fence must match the opening one: %q", out)

	assert.Equal(t, "```diff\n\n```", fencedDiff(""), "empty diff still yields a well-formed block")
}

// TestReviewBriefingRendersGrounding asserts that the review briefing template
// accepts and embeds a non-empty repo-grounding value without producing a
// %! formatting artifact (placeholder/argument count mismatch).
func TestReviewBriefingRendersGrounding(t *testing.T) {
	g := "REPO GROUNDING\nFollow the repo conventions.\n"
	out := fmt.Sprintf(reviewBriefing, g, "", "My Title", "My Description", fencedDiff("diff - old +new"), "previous findings", "")
	assert.Contains(t, out, g, "review briefing must embed the grounding value")
	assert.NotContains(t, out, "%!", "review briefing must have no formatting artifact")
}

// TestCheckpointBriefingRendersGrounding asserts that the checkpoint briefing
// template accepts and embeds a non-empty repo-grounding value without
// producing a %! formatting artifact.
func TestCheckpointBriefingRendersGrounding(t *testing.T) {
	g := "REPO GROUNDING\nCheck the CLAUDE.md.\n"
	out := fmt.Sprintf(checkpointBriefing, g, "", "Subtask Title", "Subtask description", "Parent Card Title", "ENV go 1.22", fencedDiff("diff - old +new"))
	assert.Contains(t, out, g, "checkpoint briefing must embed the grounding value")
	assert.NotContains(t, out, "%!", "checkpoint briefing must have no formatting artifact")
}

// TestReadRootsBlockEmptyKeepsPromptSpacing pins that an undeclared root list
// leaves the three widened prompts byte-identical at the join, the same way the
// coder prompt's empty verify block does.
func TestReadRootsBlockEmptyKeepsPromptSpacing(t *testing.T) {
	assert.Empty(t, readRootsBlock(nil))

	plan := fmt.Sprintf(planPrompt, "", "", "/ws", readRootsBlock(nil), "t", "b", "", "", "", "", "")
	assert.Contains(t, plan, "paths are relative to it.\n\nFirst understand",
		"an empty roots block leaves the plan prompt spacing unchanged")

	diagnose := fmt.Sprintf(diagnosePrompt, "", "/ws", readRootsBlock(nil), "t", "b")
	assert.Contains(t, diagnose, "paths are relative to it.\n\nWork the evidence",
		"an empty roots block leaves the diagnosis prompt spacing unchanged")

	specialist := fmt.Sprintf(specialistPrompt, "", "", readRootsBlock(nil), "LENS", "t", "b", "d", "", "")
	assert.Contains(t, specialist, "one line of evidence.\n\nLENS\n\nReview only",
		"an empty roots block leaves the specialist prompt spacing unchanged")
}

// TestReadRootsBlockEmptyKeepsBriefingSpacing pins that an undeclared root list
// leaves the three mob briefings byte-identical at the join, the same way the
// solo prompts collapse theirs.
func TestReadRootsBlockEmptyKeepsBriefingSpacing(t *testing.T) {
	plan := fmt.Sprintf(planBriefing, "", "", "/ws", readRootsBlock(nil), "t", "b", "", "", "")
	assert.Contains(t, plan, "real code structure.\n\nP",
		"an empty roots block leaves the plan briefing spacing unchanged")

	review := fmt.Sprintf(reviewBriefing, "", readRootsBlock(nil), "t", "b", fencedDiff("d"), "", "")
	assert.Contains(t, review, "survive rebuttal.\n\n"+unreachableVerifyInstruction+"\n\nPARENT CARD",
		"an empty roots block leaves the review briefing spacing unchanged")

	checkpoint := fmt.Sprintf(checkpointBriefing, "", readRootsBlock(nil), "t", "b", "p", "env", fencedDiff("d"))
	assert.Contains(t, checkpoint, "survive rebuttal.\n\nSUBTASK",
		"an empty roots block leaves the checkpoint briefing spacing unchanged")
}

// TestMobBriefingsNameReadRoots pins that a mob seat is told about the trees its
// read tools can reach. The seats run on the same read registry the solo phases
// do, widened with the operator's roots, so a briefing that never names them
// leaves the seat with an access it has no reason to use.
func TestMobBriefingsNameReadRoots(t *testing.T) {
	const root = "/opt/deps/registry"

	t.Run("plan", func(t *testing.T) {
		o := mobPlanRun(&fakeOps{}, &planLLM{}, &scriptedEngine{})
		o.d.ReadRoots = []string{root}

		assert.Contains(t, o.mobPlanBriefing("", ""), root)
	})

	t.Run("review", func(t *testing.T) {
		o := mobReviewRun(t, &fakeOps{}, &fakeGit{}, &planLLM{}, &scriptedEngine{})
		o.d.ReadRoots = []string{root}

		briefing, err := o.mobReviewBriefing(context.Background())
		require.NoError(t, err)
		assert.Contains(t, briefing, root)
	})

	t.Run("checkpoint", func(t *testing.T) {
		eng := &scriptedEngine{outcomes: []mob.Outcome{{Synthesis: `{"verdict":"proceed","fixes":[]}`}}}
		o := mobTestRun(&fakeOps{}, MobConfig{
			Participants: 2, Execute: true, CheckpointMinTier: "simple", CheckpointRounds: 2,
		}, 0)
		o.d.ReadRoots = []string{root}
		o.mobEngine = eng.run
		o.solver.git = &diffGit{fakeGit: &fakeGit{}, diff: "diff --git a/a.go b/a.go\n+lgtm\n"}

		o.mobCheckpoint(context.Background(), o.solver,
			subtaskRef{ID: "SUB-1", Title: "t", Sizing: seedSizing("simple")}, "abc123")

		require.Len(t, eng.topics, 1)
		assert.Contains(t, eng.topics[0].Briefing, root)
	})
}
