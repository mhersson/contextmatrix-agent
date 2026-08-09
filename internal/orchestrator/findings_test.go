package orchestrator

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindingsToolRecordsAndEchoes(t *testing.T) {
	t.Parallel()

	ft := NewFindingsTool()
	ctx := context.Background()

	res, err := ft.Execute(ctx, map[string]any{"anchor": "prompts.go:412", "fact": "synthesisPrompt has 6 format slots"})
	require.NoError(t, err)
	assert.Contains(t, res.Text, "prompts.go:412")
	assert.Contains(t, res.Text, "synthesisPrompt has 6 format slots")
	assert.Contains(t, res.Text, "RECORDED FINDINGS (1)")

	res, err = ft.Execute(ctx, map[string]any{"fact": "the plan needs one subtask"})
	require.NoError(t, err)
	assert.Contains(t, res.Text, "RECORDED FINDINGS (2)", "the result must echo every finding so far")
	assert.Contains(t, res.Text, "prompts.go:412", "earlier findings must survive in the echoed list")
	assert.Contains(t, res.Text, "the plan needs one subtask")
}

func TestFindingsToolDeduplicates(t *testing.T) {
	t.Parallel()

	ft := NewFindingsTool()
	ctx := context.Background()

	_, err := ft.Execute(ctx, map[string]any{"anchor": "review.go:904", "fact": "fixFiles parses the leading path"})
	require.NoError(t, err)

	// Same content, different spacing and case: must not create a second entry.
	res, err := ft.Execute(ctx, map[string]any{"anchor": "REVIEW.GO:904", "fact": "fixFiles   parses the leading path"})
	require.NoError(t, err)
	assert.Contains(t, res.Text, "RECORDED FINDINGS (1)", "a duplicate must not grow the list")
}

func TestFindingsToolRejectsMissingFact(t *testing.T) {
	t.Parallel()

	ft := NewFindingsTool()

	for name, args := range map[string]map[string]any{
		"absent":     {"anchor": "a.go:1"},
		"empty":      {"fact": "   "},
		"wrong type": {"fact": 42},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := ft.Execute(context.Background(), args)
			require.NoError(t, err, "a bad argument must never fail the run")
			assert.Contains(t, res.Text, `"fact"`, "the result must name the argument the model got wrong")
		})
	}
}

func TestFindingsToolEntryCap(t *testing.T) {
	t.Parallel()

	ft := NewFindingsTool()
	ctx := context.Background()

	for i := range maxFindings {
		_, err := ft.Execute(ctx, map[string]any{"fact": "finding number " + strconv.Itoa(i)})
		require.NoError(t, err)
	}

	res, err := ft.Execute(ctx, map[string]any{"fact": "one too many"})
	require.NoError(t, err)
	assert.Contains(t, res.Text, "cap reached", "hitting the cap must be stated, never silent")
	assert.NotContains(t, res.Text, "one too many", "the rejected finding must not appear in the list")
}

func TestFindingsToolByteCap(t *testing.T) {
	t.Parallel()

	ft := NewFindingsTool()
	ctx := context.Background()

	// Sized so the first entry fits and any further entry cannot.
	res, err := ft.Execute(ctx, map[string]any{"fact": strings.Repeat("y", maxFindingsBytes-10)})
	require.NoError(t, err)
	assert.Contains(t, res.Text, "RECORDED FINDINGS (1)")

	res, err = ft.Execute(ctx, map[string]any{"fact": "this one pushes past the byte cap"})
	require.NoError(t, err)
	assert.Contains(t, res.Text, "cap reached")
	assert.Contains(t, res.Text, "RECORDED FINDINGS (1)", "the rejected entry must not be added")
}

func TestFindingsToolInstancesAreIndependent(t *testing.T) {
	t.Parallel()

	a, b := NewFindingsTool(), NewFindingsTool()
	_, err := a.Execute(context.Background(), map[string]any{"fact": "only in a"})
	require.NoError(t, err)

	res, err := b.Execute(context.Background(), map[string]any{"fact": "only in b"})
	require.NoError(t, err)
	assert.NotContains(t, res.Text, "only in a", "two instances must not share state")
	assert.Contains(t, res.Text, "RECORDED FINDINGS (1)")
}

func TestFindingsToolSchema(t *testing.T) {
	t.Parallel()

	ft := NewFindingsTool()
	assert.Equal(t, "record_finding", ft.Name())

	sc := ft.Schema()
	assert.Equal(t, "function", sc.Type)
	assert.Equal(t, "record_finding", sc.Function.Name)
	assert.Contains(t, string(sc.Function.Parameters), `"fact"`)
	assert.Contains(t, string(sc.Function.Parameters), `"anchor"`)

	// The tool is phase-agnostic: nothing in it may name the planning phase.
	low := strings.ToLower(sc.Function.Description + string(sc.Function.Parameters))
	for _, banned := range []string{"plan", "subtask", "decompos"} {
		assert.NotContainsf(t, low, banned, "the tool must stay phase-agnostic (found %q)", banned)
	}
}
