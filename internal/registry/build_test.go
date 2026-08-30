package registry

import (
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	r := FromSelection(sc, "capable/default", 0, false)

	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
	if got.Model != "z-ai/glm-5.2" {
		t.Fatalf("want glm-5.2, got %q", got.Model)
	}

	if !r.blacklist["bad/model"] {
		t.Error("blacklist not applied")
	}
}

func TestFromSelectionNilReturnsCapableDefault(t *testing.T) {
	r := FromSelection(nil, "capable/default", 0, false)

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

	rDefault := FromSelection(sc, "capable/default", 0, false) // 0 -> worker default (1.5)
	assert.Equal(t, "cheap/model", rDefault.SelectByComplexity(in).Model)

	rWide := FromSelection(sc, "capable/default", 3.0, false)
	assert.Equal(t, "premium/model", rWide.SelectByComplexity(in).Model,
		"a non-default headroom must widen the best-value band")
}

func TestFromSelectionThreadsMaxCapability(t *testing.T) {
	// With the default headroom (1.5x) cheap wins. With maxCapability=true,
	// the expensive high-quality model wins regardless of price.
	sc := &protocol.SelectionContext{
		Candidates: []protocol.CandidateModel{
			{Slug: "cheap/model", PromptPricePerTok: 1, CompletionPricePerTok: 1, ContextWindow: 200000, CoderPrior: 0.80, ReviewerPrior: 0.80},
			{Slug: "premium/model", PromptPricePerTok: 2, CompletionPricePerTok: 2.5, ContextWindow: 200000, CoderPrior: 0.95, ReviewerPrior: 0.95},
		},
	}
	in := SelectInput{Role: RoleCoder, Tier: TierModerate}

	rDefault := FromSelection(sc, "capable/default", 0, false)
	assert.Equal(t, "cheap/model", rDefault.SelectByComplexity(in).Model,
		"default must pick the cheaper model")

	rMax := FromSelection(sc, "capable/default", 0, true)
	assert.Equal(t, "premium/model", rMax.SelectByComplexity(in).Model,
		"maxCapability=true must pick the premium (more capable) model regardless of price")
}

// TestFromSelectionIgnoresOutcomeStats pins the priors-only contract: even
// when a payload arrives carrying per-candidate outcome stats and an outcome
// floor (an older CM), the priors pass through untouched - recorded outcomes
// never bias a pick.
func TestFromSelectionIgnoresOutcomeStats(t *testing.T) {
	candidates := []protocol.CandidateModel{
		{
			Slug: "model/a", PromptPricePerTok: 1e-6, CompletionPricePerTok: 1e-6,
			ContextWindow: 200000, CoderPrior: 0.80, ReviewerPrior: 0.80,
			Outcomes: &protocol.OutcomeStats{Samples: 30, Wins: 20, ExpectedWins: 10},
		},
		{
			Slug: "model/b", PromptPricePerTok: 1e-6, CompletionPricePerTok: 1e-6,
			ContextWindow: 200000, CoderPrior: 0.80, ReviewerPrior: 0.80,
			Outcomes: &protocol.OutcomeStats{Samples: 30, Wins: 4, ExpectedWins: 10},
		},
	}
	sc := &protocol.SelectionContext{Candidates: candidates, OutcomeFloor: 20}
	r := FromSelection(sc, "fallback/capable", 0, false)

	a, ok := r.priors.ForRole("model/a", RoleCoder)
	require.True(t, ok)
	assert.InDelta(t, 0.80, a, 1e-9, "coder prior must pass through unbiased")

	b, ok := r.priors.ForRole("model/b", RoleCoder)
	require.True(t, ok)
	assert.InDelta(t, 0.80, b, 1e-9, "coder prior must pass through unbiased")
}

func TestFromSelectionThreadsCreators(t *testing.T) {
	// The incident scenario end-to-end: an OpenAI-endpoint payload (bare
	// slugs, creators supplied by CM) must come out of FromSelection with the
	// vendor-diversity preference live in the discussion panel.
	sc := &protocol.SelectionContext{
		Candidates: []protocol.CandidateModel{
			{Slug: "gpt-a", PromptPricePerTok: 1e-6, CompletionPricePerTok: 2e-6, ContextWindow: 200000, ReviewerPrior: 0.95, Creator: "openai"},
			{Slug: "gpt-b", PromptPricePerTok: 1e-6, CompletionPricePerTok: 2e-6, ContextWindow: 200000, ReviewerPrior: 0.90, Creator: "openai"},
			{Slug: "gpt-c", PromptPricePerTok: 1e-6, CompletionPricePerTok: 2e-6, ContextWindow: 200000, ReviewerPrior: 0.88, Creator: "openai"},
			{Slug: "claude-x", PromptPricePerTok: 1e-6, CompletionPricePerTok: 2e-6, ContextWindow: 200000, ReviewerPrior: 0.85, Creator: "anthropic"},
		},
	}
	r := FromSelection(sc, "capable-default", 0, false)

	panel := r.SelectDiscussionPanel(SelectInput{Role: RoleReviewer, Tier: TierComplex, EstTokens: 50000}, 3)
	require.Len(t, panel, 3)
	assert.Equal(t, "gpt-a", panel[0].Model)
	assert.Equal(t, "claude-x", panel[1].Model, "creators from the payload must drive vendor diversity")
	assert.Equal(t, "gpt-b", panel[2].Model)

	// Without creators (older CM, bare slugs) the walk stays vendor-blind.
	for i := range sc.Candidates {
		sc.Candidates[i].Creator = ""
	}

	rBlind := FromSelection(sc, "capable-default", 0, false)

	panel = rBlind.SelectDiscussionPanel(SelectInput{Role: RoleReviewer, Tier: TierComplex, EstTokens: 50000}, 3)
	require.Len(t, panel, 3)
	assert.Equal(t, "gpt-a", panel[0].Model)
	assert.Equal(t, "gpt-b", panel[1].Model)
	assert.Equal(t, "gpt-c", panel[2].Model)
}
