package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/mhersson/contextmatrix-agent/internal/eval"
	"github.com/mhersson/contextmatrix-agent/internal/redact"
	"github.com/mhersson/contextmatrix-agent/internal/registry"
)

const (
	// defaultAAEndpoint is the Artificial Analysis v2 models endpoint. Tests
	// override it with an httptest server URL; the live API is never called from
	// the test suite.
	defaultAAEndpoint = "https://artificialanalysis.ai/api/v2/data/llms/models"

	// priorsProcedure is the documented refresh procedure the priors meta points
	// at. It is committed alongside model-priors.json.
	priorsProcedure = "docs/model-priors.md"

	// livePriorsPath is the embedded priors file the command must NEVER write.
	// runPriorsRefresh refuses any --out that resolves to it; the human renames a
	// proposal over it manually after review.
	livePriorsPath = "internal/registry/data/model-priors.json"

	// proposedPriorsPath is the default output. It is deliberately NOT the live
	// model-priors.json: the command only ever writes a proposal for a human to
	// review and rename.
	proposedPriorsPath = livePriorsPath + ".proposed"

	// aaSourceLabel tags every emitted PriorEntry with its provenance.
	aaSourceLabel = "artificialanalysis"
)

// aaSlugToOpenRouter maps Artificial Analysis model slugs (data[].slug) to the
// OpenRouter slugs the harness routes on. Refresh sessions EXTEND this table as
// the candidate list evolves: when the gap report surfaces a high-ranking AA
// model that is not yet mapped, add its AA-slug -> OpenRouter-slug pair here so
// the next run picks it up. Unmapped AA models are never silently dropped; they
// appear in the gap report instead.
var aaSlugToOpenRouter = map[string]string{
	// cheap floor
	"gpt-5-nano":            "openai/gpt-5-nano",
	"gemini-2-5-flash-lite": "google/gemini-2.5-flash-lite",
	"deepseek-v4-flash":     "deepseek/deepseek-v4-flash",
	"qwen3-coder-flash":     "qwen/qwen3-coder-flash",
	"gpt-oss-20b":           "openai/gpt-oss-20b",
	"gpt-5-4-nano-medium":   "openai/gpt-5.4-nano",
	// mid tier
	"gpt-5-4-mini":           "openai/gpt-5.4-mini",
	"claude-haiku-4-5":       "anthropic/claude-haiku-4.5",
	"gemini-3-flash-preview": "google/gemini-3-flash-preview",
	"deepseek-v3-2":          "deepseek/deepseek-v3.2",
	"glm-4-7":                "z-ai/glm-4.7",
	"qwen3-coder-plus":       "qwen/qwen3-coder-plus",
	"mistral-large-2512":     "mistralai/mistral-large-2512",
	"gpt-5-5-non-reasoning":  "openai/gpt-5.5-non-reasoning",
	// frontier
	"claude-opus-4-8":   "anthropic/claude-opus-4.8",
	"claude-sonnet-4-6": "anthropic/claude-sonnet-4.6",
	"gpt-5-5-medium":    "openai/gpt-5.5",
	"gpt-5-2-codex":     "openai/gpt-5.2-codex",
	"deepseek-v4-pro":   "deepseek/deepseek-v4-pro",
	"qwen3-max":         "qwen/qwen3-max",
	"grok-4-3":          "x-ai/grok-4.3",
	"kimi-k2-6":         "moonshotai/kimi-k2.6",
}

// priorsRefreshParams is the testable parameter bundle for runPriorsRefresh.
// Defaults that depend on the wall clock or env (date, key) are resolved in
// RunE so the business logic stays deterministic.
type priorsRefreshParams struct {
	apiURL       string
	out          string
	date         string
	gapThreshold float64
	key          string
	candidates   []string
}

// aaModel is the subset of an Artificial Analysis data[] entry we consume.
type aaModel struct {
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	ModelCreator struct {
		Slug string `json:"slug"`
	} `json:"model_creator"`
	Evaluations struct {
		// Both indices are ~0..100 and nullable. A null index means no prior for
		// that role — pointers distinguish absent from a real 0.
		CodingIndex       *float64 `json:"artificial_analysis_coding_index"`
		IntelligenceIndex *float64 `json:"artificial_analysis_intelligence_index"`
	} `json:"evaluations"`
}

