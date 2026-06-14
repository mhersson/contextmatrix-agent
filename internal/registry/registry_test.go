package registry

import (
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCatalog() llm.Catalog {
	return llm.Catalog{
		{ID: "deepseek/deepseek-v4-flash", ContextLength: 131072, PromptPricePerTok: 0.0000005, CompletionPricePerTok: 0.0000015, SupportedParameters: []string{"tools"}},
		{ID: "cheap/small", ContextLength: 8192, PromptPricePerTok: 0.0000001, CompletionPricePerTok: 0.0000001, SupportedParameters: []string{"tools"}},
		{ID: "no/tools", ContextLength: 4096, SupportedParameters: []string{}},
	}
}

func TestResolvePinBeatsDefault(t *testing.T) {
	r := NewRegistry(map[Role]string{RoleCoder: "deepseek/deepseek-v4-flash"}, "capable/default", testCatalog())

	spec, err := r.Resolve("ignored-actor", RoleCoder)
	require.NoError(t, err)
	assert.Equal(t, "deepseek/deepseek-v4-flash", spec.Model)
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
	assert.True(t, r.fitsWindow("deepseek/deepseek-v4-flash", 100000))
	assert.False(t, r.fitsWindow("cheap/small", 100000))
	assert.True(t, r.fitsWindow("unknown/model", 100000)) // fail-open
}

func TestSelectByComplexityPrefersCheapestCapableToolModel(t *testing.T) {
	r := NewRegistry(nil, "deepseek/deepseek-v4-flash", testCatalog())
	spec := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
	assert.Equal(t, "deepseek/deepseek-v4-flash", spec.Model) // only seeded model clears the complex bar
}

func TestSelectByComplexityFallsBackToCapable(t *testing.T) {
	// All tool-capable models are unseeded (no measured score). Unmeasured cells
	// are never selectable, so selection falls back to the capable default.
	cat := llm.Catalog{
		{ID: "unseeded/a", ContextLength: 8192, SupportedParameters: []string{"tools"}},
		{ID: "unseeded/b", ContextLength: 8192, SupportedParameters: []string{"tools"}},
	}
	r := NewRegistry(nil, "capable/default", cat)
	spec := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierSimple})
	assert.Equal(t, "capable/default", spec.Model)
}

// ptr returns a pointer to v; used to build *float64 prior literals.
func ptr(v float64) *float64 { return &v }

// entry builds a CatalogEntry from prices given in dollars per million tokens,
// converting to the per-token units the catalog stores.
func entry(id string, promptPerM, completionPerM float64, window int) llm.CatalogEntry {
	return llm.CatalogEntry{
		ID:                    id,
		PromptPricePerTok:     promptPerM / 1e6,
		CompletionPricePerTok: completionPerM / 1e6,
		ContextLength:         window,
		SupportedParameters:   []string{"tools"},
	}
}

