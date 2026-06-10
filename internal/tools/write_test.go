package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteToolCreatesAndOverwrites(t *testing.T) {
	root := t.TempDir()
	wt := NewWriteTool(root)

	out, err := wt.Execute(context.Background(), map[string]any{"path": "a.txt", "content": "one\ntwo\n"})
	require.NoError(t, err)
	assert.Contains(t, out, "created a.txt")

	b, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	assert.Equal(t, "one\ntwo\n", string(b))

	out, err = wt.Execute(context.Background(), map[string]any{"path": "a.txt", "content": "one\nTWO\n"})
	require.NoError(t, err)
	assert.Contains(t, out, "-two")
	assert.Contains(t, out, "+TWO")
}

func TestWriteToolCreateDirs(t *testing.T) {
	root := t.TempDir()
	_, err := NewWriteTool(root).Execute(context.Background(), map[string]any{
		"path": "sub/dir/f.txt", "content": "x", "create_dirs": true,
	})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(root, "sub/dir/f.txt"))
}

func TestWriteToolMissingDirWithoutCreateDirs(t *testing.T) {
	root := t.TempDir()
	_, err := NewWriteTool(root).Execute(context.Background(), map[string]any{"path": "no/such/f.txt", "content": "x"})
	require.Error(t, err)
}

func TestWriteToolRequiresPathAndContent(t *testing.T) {
	root := t.TempDir()
	_, err := NewWriteTool(root).Execute(context.Background(), map[string]any{"content": "x"})
	require.Error(t, err)
	_, err = NewWriteTool(root).Execute(context.Background(), map[string]any{"path": "a.txt"})
	require.Error(t, err)
}