type aaResponse struct {
	Status int       `json:"status"`
	Data   []aaModel `json:"data"`
}

func newPriorsRefreshCmd() *cobra.Command {
	var p priorsRefreshParams

	cmd := &cobra.Command{
		Use:   "priors-refresh",
		Short: "Propose model-priors updates from Artificial Analysis indices",
		Long: "Fetch Artificial Analysis coding/intelligence indices for the curated " +
			"candidate list and write a human-reviewed proposal (never the live " +
			"priors file). High-ranking models absent from the candidate list are " +
			"reported as suggestions. See docs/model-priors.md.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Key: env is primary; the service config is a fallback for operators who
			// keep it there. The key is helper-only and is redacted everywhere.
			p.key = os.Getenv("CMX_ARTIFICIAL_ANALYSIS_API_KEY")
			if p.key == "" {
				if cfg, err := config.LoadService(""); err == nil {
					p.key = cfg.ArtificialAnalysisAPIKey
				}
			}

			// Default the retrieval date here, not in the business logic, so tests are
			// deterministic (no time.Now in runPriorsRefresh).
			if p.date == "" {
				p.date = time.Now().UTC().Format("2006-01-02")
			}

			// The model universe is the shipped curated candidate list.
			if p.candidates == nil {
				p.candidates = eval.DefaultCandidates()
			}

			return runPriorsRefresh(cmd.Context(), cmd.OutOrStdout(), p)
		},
	}

	cmd.Flags().StringVar(&p.out, "out", proposedPriorsPath,
		"proposal output path (never the live model-priors.json)")
	cmd.Flags().StringVar(&p.apiURL, "api-url", defaultAAEndpoint,
		"Artificial Analysis models endpoint")
	cmd.Flags().StringVar(&p.date, "date", "",
		"retrieval date stamp YYYY-MM-DD (default: today, UTC)")
	cmd.Flags().Float64Var(&p.gapThreshold, "gap-threshold", 0.85,
		"normalized coding-index threshold for gap suggestions [0,1]")

	return cmd
}

