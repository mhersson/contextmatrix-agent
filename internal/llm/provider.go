package llm

import "encoding/json"

// Provider mirrors OpenRouter's `provider` routing object. Pointer + omitempty
// so unset fields never appear on the wire.
type Provider struct {
	RequireParameters *bool    `json:"require_parameters,omitempty"`
	Order             []string `json:"order,omitempty"`
	Sort              *string  `json:"sort,omitempty"`
}

// Reasoning mirrors OpenRouter's `reasoning` object.
type Reasoning struct {
	Effort    *string `json:"effort,omitempty"` // "low" | "medium" | "high"
	MaxTokens *int    `json:"max_tokens,omitempty"`
	Exclude   *bool   `json:"exclude,omitempty"`
}

// Raw marshals p to a json.RawMessage, or returns nil if p is entirely empty.
func (p Provider) Raw() (json.RawMessage, error) {
	if p.RequireParameters == nil && len(p.Order) == 0 && p.Sort == nil {
		return nil, nil
	}

	return json.Marshal(p)
}

// Raw marshals r to a json.RawMessage, or returns nil if r is entirely empty.
func (r Reasoning) Raw() (json.RawMessage, error) {
	if r.Effort == nil && r.MaxTokens == nil && r.Exclude == nil {
		return nil, nil
	}

	return json.Marshal(r)
}
