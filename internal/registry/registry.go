// Package registry resolves roles to concrete model specs for the harness. It
// is caller-facing: the FSM-free harness loop never imports it. SelectByComplexity
// is the best-value selector: a normalized external prior carries the tier bar,
// blacklisted slugs are excluded, and eligible favorites win before the
// cost-optimal pick.
package registry

import (
	"strings"

	"github.com/mhersson/contextmatrix-harness/llm"
)

type Role string

const (
	RoleCoder    Role = "coder"
	RoleReviewer Role = "reviewer"
)

type Tier string

const (
	TierSimple   Tier = "simple"
	TierModerate Tier = "moderate"
	TierComplex  Tier = "complex"
	TierCritical Tier = "critical"
)

// DefaultTierBars are the normalized-prior thresholds per complexity tier.
func DefaultTierBars() map[Tier]float64 {
	return map[Tier]float64{
		TierSimple:   0.65,
		TierModerate: 0.76,
		TierComplex:  0.82,
		TierCritical: 0.90,
	}
}

// ModelSpec is what the caller feeds into harness.Config for a given role.
type ModelSpec struct {
	Model         string
	ContextWindow int // from the catalog; 0 if unknown
}

// favKey indexes operator-pinned favorites by complexity tier and (optionally)
// role. A zero Role applies the favorite list to every role at that tier.
type favKey struct {
	Tier Tier
	Role Role // "" = applies to all roles
}

// Registry maps roles to models, backed by the live catalog for window/price.
type Registry struct {
	capable   string
	catalog   llm.Catalog
	priors    Priors
	blacklist map[string]bool
	favorites map[favKey][]string
	creators  map[string]string // slug -> creator, behind the vendor-diversity preference
	sel       Selection         // selection config (price headroom, tier bars)
}

// Selection configures the best-value selector. Zero value is valid: headroom
// defaults to 1.5.
type Selection struct {
	PriceHeadroom float64 // <= 0 -> defaultPriceHeadroom
}

const defaultPriceHeadroom = 1.5

// NewRegistryFromParts builds a payload-driven registry: the live catalog plus
// CM-injected priors, blacklist, and favorites. Quality is the normalized prior
// only; there is no measured-capability gate.
func NewRegistryFromParts(cat llm.Catalog, pr Priors, blacklist map[string]bool, favorites map[favKey][]string, capable string) *Registry {
	if blacklist == nil {
		blacklist = map[string]bool{}
	}

	if favorites == nil {
		favorites = map[favKey][]string{}
	}

	return &Registry{
		capable:   capable,
		catalog:   cat,
		priors:    pr,
		blacklist: blacklist,
		favorites: favorites,
		sel:       Selection{PriceHeadroom: defaultPriceHeadroom},
	}
}

// SelectInput describes a single best-value selection request.
type SelectInput struct {
	Role      Role
	Tier      Tier
	EstTokens int             // window-fit estimate; 0 skips the window check
	Exclude   map[string]bool // diversity: models to avoid if alternatives exist
	// ExcludeVendors is a hard filter in candidates(); the panel walk applies
	// it softly (vendor-filtered attempt first, retry without on an empty pool).
	ExcludeVendors map[string]bool
}

// NewRegistry builds a priors-only registry with the given capable default.
// Selection is payload-driven: with no priors injected, SelectByComplexity
// always falls back to the capable default.
func NewRegistry(capableDefault string, catalog llm.Catalog) *Registry {
	return NewRegistryFromParts(catalog, Priors{}, nil, nil, capableDefault)
}

// WithCreators attaches the slug -> creator map behind the vendor-diversity
// preference. nil disables vendor tracking. Returns r for chaining.
func (r *Registry) WithCreators(creators map[string]string) *Registry {
	r.creators = creators

	return r
}

// vendorOf resolves a model's vendor: the CM-supplied creator first, else the
// namespace prefix of a namespaced slug (OpenRouter-leg fallback for CMs that
// predate CandidateModel.Creator), else "". The two vocabularies (AA creator
// slugs like "zai" vs OR prefixes like "z-ai") never mix within one run: the
// fallback only fires when CM sent no creator for the slug.
func (r *Registry) vendorOf(id string) string {
	if v := r.creators[id]; v != "" {
		return v
	}

	if vendor, _, ok := strings.Cut(id, "/"); ok && vendor != "" {
		return vendor
	}

	return ""
}

