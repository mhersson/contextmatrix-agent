package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubTool struct{ name string }

func (s stubTool) Name() string { return s.name }
func (s stubTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: s.name}}
}
func (s stubTool) Execute(ctx context.Context, args map[string]any) (string, error) { return "ok", nil }

func TestRegistryGetAndOrderedSchemas(t *testing.T) {
	r := NewRegistry(stubTool{"read"}, stubTool{"edit"})
	_, ok := r.Get("read")
	assert.True(t, ok)
	_, ok = r.Get("nope")
	assert.False(t, ok)

	schemas := r.Schemas()
	require.Len(t, schemas, 2)
	assert.Equal(t, "read", schemas[0].Function.Name) // insertion order, deterministic
	assert.Equal(t, "edit", schemas[1].Function.Name)
}

func TestResolveInRootRejectsEscape(t *testing.T) {
	root := t.TempDir()
	in, err := resolveInRoot(root, "sub/file.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "sub/file.txt"), in)

	_, err = resolveInRoot(root, "../escape.txt")
	require.Error(t, err)
}

func TestResolveInRootRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))

	// A path that traverses a symlink pointing outside the root must be rejected.
	_, err := resolveInRoot(root, "link/secret.txt")
	require.Error(t, err)

	// A genuinely in-root path through a real subdir still resolves.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "real"), 0o755))
	got, err := resolveInRoot(root, "real/ok.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "real/ok.txt"), got)
}

func TestRequireString(t *testing.T) {
	v, err := requireString(map[string]any{"path": "x"}, "path")
	require.NoError(t, err)
	assert.Equal(t, "x", v)

	_, err = requireString(map[string]any{}, "path")
	require.Error(t, err)
}

// Exercises the optional accessors so they are used at this task's lint gate
// (they are first called by tools in later tasks).
func TestOptionalArgAccessors(t *testing.T) {
	args := map[string]any{"s": "v", "b": true, "f": float64(5), "i": 3}
	assert.Equal(t, "v", optString(args, "s", "def"))
	assert.Equal(t, "def", optString(args, "missing", "def"))
	assert.True(t, optBool(args, "b", false))
	assert.False(t, optBool(args, "missing", false))
	assert.Equal(t, 5, optInt(args, "f", 0)) // JSON number → float64 path
	assert.Equal(t, 3, optInt(args, "i", 0)) // int path
	assert.Equal(t, 7, optInt(args, "missing", 7))
}
