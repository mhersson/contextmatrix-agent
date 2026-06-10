package eval

import (
	"bytes"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteCapabilitiesRoundTrip(t *testing.T) {
	caps := map[string]map[registry.Role]float64{"m": {registry.RoleCoder: 0.7}}

	var buf bytes.Buffer
	require.NoError(t, WriteCapabilities(&buf, caps))
	got, err := registry.LoadCapabilities(&buf)
	require.NoError(t, err)
	assert.InDelta(t, 0.7, got["m"][registry.RoleCoder], 1e-9)
}

func TestRenderScoresStable(t *testing.T) {
	scores := map[string]map[registry.Role]float64{
		"b/model": {registry.RoleCoder: 0.8},
		"a/model": {registry.RoleReviewer: 0.5},
	}

	var buf bytes.Buffer
	RenderScores(&buf, MatrixResult{}, scores)
	out := buf.String()
	assert.Less(t, indexOf(out, "a/model"), indexOf(out, "b/model")) // sorted
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}

	return -1
}
