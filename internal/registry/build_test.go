package registry

import (
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"
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
	r := FromSelection(sc, "capable/default")

	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
	if got.Model != "z-ai/glm-5.2" {
		t.Fatalf("want glm-5.2, got %q", got.Model)
	}

	if !r.blacklist["bad/model"] {
		t.Error("blacklist not applied")
	}
}

func TestFromSelectionNilReturnsCapableDefault(t *testing.T) {
	r := FromSelection(nil, "capable/default")

	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
	if got.Model != "capable/default" {
		t.Fatalf("nil selection must yield the capable default, got %q", got.Model)
	}
}
