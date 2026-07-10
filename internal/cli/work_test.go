package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/mhersson/contextmatrix-agent/internal/secrets"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSelfSignedCA writes a self-signed CA PEM to a temp file and returns its
// path — enough for the CA helpers to parse and trust.
func writeSelfSignedCA(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))

	return path
}

func TestCAInjections(t *testing.T) {
	t.Run("empty path yields no options", func(t *testing.T) {
		l, m, err := caInjections("")
		require.NoError(t, err)
		assert.Empty(t, l)
		assert.Empty(t, m)
	})

	t.Run("valid cert yields one llm and one cmclient option", func(t *testing.T) {
		l, m, err := caInjections(writeSelfSignedCA(t))
		require.NoError(t, err)
		assert.Len(t, l, 1)
		assert.Len(t, m, 1)
	})

	t.Run("bad path errors", func(t *testing.T) {
		_, _, err := caInjections(filepath.Join(t.TempDir(), "nope.pem"))
		require.Error(t, err)
	})
}

// TestResolveLLMValue pins the env-first-then-file resolution the worker uses
// for LLM endpoint values: a set container env var wins (set-but-empty counts
// as set, so an empty LLM_BASE_URL overrides the file with "use the canonical
// default"), and an unset env var falls back to the mounted secrets file.
func TestResolveLLMValue(t *testing.T) {
	writeEnvFile := func(t *testing.T, body string) *secrets.Source {
		t.Helper()

		path := filepath.Join(t.TempDir(), "env")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

		src, err := secrets.Open(path)
		require.NoError(t, err)

		return src
	}

	t.Run("env set wins over file", func(t *testing.T) {
		src := writeEnvFile(t, "LLM_API_KEY=from-file\n")
		t.Setenv("LLM_API_KEY", "from-env")

		assert.Equal(t, "from-env", resolveLLMValue("LLM_API_KEY", src))
	})

	t.Run("env set-but-empty wins over file", func(t *testing.T) {
		src := writeEnvFile(t, "LLM_BASE_URL=https://from-file/v1\n")
		t.Setenv("LLM_BASE_URL", "")

		assert.Empty(t, resolveLLMValue("LLM_BASE_URL", src),
			"set-but-empty env must win: empty base url means the type's canonical default")
	})

	t.Run("env unset falls back to file", func(t *testing.T) {
		src := writeEnvFile(t, "LLM_TYPE=openai\n")
		// LLM_TYPE deliberately not set in the environment.
		assert.Equal(t, "openai", resolveLLMValue("LLM_TYPE", src))
	})
}

func TestDialectFromType(t *testing.T) {
	assert.Equal(t, llm.DialectOpenAI, dialectFromType("openai"))
	assert.Equal(t, llm.DialectOpenRouter, dialectFromType("openrouter"))
	assert.Equal(t, llm.DialectOpenRouter, dialectFromType(""))
	assert.Equal(t, llm.DialectOpenRouter, dialectFromType("anything-else"))
}

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

