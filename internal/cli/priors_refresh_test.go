package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// aaSampleFixture is a saved Artificial Analysis v2 response authored from the
// real endpoint shape. It carries four mappable candidates with their real
// indices plus one model (claude-opus-4-8) with a NULL coding index to exercise
// the role-skip path, and an unmapped frontier model (gemini-3-1-pro) to
// exercise the gap report.
const aaSampleFixture = `{
  "status": 200,
  "prompt_options": {"parallel_queries": 1},
  "data": [
    {
      "id": "id-gpt-oss-20b",
      "name": "gpt-oss-20B (high)",
      "slug": "gpt-oss-20b",
      "release_date": "2025-08-05",
      "model_creator": {"id": "c-openai", "name": "OpenAI", "slug": "openai"},
      "evaluations": {
        "artificial_analysis_intelligence_index": 24.5,
        "artificial_analysis_coding_index": 18.5,
        "mmlu_pro": 0.748
      },
      "pricing": {"price_1m_blended_3_to_1": 0.088, "price_1m_input_tokens": 0.05, "price_1m_output_tokens": 0.2},
      "median_output_tokens_per_second": 254.601,
      "median_time_to_first_token_seconds": 0.464
    },
    {
      "id": "id-gpt-5-5-medium",
      "name": "GPT-5.5 (medium)",
      "slug": "gpt-5-5-medium",
      "release_date": "2026-01-01",
      "model_creator": {"id": "c-openai", "name": "OpenAI", "slug": "openai"},
      "evaluations": {
        "artificial_analysis_intelligence_index": 56.7,
        "artificial_analysis_coding_index": 56.2
      },
      "pricing": {"price_1m_blended_3_to_1": 3.5}
    },
    {
      "id": "id-gpt-5-5-non-reasoning",
      "name": "GPT-5.5 (non-reasoning)",
      "slug": "gpt-5-5-non-reasoning",
      "release_date": "2026-01-01",
      "model_creator": {"id": "c-openai", "name": "OpenAI", "slug": "openai"},
      "evaluations": {
        "artificial_analysis_intelligence_index": 40.9,
        "artificial_analysis_coding_index": 48.6
      },
      "pricing": {"price_1m_blended_3_to_1": 1.25}
    },
    {
      "id": "id-gpt-5-4-nano-medium",
      "name": "GPT-5.4 Nano (medium)",
      "slug": "gpt-5-4-nano-medium",
      "release_date": "2025-12-01",
      "model_creator": {"id": "c-openai", "name": "OpenAI", "slug": "openai"},
      "evaluations": {
        "artificial_analysis_intelligence_index": 38.1,
        "artificial_analysis_coding_index": 35.0
      },
      "pricing": {"price_1m_blended_3_to_1": 0.2}
    },
    {
      "id": "id-claude-opus-4-8",
      "name": "Claude Opus 4.8",
      "slug": "claude-opus-4-8",
      "release_date": "2026-01-01",
      "model_creator": {"id": "c-anthropic", "name": "Anthropic", "slug": "anthropic"},
      "evaluations": {
        "artificial_analysis_intelligence_index": 60.3,
        "artificial_analysis_coding_index": null
      },
      "pricing": {"price_1m_blended_3_to_1": 30.0}
    },
    {
      "id": "id-gemini-3-1-pro",
      "name": "Gemini 3.1 Pro",
      "slug": "gemini-3-1-pro-preview",
      "release_date": "2026-01-01",
      "model_creator": {"id": "c-google", "name": "Google", "slug": "google"},
      "evaluations": {
        "artificial_analysis_intelligence_index": 58.0,
        "artificial_analysis_coding_index": 55.0
      },
      "pricing": {"price_1m_blended_3_to_1": 5.0}
    }
  ]
}`

const testAPIKey = "aa-secret-key-do-not-leak-1234567890"

// aaStub serves the saved fixture and asserts the API key is sent in the
// x-api-key header, covering the key-transmission path.
func aaStub(t *testing.T) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, testAPIKey, r.Header.Get("x-api-key"), "key must be sent in x-api-key header")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(aaSampleFixture))
	}))
}

// testParams returns priorsRefreshParams wired to the stub, with the candidate
// universe overridden to a small mappable set so the test does not depend on the
// shipped candidates.txt drifting.
func testParams(srv *httptest.Server, out, key string) priorsRefreshParams {
	return priorsRefreshParams{
		apiURL:       srv.URL,
		out:          out,
		date:         "2026-06-13",
		gapThreshold: 0.85,
		key:          key,
		candidates: []string{
			"openai/gpt-oss-20b",
			"openai/gpt-5.5",
			"openai/gpt-5.5-non-reasoning",
			"openai/gpt-5.4-nano",
			"anthropic/claude-opus-4.8",
		},
	}
}

