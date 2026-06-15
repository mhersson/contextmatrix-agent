package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
)

// CapabilitiesMeta carries provenance information for the capabilities baseline.
type CapabilitiesMeta struct {
	Date            string  `json:"date"`
	Samples         int     `json:"samples"`
	TaskLibraryHash string  `json:"task_library_hash"`
	Routing         string  `json:"routing"`
	HarnessVersion  string  `json:"harness_version"`
	Floor           float64 `json:"floor"` // calibrated functional floor
}

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

// LoadCapabilities parses a capabilities document. It supports both formats:
//   - Legacy flat map: {"model": {"coder": 0.71, "reviewer": 0.55}}
//   - Meta-wrapped:    {"meta": {...}, "models": {"model": {"coder": 0.71}}}
func LoadCapabilities(r io.Reader) (map[string]map[Role]float64, error) {
	caps, _, err := loadCapabilitiesDoc(r)

	return caps, err
}

// LoadCapabilitiesWithMeta parses a meta-wrapped capabilities document and
// returns the capability map together with its metadata envelope.
// Legacy flat-map files are also accepted; in that case an empty CapabilitiesMeta is returned.
func LoadCapabilitiesWithMeta(r io.Reader) (map[string]map[Role]float64, CapabilitiesMeta, error) {
	return loadCapabilitiesDoc(r)
}

// loadCapabilitiesDoc is the shared parser for both LoadCapabilities and
// LoadCapabilitiesWithMeta. It detects the meta-wrapped format by the shape of
// the top-level "meta"/"models" keys and dispatches accordingly.
func loadCapabilitiesDoc(r io.Reader) (map[string]map[Role]float64, CapabilitiesMeta, error) {
	// Decode into a raw map first so we can detect the format without double-reading.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, CapabilitiesMeta{}, fmt.Errorf("decode capabilities: %w", err)
	}

	// Footgun: a legacy flat file can contain a model slug literally named
	// "meta" — its value is a role map, which is also a JSON object — so a
	// doc is only treated as meta-wrapped when "meta" is an object AND a
	// "models" key is present (the writer always emits both).
	if metaRaw, hasMetaKey := raw["meta"]; hasMetaKey && isJSONObject(metaRaw) {
		if _, hasModelsKey := raw["models"]; hasModelsKey {
			return parseMetaFormat(raw)
		}
	}

	return parseLegacyFormat(raw)
}

// isJSONObject reports whether raw's first token opens a JSON object.
func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")

	return len(trimmed) > 0 && trimmed[0] == '{'
}

// parseMetaFormat handles {"meta":{...},"models":{...}}.
func parseMetaFormat(raw map[string]json.RawMessage) (map[string]map[Role]float64, CapabilitiesMeta, error) {
	var meta CapabilitiesMeta
	if err := json.Unmarshal(raw["meta"], &meta); err != nil {
		return nil, CapabilitiesMeta{}, fmt.Errorf("decode capabilities meta: %w", err)
	}

	modelsRaw, ok := raw["models"]
	if !ok {
		return map[string]map[Role]float64{}, meta, nil
	}

	var models map[string]map[Role]float64
	if err := json.Unmarshal(modelsRaw, &models); err != nil {
		return nil, CapabilitiesMeta{}, fmt.Errorf("decode capabilities models: %w", err)
	}

	if models == nil {
		models = map[string]map[Role]float64{}
	}

	return models, meta, nil
}

// parseLegacyFormat handles the flat map {"model": {"coder": 0.71}}.
func parseLegacyFormat(raw map[string]json.RawMessage) (map[string]map[Role]float64, CapabilitiesMeta, error) {
	caps := make(map[string]map[Role]float64, len(raw))

	for model, rolesRaw := range raw {
		var roles map[Role]float64
		if err := json.Unmarshal(rolesRaw, &roles); err != nil {
			return nil, CapabilitiesMeta{}, fmt.Errorf("decode capabilities entry %q: %w", model, err)
		}

		caps[model] = roles
	}

	return caps, CapabilitiesMeta{}, nil
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