func TestSpecFromEnv_CACertFile(t *testing.T) {
	t.Run("absent leaves it empty", func(t *testing.T) {
		setRequired(t)

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Empty(t, spec.CACertFile)
	})

	t.Run("set is threaded onto the spec", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_CA_CERT_FILE", "/run/cm-ca/ca.crt")

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Equal(t, "/run/cm-ca/ca.crt", spec.CACertFile)
	})
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
		assert.Equal(t, 131072, spec.ToolOutputMax)
		assert.Equal(t, derefInt(config.Defaults().MaxTurns), spec.MaxTurns)
		assert.Equal(t, 0, spec.BestOfN, "CM_BEST_OF_N unset must default to 0 (normal run)")
	})

	t.Run("valid_override", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_BASH_TIMEOUT_MAX_SECONDS", "120")
		t.Setenv("CMX_TOOL_OUTPUT_MAX_BYTES", "50000")
		t.Setenv("CMX_MAX_TURNS", "50")
		t.Setenv("CM_BEST_OF_N", "3")

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Equal(t, 120, spec.BashTimeoutMax)
		assert.Equal(t, 50000, spec.ToolOutputMax)
		assert.Equal(t, 50, spec.MaxTurns)
		assert.Equal(t, 3, spec.BestOfN)
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

	t.Run("garbage_best_of_n", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CM_BEST_OF_N", "x")

		_, err := specFromEnv()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CM_BEST_OF_N")
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

func TestSpecFromEnv_CardIDShape(t *testing.T) {
	valid := []string{"CM-001", "ALPHA-042", "TEST-12345", "cmx-7", "A2-001", "MY-PROJ-001", "A--1"}
	for _, id := range valid {
		t.Run("valid_"+id, func(t *testing.T) {
			setRequired(t)
			t.Setenv("CM_CARD_ID", id)

			spec, err := specFromEnv()
			require.NoError(t, err)
			assert.Equal(t, id, spec.CardID)
		})
	}

	invalid := []struct {
		name string
		id   string
	}{
		{"colon", "CM:001"},
		{"slash", "cm/evil"},
		{"space", "CM 001"},
		{"leading dash", "-001"},
		{"empty", ""},
		{"no dash", "main"},
		{"trailing junk after digits", "CM-001x"},
		{"empty numeric part", "CM-"},
		{"refspec injection", "CM-001:refs/heads/main"},
		{"path traversal", "../etc"},
	}
	for _, tc := range invalid {
		t.Run("invalid_"+tc.name, func(t *testing.T) {
			setRequired(t)
			t.Setenv("CM_CARD_ID", tc.id)

			_, err := specFromEnv()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "CM_CARD_ID")
		})
	}
}

func TestSpecFromEnv_OptionalVars(t *testing.T) {
	setRequired(t)
	t.Setenv("CM_BASE_BRANCH", "main")
	t.Setenv("CM_MODEL", "anthropic/claude-3-5-sonnet")

	spec, err := specFromEnv()
	require.NoError(t, err)

	assert.Equal(t, "main", spec.BaseBranch)
	assert.Equal(t, "anthropic/claude-3-5-sonnet", spec.Model)
}

func TestSpecFromEnv_Compaction(t *testing.T) {
	t.Run("defaults_disabled", func(t *testing.T) {
		setRequired(t)

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.False(t, spec.CompactionEnabled, "compaction disabled by default")
		assert.InDelta(t, 0.85, spec.CompactionThreshold, 1e-9)
		assert.Equal(t, 6, spec.CompactionKeepRecentTurns)
	})

	t.Run("enabled_via_env", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_COMPACTION_ENABLED", "true")
		t.Setenv("CMX_COMPACTION_THRESHOLD", "0.8")
		t.Setenv("CMX_COMPACTION_KEEP_RECENT_TURNS", "4")

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.True(t, spec.CompactionEnabled)
		assert.InDelta(t, 0.8, spec.CompactionThreshold, 1e-9)
		assert.Equal(t, 4, spec.CompactionKeepRecentTurns)
	})

	t.Run("garbage_threshold_errors", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_COMPACTION_THRESHOLD", "high")

		_, err := specFromEnv()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CMX_COMPACTION_THRESHOLD")
	})
}

func TestSpecFromEnv_Verify(t *testing.T) {
	t.Run("absent_leaves_nil", func(t *testing.T) {
		setRequired(t)

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Nil(t, spec.Verify)
	})

	t.Run("parsed", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_VERIFY", `{"command":"cargo test","timeout_seconds":900,"env":["JAVA_HOME"]}`)

		spec, err := specFromEnv()
		require.NoError(t, err)
		require.NotNil(t, spec.Verify)
		assert.Equal(t, "cargo test", spec.Verify.Command)
		assert.Equal(t, 900, spec.Verify.TimeoutSeconds)
		assert.Equal(t, []string{"JAVA_HOME"}, spec.Verify.Env)
	})

	t.Run("malformed_degrades_to_nil", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_VERIFY", "{not json")

		// Mirrors CMX_SELECTION: a malformed value is a warning, not an error —
		// the run proceeds and falls back to detection.
		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Nil(t, spec.Verify)
	})
}