// Has reports whether model is present in the live catalog. The orchestrator
// uses it to decide whether a card-pinned model slug is resolvable before
// honouring the pin.
func (r *Registry) Has(model string) bool {
	_, ok := r.catalog.Find(model)

	return ok
}

// ContextWindow returns model's context window from the live catalog, or 0 if
// the model is absent (0 disables the harness context-limit check for it).
func (r *Registry) ContextWindow(model string) int {
	e, ok := r.catalog.Find(model)
	if !ok {
		return 0
	}

	return e.ContextLength
}

// fitsWindow reports whether model's context window can hold estTokens. Models
// absent from the catalog are treated as fitting (fail-open; the harness still
// enforces context_limit at runtime).
func (r *Registry) fitsWindow(model string, estTokens int) bool {
	e, ok := r.catalog.Find(model)
	if !ok {
		return true
	}

	return e.ContextLength >= estTokens
}

// candidate is a model that passed the gate/bar/window filters, carried with the
// quality score and blended price used by the best-value rule.
type candidate struct {
	id      string
	quality float64
	price   float64
}

// SelectByComplexity picks the best-value model for (role, tier) per the design
// contract: a candidate must be tools-capable, not excluded, not blacklisted,
// have a normalized prior for the role that clears the tier bar, and fit the
// window estimate. An eligible operator favorite wins outright. Otherwise pick
// the most capable candidate whose blended price is within PriceHeadroom of the
// cheapest candidate; quality tie breaks to the cheaper model. Nothing passes
// -> capable default.
func (r *Registry) SelectByComplexity(in SelectInput) ModelSpec {
	if fav := r.favoriteFor(in); fav != "" {
		return r.specFor(fav)
	}

	cands := r.candidates(in)
	if len(cands) == 0 {
		return r.specFor(r.capable)
	}

	cheapest := cands[0].price
	for _, c := range cands[1:] {
		if c.price < cheapest {
			cheapest = c.price
		}
	}

	headroom := r.sel.PriceHeadroom
	if headroom <= 0 {
		headroom = defaultPriceHeadroom
	}

	band := cheapest * headroom

	best := candidate{}
	have := false

	for _, c := range cands {
		if c.price > band {
			continue
		}

		switch {
		case !have:
			best, have = c, true
		case c.quality > best.quality:
			best = c
		case c.quality == best.quality && c.price < best.price:
			best = c
		}
	}

	return r.specFor(best.id)
}

// candidates returns the models passing every filter for the given input.
// Quality is the normalized prior for the role; a model with no prior for the
// role, a prior below the tier bar, no tool support, an exclusion, a blacklist
// entry, or a window that cannot hold the estimate is dropped.
func (r *Registry) candidates(in SelectInput) []candidate {
	bar := r.tierBar(in.Tier)

	var cands []candidate

	for _, e := range r.catalog {
		if !e.SupportsTools() || in.Exclude[e.ID] || r.blacklist[e.ID] {
			continue
		}

		if len(in.ExcludeVendors) > 0 {
			// Models with no resolvable vendor are never vendor-filtered.
			if v := r.vendorOf(e.ID); v != "" && in.ExcludeVendors[v] {
				continue
			}
		}

		quality, ok := r.priors.ForRole(e.ID, in.Role)
		if !ok || quality < bar {
			continue
		}

		if in.EstTokens > 0 && !r.fitsWindow(e.ID, in.EstTokens) {
			continue
		}

		cands = append(cands, candidate{
			id:      e.ID,
			quality: quality,
			price:   e.PromptPricePerTok + e.CompletionPricePerTok,
		})
	}

	return cands
}

// favoriteFor returns the first operator favorite for (tier, role) - then
// (tier, any role) - that is a live candidate (clears the bar, not blacklisted,
// fits the window). An empty string means no eligible favorite.
func (r *Registry) favoriteFor(in SelectInput) string {
	// Favorites bypass the vendor-diversity preference: explicit operator
	// intent beats the emergent heuristic (in is a value copy).
	in.ExcludeVendors = nil

	if len(r.favorites) == 0 {
		return ""
	}

	eligible := map[string]bool{}
	for _, c := range r.candidates(in) {
		eligible[c.id] = true
	}

	for _, key := range []favKey{{Tier: in.Tier, Role: in.Role}, {Tier: in.Tier}} {
		for _, slug := range r.favorites[key] {
			if eligible[slug] {
				return slug
			}
		}
	}

	return ""
}

