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
// restriction, the no-git instruction, and the COMMIT: docs(...) convention.
func TestDocumentPromptShape(t *testing.T) {
	low := strings.ToLower(documentPrompt)
	assert.Contains(t, low, "default: no external documentation is needed")
	assert.Contains(t, low, "documentation only")
	assert.Contains(t, low, "user-facing behavior")
	assert.Contains(t, low, "api contracts")
	assert.Contains(t, low, "do not run git")
	assert.Contains(t, low, "commit: docs(")
}
