package runkind

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLimitCheckCandidates(t *testing.T) {
	long := "./run " + strings.Repeat("x", 600)

	var cands []string

	for i := range 40 {
		if i == 2 {
			cands = append(cands, long)

			continue
		}

		cands = append(cands, fmt.Sprintf("./run t%d", i))
	}

	got := limitCheck(protocol.CheckResult{Candidates: cands})

	require.Len(t, got.Candidates, 32)
	assert.NotContains(t, got.Candidates, long)
	assert.Equal(t, "./run t32", got.Candidates[31], "the first 32 after the drop are kept")

	for _, c := range got.Candidates {
		assert.LessOrEqual(t, len(c), 512)
	}

	assert.Equal(t, long, cands[2], "the input is not modified")
}

func TestLimitCheckKeepsAnEmptyListNonNil(t *testing.T) {
	got := limitCheck(protocol.CheckResult{})
	require.NotNil(t, got.Candidates)
	assert.Empty(t, got.Candidates)
	assert.Nil(t, got.AbsentFromRepo)
}

func TestLimitCheckAbsentFromRepo(t *testing.T) {
	absent := make([]string, 70)
	for i := range absent {
		absent[i] = fmt.Sprintf("https://host%d.example.net", i)
	}

	absent[1] = strings.Repeat("é", 300)

	got := limitCheck(protocol.CheckResult{AbsentFromRepo: absent})

	require.Len(t, got.AbsentFromRepo, 64)
	assert.Equal(t, absent[0], got.AbsentFromRepo[0])
	assert.Equal(t, absent[63], got.AbsentFromRepo[63], "a long entry is truncated, never dropped")
	assert.LessOrEqual(t, len(got.AbsentFromRepo[1]), 512)
	assert.True(t, utf8.ValidString(got.AbsentFromRepo[1]))
	assert.True(t, strings.HasPrefix(absent[1], got.AbsentFromRepo[1]))
	assert.Equal(t, strings.Repeat("é", 300), absent[1], "the input is not modified")
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc"},
		{"aé", 2, "a"},
		{"éé", 3, "é"},
		{"é", 0, ""},
	}

	for _, tt := range tests {
		got := truncate(tt.in, tt.n)
		assert.Equal(t, tt.want, got, "%q at %d", tt.in, tt.n)
		assert.True(t, utf8.ValidString(got))
	}
}
