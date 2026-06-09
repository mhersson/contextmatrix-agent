package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReviewerFixturesProvision asserts every reviewer fixture provisions a
// REVIEW.diff plus its source, and that every mutant's planted symbol appears in
// the provisioned source (so a correct reviewer can actually cite it). It also
// checks the set is balanced (>= 3 mutants and >= 3 clean).
func TestReviewerFixturesProvision(t *testing.T) {
	tasks, err := DefaultTasks("reviewer")
	require.NoError(t, err)
	require.NotEmpty(t, tasks)

	var mutants, clean int
	for _, task := range tasks {
		rt, ok := task.(ReviewerTask)
		require.True(t, ok)
		if rt.wantApprove {
			clean++
		} else {
			mutants++
		}
		t.Run(rt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, rt.Provision(dir))
			assert.FileExists(t, filepath.Join(dir, "REVIEW.diff"))

			if rt.wantApprove {
				return
			}
			require.NotEmpty(t, rt.plantedSymbol, "a mutant must name a planted symbol")
			found := false
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			for _, e := range entries {
				if !strings.HasSuffix(e.Name(), ".go") {
					continue
				}
				b, err := os.ReadFile(filepath.Join(dir, e.Name()))
				require.NoError(t, err)
				if strings.Contains(string(b), rt.plantedSymbol) {
					found = true
				}
			}
			assert.True(t, found, "planted symbol %q must appear in a provisioned .go source", rt.plantedSymbol)
		})
	}
	assert.GreaterOrEqual(t, mutants, 3, "want a balanced mutant set")
	assert.GreaterOrEqual(t, clean, 3, "want a balanced clean set")
}
