package registry

import (
	"encoding/json"
	"fmt"
	"io"
)

// PriorEntry holds external quality scores for a model (e.g. from Artificial
// Analysis). Role scores are pointers so an omitted role is distinguishable
// from an explicit zero: absent/null decodes to nil, meaning no prior exists.
type PriorEntry struct {
	Coder     *float64 `json:"coder"`
	Reviewer  *float64 `json:"reviewer"`
	Source    string   `json:"source"`
	Retrieved string   `json:"retrieved"`
}

// PriorsMeta carries provenance information for the priors file.
type PriorsMeta struct {
	Updated   string             `json:"updated"`
	Procedure string             `json:"procedure"`
	TierBars  map[string]float64 `json:"tier_bars"` // keyed by Tier string
}

// Priors is the parsed model-priors.json document.
type Priors struct {
	Meta   PriorsMeta            `json:"meta"`
	Models map[string]PriorEntry `json:"models"`
}

// LoadPriors decodes a model-priors.json document from r.
// Unknown fields are tolerated for forward compatibility.
func LoadPriors(r io.Reader) (Priors, error) {
	var p Priors
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return Priors{}, fmt.Errorf("decode priors: %w", err)
	}

	return p, nil
}

// ForRole returns the prior score for the given model and role, and whether one
// exists. A role omitted from the model's JSON entry has no prior — (0, false)
// — so callers fall back to measured scores; an explicit 0 in the file is a
// real prior and returns (0, true). Only RoleCoder and RoleReviewer are
// tracked in priors; other roles return (0, false).
func (p Priors) ForRole(model string, role Role) (float64, bool) {
	entry, ok := p.Models[model]
	if !ok {
		return 0, false
	}

	var score *float64

	switch role {
	case RoleCoder:
		score = entry.Coder
	case RoleReviewer:
		score = entry.Reviewer
	}

	if score == nil {
		return 0, false
	}

	return *score, true
}