func readProposal(t *testing.T, path string) registry.Priors {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)

	defer f.Close()

	p, err := registry.LoadPriors(f)
	require.NoError(t, err)

	return p
}

func TestPriorsRefreshProposal(t *testing.T) {
	srv := aaStub(t)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "model-priors.json.proposed")

	var buf bytes.Buffer

	err := runPriorsRefresh(context.Background(), &buf, testParams(srv, out, testAPIKey))
	require.NoError(t, err)

	p := readProposal(t, out)

	// Candidate present in AA data with both indices -> both roles, normalized.
	entry, ok := p.Models["openai/gpt-5.5"]
	require.True(t, ok, "mapped candidate must get an entry")
	require.NotNil(t, entry.Coder)
	require.NotNil(t, entry.Reviewer)
	// max coding index across response = 56.2 (gpt-5-5-medium itself) -> 1.0.
	assert.InDelta(t, 1.0, *entry.Coder, 1e-9)
	// max intelligence index across response = 60.3 (opus) -> 56.7/60.3.
	assert.InDelta(t, 56.7/60.3, *entry.Reviewer, 1e-9)
	assert.Equal(t, "artificialanalysis", entry.Source)
	assert.Equal(t, "2026-06-13", entry.Retrieved)

	// Round-trips through LoadPriors and ForRole.
	coder, has := p.ForRole("openai/gpt-5.5", registry.RoleCoder)
	assert.True(t, has)
	assert.InDelta(t, 1.0, coder, 1e-9)

	// Meta points at the documented procedure.
	assert.Equal(t, "docs/model-priors.md", p.Meta.Procedure)
	assert.Equal(t, "2026-06-13", p.Meta.Updated)

	// Tier bars are carried forward from the embedded baseline (non-null), so a
	// human renaming the proposal over the live file does not wipe the selector's
	// per-role floors.
	baseline := registry.DefaultPriors()
	require.NotEmpty(t, baseline.Meta.TierBars, "embedded baseline must define tier bars")
	assert.Equal(t, baseline.Meta.TierBars, p.Meta.TierBars)
}

func TestLivePathGuard(t *testing.T) {
	srv := aaStub(t)
	defer srv.Close()

	// Run from the repo root so the canonical live path resolves to the real
	// embedded file, exactly as the command does in production.
	chdirRepoRoot(t)

	const livePath = "internal/registry/data/model-priors.json"

	// Snapshot the live priors file so we can prove the guard left it byte-identical.
	before, err := os.ReadFile(livePath)
	require.NoError(t, err)

	var buf bytes.Buffer

	p := testParams(srv, livePath, testAPIKey)

	err = runPriorsRefresh(context.Background(), &buf, p)
	require.Error(t, err, "writing the live priors file must be refused")
	assert.Contains(t, err.Error(), "model-priors.json")

	after, readErr := os.ReadFile(livePath)
	require.NoError(t, readErr)
	assert.Equal(t, before, after, "live priors file must be untouched by the guard")
}

// chdirRepoRoot switches the test's working directory to the module root
// (two levels up from internal/cli) and restores it on cleanup.
func chdirRepoRoot(t *testing.T) {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(wd) })

	require.NoError(t, os.Chdir(filepath.Join("..", "..")))
}

func TestSlugMapping(t *testing.T) {
	srv := aaStub(t)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "model-priors.json.proposed")

	var buf bytes.Buffer

	require.NoError(t, runPriorsRefresh(context.Background(), &buf, testParams(srv, out, testAPIKey)))

	p := readProposal(t, out)

	// AA slug gpt-oss-20b maps to OpenRouter openai/gpt-oss-20b.
	_, ok := p.Models["openai/gpt-oss-20b"]
	assert.True(t, ok, "AA gpt-oss-20b must map to openai/gpt-oss-20b")

	// gemini-3-1-pro-preview has no mapping entry and is not a candidate; it must
	// surface in the report as a gap suggestion, never be silently dropped.
	out2 := buf.String()
	assert.Contains(t, out2, "gemini-3-1-pro-preview", "unmapped high-ranking AA model must appear in the report")
}

func TestCandidateGapReport(t *testing.T) {
	srv := aaStub(t)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "model-priors.json.proposed")

	var buf bytes.Buffer

	require.NoError(t, runPriorsRefresh(context.Background(), &buf, testParams(srv, out, testAPIKey)))

	report := buf.String()
	// gemini-3-1-pro (coding 55.0, normalized 55/56.2 ~= 0.978 >= 0.85) is absent
	// from candidates -> listed as a suggestion.
	assert.Contains(t, report, "gemini-3-1-pro-preview")

	p := readProposal(t, out)
	// Nothing auto-added: the gap model is NOT written into the proposal.
	_, present := p.Models["google/gemini-3.1-pro-preview"]
	assert.False(t, present, "gap suggestions must never be auto-added to the proposal")

	_, present2 := p.Models["gemini-3-1-pro-preview"]
	assert.False(t, present2)
}

