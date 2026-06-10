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

func TestGrepTool(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep (rg) not installed")
	}

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package x\nfunc Encode() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.txt"), []byte("nothing here\n"), 0o644))

	out, err := NewGrepTool(root).Execute(context.Background(), map[string]any{"pattern": "Encode"})
	require.NoError(t, err)
	assert.Contains(t, out, "a.go")
	assert.Contains(t, out, "Encode")

	out, err = NewGrepTool(root).Execute(context.Background(), map[string]any{"pattern": "no-such-token-xyz"})
	require.NoError(t, err)
	assert.Contains(t, out, "no matches")
}

func TestGrepToolDashPattern(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep (rg) not installed")
	}

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "c.txt"), []byte("a-b-c\n"), 0o644))

	out, err := NewGrepTool(root).Execute(context.Background(), map[string]any{"pattern": "-b"})
	require.NoError(t, err)
	assert.Contains(t, out, "a-b-c")
}
