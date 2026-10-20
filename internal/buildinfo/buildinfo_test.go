package buildinfo

import (
	"testing"
	"testing/fstest"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stamp sets the ldflags vars the way a stamped build would, restoring them
// when the test ends.
func stamp(t *testing.T, v, c, bt string) {
	t.Helper()

	oldVersion, oldCommit, oldBuildTime := version, commit, buildTime
	version, commit, buildTime = v, c, bt

	t.Cleanup(func() {
		version, commit, buildTime = oldVersion, oldCommit, oldBuildTime
	})
}

func TestBuild_CarriesStampedValues(t *testing.T) {
	stamp(t, "v1.2.3", "0123456789abcdef0123456789abcdef01234567", "2026-10-21T08:00:00Z")

	got := build(fstest.MapFS{})

	assert.Equal(t, "v1.2.3", got.Version)
	assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", got.Commit)
	assert.Equal(t, "2026-10-21T08:00:00Z", got.BuildTime)
}

func TestBuild_NoHistoryFileGivesEmptyChanges(t *testing.T) {
	got := build(fstest.MapFS{})

	require.NotNil(t, got.Changes)
	assert.Empty(t, got.Changes)
}

func TestBuild_ParsesHistoryFile(t *testing.T) {
	raw := "2026-10-20T10:00:00+02:00\x1fMerge pull request #236 from mhersson/fix/no-ci-fast-pass\x1f" +
		"fix(worker): pass the CI gate at once on a repo with no CI\x1e"

	got := build(fstest.MapFS{"history/merges.log": {Data: []byte(raw)}})

	require.Len(t, got.Changes, 1)
	assert.Equal(t, 236, got.Changes[0].Number)
	assert.Equal(t, protocol.ChangeKindFix, got.Changes[0].Kind)
}

func TestInfo_ChangesNeverNil(t *testing.T) {
	assert.NotNil(t, Info().Changes)
}
