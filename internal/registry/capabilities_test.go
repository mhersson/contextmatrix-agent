package registry

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadAndMergeCapabilities(t *testing.T) {
	loaded, err := LoadCapabilities(strings.NewReader(`{"cheap/small":{"coder":0.85}}`))
	require.NoError(t, err)

	merged := MergeCapabilities(seededCapabilities(), loaded)
	assert.InDelta(t, 0.85, merged["cheap/small"][RoleCoder], 1e-9)                // measured added
	assert.InDelta(t, 0.85, merged["deepseek/deepseek-v4-flash"][RoleCoder], 1e-9) // seed preserved
	// base not mutated
	assert.NotContains(t, seededCapabilities(), "cheap/small")
}

func TestSelectByComplexityReflectsPriors(t *testing.T) {
	cat := llm.Catalog{
		{ID: "cheap/small", ContextLength: 8192, PromptPricePerTok: 1e-7, CompletionPricePerTok: 1e-7, SupportedParameters: []string{"tools"}},
		{ID: "pricey/big", ContextLength: 131072, PromptPricePerTok: 1e-5, CompletionPricePerTok: 1e-5, SupportedParameters: []string{"tools"}},
	}
	// Prior: cheap/small clears the complex bar (0.82); pricey/big has no prior
	// so it is never a candidate. Quality is the prior only.
	pr := Priors{Models: map[string]PriorEntry{"cheap/small": {Coder: ptr(0.9)}}}
	r := NewRegistryFromParts(cat, pr, nil, nil, "capable/default")
	spec := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
	assert.Equal(t, "cheap/small", spec.Model) // cheapest model whose prior clears the bar wins
}

func TestRoundTripJSON(t *testing.T) {
	caps := map[string]map[Role]float64{"m": {RoleCoder: 0.7, RoleReviewer: 0.5}}

	var buf bytes.Buffer
	require.NoError(t, writeCaps(&buf, caps)) // helper defined inline below
	got, err := LoadCapabilities(&buf)
	require.NoError(t, err)
	assert.Equal(t, caps, got)
}

func writeCaps(w *bytes.Buffer, caps map[string]map[Role]float64) error {
	return jsonEncode(w, caps)
}

func jsonEncode(w *bytes.Buffer, v any) error {
	return json.NewEncoder(w).Encode(v)
}