func TestNoKeyExplains(t *testing.T) {
	srv := aaStub(t)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "model-priors.json.proposed")

	var buf bytes.Buffer

	p := testParams(srv, out, "")
	err := runPriorsRefresh(context.Background(), &buf, p)
	require.Error(t, err, "empty key must be an error")

	// The error/output points at the documented manual procedure.
	combined := err.Error() + buf.String()
	assert.Contains(t, combined, "docs/model-priors.md")

	// No network call happened -> no proposal file written.
	_, statErr := os.Stat(out)
	assert.True(t, os.IsNotExist(statErr), "no proposal must be written without a key")
}

func TestNullIndexSkipsRole(t *testing.T) {
	srv := aaStub(t)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "model-priors.json.proposed")

	var buf bytes.Buffer

	require.NoError(t, runPriorsRefresh(context.Background(), &buf, testParams(srv, out, testAPIKey)))

	p := readProposal(t, out)

	// claude-opus-4-8 has a null coding index -> no coder prior (and not a 0).
	entry, ok := p.Models["anthropic/claude-opus-4.8"]
	require.True(t, ok)
	assert.Nil(t, entry.Coder, "null coding index must not emit a coder prior")
	// reviewer is still emitted because its intelligence index is present.
	// opus intelligence index 60.3 is the response-wide max -> normalized 1.0.
	require.NotNil(t, entry.Reviewer)
	assert.InDelta(t, 1.0, *entry.Reviewer, 1e-9)

	// ForRole agrees: coder absent, reviewer present.
	_, hasCoder := p.ForRole("anthropic/claude-opus-4.8", registry.RoleCoder)
	assert.False(t, hasCoder)

	_, hasReviewer := p.ForRole("anthropic/claude-opus-4.8", registry.RoleReviewer)
	assert.True(t, hasReviewer)
}

func TestKeyRedacted(t *testing.T) {
	srv := aaStub(t)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "model-priors.json.proposed")

	var buf bytes.Buffer

	require.NoError(t, runPriorsRefresh(context.Background(), &buf, testParams(srv, out, testAPIKey)))

	// Key must not appear in stdout/report.
	assert.NotContains(t, buf.String(), testAPIKey, "key must never appear in output")

	// Key must not appear in the proposal file.
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.NotContains(t, string(data), testAPIKey, "key must never appear in the proposal file")
}

// TestFetchErrorRedactsKey proves the fetch-error path runs the error string
// through the redactor: an unreachable URL that embeds the key must surface a
// fetch error with the key masked, never echoed verbatim.
func TestFetchErrorRedactsKey(t *testing.T) {
	out := filepath.Join(t.TempDir(), "model-priors.json.proposed")

	var buf bytes.Buffer

	// Embed the key in the URL so the transport error naturally contains it; the
	// host is unroutable so Do() fails fast. runPriorsRefresh must redact it.
	p := priorsRefreshParams{
		apiURL:       "http://" + testAPIKey + ".invalid./v2/models",
		out:          out,
		date:         "2026-06-13",
		gapThreshold: 0.85,
		key:          testAPIKey,
		candidates:   []string{"openai/gpt-5.5"},
	}

	err := runPriorsRefresh(context.Background(), &buf, p)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testAPIKey, "key must be redacted in the fetch error")
	assert.Contains(t, err.Error(), "[REDACTED]", "redactor must have masked the key")

	// A failed fetch writes no proposal.
	_, statErr := os.Stat(out)
	assert.True(t, os.IsNotExist(statErr), "no proposal must be written on a fetch error")
}

// TestProposalIsValidJSONEnvelope guards the shipped wire shape: the proposal
// must decode as the canonical priors document with a models map.
func TestProposalIsValidJSONEnvelope(t *testing.T) {
	srv := aaStub(t)
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "model-priors.json.proposed")

	var buf bytes.Buffer

	require.NoError(t, runPriorsRefresh(context.Background(), &buf, testParams(srv, out, testAPIKey)))

	data, err := os.ReadFile(out)
	require.NoError(t, err)

	var doc struct {
		Meta   map[string]any            `json:"meta"`
		Models map[string]map[string]any `json:"models"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))
	assert.NotEmpty(t, doc.Models)
	assert.Equal(t, "docs/model-priors.md", doc.Meta["procedure"])

	// Output ends with a newline and is human-diff-friendly (indented).
	assert.True(t, strings.HasSuffix(string(data), "\n"))
	assert.Contains(t, string(data), "\n  ")
}
