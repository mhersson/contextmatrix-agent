package cli

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mhersson/contextmatrix-agent/internal/config"
)

func TestComposeMCPURL(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{"plain", "http://cm:8080", "http://cm:8080/mcp"},
		{"trailing slash trimmed", "http://cm:8080/", "http://cm:8080/mcp"},
		{"multiple trailing slashes trimmed", "http://cm:8080///", "http://cm:8080/mcp"},
		{"path base", "http://cm:8080/api", "http://cm:8080/api/mcp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, composeMCPURL(tt.base))
		})
	}
}

func TestExitStatus(t *testing.T) {
	tests := []struct {
		name        string
		code        int64
		wantStatus  string
		wantMessage string
	}{
		{"zero is completed", 0, "completed", ""},
		{"nonzero is failed", 1, "failed", "worker exited with code 1"},
		{"timeout sentinel is failed", -1, "failed", "worker exited with code -1"},
		{"high code is failed", 137, "failed", "worker exited with code 137"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, message := exitStatus(tt.code)
			assert.Equal(t, tt.wantStatus, status)
			assert.Equal(t, tt.wantMessage, message)
		})
	}
}

func TestLaunchEnvMCPURL(t *testing.T) {
	t.Run("container override wins for MCP base", func(t *testing.T) {
		cfg := &config.ServiceConfig{
			ContextMatrixURL:          "http://public:8080",
			ContainerContextMatrixURL: "http://internal:8080",
		}
		env := launchEnv(cfg, "/secrets/shared")
		assert.Equal(t, "http://internal:8080/mcp", env.MCPURL)
		assert.Equal(t, "/secrets/shared", env.SecretsHostDir)
	})

	t.Run("falls back to public URL when no container override", func(t *testing.T) {
		cfg := &config.ServiceConfig{ContextMatrixURL: "http://public:8080"}
		env := launchEnv(cfg, "/secrets/shared")
		assert.Equal(t, "http://public:8080/mcp", env.MCPURL)
	})

	t.Run("forwards worker knobs", func(t *testing.T) {
		cfg := &config.ServiceConfig{
			ContextMatrixURL:      "http://public:8080",
			MCPAPIKey:             "mcp-key",
			BaseImage:             "img@sha256:abc",
			ContainerMemoryBytes:  1234,
			ContainerPidsLimit:    99,
			BashTimeoutMaxSeconds: 700,
			ToolOutputMaxBytes:    40000,
			DefaultModel:          "deepseek/deepseek-v4",
		}
		env := launchEnv(cfg, "/secrets/shared")
		assert.Equal(t, "mcp-key", env.MCPAPIKey)
		assert.Equal(t, "img@sha256:abc", env.BaseImage)
		assert.Equal(t, int64(1234), env.MemoryBytes)
		assert.Equal(t, int64(99), env.PidsLimit)
		assert.Equal(t, 700, env.BashTimeoutMaxSeconds)
		assert.Equal(t, 40000, env.ToolOutputMaxBytes)
		assert.Equal(t, "deepseek/deepseek-v4", env.DefaultModel)
	})

	t.Run("forwards compaction settings", func(t *testing.T) {
		cfg := &config.ServiceConfig{
			ContextMatrixURL: "http://public:8080",
			Compaction: config.CompactionConfig{
				Enabled:         true,
				Threshold:       0.8,
				KeepRecentTurns: 4,
			},
		}
		env := launchEnv(cfg, "/secrets/shared")
		assert.True(t, env.CompactionEnabled)
		assert.InDelta(t, 0.8, env.CompactionThreshold, 1e-9)
		assert.Equal(t, 4, env.CompactionKeepRecentTurns)
	})
}

func TestFlattenEnv(t *testing.T) {
	t.Run("nil map yields nil", func(t *testing.T) {
		assert.Nil(t, flattenEnv(nil))
	})

	t.Run("renders KEY=VALUE pairs", func(t *testing.T) {
		got := flattenEnv(map[string]string{"FOO": "bar", "BAZ": "qux"})
		sort.Strings(got)
		assert.Equal(t, []string{"BAZ=qux", "FOO=bar"}, got)
	})
}

func TestServeCommandRegistered(t *testing.T) {
	root := NewRootCmd()

	var found bool

	for _, c := range root.Commands() {
		if c.Name() == "serve" {
			found = true

			break
		}
	}

	assert.True(t, found, "serve command should be registered on root")
}
