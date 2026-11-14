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

func TestLimitFindings(t *testing.T) {
	in := make([]protocol.FrictionFinding, 55)
	for i := range in {
		in[i] = protocol.FrictionFinding{Kind: protocol.FindingGuidance, Text: fmt.Sprintf("f%d", i), Evidence: "e"}
	}

	in[1].Title = strings.Repeat("t", 201)
	in[2].Text = strings.Repeat("x", 16<<10+1)
	in[3].Evidence = strings.Repeat("é", 5<<9)

	got := limitFindings(in)

	require.Len(t, got, 50)
	assert.Equal(t, "f0", got[0].Text)
	assert.Equal(t, "f3", got[1].Text, "the 2nd and 3rd are dropped")
	assert.Equal(t, "f51", got[49].Text)
	assert.LessOrEqual(t, len(got[1].Evidence), 4<<10)
	assert.True(t, utf8.ValidString(got[1].Evidence))
	assert.Len(t, in[3].Evidence, 5<<10, "the input is not modified")

	empty := limitFindings([]protocol.FrictionFinding{})
	assert.NotNil(t, empty)
	assert.Empty(t, empty)
	assert.Nil(t, limitFindings(nil))
}

func TestMaskFindings(t *testing.T) {
	mask := func(s string) string { return strings.ReplaceAll(s, "secret", "[R]") }
	in := []protocol.FrictionFinding{{Kind: "k-secret", Title: "t-secret", Text: "x-secret", Evidence: "e-secret"}}

	got := maskFindings(in, mask)

	assert.Equal(t, []protocol.FrictionFinding{{Kind: "k-[R]", Title: "t-[R]", Text: "x-[R]", Evidence: "e-[R]"}}, got)
	assert.Equal(t, "t-secret", in[0].Title, "the input is not modified")
	assert.Nil(t, maskFindings(nil, mask))
}

func TestLimitAnalyse(t *testing.T) {
	got := limitAnalyse(protocol.AnalyseResult{})
	assert.NotNil(t, got.Findings)
	assert.Empty(t, got.Findings)

	got = limitAnalyse(protocol.AnalyseResult{Findings: make([]protocol.FrictionFinding, 51)})
	assert.Len(t, got.Findings, 50)
}

func TestFitOutcomeDropsTrailingAnalyseFindings(t *testing.T) {
	findings := make([]protocol.FrictionFinding, 50)
	for i := range findings {
		findings[i] = protocol.FrictionFinding{
			Kind:     protocol.FindingCode,
			Title:    fmt.Sprintf("%03d", i) + strings.Repeat("t", 197),
			Text:     strings.Repeat("x", 16<<10),
			Evidence: strings.Repeat("e", 4<<10),
		}
	}

	d := runoutcome.Data{Cause: protocol.StopCauseDone, Result: &protocol.RunKindResult{Analyse: &protocol.AnalyseResult{Findings: findings}}}

	got := fitOutcome(d, maxOutcomeBytes)

	enc, err := json.Marshal(got)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(enc), 1000<<10)

	kept := got.Result.Analyse.Findings
	assert.NotEmpty(t, kept)
	assert.Less(t, len(kept), 50)
	assert.Equal(t, findings[:len(kept)], kept, "the first findings are kept, in order")
	assert.Len(t, findings, 50, "the input is not modified")
}
