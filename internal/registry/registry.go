// Package registry resolves roles to concrete model specs for the harness. It
// is caller-facing: the FSM-free harness loop never imports it. SelectByComplexity
// is a seeded stub; measured capability scores arrive with the B2 eval harness.
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
}

func NewRegistry(pins map[Role]string, capableDefault string, catalog llm.Catalog) *Registry {
	if pins == nil {
		pins = map[Role]string{}
	}
	return &Registry{pins: pins, capable: capableDefault, catalog: catalog, capabilities: seededCapabilities()}
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

// SelectByComplexity is the seam C2 consumes: the cheapest tool-capable model in
// the registry's catalog whose seeded capability for role clears the tier bar,
// else the capable default. It reads r.catalog (single source of truth, matching
// Resolve/fitsWindow). Capability scores are hand-seeded in B1; B2 replaces them.
func (r *Registry) SelectByComplexity(role Role, t Tier) ModelSpec {
	bar := tierBar(t)
	best, bestPrice := "", -1.0
	for _, e := range r.catalog {
		if !e.SupportsTools() || r.capabilities[e.ID][role] < bar {
			continue
		}
		price := e.PromptPricePerTok + e.CompletionPricePerTok
		if best == "" || price < bestPrice {
			best, bestPrice = e.ID, price
		}
	}
	if best == "" {
		best = r.capable
	}
	spec := ModelSpec{Model: best}
	if e, ok := r.catalog.Find(best); ok {
		spec.ContextWindow = e.ContextLength
	}
	return spec
}

func tierBar(t Tier) float64 {
	switch t {
	case TierComplex:
		return 0.8
	case TierModerate:
		return 0.6
	default:
		return 0.4
	}
}

// seededCapabilities is the conservative hand-seed reflecting the B0 model floor
// (gpt-oss-120b-class completes; gemma-4-31b-class does not). B2 measures and
// replaces these values.
func seededCapabilities() map[string]map[Role]float64 {
	return map[string]map[Role]float64{
		"openai/gpt-oss-120b": {RoleOrchestrator: 0.85, RolePlanner: 0.85, RoleCoder: 0.85, RoleReviewer: 0.85, RoleDocs: 0.85},
	}
}
