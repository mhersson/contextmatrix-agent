package registry

import (
	"errors"
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// twoModels is a two-candidate payload where the price band decides at the
// built-in complex bars (coder 0.84, reviewer 0.90): both clear them, glm is
// cheapest at 5.8e-6 per token, sol at 1.2e-5 is outside the 1.5x band, so
// glm wins for both roles.
func twoModels() *protocol.SelectionContext {
	return &protocol.SelectionContext{
		Candidates: []protocol.CandidateModel{
			{Slug: "z-ai/glm-5.3", PromptPricePerTok: 2.9e-6, CompletionPricePerTok: 2.9e-6, ContextWindow: 200000, CoderPrior: 0.917, ReviewerPrior: 0.91},
			{Slug: "openai/gpt-5.6-sol", PromptPricePerTok: 6e-6, CompletionPricePerTok: 6e-6, ContextWindow: 400000, CoderPrior: 0.949, ReviewerPrior: 0.95},
		},
	}
}

func TestFromSelectionBuildsCandidatesPriorsAndFavorites(t *testing.T) {
	sc := &protocol.SelectionContext{
		Candidates: []protocol.CandidateModel{{
			Slug: "z-ai/glm-5.2", PromptPricePerTok: 1.2e-6, CompletionPricePerTok: 4.1e-6,
			ContextWindow: 1048576, CoderPrior: 0.90, ReviewerPrior: 0.85,
		}},
		Favorites: []protocol.FavoriteRule{{Tier: "complex", Models: []string{"z-ai/glm-5.2"}}},
		Blacklist: []string{"bad/model"},
	}
	r, faults := FromSelection(sc, "capable/default", false)
	require.Empty(t, faults)

	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
	assert.Equal(t, "z-ai/glm-5.2", got.Model)
	assert.Equal(t, SourceFavorite, got.Source)
	assert.True(t, got.HasPrior)
	assert.InDelta(t, 0.90, got.Prior, 1e-9)
	assert.Equal(t, 1048576, r.ContextWindow("z-ai/glm-5.2"))
}

func TestFromSelectionNilReturnsCapableDefault(t *testing.T) {
	r, faults := FromSelection(nil, "capable/default", false)
	require.Empty(t, faults)

	got := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
	assert.Equal(t, "capable/default", got.Model, "nil selection must yield the capable default")
	assert.False(t, got.HasPrior, "the default sits outside the candidate set and has no prior")
}

func TestFromSelectionAppliesThePayloadHeadroom(t *testing.T) {
	// premium is higher quality but priced >1.5x and <3x the cheapest, so the
	// headroom on the wire decides the winner: absent -> cheap wins; 3.0 ->
	// premium wins.
	sc := &protocol.SelectionContext{
		Candidates: []protocol.CandidateModel{
			{Slug: "cheap/model", PromptPricePerTok: 1, CompletionPricePerTok: 1, ContextWindow: 200000, CoderPrior: 0.80, ReviewerPrior: 0.80},
			{Slug: "premium/model", PromptPricePerTok: 2, CompletionPricePerTok: 2.5, ContextWindow: 200000, CoderPrior: 0.95, ReviewerPrior: 0.95},
		},
	}
	in := SelectInput{Role: RoleCoder, Tier: TierModerate}

	rDefault, _ := FromSelection(sc, "capable/default", false)
	assert.Equal(t, "cheap/model", rDefault.SelectByComplexity(in).Model, "no headroom on the wire is the built-in 1.5")

	sc.PriceHeadroom = 3.0

	rWide, _ := FromSelection(sc, "capable/default", false)
	assert.Equal(t, "premium/model", rWide.SelectByComplexity(in).Model,
		"the payload headroom must widen the best-value band")
}

func TestFromSelectionThreadsMaxCapability(t *testing.T) {
	sc := &protocol.SelectionContext{
		Candidates: []protocol.CandidateModel{
			{Slug: "cheap/model", PromptPricePerTok: 1, CompletionPricePerTok: 1, ContextWindow: 200000, CoderPrior: 0.80, ReviewerPrior: 0.80},
			{Slug: "premium/model", PromptPricePerTok: 2, CompletionPricePerTok: 2.5, ContextWindow: 200000, CoderPrior: 0.95, ReviewerPrior: 0.95},
		},
	}
	in := SelectInput{Role: RoleCoder, Tier: TierModerate}

	rDefault, _ := FromSelection(sc, "capable/default", false)
	assert.Equal(t, "cheap/model", rDefault.SelectByComplexity(in).Model, "default must pick the cheaper model")

	rMax, _ := FromSelection(sc, "capable/default", true)
	assert.Equal(t, "premium/model", rMax.SelectByComplexity(in).Model,
		"maxCapability=true must pick the premium (more capable) model regardless of price")
}

func TestFromSelectionThreadsCreators(t *testing.T) {
	// An OpenAI-endpoint payload (bare slugs, creators supplied by CM) must
	// come out of FromSelection with the vendor-diversity preference live in
	// the discussion panel.
	sc := &protocol.SelectionContext{
		Candidates: []protocol.CandidateModel{
			{Slug: "gpt-a", PromptPricePerTok: 1e-6, CompletionPricePerTok: 2e-6, ContextWindow: 200000, ReviewerPrior: 0.95, Creator: "openai"},
			{Slug: "gpt-b", PromptPricePerTok: 1e-6, CompletionPricePerTok: 2e-6, ContextWindow: 200000, ReviewerPrior: 0.90, Creator: "openai"},
			{Slug: "gpt-c", PromptPricePerTok: 1e-6, CompletionPricePerTok: 2e-6, ContextWindow: 200000, ReviewerPrior: 0.88, Creator: "openai"},
			{Slug: "claude-x", PromptPricePerTok: 1e-6, CompletionPricePerTok: 2e-6, ContextWindow: 200000, ReviewerPrior: 0.85, Creator: "anthropic"},
		},
		// The fixture reviewer ladder keeps every candidate in the panel.
		TierBars: map[string]map[string]float64{"reviewer": {"simple": 0.65, "moderate": 0.76, "complex": 0.82, "critical": 0.90}},
	}
	r, _ := FromSelection(sc, "capable-default", false)

	panel := SeatPicks(r.SelectDiscussionPanelReport(SelectInput{Role: RoleReviewer, Tier: TierComplex, EstTokens: 50000}, 3))
	require.Len(t, panel, 3)
	assert.Equal(t, "gpt-a", panel[0].Model)
	assert.Equal(t, "claude-x", panel[1].Model, "creators from the payload must drive vendor diversity")
	assert.Equal(t, "gpt-b", panel[2].Model)

	// Without creators (older CM, bare slugs) the walk stays vendor-blind.
	for i := range sc.Candidates {
		sc.Candidates[i].Creator = ""
	}

	rBlind, _ := FromSelection(sc, "capable-default", false)

	panel = SeatPicks(rBlind.SelectDiscussionPanelReport(SelectInput{Role: RoleReviewer, Tier: TierComplex, EstTokens: 50000}, 3))
	require.Len(t, panel, 3)
	assert.Equal(t, "gpt-a", panel[0].Model)
	assert.Equal(t, "gpt-b", panel[1].Model)
	assert.Equal(t, "gpt-c", panel[2].Model)
}

func TestFromSelectionAppliesPayloadLaddersPerRole(t *testing.T) {
	// Raising the coder complex bar above glm's 0.917 leaves sol alone at
	// that rung for coders; the reviewer ladder is not on the wire, so the
	// same request as a reviewer still resolves at the built-in bar, where
	// the price band picks glm. The two roles now answer differently.
	sc := twoModels()
	sc.TierBars = map[string]map[string]float64{"coder": {"complex": 0.93, "critical": 0.95}}

	r, faults := FromSelection(sc, "capable/default", false)
	require.Empty(t, faults)

	assert.InDelta(t, 0.93, r.BarFor(RoleCoder, TierComplex), 1e-9)
	assert.InDelta(t, 0.90, r.BarFor(RoleReviewer, TierComplex), 1e-9, "a role not on the wire keeps its built-in bars")

	coder := r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex})
	assert.Equal(t, "openai/gpt-5.6-sol", coder.Model)
	assert.True(t, coder.AtBar())

	reviewer := r.SelectByComplexity(SelectInput{Role: RoleReviewer, Tier: TierComplex})
	assert.Equal(t, "z-ai/glm-5.3", reviewer.Model)
	assert.True(t, reviewer.AtBar())

	rDefault, _ := FromSelection(twoModels(), "capable/default", false)
	assert.Equal(t, "z-ai/glm-5.3", rDefault.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex}).Model,
		"without a ladder the coder pick is the band winner")
}

