package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultsYAML guards the contextmatrix-setup contract: every key the
// loader accepts is printed, empty maps and lists print as {} and [], and
// the output loads back through LoadService unchanged.
func TestDefaultsYAML(t *testing.T) {
	clearServiceEnv(t)

	out, err := DefaultsYAML()
	require.NoError(t, err)

	text := string(out)

	for _, key := range []string{
		"contextmatrix_url:", "container_contextmatrix_url:", "container_serve_url:", "api_key:", "mcp_api_key:",
		"port: 9092", "base_image:", "image_pull_policy: if-not-present", "secrets_dir:",
		"log_dir:", "default_model:", "review_attempts_cap:", "replay_reasoning: true",
	} {
		assert.Contains(t, text, key)
	}

	assert.Contains(t, text, "log_dir: ~/.contextmatrix/agent/logs")
	assert.Contains(t, text, "log_retention_days: 30")
	assert.Contains(t, text, "worker_extra_env: {}")
	assert.Contains(t, text, "image_list_filters:\n    - contextmatrix-agent")
	assert.NotContains(t, text, "null")

	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, out, 0o600))

	cfg, err := LoadService(path)
	require.NoError(t, err)
	assert.Equal(t, 9092, cfg.Port)
	assert.Equal(t, defaultSecretsDir, cfg.SecretsDir)

	// Required keys are present but empty, so Validate rejects the raw defaults.
	err = cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contextmatrix_url")
}

func TestLogDirDefaultsUnderHome(t *testing.T) {
	home := clearServiceEnv(t)

	cfg, err := LoadService("")
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(home, ".contextmatrix", "agent", "logs"), cfg.LogDir)
	assert.Equal(t, 30, cfg.LogRetentionDays)
}

func TestLogDirOverrides(t *testing.T) {
	home := clearServiceEnv(t)

	for name, tc := range map[string]struct{ yaml, want string }{
		"tilde slash": {"log_dir: ~/elsewhere\n", filepath.Join(home, "elsewhere")},
		"bare tilde":  {"log_dir: \"~\"\n", home},
		"absolute":    {"log_dir: /abs/logs\n", "/abs/logs"},
		"empty":       {"log_dir: \"\"\n", filepath.Join(home, ".contextmatrix", "agent", "logs")},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "serve.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tc.yaml), 0o600))

			cfg, err := LoadService(path)
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.LogDir)
		})
	}
}

func TestLogRetentionDaysValidated(t *testing.T) {
	for days, wantErr := range map[int]bool{-1: true, 0: true, 1: false, 36500: false, 36501: true} {
		cfg := validServiceConfig()
		cfg.LogRetentionDays = days

		err := cfg.Validate()
		if wantErr {
			assert.ErrorContains(t, err, "log_retention_days", "days=%d", days)
		} else {
			assert.NoError(t, err, "days=%d", days)
		}
	}
}
