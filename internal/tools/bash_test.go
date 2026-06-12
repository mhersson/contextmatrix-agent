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

func TestBashTimeoutClamp(t *testing.T) {
	tool := NewBashTool(t.TempDir()).WithMaxTimeout(1)

	start := time.Now()
	out, err := tool.Execute(context.Background(), map[string]any{
		"command": "sleep 30", "timeout_seconds": 9999,
	})
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Contains(t, out, "timed out after 1s")
}

func TestBashWithMaxTimeout(t *testing.T) {
	root := t.TempDir()

	tool := NewBashTool(root)
	assert.Equal(t, defaultBashMaxTimeout, tool.maxTimeout)

	// Non-positive values are no-ops: the current ceiling is kept.
	assert.Equal(t, defaultBashMaxTimeout, tool.WithMaxTimeout(0).maxTimeout)
	assert.Equal(t, defaultBashMaxTimeout, tool.WithMaxTimeout(-7).maxTimeout)

	// Positive values take effect, without mutating the receiver.
	assert.Equal(t, 5, tool.WithMaxTimeout(5).maxTimeout)
	assert.Equal(t, defaultBashMaxTimeout, tool.maxTimeout)
}

func TestBashSchemaReflectsMaxTimeout(t *testing.T) {
	root := t.TempDir()

	assert.Contains(t, string(NewBashTool(root).Schema().Function.Parameters), "max 600")
	assert.Contains(t, string(NewBashTool(root).WithMaxTimeout(42).Schema().Function.Parameters), "max 42")
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
