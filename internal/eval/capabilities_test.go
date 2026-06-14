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

// TestMetaStamp: WriteCapabilitiesWithMeta output round-trips a fully populated
// meta envelope — date, samples, task_library_hash, routing, harness version, floor.
func TestMetaStamp(t *testing.T) {
	caps := map[string]map[registry.Role]float64{"m": {registry.RoleCoder: 0.7}}
	meta := registry.CapabilitiesMeta{
		Date:            "2026-06-13",
		Samples:         15,
		TaskLibraryHash: TaskLibraryHash(),
		Routing:         "throughput",
		HarnessVersion:  HarnessVersion,
		Floor:           CalibratedFloor(15, 1.96),
	}

	var buf bytes.Buffer
	require.NoError(t, WriteCapabilitiesWithMeta(&buf, caps, meta))

	gotCaps, gotMeta, err := registry.LoadCapabilitiesWithMeta(&buf)
	require.NoError(t, err)
	assert.InDelta(t, 0.7, gotCaps["m"][registry.RoleCoder], 1e-9)
	assert.Equal(t, "2026-06-13", gotMeta.Date)
	assert.Equal(t, 15, gotMeta.Samples)
	assert.NotEmpty(t, gotMeta.TaskLibraryHash)
	assert.Equal(t, meta.TaskLibraryHash, gotMeta.TaskLibraryHash)
	assert.Equal(t, "throughput", gotMeta.Routing)
	assert.Equal(t, HarnessVersion, gotMeta.HarnessVersion)
	assert.InDelta(t, meta.Floor, gotMeta.Floor, 1e-9)
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
