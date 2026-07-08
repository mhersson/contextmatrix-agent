package orchestrator

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSelfReviewInBothCodingPrompts(t *testing.T) {
	for name, p := range map[string]string{"coder": coderPrompt, "fix": fixPrompt} {
		assert.Contains(t, p, "self-review", "%s prompt must include the self-review block", name)
		assert.Contains(t, p, "Re-read every file you changed", name)
		assert.Contains(t, p, "no fall-through after writing an error response", name)
	}
}

func TestSpecialistPromptScopesToTask(t *testing.T) {
	assert.Contains(t, specialistPrompt, "not an idealized production service")
	assert.Contains(t, specialistPrompt, "speculative abstractions")
	// trimmed gold-plating solicitations:
	assert.NotContains(t, designPrompt, "API / interface design at module boundaries")
	assert.NotContains(t, securityPrompt, "caching effectiveness")
}

func TestSynthesisPromptGatesScope(t *testing.T) {
	assert.Contains(t, synthesisPrompt, "never blocking")
	assert.Contains(t, synthesisPrompt, "acceptance criteria")
	assert.Contains(t, synthesisPrompt, "remove them")
}

func TestFixPromptForbidsNewArchitecture(t *testing.T) {
	assert.Contains(t, fixPrompt, "add no new abstractions")
	assert.Contains(t, fixPrompt, "flag it, don't build it")
}

func TestBuildArtifactHygieneInBothCodingPrompts(t *testing.T) {
	for name, p := range map[string]string{"coder": coderPrompt, "fix": fixPrompt} {
		assert.Contains(t, p, "do not leave the binary behind",
			"%s prompt must include the build-hygiene note", name)
	}
}

// guard: the document prompt must carry the conservative gate, the docs-only
// restriction, the no-git instruction, and the finish-tool docs(...) convention.
func TestDocumentPromptShape(t *testing.T) {
	low := strings.ToLower(documentPrompt)
	assert.Contains(t, low, "default: no external documentation is needed")
	assert.Contains(t, low, "documentation only")
	assert.Contains(t, low, "user-facing behavior")
	assert.Contains(t, low, "api contracts")
	assert.Contains(t, low, "do not run git")
	assert.Contains(t, low, "finish tool")
	assert.NotContains(t, low, "commit:")
}

// guard: the fix prompt must reference the finish tool and carry no remnant of
// the removed commit-prefix convention.
func TestFixPromptShape(t *testing.T) {
	low := strings.ToLower(fixPrompt)
	assert.Contains(t, low, "finish tool")
	assert.NotContains(t, low, "commit:")
}

func TestBrainstormPromptShape(t *testing.T) {
	low := strings.ToLower(brainstormPrompt)
	assert.Contains(t, low, "one question at a time")
	assert.Contains(t, low, "2-3 approaches")
	assert.Contains(t, low, "## design")
	assert.Contains(t, low, "design_complete")
	assert.Contains(t, low, "read-only")
}

func TestFeedbackBlock(t *testing.T) {
	assert.Empty(t, feedbackBlock("   "), "empty feedback collapses to nothing")
	out := feedbackBlock("split subtask 2")
	assert.Contains(t, out, "REQUESTED CHANGES")
	assert.Contains(t, out, "split subtask 2")
}

func TestDiagnosePromptRigor(t *testing.T) {
	low := strings.ToLower(diagnosePrompt)
	assert.Contains(t, low, "similar path that works")
	assert.Contains(t, low, "hypothes")
	assert.Contains(t, low, "### test approach")
	assert.Contains(t, low, "### risk / scope notes")
}

func TestDesignBlock(t *testing.T) {
	assert.Empty(t, designBlock(""), "empty design collapses to nothing")
	assert.Empty(t, designBlock("   "), "whitespace-only design collapses to nothing")
	out := designBlock("## Design\n\nUse option A.")
	assert.Contains(t, out, "AGREED DESIGN")
	assert.Contains(t, out, "## Design\n\nUse option A.")
}

func TestPromptsCarryRepoRoot(t *testing.T) {
	for name, tpl := range map[string]string{
		"coder":    coderPrompt,
		"fix":      fixPrompt,
		"document": documentPrompt,
		"diagnose": diagnosePrompt,
		"plan":     planPrompt,
	} {
		assert.Contains(t, tpl, "Repo root: %s", "the %s prompt must name the repo root", name)
	}
}

func TestCoderPromptDiscouragesRepeatVerification(t *testing.T) {
	assert.Contains(t, coderPrompt, "finish immediately",
		"the coder prompt sets the stop-when-green expectation early")
}

// guard: the plan prompt must emphatically forbid test-only / test-pinning
// subtasks. A prior run produced a subtask titled "pin ... with a prompts test"
// despite the softer "do NOT create separate write-tests subtasks" wording, so
// the rule is strengthened to name that failure mode.
func TestPlanPromptForbidsTestOnlySubtasks(t *testing.T) {
	low := strings.ToLower(planPrompt)
	assert.Contains(t, low, "do not create separate")
	assert.Contains(t, low, "testing, pinning, asserting, or verifying another subtask's code")
	assert.Contains(t, low, "writes and runs its own tests")
}
