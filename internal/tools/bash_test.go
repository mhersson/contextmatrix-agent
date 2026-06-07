package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestBashToolKillsProcessGroupOnTimeout(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "marker")
	// A backgrounded grandchild touches marker after 2s. With a 1s timeout the
	// whole process group must be killed, so marker must NEVER appear.
	out, err := NewBashTool(root).Execute(context.Background(), map[string]any{
		"command":         "(sleep 2 && touch marker) & echo started",
		"timeout_seconds": 1.0,
	})
	require.NoError(t, err)
	assert.Contains(t, out, "started")
	assert.Contains(t, out, "timed out")

	time.Sleep(3 * time.Second) // outlast the grandchild's sleep
	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "grandchild survived the timeout; process group was not killed")
}
