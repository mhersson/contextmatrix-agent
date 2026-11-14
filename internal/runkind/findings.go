package runkind

import (
	"fmt"
	"slices"
	"strings"

	protocol "github.com/mhersson/contextmatrix-protocol"
)

// findingKindsText lists the finding kinds for the analyse and investigate
// prompts; one text keeps the two from drifting.
const findingKindsText = `- guidance: lines for the project's AGENTS.md or CLAUDE.md: project knowledge
  that holds for anyone working on the project, a human or any agent, with or
  without ContextMatrix, such as its conventions, commands, layout and gotchas.
  Never mention Sentinel, ContextMatrix, the agent, its worker or tools, cards,
  runs, or limits of the environment the run had; friction those cause belongs
  in project_verify, card_advice or ecosystem. Use code spans or fenced blocks
  in them only for a command or path quoted exactly as the repository has it,
  write any URL or host name exactly as the repository has it, and describe
  everything else in prose: other code text makes the finding fail the text
  check, and it is dropped.
- code: friction in the project's code, as a card: a title and a body.
- card_advice: advice on writing cards like this one.
- project_verify: a proposal for the project's execution setup, for admins:
  its verify command, or a worker image that lacks a tool the work needs.
- hard_card: a note that this card is hard.
- ecosystem: a bug in ContextMatrix or the agent, or its generic prompts, as
  an issue: a title and a body. Documented, intentional behaviour of the agent
  or ContextMatrix is not a bug.
Evidence summarises what in the run shows the friction and names no card IDs.
`

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
