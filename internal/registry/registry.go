// Package registry resolves roles to concrete model specs for the harness. It
// is caller-facing: the FSM-free harness loop never imports it. SelectByComplexity
// is the best-value selector: a normalized external prior carries the tier bar,
// blacklisted slugs are excluded, and eligible favorites win before the
// cost-optimal pick.
package registry

import (
	"fmt"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
)

type Role string

const (
	RoleOrchestrator Role = "orchestrator"
	RolePlanner      Role = "planner"
	RoleCoder        Role = "coder"
	RoleReviewer     Role = "reviewer"
	RoleDocs         Role = "docs"
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
	Models        []string // OpenRouter native failover order (optional)
	ContextWindow int      // from the catalog; 0 if unknown
}

// favKey indexes operator-pinned favorites by complexity tier and (optionally)
// role. A zero Role applies the favorite list to every role at that tier.
type favKey struct {
	Tier Tier
	Role Role // "" = applies to all roles
}

// Registry maps roles to models, backed by the live catalog for window/price.
type Registry struct {
	pins      map[Role]string
	capable   string
	catalog   llm.Catalog
	priors    Priors
	blacklist map[string]bool
	favorites map[favKey][]string
	sel       Selection // selection config (price headroom, tier bars)

	// capabilities is the legacy measured-score table, still populated by
	// NewRegistryWithCapabilities so worker.go keeps compiling. Selection no
	// longer reads it (priors-only); T3 removes the old path and this field.
	capabilities map[string]map[Role]float64
}

// Selection configures the best-value selector. Zero value is valid: headroom
// defaults to 1.5 and TierBars falls back to DefaultTierBars().
type Selection struct {
	PriceHeadroom float64          // <= 0 -> defaultPriceHeadroom
	TierBars      map[Tier]float64 // config-driven bars; nil -> DefaultTierBars()

	// Priors and Floor are retained for the legacy WithSelection path
	// (worker.go). WithSelection mirrors Priors onto Registry.priors; Floor is
	// no longer consulted by selection (the measured floor-gate is gone).
	Priors Priors
	Floor  float64
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
		sel:       Selection{TierBars: DefaultTierBars(), PriceHeadroom: defaultPriceHeadroom},
	}
}

// WithSelection stores the selection config and returns r. A zero or negative
// PriceHeadroom defaults to defaultPriceHeadroom. The legacy path passes priors
// via Selection.Priors; mirror them onto r.priors so the priors-only candidate
// scan reads a single source regardless of constructor.
func (r *Registry) WithSelection(s Selection) *Registry {
	if s.PriceHeadroom <= 0 {
		s.PriceHeadroom = defaultPriceHeadroom
	}

	r.sel = s
	r.priors = s.Priors

	return r
}

// SelectInput describes a single best-value selection request.
type SelectInput struct {
	Role      Role
	Tier      Tier
	EstTokens int             // window-fit estimate; 0 skips the window check
	Exclude   map[string]bool // diversity: models to avoid if alternatives exist
}

func NewRegistry(pins map[Role]string, capableDefault string, catalog llm.Catalog) *Registry {
	return NewRegistryWithCapabilities(pins, capableDefault, catalog, seededCapabilities())
}

// Resolve returns the ModelSpec for role. actor is the multi-user seam (ignored
// today). Precedence: explicit pin > capable default.
func (r *Registry) Resolve(actor string, role Role) (ModelSpec, error) {
	_ = actor // seam: per-principal resolution is future work

	model := r.pins[role]
	if model == "" {
		model = r.capable
	}

	if model == "" {
		return ModelSpec{}, fmt.Errorf("no model pinned for role %q and no capable default set", role)
	}

	spec := ModelSpec{Model: model}
	if e, ok := r.catalog.Find(model); ok {
		spec.ContextWindow = e.ContextLength
	}

	return spec, nil
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

// favoriteFor returns the first operator favorite for (tier, role) — then
// (tier, any role) — that is a live candidate (clears the bar, not blacklisted,
// fits the window). An empty string means no eligible favorite.
func (r *Registry) favoriteFor(in SelectInput) string {
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
// chosen by repeated SelectByComplexity with a growing Exclude set. When the
// pool runs dry, the last pick is reused to fill remaining slots rather than
// escalating price.
func (r *Registry) SelectReviewPanel(in SelectInput, n int) []ModelSpec {
	if n <= 0 {
		return nil
	}

	exclude := map[string]bool{}
	for id := range in.Exclude {
		exclude[id] = true
	}

	panel := make([]ModelSpec, 0, n)

	var last ModelSpec

	for len(panel) < n {
		next := in
		next.Exclude = exclude

		// Probe the candidate pool directly: an empty pool means no distinct
		// model remains, so reuse the last real pick rather than escalating to
		// the (pricier) capable default. The probe duplicates the filter work
		// SelectByComplexity does internally — accepted for clarity at catalog
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

		spec := r.SelectByComplexity(next)
		panel = append(panel, spec)
		last = spec
		exclude[spec.Model] = true
	}

	return panel
}

// specFor builds a ModelSpec for id, filling the context window from the catalog.
func (r *Registry) specFor(id string) ModelSpec {
	spec := ModelSpec{Model: id}
	if e, ok := r.catalog.Find(id); ok {
		spec.ContextWindow = e.ContextLength
	}

	return spec
}

// tierBar returns the quality bar for a tier. It checks the config-driven
// TierBars first, then the priors-file TierBars, then DefaultTierBars().
func (r *Registry) tierBar(t Tier) float64 {
	if r.sel.TierBars != nil {
		if v, ok := r.sel.TierBars[t]; ok {
			return v
		}
	}

	if bars := r.sel.Priors.Meta.TierBars; bars != nil {
		if v, ok := bars[string(t)]; ok {
			return v
		}
	}

	return DefaultTierBars()[t]
}

// seededCapabilities is the conservative hand-seed for the capable default.
// deepseek-v4-flash scored a perfect coder record in both B2 sweeps, emits
// standard-format tool calls (no harmony parsing gap), and carries a 1M
// context window. B2 measures and replaces these values.
func seededCapabilities() map[string]map[Role]float64 {
	return map[string]map[Role]float64{
		"deepseek/deepseek-v4-flash": {RoleOrchestrator: 0.85, RolePlanner: 0.85, RoleCoder: 0.85, RoleReviewer: 0.85, RoleDocs: 0.85},
	}
}
