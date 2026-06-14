package orchestrator

import (
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
