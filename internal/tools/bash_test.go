package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBashToolRunsInRoot(t *testing.T) {
	root := t.TempDir()
	out, err := NewBashTool(root).Execute(context.Background(), map[string]any{"command": "pwd"})
	require.NoError(t, err)
	assert.Contains(t, out, root)
}

func TestBashToolReturnsFailureAsOutputNotError(t *testing.T) {
	root := t.TempDir()
	// A failing command must NOT return a Go error — the model needs to see it.
	out, err := NewBashTool(root).Execute(context.Background(), map[string]any{"command": "exit 3"})
	require.NoError(t, err)
	assert.Contains(t, out, "exit")
}

func TestBashToolTimeout(t *testing.T) {
	root := t.TempDir()
	out, err := NewBashTool(root).Execute(context.Background(), map[string]any{"command": "sleep 5", "timeout_seconds": 1.0})
	require.NoError(t, err)
	assert.Contains(t, out, "timed out")
}
