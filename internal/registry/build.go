package registry

import (
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// FromSelection builds a payload-driven Registry from CM's SelectionContext.
// All candidates are tool-capable by construction (CM filtered on it), so the
// synthesized catalog marks them so.
func FromSelection(sc *protocol.SelectionContext, capable string) *Registry {
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

	return NewRegistryFromParts(cat, priors, blacklist, favorites, capable)
}
