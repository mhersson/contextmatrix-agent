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
	out, err := DefaultsYAML()
	require.NoError(t, err)

	text := string(out)

	for _, key := range []string{
		"contextmatrix_url:", "container_contextmatrix_url:", "api_key:", "mcp_api_key:",
		"port: 9092", "base_image:", "image_pull_policy: if-not-present", "secrets_dir:",
		"log_dir:", "default_model:", "review_attempts_cap:", "selector_tier_bars:",
	} {
		assert.Contains(t, text, key)
	}

	assert.Contains(t, text, "worker_extra_env: {}")
	assert.Contains(t, text, "image_list_filters: []")
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
