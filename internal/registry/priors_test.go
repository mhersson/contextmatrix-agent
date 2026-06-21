package registry

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadPriors(t *testing.T) {
	in := `{"meta":{"updated":"2026-06-12","procedure":"docs/model-priors.md",
	        "tier_bars":{"simple":0.30,"moderate":0.55,"complex":0.75}},
	       "models":{"openai/gpt-5.5":{"coder":0.92,"reviewer":0.90,
	        "source":"artificialanalysis","retrieved":"2026-06-12"}}}`
	p, err := LoadPriors(strings.NewReader(in))
	require.NoError(t, err)
	assert.InDelta(t, 0.55, p.Meta.TierBars["moderate"], 1e-9)
	require.NotNil(t, p.Models["openai/gpt-5.5"].Coder)
	assert.InDelta(t, 0.92, *p.Models["openai/gpt-5.5"].Coder, 1e-9)
}

func TestPriorsForRole(t *testing.T) {
	in := `{"models":{"openai/gpt-5.5":{"coder":0.92,"reviewer":0.90}}}`
	p, err := LoadPriors(strings.NewReader(in))
	require.NoError(t, err)

	score, ok := p.ForRole("openai/gpt-5.5", RoleCoder)
	assert.True(t, ok)
	assert.InDelta(t, 0.92, score, 1e-9)

	score, ok = p.ForRole("openai/gpt-5.5", RoleReviewer)
	assert.True(t, ok)
	assert.InDelta(t, 0.90, score, 1e-9)

	_, ok = p.ForRole("unknown/model", RoleCoder)
	assert.False(t, ok)
}

func TestPriorsForRoleOmittedVsExplicitZero(t *testing.T) {
	// A model entry that sets only coder has NO reviewer prior (ok == false) —
	// the selector must fall back to measured scores, not treat it as a prior
	// of zero. An explicit 0 is a deliberate curator statement: (0, true).
	in := `{"models":{
	        "only/coder":    {"coder":0.88},
	        "zero/reviewer": {"coder":0.70,"reviewer":0}}}`
	p, err := LoadPriors(strings.NewReader(in))
	require.NoError(t, err)

	score, ok := p.ForRole("only/coder", RoleCoder)
	assert.True(t, ok)
	assert.InDelta(t, 0.88, score, 1e-9)

	_, ok = p.ForRole("only/coder", RoleReviewer)
	assert.False(t, ok, "omitted role must report no prior, not zero")

	score, ok = p.ForRole("zero/reviewer", RoleReviewer)
	assert.True(t, ok, "explicit zero is a real prior")
	assert.InDelta(t, 0.0, score, 1e-9)
}
