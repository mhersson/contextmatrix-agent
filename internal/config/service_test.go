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
		ImagePullPolicy:  "if-not-present",
		MaxConcurrent:    5,
		Port:             9092,
		ContainerTimeout: 150 * time.Minute,
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
	assert.Empty(t, cfg.LogDir, "log_dir is opt-in; empty by default")
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
webhook_replay_skew_seconds: 120
webhook_replay_cache_size: 4096
message_dedup_ttl_seconds: 300
message_dedup_cache_size: 512
bash_timeout_max_seconds: 900
tool_output_max_bytes: 50000
default_model: deepseek/deepseek-v4
log_level: debug
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
	assert.Equal(t, 120*time.Second, cfg.ReplaySkew)
	assert.Equal(t, 4096, cfg.ReplayCacheSize)
	assert.Equal(t, 300*time.Second, cfg.MessageDedupTTL)
	assert.Equal(t, 512, cfg.MessageDedupCacheSize)
	assert.Equal(t, 900, cfg.BashTimeoutMaxSeconds)
	assert.Equal(t, 50000, cfg.ToolOutputMaxBytes)
	assert.Equal(t, "deepseek/deepseek-v4", cfg.DefaultModel)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, map[string]string{"FOO": "bar", "BAZ": "qux"}, cfg.WorkerExtraEnv)
}

func TestServiceEnvOverridesFile(t *testing.T) {
	clearServiceEnv(t)

	yaml := `
contextmatrix_url: http://from-file:8080
api_key: filekeyfilekeyfilekeyfilekeyfile
base_image: ghcr.io/example/worker:v1
`
	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	t.Setenv("CMX_CONTEXTMATRIX_URL", "http://from-env:8080")
	t.Setenv("CMX_PORT", "7777")

	cfg, err := LoadService(path)
	require.NoError(t, err)

	assert.Equal(t, "http://from-env:8080", cfg.ContextMatrixURL)
	assert.Equal(t, 7777, cfg.Port)
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

	t.Run("sub-unit selector_price_headroom errors (0 < h < 1 is meaningless)", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.SelectorPriceHeadroom = 0.5
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "selector_price_headroom")
	})

	t.Run("exactly 1.0 selector_price_headroom passes (no band)", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.SelectorPriceHeadroom = 1.0
		require.NoError(t, cfg.Validate())
	})

	t.Run("enabled compaction with valid values passes", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Compaction = CompactionConfig{Enabled: true, Threshold: 0.85, KeepRecentTurns: 6}
		require.NoError(t, cfg.Validate())
	})

	t.Run("enabled compaction with out-of-range threshold errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Compaction = CompactionConfig{Enabled: true, Threshold: 1.5, KeepRecentTurns: 6}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "compaction_threshold")
	})

	t.Run("enabled compaction with non-positive keep_recent_turns errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Compaction = CompactionConfig{Enabled: true, Threshold: 0.85, KeepRecentTurns: 0}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "compaction_keep_recent_turns")
	})

	t.Run("disabled compaction skips validation", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Compaction = CompactionConfig{Enabled: false, Threshold: 9, KeepRecentTurns: -1}
		require.NoError(t, cfg.Validate())
	})
}

func TestServiceValidate_ReasoningEffort(t *testing.T) {
	t.Parallel()

	ok := validServiceConfig()
	ok.ReasoningEffort = "high"
	require.NoError(t, ok.Validate())

	// Non-canonical values are forwarded to the provider as-is (serve.yaml.example
	// documents provider-specific tiers like "xhigh"), so Validate must not reject
	// them - it only logs a startup warning (verified by code inspection; slog.Warn
	// here mirrors the existing base_image-not-pinned warning, which is likewise
	// untested via log capture in this file).
	for _, v := range []string{"extreme", "xhigh"} {
		nonCanonical := validServiceConfig()
		nonCanonical.ReasoningEffort = v
		assert.NoError(t, nonCanonical.Validate(), "reasoning_effort %q should be allowed", v)
	}
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

func TestServiceAdminPort_DefaultZero(t *testing.T) {
	clearServiceEnv(t)

	cfg, err := LoadService(filepath.Join(t.TempDir(), "nope.yaml"))
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.AdminPort, "admin_port defaults to 0 (disabled)")
}