// SelectReviewPanel returns n specs for the review specialists: distinct models
// chosen by repeated SelectByComplexity with a growing Exclude set. Each seat
// softly prefers vendors not yet on the panel: the pick runs vendor-filtered
// when that still leaves a qualifying candidate, vendor-blind otherwise. When
// the pool runs dry, the last pick is reused to fill remaining slots rather
// than escalating price.
func (r *Registry) SelectReviewPanel(in SelectInput, n int) []ModelSpec {
	if n <= 0 {
		return nil
	}

	exclude := map[string]bool{}
	for id := range in.Exclude {
		exclude[id] = true
	}

	usedVendors := map[string]bool{}
	for v := range in.ExcludeVendors {
		usedVendors[v] = true // e.g. a Best-of-N pin's vendor
	}

	panel := make([]ModelSpec, 0, n)

	var last ModelSpec

	for len(panel) < n {
		next := in
		next.Exclude = exclude
		next.ExcludeVendors = nil // the dry probe and the fallback are vendor-blind

		// Probe the candidate pool directly: an empty pool means no distinct
		// model remains, so reuse the last real pick rather than escalating to
		// the (pricier) capable default. The probe duplicates the filter work
		// SelectByComplexity does internally - accepted for clarity at catalog
		// sizes.
		if len(r.candidates(next)) == 0 {
			if len(panel) == 0 {
				// Dry from the start: every slot is the capable default, so the
				// panel is always n non-empty specs.
				last = r.SelectByComplexity(next)
			}

			panel = append(panel, last)

			continue
		}

		// Soft vendor preference: restrict to unseated vendors only when that
		// still leaves a qualifying candidate. The price band re-anchors on the
		// filtered subset, so a diverse seat may cost more than the
		// vendor-blind pick would have - accepted, diversity is the point.
		if len(usedVendors) > 0 {
			filtered := next
			filtered.ExcludeVendors = usedVendors

			if len(r.candidates(filtered)) > 0 {
				next = filtered
			}
		}

		spec := r.SelectByComplexity(next)
		panel = append(panel, spec)
		last = spec
		exclude[spec.Model] = true

		if v := r.vendorOf(spec.Model); v != "" {
			usedVendors[v] = true
		}
	}

	return panel
}

// SelectDiscussionPanel returns n distinct models for mob session discussion seats.
// It is the review-panel diversity walk by construction - distinct-first with
// wrap-around when the pool runs dry - honoring the caller's exclusions
// (review discussions exclude the models that coded the card). It exists as a
// named seam so discussion selection can diverge from review selection
// without touching call sites.
func (r *Registry) SelectDiscussionPanel(in SelectInput, n int) []ModelSpec {
	return r.SelectReviewPanel(in, n)
}

// SelectCandidateModels picks n coder models for a Best-of-N fan-out. pin, if
// non-empty, occupies slot 1 (excluded from the auto picks); the remaining
// slots are distinct-first with wrap-around when the pool is smaller than n
// (SelectReviewPanel semantics) - model scarcity never shrinks n.
func (r *Registry) SelectCandidateModels(in SelectInput, n int, pin string) []ModelSpec {
	if n <= 0 {
		return nil
	}

	if pin == "" {
		return r.SelectReviewPanel(in, n)
	}

	next := in
	next.Exclude = map[string]bool{pin: true}

	for id := range in.Exclude {
		next.Exclude[id] = true
	}

	// The pin occupies a seat, so its vendor counts as seated for the
	// auto-picked slots (fresh map; the caller's is never mutated).
	if v := r.vendorOf(pin); v != "" {
		next.ExcludeVendors = map[string]bool{v: true}
		for u := range in.ExcludeVendors {
			next.ExcludeVendors[u] = true
		}
	}

	out := make([]ModelSpec, 0, n)
	out = append(out, ModelSpec{Model: pin, ContextWindow: r.ContextWindow(pin)})

	return append(out, r.SelectReviewPanel(next, n-1)...)
}

// specFor builds a ModelSpec for id, filling the context window from the catalog.
func (r *Registry) specFor(id string) ModelSpec {
	spec := ModelSpec{Model: id}
	if e, ok := r.catalog.Find(id); ok {
		spec.ContextWindow = e.ContextLength
	}

	return spec
}

// tierBar returns the quality bar for a tier per DefaultTierBars().
func (r *Registry) tierBar(t Tier) float64 {
	return DefaultTierBars()[t]
}
