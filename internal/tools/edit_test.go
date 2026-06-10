package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEditToolReplace(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "f.txt")
	require.NoError(t, os.WriteFile(p, []byte("hello world"), 0o644))

	et := NewEditTool(root)

	_, err := et.Execute(context.Background(), map[string]any{"path": "f.txt", "old_string": "world", "new_string": "gophers"})
	require.NoError(t, err)

	b, _ := os.ReadFile(p)
	assert.Equal(t, "hello gophers", string(b))
}

func TestEditToolAmbiguousWithoutReplaceAll(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x x x"), 0o644))
	et := NewEditTool(root)

	_, err := et.Execute(context.Background(), map[string]any{"path": "f.txt", "old_string": "x", "new_string": "y"})
	require.Error(t, err) // appears 3 times, replace_all not set

	_, err = et.Execute(context.Background(), map[string]any{"path": "f.txt", "old_string": "x", "new_string": "y", "replace_all": true})
	require.NoError(t, err)
}

func TestEditToolNotFound(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("abc"), 0o644))
	_, err := NewEditTool(root).Execute(context.Background(), map[string]any{"path": "f.txt", "old_string": "zzz", "new_string": "y"})
	require.Error(t, err)
}
