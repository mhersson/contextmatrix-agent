package cli

import (
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requiredEnvVars is the full set of required CM_* vars for specFromEnv.
var requiredEnvVars = map[string]string{
	"CM_CARD_ID":     "CM-001",
	"CM_PROJECT":     "alpha",
	"CM_REPO_URL":    "https://github.com/org/repo",
	"CM_MCP_URL":     "http://localhost:8080/mcp",
	"CM_MCP_API_KEY": "test-key",
}

// setRequired calls t.Setenv for all required vars.
func setRequired(t *testing.T) {
	t.Helper()

	for k, v := range requiredEnvVars {
		t.Setenv(k, v)
	}
}

func TestSpecFromEnv_RequiredVars(t *testing.T) {
	required := []string{
		"CM_CARD_ID",
		"CM_PROJECT",
		"CM_REPO_URL",
		"CM_MCP_URL",
		"CM_MCP_API_KEY",
	}

	for _, missing := range required {
		missing := missing

		t.Run("missing_"+missing, func(t *testing.T) {
			setRequired(t)
			t.Setenv(missing, "") // blank the specific required var

			_, err := specFromEnv()
			require.Error(t, err)
			assert.Contains(t, err.Error(), missing)
		})
	}
}

func TestSpecFromEnv_HappyPath(t *testing.T) {
	setRequired(t)

	spec, err := specFromEnv()
	require.NoError(t, err)

	assert.Equal(t, "CM-001", spec.CardID)
	assert.Equal(t, "alpha", spec.Project)
	assert.Equal(t, "https://github.com/org/repo", spec.RepoURL)
	assert.Equal(t, "http://localhost:8080/mcp", spec.MCPURL)
	assert.Equal(t, "test-key", spec.MCPAPIKey)
}

func TestSpecFromEnv_BoolParsing(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected bool
	}{
		{"true", "true", true},
		{"false", "false", false},
		{"absent", "", false},
		{"non-true", "yes", false},
		{"TRUE-uppercase", "TRUE", false}, // only exact "true" is interactive
	}

	for _, tc := range tests {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			setRequired(t)
			t.Setenv("CM_INTERACTIVE", tc.value)

			spec, err := specFromEnv()
			require.NoError(t, err)
			assert.Equal(t, tc.expected, spec.Interactive)
		})
	}
}

func TestSpecFromEnv_IntParsing(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		setRequired(t)

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Equal(t, 600, spec.BashTimeoutMax)
		assert.Equal(t, 30000, spec.ToolOutputMax)
		assert.Equal(t, derefInt(config.Defaults().MaxTurns), spec.MaxTurns)
	})

	t.Run("valid_override", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_BASH_TIMEOUT_MAX_SECONDS", "120")
		t.Setenv("CMX_TOOL_OUTPUT_MAX_BYTES", "50000")
		t.Setenv("CMX_MAX_TURNS", "50")

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Equal(t, 120, spec.BashTimeoutMax)
		assert.Equal(t, 50000, spec.ToolOutputMax)
		assert.Equal(t, 50, spec.MaxTurns)
	})

	t.Run("garbage_bash_timeout", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_BASH_TIMEOUT_MAX_SECONDS", "not-a-number")

		_, err := specFromEnv()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CMX_BASH_TIMEOUT_MAX_SECONDS")
	})

	t.Run("garbage_tool_output", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_TOOL_OUTPUT_MAX_BYTES", "??")

		_, err := specFromEnv()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CMX_TOOL_OUTPUT_MAX_BYTES")
	})

	t.Run("garbage_max_turns", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_MAX_TURNS", "abc")

		_, err := specFromEnv()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CMX_MAX_TURNS")
	})
}

func TestSpecFromEnv_FloatParsing(t *testing.T) {
	t.Run("default_max_cost", func(t *testing.T) {
		setRequired(t)

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.InDelta(t, derefFloat(config.Defaults().MaxCostUSD), spec.MaxCostUSD, 0.001)
	})

	t.Run("valid_override", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_MAX_COST_USD", "1.25")

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.InDelta(t, 1.25, spec.MaxCostUSD, 0.001)
	})

	t.Run("garbage_max_cost", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_MAX_COST_USD", "cheap")

		_, err := specFromEnv()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CMX_MAX_COST_USD")
	})
}

func TestSpecFromEnv_DefaultModelFallback(t *testing.T) {
	t.Run("uses_capable_default_when_unset", func(t *testing.T) {
		setRequired(t)

		spec, err := specFromEnv()
		require.NoError(t, err)
		// Mirrors how specFromEnv derives the fallback: config's capable default.
		assert.Equal(t, derefStr(config.Defaults().CapableModel), spec.DefaultModel)
	})

	t.Run("uses_env_override", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_DEFAULT_MODEL", "openai/gpt-4o")

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Equal(t, "openai/gpt-4o", spec.DefaultModel)
	})
}

func TestSpecFromEnv_WorkspaceDefault(t *testing.T) {
	t.Run("default_workspace", func(t *testing.T) {
		setRequired(t)

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Equal(t, "/home/user/workspace", spec.Workspace)
	})

	t.Run("env_override", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_WORKSPACE", "/tmp/myworkspace")

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Equal(t, "/tmp/myworkspace", spec.Workspace)
	})
}

func TestSpecFromEnv_OptionalVars(t *testing.T) {
	setRequired(t)
	t.Setenv("CM_BASE_BRANCH", "main")
	t.Setenv("CM_MODEL", "anthropic/claude-3-5-sonnet")
	t.Setenv("CM_CORRELATION_ID", "corr-abc-123")

	spec, err := specFromEnv()
	require.NoError(t, err)

	assert.Equal(t, "main", spec.BaseBranch)
	assert.Equal(t, "anthropic/claude-3-5-sonnet", spec.Model)
	assert.Equal(t, "corr-abc-123", spec.CorrelationID)
}
