package registry

import (
	"fmt"
	"maps"
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
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

func TestFitsWindow(t *testing.T) {
	r := NewRegistry("x", testCatalog())
	assert.True(t, r.fitsWindow("deepseek/deepseek-v4-flash", 100000))
	assert.False(t, r.fitsWindow("cheap/small", 100000))
	assert.True(t, r.fitsWindow("unknown/model", 100000)) // fail-open
}

func TestSelectByComplexityPrefersCheapestCapableToolModel(t *testing.T) {
	// NewRegistry injects no priors, so no catalog model carries a prior for the
	// role: selection always falls back to the capable default.
	r := NewRegistry("deepseek/deepseek-v4-flash", testCatalog())
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
	r := NewRegistry("capable/default", cat)
	spec := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierSimple})
	assert.Equal(t, "capable/default", spec.Model)
}

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
		"cheap-weak": {Coder: new(0.50)}, "cheap-good": {Coder: new(0.70)},
		"mid-better": {Coder: new(0.85)}, "frontier": {Coder: new(0.95)},
		"star": {Coder: new(0.99)}, "small-window": {Coder: new(0.85)},
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
		"alpha": {Reviewer: new(0.80)}, "beta": {Reviewer: new(0.85)},
		"gamma": {Reviewer: new(0.82)}, "delta": {Reviewer: new(0.95)},
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
		"alpha": {Reviewer: new(0.80)}, "beta": {Reviewer: new(0.85)},
		"gamma": {Reviewer: new(0.50)}, "delta": {Reviewer: new(0.50)},
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
	r := &Registry{}
	if got := r.barFor(TierCritical); got != 0.90 {
		t.Errorf("critical bar = %v, want 0.90", got)
	}

	if got := r.barFor(TierSimple); got != 0.65 {
		t.Errorf("simple bar = %v, want 0.65", got)
	}
}

func TestRegistryContextWindow(t *testing.T) {
	r := NewRegistry("x", testCatalog())

	assert.Equal(t, 131072, r.ContextWindow("deepseek/deepseek-v4-flash"))
	assert.Equal(t, 8192, r.ContextWindow("cheap/small"))
	assert.Equal(t, 0, r.ContextWindow("unknown/model"))
}

func TestSelectReviewPanelDryFromStart(t *testing.T) {
	// Zero qualifying candidates (no model carries a prior for the role): the
	// panel must still be n non-empty specs - all the capable default, never
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
		"cheap/ok":     {Coder: new(0.80)},
		"black/listed": {Coder: new(0.95)},
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

func TestSelectCandidateModelsNoPinWrapsAround(t *testing.T) {
	// Three equally-priced, equally-qualified models: the exclude set built up
	// across rounds forces distinct picks in catalog order while the pool
	// lasts (m1, m2, m3), then the pool runs dry and the 4th slot reuses the
	// last real pick (SelectReviewPanel wrap semantics) rather than shrinking
	// n or escalating price.
	catalog := llm.Catalog{
		entry("m1", 1.0, 2.0, 200000),
		entry("m2", 1.0, 2.0, 200000),
		entry("m3", 1.0, 2.0, 200000),
	}
	priors := Priors{Models: map[string]PriorEntry{
		"m1": {Coder: new(0.80)}, "m2": {Coder: new(0.80)}, "m3": {Coder: new(0.80)},
	}}
	r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default")
	in := SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000}

	specs := r.SelectCandidateModels(in, 4, "")
	require.Len(t, specs, 4)
	assert.Equal(t, "m1", specs[0].Model)
	assert.Equal(t, "m2", specs[1].Model)
	assert.Equal(t, "m3", specs[2].Model)
	assert.Equal(t, specs[2], specs[3], "4th slot must wrap and repeat the 3rd pick exactly")
}

func TestSelectCandidateModelsSingleModelPoolRepeatsThroughout(t *testing.T) {
	catalog := llm.Catalog{entry("m1", 1.0, 2.0, 200000)}
	priors := Priors{Models: map[string]PriorEntry{"m1": {Coder: new(0.80)}}}
	r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default")
	in := SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000}

	specs := r.SelectCandidateModels(in, 3, "")
	require.Len(t, specs, 3)

	for i, s := range specs {
		assert.Equal(t, "m1", s.Model, "slot %d", i)
		assert.Equal(t, 200000, s.ContextWindow, "slot %d", i)
	}
}

func TestSelectCandidateModelsPinOccupiesSlotOneExcludedFromRest(t *testing.T) {
	// pinned/x sits first in catalog order with the same price/quality profile
	// as m1..m3: if the pin were not excluded from the auto-pick rounds, tie
	// break would select it again for slot 2. Catching that requires pinned/x
	// to be genuinely attractive, not just present.
	catalog := llm.Catalog{
		entry("pinned/x", 1.0, 2.0, 99000),
		entry("m1", 1.0, 2.0, 200000),
		entry("m2", 1.0, 2.0, 200000),
		entry("m3", 1.0, 2.0, 200000),
	}
	priors := Priors{Models: map[string]PriorEntry{
		"pinned/x": {Coder: new(0.80)}, "m1": {Coder: new(0.80)},
		"m2": {Coder: new(0.80)}, "m3": {Coder: new(0.80)},
	}}
	r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default")
	in := SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000}

	specs := r.SelectCandidateModels(in, 3, "pinned/x")
	require.Len(t, specs, 3)
	assert.Equal(t, "pinned/x", specs[0].Model)
	assert.Equal(t, 99000, specs[0].ContextWindow, "pin must carry its own catalog context window")
	assert.Equal(t, "m1", specs[1].Model)
	assert.Equal(t, "m2", specs[2].Model)
}

func TestSelectCandidateModelsPinMergesExcludeWithoutMutatingCaller(t *testing.T) {
	catalog := llm.Catalog{
		entry("pinned/x", 1.0, 2.0, 200000),
		entry("m1", 1.0, 2.0, 200000),
		entry("m2", 1.0, 2.0, 200000),
		entry("m3", 1.0, 2.0, 200000),
		entry("already-excluded", 1.0, 2.0, 200000),
	}
	priors := Priors{Models: map[string]PriorEntry{
		"pinned/x": {Coder: new(0.80)}, "m1": {Coder: new(0.80)}, "m2": {Coder: new(0.80)},
		"m3": {Coder: new(0.80)}, "already-excluded": {Coder: new(0.80)},
	}}
	r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default")

	origExclude := map[string]bool{"already-excluded": true}
	in := SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000, Exclude: origExclude}

	specs := r.SelectCandidateModels(in, 3, "pinned/x")
	require.Len(t, specs, 3)
	assert.Equal(t, "pinned/x", specs[0].Model)
	assert.Equal(t, "m1", specs[1].Model)
	assert.Equal(t, "m2", specs[2].Model)

	for _, s := range specs {
		assert.NotEqual(t, "already-excluded", s.Model, "pre-existing Exclude entries must carry through to the auto picks")
	}

	assert.Equal(t, map[string]bool{"already-excluded": true}, origExclude,
		"SelectCandidateModels must not mutate the caller's Exclude map")
}

