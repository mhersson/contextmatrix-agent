package runkind

import (
	"context"
	"errors"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
)

var errUnpriced = errors.New("run kind: a model call has neither a gateway cost nor a list price")

// meteredClient enforces a run kind's own ceiling: a run kind has no MCP key,
// so nothing else prices its calls.
type meteredClient struct {
	next   llm.LLM
	ledger *orchestrator.Ledger
	prices map[string]protocol.CandidateModel
}

func newMeteredClient(next llm.LLM, maxUSD float64, sel *protocol.SelectionContext) *meteredClient {
	prices := map[string]protocol.CandidateModel{}

	if sel != nil {
		for _, c := range sel.Candidates {
			prices[c.Slug] = c
		}
	}

	return &meteredClient{next: next, ledger: orchestrator.NewLedger(maxUSD, 0), prices: prices}
}

func (m *meteredClient) Send(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := m.ledger.Check(); err != nil {
		return llm.Response{}, err
	}

	resp, err := m.next.Send(ctx, req)

	return m.charge(req, resp, err)
}

func (m *meteredClient) SendStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	if err := m.ledger.Check(); err != nil {
		return llm.Response{}, err
	}

	resp, err := m.next.SendStream(ctx, req, onDelta)

	return m.charge(req, resp, err)
}

func (m *meteredClient) charge(req llm.Request, resp llm.Response, err error) (llm.Response, error) {
	if err != nil {
		return resp, err
	}

	c, listed := m.prices[req.Model]

	usd, priced := callCost(resp.Usage, c.PromptPricePerTok, c.CompletionPricePerTok, listed)
	if !priced {
		return resp, errUnpriced
	}

	m.ledger.Spend(usd)

	return resp, nil
}

// callCost prices one call: the gateway's cost when reported, else the list
// price of a listed model, else unpriced.
func callCost(u llm.Usage, prompt, completion float64, listed bool) (usd float64, priced bool) {
	if u.Cost > 0 {
		return u.Cost, true
	}

	if !listed {
		return 0, false
	}

	// Cache tokens are priced as plain input: an upper bound, never an undercount.
	in, cacheRead, cacheCreation := u.Buckets()

	return float64(in+cacheRead+cacheCreation)*prompt + float64(u.CompletionTokens)*completion, true
}

// usageRow is the terminal status's row for one model. On the endpoint the
// cost is the gateway's sum only, so ContextMatrix applies its own estimate
// when the gateway reported none.
func usageRow(model string, res harness.Result, claudeCode bool, windows []protocol.SubscriptionUsage) protocol.RunUsage {
	row := protocol.RunUsage{Model: model, LogTokenUsage: protocol.LogTokenUsage{
		InputTokens:       res.PromptTokens,
		OutputTokens:      res.CompletionTokens,
		CacheReadTokens:   res.CacheReadTokens,
		CacheCreateTokens: res.CacheCreationTokens,
	}}

	if claudeCode {
		row.CostBasis = protocol.CostBasisSubscription
		row.Subscription = windows
	} else {
		row.CostUSD = res.TotalCostUSD
	}

	return row
}
