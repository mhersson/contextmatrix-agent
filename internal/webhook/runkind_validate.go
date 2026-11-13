package webhook

import (
	"github.com/mhersson/contextmatrix-agent/internal/cardid"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

// usesModel reports whether a run kind calls a model, so it takes an LLM
// endpoint and a budget.
func usesModel(kind string) bool {
	return kind == protocol.RunKindInvestigate
}

// runKindInvalidField names the first field /run-kind rejects in p, or "" when
// p is valid. Reserved kinds are rejected until they ship. Each input is set
// exactly for its own kind, and an endpoint and a budget exactly for a model
// kind.
func runKindInvalidField(p protocol.RunKindPayload) string {
	isCheck := p.Kind == protocol.RunKindCheck
	isInvestigate := p.Kind == protocol.RunKindInvestigate

	switch {
	case !isCheck && !isInvestigate:
		return "kind"
	case !cardid.Valid(p.CardID):
		return "card_id"
	case p.GitToken == "":
		return "git_token"
	case (p.Check != nil) != isCheck:
		return "check"
	case (p.Investigate != nil) != isInvestigate:
		return "investigate"
	case (p.LLMEndpoint != nil) != usesModel(p.Kind):
		return "llm_endpoint"
	case usesModel(p.Kind) && p.MaxCostUSD <= 0:
		return "max_cost_usd"
	}

	return ""
}
