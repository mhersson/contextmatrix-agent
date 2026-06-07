package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadToolWholeFileAndSlice(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("a\nb\nc\nd\n"), 0o644))
	rt := NewReadTool(root)

	out, err := rt.Execute(context.Background(), map[string]any{"path": "f.txt"})
	require.NoError(t, err)
	assert.Equal(t, "a\nb\nc\nd\n", out)

	out, err = rt.Execute(context.Background(), map[string]any{"path": "f.txt", "offset": 2.0, "limit": 2.0})
	require.NoError(t, err)
	assert.Equal(t, "b\nc\n", out)

	_, err = rt.Execute(context.Background(), map[string]any{})
	require.Error(t, err) // missing required path
}
