// Package registry resolves roles to concrete model specs for the harness. It
// is caller-facing: the FSM-free harness loop never imports it. SelectByComplexity
// is the two-signal best-value selector: an external prior carries the tier bar
// while measured capabilities gate at a calibrated floor.
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
)

// ModelSpec is what the caller feeds into harness.Config for a given role.
type ModelSpec struct {
	Model         string
	Models        []string // OpenRouter native failover order (optional)
	ContextWindow int      // from the catalog; 0 if unknown
}

// Registry maps roles to models, backed by the live catalog for window/price.
type Registry struct {
	pins         map[Role]string
	capable      string
	catalog      llm.Catalog
	capabilities map[string]map[Role]float64 // seeded in B1; refined by B2
	sel          Selection                   // two-signal selection config (optional)
}

// Selection configures the two-signal best-value selector. Zero value is valid:
// no priors (bar applies to measured scores), Floor 0, headroom defaults to 1.5.
type Selection struct {
	Priors        Priors
	Floor         float64 // functional gate on measured capabilities
	PriceHeadroom float64 // <= 0 -> defaultPriceHeadroom
}

const defaultPriceHeadroom = 1.5

// WithSelection stores the selection config and returns r. A zero or negative
// PriceHeadroom defaults to defaultPriceHeadroom.
func (r *Registry) WithSelection(s Selection) *Registry {
	if s.PriceHeadroom <= 0 {
		s.PriceHeadroom = defaultPriceHeadroom
	}

	r.sel = s

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
// contract: a candidate must be tools-capable, not excluded, have a measured
// score >= Floor (unmeasured cells are never selectable), have its quality
// score (external prior when present, else measured) clear the tier bar, and
// fit the window estimate. Among candidates, pick the most capable whose
// blended price is within PriceHeadroom of the cheapest candidate; quality tie
// breaks to the cheaper model. Nothing passes -> capable default.
func (r *Registry) SelectByComplexity(in SelectInput) ModelSpec {
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
func (r *Registry) candidates(in SelectInput) []candidate {
	bar := r.tierBar(in.Tier)

	var cands []candidate

	for _, e := range r.catalog {
		if !e.SupportsTools() || in.Exclude[e.ID] {
			continue
		}

		// Functional gate: measured score must exist AND clear the floor.
		// Unmeasured (model, role) cells are unknown, never selectable.
		measured, ok := r.measured(e.ID, in.Role)
		if !ok || measured < r.sel.Floor {
			continue
		}

		// Prior bar: external prior carries the bar when present; otherwise the
		// bar applies to our measured score (today's behavior).
		quality := measured
		if p, ok := r.sel.Priors.ForRole(e.ID, in.Role); ok {
			quality = p
		}

		if quality < bar {
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

// measured returns the measured capability for (model, role) and whether one
// exists. A missing model or role reports (0, false) — unmeasured, not zero.
func (r *Registry) measured(model string, role Role) (float64, bool) {
	roles, ok := r.capabilities[model]
	if !ok {
		return 0, false
	}

	v, ok := roles[role]

	return v, ok
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

// tierBar returns the quality bar for a tier, reading the priors-file TierBars
// when configured and falling back to the seeded constants otherwise.
func (r *Registry) tierBar(t Tier) float64 {
	if bars := r.sel.Priors.Meta.TierBars; bars != nil {
		if v, ok := bars[string(t)]; ok {
			return v
		}
	}

	switch t {
	case TierComplex:
		return 0.8
	case TierModerate:
		return 0.6
	default:
		return 0.4
	}
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
