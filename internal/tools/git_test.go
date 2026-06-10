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

func TestGitToolReadOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	root := t.TempDir()

	for _, c := range [][]string{{"init"}, {"config", "user.email", "a@b.c"}, {"config", "user.name", "x"}} {
		cmd := exec.Command("git", c...)
		cmd.Dir = root
		require.NoError(t, cmd.Run())
	}

	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("hi\n"), 0o644))

	gt := NewGitTool(root)
	out, err := gt.Execute(context.Background(), map[string]any{"subcommand": "status"})
	require.NoError(t, err)
	assert.Contains(t, out, "f.txt")

	_, err = gt.Execute(context.Background(), map[string]any{"subcommand": "commit", "args": []any{"-m", "x"}})
	require.Error(t, err)
	_, err = gt.Execute(context.Background(), map[string]any{"subcommand": "push"})
	require.Error(t, err)
}
