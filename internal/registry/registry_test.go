package registry

import (
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCatalog() llm.Catalog {
	return llm.Catalog{
		{ID: "openai/gpt-oss-120b", ContextLength: 131072, PromptPricePerTok: 0.0000005, CompletionPricePerTok: 0.0000015, SupportedParameters: []string{"tools"}},
		{ID: "cheap/small", ContextLength: 8192, PromptPricePerTok: 0.0000001, CompletionPricePerTok: 0.0000001, SupportedParameters: []string{"tools"}},
		{ID: "no/tools", ContextLength: 4096, SupportedParameters: []string{}},
	}
}

func TestResolvePinBeatsDefault(t *testing.T) {
	r := NewRegistry(map[Role]string{RoleCoder: "openai/gpt-oss-120b"}, "capable/default", testCatalog())

	spec, err := r.Resolve("ignored-actor", RoleCoder)
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-oss-120b", spec.Model)
	assert.Equal(t, 131072, spec.ContextWindow)

	spec, err = r.Resolve("", RoleReviewer) // unpinned -> capable default
	require.NoError(t, err)
	assert.Equal(t, "capable/default", spec.Model)
}

func TestResolveErrorsWithoutPinOrDefault(t *testing.T) {
	r := NewRegistry(nil, "", testCatalog())
	_, err := r.Resolve("", RoleCoder)
	require.Error(t, err)
}

func TestFitsWindow(t *testing.T) {
	r := NewRegistry(nil, "x", testCatalog())
	assert.True(t, r.fitsWindow("openai/gpt-oss-120b", 100000))
	assert.False(t, r.fitsWindow("cheap/small", 100000))
	assert.True(t, r.fitsWindow("unknown/model", 100000)) // fail-open
}

func TestSelectByComplexityPrefersCheapestCapableToolModel(t *testing.T) {
	r := NewRegistry(nil, "openai/gpt-oss-120b", testCatalog())
	spec := r.SelectByComplexity(RoleCoder, TierComplex, testCatalog())
	assert.Equal(t, "openai/gpt-oss-120b", spec.Model) // only seeded model clears the complex bar
}

func TestSelectByComplexityFallsBackToCapable(t *testing.T) {
	// All tool-capable models are unseeded (score 0 < any bar). Reading the nil
	// inner capability map returns 0 (no panic), so selection falls back to the
	// capable default.
	cat := llm.Catalog{
		{ID: "unseeded/a", ContextLength: 8192, SupportedParameters: []string{"tools"}},
		{ID: "unseeded/b", ContextLength: 8192, SupportedParameters: []string{"tools"}},
	}
	r := NewRegistry(nil, "capable/default", cat)
	spec := r.SelectByComplexity(RoleCoder, TierSimple, cat)
	assert.Equal(t, "capable/default", spec.Model)
}