func TestServiceAdminPort_FromEnv(t *testing.T) {
	clearServiceEnv(t)
	t.Setenv("CMX_ADMIN_PORT", "9093")

	cfg, err := LoadService(filepath.Join(t.TempDir(), "nope.yaml"))
	require.NoError(t, err)
	assert.Equal(t, 9093, cfg.AdminPort)
}

func TestServiceAdminPort_Validate(t *testing.T) {
	t.Run("disabled is valid", func(t *testing.T) {
		c := validServiceConfig()
		c.AdminPort = 0
		require.NoError(t, c.Validate())
	})

	t.Run("distinct port is valid", func(t *testing.T) {
		c := validServiceConfig()
		c.Port = 9092
		c.AdminPort = 9093
		require.NoError(t, c.Validate())
	})

	t.Run("out of range is rejected", func(t *testing.T) {
		c := validServiceConfig()
		c.AdminPort = 70000
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "admin_port")
	})

	t.Run("collision with port is rejected", func(t *testing.T) {
		c := validServiceConfig()
		c.Port = 9092
		c.AdminPort = 9092
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "admin_port")
	})
}

func TestServiceCompactionDefaults(t *testing.T) {
	// Compaction is OFF by default (behavior-neutral): the agent keeps the hard
	// context_limit stop. Threshold defaults to 0.85, keep-recent to 6.
	clearServiceEnv(t)

	cfg, err := LoadService(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.NoError(t, err)

	assert.False(t, cfg.Compaction.Enabled, "compaction must default to disabled")
	assert.InDelta(t, 0.85, cfg.Compaction.Threshold, 1e-9)
	assert.Equal(t, 6, cfg.Compaction.KeepRecentTurns)
}

func TestServiceCompactionFromEnv(t *testing.T) {
	// CMX_COMPACTION_* overrides land on the typed Compaction config.
	clearServiceEnv(t)

	t.Setenv("CMX_COMPACTION_ENABLED", "true")
	t.Setenv("CMX_COMPACTION_THRESHOLD", "0.8")
	t.Setenv("CMX_COMPACTION_KEEP_RECENT_TURNS", "4")

	cfg, err := LoadService(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.NoError(t, err)

	assert.True(t, cfg.Compaction.Enabled)
	assert.InDelta(t, 0.8, cfg.Compaction.Threshold, 1e-9)
	assert.Equal(t, 4, cfg.Compaction.KeepRecentTurns)
}

func TestServiceCompactionFromFile(t *testing.T) {
	clearServiceEnv(t)

	content := `
compaction_enabled: true
compaction_threshold: 0.7
compaction_keep_recent_turns: 8
`
	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := LoadService(path)
	require.NoError(t, err)

	assert.True(t, cfg.Compaction.Enabled)
	assert.InDelta(t, 0.7, cfg.Compaction.Threshold, 1e-9)
	assert.Equal(t, 8, cfg.Compaction.KeepRecentTurns)
}

func TestCACertFileValidation(t *testing.T) {
	t.Run("empty is allowed (feature disabled)", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.CACertFile = ""
		require.NoError(t, cfg.Validate())
	})

	t.Run("existing file passes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.pem")
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))

		cfg := validServiceConfig()
		cfg.CACertFile = path
		require.NoError(t, cfg.Validate())
	})

	t.Run("missing file is rejected", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.CACertFile = filepath.Join(t.TempDir(), "nope.pem")
		assert.ErrorContains(t, cfg.Validate(), "ca_cert_file")
	})
}

