package registry

import (
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// poolByModel indexes a report's pool entries by slug for outcome assertions.
func poolByModel(rep SelectionReport) map[string]PoolEntry {
	out := make(map[string]PoolEntry, len(rep.Pool))
	for _, e := range rep.Pool {
		out[e.Model] = e
	}

	return out
}

// filteredByReason indexes a report's filtered-out entries by reason.
func filteredByReason(rep SelectionReport) map[FilterReason][]string {
	out := make(map[FilterReason][]string, len(rep.FilteredOut))
	for _, e := range rep.FilteredOut {
		out[e.Reason] = e.Models
	}

	return out
}

func TestSelectByComplexityReportClassifiesThePool(t *testing.T) {
	// ladderRegistry at complex (bar 0.82): the pool is top/one ($15, q0.93),
	// high/one ($3, q0.85), high/two ($3.3, q0.83). Cheapest $3 -> band $4.5:
	// high/one wins, high/two is in band, top/one is priced out. Everything
	// below the bar (mid/one 0.79, mid/two 0.77, low/one 0.66, sub/floor 0.40)
	// never reaches the pool.
	r := ladderRegistry(nil)
	in := SelectInput{Role: RoleCoder, Tier: TierComplex, EstTokens: 50000}

	pick, rep := r.SelectByComplexityReport(in)

	assert.Equal(t, "high/one", pick.Model)
	assert.Equal(t, r.SelectByComplexity(in), pick, "the report variant must not change the pick")
	assert.Equal(t, TierComplex, rep.Rung)
	assert.InDelta(t, 0.82, rep.Bar, 1e-9)

	pool := poolByModel(rep)
	require.Len(t, pool, 3, "the report pool is the rung's candidates, no more")

	assert.Equal(t, PoolSelected, pool["high/one"].Outcome)
	assert.InDelta(t, 0.85, pool["high/one"].Prior, 1e-9)
	assert.InDelta(t, 3.0/1e6, pool["high/one"].Price, 1e-15)
	assert.Equal(t, PoolInBand, pool["high/two"].Outcome)
	assert.Equal(t, PoolOutOfBand, pool["top/one"].Outcome,
		"the stronger model is out of band: $15 exceeds the $4.5 band")

	filtered := filteredByReason(rep)
	require.Len(t, filtered, 1)
	assert.Equal(t, []string{"mid/one", "mid/two", "low/one", "sub/floor"}, filtered[FilterPriorBelowBar])
}

func TestSelectByComplexityReportNamesTheRungItLandedOn(t *testing.T) {
	// The complex rung is dry (every complex-clearing model excluded), so the
	// pick clamps to moderate. The report must describe the moderate rung's
	// pool - mid/one and mid/two - not the complex one, and the filtered-out
	// summary is the moderate rung's view too: low/one (0.66) sits below the
	// moderate bar while the excluded top of the ladder shows as excluded.
	r := ladderRegistry(nil)
	in := SelectInput{Role: RoleCoder, Tier: TierComplex, EstTokens: 50000, Exclude: map[string]bool{
		"top/one": true, "high/one": true, "high/two": true,
	}}

	pick, rep := r.SelectByComplexityReport(in)

	assert.Equal(t, "mid/one", pick.Model)
	assert.Equal(t, TierModerate, rep.Rung, "the report names the rung the pick was made on, not the tier asked for")
	assert.InDelta(t, 0.76, rep.Bar, 1e-9)

	pool := poolByModel(rep)
	require.Len(t, pool, 2)
	assert.Equal(t, PoolSelected, pool["mid/one"].Outcome)
	assert.Equal(t, PoolInBand, pool["mid/two"].Outcome,
		"mid/two ($1.5 -> band $2.25) is in band but loses on quality")
	assert.NotContains(t, pool, "top/one", "the dry requested rung contributes nothing to the report")

	filtered := filteredByReason(rep)
	assert.Equal(t, []string{"top/one", "high/one", "high/two"}, filtered[FilterExcluded])
	assert.Equal(t, []string{"low/one", "sub/floor"}, filtered[FilterPriorBelowBar])
}

func TestSelectByComplexityReportDefaultHasNoRungPool(t *testing.T) {
	// Every ladder model excluded: the walk falls through to the capable
	// default, which sits below the ladder and has no rung - the report is
	// empty and the pick carries the provenance.
	r := ladderRegistry(nil)
	in := SelectInput{Role: RoleCoder, Tier: TierCritical, EstTokens: 50000, Exclude: map[string]bool{
		"top/one": true, "high/one": true, "high/two": true,
		"mid/one": true, "mid/two": true, "low/one": true, "sub/floor": true,
	}}

	pick, rep := r.SelectByComplexityReport(in)

	assert.Equal(t, "capable/default", pick.Model)
	assert.Equal(t, SourceDefault, pick.Source)
	assert.Equal(t, SelectionReport{}, rep, "an off-ladder default has no rung, so no pool and no filtered-out summary")
	assert.Empty(t, rep.Rung)
}

func TestSelectByComplexityReportGrowingExcludeStaysOutOfThePool(t *testing.T) {
	// A panel-style walk: seat N calls the report variant with seats 1..N-1
	// excluded, and every earlier seat shows up in the filtered-out summary,
	// never in the pool.
	r := ladderRegistry(nil)

	exclude := map[string]bool{}

	for seat, want := range []string{"high/one", "high/two", "top/one"} {
		in := SelectInput{Role: RoleReviewer, Tier: TierComplex, EstTokens: 50000, Exclude: exclude}

		pick, rep := r.SelectByComplexityReport(in)
		require.Equal(t, want, pick.Model, "seat %d", seat+1)

		pool := poolByModel(rep)
		assert.NotContains(t, pool, exclude, "seat %d: previously seated models must not be in the pool", seat+1)

		filtered := filteredByReason(rep)
		for slug := range exclude {
			assert.Contains(t, filtered[FilterExcluded], slug,
				"seat %d: seated model %q must be attributable in the filtered-out summary", seat+1, slug)
		}

		exclude[pick.Model] = true
	}
}

func TestSelectByComplexityReportFavoriteMarksThePool(t *testing.T) {
	// The favorite is the most expensive model in the pool: it is marked
	// selected wherever it sits, and the band winner it displaced is reported
	// as in band, so the log shows what the automatic rule would have done.
	r := ladderRegistry(map[favKey][]string{{Tier: TierComplex}: {"top/one"}})
	in := SelectInput{Role: RoleCoder, Tier: TierComplex, EstTokens: 50000}

	pick, rep := r.SelectByComplexityReport(in)

	assert.Equal(t, "top/one", pick.Model)
	assert.Equal(t, SourceFavorite, pick.Source)
	assert.Equal(t, TierComplex, rep.Rung)

	pool := poolByModel(rep)
	require.Len(t, pool, 3)
	assert.Equal(t, PoolSelected, pool["top/one"].Outcome)
	assert.Equal(t, PoolInBand, pool["high/one"].Outcome,
		"the favorite displaced the band winner, which reports in band")
	assert.Equal(t, PoolInBand, pool["high/two"].Outcome)
}

func TestSelectByComplexityReportMaxCapabilityBandsNothing(t *testing.T) {
	// With MaxCapability the band is unbounded: no candidate is out of band,
	// and the highest-quality candidate wins.
	r := ladderRegistry(nil)
	r.sel.MaxCapability = true
	in := SelectInput{Role: RoleCoder, Tier: TierComplex, EstTokens: 50000}

	pick, rep := r.SelectByComplexityReport(in)

	assert.Equal(t, "top/one", pick.Model)

	for _, e := range rep.Pool {
		assert.NotEqual(t, PoolOutOfBand, e.Outcome,
			"an infinite band must not mark %s out of band", e.Model)
	}

	assert.Equal(t, PoolSelected, poolByModel(rep)["top/one"].Outcome)
	assert.Equal(t, PoolInBand, poolByModel(rep)["high/one"].Outcome)
}

func TestSelectByComplexityReportBucketingIsCompleteAndDisjoint(t *testing.T) {
	// Every catalog model lands in exactly one bucket: pool or one filtered
	// reason, and the whole catalog is accounted for.
	r := ladderRegistry(nil).
		WithCreators(map[string]string{"mid/one": "alpha", "mid/two": "alpha"})
	r.blacklist["low/one"] = true

	in := SelectInput{
		Role: RoleCoder, Tier: TierModerate, EstTokens: 15000,
		Exclude:        map[string]bool{"sub/floor": true},
		ExcludeVendors: map[string]bool{"alpha": true},
	}

	pick, rep := r.SelectByComplexityReport(in)

	assert.Equal(t, "high/one", pick.Model)

	seen := map[string]bool{}
	for _, e := range rep.Pool {
		require.False(t, seen[e.Model], "%s appears twice", e.Model)
		seen[e.Model] = true
	}

	for _, entry := range rep.FilteredOut {
		for _, slug := range entry.Models {
			require.False(t, seen[slug], "%s appears in both the pool and %s", slug, entry.Reason)
			seen[slug] = true
		}
	}

	assert.Len(t, seen, 7, "every catalog model is accounted for exactly once")

	filtered := filteredByReason(rep)
	assert.Equal(t, []string{"mid/one", "mid/two"}, filtered[FilterVendorExcluded],
		"models with a resolvable, excluded vendor are vendor-excluded")
	assert.Equal(t, []string{"low/one"}, filtered[FilterBlacklisted])
	assert.Equal(t, []string{"sub/floor"}, filtered[FilterExcluded])
	assert.NotContains(t, filtered, FilterNoPrior,
		"every catalog model carries a coder prior here, so the no-prior bucket is absent")
}

func TestSelectByComplexityReportBucketsModelsWithoutAPrior(t *testing.T) {
	// A catalog model with no prior for the role is bucketed, not silently
	// dropped from the report.
	r := NewRegistryFromParts(
		llm.Catalog{
			entry("scored/a", 1.0, 2.0, 200000),
			entry("unscored/b", 1.0, 2.0, 200000),
		},
		Priors{Models: map[string]PriorEntry{"scored/a": {Coder: new(0.80)}}},
		nil, nil, "capable-default",
	)

	pick, rep := r.SelectByComplexityReport(SelectInput{Role: RoleCoder, Tier: TierSimple, EstTokens: 50000})

	assert.Equal(t, "scored/a", pick.Model)
	assert.Equal(t, []string{"unscored/b"}, filteredByReason(rep)[FilterNoPrior])
	assert.Equal(t, []PoolEntry{{Model: "scored/a", Prior: 0.80, Price: 3.0 / 1e6, Outcome: PoolSelected}}, rep.Pool)
}