// runPriorsRefresh is the testable core. It fetches AA indices, maps AA slugs to
// candidate OpenRouter slugs, writes a proposed priors document, and prints a
// diff-style summary plus a gap report. It NEVER overwrites the live priors
// file: callers point --out at a proposal path.
func runPriorsRefresh(ctx context.Context, w io.Writer, p priorsRefreshParams) error {
	red := redact.New([]string{p.key})

	// Keyless: explain the documented manual procedure and exit without any
	// network call or file write. Keyless refreshes transcribe indices by hand
	// per docs/model-priors.md.
	if strings.TrimSpace(p.key) == "" {
		fmt.Fprintf(w, "no Artificial Analysis API key (CMX_ARTIFICIAL_ANALYSIS_API_KEY); "+ //nolint:errcheck
			"follow the manual transcription procedure in %s\n", priorsProcedure)

		return fmt.Errorf("no API key set: see %s for the manual refresh procedure", priorsProcedure)
	}

	// Hard guard: refuse to write the live priors file even if --out points at it.
	// The command only ever emits a proposal; the human renames it over the live
	// file after review. Compared as absolute paths so "./x", trailing slashes and
	// the cwd-relative default all resolve identically. Checked before any network
	// call so a misdirected --out fails fast.
	if sameFile(p.out, livePriorsPath) {
		return fmt.Errorf("refusing to write the live priors file %s: "+
			"write a .proposed file and rename it yourself (see %s)", livePriorsPath, priorsProcedure)
	}

	models, err := fetchAAModels(ctx, p.apiURL, p.key)
	if err != nil {
		// Defensively redact the key in case it leaked into a URL/transport error.
		return fmt.Errorf("fetch Artificial Analysis data: %s", red.Apply(err.Error()))
	}

	// Per-role normalization maxima across the whole response.
	maxCoding := maxIndex(models, func(m aaModel) *float64 { return m.Evaluations.CodingIndex })
	maxIntel := maxIndex(models, func(m aaModel) *float64 { return m.Evaluations.IntelligenceIndex })

	// Index AA models by slug for candidate lookup, and remember names for the
	// report.
	bySlug := make(map[string]aaModel, len(models))
	for _, m := range models {
		bySlug[m.Slug] = m
	}

	// Invert the slug table so we can find an AA slug for a target OpenRouter slug.
	orToAA := make(map[string]string, len(aaSlugToOpenRouter))
	for aaSlug, orSlug := range aaSlugToOpenRouter {
		orToAA[orSlug] = aaSlug
	}

	candidateSet := make(map[string]struct{}, len(p.candidates))
	for _, c := range p.candidates {
		candidateSet[c] = struct{}{}
	}

	priors := registry.Priors{
		Meta: registry.PriorsMeta{
			Updated:   p.date,
			Procedure: priorsProcedure,
			// Carry the existing tier bars forward so a no-scale-shift refresh is a
			// safe drop-in: a human renaming the proposal over the live file must not
			// wipe the selector's per-role tier floors to null. The docs instruct
			// manual adjustment only when the normalization scale shifts.
			TierBars: registry.DefaultPriors().Meta.TierBars,
		},
		Models: map[string]registry.PriorEntry{},
	}

	var (
		written []string // candidate OpenRouter slugs that got an entry
		missing []string // candidates with no AA data (not in the response or unmapped)
	)

	for _, orSlug := range p.candidates {
		aaSlug, ok := orToAA[orSlug]
		if !ok {
			missing = append(missing, fmt.Sprintf("%s (no AA-slug mapping)", orSlug))

			continue
		}

		m, ok := bySlug[aaSlug]
		if !ok {
			missing = append(missing, fmt.Sprintf("%s (AA slug %q absent from response)", orSlug, aaSlug))

			continue
		}

		entry := registry.PriorEntry{
			Source:    aaSourceLabel,
			Retrieved: p.date,
		}
		// Null index -> no prior for that role. Never emit a 0.
		entry.Coder = normalize(m.Evaluations.CodingIndex, maxCoding)
		entry.Reviewer = normalize(m.Evaluations.IntelligenceIndex, maxIntel)

		priors.Models[orSlug] = entry
		written = append(written, orSlug)
	}

	// Gap report: AA models clearing the normalized coding threshold whose mapped
	// OpenRouter slug (or, if unmapped, the AA model itself) is NOT a candidate.
	// Nothing here is auto-added — these are suggestions only.
	gaps := gapSuggestions(models, maxCoding, p.gapThreshold, candidateSet)

	if err := writeProposal(p.out, priors); err != nil {
		return err
	}

	renderRefreshReport(w, p, written, missing, gaps)

	return nil
}

// fetchAAModels GETs the AA endpoint with the key header and decodes the data
// array. The key is sent only in the request header, never logged.
func fetchAAModels(ctx context.Context, url, key string) ([]aaModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("x-api-key", key)
	req.Header.Set("Accept", "application/json")

	// Bound the whole exchange so a hung AA endpoint cannot block the CLI forever.
	// Context is already threaded for cancellation; this caps dial+read.
	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var parsed aaResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return parsed.Data, nil
}

// maxIndex returns the maximum non-nil index selected by sel across models, or 0
// when none are present.
func maxIndex(models []aaModel, sel func(aaModel) *float64) float64 {
	var top float64
	for _, m := range models {
		if v := sel(m); v != nil && *v > top {
			top = *v
		}
	}

	return top
}

// normalize divides idx by the response-wide maximum and clamps to [0,1]. A nil
// idx (null in the response) yields nil: no prior for that role. A non-positive
// maximum also yields nil to avoid divide-by-zero producing a bogus prior.
func normalize(idx *float64, maximum float64) *float64 {
	if idx == nil || maximum <= 0 {
		return nil
	}

	n := *idx / maximum
	if n < 0 {
		n = 0
	}

	if n > 1 {
		n = 1
	}

	return &n
}