func TestSelectCandidateModelsZeroOrNegativeNReturnsNil(t *testing.T) {
	r := NewRegistry("capable-default", testCatalog())
	in := SelectInput{Role: RoleCoder, Tier: TierSimple}

	assert.Nil(t, r.SelectCandidateModels(in, 0, ""))
	assert.Nil(t, r.SelectCandidateModels(in, -1, ""))
	assert.Nil(t, r.SelectCandidateModels(in, 0, "pinned/x"))
}

func TestFavoritesConsideredFirst(t *testing.T) {
	cat := llm.Catalog{
		{ID: "cheap/win", PromptPricePerTok: 1e-8, CompletionPricePerTok: 1e-8, ContextLength: 200000, SupportedParameters: []string{"tools"}},
		{ID: "fav/pick", PromptPricePerTok: 1e-6, CompletionPricePerTok: 1e-6, ContextLength: 200000, SupportedParameters: []string{"tools"}},
	}
	pr := Priors{Models: map[string]PriorEntry{
		"cheap/win": {Coder: new(0.90)}, "fav/pick": {Coder: new(0.90)},
	}}
	favs := map[favKey][]string{{Tier: TierComplex}: {"fav/pick"}}
	r := NewRegistryFromParts(cat, pr, nil, favs, "capable/default")

	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
	if got.Model != "fav/pick" {
		t.Errorf("favorite must win over cheaper cost-optimal, got %q", got.Model)
	}
}

// TestSelectDiscussionPanel pins the mob session seat-selection seam: it must give
// distinct models first, honor the caller's exclusions (review discussions
// exclude the models that coded the card), and wrap around on scarcity
// instead of shrinking the panel - the SelectReviewPanel walk, by name.
func TestSelectDiscussionPanel(t *testing.T) {
	// Four qualifying reviewers at the complex bar (0.82) with distinct prices.
	catalog := llm.Catalog{
		entry("disc/alpha", 0.7, 1.4, 200000),
		entry("disc/beta", 0.9, 1.8, 200000),
		entry("disc/gamma", 1.0, 2.0, 200000),
		entry("disc/delta", 6.0, 12.0, 200000),
	}
	fullPriors := Priors{Models: map[string]PriorEntry{
		"disc/alpha": {Reviewer: new(0.95)}, "disc/beta": {Reviewer: new(0.92)},
		"disc/gamma": {Reviewer: new(0.90)}, "disc/delta": {Reviewer: new(0.88)},
	}}
	// Scarce pool: only alpha and beta clear the complex bar.
	scarcePriors := Priors{Models: map[string]PriorEntry{
		"disc/alpha": {Reviewer: new(0.95)}, "disc/beta": {Reviewer: new(0.92)},
		"disc/gamma": {Reviewer: new(0.50)}, "disc/delta": {Reviewer: new(0.50)},
	}}

	tests := []struct {
		name    string
		priors  Priors
		in      SelectInput
		n       int
		want    []string
		wantLen int
	}{
		{
			// Blended $/Mtok: alpha 2.1, beta 2.7, gamma 3.0, delta 18. Pick 1:
			// cheapest 2.1 -> band 3.15: alpha, beta, gamma in; top quality
			// alpha. Pick 2 (alpha excluded): band from 2.7 -> 4.05: beta,
			// gamma; top beta. Pick 3: gamma.
			name:   "distinct models across seats",
			priors: fullPriors,
			in:     SelectInput{Role: RoleReviewer, Tier: TierComplex, EstTokens: 50000},
			n:      3,
			want:   []string{"disc/alpha", "disc/beta", "disc/gamma"},
		},
		{
			// Excluding the coder's model removes it from every seat.
			name:   "caller exclusions respected",
			priors: fullPriors,
			in: SelectInput{
				Role: RoleReviewer, Tier: TierComplex, EstTokens: 50000,
				Exclude: map[string]bool{"disc/alpha": true},
			},
			n:    3,
			want: []string{"disc/beta", "disc/gamma", "disc/delta"},
		},
		{
			// Two qualifying models, three seats: wrap around on the last real
			// pick rather than escalating price or shrinking the panel.
			name:   "wrap-around on scarcity",
			priors: scarcePriors,
			in:     SelectInput{Role: RoleReviewer, Tier: TierComplex, EstTokens: 50000},
			n:      3,
			want:   []string{"disc/alpha", "disc/beta", "disc/beta"},
		},
		{
			// n far beyond the pool: still n non-empty specs.
			name:    "n greater than available",
			priors:  scarcePriors,
			in:      SelectInput{Role: RoleReviewer, Tier: TierComplex, EstTokens: 50000},
			n:       5,
			wantLen: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistryFromParts(catalog, tt.priors, nil, nil, "capable-default")

			panel := r.SelectDiscussionPanel(tt.in, tt.n)

			if tt.wantLen > 0 {
				require.Len(t, panel, tt.wantLen)

				for i, spec := range panel {
					assert.NotEmpty(t, spec.Model, "slot %d must not be empty", i)
				}

				return
			}

			require.Len(t, panel, len(tt.want))

			for i, w := range tt.want {
				assert.Equal(t, w, panel[i].Model, "slot %d", i)
			}

			if tt.in.Exclude != nil {
				for i, spec := range panel {
					assert.False(t, tt.in.Exclude[spec.Model], "slot %d picked an excluded model %q", i, spec.Model)
				}
			}
		})
	}
}

// TestSelectReviewPanelSpansVendors reproduces the reported incident: an
// OpenAI-compatible gateway (bare slugs, creators supplied by CM) whose top
// reviewer priors all belong to one vendor. Without vendor awareness the
// panel came out gpt/gpt/gpt even though a qualifying model from another
// vendor was available.
func TestSelectReviewPanelSpansVendors(t *testing.T) {
	// Blended $/Mtok: gpt-a 2.1, gpt-b 2.7, gpt-c 3.0, claude-x 3.6. All
	// clear the complex bar (0.82).
	catalog := llm.Catalog{
		entry("gpt-a", 0.7, 1.4, 200000),
		entry("gpt-b", 0.9, 1.8, 200000),
		entry("gpt-c", 1.0, 2.0, 200000),
		entry("claude-x", 1.2, 2.4, 200000),
	}
	priors := Priors{Models: map[string]PriorEntry{
		"gpt-a": {Reviewer: new(0.95)}, "gpt-b": {Reviewer: new(0.90)},
		"gpt-c": {Reviewer: new(0.88)}, "claude-x": {Reviewer: new(0.85)},
	}}
	r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default").
		WithCreators(map[string]string{
			"gpt-a": "openai", "gpt-b": "openai", "gpt-c": "openai",
			"claude-x": "anthropic",
		})

	in := SelectInput{Role: RoleReviewer, Tier: TierComplex, EstTokens: 50000}
	panel := r.SelectReviewPanel(in, 3)
	require.Len(t, panel, 3)

	// Seat 1 is unchanged from the vendor-blind walk: band 2.1*1.5=3.15 holds
	// gpt-a/b/c; top quality gpt-a. Seat 2 prefers the unseated vendor:
	// claude-x (band re-anchors on the filtered subset). Seat 3 has no unseated
	// vendor left and falls back to the vendor-blind pick: gpt-b.
	assert.Equal(t, "gpt-a", panel[0].Model)
	assert.Equal(t, "claude-x", panel[1].Model)
	assert.Equal(t, "gpt-b", panel[2].Model)
}

