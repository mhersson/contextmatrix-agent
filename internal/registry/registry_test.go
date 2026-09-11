package registry

import (
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// entry builds a tool-capable CatalogEntry from prices given in dollars per
// million tokens, converting to the per-token units the catalog stores.
func entry(id string, promptPerM, completionPerM float64, window int) llm.CatalogEntry {
	return llm.CatalogEntry{
		ID:                    id,
		PromptPricePerTok:     promptPerM / 1e6,
		CompletionPricePerTok: completionPerM / 1e6,
		ContextLength:         window,
		SupportedParameters:   []string{"tools"},
	}
}

// filteredByReason indexes a report's filtered-out entries by reason.
func filteredByReason(rep SelectionReport) map[FilterReason][]string {
	out := make(map[FilterReason][]string, len(rep.FilteredOut))
	for _, e := range rep.FilteredOut {
		out[e.Reason] = e.Models
	}

	return out
}

func TestNewRegistryFromPartsDropsEntriesWithoutToolSupport(t *testing.T) {
	// The shared selector has no tools gate because CM ships only
	// tool-capable candidates. A harness catalog can still describe a model
	// without tool support, and such a model must stay out of the selector
	// entirely: never picked, and not resolvable as a pin.
	cat := llm.Catalog{
		entry("tools/yes", 1, 1, 200000),
		{ID: "tools/no", ContextLength: 200000, SupportedParameters: []string{}},
	}
	pr := Priors{Models: map[string]PriorEntry{
		"tools/yes": {Coder: new(0.9), Reviewer: new(0.9)},
		"tools/no":  {Coder: new(0.9), Reviewer: new(0.9)},
	}}
	r := NewRegistryFromParts(cat, pr, nil, nil, "capable/default")

	assert.True(t, r.Has("tools/yes"))
	assert.Equal(t, 200000, r.ContextWindow("tools/yes"))
	assert.False(t, r.Has("tools/no"), "a tools-less entry is not a candidate")
	assert.Equal(t, 0, r.ContextWindow("tools/no"))

	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierSimple})
	assert.Equal(t, "tools/yes", got.Model)

	got = r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierSimple, Exclude: map[string]bool{"tools/yes": true}})
	assert.Equal(t, "capable/default", got.Model, "tools/no is free but must never be selected")
	assert.Equal(t, SourceDefault, got.Source)
}

func TestNewRegistryFromPartsNilRolePriorIsUnmeasured(t *testing.T) {
	// The wire shape has a float per role and reads 0 as "no measured prior",
	// so the adapter writes a nil PriorEntry role as 0 and nothing more. The
	// model is never selected for that role, is bucketed no-prior-for-role,
	// and a pick of it reports HasPrior false - the shortfall advisory prints
	// "no measured prior" on the strength of that flag.
	cat := llm.Catalog{
		entry("only/coder", 1, 1, 200000),
		entry("both/roles", 2, 2, 200000),
	}
	pr := Priors{Models: map[string]PriorEntry{
		"only/coder": {Coder: new(0.9)},
		"both/roles": {Coder: new(0.9), Reviewer: new(0.9)},
	}}
	r := NewRegistryFromParts(cat, pr, nil, nil, "capable/default")

	in := SelectInput{Role: RoleReviewer, Tier: TierSimple}

	got, rep := r.SelectByComplexityReport(in)
	assert.Equal(t, "both/roles", got.Model, "only/coder is cheaper but carries no reviewer prior")
	assert.True(t, got.HasPrior)
	assert.InDelta(t, 0.9, got.Prior, 1e-9)
	assert.Equal(t, []string{"only/coder"}, filteredByReason(rep)[FilterNoPrior])

	in.Exclude = map[string]bool{"both/roles": true}
	got = r.SelectByComplexity(in)
	assert.Equal(t, "capable/default", got.Model, "with the only scored reviewer excluded the walk falls to the default")
	assert.False(t, got.HasPrior)

	// A pin is measured, not asserted: pinning the unscored model as a
	// reviewer reports no prior and clears no tier.
	seats := r.SelectCandidateModelsReport(SelectInput{Role: RoleReviewer, Tier: TierSimple}, 1, "only/coder")
	require.Len(t, seats, 1)
	assert.False(t, seats[0].Pick.HasPrior, "a nil prior must not come back as a measured 0")
	assert.InDelta(t, 0.0, seats[0].Pick.Prior, 1e-9)
	assert.Empty(t, seats[0].Pick.MetTier)

	// The coder reading of the same model is untouched.
	got = r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierSimple})
	assert.Equal(t, "only/coder", got.Model)
	assert.True(t, got.HasPrior)
	assert.InDelta(t, 0.9, got.Prior, 1e-9)
}

func TestNewRegistryFromPartsThreadsBlacklistFavoritesAndExclusions(t *testing.T) {
	// The three inputs the orchestrator relies on all reach the shared
	// selector through the conversion: a true blacklist entry is applied and
	// a false one is not, a favorite wins at its own tier, and the per-call
	// Exclude set is honoured and reported.
	cat := llm.Catalog{
		entry("cheap/blacklisted", 1, 1, 200000),
		entry("fav/model", 3, 3, 200000),
		entry("value/model", 2, 2, 200000),
	}
	pr := Priors{Models: map[string]PriorEntry{
		"cheap/blacklisted": {Coder: new(0.95)},
		"fav/model":         {Coder: new(0.80)},
		"value/model":       {Coder: new(0.90)},
	}}
	blacklist := map[string]bool{"cheap/blacklisted": true, "value/model": false}
	favorites := map[favKey][]string{{Tier: TierModerate, Role: RoleCoder}: {"fav/model"}}
	r := NewRegistryFromParts(cat, pr, blacklist, favorites, "capable/default")

	pick, rep := r.SelectByComplexityReport(SelectInput{Role: RoleCoder, Tier: TierModerate})
	assert.Equal(t, "fav/model", pick.Model, "the favorite wins at its tier")
	assert.Equal(t, SourceFavorite, pick.Source)
	assert.Equal(t, []string{"cheap/blacklisted"}, filteredByReason(rep)[FilterBlacklisted])

	// At complex the favorite is below the bar and value/model is excluded,
	// so the walk lands on moderate, where the favorite applies again.
	pick, rep = r.SelectByComplexityReport(SelectInput{Role: RoleCoder, Tier: TierComplex, Exclude: map[string]bool{"value/model": true}})
	assert.Equal(t, "fav/model", pick.Model)
	assert.Equal(t, TierModerate, pick.MetTier)
	assert.Equal(t, TierModerate, rep.Rung)
	assert.Equal(t, []string{"value/model"}, filteredByReason(rep)[FilterExcluded], "the per-call exclude set reaches the shared selector")
}

func TestNewRegistryFromPartsVendorFallsBackToTheSlugPrefix(t *testing.T) {
	// The test constructor carries no creators (only the payload does), so
	// the vendor-diversity preference reads the namespace prefix.
	r := NewRegistryFromParts(llm.Catalog{entry("z-ai/glm", 1, 1, 200000), entry("bare-slug", 1, 1, 200000)}, Priors{}, nil, nil, "x")

	assert.Equal(t, "z-ai", r.Vendor("z-ai/glm"))
	assert.Empty(t, r.Vendor("bare-slug"))
}
