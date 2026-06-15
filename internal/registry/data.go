package registry

import (
	"bytes"
	_ "embed"
)

//go:embed data/model-priors.json
var embeddedPriors []byte

//go:embed data/capabilities.json
var embeddedCapabilities []byte

// DefaultPriors returns the embedded model-priors.json baseline parsed as Priors.
// It panics if the embedded file is malformed — this is a build-time guarantee.
func DefaultPriors() Priors {
	p, err := LoadPriors(bytes.NewReader(embeddedPriors))
	if err != nil {
		panic("registry: embedded model-priors.json is invalid: " + err.Error())
	}

	return p
}

// DefaultCapabilities returns the embedded capabilities.json baseline as a
// capability map and its metadata envelope.
// It panics if the embedded file is malformed — this is a build-time guarantee.
func DefaultCapabilities() (map[string]map[Role]float64, CapabilitiesMeta) {
	caps, meta, err := LoadCapabilitiesWithMeta(bytes.NewReader(embeddedCapabilities))
	if err != nil {
		panic("registry: embedded capabilities.json is invalid: " + err.Error())
	}

	return caps, meta
}
