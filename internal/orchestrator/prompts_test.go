package orchestrator

import (
	"fmt"
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

func TestVerifyCommandBlock(t *testing.T) {
	assert.Empty(t, verifyCommandBlock(verifyPlan{}), "an empty plan yields no block (prompt unchanged)")
	assert.Empty(t, verifyCommandBlock(verifyPlan{Source: verifySourceNone}), "a skip plan yields no block")

	out := verifyCommandBlock(verifyPlan{Argv: []string{"go", "test", "./..."}, Display: "go test ./...", Source: verifySourceDetected})
	assert.Contains(t, out, "The project's verify command is `go test ./...` (detected)")
	assert.Contains(t, out, "make it pass")
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
		assert.Contains(t, p, "do not leave its output",
			"%s prompt must include the build-hygiene note", name)
		// The hygiene note must name no build tool — it applies to every language.
		assert.NotContains(t, p, "go build",
			"%s prompt build-hygiene note must stay language-neutral", name)
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

// allPhasePrompts is every phase prompt constant defined in prompts.go, keyed by
// a readable name for failure messages. The neutrality sweep runs over all of
// them so a language-specific token cannot slip back into any single prompt.
var allPhasePrompts = map[string]string{
	"plan":         planPrompt,
	"diagnose":     diagnosePrompt,
	"buildHygiene": buildHygieneNote,
	"selfReview":   selfReviewBlock,
	"coder":        coderPrompt,
	"specialist":   specialistPrompt,
	"correctness":  correctnessPrompt,
	"design":       designPrompt,
	"security":     securityPrompt,
	"synthesis":    synthesisPrompt,
	"fix":          fixPrompt,
	"prBody":       prBodyPrompt,
	"document":     documentPrompt,
	"gateClassify": gateClassifyPrompt,
	"brainstorm":   brainstormPrompt,
}

// TestPromptsAreLanguageNeutral sweeps every phase prompt for target-language and
// target-ecosystem tokens. The agent is language-agnostic w.r.t. the target repo,
// so no prompt may name a specific toolchain. The concurrency ban is the precise
// "goroutine leaks", not bare "goroutine": the correctness lens legitimately lists
// "threads, tasks, coroutines, goroutines" as an inclusive, cross-language set of
// worker kinds, which must stay allowed.
func TestPromptsAreLanguageNeutral(t *testing.T) {
	banned := []string{
		"go build", "go test", "goroutine leaks", "golang", "gofmt",
		"make test", "npm ", "typescript",
	}

	for name, p := range allPhasePrompts {
		low := strings.ToLower(p)
		for _, b := range banned {
			assert.NotContainsf(t, low, b, "%s prompt must stay language-neutral (found %q)", name, b)
		}
	}
}

// TestPromptsCarryNeutralisedStrings pins the language-neutral replacements from
// the de-Go'ing of the prompts: each must survive in the specific prompt that
// carries it, so a future edit cannot silently drop the neutral wording (or
// re-introduce a Go-specific phrasing in its place).
func TestPromptsCarryNeutralisedStrings(t *testing.T) {
	assert.Contains(t, correctnessPrompt, "leaked concurrent workers",
		"the correctness lens must keep the language-neutral concurrency wording")
	assert.Contains(t, designPrompt, "cross-module coupling",
		"the design lens must keep the neutral coupling wording")
	assert.Contains(t, designPrompt, "unused public symbols",
		"the design lens must keep the neutral dead-symbol wording")
	assert.Contains(t, synthesisPrompt, "passing verify run",
		"synthesis must weigh a passing verify run, not a language-specific test command")
	assert.Contains(t, planPrompt, "keeps the tree passing its checks",
		"the plan prompt must keep the neutral 'passing its checks' wording")
}