func TestFromSelectionFallsBackOnlyTheRoleWhoseLadderFailed(t *testing.T) {
	// The reviewer ladder is non-monotone (complex 0.5 below moderate 0.90)
	// and must fall back to the reviewer built-in bars; the coder ladder validates
	// and must still apply. The registry is usable either way.
	sc := twoModels()
	sc.TierBars = map[string]map[string]float64{
		"coder":    {"complex": 0.93, "critical": 0.95},
		"reviewer": {"complex": 0.5},
	}

	r, faults := FromSelection(sc, "capable/default", false)
	require.Len(t, faults, 1)
	assert.Equal(t, "reviewer", faults[0].Role)

	assert.InDelta(t, 0.93, r.BarFor(RoleCoder, TierComplex), 1e-9, "the valid coder ladder is applied")
	assert.InDelta(t, 0.90, r.BarFor(RoleReviewer, TierComplex), 1e-9, "the failed reviewer ladder is the reviewer built-in bars")
	assert.InDelta(t, 0.92, r.BarFor(RoleReviewer, TierCritical), 1e-9)
	assert.Equal(t, "openai/gpt-5.6-sol", r.SelectByComplexity(SelectInput{Role: RoleCoder, Tier: TierComplex}).Model)
	assert.Equal(t, "z-ai/glm-5.3", r.SelectByComplexity(SelectInput{Role: RoleReviewer, Tier: TierComplex}).Model)
}