// TestSelectReviewPanelFavoriteBypassesVendorFilter pins config precedence:
// an operator favorite wins its seat even when its vendor is already on the
// panel - the vendor preference is an emergent heuristic and must never
// override explicit favorites. The seated favorite's vendor still counts as
// used for later seats.
func TestSelectReviewPanelFavoriteBypassesVendorFilter(t *testing.T) {
	catalog := llm.Catalog{
		entry("v1-fav", 1.0, 2.0, 200000),
		entry("v1-fav2", 1.0, 2.0, 200000),
		entry("v2-b", 1.0, 2.0, 200000),
	}
	priors := Priors{Models: map[string]PriorEntry{
		"v1-fav": {Reviewer: new(0.90)}, "v1-fav2": {Reviewer: new(0.85)},
		"v2-b": {Reviewer: new(0.95)},
	}}
	favs := map[favKey][]string{{Tier: TierComplex}: {"v1-fav", "v1-fav2"}}
	r := NewRegistryFromParts(catalog, priors, nil, favs, "capable-default").
		WithCreators(map[string]string{
			"v1-fav": "openai", "v1-fav2": "openai", "v2-b": "anthropic",
		})

	in := SelectInput{Role: RoleReviewer, Tier: TierComplex, EstTokens: 50000}
	panel := r.SelectReviewPanel(in, 3)
	require.Len(t, panel, 3)

	// Seat 1: first favorite. Seat 2: the second favorite must win despite
	// openai already being seated (bypass), beating the unseated-vendor v2-b.
	// Seat 3: both favorites consumed; v2-b remains.
	assert.Equal(t, "v1-fav", panel[0].Model)
	assert.Equal(t, "v1-fav2", panel[1].Model)
	assert.Equal(t, "v2-b", panel[2].Model)
}

// TestSelectCandidateModelsPinSeedsVendor pins that a Best-of-N pin counts as
// a seated vendor: the auto-filled slots steer toward other vendors first.
func TestSelectCandidateModelsPinSeedsVendor(t *testing.T) {
	catalog := llm.Catalog{
		entry("gpt-pin", 1.0, 2.0, 200000),
		entry("gpt-d", 1.0, 2.0, 200000),
		entry("claude-y", 1.0, 2.0, 200000),
	}
	priors := Priors{Models: map[string]PriorEntry{
		"gpt-pin": {Coder: new(0.90)}, "gpt-d": {Coder: new(0.95)},
		"claude-y": {Coder: new(0.85)},
	}}
	r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default").
		WithCreators(map[string]string{
			"gpt-pin": "openai", "gpt-d": "openai", "claude-y": "anthropic",
		})

	in := SelectInput{Role: RoleCoder, Tier: TierComplex, EstTokens: 50000}
	specs := r.SelectCandidateModels(in, 3, "gpt-pin")
	require.Len(t, specs, 3)
	assert.Equal(t, "gpt-pin", specs[0].Model)
	// Slot 2 prefers the unseated vendor over the higher-prior gpt-d.
	assert.Equal(t, "claude-y", specs[1].Model)
	assert.Equal(t, "gpt-d", specs[2].Model)

	// A pin with no resolvable vendor (absent from the catalog, bare slug)
	// seeds nothing and must not panic.
	specs = r.SelectCandidateModels(in, 2, "mystery")
	require.Len(t, specs, 2)
	assert.Equal(t, "mystery", specs[0].Model)
	assert.Equal(t, "gpt-d", specs[1].Model, "no vendor seed: slot 2 stays the vendor-blind pick")
}