func TestSelectByComplexityTwoSignal(t *testing.T) {
	// Blended price ($/Mtok) per model: cheap-weak 1.5, cheap-good 2.1,
	// mid-better 2.7, frontier 18.0, unmeasured-star 1.2, small-window 1.8.
	catalog := llm.Catalog{
		entry("cheap-weak", 0.5, 1.0, 200000),
		entry("cheap-good", 0.7, 1.4, 200000),
		entry("mid-better", 0.9, 1.8, 200000),
		entry("frontier", 6.0, 12.0, 200000),
		entry("unmeasured-star", 0.4, 0.8, 200000),
		entry("small-window", 0.6, 1.2, 8000),
	}
	caps := map[string]map[Role]float64{ // functional gate, floor 0.6
		"cheap-weak": {RoleCoder: 0.65}, "cheap-good": {RoleCoder: 0.70},
		"mid-better": {RoleCoder: 0.72}, "frontier": {RoleCoder: 0.75},
		"small-window": {RoleCoder: 0.70},
		// unmeasured-star deliberately absent (unknown != zero -> never selectable)
	}
	priors := Priors{
		Meta: PriorsMeta{TierBars: map[string]float64{"simple": 0.3, "moderate": 0.55, "complex": 0.75}},
		Models: map[string]PriorEntry{
			"cheap-weak": {Coder: ptr(0.40)}, "cheap-good": {Coder: ptr(0.60)},
			"mid-better": {Coder: ptr(0.80)}, "frontier": {Coder: ptr(0.95)},
			"unmeasured-star": {Coder: ptr(0.99)}, "small-window": {Coder: ptr(0.70)},
		},
	}
	r := NewRegistryWithCapabilities(nil, "capable-default", catalog, caps).
		WithSelection(Selection{Priors: priors, Floor: 0.6, PriceHeadroom: 1.5})

	tests := []struct {
		name string
		in   SelectInput
		want string
	}{
		// moderate (bar 0.55): gate>=0.6 + prior>=0.55 + window. Candidates:
		// cheap-good(q0.60,$2.1), mid-better(q0.80,$2.7), frontier(q0.95,$18).
		// cheap-weak prior 0.40<0.55 out; small-window window 8k<50k out;
		// unmeasured-star has no measured score, out. Cheapest $2.1 -> band
		// 2.1*1.5=3.15: cheap-good($2.1), mid-better($2.7) in; frontier out.
		// Highest quality in band: mid-better (0.80). -> mid-better.
		{"best value beats cheapest", SelectInput{Role: RoleCoder, Tier: TierModerate, EstTokens: 50000}, "mid-better"},
		// complex (bar 0.75): only mid-better (prior 0.80) and frontier (0.95)
		// clear the bar. Cheapest $2.7 -> band 4.05; frontier $18 out. -> mid-better.
		{"headroom excludes frontier", SelectInput{Role: RoleCoder, Tier: TierComplex, EstTokens: 50000}, "mid-better"},
		// simple (bar 0.3): cheap-weak(q0.40,$1.5), cheap-good(q0.60,$2.1),
		// mid-better(q0.80,$2.7), frontier(q0.95,$18) clear bar+gate+window.
		// unmeasured-star never selectable (no measured score), despite prior 0.99.
		// small-window window 8k<50k out. Cheapest $1.5 -> band 1.5*1.5=2.25:
		// cheap-weak($1.5), cheap-good($2.1) in; mid-better $2.7>2.25 out.
		// Highest quality in band: cheap-good (0.60). -> cheap-good.
		// (Plan comment said "mid-better" but mid-better $2.7 is outside the
		// 2.25 band; corrected to cheap-good per the authoritative spec rules.)
		{"unmeasured excluded", SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000}, "cheap-good"},
		// simple, est 50k, four cheaper models excluded; small-window fails the
		// window check; unmeasured-star never selectable -> no candidates -> default.
		{"window fit enforced", SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000, Exclude: map[string]bool{"mid-better": true, "cheap-good": true, "cheap-weak": true, "frontier": true}}, "capable-default"},
		// moderate, mid-better excluded: candidates cheap-good($2.1), frontier($18).
		// Cheapest $2.1 -> band 3.15; frontier out. -> cheap-good.
		{"exclusion respected", SelectInput{Role: RoleCoder, Tier: TierModerate, EstTokens: 50000, Exclude: map[string]bool{"mid-better": true}}, "cheap-good"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, r.SelectByComplexity(tt.in).Model)
		})
	}
}

func TestSelectFallsBackToMeasuredWithoutPriors(t *testing.T) {
	// No prior entries: the tier bar applies to MEASURED capability, and the
	// best-value rule runs over measured scores.
	catalog := llm.Catalog{
		entry("cheap-good", 0.7, 1.4, 200000), // $2.1
		entry("mid-better", 0.9, 1.8, 200000), // $2.7
		entry("frontier", 6.0, 12.0, 200000),  // $18
		entry("cheap-weak", 0.5, 1.0, 200000), // $1.5
	}
	caps := map[string]map[Role]float64{
		"cheap-weak": {RoleCoder: 0.40}, // < floor 0.6, gated out
		"cheap-good": {RoleCoder: 0.60},
		"mid-better": {RoleCoder: 0.80},
		"frontier":   {RoleCoder: 0.95},
	}
	// Empty Priors.Models -> bar applies to measured. TierBars still supplied.
	priors := Priors{Meta: PriorsMeta{TierBars: map[string]float64{"moderate": 0.55}}}
	r := NewRegistryWithCapabilities(nil, "capable-default", catalog, caps).
		WithSelection(Selection{Priors: priors, Floor: 0.6, PriceHeadroom: 1.5})

	// moderate (bar 0.55): cheap-weak measured 0.40 fails floor AND bar.
	// Candidates: cheap-good(q0.60,$2.1), mid-better(q0.80,$2.7), frontier(q0.95,$18).
	// Cheapest $2.1 -> band 3.15: cheap-good, mid-better in; frontier out.
	// Highest quality in band: mid-better. -> mid-better.
	spec := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierModerate, EstTokens: 50000})
	assert.Equal(t, "mid-better", spec.Model)
}

