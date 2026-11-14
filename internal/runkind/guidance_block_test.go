package runkind

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
)

func TestGuidanceFile(t *testing.T) {
	tests := []struct {
		name                 string
		hasAgents, hasClaude bool
		want                 string
	}{
		{"both", true, true, "AGENTS.md"},
		{"agents only", true, false, "AGENTS.md"},
		{"claude only", false, true, "CLAUDE.md"},
		{"neither", false, false, "AGENTS.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, guidanceFile(tt.hasAgents, tt.hasClaude))
		})
	}
}

func TestBlockMarkers(t *testing.T) {
	start, end := blockMarkers("alpha")
	assert.Equal(t, "<!-- sentinel:alpha:start -->", start)
	assert.Equal(t, "<!-- sentinel:alpha:end -->", end)
}

func TestWithGuidance(t *testing.T) {
	const (
		aStart = "<!-- sentinel:alpha:start -->\n"
		aEnd   = "<!-- sentinel:alpha:end -->\n"
		head   = "## Sentinel guidance\n\n"
		bBlock = "<!-- sentinel:beta:start -->\n## Sentinel guidance\n\nBeta rule.\n<!-- sentinel:beta:end -->\n"
	)

	newBlock := aStart + head + "New rule.\n" + aEnd
	filler := strings.Repeat("x", orchestrator.GroundingByteCap)
	fit := strings.Repeat("y", orchestrator.GroundingByteCap-len("\n\n"+newBlock))

	edited := aStart + head + "Run ./ci/verify first, always.\n"
	moved := edited + "\nNew rule.\n" + aEnd

	tests := []struct {
		name    string
		file    string
		section string
		want    string
		wantErr bool
	}{
		{"empty file", "", "New rule.", newBlock, false},
		{"empty file, trailing newlines", "", "New rule.\n\n", newBlock, false},
		{
			"file without block", "# Repo\n\nRules.\n", "New rule.",
			"# Repo\n\nRules.\n\n" + newBlock, false,
		},
		{
			"marker mentioned inline is not a marker", "# Repo\n\nWrap blocks in `<!-- sentinel:alpha:start -->` and `<!-- sentinel:alpha:end -->`.\n", "New rule.",
			"# Repo\n\nWrap blocks in `<!-- sentinel:alpha:start -->` and `<!-- sentinel:alpha:end -->`.\n\n" + newBlock, false,
		},
		{
			"file without block, many trailing newlines", "# Repo\n\nRules.\n\n\n", "New rule.\n",
			"# Repo\n\nRules.\n\n" + newBlock, false,
		},
		{
			"human-edited block",
			"# Repo\n\n" + aStart + head + "Run ./ci/verify first, always.\n" + aEnd + "Tail.\n", "New rule.\n",
			"# Repo\n\n" + aStart + head + "Run ./ci/verify first, always.\n\nNew rule.\n" + aEnd + "Tail.\n", false,
		},
		{
			"block, section without newline", aStart + head + "Edited.\n" + aEnd, "New rule.",
			aStart + head + "Edited.\n\nNew rule.\n" + aEnd, false,
		},
		{
			"other project's block", "# Repo\n\n" + bBlock, "New rule.",
			"# Repo\n\n" + bBlock + "\n" + newBlock, false,
		},
		{"two starts", aStart + aStart + aEnd, "New rule.", "", true},
		{"two ends", aStart + aEnd + aEnd, "New rule.", "", true},
		{"end only", "# Repo\n" + aEnd, "New rule.", "", true},
		{"start only", "# Repo\n" + aStart, "New rule.", "", true},
		{"end before start", aEnd + aStart, "New rule.", "", true},
		{"exactly at the cap", fit, "New rule.", fit + "\n\n" + newBlock, false},
		{
			"past the cap, after title", "# Repo\n" + filler, "New rule.",
			"# Repo\n\n" + newBlock + "\n" + filler, false,
		},
		{
			"existing block past the cap moves after title",
			"# Repo\n\n" + filler + "\n\n" + edited + aEnd, "New rule.",
			"# Repo\n\n" + moved + "\n\n" + filler + "\n", false,
		},
		{
			"existing block past the cap moves to the top without title",
			filler + "\n\n" + edited + aEnd, "New rule.",
			moved + "\n" + filler + "\n", false,
		},
		{"past the cap, no title", filler, "New rule.", newBlock + "\n" + filler, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := withGuidance(tt.file, "alpha", tt.section)
			if tt.wantErr {
				require.ErrorIs(t, err, errMalformedBlock)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
