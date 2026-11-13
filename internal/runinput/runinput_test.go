package runinput_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/runinput"
)

func TestStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".run-inputs")
	s := runinput.New(root)

	dir, err := s.Create("ABC-12-check-ab12cd34")
	require.NoError(t, err)
	assert.Equal(t, root, filepath.Dir(dir))

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	_, err = s.Create("ABC-12-check-ab12cd34")
	require.Error(t, err, "a run's dir is created once")

	escaped, err := s.Create("../x")
	require.NoError(t, err)
	assert.Equal(t, root, filepath.Dir(escaped))
	assert.NotContains(t, filepath.Base(escaped), ".")

	s.Remove("ABC-12-check-ab12cd34")
	s.Remove("ABC-12-check-ab12cd34")

	_, err = os.Stat(dir)
	assert.True(t, os.IsNotExist(err))

	require.NoError(t, s.CleanupOrphans())

	_, err = os.Stat(escaped)
	assert.True(t, os.IsNotExist(err))
}