// TestSelectReviewPanelVendorEdgeCases pins the soft-preference semantics:
// the vendor filter never downgrades quality below the tier bar, never
// touches models without a resolvable vendor, re-anchors the price band on
// the filtered subset, degrades to the vendor-blind walk on single-vendor
// pools, and keeps wrap-around scarcity semantics.
func TestSelectReviewPanelVendorEdgeCases(t *testing.T) {
	in := SelectInput{Role: RoleReviewer, Tier: TierComplex, EstTokens: 50000}

	t.Run("soft fallback when the only unseated vendor misses the bar", func(t *testing.T) {
		catalog := llm.Catalog{
			entry("gpt-a", 1.0, 2.0, 200000),
			entry("gpt-b", 1.0, 2.0, 200000),
			entry("claude-weak", 1.0, 2.0, 200000),
		}
		priors := Priors{Models: map[string]PriorEntry{
			"gpt-a": {Reviewer: new(0.95)}, "gpt-b": {Reviewer: new(0.90)},
			"claude-weak": {Reviewer: new(0.80)}, // below the complex bar 0.82
		}}
		r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default").
			WithCreators(map[string]string{"gpt-a": "openai", "gpt-b": "openai", "claude-weak": "anthropic"})

		panel := r.SelectReviewPanel(in, 2)
		require.Len(t, panel, 2)
		assert.Equal(t, "gpt-a", panel[0].Model)
		assert.Equal(t, "gpt-b", panel[1].Model, "diversity must never seat a below-bar model")
	})

	t.Run("diverse seat may cost above the vendor-blind band", func(t *testing.T) {
		catalog := llm.Catalog{
			entry("gpt-a", 0.7, 1.4, 200000),        // $2.1
			entry("gpt-b", 0.9, 1.8, 200000),        // $2.7
			entry("claude-exp", 10.0, 20.0, 200000), // $30, far outside 2.7*1.5
		}
		priors := Priors{Models: map[string]PriorEntry{
			"gpt-a": {Reviewer: new(0.95)}, "gpt-b": {Reviewer: new(0.90)},
			"claude-exp": {Reviewer: new(0.93)},
		}}
		r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default").
			WithCreators(map[string]string{"gpt-a": "openai", "gpt-b": "openai", "claude-exp": "anthropic"})

		panel := r.SelectReviewPanel(in, 2)
		require.Len(t, panel, 2)
		assert.Equal(t, "gpt-a", panel[0].Model)
		assert.Equal(t, "claude-exp", panel[1].Model,
			"the band re-anchors on the vendor-filtered subset (documented cost of diversity)")
	})

	t.Run("models without a resolvable vendor pass every vendor filter", func(t *testing.T) {
		catalog := llm.Catalog{
			entry("gpt-a", 1.0, 2.0, 200000),
			entry("gpt-b", 1.0, 2.0, 200000),
			entry("bare-n", 1.0, 2.0, 200000), // no creator, no slug prefix
		}
		priors := Priors{Models: map[string]PriorEntry{
			"gpt-a": {Reviewer: new(0.95)}, "gpt-b": {Reviewer: new(0.93)},
			"bare-n": {Reviewer: new(0.85)},
		}}
		r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default").
			WithCreators(map[string]string{"gpt-a": "openai", "gpt-b": "openai"})

		panel := r.SelectReviewPanel(in, 3)
		require.Len(t, panel, 3)
		assert.Equal(t, "gpt-a", panel[0].Model)
		assert.Equal(t, "bare-n", panel[1].Model,
			"vendor-less models stay selectable during a vendor-filtered attempt")
		assert.Equal(t, "gpt-b", panel[2].Model)
	})

	t.Run("single-vendor pool matches the vendor-blind walk", func(t *testing.T) {
		catalog := llm.Catalog{
			entry("gpt-a", 0.7, 1.4, 200000),
			entry("gpt-b", 0.9, 1.8, 200000),
			entry("gpt-c", 1.0, 2.0, 200000),
		}
		priors := Priors{Models: map[string]PriorEntry{
			"gpt-a": {Reviewer: new(0.95)}, "gpt-b": {Reviewer: new(0.90)}, "gpt-c": {Reviewer: new(0.88)},
		}}
		creators := map[string]string{"gpt-a": "openai", "gpt-b": "openai", "gpt-c": "openai"}

		blind := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default").SelectReviewPanel(in, 3)
		aware := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default").
			WithCreators(creators).SelectReviewPanel(in, 3)
		assert.Equal(t, blind, aware)
	})

	t.Run("wrap-around scarcity keeps reusing the last pick", func(t *testing.T) {
		catalog := llm.Catalog{
			entry("gpt-a", 1.0, 2.0, 200000),
			entry("claude-x", 1.0, 2.0, 200000),
		}
		priors := Priors{Models: map[string]PriorEntry{
			"gpt-a": {Reviewer: new(0.95)}, "claude-x": {Reviewer: new(0.90)},
		}}
		r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default").
			WithCreators(map[string]string{"gpt-a": "openai", "claude-x": "anthropic"})

		panel := r.SelectReviewPanel(in, 4)
		require.Len(t, panel, 4)
		assert.Equal(t, "gpt-a", panel[0].Model)
		assert.Equal(t, "claude-x", panel[1].Model)
		assert.Equal(t, "claude-x", panel[2].Model, "dry pool wraps on the last real pick")
		assert.Equal(t, "claude-x", panel[3].Model)
	})

	t.Run("namespaced slugs diversify without a creators map", func(t *testing.T) {
		// Old-CM OpenRouter leg: no creators shipped, vendors recovered from
		// the slug prefix.
		catalog := llm.Catalog{
			entry("openai/one", 1.0, 2.0, 200000),
			entry("openai/two", 1.0, 2.0, 200000),
			entry("anthropic/x", 1.0, 2.0, 200000),
		}
		priors := Priors{Models: map[string]PriorEntry{
			"openai/one": {Reviewer: new(0.95)}, "openai/two": {Reviewer: new(0.90)},
			"anthropic/x": {Reviewer: new(0.85)},
		}}
		r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default")

		panel := r.SelectReviewPanel(in, 3)
		require.Len(t, panel, 3)
		assert.Equal(t, "openai/one", panel[0].Model)
		assert.Equal(t, "anthropic/x", panel[1].Model)
		assert.Equal(t, "openai/two", panel[2].Model)
	})
}

func TestSelectReviewPanelClampsBeforeDuplicating(t *testing.T) {
	panel := ladderRegistry(nil).SelectReviewPanel(SelectInput{Role: RoleReviewer, Tier: TierCritical}, 3)
	require.Len(t, panel, 3)

	models := make([]string, len(panel))
	for i, s := range panel {
		require.True(t, s.OK, "seat %d", i)
		assert.False(t, s.Duplicate, "seat %d: a clamped-but-distinct seat is not a duplicate", i)
		models[i] = s.Model
	}

	assert.Equal(t, 3, DistinctModels(panel),
		"seats must be distinct models, not one model repeated: %v", models)

	// Seat 1 holds the requested tier; the seats below it clamp down a rung to
	// stay distinct rather than duplicating seat 1 at critical price.
	assert.Equal(t, "top/one", panel[0].Model)
	assert.Equal(t, TierCritical, panel[0].MetTier)
	assert.True(t, panel[0].AtBar())

	for _, s := range panel[1:] {
		assert.True(t, s.BelowBar(), "seat %s must report its clamp", s.Model)
		assert.Equal(t, TierCritical, s.RequestedTier)
	}
}

// TestSelectReviewPanelSeatsDegradeIndependently pins that one seat dropping a
// rung never drags the seats above it down with it. high/two is excluded so
// the complex pool holds exactly two models (top/one, high/one): both seats 1
// and 2 stay at complex, and only seat 3, with the complex pool now empty,
// clamps to moderate.
func TestSelectReviewPanelSeatsDegradeIndependently(t *testing.T) {
	in := SelectInput{Role: RoleReviewer, Tier: TierComplex, Exclude: map[string]bool{"high/two": true}}
	panel := ladderRegistry(nil).SelectReviewPanel(in, 3)
	require.Len(t, panel, 3)

	assert.Equal(t, TierComplex, panel[0].MetTier)
	assert.Equal(t, TierComplex, panel[1].MetTier, "a second complex-clearing model must stay at complex")
	assert.Equal(t, TierModerate, panel[2].MetTier)
	assert.Equal(t, 3, DistinctModels(panel))
}

// TestSelectReviewPanelFillsWithARealPickNotAnEscalation pins the last-resort
// order: a repeat is reached only when no rung holds an unseated model, and it
// repeats a real, quality-bearing pick rather than escalating price.
func TestSelectReviewPanelFillsWithARealPickNotAnEscalation(t *testing.T) {
	cat := llm.Catalog{
		entry("only/one", 1.0, 2.0, 200000),
		entry("sub/floor", 0.1, 0.2, 200000),
	}
	priors := Priors{Models: map[string]PriorEntry{
		"only/one":  {Reviewer: new(0.88)},
		"sub/floor": {Reviewer: new(0.40)},
	}}
	r := NewRegistryFromParts(cat, priors, nil, nil, "capable/default")

	panel := r.SelectReviewPanel(SelectInput{Role: RoleReviewer, Tier: TierCritical}, 3)
	require.Len(t, panel, 3, "the panel is always n seats")

	assert.Equal(t, "only/one", panel[0].Model)
	assert.InDelta(t, 0.88, panel[0].Prior, 1e-9)
	assert.False(t, panel[0].Duplicate)

	// Seats 2 and 3: only/one is excluded now, so the ladder is dry and the
	// capable default is the floor - a duplicate of it, flagged both ways.
	for i, s := range panel[1:] {
		require.True(t, s.OK, "seat %d", i+1)
		assert.True(t, s.Duplicate, "seat %d must be flagged as a repeat", i+1)
	}

	assert.Less(t, DistinctModels(panel), 3, "the collapse must be countable")
}

