package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGlobTool(t *testing.T) {
	if fdBinary() == "" {
		if _, err := exec.LookPath("rg"); err != nil {
			t.Skip("neither fd nor rg installed")
		}
	}

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package x\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "b.go"), []byte("package y\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "c.txt"), []byte("nope\n"), 0o644))

	out, err := NewGlobTool(root).Execute(context.Background(), map[string]any{"pattern": "*.go"})
	require.NoError(t, err)
	assert.Contains(t, out, "a.go")
	assert.Contains(t, out, filepath.Join("sub", "b.go"))
	assert.NotContains(t, out, "c.txt")

	out, err = NewGlobTool(root).Execute(context.Background(), map[string]any{"pattern": "*.nope"})
	require.NoError(t, err)
	assert.Contains(t, out, "no matches")
}

func TestGlobToolRespectsGitignore(t *testing.T) {
	if fdBinary() == "" {
		if _, err := exec.LookPath("rg"); err != nil {
			t.Skip("neither fd nor rg installed")
		}
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	root := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	require.NoError(t, cmd.Run())
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.go\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "kept.go"), []byte("package x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "ignored.go"), []byte("package x\n"), 0o644))

	out, err := NewGlobTool(root).Execute(context.Background(), map[string]any{"pattern": "*.go"})
	require.NoError(t, err)
	assert.Contains(t, out, "kept.go")
	assert.NotContains(t, out, "ignored.go")
}

func TestFilterByGlob(t *testing.T) {
	files := []string{"a.go", "sub/b.go", "sub/deep/d.go", "c.txt"}

	// No separator → match the base name (fd's default), so nested files match too.
	assert.Equal(t, []string{"a.go", "sub/b.go", "sub/deep/d.go"}, filterByGlob(files, "*.go"))
	// Leading **/ is stripped, then base-name matched.
	assert.Equal(t, []string{"a.go", "sub/b.go", "sub/deep/d.go"}, filterByGlob(files, "**/*.go"))
	assert.Equal(t, []string{"c.txt"}, filterByGlob(files, "*.txt"))
	// Pattern with a separator → match the relative path; * does not cross '/'.
	assert.Equal(t, []string{"sub/b.go"}, filterByGlob(files, "sub/*.go"))
	// Mid-pattern ** matches exactly one intervening component (stdlib limitation):
	// "sub/b.go" has none → no match; "sub/deep/d.go" has one → match.
	assert.Equal(t, []string{"sub/deep/d.go"}, filterByGlob(files, "sub/**/*.go"))
	assert.Nil(t, filterByGlob(files, "*.md"))
}

func TestGlobViaRgRespectsGitignore(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	root := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	require.NoError(t, cmd.Run())
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.go\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "kept.go"), []byte("package x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "ignored.go"), []byte("package x\n"), 0o644))

	// Call the rg path DIRECTLY so it is exercised even on a host where fd exists
	// (fd would otherwise shadow rg inside Execute).
	rels, err := NewGlobTool(root).globViaRg(context.Background(), "*.go", root)
	require.NoError(t, err)
	assert.Contains(t, rels, "kept.go")
	assert.NotContains(t, rels, "ignored.go")
}
