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

func TestLoadCapabilitiesMetaFormat(t *testing.T) {
	in := `{"meta":{"date":"2026-06-12","samples":3,"task_library_hash":"abc",
	        "routing":"default","harness_version":"dev"},
	       "models":{"deepseek/deepseek-v4-flash":{"coder":0.79,"reviewer":0.74}}}`
	caps, meta, err := LoadCapabilitiesWithMeta(strings.NewReader(in))
	require.NoError(t, err)
	assert.Equal(t, "abc", meta.TaskLibraryHash)
	assert.InDelta(t, 0.79, caps["deepseek/deepseek-v4-flash"][RoleCoder], 1e-9)
}

func TestLoadCapabilitiesLegacyFlatMap(t *testing.T) {
	// Legacy flat format (no "meta" key) must still load via LoadCapabilities.
	in := `{"deepseek/deepseek-v4-flash":{"coder":0.85,"reviewer":0.70}}`
	caps, err := LoadCapabilities(strings.NewReader(in))
	require.NoError(t, err)
	assert.InDelta(t, 0.85, caps["deepseek/deepseek-v4-flash"][RoleCoder], 1e-9)
}

func TestLoadCapabilitiesLegacyModelNamedMeta(t *testing.T) {
	// A legacy flat file can contain a model slug literally named "meta"; it
	// must load via the legacy path, not be misread as a meta envelope.
	in := `{"meta":{"coder":0.61,"reviewer":0.52},
	       "deepseek/deepseek-v4-flash":{"coder":0.85}}`
	caps, err := LoadCapabilities(strings.NewReader(in))
	require.NoError(t, err)
	assert.InDelta(t, 0.61, caps["meta"][RoleCoder], 1e-9)
	assert.InDelta(t, 0.85, caps["deepseek/deepseek-v4-flash"][RoleCoder], 1e-9)
}

func TestEmbeddedDataParses(t *testing.T) {
	p := DefaultPriors()
	assert.NotEmpty(t, p.Meta.TierBars)

	caps, meta := DefaultCapabilities()
	assert.NotEmpty(t, caps)
	assert.NotEmpty(t, meta.Date)
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