// TestSelectReviewPanelVendorPreferenceIsBoundedToTheRung pins that the soft
// diversity preference breaks ties WITHIN a rung and never overrides the
// quality ladder. Without the rung bound, seat 2 walks down to find the fresh
// vendor and the panel trades a measured 0.83 for a measured 0.66.
func TestSelectReviewPanelVendorPreferenceIsBoundedToTheRung(t *testing.T) {
	r := ladderRegistry(nil).WithCreators(map[string]string{
		"top/one": "alpha", "high/one": "alpha", "high/two": "alpha",
		"mid/one": "alpha", "mid/two": "alpha", "low/one": "beta",
	})

	panel := r.SelectReviewPanel(SelectInput{Role: RoleReviewer, Tier: TierComplex}, 2)
	require.Len(t, panel, 2)

	assert.Equal(t, "high/one", panel[0].Model)
	assert.Equal(t, "high/two", panel[1].Model,
		"seat 2 must stay on the complex rung; diversity must not buy a rung of quality")
	assert.True(t, panel[1].AtBar())
}

func TestSelectReviewPanelReturnsNothingWhenNoModelIsSelectable(t *testing.T) {
	cat := llm.Catalog{entry("sub/floor", 0.1, 0.2, 200000)}
	priors := Priors{Models: map[string]PriorEntry{"sub/floor": {Reviewer: new(0.40)}}}
	// No capable default and nothing employable: the only honest answer is none.
	r := NewRegistryFromParts(cat, priors, nil, nil, "")

	assert.Nil(t, r.SelectReviewPanel(SelectInput{Role: RoleReviewer, Tier: TierSimple}, 3))
}

// TestSelectCandidateModelsPinReportsMeasuredNotAsserted pins that the pin seat
// does not fabricate a met tier: authority is Source, measurement is MetTier.
func TestSelectCandidateModelsPinReportsMeasuredNotAsserted(t *testing.T) {
	r := ladderRegistry(nil)

	picks := r.SelectCandidateModels(SelectInput{Role: RoleCoder, Tier: TierCritical}, 2, "sub/floor")
	require.Len(t, picks, 2)

	assert.Equal(t, "sub/floor", picks[0].Model)
	assert.Equal(t, SourcePinned, picks[0].Source, "a pin is authoritative")
	assert.Empty(t, picks[0].MetTier, "a 0.40 prior clears no configured bar - say so")
	assert.False(t, picks[0].AtBar())
	assert.InDelta(t, 0.40, picks[0].Prior, 1e-9)
	assert.True(t, picks[0].HasPrior)

	assert.Equal(t, "top/one", picks[1].Model, "the auto seat beside a pin is unaffected")
	assert.True(t, picks[1].AtBar())
}

// --- MaxCapability tests ---

func TestMaxCapabilityBypassesFavorites(t *testing.T) {
	cat := llm.Catalog{
		{ID: "cheap/win", PromptPricePerTok: 1e-8, CompletionPricePerTok: 1e-8, ContextLength: 200000, SupportedParameters: []string{"tools"}},
		{ID: "fav/pick", PromptPricePerTok: 1e-6, CompletionPricePerTok: 1e-6, ContextLength: 200000, SupportedParameters: []string{"tools"}},
	}
	pr := Priors{Models: map[string]PriorEntry{
		"cheap/win": {Coder: new(0.90)}, "fav/pick": {Coder: new(0.90)},
	}}
	favs := map[favKey][]string{{Tier: TierComplex}: {"fav/pick"}}

	t.Run("favorite honored by default", func(t *testing.T) {
		r := NewRegistryFromParts(cat, pr, nil, favs, "capable/default")
		got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
		assert.Equal(t, "fav/pick", got.Model)
	})

	t.Run("favorite bypassed with MaxCapability", func(t *testing.T) {
		r := NewRegistryFromParts(cat, pr, nil, favs, "capable/default")
		r.sel.MaxCapability = true
		got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
		// cheap/win is cheaper and same quality; tie breaks to cheaper.
		assert.Equal(t, "cheap/win", got.Model)
	})
}

func TestMaxCapabilitySelectsMostExpensiveQualifying(t *testing.T) {
	// Reuse the TestSelectByComplexityPriorsOnly catalog. frontier is the most
	// expensive ($18, q0.95) and star is cheapest ($1.2, q0.99). Excluding star
	// makes frontier the highest-quality remaining candidate.
	catalog := llm.Catalog{
		entry("cheap-weak", 0.5, 1.0, 200000),
		entry("cheap-good", 0.7, 1.4, 200000),
		entry("mid-better", 0.9, 1.8, 200000),
		entry("frontier", 6.0, 12.0, 200000),
		entry("star", 0.4, 0.8, 200000),
		entry("small-window", 0.6, 1.2, 8000),
	}
	priors := Priors{Models: map[string]PriorEntry{
		"cheap-weak": {Coder: new(0.50)}, "cheap-good": {Coder: new(0.70)},
		"mid-better": {Coder: new(0.85)}, "frontier": {Coder: new(0.95)},
		"star": {Coder: new(0.99)}, "small-window": {Coder: new(0.85)},
	}}
	// TierSimple (bar 0.65): cheap-weak (0.50 < 0.65) out; small-window (8k<50k) out.
	// star excluded; remaining: cheap-good, mid-better, frontier.
	in := SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000, Exclude: map[string]bool{"star": true}}

	t.Run("default picks best value (mid-better)", func(t *testing.T) {
		r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default")
		got := r.SelectByComplexity(in)
		// cheapest $2.1 -> band $3.15; cheap-good, mid-better in; frontier out; highest in band: mid-better.
		assert.Equal(t, "mid-better", got.Model)
	})

	t.Run("MaxCapability picks most capable regardless of price (frontier)", func(t *testing.T) {
		r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default")
		r.sel.MaxCapability = true
		got := r.SelectByComplexity(in)
		// band = +Inf; frontier has highest quality (0.95).
		assert.Equal(t, "frontier", got.Model)
	})
}

func TestMaxCapabilityRespectsTierBar(t *testing.T) {
	catalog := llm.Catalog{
		entry("below-bar", 0.5, 1.0, 200000),
		entry("above-bar", 1.0, 2.0, 200000),
	}
	priors := Priors{Models: map[string]PriorEntry{
		"below-bar": {Coder: new(0.50)}, // below simple bar 0.65
		"above-bar": {Coder: new(0.80)}, // passes simple bar
	}}
	r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default")
	r.sel.MaxCapability = true

	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000})
	assert.Equal(t, "above-bar", got.Model, "below-bar model must never be selected even with MaxCapability")
}

func TestMaxCapabilityRespectsBlacklist(t *testing.T) {
	catalog := llm.Catalog{
		entry("good/model", 1.0, 2.0, 200000),
		entry("black/listed", 0.5, 1.0, 200000),
	}
	priors := Priors{Models: map[string]PriorEntry{
		"good/model":   {Coder: new(0.80)},
		"black/listed": {Coder: new(0.95)},
	}}
	blacklist := map[string]bool{"black/listed": true}
	r := NewRegistryFromParts(catalog, priors, blacklist, nil, "capable-default")
	r.sel.MaxCapability = true

	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000})
	assert.Equal(t, "good/model", got.Model, "blacklisted model must never be selected even with MaxCapability")
}

