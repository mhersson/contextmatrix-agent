package registry

import (
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
)

func TestFromSelectionBuildsCatalogPriorsAndFavorites(t *testing.T) {
	sc := &protocol.SelectionContext{
		Candidates: []protocol.CandidateModel{{
			Slug: "z-ai/glm-5.2", PromptPricePerTok: 1.2e-6, CompletionPricePerTok: 4.1e-6,
			ContextWindow: 1048576, CoderPrior: 0.90, ReviewerPrior: 0.85,
		}},
		Favorites: []protocol.FavoriteRule{{Tier: "complex", Models: []string{"z-ai/glm-5.2"}}},
		Blacklist: []string{"bad/model"},
	}
	r := FromSelection(sc, "capable/default", 0)

	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
	if got.Model != "z-ai/glm-5.2" {
		t.Fatalf("want glm-5.2, got %q", got.Model)
	}

	if !r.blacklist["bad/model"] {
		t.Error("blacklist not applied")
	}
}

func TestFromSelectionNilReturnsCapableDefault(t *testing.T) {
	r := FromSelection(nil, "capable/default", 0)

	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
	if got.Model != "capable/default" {
		t.Fatalf("nil selection must yield the capable default, got %q", got.Model)
	}
}

func TestFromSelectionThreadsPriceHeadroom(t *testing.T) {
	// premium is higher quality but priced >1.5x and <3x the cheapest, so the
	// applied headroom decides the winner: 1.5 -> cheap wins; 3.0 -> premium wins.
	sc := &protocol.SelectionContext{
		Candidates: []protocol.CandidateModel{
			{Slug: "cheap/model", PromptPricePerTok: 1, CompletionPricePerTok: 1, ContextWindow: 200000, CoderPrior: 0.80, ReviewerPrior: 0.80},
			{Slug: "premium/model", PromptPricePerTok: 2, CompletionPricePerTok: 2.5, ContextWindow: 200000, CoderPrior: 0.95, ReviewerPrior: 0.95},
		},
	}
	in := SelectInput{Role: RoleCoder, Tier: TierModerate}

	rDefault := FromSelection(sc, "capable/default", 0) // 0 -> worker default (1.5)
	assert.Equal(t, "cheap/model", rDefault.SelectByComplexity(in).Model)

	rWide := FromSelection(sc, "capable/default", 3.0)
	assert.Equal(t, "premium/model", rWide.SelectByComplexity(in).Model,
		"a non-default headroom must widen the best-value band")
}