// sameFile reports whether a and b name the same path. It compares cleaned
// absolute paths so relative, "./"-prefixed, and trailing-slash spellings all
// match. If either path cannot be resolved to absolute, it falls back to a
// cleaned-string comparison rather than reporting a false negative.
func sameFile(a, b string) bool {
	absA, errA := filepath.Abs(a)

	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}

	return absA == absB
}

// gapSuggestions lists AA models whose normalized coding index clears threshold
// but which are absent from the candidate set, sorted by descending normalized
// coding index. An AA model maps to a candidate via the slug table; if it has no
// mapping it can never be a candidate, so it is reported under its AA slug.
func gapSuggestions(models []aaModel, maxCoding, threshold float64, candidateSet map[string]struct{}) []string {
	type sug struct {
		line string
		norm float64
	}

	var out []sug

	for _, m := range models {
		nptr := normalize(m.Evaluations.CodingIndex, maxCoding)
		if nptr == nil || *nptr < threshold {
			continue
		}

		orSlug, mapped := aaSlugToOpenRouter[m.Slug]
		if mapped {
			if _, isCandidate := candidateSet[orSlug]; isCandidate {
				continue // already covered
			}

			out = append(out, sug{
				line: fmt.Sprintf("%s (AA %q, %s) coding=%.2f -> not in candidates.txt",
					orSlug, m.Slug, m.Name, *nptr),
				norm: *nptr,
			})

			continue
		}

		out = append(out, sug{
			line: fmt.Sprintf("%s (%s, creator %s) coding=%.2f -> unmapped; add to aaSlugToOpenRouter if wanted",
				m.Slug, m.Name, m.ModelCreator.Slug, *nptr),
			norm: *nptr,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].norm > out[j].norm })

	lines := make([]string, len(out))
	for i, s := range out {
		lines[i] = s.line
	}

	return lines
}

// writeProposal serializes the priors document to path with stable, indented,
// diff-friendly JSON. It writes to the given path verbatim — callers default
// this to a .proposed path so the live file is never touched. The parent
// directory is created if missing; the live-path guard in runPriorsRefresh runs
// first, so a blocked write never reaches here to create the live dir tree.
func writeProposal(path string, priors registry.Priors) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create proposal dir for %q: %w", path, err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create proposal %q: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")

	if err := enc.Encode(priors); err != nil {
		return fmt.Errorf("encode proposal: %w", err)
	}

	return nil
}

// renderRefreshReport prints the diff-style summary, the unmatched/missing
// candidates, the gap suggestions, and the human follow-up instructions. The
// key never appears here.
func renderRefreshReport(w io.Writer, p priorsRefreshParams, written, missing, gaps []string) {
	sort.Strings(written)
	sort.Strings(missing)

	fmt.Fprintf(w, "priors-refresh: wrote PROPOSAL to %s (date=%s)\n", p.out, p.date) //nolint:errcheck

	fmt.Fprintf(w, "\n=== priors written (%d candidates) ===\n", len(written)) //nolint:errcheck

	for _, m := range written {
		fmt.Fprintf(w, "  + %s\n", m) //nolint:errcheck
	}

	if len(missing) > 0 {
		fmt.Fprintf(w, "\n=== unmatched candidates (no AA data; left without a prior) ===\n") //nolint:errcheck

		for _, m := range missing {
			fmt.Fprintf(w, "  ? %s\n", m) //nolint:errcheck
		}
	}

	fmt.Fprintf(w, "\n=== gap report (AA coding >= %.2f normalized, absent from candidates.txt; NOT auto-added) ===\n", //nolint:errcheck
		p.gapThreshold)

	if len(gaps) == 0 {
		fmt.Fprintln(w, "  (none)") //nolint:errcheck
	}

	for _, g := range gaps {
		fmt.Fprintf(w, "  ! %s\n", g) //nolint:errcheck
	}

	fmt.Fprintf(w, "\nReview the proposal, cross-check against llm-stats, update tier bars if the "+ //nolint:errcheck
		"scale shifted, then rename over internal/registry/data/model-priors.json, re-embed via "+
		"build, and commit. See %s.\n", priorsProcedure)
}
