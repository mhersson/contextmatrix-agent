package orchestrator

import (
	"fmt"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
	"github.com/stretchr/testify/assert"
)

func TestGroundingInjectedIntoCoderPrompt(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "CLAUDE.md", "# PROJECT RULE: always run gofumpt")

	o := &run{
		grounding: groundingBlock(discoverGrounding(root)),
		tc:        cmclient.TaskContext{Title: "t", Description: "d"},
	}

	// coderPrompt is assembled in executeSubtask; assert the assembled
	// prompt carries the grounding rule.
	prompt := fmt.Sprintf(coderPrompt, o.skillEngage(), o.grounding, root,
		"sub title", "sub body", o.tc.Title, o.tc.Description)

	assert.Contains(t, prompt, "PROJECT RULE: always run gofumpt")
	assert.Contains(t, prompt, "REPO GROUNDING")
}
