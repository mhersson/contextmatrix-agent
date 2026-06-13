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

// toolThenStopLLM emits one tool call on the first turn, then stops once it sees the
// tool result come back. This makes a run a REAL (tool-using) trial — Res.ToolCallCount
// > 0 — so a failing outcome scores 0.00 instead of being treated as a parse artifact.
type toolThenStopLLM struct{}

func (toolThenStopLLM) reply(req llm.Request) llm.Response {
	for _, m := range req.Messages {
		if m.Role == "tool" {
			return llm.Response{FinishReason: "stop"}
		}
	}

	return llm.Response{
		FinishReason: "tool_calls",
		ToolCalls: []llm.ToolCall{{
			ID: "1", Type: "function",
			Function: llm.FunctionCall{Name: "noop", Arguments: "{}"},
		}},
	}
}

func (s toolThenStopLLM) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	return s.reply(req), nil
}

func (s toolThenStopLLM) SendStream(_ context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return s.reply(req), nil
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
	// Baseline says model m clears complex coder; the measured run is a REAL failure
	// (the model used tools but didn't pass), which drops the tier. A tool-using
	// failure is distinct from the all-zero-tool-call parse artifact, so it scores
	// 0.00 and is compared rather than excluded.
	base := filepath.Join(t.TempDir(), "baseline.json")
	require.NoError(t, os.WriteFile(base, []byte(`{"m":{"coder":0.95}}`), 0o644))
	out := filepath.Join(t.TempDir(), "caps.json")

	var buf bytes.Buffer

	err := runEval(context.Background(), &buf, toolThenStopLLM{}, llm.Catalog{}, []string{"m"},
		[]eval.Task{stubTask{name: "c", role: registry.RoleCoder, pass: false}},
		evalParams{role: "coder", samples: 3, out: out, check: base, maxTurns: 3})
	require.Error(t, err)
	assert.Contains(t, buf.String(), "REGRESSION")
}

// TestCheckUnknownIsUnknown: --check treats unmeasured cells as UNKNOWN, not a
// regression. A baseline cell the measured set lacks is not compared (no
// regression); a measured cell absent from the baseline is reported as new.
func TestCheckUnknownIsUnknown(t *testing.T) {
	hash := eval.TaskLibraryHash()

	// Baseline has m1 (clears complex) AND m2; measured only covers m1 (still clears).
	// m2 is unmeasured this run -> must NOT be reported as a regression.
	base := filepath.Join(t.TempDir(), "baseline.json")
	writeMetaBaseline(t, base, hash, map[string]map[registry.Role]float64{
		"m1": {registry.RoleCoder: 0.95},
		"m2": {registry.RoleCoder: 0.95},
	})

	measured := map[string]map[registry.Role]float64{
		"m1": {registry.RoleCoder: 0.95},
		"m3": {registry.RoleCoder: 0.95}, // new, not in baseline
	}

	var buf bytes.Buffer

	err := runCheck(&buf, base, measured, hash)
	require.NoError(t, err, "an unmeasured baseline cell (m2) is unknown, not a regression")

	out := buf.String()
	assert.NotContains(t, out, "REGRESSION")
	assert.Contains(t, out, "m3", "a measured cell absent from baseline is reported as new")
	assert.Contains(t, out, "new")
}

// TestCheckRegressionMetaFormat: a real drop on a measured+baselined cell is still
// caught when the baseline is in meta-wrapped format with a matching hash.
func TestCheckRegressionMetaFormat(t *testing.T) {
	hash := eval.TaskLibraryHash()
	base := filepath.Join(t.TempDir(), "baseline.json")
	writeMetaBaseline(t, base, hash, map[string]map[registry.Role]float64{
		"m1": {registry.RoleCoder: 0.95},
	})

	measured := map[string]map[registry.Role]float64{"m1": {registry.RoleCoder: 0.0}}

	var buf bytes.Buffer

	err := runCheck(&buf, base, measured, hash)
	require.Error(t, err)
	assert.Contains(t, buf.String(), "REGRESSION")
}

// TestCheckRefusesHashMismatch: --check across a different task_library_hash is an
// explicit error, not a (meaningless) comparison.
func TestCheckRefusesHashMismatch(t *testing.T) {
	base := filepath.Join(t.TempDir(), "baseline.json")
	writeMetaBaseline(t, base, "OTHER-HASH", map[string]map[registry.Role]float64{
		"m1": {registry.RoleCoder: 0.95},
	})

	measured := map[string]map[registry.Role]float64{"m1": {registry.RoleCoder: 0.0}}

	var buf bytes.Buffer

	err := runCheck(&buf, base, measured, eval.TaskLibraryHash())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hash", "cross-hash check must refuse with an explicit error")
	assert.NotContains(t, buf.String(), "REGRESSION", "no comparison is performed on a hash mismatch")
}

// TestRunEvalStampsMeta: a full runEval writes a meta-wrapped baseline carrying the
// task library hash, samples and harness version (date defaulted in RunE, passed in).
func TestRunEvalStampsMeta(t *testing.T) {
	out := filepath.Join(t.TempDir(), "caps.json")

	var buf bytes.Buffer

	err := runEval(context.Background(), &buf, stubLLM{cost: 0.001}, llm.Catalog{}, []string{"m"},
		[]eval.Task{stubTask{name: "c", role: registry.RoleCoder, pass: true}},
		evalParams{role: "coder", samples: 4, out: out, maxTurns: 3, date: "2026-06-13"})
	require.NoError(t, err)

	f, err := os.Open(out)
	require.NoError(t, err)

	defer f.Close() //nolint:errcheck

	_, meta, err := registry.LoadCapabilitiesWithMeta(f)
	require.NoError(t, err)
	assert.Equal(t, "2026-06-13", meta.Date)
	assert.Equal(t, 4, meta.Samples)
	assert.Equal(t, eval.TaskLibraryHash(), meta.TaskLibraryHash)
	assert.Equal(t, eval.HarnessVersion, meta.HarnessVersion)
	assert.Greater(t, meta.Floor, 0.0)
	// No provider-sort set -> default routing label (closes routingLabel coverage).
	assert.Equal(t, "default", meta.Routing)
}

// writeMetaBaseline writes a meta-wrapped capabilities file with the given hash and
// model scores, for --check tests.
func writeMetaBaseline(t *testing.T, path, hash string, caps map[string]map[registry.Role]float64) {
	t.Helper()

	f, err := os.Create(path)
	require.NoError(t, err)

	defer f.Close() //nolint:errcheck

	require.NoError(t, eval.WriteCapabilitiesWithMeta(f, caps, registry.CapabilitiesMeta{
		Date:            "2026-06-10",
		TaskLibraryHash: hash,
	}))
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
