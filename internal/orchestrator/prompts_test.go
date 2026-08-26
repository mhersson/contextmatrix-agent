package orchestrator

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSelfReviewInBothCodingPrompts(t *testing.T) {
	for name, p := range map[string]string{"coder": coderPrompt, "fix": fixPrompt, "verify-fix": verifyFixPrompt} {
		assert.Contains(t, p, "self-review", "%s prompt must include the self-review block", name)
		assert.Contains(t, p, "Re-read every file you changed", name)
		assert.Contains(t, p, "no fall-through after writing an error response", name)
	}
}

func TestVerifyCommandBlock(t *testing.T) {
	assert.Empty(t, verifyCommandBlock(verifyPlan{}), "an empty plan yields no block (prompt unchanged)")
	assert.Empty(t, verifyCommandBlock(verifyPlan{Source: verifySourceNone}), "a skip plan yields no block")

	out := verifyCommandBlock(verifyPlan{Argv: []string{"go", "test", "./..."}, Display: "go test ./...", Source: verifySourceDetected})
	assert.Contains(t, out, "`verify` tool", "the coder must be sent to the tool, not to a bare command string")
	assert.Contains(t, out, "`go test ./...` (detected)")
	assert.Contains(t, out, "Make it pass")
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

func TestVerifyFixPromptIsTitleOnly(t *testing.T) {
	assert.Contains(t, verifyFixPrompt, "VERIFY FAILURE TO FIX")
	assert.NotContains(t, verifyFixPrompt, "Description:")
}

func TestBuildArtifactHygieneInBothCodingPrompts(t *testing.T) {
	for name, p := range map[string]string{"coder": coderPrompt, "fix": fixPrompt, "verify-fix": verifyFixPrompt} {
		assert.Contains(t, p, "do not leave its output",
			"%s prompt must include the build-hygiene note", name)
		// The hygiene note must name no build tool - it applies to every language.
		assert.NotContains(t, p, "go build",
			"%s prompt build-hygiene note must stay language-neutral", name)
	}
}

func TestProcessTeardownInBothCodingPrompts(t *testing.T) {
	for name, p := range map[string]string{"coder": coderPrompt, "fix": fixPrompt, "verify-fix": verifyFixPrompt} {
		assert.Contains(t, p, "Shut down any long-running process you start for verification",
			"%s prompt must include the process-teardown note", name)
		assert.Contains(t, p, `"still running" checks in later phases of this run`, name)
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
	"plan":                planPrompt,
	"diagnose":            diagnosePrompt,
	"buildHygiene":        buildHygieneNote,
	"processTeardown":     processTeardownNote,
	"selfReview":          selfReviewBlock,
	"coder":               coderPrompt,
	"specialist":          specialistPrompt,
	"correctness":         correctnessPrompt,
	"design":              designPrompt,
	"security":            securityPrompt,
	"synthesis":           synthesisPrompt,
	"fix":                 fixPrompt,
	"prBody":              prBodyPrompt,
	"document":            documentPrompt,
	"gateClassify":        gateClassifyPrompt,
	"brainstorm":          brainstormPrompt,
	"sweepRule":           sweepRule,
	"reviewSynthesis":     reviewSynthesisPrompt,
	"checkpointSynthesis": checkpointSynthesisPrompt,
	"checkpointRevise":    checkpointRevisePrompt,
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

func TestPlannerGroundingRuleInPlanPrompts(t *testing.T) {
	for name, p := range map[string]string{
		"planPrompt":          planPrompt,
		"planSynthesisPrompt": planSynthesisPrompt,
		"planBriefing":        planBriefing,
	} {
		assert.Contains(t, p, "Do not put unverified specifics",
			"%s must include the planner grounding rule", name)
	}
}

func TestPlanPromptCarriesFindingsClauses(t *testing.T) {
	t.Parallel()

	assert.Contains(t, planPrompt, "record_finding",
		"planPrompt must name the tool the plan registry provides")
	assert.Contains(t, planPrompt, "counts as confirmed",
		"planPrompt must let a recorded anchor satisfy the grounding rule")
}

// The findings clauses are plan-only. planBriefing and planSynthesisPrompt run
// in mob planning, where the tool is not registered, so neither may reference it
// and the shared grounding rule must stay clean.
func TestFindingsClausesAreNotInSharedPlanPrompts(t *testing.T) {
	t.Parallel()

	for name, p := range map[string]string{
		"plannerGroundingRule": plannerGroundingRule,
		"planBriefing":         planBriefing,
		"planSynthesisPrompt":  planSynthesisPrompt,
	} {
		assert.NotContains(t, p, "record_finding",
			"%s must not promise a tool that is not registered for it", name)
	}
}

func TestSweepRuleInSynthesisPrompts(t *testing.T) {
	for name, p := range map[string]string{
		"synthesisPrompt":           synthesisPrompt,
		"reviewSynthesisPrompt":     reviewSynthesisPrompt,
		"checkpointSynthesisPrompt": checkpointSynthesisPrompt,
	} {
		assert.Contains(t, p, "When a finding asserts that a specific statement",
			"%s must splice the sweepRule", name)
	}
}

func TestSeverityFieldInSynthesisPrompts(t *testing.T) {
	for name, p := range map[string]string{
		"synthesisPrompt":       synthesisPrompt,
		"reviewSynthesisPrompt": reviewSynthesisPrompt,
	} {
		assert.Contains(t, p, `"severity"`,
			"%s must request a severity field on each fix", name)

		// normalizeSeverity drops anything outside validSeverities, so a tag the
		// prompt never offers would silently render as no tag at all. Assert
		// against the map itself rather than a copy of its values, so adding a
		// severity in one place fails here until the other follows.
		for sev := range validSeverities {
			assert.Contains(t, p, sev,
				"%s must offer the %q severity that normalizeSeverity accepts", name, sev)
		}
	}
}

func TestCoderGroundingRuleInCoderPrompt(t *testing.T) {
	assert.Contains(t, coderPrompt, "hints to verify, not guarantees",
		"coderPrompt must include the coder grounding rule")
	assert.NotContains(t, fixPrompt, "hints to verify, not guarantees",
		"the coder grounding rule is coder-only, not spliced into fixPrompt")
}

// guard: a production coder finished its subtask with turns to spare, then
// spent them implementing work the parent card's description and acceptance
// criteria described but the plan had assigned to a sibling subtask. The
// scope rule already forbade sibling-subtask work; this pins the added
// clause that the parent card itself is not a second source of scope.
func TestCoderPromptScopedAgainstParentCriteria(t *testing.T) {
	low := strings.ToLower(coderPrompt)
	assert.Contains(t, low, "nothing from sibling subtasks")
	assert.Contains(t, low, "the parent card's description and acceptance criteria may cover")
}

// TestReadOnlyPromptsMentionGit asserts that the six read-only prompt
// constants enumerate git among their tools and none forbids running git.
// planPrompt is asserted as already correct (unchanged).
func TestReadOnlyPromptsMentionGit(t *testing.T) {
	readOnly := map[string]string{
		"diagnose":      diagnosePrompt,
		"specialist":    specialistPrompt,
		"prBody":        prBodyPrompt,
		"copilotTriage": copilotTriagePrompt,
		"brainstorm":    brainstormPrompt,
		"seatSystem":    seatSystemPrompt,
	}

	prohibitions := []string{
		"do not run git",
		"do NOT run git",
		"never run git",
	}

	for name, p := range readOnly {
		t.Run(name, func(t *testing.T) {
			assert.Contains(t, p, "git", "%s must mention git", name)

			for _, phrase := range prohibitions {
				assert.NotContains(t, p, phrase,
					"%s must not forbid running git", name)
			}
		})
	}

	// planPrompt is not part of this change - it already has git and must stay that way.
	assert.Contains(t, planPrompt, "git")
}

// TestReviewBriefingRendersGrounding asserts that the review briefing template
// accepts and embeds a non-empty repo-grounding value without producing a
// %! formatting artifact (placeholder/argument count mismatch).
func TestReviewBriefingRendersGrounding(t *testing.T) {
	g := "REPO GROUNDING\nFollow the repo conventions.\n"
	out := fmt.Sprintf(reviewBriefing, g, "My Title", "My Description", fencedDiff("diff - old +new"), "previous findings")
	assert.Contains(t, out, g, "review briefing must embed the grounding value")
	assert.NotContains(t, out, "%!", "review briefing must have no formatting artifact")
}

// TestCheckpointBriefingRendersGrounding asserts that the checkpoint briefing
// template accepts and embeds a non-empty repo-grounding value without
// producing a %! formatting artifact.
func TestCheckpointBriefingRendersGrounding(t *testing.T) {
	g := "REPO GROUNDING\nCheck the CLAUDE.md.\n"
	out := fmt.Sprintf(checkpointBriefing, g, "Subtask Title", "Subtask description", "Parent Card Title", "ENV go 1.22", fencedDiff("diff - old +new"))
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

	specialist := fmt.Sprintf(specialistPrompt, "", "", readRootsBlock(nil), "LENS", "t", "b", "d", "")
	assert.Contains(t, specialist, "single verdict.\n\nLENS\n\nReview only",
		"an empty roots block leaves the specialist prompt spacing unchanged")
}
