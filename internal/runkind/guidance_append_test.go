package runkind

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mhersson/contextmatrix-agent/internal/orchestrator"
)

func TestWithGuidance(t *testing.T) {
	capLen := orchestrator.GroundingByteCap
	// bulk pads a file past the grounding cap with ordinary lines.
	bulk := strings.Repeat("filler line\n", capLen/len("filler line\n")+1)
	withH1 := "# Repo\n\nIntro.\n" + bulk
	noH1 := "Intro.\n" + bulk
	lines := "Run the tests.\nLint too.\n"

	tests := []struct {
		name, file, section, want string
	}{
		{"empty file", "", "Run the tests.", "Run the tests.\n"},
		{"only newlines", "\n\n\n", "Run the tests.", "Run the tests.\n"},
		{"one trailing newline", "# Repo\n", "Run the tests.", "# Repo\n\nRun the tests.\n"},
		{"several trailing newlines", "# Repo\n\n\n\n", "Run the tests.", "# Repo\n\nRun the tests.\n"},
		{"no trailing newline", "# Repo", "Run the tests.", "# Repo\n\nRun the tests.\n"},
		{"section with trailing newlines", "# Repo\n", "Run the tests.\nLint too.\n\n\n", "# Repo\n\n" + lines},
		{
			"newer base replaces an older revision", "# Repo\n\nHuman edit.\n", "Run the tests.\nLint too.\nMore.",
			"# Repo\n\nHuman edit.\n\nRun the tests.\nLint too.\nMore.\n",
		},
		{
			"past the cap with an H1", withH1, "Run the tests.\nLint too.",
			"# Repo\n\n" + lines + "\n\nIntro.\n" + bulk,
		},
		{
			"past the cap without an H1", noH1, "Run the tests.\nLint too.",
			lines + "\n" + noH1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, withGuidance(tt.file, tt.section))
		})
	}
}

func TestWithGuidanceAtTheCap(t *testing.T) {
	section := "Run the tests."
	lines := section + "\n"
	// The joined text is exactly the cap: it still goes at the end.
	file := "# Repo\n" + strings.Repeat("x", orchestrator.GroundingByteCap-len("# Repo\n")-2-len(lines)) + "\n"
	got := withGuidance(file, section)
	assert.Len(t, got, orchestrator.GroundingByteCap)
	assert.True(t, strings.HasSuffix(got, "\n\n"+lines))

	// One byte more crosses it.
	got = withGuidance("y"+file, section)
	assert.True(t, strings.HasPrefix(got, lines+"\ny# Repo\n"), "no line starts with '# ', so the lines go to the top")
}
