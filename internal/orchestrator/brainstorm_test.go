package orchestrator

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHasDesignSection(t *testing.T) {
	assert.True(t, hasDesignSection("intro\n\n## Design\n\nstuff"))
	assert.False(t, hasDesignSection("intro\n\n## Plan\n\nstuff"))
	assert.False(t, hasDesignSection(""))
}

func TestExtractDesign(t *testing.T) {
	t.Run("no marker", func(t *testing.T) {
		_, done := extractDesign("Let's discuss. What palette sizes do you need?")
		assert.False(t, done)
	})

	t.Run("marker with design heading", func(t *testing.T) {
		out := "Great.\n\n## Design\n\nA palette config with N slots.\n\nDESIGN_COMPLETE"
		design, done := extractDesign(out)
		assert.True(t, done)
		assert.True(t, hasDesignSection(design), "the captured text is the design section")
		assert.NotContains(t, design, "DESIGN_COMPLETE", "the marker line is stripped")
		assert.Contains(t, design, "A palette config")
	})

	t.Run("marker without heading keeps the body", func(t *testing.T) {
		design, done := extractDesign("Final design: just add a flag.\nDESIGN_COMPLETE")
		assert.True(t, done)
		assert.Contains(t, design, "just add a flag")
		assert.NotContains(t, design, "DESIGN_COMPLETE")
	})
}
