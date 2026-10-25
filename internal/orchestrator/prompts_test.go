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
	assert.Contains(t, out, "go test ./...", "the block names the command the tool runs")
	assert.NotContains(t, out, "%!", "the block has no formatting artifact")
}

func TestFixVerifyLine(t *testing.T) {
	out := fixVerifyLine(verifyPlan{Argv: []string{"cargo", "test"}, Display: "cargo test", Source: verifySourceProposed})
	assert.Contains(t, out, "cargo test", "the line names the resolved command")
	assert.NotContains(t, out, "%!", "the line has no formatting artifact")
}

func TestFeedbackBlock(t *testing.T) {
	assert.Empty(t, feedbackBlock("   "), "empty feedback collapses to nothing")
	assert.Contains(t, feedbackBlock("split subtask 2"), "split subtask 2")
}

func TestDesignBlock(t *testing.T) {
	assert.Empty(t, designBlock(""), "empty design collapses to nothing")
	assert.Empty(t, designBlock("   "), "whitespace-only design collapses to nothing")
	assert.Contains(t, designBlock("## Design\n\nUse option A."), "## Design\n\nUse option A.")
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

func TestReadRootsBlock(t *testing.T) {
	assert.Empty(t, readRootsBlock(nil), "no declared roots leave the prompt unchanged")

	out := readRootsBlock([]string{"/opt/deps/a", "/opt/deps/b"})
	assert.Contains(t, out, "/opt/deps/a")
	assert.Contains(t, out, "/opt/deps/b")
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
