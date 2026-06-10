package eval

import (
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/stretchr/testify/assert"
)

func TestDefaultCandidates(t *testing.T) {
	c := DefaultCandidates()
	assert.Contains(t, c, "anthropic/claude-sonnet-4.6")

	for _, line := range c {
		assert.NotContains(t, line, "#")
	}
}

func TestFreeToolModels(t *testing.T) {
	cat := llm.Catalog{
		{ID: "a/b:free", ContextLength: 32768, SupportedParameters: []string{"tools"}},
		{ID: "a/b", ContextLength: 32768, SupportedParameters: []string{"tools"}},     // paid
		{ID: "c/d:free", ContextLength: 4096, SupportedParameters: []string{"tools"}}, // too small
		{ID: "e/f:free", ContextLength: 32768, SupportedParameters: []string{}},       // no tools
	}
	got := FreeToolModels(cat, 16384)
	assert.Equal(t, []string{"a/b:free"}, got)
}

func TestEstimateCost(t *testing.T) {
	cat := llm.Catalog{{ID: "m", PromptPricePerTok: 1e-6, CompletionPricePerTok: 2e-6}}
	// 1 model × 2 tasks × 3 samples × (1000*1e-6 + 500*2e-6) = 6 × 0.002 = 0.012
	got := EstimateCost(cat, []string{"m", "unknown"}, 2, 3, 1000, 500)
	assert.InEpsilon(t, 0.012, got, 1e-9)
}
