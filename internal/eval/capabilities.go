package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
)

// WriteCapabilities serializes scores as pretty JSON (the capabilities.json the
// registry loads).
func WriteCapabilities(w io.Writer, caps map[string]map[registry.Role]float64) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(caps)
}

// RenderScores prints a stable, human-readable score table plus the run total.
func RenderScores(w io.Writer, mr MatrixResult, scores map[string]map[registry.Role]float64) {
	cost := map[string]float64{}
	for _, o := range mr.Outcomes {
		cost[o.Model] += o.Cost
	}
	models := make([]string, 0, len(scores))
	for m := range scores {
		models = append(models, m)
	}
	sort.Strings(models)
	fmt.Fprintf(w, "\n=== capability scores (Wilson LB) ===\n")                    //nolint:errcheck
	fmt.Fprintf(w, "%-44s %-8s %-8s %-9s\n", "model", "coder", "reviewer", "cost") //nolint:errcheck
	for _, m := range models {
		fmt.Fprintf(w, "%-44s %-8.2f %-8.2f %-9.5f\n", //nolint:errcheck
			m, scores[m][registry.RoleCoder], scores[m][registry.RoleReviewer], cost[m])
	}
	fmt.Fprintf(w, "total_cost_usd=%.5f aborted=%v\n", mr.TotalCost, mr.Aborted) //nolint:errcheck
}
