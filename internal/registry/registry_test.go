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
	// NewRegistry injects no priors, so no catalog model carries a prior for the
	// role: selection always falls back to the capable default.
	r := NewRegistry(nil, "deepseek/deepseek-v4-flash", testCatalog())
	spec := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
	assert.Equal(t, "deepseek/deepseek-v4-flash", spec.Model)
}

func TestSelectByComplexityFallsBackToCapable(t *testing.T) {
	// All tool-capable models lack a prior (NewRegistry injects none). A model
	// with no prior for the role is never selectable, so selection falls back to
	// the capable default.
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

func TestSelectByComplexityPriorsOnly(t *testing.T) {
	// Blended price ($/Mtok) per model: cheap-weak 1.5, cheap-good 2.1,
	// mid-better 2.7, frontier 18.0, star 1.2, small-window 1.8. Quality is the
	// normalized prior only; there is no measured-capability gate. Bars come from
	// DefaultTierBars (simple 0.65, moderate 0.76, complex 0.82); headroom 1.5.
	catalog := llm.Catalog{
		entry("cheap-weak", 0.5, 1.0, 200000),
		entry("cheap-good", 0.7, 1.4, 200000),
		entry("mid-better", 0.9, 1.8, 200000),
		entry("frontier", 6.0, 12.0, 200000),
		entry("star", 0.4, 0.8, 200000),
		entry("small-window", 0.6, 1.2, 8000),
	}
	priors := Priors{Models: map[string]PriorEntry{
		"cheap-weak": {Coder: ptr(0.50)}, "cheap-good": {Coder: ptr(0.70)},
		"mid-better": {Coder: ptr(0.85)}, "frontier": {Coder: ptr(0.95)},
		"star": {Coder: ptr(0.99)}, "small-window": {Coder: ptr(0.85)},
	}}
	r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default")

	tests := []struct {
		name string
		in   SelectInput
		want string
	}{
		// simple (bar 0.65): prior>=0.65 + window. Candidates cheap-good(q0.70,$2.1),
		// mid-better(q0.85,$2.7), frontier(q0.95,$18), star(q0.99,$1.2). cheap-weak
		// prior 0.50<0.65 out; small-window window 8k<50k out. Cheapest star $1.2 ->
		// band 1.2*1.5=1.8: only star in band. -> star.
		{"cheapest in-band wins", SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000}, "star"},
		// simple, star excluded: cheap-good($2.1), mid-better($2.7), frontier($18).
		// Cheapest $2.1 -> band 3.15: cheap-good, mid-better in; frontier out.
		// Highest quality in band: mid-better (0.85). -> mid-better.
		{"best value beats cheapest", SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000, Exclude: map[string]bool{"star": true}}, "mid-better"},
		// moderate (bar 0.76), star excluded: mid-better(q0.85,$2.7), frontier(q0.95,$18).
		// cheap-good prior 0.70<0.76 out. Cheapest $2.7 -> band 4.05; frontier $18 out.
		// -> mid-better.
		{"headroom excludes frontier", SelectInput{Role: RoleCoder, Tier: TierModerate, EstTokens: 50000, Exclude: map[string]bool{"star": true}}, "mid-better"},
		// complex (bar 0.82): prior>=0.82: mid-better(q0.85,$2.7), frontier(q0.95,$18),
		// star(q0.99,$1.2). Cheapest star $1.2 -> band 1.8: only star. -> star.
		{"complex bar still cost-optimal", SelectInput{Role: RoleCoder, Tier: TierComplex, EstTokens: 50000}, "star"},
		// simple, est 50k, every wide-window model excluded; small-window fails the
		// window check -> no candidates -> capable default.
		{"window fit enforced", SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000, Exclude: map[string]bool{"star": true, "mid-better": true, "cheap-good": true, "cheap-weak": true, "frontier": true}}, "capable-default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, r.SelectByComplexity(tt.in).Model)
		})
	}
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
	// Bars come from DefaultTierBars (moderate 0.76); headroom 1.5.
	priors := Priors{Models: map[string]PriorEntry{
		"alpha": {Reviewer: ptr(0.80)}, "beta": {Reviewer: ptr(0.85)},
		"gamma": {Reviewer: ptr(0.82)}, "delta": {Reviewer: ptr(0.95)},
	}}
	r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default")

	// moderate (bar 0.76): all four clear the bar. Exclude alpha (coder's pick).
	// Remaining candidates: beta(q0.85,$2.7), gamma(q0.82,$3.0), delta(q0.95,$18).
	// Pick 1: cheapest $2.7 -> band 4.05: beta, gamma in; delta out. Top: beta.
	// Pick 2 (exclude beta): gamma(q0.82,$3.0), delta(q0.95,$18). Cheapest $3.0 ->
	//   band 4.5: gamma only. -> gamma.
	// Pick 3 (exclude beta,gamma): delta only ($18). -> delta.
	in := SelectInput{Role: RoleReviewer, Tier: TierModerate, EstTokens: 50000, Exclude: map[string]bool{"alpha": true}}
	panel := r.SelectReviewPanel(in, 3)
	require.Len(t, panel, 3)
	assert.Equal(t, "beta", panel[0].Model)
	assert.Equal(t, "gamma", panel[1].Model)
	assert.Equal(t, "delta", panel[2].Model)

	// Only two qualifying models -> reuse the last pick to fill 3 slots rather
	// than escalating price. Restrict the pool via priors: gamma/delta sit below
	// the moderate bar (0.76) so they are never candidates.
	priors2 := Priors{Models: map[string]PriorEntry{
		"alpha": {Reviewer: ptr(0.80)}, "beta": {Reviewer: ptr(0.85)},
		"gamma": {Reviewer: ptr(0.50)}, "delta": {Reviewer: ptr(0.50)},
	}}
	r2 := NewRegistryFromParts(catalog, priors2, nil, nil, "capable-default")
	in2 := SelectInput{Role: RoleReviewer, Tier: TierModerate, EstTokens: 50000}
	// Candidates: alpha(q0.80,$2.1), beta(q0.85,$2.7). Pick1 cheapest $2.1 ->
	// band 3.15: both in; top quality beta(0.85). Pick1=beta.
	// Pick2 (exclude beta): alpha only. Pick2=alpha.
	// Pick3 (exclude beta,alpha): pool dry -> reuse last pick alpha.
	panel2 := r2.SelectReviewPanel(in2, 3)
	require.Len(t, panel2, 3)
	assert.Equal(t, "beta", panel2[0].Model)
	assert.Equal(t, "alpha", panel2[1].Model)
	assert.Equal(t, "alpha", panel2[2].Model) // reuse, no price escalation
}

