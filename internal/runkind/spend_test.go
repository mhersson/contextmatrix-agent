package runkind

import (
	"context"
	"errors"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallCost(t *testing.T) {
	tokens := llm.Usage{PromptTokens: 1000, CompletionTokens: 500}

	tests := []struct {
		name       string
		usage      llm.Usage
		listed     bool
		wantUSD    float64
		wantPriced bool
	}{
		{"gateway cost wins", llm.Usage{Cost: 0.02, PromptTokens: 1000}, true, 0.02, true},
		{"gateway cost without a listing", llm.Usage{Cost: 0.02}, false, 0.02, true},
		{"list price", tokens, true, 0.002, true},
		{"cache buckets priced as input", llm.Usage{
			PromptTokens: 1000, CompletionTokens: 500,
			PromptTokensDetails: &llm.PromptTokensDetails{CachedTokens: 600, CacheWriteTokens: 100},
		}, true, 0.002, true},
		{"separate cache buckets", llm.Usage{
			PromptTokens: 400, CompletionTokens: 500, CacheReadInputTokens: 500, CacheCreationInputTokens: 100,
		}, true, 0.002, true},
		{"unlisted and unreported", tokens, false, 0, false},
		{"free listed model", llm.Usage{}, true, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usd, priced := callCost(tt.usage, 1e-6, 2e-6, tt.listed)
			assert.InDelta(t, tt.wantUSD, usd, 1e-12)
			assert.Equal(t, tt.wantPriced, priced)
		})
	}

	usd, priced := callCost(llm.Usage{PromptTokens: 5}, 0, 0, true)
	assert.Zero(t, usd)
	assert.True(t, priced)
}

type fakeLLM struct {
	calls int
	resp  llm.Response
	err   error
}

func (f *fakeLLM) Send(context.Context, llm.Request) (llm.Response, error) {
	f.calls++

	return f.resp, f.err
}

func (f *fakeLLM) SendStream(context.Context, llm.Request, func(llm.Delta)) (llm.Response, error) {
	f.calls++

	return f.resp, f.err
}

func TestMeteredClientStopsAtTheCeiling(t *testing.T) {
	sends := map[string]func(*meteredClient) error{
		"send": func(m *meteredClient) error {
			_, err := m.Send(t.Context(), llm.Request{Model: "m"})

			return err
		},
		"stream": func(m *meteredClient) error {
			_, err := m.SendStream(t.Context(), llm.Request{Model: "m"}, nil)

			return err
		},
	}

	for name, send := range sends {
		t.Run(name, func(t *testing.T) {
			next := &fakeLLM{resp: llm.Response{Usage: llm.Usage{Cost: 0.02}}}
			m := newMeteredClient(next, 0.01, nil)

			require.NoError(t, send(m))

			var be *orchestrator.BudgetExceededError

			require.ErrorAs(t, send(m), &be)
			assert.Equal(t, 1, next.calls)
		})
	}
}

func TestMeteredClientPricesByListing(t *testing.T) {
	sel := &protocol.SelectionContext{Candidates: []protocol.CandidateModel{
		{Slug: "vendor/a", PromptPricePerTok: 1e-6, CompletionPricePerTok: 2e-6},
	}}
	next := &fakeLLM{resp: llm.Response{Usage: llm.Usage{PromptTokens: 10000, CompletionTokens: 0}}}
	m := newMeteredClient(next, 0.01, sel)

	_, err := m.Send(t.Context(), llm.Request{Model: "vendor/a"})
	require.NoError(t, err)

	_, err = m.Send(t.Context(), llm.Request{Model: "vendor/a"})

	var be *orchestrator.BudgetExceededError

	require.ErrorAs(t, err, &be)
}

func TestMeteredClientEndsAnUnpricedRun(t *testing.T) {
	next := &fakeLLM{resp: llm.Response{Usage: llm.Usage{PromptTokens: 10}}}
	m := newMeteredClient(next, 1, &protocol.SelectionContext{})

	_, err := m.Send(t.Context(), llm.Request{Model: "vendor/unknown"})
	require.ErrorIs(t, err, errUnpriced)
}

func TestMeteredClientKeepsTheCallError(t *testing.T) {
	boom := errors.New("boom")
	m := newMeteredClient(&fakeLLM{err: boom}, 1, nil)

	_, err := m.Send(t.Context(), llm.Request{Model: "m"})
	require.ErrorIs(t, err, boom)
}

func TestUsageRow(t *testing.T) {
	res := harness.Result{
		PromptTokens: 41210, CompletionTokens: 2210, CacheReadTokens: 7, CacheCreationTokens: 3,
		TotalCostUSD: 0.1534,
	}
	window := protocol.SubscriptionUsage{}

	t.Run("endpoint", func(t *testing.T) {
		got := usageRow("vendor/a", res, false, []protocol.SubscriptionUsage{window})
		assert.Equal(t, "vendor/a", got.Model)
		assert.EqualValues(t, 41210, got.InputTokens)
		assert.EqualValues(t, 2210, got.OutputTokens)
		assert.EqualValues(t, 7, got.CacheReadTokens)
		assert.EqualValues(t, 3, got.CacheCreateTokens)
		assert.InDelta(t, 0.1534, got.CostUSD, 1e-12)
		assert.Empty(t, got.CostBasis)
		assert.Empty(t, got.Subscription)
	})

	t.Run("endpoint with no gateway cost", func(t *testing.T) {
		res := res
		res.TotalCostUSD = 0
		assert.Zero(t, usageRow("m", res, false, nil).CostUSD)
	})

	t.Run("claude code", func(t *testing.T) {
		got := usageRow("claude-opus-5-5", res, true, []protocol.SubscriptionUsage{window})
		assert.Equal(t, protocol.CostBasisSubscription, got.CostBasis)
		assert.Len(t, got.Subscription, 1)
		assert.Zero(t, got.CostUSD)
	})
}
