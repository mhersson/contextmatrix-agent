package eval

import (
	"bufio"
	"strings"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
)

// DefaultCandidates is the shipped curated candidate set (fixtures/candidates.txt;
// blank lines and # comments ignored).
func DefaultCandidates() []string {
	data, _ := fixturesFS.ReadFile("fixtures/candidates.txt") //nolint:errcheck
	return parseLines(string(data))
}

func parseLines(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// FreeToolModels returns the :free, tool-capable catalog models whose context
// window is at least minContext — the cheap regression subset.
func FreeToolModels(cat llm.Catalog, minContext int) []string {
	var out []string
	for _, e := range cat {
		if strings.HasSuffix(e.ID, ":free") && e.SupportsTools() && e.ContextLength >= minContext {
			out = append(out, e.ID)
		}
	}
	return out
}

// EstimateCost is a rough dry-run estimate: per run ≈ approxPromptTok*prompt +
// approxComplTok*completion at catalog prices, × tasks × samples × models. Models
// absent from the catalog contribute 0 (unknown price).
func EstimateCost(cat llm.Catalog, models []string, nTasks, samples, approxPromptTok, approxComplTok int) float64 {
	var total float64
	for _, m := range models {
		e, ok := cat.Find(m)
		if !ok {
			continue
		}
		perRun := float64(approxPromptTok)*e.PromptPricePerTok + float64(approxComplTok)*e.CompletionPricePerTok
		total += perRun * float64(nTasks) * float64(samples)
	}
	return total
}