func TestTierBarsIncludeCritical(t *testing.T) {
	r := &Registry{sel: Selection{TierBars: DefaultTierBars()}}
	if got := r.tierBar(TierCritical); got != 0.90 {
		t.Errorf("critical bar = %v, want 0.90", got)
	}

	if got := r.tierBar(TierSimple); got != 0.65 {
		t.Errorf("simple bar = %v, want 0.65", got)
	}
}

func TestRegistryContextWindow(t *testing.T) {
	r := NewRegistry(nil, "x", testCatalog())

	assert.Equal(t, 131072, r.ContextWindow("deepseek/deepseek-v4-flash"))
	assert.Equal(t, 8192, r.ContextWindow("cheap/small"))
	assert.Equal(t, 0, r.ContextWindow("unknown/model"))
}

func TestSelectReviewPanelDryFromStart(t *testing.T) {
	// Zero qualifying candidates (no model carries a prior for the role): the
	// panel must still be n non-empty specs — all the capable default, never
	// ModelSpec{}.
	catalog := llm.Catalog{
		entry("alpha", 0.7, 1.4, 200000),
		entry("beta", 0.9, 1.8, 200000),
	}
	r := NewRegistryFromParts(catalog, Priors{}, nil, nil, "capable-default")

	panel := r.SelectReviewPanel(SelectInput{Role: RoleReviewer, Tier: TierModerate, EstTokens: 50000}, 3)
	require.Len(t, panel, 3)

	for i, spec := range panel {
		assert.Equal(t, "capable-default", spec.Model, "slot %d", i)
	}
}

func TestCandidatesArePriorsOnlyAndSkipBlacklist(t *testing.T) {
	cat := llm.Catalog{
		{ID: "cheap/ok", PromptPricePerTok: 1e-7, CompletionPricePerTok: 2e-7, ContextLength: 200000, SupportedParameters: []string{"tools"}},
		{ID: "black/listed", PromptPricePerTok: 1e-8, CompletionPricePerTok: 1e-8, ContextLength: 200000, SupportedParameters: []string{"tools"}},
	}
	pr := Priors{Models: map[string]PriorEntry{
		"cheap/ok":     {Coder: ptr(0.80)},
		"black/listed": {Coder: ptr(0.95)},
	}}
	r := NewRegistryFromParts(cat, pr, map[string]bool{"black/listed": true}, nil, "capable/default")
	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex}) // bar 0.82
	// cheap/ok (0.80) is below 0.82; blacklisted is excluded; nothing qualifies -> capable default
	if got.Model != "capable/default" {
		t.Errorf("want capable default when nothing clears bar, got %q", got.Model)
	}

	got = r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierModerate}) // bar 0.76
	if got.Model != "cheap/ok" {
		t.Errorf("want cheap/ok at moderate, got %q (blacklisted must never win)", got.Model)
	}
}

func TestFavoritesConsideredFirst(t *testing.T) {
	cat := llm.Catalog{
		{ID: "cheap/win", PromptPricePerTok: 1e-8, CompletionPricePerTok: 1e-8, ContextLength: 200000, SupportedParameters: []string{"tools"}},
		{ID: "fav/pick", PromptPricePerTok: 1e-6, CompletionPricePerTok: 1e-6, ContextLength: 200000, SupportedParameters: []string{"tools"}},
	}
	pr := Priors{Models: map[string]PriorEntry{
		"cheap/win": {Coder: ptr(0.90)}, "fav/pick": {Coder: ptr(0.90)},
	}}
	favs := map[favKey][]string{{Tier: TierComplex}: {"fav/pick"}}
	r := NewRegistryFromParts(cat, pr, nil, favs, "capable/default")

	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
	if got.Model != "fav/pick" {
		t.Errorf("favorite must win over cheaper cost-optimal, got %q", got.Model)
	}
}
