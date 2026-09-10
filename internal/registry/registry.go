// Package registry is the agent's seam onto the shared model selector in
// contextmatrix-protocol/selection. It builds a selection.Selector from CM's
// SelectionContext (FromSelection) or from harness catalog parts in tests
// (NewRegistryFromParts), and re-exports the selection vocabulary under the
// names the orchestrator and worker have always used. The rule itself - tier
// ladders per role, descent, price band, favorites, panel seats,
// reachability - lives in the shared package so CM's admin preview and the
// agent's real pick are one implementation. What stays here is agent policy
// on top of it: the per-role ladder fallback.
package registry

import (
	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/mhersson/contextmatrix-protocol/selection"
)

// The selection vocabulary, aliased so every call site keeps its name.
type (
	Role             = selection.Role
	Tier             = selection.Tier
	PickSource       = selection.PickSource
	ModelSpec        = selection.ModelSpec
	Pick             = selection.Pick
	SelectInput      = selection.SelectInput
	SelectionReport  = selection.SelectionReport
	PoolOutcome      = selection.PoolOutcome
	PoolEntry        = selection.PoolEntry
	FilterReason     = selection.FilterReason
	FilteredOutEntry = selection.FilteredOutEntry
	SeatReport       = selection.SeatReport
	TierReach        = selection.TierReach
)

const (
	RoleCoder    = selection.RoleCoder
	RoleReviewer = selection.RoleReviewer

	TierSimple   = selection.TierSimple
	TierModerate = selection.TierModerate
	TierComplex  = selection.TierComplex
	TierCritical = selection.TierCritical

	SourceAuto     = selection.SourceAuto
	SourceFavorite = selection.SourceFavorite
	SourcePinned   = selection.SourcePinned
	SourceDefault  = selection.SourceDefault

	PoolSelected  = selection.PoolSelected
	PoolInBand    = selection.PoolInBand
	PoolOutOfBand = selection.PoolOutOfBand

	FilterPriorBelowBar  = selection.FilterPriorBelowBar
	FilterNoPrior        = selection.FilterNoPrior
	FilterExcluded       = selection.FilterExcluded
	FilterBlacklisted    = selection.FilterBlacklisted
	FilterVendorExcluded = selection.FilterVendorExcluded
	FilterWindowTooSmall = selection.FilterWindowTooSmall
)

// DistinctModels counts the distinct models across picks; a panel whose
// seats collapse onto one model is not a panel.
func DistinctModels(picks []Pick) int { return selection.DistinctModels(picks) }

// SeatPicks strips a seat list to its picks, for callers that consume a
// panel positionally.
func SeatPicks(seats []SeatReport) []Pick { return selection.SeatPicks(seats) }

// favKey indexes favorites by complexity tier and optional role, the shape
// NewRegistryFromParts has always taken. A zero Role applies the favorite
// list to every role at that tier. FromSelection never builds one: the wire
// FavoriteRule goes straight to the shared selector.
type favKey struct {
	Tier Tier
	Role Role // "" = applies to all roles
}

// Registry is the agent's handle on the shared selector. Embedding promotes
// every selection method; the adapter adds none, so a pick here and a pick
// in CM's preview are the same code path.
type Registry struct {
	*selection.Selector
}

// NewRegistryFromParts builds a registry from harness catalog parts. It is
// the test-facing constructor; production goes through FromSelection. The
// conversion to the wire shape drops entries without tool support (the
// shared selector has no tools gate because CM ships only tool-capable
// candidates) and writes a nil role prior as 0, which the shared selector
// reads as "no measured prior" - the same convention CM uses on the wire.
func NewRegistryFromParts(cat llm.Catalog, pr Priors, blacklist map[string]bool, favorites map[favKey][]string, capable string) *Registry {
	in := selection.Input{Capable: capable}

	for _, e := range cat {
		if !e.SupportsTools() {
			continue
		}

		p := pr.Models[e.ID]

		in.Candidates = append(in.Candidates, protocol.CandidateModel{
			Slug:                  e.ID,
			PromptPricePerTok:     e.PromptPricePerTok,
			CompletionPricePerTok: e.CompletionPricePerTok,
			ContextWindow:         e.ContextLength,
			CoderPrior:            priorOrZero(p.Coder),
			ReviewerPrior:         priorOrZero(p.Reviewer),
		})
	}

	for slug, on := range blacklist {
		if on {
			in.Blacklist = append(in.Blacklist, slug)
		}
	}

	for key, models := range favorites {
		in.Favorites = append(in.Favorites, protocol.FavoriteRule{
			Tier: string(key.Tier), Role: string(key.Role), Models: models,
		})
	}

	return &Registry{Selector: selection.New(in)}
}

// priorOrZero reads one role's pointer prior as the wire float: nil and 0
// both mean unmeasured to the shared selector.
func priorOrZero(p *float64) float64 {
	if p == nil {
		return 0
	}

	return *p
}
