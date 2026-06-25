package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validServiceConfig returns a ServiceConfig with every required field set so
// individual tests can null out one field and assert the resulting error.
func validServiceConfig() ServiceConfig {
	return ServiceConfig{
		ContextMatrixURL: "http://contextmatrix:8080",
		APIKey:           "0123456789abcdef0123456789abcdef", // 32 chars
		BaseImage:        "ghcr.io/example/agent@sha256:" + repeatHex(64),
		OpenRouterAPIKey: "sk-or-test",
		ImagePullPolicy:  "if-not-present",
		MaxConcurrent:    5,
		Port:             9092,
		ContainerTimeout: 150 * time.Minute,
		GitHub: GitHubConfig{
			AuthMode: "pat",
			PAT:      GitHubPATConfig{Token: "ghp_test"},
		},
	}
}

func repeatHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}

	return string(b)
}

func TestServiceDefaults(t *testing.T) {
	// Loading from a nonexistent path yields defaults+env only (matching
	// config.go's behavior). With no CMX_* env set, defaults must all land.
	clearServiceEnv(t)

	cfg, err := LoadService(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.NoError(t, err)

	assert.Equal(t, 9092, cfg.Port)
	assert.Equal(t, "if-not-present", cfg.ImagePullPolicy)
	assert.Equal(t, 5, cfg.MaxConcurrent)
	assert.Equal(t, 2*time.Hour+30*time.Minute, cfg.ContainerTimeout)
	assert.Equal(t, int64(8*1024*1024*1024), cfg.ContainerMemoryBytes)
	assert.Equal(t, int64(512), cfg.ContainerPidsLimit)
	assert.Equal(t, 30*time.Minute, cfg.IdleOutputTimeout)
	assert.Equal(t, 30*time.Second, cfg.IdleWatchdogInterval)
	assert.Equal(t, "/var/run/cm-agent/secrets", cfg.SecretsDir)
	assert.Equal(t, 330*time.Second, cfg.ReplaySkew)
	assert.Equal(t, 10000, cfg.ReplayCacheSize)
	assert.Equal(t, 10*time.Minute, cfg.MessageDedupTTL)
	assert.Equal(t, 1000, cfg.MessageDedupCacheSize)
	assert.Equal(t, 600, cfg.BashTimeoutMaxSeconds)
	assert.Equal(t, 131072, cfg.ToolOutputMaxBytes)
}

func TestServiceLoadFromFile(t *testing.T) {
	clearServiceEnv(t)

	yaml := `
contextmatrix_url: http://cm.example:8080
container_contextmatrix_url: http://cm-internal:8080
api_key: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
mcp_api_key: mcp-secret-key
port: 9999
base_image: ghcr.io/example/worker:v1
image_pull_policy: always
max_concurrent: 12
container_timeout: 2h
container_memory_limit: 4294967296
container_pids_limit: 256
idle_output_timeout: 15m
idle_watchdog_interval: 10s
secrets_dir: /opt/secrets
openrouter_api_key: sk-or-fromfile
webhook_replay_skew_seconds: 120
webhook_replay_cache_size: 4096
message_dedup_ttl_seconds: 300
message_dedup_cache_size: 512
bash_timeout_max_seconds: 900
tool_output_max_bytes: 50000
default_model: deepseek/deepseek-v4
log_level: debug
github:
  auth_mode: app
  app:
    app_id: 12345
    installation_id: 67890
    private_key_path: /etc/key.pem
worker_extra_env:
  FOO: bar
  BAZ: qux
`
	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	cfg, err := LoadService(path)
	require.NoError(t, err)

	assert.Equal(t, "http://cm.example:8080", cfg.ContextMatrixURL)
	assert.Equal(t, "http://cm-internal:8080", cfg.ContainerContextMatrixURL)
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", cfg.APIKey)
	assert.Equal(t, "mcp-secret-key", cfg.MCPAPIKey)
	assert.Equal(t, 9999, cfg.Port)
	assert.Equal(t, "ghcr.io/example/worker:v1", cfg.BaseImage)
	assert.Equal(t, "always", cfg.ImagePullPolicy)
	assert.Equal(t, 12, cfg.MaxConcurrent)
	assert.Equal(t, 2*time.Hour, cfg.ContainerTimeout)
	assert.Equal(t, int64(4294967296), cfg.ContainerMemoryBytes)
	assert.Equal(t, int64(256), cfg.ContainerPidsLimit)
	assert.Equal(t, 15*time.Minute, cfg.IdleOutputTimeout)
	assert.Equal(t, 10*time.Second, cfg.IdleWatchdogInterval)
	assert.Equal(t, "/opt/secrets", cfg.SecretsDir)
	assert.Equal(t, "sk-or-fromfile", cfg.OpenRouterAPIKey)
	assert.Equal(t, 120*time.Second, cfg.ReplaySkew)
	assert.Equal(t, 4096, cfg.ReplayCacheSize)
	assert.Equal(t, 300*time.Second, cfg.MessageDedupTTL)
	assert.Equal(t, 512, cfg.MessageDedupCacheSize)
	assert.Equal(t, 900, cfg.BashTimeoutMaxSeconds)
	assert.Equal(t, 50000, cfg.ToolOutputMaxBytes)
	assert.Equal(t, "deepseek/deepseek-v4", cfg.DefaultModel)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, "app", cfg.GitHub.AuthMode)
	assert.Equal(t, int64(12345), cfg.GitHub.App.AppID)
	assert.Equal(t, int64(67890), cfg.GitHub.App.InstallationID)
	assert.Equal(t, "/etc/key.pem", cfg.GitHub.App.PrivateKeyPath)
	assert.Equal(t, map[string]string{"FOO": "bar", "BAZ": "qux"}, cfg.WorkerExtraEnv)
}

