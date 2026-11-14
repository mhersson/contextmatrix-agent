package webhook

import (
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/cardid"
	"github.com/mhersson/contextmatrix-agent/internal/runkind"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// Bounds on a guidance_pr input.
const (
	maxGuidanceSection = 16 << 10
	maxGuidanceTitle   = 256
	maxGuidanceBody    = 60 << 10
	maxGuidanceAllowed = 64
	maxAllowedEntry    = 512
)

// usesModel reports whether a run kind calls a model, so it takes an LLM
// endpoint and a budget.
func usesModel(kind string) bool {
	return kind == protocol.RunKindInvestigate || kind == protocol.RunKindAnalyse
}

// runKindInvalidField names the first field /run-kind rejects in p, or "" when
// p is valid. Each input is set exactly for its own kind, and an endpoint and a
// budget exactly for a model kind. guidance_pr is project-level and takes no
// card.
func runKindInvalidField(p protocol.RunKindPayload) string {
	switch p.Kind {
	case protocol.RunKindCheck, protocol.RunKindInvestigate, protocol.RunKindAnalyse, protocol.RunKindGuidancePR:
	default:
		return "kind"
	}

	isGuidance := p.Kind == protocol.RunKindGuidancePR

	switch {
	case isGuidance && p.CardID != "", !isGuidance && !cardid.Valid(p.CardID):
		return "card_id"
	case p.GitToken == "":
		return "git_token"
	case (p.Check != nil) != (p.Kind == protocol.RunKindCheck):
		return "check"
	case (p.Investigate != nil) != (p.Kind == protocol.RunKindInvestigate):
		return "investigate"
	case (p.Analyse != nil) != (p.Kind == protocol.RunKindAnalyse):
		return "analyse"
	case (p.GuidancePR != nil) != isGuidance:
		return "guidance_pr"
	case (p.LLMEndpoint != nil) != usesModel(p.Kind):
		return "llm_endpoint"
	case usesModel(p.Kind) && p.MaxCostUSD <= 0, !usesModel(p.Kind) && p.MaxCostUSD != 0:
		return "max_cost_usd"
	}

	if isGuidance {
		return guidancePRInvalidField(p.GuidancePR)
	}

	return ""
}

// guidancePRInvalidField names the first bad field of a guidance_pr input,
// prefixed with its object, or "".
func guidancePRInvalidField(in *protocol.GuidancePRInput) string {
	switch {
	case in.Section == "", len(in.Section) > maxGuidanceSection, strings.Contains(in.Section, runkind.GuidanceMarkerPrefix):
		return "guidance_pr.section"
	case in.Title == "", len(in.Title) > maxGuidanceTitle, strings.ContainsAny(in.Title, "\r\n"):
		return "guidance_pr.title"
	case len(in.Body) > maxGuidanceBody:
		return "guidance_pr.body"
	case len(in.Allowed) > maxGuidanceAllowed, anyLonger(in.Allowed, maxAllowedEntry):
		return "guidance_pr.allowed"
	case in.ExpectHead != "" && !lowerHex(in.ExpectHead, 40) && !lowerHex(in.ExpectHead, 64):
		return "guidance_pr.expect_head"
	}

	return ""
}

func anyLonger(entries []string, limit int) bool {
	for _, e := range entries {
		if len(e) > limit {
			return true
		}
	}

	return false
}

// lowerHex reports whether s is exactly n lower-case hex characters.
func lowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}

	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// runLogID is the run ID of the run whose log section the kind reads, or "" for
// a kind that reads none.
func runLogID(p protocol.RunKindPayload) string {
	switch {
	case p.Investigate != nil:
		return p.Investigate.RunID
	case p.Analyse != nil:
		return p.Analyse.RunID
	}

	return ""
}
