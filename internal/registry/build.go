package registry

import (
	"math"

	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// FromSelection builds a payload-driven Registry from CM's SelectionContext.
// All candidates are tool-capable by construction (CM filtered on it), so the
// synthesized catalog marks them so.
func FromSelection(sc *protocol.SelectionContext, capable string, priceHeadroom float64) *Registry {
	cat := make(llm.Catalog, 0)
	priors := Priors{Models: map[string]PriorEntry{}}

	if sc != nil {
		for _, c := range sc.Candidates {
			cat = append(cat, llm.CatalogEntry{
				ID:                    c.Slug,
				PromptPricePerTok:     c.PromptPricePerTok,
				CompletionPricePerTok: c.CompletionPricePerTok,
				ContextLength:         c.ContextWindow,
				SupportedParameters:   []string{"tools"},
			})
			coder, rev := c.CoderPrior, c.ReviewerPrior
			if sc.OutcomeFloor > 0 && c.Outcomes != nil && c.Outcomes.Samples >= sc.OutcomeFloor {
				coder *= outcomeBiasFactor(c.Outcomes)
			}
			priors.Models[c.Slug] = PriorEntry{Coder: &coder, Reviewer: &rev}
		}
	}

	blacklist := map[string]bool{}
	favorites := map[favKey][]string{}

	if sc != nil {
		for _, s := range sc.Blacklist {
			blacklist[s] = true
		}

		for _, fr := range sc.Favorites {
			favorites[favKey{Tier: Tier(fr.Tier), Role: Role(fr.Role)}] = fr.Models
		}
	}

	r := NewRegistryFromParts(cat, priors, blacklist, favorites, capable)
	if priceHeadroom > 0 {
		// Honor the operator's selector_price_headroom; 0 means "use the worker
		// default", which NewRegistryFromParts already set to defaultPriceHeadroom.
		r.sel.PriceHeadroom = priceHeadroom
	}

	return r
}

const (
	outcomeBiasMin = 0.8
	outcomeBiasMax = 1.2
)

// outcomeBiasFactor turns head-to-head history into a bounded multiplier on
// the coder prior: observed win-rate minus the expected baseline, clamped to
// ±20%. Self-play (wrap-around candidates) nets to exactly 1.0.
func outcomeBiasFactor(o *protocol.OutcomeStats) float64 {
	if o.Samples == 0 {
		return 1
	}

	f := 1 + (float64(o.Wins)-o.ExpectedWins)/float64(o.Samples)

	return math.Min(math.Max(f, outcomeBiasMin), outcomeBiasMax)
}
