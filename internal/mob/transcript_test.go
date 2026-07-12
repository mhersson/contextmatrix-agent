package mob

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestRenderEntry(t *testing.T) {
	tests := []struct {
		name string
		in   Entry
		want string
	}{
		{
			name: "seat with lens",
			in:   Entry{Author: "seat-2", Lens: "security", Round: 1, Content: "finding A"},
			want: "[round 1] seat-2 (security): finding A",
		},
		{
			name: "lens-less human",
			in:   Entry{Author: "human", Round: 2, Content: "please consider Y"},
			want: "[round 2] human: please consider Y",
		},
		{
			name: "lens-less moderator round zero",
			in:   Entry{Author: "moderator", Round: 0, Content: "briefing"},
			want: "[round 0] moderator: briefing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, renderEntry(tt.in))
		})
	}
}

func TestRenderDelta(t *testing.T) {
	entries := []Entry{
		{Author: "moderator", Round: 0, Content: "briefing"},
		{Author: "seat-1", Lens: "feasibility", Round: 0, Content: "pos-1"},
		{Author: "seat-2", Lens: "risk", Round: 0, Content: "pos-2"},
		{Author: "human", Round: 1, Content: "note"},
	}

	t.Run("excludes own entries and appends instruction", func(t *testing.T) {
		got := renderDelta(entries, 1, "seat-1", "Reply now.")

		want := "[round 0] seat-2 (risk): pos-2\n\n[round 1] human: note\n\nReply now."
		assert.Equal(t, want, got)
	})

	t.Run("from index skips earlier entries", func(t *testing.T) {
		assert.Equal(t, "[round 1] human: note", renderDelta(entries, 3, "seat-1", ""))
	})

	t.Run("full snapshot from zero includes briefing", func(t *testing.T) {
		got := renderDelta(entries, 0, "", "")
		assert.True(t, strings.HasPrefix(got, "[round 0] moderator: briefing\n\n"), got)
	})

	t.Run("empty and no instruction is empty", func(t *testing.T) {
		assert.Empty(t, renderDelta(nil, 0, "seat-1", ""))
	})

	t.Run("empty with instruction is the instruction", func(t *testing.T) {
		assert.Equal(t, "go", renderDelta(entries, len(entries), "seat-1", "go"))
	})

	t.Run("from beyond length is clamped", func(t *testing.T) {
		assert.Empty(t, renderDelta(entries, 99, "seat-1", ""))
	})
}

func TestTruncateUtterance(t *testing.T) {
	t.Run("under cap unchanged", func(t *testing.T) {
		assert.Equal(t, "short", truncateUtterance("short"))
	})

	t.Run("exactly cap unchanged", func(t *testing.T) {
		s := strings.Repeat("a", utteranceCap)
		assert.Equal(t, s, truncateUtterance(s))
	})

	t.Run("over cap truncated with marker", func(t *testing.T) {
		s := strings.Repeat("a", utteranceCap+100)

		got := truncateUtterance(s)

		assert.True(t, strings.HasSuffix(got, truncationMarker))
		assert.LessOrEqual(t, len(got), utteranceCap+len(truncationMarker))
		assert.True(t, utf8.ValidString(got))
	})

	t.Run("multibyte rune straddling the cap is cut rune-safe", func(t *testing.T) {
		s := strings.Repeat("a", utteranceCap-1) + "世界"

		got := truncateUtterance(s)

		assert.True(t, utf8.ValidString(got))
		assert.Equal(t, strings.Repeat("a", utteranceCap-1)+truncationMarker, got)
	})
}