func TestServiceEnvOverridesFile(t *testing.T) {
	clearServiceEnv(t)

	yaml := `
contextmatrix_url: http://from-file:8080
api_key: filekeyfilekeyfilekeyfilekeyfile
base_image: ghcr.io/example/worker:v1
openrouter_api_key: sk-or-file
github:
  auth_mode: pat
  pat:
    token: ghp_file
`
	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	t.Setenv("CMX_CONTEXTMATRIX_URL", "http://from-env:8080")
	t.Setenv("CMX_PORT", "7777")
	t.Setenv("CMX_OPENROUTER_API_KEY", "sk-or-env")
	t.Setenv("CMX_GITHUB__AUTH_MODE", "pat")
	t.Setenv("CMX_GITHUB__PAT__TOKEN", "ghp_env")

	cfg, err := LoadService(path)
	require.NoError(t, err)

	assert.Equal(t, "http://from-env:8080", cfg.ContextMatrixURL)
	assert.Equal(t, 7777, cfg.Port)
	assert.Equal(t, "sk-or-env", cfg.OpenRouterAPIKey)
	assert.Equal(t, "ghp_env", cfg.GitHub.PAT.Token)
	// Untouched file value survives.
	assert.Equal(t, "ghcr.io/example/worker:v1", cfg.BaseImage)
}

func TestServiceValidate(t *testing.T) {
	t.Run("valid passes", func(t *testing.T) {
		cfg := validServiceConfig()
		require.NoError(t, cfg.Validate())
	})

	t.Run("missing contextmatrix_url errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.ContextMatrixURL = ""
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "contextmatrix_url")
	})

	t.Run("missing api_key errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.APIKey = ""
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "api_key")
	})

	t.Run("short api_key errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.APIKey = "tooshort"
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "32")
	})

	t.Run("missing base_image errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.BaseImage = ""
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "base_image")
	})

	t.Run("missing openrouter_api_key errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.OpenRouterAPIKey = ""
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "openrouter_api_key")
	})

	t.Run("bad image_pull_policy errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.ImagePullPolicy = "sometimes"
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "image_pull_policy")
	})

	t.Run("zero max_concurrent errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.MaxConcurrent = 0
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_concurrent")
		// Message must explain that 0 would refuse every launch.
		assert.Contains(t, err.Error(), "refuses every launch")
	})

	t.Run("negative max_concurrent errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.MaxConcurrent = -3
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_concurrent")
	})

	t.Run("port zero errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Port = 0
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "port")
	})

	t.Run("port above 65535 errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Port = 70000
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "port")
	})

	t.Run("container_timeout over 150m errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.ContainerTimeout = 151 * time.Minute
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "container_timeout")
		// Message must explain the reconcile-cap reason.
		assert.Contains(t, err.Error(), "150m")
	})

	t.Run("container_timeout exactly 150m passes", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.ContainerTimeout = 150 * time.Minute
		require.NoError(t, cfg.Validate())
	})

	t.Run("non-digest base_image passes (warns only)", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.BaseImage = "ghcr.io/example/worker:v1"
		require.NoError(t, cfg.Validate())
	})

	t.Run("app auth_mode requires app fields", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.GitHub = GitHubConfig{AuthMode: "app"}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "app")
	})

	t.Run("pat auth_mode requires token", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.GitHub = GitHubConfig{AuthMode: "pat"}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "token")
	})

	t.Run("unknown auth_mode errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.GitHub = GitHubConfig{AuthMode: "oauth"}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "auth_mode")
	})

	t.Run("negative max_card_cost errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.MaxCardCost = -1.0
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_card_cost")
	})

	t.Run("zero max_card_cost passes (disables ceiling)", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.MaxCardCost = 0
		require.NoError(t, cfg.Validate())
	})

	t.Run("negative selector_price_headroom errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.SelectorPriceHeadroom = -0.5
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "selector_price_headroom")
	})

	t.Run("zero selector_price_headroom passes (worker default)", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.SelectorPriceHeadroom = 0
		require.NoError(t, cfg.Validate())
	})
}

