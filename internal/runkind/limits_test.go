package runkind

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-agent/internal/runoutcome"
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

func TestLimitInvestigate(t *testing.T) {
	t.Run("diagnosis is truncated at a rune boundary", func(t *testing.T) {
		in := protocol.InvestigateResult{Diagnosis: strings.Repeat("é", 33<<9)}
		got := limitInvestigate(in)
		assert.LessOrEqual(t, len(got.Diagnosis), 32<<10)
		assert.True(t, utf8.ValidString(got.Diagnosis))
		assert.True(t, strings.HasPrefix(in.Diagnosis, got.Diagnosis))
	})

	t.Run("fix", func(t *testing.T) {
		tests := []struct {
			name string
			fix  protocol.CardFix
			kept bool
		}{
			{"section at the limit", protocol.CardFix{Section: strings.Repeat("s", 16<<10)}, true},
			{"section over the limit", protocol.CardFix{Section: strings.Repeat("s", 16<<10+1)}, false},
			{"50 retired subtasks", protocol.CardFix{RetireSubtasks: ids(50)}, true},
			{"51 retired subtasks", protocol.CardFix{RetireSubtasks: ids(51)}, false},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				fix := tt.fix
				got := limitInvestigate(protocol.InvestigateResult{Fix: &fix})
				assert.Equal(t, tt.kept, got.Fix != nil)
			})
		}
	})

	t.Run("forward_to", func(t *testing.T) {
		assert.Len(t, limitInvestigate(protocol.InvestigateResult{ForwardTo: ids(50)}).ForwardTo, 50)
		assert.Nil(t, limitInvestigate(protocol.InvestigateResult{ForwardTo: ids(51)}).ForwardTo)
	})

	t.Run("findings", func(t *testing.T) {
		in := make([]protocol.FrictionFinding, 55)
		for i := range in {
			in[i] = protocol.FrictionFinding{Kind: protocol.FindingGuidance, Text: fmt.Sprintf("f%d", i), Evidence: "e"}
		}

		in[1].Title = strings.Repeat("t", 201)
		in[2].Text = strings.Repeat("x", 16<<10+1)
		in[3].Evidence = strings.Repeat("é", 5<<9)

		got := limitInvestigate(protocol.InvestigateResult{Findings: in}).Findings

		require.Len(t, got, 50)
		assert.Equal(t, "f0", got[0].Text)
		assert.Equal(t, "f3", got[1].Text, "the 2nd and 3rd are dropped")
		assert.Equal(t, "f51", got[49].Text)
		assert.LessOrEqual(t, len(got[1].Evidence), 4<<10)
		assert.True(t, utf8.ValidString(got[1].Evidence))
		assert.Len(t, in[3].Evidence, 5<<10, "the input is not modified")
	})
}

func ids(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("ABC-%d", i+1)
	}

	return out
}

func TestFitOutcomeDropsTrailingFindings(t *testing.T) {
	full := func() *protocol.InvestigateResult {
		findings := make([]protocol.FrictionFinding, 50)
		for i := range findings {
			findings[i] = protocol.FrictionFinding{
				Kind:     protocol.FindingCode,
				Title:    fmt.Sprintf("%03d", i) + strings.Repeat("t", 197),
				Text:     strings.Repeat("x", 16<<10),
				Evidence: strings.Repeat("e", 4<<10),
			}
		}

		return &protocol.InvestigateResult{
			Diagnosis: strings.Repeat("d", 32<<10),
			Cause:     protocol.DiagnosisCard,
			Fix:       &protocol.CardFix{Section: strings.Repeat("s", 16<<10)},
			Findings:  findings,
		}
	}

	in := full()
	d := runoutcome.Data{Cause: protocol.StopCauseDone, Result: &protocol.RunKindResult{Investigate: in}}

	got := fitOutcome(d, maxOutcomeBytes)

	enc, err := json.Marshal(got)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(enc), 1000<<10)

	inv := got.Result.Investigate
	assert.Equal(t, in.Diagnosis, inv.Diagnosis)
	assert.Equal(t, in.Fix, inv.Fix)
	assert.Less(t, len(inv.Findings), 50)
	assert.NotEmpty(t, inv.Findings)
	assert.Equal(t, in.Findings[:len(inv.Findings)], inv.Findings, "the first findings are kept, in order")

	t.Run("a small result is unchanged", func(t *testing.T) {
		small := runoutcome.Data{Cause: protocol.StopCauseDone, Result: &protocol.RunKindResult{
			Investigate: &protocol.InvestigateResult{Diagnosis: "d", Cause: protocol.DiagnosisModel, Findings: []protocol.FrictionFinding{{Text: "t"}}},
		}}
		assert.Equal(t, small, fitOutcome(small, maxOutcomeBytes))
	})

	t.Run("nothing but findings is dropped", func(t *testing.T) {
		big := runoutcome.Data{Cause: protocol.StopCauseDone, Result: &protocol.RunKindResult{
			Investigate: &protocol.InvestigateResult{Diagnosis: strings.Repeat("d", 2048)},
		}}
		assert.Equal(t, big, fitOutcome(big, 1024))
	})
}
