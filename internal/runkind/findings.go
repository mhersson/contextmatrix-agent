package runkind

import (
	"fmt"
	"slices"
	"strings"

	protocol "github.com/mhersson/contextmatrix-protocol"
)

// findingsSchema is the JSON schema of a terminal tool's findings array.
const findingsSchema = `{
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"kind": {"type": "string", "enum": ["guidance", "code", "card_advice", "project_verify", "hard_card", "ecosystem"]},
					"title": {"type": "string", "description": "code and ecosystem findings only."},
					"text": {"type": "string"},
					"evidence": {"type": "string", "description": "What in the run shows it. No card IDs."}
				},
				"required": ["kind", "text", "evidence"]
			}
		}`

var findingKinds = []string{
	protocol.FindingGuidance, protocol.FindingCode, protocol.FindingCardAdvice,
	protocol.FindingProjectVerify, protocol.FindingHardCard, protocol.FindingEcosystem,
}

// findingFault is why ContextMatrix would reject finding i (0-based), or nil.
func findingFault(i int, f protocol.FrictionFinding) error {
	if !slices.Contains(findingKinds, f.Kind) {
		return fmt.Errorf("finding %d: kind %q is not one of %s", i+1, f.Kind, strings.Join(findingKinds, ", "))
	}

	if strings.TrimSpace(f.Text) == "" {
		return fmt.Errorf("finding %d: text is empty", i+1)
	}

	return nil
}

// limitFindings drops each finding whose title or text is over its limit
// before it keeps the first 50, and truncates evidence. A non-nil input stays
// non-nil.
func limitFindings(fs []protocol.FrictionFinding) []protocol.FrictionFinding {
	if fs == nil {
		return nil
	}

	out := make([]protocol.FrictionFinding, 0, min(len(fs), maxFindings))

	for _, f := range fs {
		if len(out) == maxFindings {
			break
		}

		if len(f.Title) > maxTitleBytes || len(f.Text) > maxTextBytes {
			continue
		}

		f.Evidence = truncate(f.Evidence, maxEvidenceBytes)
		out = append(out, f)
	}

	return out
}

// maskFindings masks every string of fs, kind included: a token the model
// echoed can land in any of them.
func maskFindings(fs []protocol.FrictionFinding, mask func(string) string) []protocol.FrictionFinding {
	if fs == nil {
		return nil
	}

	out := make([]protocol.FrictionFinding, len(fs))
	for i, f := range fs {
		f.Kind = mask(f.Kind)
		f.Title, f.Text, f.Evidence = mask(f.Title), mask(f.Text), mask(f.Evidence)
		out[i] = f
	}

	return out
}
