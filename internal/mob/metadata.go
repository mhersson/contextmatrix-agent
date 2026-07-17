package mob

import (
	"encoding/json"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// metadataKey is the A2A Message.Metadata key carrying mob session control data -
// the wire convention shared with the guest shim.
const metadataKey = "cm_mob"

// costKey carries an internal seat's per-turn USD cost back to the moderator
// on the utterance message. Engine-internal: guests never set it (their
// compute is their own), so a missing key reads as 0.
const costKey = "cm_mob_cost_usd"

const (
	controlRound = "round"
	controlClose = "close"
)

// control is the decoded cm_mob metadata of one moderator->seat message.
type control struct {
	Kind  string // "round" | "close"
	Round int
}

// setControl attaches c to m under the cm_mob key.
func setControl(m *a2a.Message, c control) {
	m.SetMeta(metadataKey, map[string]any{"control": c.Kind, "round": c.Round})
}

// parseControl decodes the cm_mob metadata of m. Missing or malformed
// metadata means {Kind: "round"} - executors and the shim treat unknown
// control data as an ordinary round message.
func parseControl(m *a2a.Message) control {
	c := control{Kind: controlRound}
	if m == nil {
		return c
	}

	fields, ok := m.Meta()[metadataKey].(map[string]any)
	if !ok {
		return c
	}

	if kind, ok := fields["control"].(string); ok && (kind == controlRound || kind == controlClose) {
		c.Kind = kind
	}

	switch r := fields["round"].(type) {
	case int:
		c.Round = r
	case float64:
		c.Round = int(r)
	case json.Number:
		if n, err := r.Int64(); err == nil {
			c.Round = int(n)
		}
	}

	return c
}

// setCost attaches a per-turn USD cost to an utterance message. Zero cost is
// not written - absence already reads as 0.
func setCost(m *a2a.Message, usd float64) {
	if usd == 0 {
		return
	}

	m.SetMeta(costKey, usd)
}

// costFrom reads the per-turn USD cost off an utterance message; 0 when
// absent or malformed.
func costFrom(m *a2a.Message) float64 {
	if m == nil {
		return 0
	}

	switch v := m.Meta()[costKey].(type) {
	case float64:
		return v
	case json.Number:
		f, _ := v.Float64()

		return f
	}

	return 0
}
