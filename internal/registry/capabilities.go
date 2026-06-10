package registry

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
)

// NewRegistryWithCapabilities is NewRegistry with an explicit capability table
// (measured scores from the B2 eval). NewRegistry delegates here with the hand-seed.
func NewRegistryWithCapabilities(pins map[Role]string, capableDefault string, catalog llm.Catalog, caps map[string]map[Role]float64) *Registry {
	if pins == nil {
		pins = map[Role]string{}
	}

	if caps == nil {
		caps = map[string]map[Role]float64{}
	}

	return &Registry{pins: pins, capable: capableDefault, catalog: catalog, capabilities: caps}
}

// LoadCapabilities parses a capabilities.json document
// ({"model": {"coder": 0.71, "reviewer": 0.55}}).
func LoadCapabilities(r io.Reader) (map[string]map[Role]float64, error) {
	var m map[string]map[Role]float64
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode capabilities: %w", err)
	}

	return m, nil
}

// MergeCapabilities returns a new map where `over` (measured) wins per
// (model, role) and extends `base` (the hand-seed). Inputs are not mutated.
func MergeCapabilities(base, over map[string]map[Role]float64) map[string]map[Role]float64 {
	out := map[string]map[Role]float64{}
	for m, roles := range base {
		out[m] = map[Role]float64{}
		for r, v := range roles {
			out[m][r] = v
		}
	}

	for m, roles := range over {
		if out[m] == nil {
			out[m] = map[Role]float64{}
		}

		for r, v := range roles {
			out[m][r] = v
		}
	}

	return out
}