func TestMaxCapabilityEqualQualityTieBreaksToCheaper(t *testing.T) {
	catalog := llm.Catalog{
		entry("cheap/model", 1.0, 2.0, 200000),       // $3
		entry("expensive/model", 10.0, 20.0, 200000), // $30
	}
	priors := Priors{Models: map[string]PriorEntry{
		"cheap/model":     {Coder: new(0.90)},
		"expensive/model": {Coder: new(0.90)},
	}}
	r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default")
	r.sel.MaxCapability = true

	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000})
	assert.Equal(t, "cheap/model", got.Model, "equal quality must tie-break to cheaper model")
}

func TestMaxCapabilityEmptyPoolFallsBack(t *testing.T) {
	// No model carries a prior; pool is empty.
	catalog := llm.Catalog{entry("any/model", 1.0, 2.0, 200000)}
	r := NewRegistryFromParts(catalog, Priors{}, nil, nil, "capable-default")
	r.sel.MaxCapability = true

	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000})
	assert.Equal(t, "capable-default", got.Model)
}

func TestMaxCapabilityReviewPanelSpansVendors(t *testing.T) {
	// Reuse the TestSelectReviewPanelSpansVendors scenario with MaxCapability set.
	// With band = +Inf the vendor-diversity preference still applies because it
	// is driven by ExcludeVendors filtering, not price.
	catalog := llm.Catalog{
		entry("gpt-a", 0.7, 1.4, 200000),
		entry("gpt-b", 0.9, 1.8, 200000),
		entry("gpt-c", 1.0, 2.0, 200000),
		entry("claude-x", 1.2, 2.4, 200000),
	}
	priors := Priors{Models: map[string]PriorEntry{
		"gpt-a": {Reviewer: new(0.95)}, "gpt-b": {Reviewer: new(0.90)},
		"gpt-c": {Reviewer: new(0.88)}, "claude-x": {Reviewer: new(0.85)},
	}}
	r := NewRegistryFromParts(catalog, priors, nil, nil, "capable-default").
		WithCreators(map[string]string{
			"gpt-a": "openai", "gpt-b": "openai", "gpt-c": "openai",
			"claude-x": "anthropic",
		})
	r.sel.MaxCapability = true

	in := SelectInput{Role: RoleReviewer, Tier: TierComplex, EstTokens: 50000}
	panel := r.SelectReviewPanel(in, 3)
	require.Len(t, panel, 3)

	// Seat 1: all candidates; highest quality gpt-a (0.95). Seat 2: prefers
	// unseated vendor claude-x (0.85). Seat 3: no unseated vendor left,
	// vendor-blind pick gpt-b (0.90).
	assert.Equal(t, "gpt-a", panel[0].Model)
	assert.Equal(t, "claude-x", panel[1].Model)
	assert.Equal(t, "gpt-b", panel[2].Model)
}

// ladderRegistry spreads seven tool-capable models across the default bars so a
// walk from critical to simple crosses every rung, with at least two models per
// rung so the best-value rule is live at each one rather than settled by a
// single survivor. Blended $/Mtok: top/one 15.0, high/one 3.0, high/two 3.3,
// mid/one 1.8, mid/two 1.5, low/one 0.6, sub/floor 0.3.
//
// At-rung picks: critical -> top/one; complex (cheapest 3.0, band 4.5) ->
// high/one; moderate (cheapest 1.5, band 2.25) -> mid/one; simple (cheapest
// 0.6, band 0.9) -> low/one. sub/floor (0.40) clears no bar at all.
func ladderRegistry(favorites map[favKey][]string) *Registry {
	cat := llm.Catalog{
		entry("top/one", 5.0, 10.0, 200000),
		entry("high/one", 1.0, 2.0, 200000),
		entry("high/two", 1.1, 2.2, 200000),
		entry("mid/one", 0.6, 1.2, 200000),
		entry("mid/two", 0.5, 1.0, 200000),
		entry("low/one", 0.2, 0.4, 200000),
		entry("sub/floor", 0.1, 0.2, 200000),
	}
	priors := Priors{Models: map[string]PriorEntry{
		"top/one":   {Coder: new(0.93), Reviewer: new(0.93)},
		"high/one":  {Coder: new(0.85), Reviewer: new(0.85)},
		"high/two":  {Coder: new(0.83), Reviewer: new(0.83)},
		"mid/one":   {Coder: new(0.79), Reviewer: new(0.79)},
		"mid/two":   {Coder: new(0.77), Reviewer: new(0.77)},
		"low/one":   {Coder: new(0.66), Reviewer: new(0.66)},
		"sub/floor": {Coder: new(0.40), Reviewer: new(0.40)},
	}}

	return NewRegistryFromParts(cat, priors, nil, favorites, "capable/default")
}

// oldPick recomputes HEAD's selection for in.Tier ALONE - no walk, no rung
// ordering - as the oracle for the at-bar guarantee. It deliberately reuses the
// lifted helpers: what changed in this task is the walk, so the oracle's job is
// to prove the walk never touches an answer the walk should not reach.
func oldPick(r *Registry, in SelectInput) string {
	blind := in
	blind.ExcludeVendors = nil

	if fav := r.favoriteAmong(r.candidates(blind), in.Tier, in.Role); fav != "" {
		return fav
	}

	cands := r.candidates(in)
	if len(cands) == 0 {
		return ""
	}

	return bestValue(cands, r.headroom(), r.sel.MaxCapability)
}

// TestAtBarSelectionsAreUnchanged is THE production-behaviour guard. Every
// selection whose requested tier has a non-empty pool - which is every
// selection production makes today at simple and moderate - must return exactly
// the model the pre-ladder selector returned, down to the tie-break.
func TestAtBarSelectionsAreUnchanged(t *testing.T) {
	tests := []struct {
		name string
		favs map[favKey][]string
	}{
		{name: "no favorites"},
		{name: "operator favorites configured", favs: map[favKey][]string{
			{Tier: TierModerate}:                    {"mid/two"},
			{Tier: TierComplex, Role: RoleReviewer}: {"high/two"},
		}},
	}

	tiers := []Tier{TierSimple, TierModerate, TierComplex, TierCritical}
	exclusions := []map[string]bool{
		nil,
		{"mid/two": true},
		{"low/one": true, "mid/two": true},
		{"top/one": true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := ladderRegistry(tt.favs)

			covered := 0

			for _, role := range []Role{RoleCoder, RoleReviewer} {
				for _, tier := range tiers {
					for i, excl := range exclusions {
						in := SelectInput{Role: role, Tier: tier, Exclude: excl, EstTokens: 50000}
						if len(r.candidates(in)) == 0 {
							continue // the ladder case, covered by the walk tests
						}

						covered++
						got := r.SelectByComplexity(in)

						require.True(t, got.AtBar(),
							"a non-empty pool at %s must be served at %s (role=%s excl=%d)", tier, tier, role, i)
						assert.Equal(t, oldPick(r, in), got.Model,
							"role=%s tier=%s excl=%d: the ladder changed an at-bar answer", role, tier, i)
					}
				}
			}

			require.Positive(t, covered, "fixture drift: the matrix exercised no at-bar selection")
		})
	}
}