func TestLaddersFromPayload(t *testing.T) {
	t.Run("both roles validate", func(t *testing.T) {
		got, faults := laddersFromPayload(map[string]map[string]float64{
			"coder":    {"complex": 0.9},
			"reviewer": {"critical": 0.93},
		})
		require.Empty(t, faults)
		assert.InDelta(t, 0.9, got[RoleCoder][TierComplex], 1e-9)
		assert.InDelta(t, 0.75, got[RoleCoder][TierModerate], 1e-9, "a partial ladder merges over the built-in bars")
		assert.InDelta(t, 0.93, got[RoleReviewer][TierCritical], 1e-9)
		assert.InDelta(t, 0.90, got[RoleReviewer][TierComplex], 1e-9)
	})

	t.Run("one bad role falls back alone", func(t *testing.T) {
		got, faults := laddersFromPayload(map[string]map[string]float64{
			"coder":    {"complex": 0.9},
			"reviewer": {"complex": 0.5},
		})
		require.Len(t, faults, 1)
		assert.Equal(t, "reviewer", faults[0].Role)
		assert.InDelta(t, 0.9, got[RoleCoder][TierComplex], 1e-9)

		_, present := got[RoleReviewer]
		assert.False(t, present, "a failed role is absent, which the selector reads as the built-in bars")
	})

	t.Run("unknown role is a fault and the rest applies", func(t *testing.T) {
		got, faults := laddersFromPayload(map[string]map[string]float64{
			"judge": {"complex": 0.9},
			"coder": {"complex": 0.9},
		})
		require.Len(t, faults, 1)
		assert.Equal(t, "judge", faults[0].Role)
		assert.Contains(t, faults[0].Reason.Error(), "unknown role")
		assert.InDelta(t, 0.9, got[RoleCoder][TierComplex], 1e-9)
	})

	t.Run("faults are ordered by role name", func(t *testing.T) {
		_, faults := laddersFromPayload(map[string]map[string]float64{
			"reviewer": {"complex": 0.5},
			"coder":    {"complex": 1.5},
		})
		require.Len(t, faults, 2)
		assert.Equal(t, "coder", faults[0].Role)
		assert.Equal(t, "reviewer", faults[1].Role)
	})

	t.Run("empty input is nil, nil", func(t *testing.T) {
		got, faults := laddersFromPayload(nil)
		assert.Nil(t, got)
		assert.Nil(t, faults)

		got, faults = laddersFromPayload(map[string]map[string]float64{})
		assert.Nil(t, got)
		assert.Nil(t, faults)
	})

	t.Run("a role with an empty map is the built-in bars, not a fault", func(t *testing.T) {
		got, faults := laddersFromPayload(map[string]map[string]float64{"coder": {}})
		assert.Nil(t, faults)
		assert.Nil(t, got, "nothing configured means nil ladders")
		assert.InDelta(t, 0.84, got.Bars(RoleCoder)[TierComplex], 1e-9, "nil ladders read as the built-in bars")
	})
}

func TestFromSelectionMergesEachRoleOverItsOwnBuiltIn(t *testing.T) {
	sc := twoModels()
	sc.TierBars = map[string]map[string]float64{"coder": {"critical": 0.96}, "reviewer": {"critical": 0.95}}

	r, faults := FromSelection(sc, "capable/default", false)
	require.Empty(t, faults)

	assert.InDelta(t, 0.84, r.BarFor(RoleCoder, TierComplex), 1e-9)
	assert.InDelta(t, 0.96, r.BarFor(RoleCoder, TierCritical), 1e-9)
	assert.InDelta(t, 0.90, r.BarFor(RoleReviewer, TierComplex), 1e-9)
	assert.InDelta(t, 0.95, r.BarFor(RoleReviewer, TierCritical), 1e-9)
}

func TestFromSelectionValidatesEachRoleAgainstItsOwnBuiltIn(t *testing.T) {
	// simple 0.85 sits above the coder built-in moderate 0.75 (invalid) and
	// below the reviewer built-in moderate 0.90 (valid).
	sc := twoModels()
	sc.TierBars = map[string]map[string]float64{"coder": {"simple": 0.85}, "reviewer": {"simple": 0.85}}

	r, faults := FromSelection(sc, "capable/default", false)
	require.Len(t, faults, 1)
	assert.Equal(t, "coder", faults[0].Role)

	assert.Equal(t, map[Tier]float64{TierSimple: 0.65, TierModerate: 0.75, TierComplex: 0.84, TierCritical: 0.94}, r.Bars(RoleCoder))
	assert.Equal(t, map[Tier]float64{TierSimple: 0.85, TierModerate: 0.90, TierComplex: 0.90, TierCritical: 0.92}, r.Bars(RoleReviewer))
}

func TestLadderFaultErrorNamesRoleAndReason(t *testing.T) {
	f := LadderFault{Role: "coder", Reason: errors.New("boom")}
	assert.Equal(t, "coder tier ladder from the payload did not validate (boom)", f.Error())
}
