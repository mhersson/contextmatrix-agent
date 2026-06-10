package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/eval"
	"github.com/mhersson/contextmatrix-agent/internal/harness"
	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubLLM struct{ cost float64 }

func (s stubLLM) Send(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{FinishReason: "stop", Usage: llm.Usage{Cost: s.cost}}, nil
}

func (s stubLLM) SendStream(context.Context, llm.Request, func(llm.Delta)) (llm.Response, error) {
	return llm.Response{FinishReason: "stop", Usage: llm.Usage{Cost: s.cost}}, nil
}

type stubTask struct {
	name string
	role registry.Role
	pass bool
}

func (t stubTask) Name() string           { return t.name }
func (t stubTask) Role() registry.Role    { return t.role }
func (t stubTask) Provision(string) error { return nil }
func (t stubTask) Prompt() string         { return "go" }
func (t stubTask) Check(context.Context, string, harness.Result) (harness.Verdict, error) {
	return harness.Verdict{OK: t.pass}, nil
}

func TestRunEvalDryRun(t *testing.T) {
	cat := llm.Catalog{{ID: "m", PromptPricePerTok: 1e-6, CompletionPricePerTok: 1e-6}}
	var buf bytes.Buffer
	err := runEval(context.Background(), &buf, stubLLM{}, cat, []string{"m"},
		[]eval.Task{stubTask{name: "c", role: registry.RoleCoder, pass: true}},
		evalParams{role: "coder", samples: 3, dryRun: true})
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "dry-run")
	assert.Contains(t, buf.String(), "3 runs")
}

func TestRunEvalWritesScores(t *testing.T) {
	out := filepath.Join(t.TempDir(), "caps.json")
	var buf bytes.Buffer
	err := runEval(context.Background(), &buf, stubLLM{cost: 0.001}, llm.Catalog{}, []string{"m"},
		[]eval.Task{stubTask{name: "c", role: registry.RoleCoder, pass: true}},
		evalParams{role: "coder", samples: 4, out: out, maxTurns: 3})
	require.NoError(t, err)
	f, err := os.Open(out)
	require.NoError(t, err)
	defer f.Close() //nolint:errcheck
	caps, err := registry.LoadCapabilities(f)
	require.NoError(t, err)
	assert.Greater(t, caps["m"][registry.RoleCoder], 0.0)
}

func TestRunEvalCheckRegression(t *testing.T) {
	// Baseline says model m clears complex coder; the measured (all-fail) run drops it.
	base := filepath.Join(t.TempDir(), "baseline.json")
	require.NoError(t, os.WriteFile(base, []byte(`{"m":{"coder":0.95}}`), 0o644))
	out := filepath.Join(t.TempDir(), "caps.json")
	var buf bytes.Buffer
	err := runEval(context.Background(), &buf, stubLLM{}, llm.Catalog{}, []string{"m"},
		[]eval.Task{stubTask{name: "c", role: registry.RoleCoder, pass: false}},
		evalParams{role: "coder", samples: 3, out: out, check: base, maxTurns: 3})
	require.Error(t, err)
	assert.Contains(t, buf.String(), "REGRESSION")
}

func TestProviderRouting(t *testing.T) {
	// empty sort disables routing
	b, err := providerRouting("", "fp8")
	require.NoError(t, err)
	assert.Nil(t, b)

	// throughput builds the gated block
	b, err = providerRouting("throughput", "fp16,bf16,fp8,unknown")
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, "throughput", m["sort"])
	assert.Equal(t, true, m["require_parameters"])
	assert.ElementsMatch(t, []any{"fp16", "bf16", "fp8", "unknown"}, m["quantizations"])
}
