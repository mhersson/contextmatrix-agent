package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initGitRepo makes root a git repo isolated from the developer's global config
// and stages a single README.md, leaving the tree effectively empty. Staging is
// enough: git ls-files reports the index, so a committed vs. staged file is
// indistinguishable to the snapshot. Reuses gitInit/gitAddAll (grounding_test.go)
// which skip when git is unavailable, matching the package's exec.LookPath
// convention.
func initGitRepo(t *testing.T, root string) {
	t.Helper()

	gitInit(t, root)
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("homelander\n"), 0o644))
	gitAddAll(t, root)
}

func TestRepoSnapshotGreenfield(t *testing.T) {
	root := t.TempDir()
	initGitRepo(t, root)

	s := repoSnapshot(root)
	assert.Contains(t, s, "effectively EMPTY")
	assert.Contains(t, s, "README.md")
	assert.Contains(t, s, "greenfield")
}

func TestRepoSnapshotCapsFileList(t *testing.T) {
	root := t.TempDir()
	gitInit(t, root)

	for i := range 250 {
		name := fmt.Sprintf("file%03d.txt", i)
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644))
	}

	gitAddAll(t, root)

	s := repoSnapshot(root)
	assert.Contains(t, s, "and 50 more")
	assert.LessOrEqual(t, strings.Count(s, "\n"), 270)
}

func TestRepoSnapshotNoGit(t *testing.T) {
	assert.Empty(t, repoSnapshot(t.TempDir()))
}
