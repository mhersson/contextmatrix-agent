package tools

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHeadTail(t *testing.T) {
	t.Run("under limit unchanged", func(t *testing.T) {
		assert.Equal(t, "short", HeadTail("short", 100))
	})
	t.Run("over limit keeps head and tail with marker", func(t *testing.T) {
		in := strings.Repeat("a", 500) + strings.Repeat("z", 500)
		out := HeadTail(in, 300)
		assert.LessOrEqual(t, len(out), 300+80) // marker allowance
		assert.True(t, strings.HasPrefix(out, "aaa"))
		assert.True(t, strings.HasSuffix(out, "zzz"))
		assert.Contains(t, out, "bytes truncated")
	})
	t.Run("zero max disables", func(t *testing.T) {
		in := strings.Repeat("a", 100)
		assert.Equal(t, in, HeadTail(in, 0))
	})
}