func TestSelectReviewPanel(t *testing.T) {
	// Four qualifying reviewers; one will be the coder's pick (excluded).
	// Blended: alpha $2.1, beta $2.7, gamma $3.0, delta $18.
	catalog := llm.Catalog{
		entry("alpha", 0.7, 1.4, 200000),
		entry("beta", 0.9, 1.8, 200000),
		entry("gamma", 1.0, 2.0, 200000),
		entry("delta", 6.0, 12.0, 200000),
	}
	caps := map[string]map[Role]float64{
		"alpha": {RoleReviewer: 0.70}, "beta": {RoleReviewer: 0.72},
		"gamma": {RoleReviewer: 0.71}, "delta": {RoleReviewer: 0.75},
	}
	priors := Priors{
		Meta: PriorsMeta{TierBars: map[string]float64{"moderate": 0.55}},
		Models: map[string]PriorEntry{
			"alpha": {Reviewer: ptr(0.60)}, "beta": {Reviewer: ptr(0.80)},
			"gamma": {Reviewer: ptr(0.70)}, "delta": {Reviewer: ptr(0.95)},
		},
	}
	r := NewRegistryWithCapabilities(nil, "capable-default", catalog, caps).
		WithSelection(Selection{Priors: priors, Floor: 0.6, PriceHeadroom: 1.5})

	// moderate (bar 0.55): all four clear gate+bar. Exclude alpha (coder's pick).
	// Remaining candidates: beta(q0.80,$2.7), gamma(q0.70,$3.0), delta(q0.95,$18).
	// Pick 1: cheapest $2.7 -> band 4.05: beta, gamma in; delta out. Top: beta.
	// Pick 2 (exclude beta): gamma(q0.70,$3.0), delta($18). Cheapest $3.0 ->
	//   band 4.5: gamma only. -> gamma.
	// Pick 3 (exclude beta,gamma): delta only ($18). -> delta.
	in := SelectInput{Role: RoleReviewer, Tier: TierModerate, EstTokens: 50000, Exclude: map[string]bool{"alpha": true}}
	panel := r.SelectReviewPanel(in, 3)
	require.Len(t, panel, 3)
	assert.Equal(t, "beta", panel[0].Model)
	assert.Equal(t, "gamma", panel[1].Model)
	assert.Equal(t, "delta", panel[2].Model)

	// Only two qualifying models -> reuse the last pick to fill 3 slots rather
	// than escalating price. Restrict pool: gamma/delta gated out by floor.
	caps2 := map[string]map[Role]float64{
		"alpha": {RoleReviewer: 0.70}, "beta": {RoleReviewer: 0.72},
		"gamma": {RoleReviewer: 0.40}, "delta": {RoleReviewer: 0.40},
	}
	r2 := NewRegistryWithCapabilities(nil, "capable-default", catalog, caps2).
		WithSelection(Selection{Priors: priors, Floor: 0.6, PriceHeadroom: 1.5})
	in2 := SelectInput{Role: RoleReviewer, Tier: TierModerate, EstTokens: 50000}
	// Candidates: alpha(q0.60,$2.1), beta(q0.80,$2.7). Pick1 cheapest $2.1 ->
	// band 3.15: both in; top alpha? No -> beta(0.80) higher. Pick1=beta.
	// Pick2 (exclude beta): alpha only. Pick2=alpha.
	// Pick3 (exclude beta,alpha): pool dry -> reuse last pick alpha.
	panel2 := r2.SelectReviewPanel(in2, 3)
	require.Len(t, panel2, 3)
	assert.Equal(t, "beta", panel2[0].Model)
	assert.Equal(t, "alpha", panel2[1].Model)
	assert.Equal(t, "alpha", panel2[2].Model) // reuse, no price escalation
}

func TestRegistryContextWindow(t *testing.T) {
	r := NewRegistry(nil, "x", testCatalog())

	assert.Equal(t, 131072, r.ContextWindow("deepseek/deepseek-v4-flash"))
	assert.Equal(t, 8192, r.ContextWindow("cheap/small"))
	assert.Equal(t, 0, r.ContextWindow("unknown/model"))
}

func TestSelectReviewPanelDryFromStart(t *testing.T) {
	// Zero qualifying candidates (every model gated out by the floor): the panel
	// must still be n non-empty specs — all the capable default, never ModelSpec{}.
	catalog := llm.Catalog{
		entry("alpha", 0.7, 1.4, 200000),
		entry("beta", 0.9, 1.8, 200000),
	}
	caps := map[string]map[Role]float64{
		"alpha": {RoleReviewer: 0.10}, "beta": {RoleReviewer: 0.20}, // both < floor
	}
	priors := Priors{Meta: PriorsMeta{TierBars: map[string]float64{"moderate": 0.55}}}
	r := NewRegistryWithCapabilities(nil, "capable-default", catalog, caps).
		WithSelection(Selection{Priors: priors, Floor: 0.6, PriceHeadroom: 1.5})

	panel := r.SelectReviewPanel(SelectInput{Role: RoleReviewer, Tier: TierModerate, EstTokens: 50000}, 3)
	require.Len(t, panel, 3)

	for i, spec := range panel {
		assert.Equal(t, "capable-default", spec.Model, "slot %d", i)
	}
}