func TestServiceBudgetDefaults(t *testing.T) {
	// max_card_cost and selector_price_headroom must default to 5.0 and 1.5
	// when the keys are absent from config and env.
	clearServiceEnv(t)

	cfg, err := LoadService(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.NoError(t, err)

	assert.InDelta(t, 5.0, cfg.MaxCardCost, 1e-9, "max_card_cost default must be 5.0")
	assert.InDelta(t, 1.5, cfg.SelectorPriceHeadroom, 1e-9, "selector_price_headroom default must be 1.5")
}

func TestServiceBudgetFromFile(t *testing.T) {
	// max_card_cost: 8.0, selector_price_headroom: 2.0 loaded from file.
	clearServiceEnv(t)

	content := `
max_card_cost: 8.0
selector_price_headroom: 2.0
`
	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := LoadService(path)
	require.NoError(t, err)

	assert.InDelta(t, 8.0, cfg.MaxCardCost, 1e-9)
	assert.InDelta(t, 2.0, cfg.SelectorPriceHeadroom, 1e-9)
}

func TestServiceBudgetFromEnv(t *testing.T) {
	// CMX_MAX_CARD_COST and CMX_SELECTOR_PRICE_HEADROOM override file values.
	clearServiceEnv(t)

	t.Setenv("CMX_MAX_CARD_COST", "3.5")
	t.Setenv("CMX_SELECTOR_PRICE_HEADROOM", "1.2")

	cfg, err := LoadService(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.NoError(t, err)

	assert.InDelta(t, 3.5, cfg.MaxCardCost, 1e-9)
	assert.InDelta(t, 1.2, cfg.SelectorPriceHeadroom, 1e-9)
}

func TestServiceBudgetZeroIsLegal(t *testing.T) {
	// max_card_cost: 0 is a legal explicit value (disables the per-card ceiling).
	// selector_price_headroom: 0 is also legal (0 = omit when passed to workers,
	// worker applies its own default).
	clearServiceEnv(t)

	content := `
max_card_cost: 0
selector_price_headroom: 0
`
	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := LoadService(path)
	require.NoError(t, err)

	assert.InDelta(t, 0.0, cfg.MaxCardCost, 1e-9)
	assert.InDelta(t, 0.0, cfg.SelectorPriceHeadroom, 1e-9)
}

// clearServiceEnv unsets any CMX_* vars that could leak into a default/file
// test from the developer's shell. t.Setenv restores them after the test.
func clearServiceEnv(t *testing.T) {
	t.Helper()

	for _, e := range []string{
		"CMX_CONTEXTMATRIX_URL", "CMX_PORT", "CMX_OPENROUTER_API_KEY",
		"CMX_API_KEY", "CMX_BASE_IMAGE", "CMX_MAX_CONCURRENT",
		"CMX_GITHUB__AUTH_MODE", "CMX_GITHUB__PAT__TOKEN",
		"CMX_MAX_CARD_COST", "CMX_SELECTOR_PRICE_HEADROOM",
	} {
		if _, ok := os.LookupEnv(e); ok {
			t.Setenv(e, "")
			require.NoError(t, os.Unsetenv(e))
		}
	}
}
