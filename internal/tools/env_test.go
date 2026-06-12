package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScrubbedEnvAllowlist(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/home/x")
	t.Setenv("OPENROUTER_API_KEY", "sk-secret")
	t.Setenv("CM_MCP_API_KEY", "mcp-secret")
	t.Setenv("CM_GIT_TOKEN", "ghs_secret")

	env := ScrubbedEnv(nil)
	assert.Contains(t, env, "PATH=/usr/bin")
	assert.Contains(t, env, "HOME=/home/x")

	for _, kv := range env {
		assert.NotContains(t, kv, "secret")
	}

	withExtra := ScrubbedEnv([]string{"GOCACHE=/tmp/gocache"})
	assert.Contains(t, withExtra, "GOCACHE=/tmp/gocache")
}

func TestBashToolDoesNotLeakEnv(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-leakcheck")

	tool := NewBashTool(t.TempDir())
	out, err := tool.Execute(context.Background(), map[string]any{"command": "env"})
	require.NoError(t, err)
	assert.NotContains(t, out, "sk-leakcheck")
	assert.Contains(t, out, "PATH=")
}
