package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gatesServiceYAML(t *testing.T, extra string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "serve.yaml")
	body := "contextmatrix_url: http://cm:8080\n" + extra
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

func TestGatesDefaults(t *testing.T) {
	clearServiceEnv(t)

	cfg, err := LoadService(filepath.Join(t.TempDir(), "missing.yaml"))
	require.NoError(t, err)

	assert.Equal(t, DefaultGatesPollIntervalSeconds, cfg.GatesPollIntervalSeconds)
	assert.Equal(t, DefaultGatesCIWaitTimeoutSeconds, cfg.GatesCIWaitTimeoutSeconds)
	assert.Equal(t, DefaultGatesCopilotWaitTimeoutSeconds, cfg.GatesCopilotWaitTimeoutSeconds)
	assert.True(t, cfg.GatesCopilotThreadReplies)

	yml, err := DefaultsYAML()
	require.NoError(t, err)
	assert.Contains(t, string(yml), "gates_poll_interval_seconds: 60")
	assert.Contains(t, string(yml), "gates_copilot_thread_replies: true")
}

func TestGatesKeysFromFileAndEnv(t *testing.T) {
	clearServiceEnv(t)

	path := gatesServiceYAML(t, "gates_poll_interval_seconds: 30\ngates_copilot_thread_replies: false\n")
	t.Setenv("CMX_GATES_CI_WAIT_TIMEOUT_SECONDS", "900")

	cfg, err := LoadService(path)
	require.NoError(t, err)

	assert.Equal(t, 30, cfg.GatesPollIntervalSeconds)
	assert.Equal(t, 900, cfg.GatesCIWaitTimeoutSeconds)
	assert.False(t, cfg.GatesCopilotThreadReplies)
}

func TestGatesValidation(t *testing.T) {
	cases := []struct {
		name string
		key  string
		set  func(*ServiceConfig)
	}{
		{"poll zero", "gates_poll_interval_seconds", func(c *ServiceConfig) { c.GatesPollIntervalSeconds = 0 }},
		{"poll negative", "gates_poll_interval_seconds", func(c *ServiceConfig) { c.GatesPollIntervalSeconds = -1 }},
		{"ci wait zero", "gates_ci_wait_timeout_seconds", func(c *ServiceConfig) { c.GatesCIWaitTimeoutSeconds = 0 }},
		{"copilot wait negative", "gates_copilot_wait_timeout_seconds", func(c *ServiceConfig) { c.GatesCopilotWaitTimeoutSeconds = -5 }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validServiceConfig()
			tc.set(&cfg)

			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.key)
		})
	}
}

func TestGatesExtraEnvFallback(t *testing.T) {
	t.Run("folds into the serve key and drops the entry", func(t *testing.T) {
		clearServiceEnv(t)

		path := gatesServiceYAML(t, "worker_extra_env:\n"+
			"  CMX_GATES_POLL_INTERVAL_SECONDS: \"15\"\n"+
			"  CMX_GATES_COPILOT_THREAD_REPLIES: \"false\"\n"+
			"  OTHER: keep\n")

		cfg, err := LoadService(path)
		require.NoError(t, err)

		assert.Equal(t, 15, cfg.GatesPollIntervalSeconds)
		assert.False(t, cfg.GatesCopilotThreadReplies)
		assert.Equal(t, map[string]string{"OTHER": "keep"}, cfg.WorkerExtraEnv)
	})

	t.Run("serve key in the file wins", func(t *testing.T) {
		clearServiceEnv(t)

		path := gatesServiceYAML(t, "gates_poll_interval_seconds: 20\n"+
			"worker_extra_env:\n  CMX_GATES_POLL_INTERVAL_SECONDS: \"15\"\n")

		cfg, err := LoadService(path)
		require.NoError(t, err)

		assert.Equal(t, 20, cfg.GatesPollIntervalSeconds)
		assert.Empty(t, cfg.WorkerExtraEnv)
	})

	t.Run("serve key in the process env wins", func(t *testing.T) {
		clearServiceEnv(t)
		t.Setenv("CMX_GATES_CI_WAIT_TIMEOUT_SECONDS", "600")

		path := gatesServiceYAML(t, "worker_extra_env:\n  CMX_GATES_CI_WAIT_TIMEOUT_SECONDS: \"15\"\n")

		cfg, err := LoadService(path)
		require.NoError(t, err)

		assert.Equal(t, 600, cfg.GatesCIWaitTimeoutSeconds)
		assert.Empty(t, cfg.WorkerExtraEnv)
	})

	t.Run("an invalid entry is rejected naming the serve key", func(t *testing.T) {
		clearServiceEnv(t)

		for _, entry := range []string{
			"CMX_GATES_POLL_INTERVAL_SECONDS: \"soon\"",
			"CMX_GATES_POLL_INTERVAL_SECONDS: \"0\"",
			"CMX_GATES_COPILOT_THREAD_REPLIES: \"maybe\"",
		} {
			_, err := LoadService(gatesServiceYAML(t, "worker_extra_env:\n  "+entry+"\n"))
			require.Error(t, err, entry)
			assert.Contains(t, err.Error(), "gates_")
		}
	})
}

func TestValidateLowerBounds(t *testing.T) {
	t.Run("container_timeout must be positive", func(t *testing.T) {
		for _, d := range []time.Duration{0, -time.Minute} {
			cfg := validServiceConfig()
			cfg.ContainerTimeout = d

			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "container_timeout")
		}
	})

	t.Run("webhook_replay_skew_seconds rejects negative and keeps zero", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.ReplaySkew = -time.Second

		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "webhook_replay_skew_seconds")

		cfg.ReplaySkew = 0
		require.NoError(t, cfg.Validate())
	})
}

func TestLogLevelAndImageFilterDefaults(t *testing.T) {
	clearServiceEnv(t)

	cfg, err := LoadService(filepath.Join(t.TempDir(), "missing.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, []string{"contextmatrix-agent"}, cfg.ImageListFilters)

	yml, err := DefaultsYAML()
	require.NoError(t, err)
	assert.Contains(t, string(yml), "log_level: info")
	assert.Contains(t, string(yml), "- contextmatrix-agent")

	t.Run("log_level is validated", func(t *testing.T) {
		for _, lvl := range []string{"debug", "info", "warn", "error", "INFO"} {
			valid := validServiceConfig()
			valid.LogLevel = lvl
			require.NoError(t, valid.Validate(), lvl)
		}

		bad := validServiceConfig()
		bad.LogLevel = "verbose"

		err := bad.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "log_level")
	})
}