func TestSelectByComplexityClampsDownTheLadder(t *testing.T) {
	r := ladderRegistry(nil)

	tests := []struct {
		name    string
		in      SelectInput
		wantID  string
		wantMet Tier
	}{
		{
			name:    "requested tier has a pool",
			in:      SelectInput{Role: RoleCoder, Tier: TierCritical},
			wantID:  "top/one",
			wantMet: TierCritical,
		},
		{
			// Critical is dry, so the pick is the one a DIRECT complex request
			// would have made, not the capable default - which may be weaker
			// than high/one.
			name:    "critical dry clamps to complex",
			in:      SelectInput{Role: RoleCoder, Tier: TierCritical, Exclude: map[string]bool{"top/one": true}},
			wantID:  "high/one",
			wantMet: TierComplex,
		},
		{
			name: "two rungs dry clamps to moderate",
			in: SelectInput{Role: RoleCoder, Tier: TierCritical, Exclude: map[string]bool{
				"top/one": true, "high/one": true, "high/two": true,
			}},
			wantID:  "mid/one",
			wantMet: TierModerate,
		},
		{
			name: "three rungs dry clamps to simple",
			in: SelectInput{Role: RoleCoder, Tier: TierCritical, Exclude: map[string]bool{
				"top/one": true, "high/one": true, "high/two": true, "mid/one": true, "mid/two": true,
			}},
			wantID:  "low/one",
			wantMet: TierSimple,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.SelectByComplexity(tt.in)

			require.True(t, got.OK)
			assert.Equal(t, tt.wantID, got.Model)
			assert.Equal(t, tt.wantMet, got.MetTier)
			assert.Equal(t, tt.in.Tier, got.RequestedTier, "the request must be reported unchanged")
			assert.Equal(t, tt.wantMet == tt.in.Tier, got.AtBar())
			assert.Equal(t, SourceAuto, got.Source)
			assert.Positive(t, got.ContextWindow, "a real pick carries its catalog window")
		})
	}
}

// TestEscalationNeverDowngrades pins the escalation-is-a-downgrade
// regression: under identical exclusions, asking for a HIGHER tier must
// never return a model with a LOWER prior. Clamping down the ladder instead
// of jumping straight to the capable default is what keeps this true even
// when the requested tier's pool is empty.
func TestEscalationNeverDowngrades(t *testing.T) {
	r := ladderRegistry(nil)

	// The exclusion sets a run actually produces: the panel walk and the
	// incapable-model recovery both feed growing Exclude sets in.
	exclusionSets := []map[string]bool{
		nil,
		{"top/one": true},
		{"top/one": true, "high/one": true},
		{"top/one": true, "high/one": true, "high/two": true},
		{"top/one": true, "high/one": true, "high/two": true, "mid/one": true},
		{"top/one": true, "high/one": true, "high/two": true, "mid/one": true, "mid/two": true, "low/one": true},
	}

	ladder := []Tier{TierSimple, TierModerate, TierComplex, TierCritical}

	for i, excl := range exclusionSets {
		t.Run(fmt.Sprintf("exclusions_%d", i), func(t *testing.T) {
			for lo := range ladder {
				for hi := lo + 1; hi < len(ladder); hi++ {
					low := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: ladder[lo], Exclude: excl})
					high := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: ladder[hi], Exclude: excl})

					require.Equal(t, low.OK, high.OK,
						"a higher tier is selectable exactly when a lower one is (%s vs %s)", ladder[lo], ladder[hi])

					if !low.OK {
						continue
					}

					assert.GreaterOrEqual(t, high.Prior, low.Prior,
						"escalating %s -> %s downgraded: %s (%.2f) -> %s (%.2f)",
						ladder[lo], ladder[hi], low.Model, low.Prior, high.Model, high.Prior)
				}
			}
		})
	}
}

// TestCapableDefaultIsTheFloorAndIsHardFiltered pins that the bottom of the
// ladder is the OPERATOR's default (it is the trigger's default_model, not
// junk), but it is subject to every hard filter the same as any candidate -
// so an excluded or blacklisted default can never be handed back.
func TestCapableDefaultIsTheFloorAndIsHardFiltered(t *testing.T) {
	allAboveFloorGone := map[string]bool{
		"top/one": true, "high/one": true, "high/two": true,
		"mid/one": true, "mid/two": true, "low/one": true,
	}

	withDefaultExcluded := maps.Clone(allAboveFloorGone)
	withDefaultExcluded["capable/default"] = true

	// A tier absent from the configured ladder has bar 0 (barFor's documented
	// fallback), so every ladderRegistry model's prior would trivially clear
	// it if any reached the pool - excluding all seven is what forces the
	// walk down to the capable default at this tier too.
	everyLadderModelGone := map[string]bool{
		"top/one": true, "high/one": true, "high/two": true,
		"mid/one": true, "mid/two": true, "low/one": true, "sub/floor": true,
	}

	tests := []struct {
		name      string
		in        SelectInput
		blacklist map[string]bool
		wantOK    bool
		wantModel string
	}{
		{
			name:      "ladder dry falls to the operator default",
			in:        SelectInput{Role: RoleCoder, Tier: TierCritical, Exclude: allAboveFloorGone},
			wantOK:    true,
			wantModel: "capable/default",
		},
		{
			// The case that matters: recoverIncapable puts the default in
			// Exclude, and the default must never be handed back regardless.
			name:   "an excluded default is never resurrected",
			in:     SelectInput{Role: RoleCoder, Tier: TierCritical, Exclude: withDefaultExcluded},
			wantOK: false,
		},
		{
			name:      "a blacklisted default is never resurrected",
			in:        SelectInput{Role: RoleCoder, Tier: TierCritical, Exclude: allAboveFloorGone},
			blacklist: map[string]bool{"capable/default": true},
			wantOK:    false,
		},
		{
			// A tier with no configured bar is a degenerate rung, not a free
			// pass: the capable default's MetTier must still come from a real
			// prior, never from a prior-less model trivially clearing a bar of
			// zero at the tier that was asked for.
			name:      "an off-ladder tier still measures the default honestly",
			in:        SelectInput{Role: RoleCoder, Tier: Tier("unrecognised"), Exclude: everyLadderModelGone},
			wantOK:    true,
			wantModel: "capable/default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := ladderRegistry(nil)
			for id := range tt.blacklist {
				r.blacklist[id] = true
			}

			got := r.SelectByComplexity(tt.in)

			assert.Equal(t, tt.wantOK, got.OK)

			if !tt.wantOK {
				assert.Empty(t, got.Model, "a refusal carries no model at all")
				assert.Equal(t, tt.in.Tier, got.RequestedTier)

				return
			}

			assert.Equal(t, tt.wantModel, got.Model)
			assert.Equal(t, SourceDefault, got.Source)
			assert.False(t, got.AtBar(), "an unmeasured default meets no bar")
			assert.False(t, got.HasPrior)
			assert.Empty(t, got.MetTier, "MetTier is measured, never asserted")
		})
	}
}