func TestCACertFileFromEnv(t *testing.T) {
	clearServiceEnv(t)

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte("x"), 0o600))

	dir := t.TempDir()
	path := filepath.Join(dir, "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
contextmatrix_url: http://localhost:8080
api_key: 0123456789012345678901234567890123456789
base_image: img@sha256:abc
`), 0o600))

	t.Setenv("CMX_CA_CERT_FILE", caPath)

	cfg, err := LoadService(path)
	require.NoError(t, err)
	assert.Equal(t, caPath, cfg.CACertFile)
	require.NoError(t, cfg.Validate())
}

func TestLogDirValidation(t *testing.T) {
	t.Run("empty is allowed (feature disabled)", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.LogDir = ""
		require.NoError(t, cfg.Validate())
	})

	t.Run("nonexistent dir is created and passes", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "logs")
		cfg := validServiceConfig()
		cfg.LogDir = p
		require.NoError(t, cfg.Validate())

		info, err := os.Stat(p)
		require.NoError(t, err)
		assert.True(t, info.IsDir())
	})

	t.Run("path that is a regular file is rejected", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "afile")
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))

		cfg := validServiceConfig()
		cfg.LogDir = p
		assert.ErrorContains(t, cfg.Validate(), "log_dir")
	})
}

func TestLogDirFromEnv(t *testing.T) {
	clearServiceEnv(t)

	logDir := filepath.Join(t.TempDir(), "logs")

	dir := t.TempDir()
	path := filepath.Join(dir, "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
contextmatrix_url: http://localhost:8080
api_key: 0123456789012345678901234567890123456789
base_image: img@sha256:abc
`), 0o600))

	t.Setenv("CMX_LOG_DIR", logDir)

	cfg, err := LoadService(path)
	require.NoError(t, err)
	assert.Equal(t, logDir, cfg.LogDir)
	require.NoError(t, cfg.Validate())
}

func TestLoadService_ImageListFiltersDefault(t *testing.T) {
	path := writeServeYAML(t, `
contextmatrix_url: http://cm:8080
api_key: 0123456789abcdef0123456789abcdef
base_image: img:dev
`)

	cfg, err := LoadService(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"contextmatrix-agent"}, cfg.ImageListFilters)
}

func TestLoadService_ImageListFiltersFromYAML(t *testing.T) {
	path := writeServeYAML(t, `
contextmatrix_url: http://cm:8080
api_key: 0123456789abcdef0123456789abcdef
base_image: img:dev
image_list_filters:
  - contextmatrix-agent
  - ghcr.io/you/my-worker
`)

	cfg, err := LoadService(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"contextmatrix-agent", "ghcr.io/you/my-worker"}, cfg.ImageListFilters)
}

func TestLoadService_ImageListFiltersEmptyFallsBackToDefault(t *testing.T) {
	path := writeServeYAML(t, `
contextmatrix_url: http://cm:8080
api_key: 0123456789abcdef0123456789abcdef
base_image: img:dev
image_list_filters: []
`)

	cfg, err := LoadService(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"contextmatrix-agent"}, cfg.ImageListFilters)
}

func TestValidate_ImageListFiltersBlankEntryRejected(t *testing.T) {
	path := writeServeYAML(t, `
contextmatrix_url: http://cm:8080
api_key: 0123456789abcdef0123456789abcdef
base_image: img:dev
image_list_filters:
  - "  "
`)

	cfg, err := LoadService(path)
	require.NoError(t, err)

	err = cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image_list_filters")
}

// writeServeYAML writes body to a temp serve.yaml and returns its path.
func writeServeYAML(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

// clearServiceEnv unsets any CMX_* vars that could leak into a default/file
// test from the developer's shell. t.Setenv restores them after the test.
func clearServiceEnv(t *testing.T) {
	t.Helper()

	for _, e := range []string{
		"CMX_CONTEXTMATRIX_URL", "CMX_PORT",
		"CMX_API_KEY", "CMX_BASE_IMAGE", "CMX_MAX_CONCURRENT",
		"CMX_CA_CERT_FILE",
		"CMX_LOG_DIR",
		"CMX_MAX_CARD_COST", "CMX_SELECTOR_PRICE_HEADROOM",
		"CMX_ADMIN_PORT",
		"CMX_COMPACTION_ENABLED", "CMX_COMPACTION_THRESHOLD",
		"CMX_COMPACTION_KEEP_RECENT_TURNS",
	} {
		if _, ok := os.LookupEnv(e); ok {
			t.Setenv(e, "")
			require.NoError(t, os.Unsetenv(e))
		}
	}
}
