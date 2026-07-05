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

func TestOutcomeBias(t *testing.T) {
	// Equal price, equal CoderPrior/ReviewerPrior (0.80) for both. A's
	// win-rate is above expected (rate = 1 + (20-10)/30 = 1.333.. -> clamped to
	// 1.2, biased coder prior 0.80*1.2 = 0.96). B's is below expected (rate =
	// 1 + (4-10)/30 = 0.8, biased coder prior 0.80*0.8 = 0.64).
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

	t.Run("floor cleared: coder prior biased, A picked over B", func(t *testing.T) {
		sc := &protocol.SelectionContext{Candidates: candidates, OutcomeFloor: 20}
		r := FromSelection(sc, "fallback/capable", 0)

		a, ok := r.priors.ForRole("model/a", RoleCoder)
		require.True(t, ok)
		assert.InDelta(t, 0.96, a, 1e-9, "0.80 * 1.2 clamped factor")

		b, ok := r.priors.ForRole("model/b", RoleCoder)
		require.True(t, ok)
		assert.InDelta(t, 0.64, b, 1e-9, "0.80 * 0.8 clamped factor")

		got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierSimple})
		assert.Equal(t, "model/a", got.Model)
	})

	t.Run("below floor: both unbiased, deterministic tie-break unchanged", func(t *testing.T) {
		sc := &protocol.SelectionContext{Candidates: candidates, OutcomeFloor: 40}
		r := FromSelection(sc, "fallback/capable", 0)

		a, ok := r.priors.ForRole("model/a", RoleCoder)
		require.True(t, ok)
		assert.InDelta(t, 0.80, a, 1e-9, "below floor: coder prior untouched")

		b, ok := r.priors.ForRole("model/b", RoleCoder)
		require.True(t, ok)
		assert.InDelta(t, 0.80, b, 1e-9, "below floor: coder prior untouched")

		got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierSimple})
		assert.Equal(t, "model/a", got.Model, "tie resolves to the first-listed candidate, same as today")
	})

	t.Run("floor zero disables biasing entirely", func(t *testing.T) {
		sc := &protocol.SelectionContext{Candidates: candidates, OutcomeFloor: 0}
		r := FromSelection(sc, "fallback/capable", 0)

		a, ok := r.priors.ForRole("model/a", RoleCoder)
		require.True(t, ok)
		assert.InDelta(t, 0.80, a, 1e-9)
	})

	t.Run("nil Outcomes untouched even with floor set", func(t *testing.T) {
		sc := &protocol.SelectionContext{
			Candidates: []protocol.CandidateModel{
				{Slug: "model/c", CoderPrior: 0.80, ReviewerPrior: 0.80},
			},
			OutcomeFloor: 1,
		}
		r := FromSelection(sc, "fallback/capable", 0)

		c, ok := r.priors.ForRole("model/c", RoleCoder)
		require.True(t, ok)
		assert.InDelta(t, 0.80, c, 1e-9, "nil Outcomes must never be biased")
	})

	t.Run("reviewer prior never biased", func(t *testing.T) {
		sc := &protocol.SelectionContext{Candidates: candidates, OutcomeFloor: 20}
		r := FromSelection(sc, "fallback/capable", 0)

		a, ok := r.priors.ForRole("model/a", RoleReviewer)
		require.True(t, ok)
		assert.InDelta(t, 0.80, a, 1e-9, "reviewer prior must never be biased")

		b, ok := r.priors.ForRole("model/b", RoleReviewer)
		require.True(t, ok)
		assert.InDelta(t, 0.80, b, 1e-9, "reviewer prior must never be biased")

		got := r.SelectByComplexity(SelectInput{Role: RoleReviewer, Tier: TierSimple})
		assert.Equal(t, "model/a", got.Model, "equal reviewer priors + price -> first-listed wins, unaffected by outcomes")
	})
}

func TestOutcomeBiasFactorClamps(t *testing.T) {
	tests := []struct {
		name string
		o    *protocol.OutcomeStats
		want float64
	}{
		{"zero samples is neutral", &protocol.OutcomeStats{Samples: 0, Wins: 0, ExpectedWins: 0}, 1.0},
		{"self-play (observed == expected) nets to 1.0", &protocol.OutcomeStats{Samples: 30, Wins: 10, ExpectedWins: 10}, 1.0},
		{"large excess wins clamps to max", &protocol.OutcomeStats{Samples: 10, Wins: 10, ExpectedWins: 1}, outcomeBiasMax},
		{"large deficit clamps to min", &protocol.OutcomeStats{Samples: 10, Wins: 0, ExpectedWins: 9}, outcomeBiasMin},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.InDelta(t, tt.want, outcomeBiasFactor(tt.o), 1e-9)
		})
	}
}