// TestMetTierEqualsTheReachedRung pins the theorem the walk relies on: because
// each rung's pool is a superset of the rung above it under identical hard
// filters, a rung is reached only when nothing in it clears the next bar up. So
// the measured MetTier is exactly the rung the walk stopped at, and the two
// encodings can never disagree.
func TestMetTierEqualsTheReachedRung(t *testing.T) {
	r := ladderRegistry(nil)

	for _, excl := range []map[string]bool{
		nil,
		{"top/one": true},
		{"top/one": true, "high/one": true, "high/two": true},
	} {
		got := r.SelectByComplexity(SelectInput{Role: RoleReviewer, Tier: TierCritical, Exclude: excl})
		require.True(t, got.OK)
		require.NotEmpty(t, got.MetTier)

		at := SelectInput{Role: RoleReviewer, Tier: got.MetTier, Exclude: excl}
		assert.NotEmpty(t, r.candidates(at), "the met rung must hold the pick")
		assert.GreaterOrEqual(t, got.Prior, r.barFor(got.MetTier))

		for _, rung := range r.descent(TierCritical) {
			if r.barFor(rung) > r.barFor(got.MetTier) {
				above := SelectInput{Role: RoleReviewer, Tier: rung, Exclude: excl}
				assert.Empty(t, r.candidates(above),
					"rung %s was skipped but is not dry - the walk stopped too early", rung)
			}
		}
	}
}

func TestFavoritesAreConsultedAtTheMetRung(t *testing.T) {
	tests := []struct {
		name       string
		favorites  map[favKey][]string
		in         SelectInput
		wantID     string
		wantMet    Tier
		wantSource PickSource
	}{
		{
			// The requested tier is dry, so the pick is made on the moderate
			// rung - and a moderate selection must honour the operator's
			// moderate favorite even though the request itself was critical.
			name:      "clamped pick honours the met rung's favorite",
			favorites: map[favKey][]string{{Tier: TierModerate}: {"mid/two"}},
			in: SelectInput{Role: RoleCoder, Tier: TierCritical, Exclude: map[string]bool{
				"top/one": true, "high/one": true, "high/two": true,
			}},
			wantID: "mid/two", wantMet: TierModerate, wantSource: SourceFavorite,
		},
		{
			// mid/two (0.77) is below the complex bar and complex has a pool, so
			// the moderate favorite must not hijack it.
			name:      "lower-tier favorite never hijacks a live higher rung",
			favorites: map[favKey][]string{{Tier: TierModerate}: {"mid/two"}},
			in:        SelectInput{Role: RoleCoder, Tier: TierComplex},
			wantID:    "high/one", wantMet: TierComplex, wantSource: SourceAuto,
		},
		{
			// Even a favorite that WOULD clear the higher bar is not inherited
			// upward: top/one clears complex, but the operator configured it for
			// moderate.
			name:      "favorites are not inherited upward even when eligible",
			favorites: map[favKey][]string{{Tier: TierModerate}: {"top/one"}},
			in:        SelectInput{Role: RoleCoder, Tier: TierComplex},
			wantID:    "high/one", wantMet: TierComplex, wantSource: SourceAuto,
		},
		{
			name:      "favorites are not inherited downward",
			favorites: map[favKey][]string{{Tier: TierComplex}: {"high/one"}},
			in:        SelectInput{Role: RoleCoder, Tier: TierSimple},
			wantID:    "low/one", wantMet: TierSimple, wantSource: SourceAuto,
		},
		{
			name: "role-specific favorite beats the any-role favorite at the same tier",
			favorites: map[favKey][]string{
				{Tier: TierComplex, Role: RoleCoder}: {"high/two"},
				{Tier: TierComplex}:                  {"high/one"},
			},
			in:     SelectInput{Role: RoleCoder, Tier: TierComplex},
			wantID: "high/two", wantMet: TierComplex, wantSource: SourceFavorite,
		},
		{
			// A favorite that does not clear its own tier's bar is not eligible,
			// so the rung falls through to the cost-optimal pick.
			name:      "favorite below its own tier bar is ignored",
			favorites: map[favKey][]string{{Tier: TierComplex}: {"mid/one"}},
			in:        SelectInput{Role: RoleCoder, Tier: TierComplex},
			wantID:    "high/one", wantMet: TierComplex, wantSource: SourceAuto,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ladderRegistry(tt.favorites).SelectByComplexity(tt.in)

			require.True(t, got.OK)
			assert.Equal(t, tt.wantID, got.Model)
			assert.Equal(t, tt.wantMet, got.MetTier)
			assert.Equal(t, tt.wantSource, got.Source)
		})
	}
}

// TestMonotonicityHoldsOnTheAutoPathAndFavoritesAreTheException sweeps favorite
// configurations rather than testing only the input class where monotonicity is
// trivially true. The auto path must be monotone under every configuration; a
// violation is permitted ONLY where a favorite fired, and the test asserts that
// pairing so the exception cannot silently widen.
func TestMonotonicityHoldsOnTheAutoPathAndFavoritesAreTheException(t *testing.T) {
	favoriteSets := []map[favKey][]string{
		nil,
		{{Tier: TierModerate}: {"mid/two"}},
		{{Tier: TierComplex}: {"high/two"}},
		{{Tier: TierSimple}: {"low/one"}, {Tier: TierCritical}: {"top/one"}},
		// The counterexample the exception exists for: an expensive, strong
		// favorite at moderate that the complex band would exclude on price.
		{{Tier: TierModerate}: {"top/one"}},
	}

	ladder := []Tier{TierSimple, TierModerate, TierComplex, TierCritical}
	exclusions := []map[string]bool{nil, {"top/one": true}, {"top/one": true, "high/one": true}}

	sawException := false

	for fi, favs := range favoriteSets {
		t.Run(fmt.Sprintf("favorites_%d", fi), func(t *testing.T) {
			r := ladderRegistry(favs)

			for ei, excl := range exclusions {
				for lo := range ladder {
					for hi := lo + 1; hi < len(ladder); hi++ {
						low := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: ladder[lo], Exclude: excl})
						high := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: ladder[hi], Exclude: excl})

						if !low.OK || !high.OK {
							continue
						}

						if high.Prior >= low.Prior {
							continue
						}

						sawException = true

						assert.Equal(t, SourceFavorite, low.Source,
							"excl=%d %s(%.2f) > %s(%.2f) is only permitted when a favorite fired at the lower tier",
							ei, low.Model, low.Prior, high.Model, high.Prior)
					}
				}
			}
		})
	}

	assert.True(t, sawException,
		"fixture drift: no favorite configuration exercised the documented exception")
}
