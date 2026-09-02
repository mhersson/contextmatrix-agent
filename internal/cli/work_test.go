package cli

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-agent/internal/config"
	"github.com/mhersson/contextmatrix-agent/internal/secrets"
	"github.com/mhersson/contextmatrix-agent/internal/worker"
	"github.com/mhersson/contextmatrix-harness/events"
	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSelfSignedCA writes a self-signed CA PEM to a temp file and returns its
// path - enough for the CA helpers to parse and trust.
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

// TestBuildWorkEmitterStampsAttemptOrdinal pins the RunE wiring site that
// builds the container's event emitter: buildWorkEmitter must thread
// spec.Attempt through the real events.WithEnvelopeFields option, not a
// hardcoded 1 - hardcoding 1 would pass every other test in the suite
// (attempt 1 needs no option, the emitter's own untouched case) while silently
// losing the ordinal that separates a restarted container's transcript from
// its predecessor's.
func TestBuildWorkEmitterStampsAttemptOrdinal(t *testing.T) {
	t.Run("an attempt above 1 is stamped onto the transcript", func(t *testing.T) {
		var buf bytes.Buffer

		cmd := &cobra.Command{}
		cmd.SetOut(&buf)

		emit := buildWorkEmitter(cmd, worker.RunSpec{Attempt: 2})
		emit.Emit(events.ToolCallKind, map[string]any{"tool": "bash"})

		var got map[string]any
		require.NoError(t, json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &got))
		assert.EqualValues(t, 2, got["attempt"])
	})

	t.Run("the first attempt is left unstamped", func(t *testing.T) {
		var buf bytes.Buffer

		cmd := &cobra.Command{}
		cmd.SetOut(&buf)

		emit := buildWorkEmitter(cmd, worker.RunSpec{Attempt: 1})
		emit.Emit(events.ToolCallKind, map[string]any{"tool": "bash"})

		var got map[string]any
		require.NoError(t, json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &got))
		_, ok := got["attempt"]
		assert.False(t, ok, "attempt 1 must not carry an explicit attempt field")
	})
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

func TestSpecFromEnv_CopilotThreadReplies(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected bool
	}{
		{"absent defaults on", "", true},
		{"explicit false disables", "false", false},
		{"true", "true", true},
		{"garbage stays on", "yes", true}, // only exact "false" disables
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setRequired(t)
			t.Setenv("CMX_GATES_COPILOT_THREAD_REPLIES", tc.value)

			spec, err := specFromEnv()
			require.NoError(t, err)
			assert.Equal(t, tc.expected, spec.GatesCopilotThreadReplies)
		})
	}
}

func TestSpecFromEnv_MaxCapability(t *testing.T) {
	t.Run("true", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CM_MAX_CAPABILITY", "true")

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.True(t, spec.MaxCapability)
	})

	t.Run("false", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CM_MAX_CAPABILITY", "false")

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.False(t, spec.MaxCapability)
	})

	t.Run("absent", func(t *testing.T) {
		setRequired(t)

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.False(t, spec.MaxCapability)
	})

	t.Run("non-true", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CM_MAX_CAPABILITY", "yes")

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.False(t, spec.MaxCapability)
	})
}

// TestSpecFromEnv_ReviewAttemptsCap covers the env hop this knob depends on:
// serve forwards the operator's serve.yaml value as CMX_REVIEW_ATTEMPTS_CAP,
// and specFromEnv is the only place the worker reads it.
func TestSpecFromEnv_ReviewAttemptsCap(t *testing.T) {
	t.Run("absent uses the shared default", func(t *testing.T) {
		setRequired(t)

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Equal(t, config.DefaultReviewAttemptsCap, spec.ReviewAttemptsCap)
	})

	t.Run("set value is carried into the spec", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_REVIEW_ATTEMPTS_CAP", "5")

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Equal(t, 5, spec.ReviewAttemptsCap)
	})

	t.Run("unparseable value is an error", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_REVIEW_ATTEMPTS_CAP", "many")

		_, err := specFromEnv()
		require.Error(t, err)
	})
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

func TestSpecFromEnv_GateKnobs(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		setRequired(t)

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Equal(t, time.Duration(0), spec.ContainerTimeout, "CMX_CONTAINER_TIMEOUT_SECONDS unset must default to 0 (unknown)")
		assert.Equal(t, 60*time.Second, spec.GatesPollInterval)
		assert.Equal(t, 45*time.Minute, spec.GatesCIWaitTimeout)
		assert.Equal(t, 20*time.Minute, spec.GatesCopilotWaitTimeout)
	})

	t.Run("valid_override", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_CONTAINER_TIMEOUT_SECONDS", "5400")
		t.Setenv("CMX_GATES_CI_WAIT_TIMEOUT_SECONDS", "600")

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Equal(t, 90*time.Minute, spec.ContainerTimeout)
		assert.Equal(t, 10*time.Minute, spec.GatesCIWaitTimeout)
	})

	t.Run("garbage_container_timeout", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_CONTAINER_TIMEOUT_SECONDS", "not-a-number")

		_, err := specFromEnv()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CMX_CONTAINER_TIMEOUT_SECONDS")
	})

	t.Run("garbage_gates_poll_interval", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_GATES_POLL_INTERVAL_SECONDS", "not-a-number")

		_, err := specFromEnv()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CMX_GATES_POLL_INTERVAL_SECONDS")
	})

	t.Run("garbage_gates_ci_wait_timeout", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_GATES_CI_WAIT_TIMEOUT_SECONDS", "not-a-number")

		_, err := specFromEnv()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CMX_GATES_CI_WAIT_TIMEOUT_SECONDS")
	})

	t.Run("garbage_gates_copilot_wait_timeout", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_GATES_COPILOT_WAIT_TIMEOUT_SECONDS", "not-a-number")

		_, err := specFromEnv()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CMX_GATES_COPILOT_WAIT_TIMEOUT_SECONDS")
	})
}

func TestSpecFromEnv_DefaultModelFallback(t *testing.T) {
	t.Run("uses_capable_default_when_unset", func(t *testing.T) {
		setRequired(t)

		spec, err := specFromEnv()
		require.NoError(t, err)
		// Mirrors how specFromEnv derives the fallback: config's capable default.
		assert.Equal(t, config.DefaultCapableModel, spec.DefaultModel)
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

func TestWithRunID(t *testing.T) {
	t.Run("with_id_attaches_attribute", func(t *testing.T) {
		var buf bytes.Buffer

		logger := slog.New(slog.NewTextHandler(&buf, nil))
		withRunID(logger, "run-abc").Info("worker line")

		assert.Contains(t, buf.String(), "run_id=run-abc")
	})

	t.Run("empty_id_leaves_output_unchanged", func(t *testing.T) {
		var withHelper, baseline bytes.Buffer

		// Suppress the time key: two independent handlers capture time.Now()
		// separately, and a scheduling boundary between them diverges the raw
		// strings even when the loggers are otherwise identical.
		dropTime := func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return a
		}

		base := slog.New(slog.NewTextHandler(&baseline, &slog.HandlerOptions{ReplaceAttr: dropTime}))
		base.Info("worker line", "card_id", "PROJ-1")

		withRunID(slog.New(slog.NewTextHandler(&withHelper, &slog.HandlerOptions{ReplaceAttr: dropTime})), "").
			Info("worker line", "card_id", "PROJ-1")

		assert.Equal(t, baseline.String(), withHelper.String())
	})
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

	t.Run("malformed records a config error and leaves Verify nil", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_VERIFY", "{not json")

		// Mirrors CMX_SELECTION: a malformed value is a warning, not a fatal
		// error - specFromEnv runs before the MCP client exists, so an early
		// return could not tell the card anything. The run proceeds and the
		// recorded error drives the verify ladder to note and park instead.
		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Nil(t, spec.Verify)
		assert.Contains(t, spec.VerifyConfigError, "could not be parsed")
	})

	t.Run("unknown fields decode to an all-zero config and are a config error", func(t *testing.T) {
		setRequired(t)
		// json.Unmarshal ignores unknown fields, so a misspelled key parses
		// cleanly into a zero value and would otherwise skip Tier 1 silently.
		t.Setenv("CMX_VERIFY", `{"cmd":"make test"}`)

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Nil(t, spec.Verify)
		assert.Contains(t, spec.VerifyConfigError, "empty verify config")
	})

	t.Run("a command-less config is legitimate and is kept", func(t *testing.T) {
		setRequired(t)
		// CM's ResolveVerify returns a non-nil config when ANY field is set, so a
		// project that declares only a timeout is a real, supported setting: the
		// command comes from detection and the timeout still applies.
		t.Setenv("CMX_VERIFY", `{"timeout_seconds":900}`)

		spec, err := specFromEnv()
		require.NoError(t, err)
		require.NotNil(t, spec.Verify)
		assert.Equal(t, 900, spec.Verify.TimeoutSeconds)
		assert.Empty(t, spec.VerifyConfigError)
	})
}

// TestVerifyConfigAllZeroPredicateCoversEveryField guards the all-zero decode
// check in specFromEnv (the `vc.Command == "" && vc.TimeoutSeconds == 0 &&
// len(vc.Env) == 0` predicate), which lists protocol.VerifyConfig's fields by
// name rather than reflecting over them. If the struct gains a fourth field
// and a server sends a config carrying only it, an agent still on this
// predicate decodes all-zero and parks the card as blocked instead of
// degrading quietly to detection - worker images routinely lag server
// upgrades, so that mismatch is a real deployment scenario, not a hypothetical.
// This test does not change protocol.VerifyConfig; it fails loudly the day
// someone else does, pointing at the predicate that must be updated with it.
func TestVerifyConfigAllZeroPredicateCoversEveryField(t *testing.T) {
	got := reflect.TypeFor[protocol.VerifyConfig]().NumField()
	require.Equal(t, 3, got,
		"protocol.VerifyConfig gained or lost a field (now %d): update the all-zero "+
			"check in specFromEnv (internal/cli/work.go) to cover it, then update this constant", got)
}

func TestSpecFromEnv_Mob(t *testing.T) {
	t.Run("absent mob yields nil", func(t *testing.T) {
		setRequired(t)

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Nil(t, spec.Mob, "no CM_MOB_* env must leave Mob nil (mob session off)")
	})

	t.Run("round trip", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CM_MOB_PARTICIPANTS", "3")
		t.Setenv("CM_MOB_PHASES", "plan,review")
		t.Setenv("CM_MOB_ROUNDS", "2")
		t.Setenv("CM_MOB_BUDGET_FACTOR", "0.75")

		spec, err := specFromEnv()
		require.NoError(t, err)
		require.NotNil(t, spec.Mob)
		assert.Equal(t, 3, spec.Mob.Participants)
		assert.Equal(t, []string{"plan", "review"}, spec.Mob.Phases)
		assert.Equal(t, 2, spec.Mob.Rounds)
		assert.InDelta(t, 0.75, spec.Mob.BudgetFactor, 1e-9)
	})

	t.Run("participants below two yields nil", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CM_MOB_PARTICIPANTS", "1")

		spec, err := specFromEnv()
		require.NoError(t, err)
		assert.Nil(t, spec.Mob, "participants < 2 is off; CM never sends it, be defensive")
	})

	t.Run("invalid participants errors", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CM_MOB_PARTICIPANTS", "x")

		_, err := specFromEnv()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CM_MOB_PARTICIPANTS")
	})
}

func TestWorkSpecMobCheckpointEnv(t *testing.T) {
	// Same setup as the CM_BEST_OF_N test above: required envs + the same
	// spec-construction call. Only the mob envs differ.
	setRequired(t)
	t.Setenv("CM_MOB_PARTICIPANTS", "3")
	t.Setenv("CM_MOB_PHASES", "plan,execute")
	t.Setenv("CM_MOB_EXECUTE_CHECKPOINTS", "true")
	t.Setenv("CM_MOB_CHECKPOINT_MIN_TIER", "simple")
	t.Setenv("CM_MOB_CHECKPOINT_ROUNDS", "3")

	spec, err := specFromEnv()
	require.NoError(t, err)
	require.NotNil(t, spec.Mob)
	assert.True(t, spec.Mob.ExecuteCheckpoints)
	assert.Equal(t, "simple", spec.Mob.CheckpointMinTier)
	assert.Equal(t, 3, spec.Mob.CheckpointRounds)
	assert.Equal(t, []string{"plan", "execute"}, spec.Mob.Phases)
}

func TestMobGuestsFromSecrets(t *testing.T) {
	openSource := func(t *testing.T, content string) *secrets.Source {
		t.Helper()

		path := filepath.Join(t.TempDir(), "env")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

		src, err := secrets.Open(path)
		require.NoError(t, err)

		return src
	}

	t.Run("parses guest specs", func(t *testing.T) {
		src := openSource(t,
			"CM_GIT_TOKEN=tok\nCM_MOB_GUESTS="+
				`[{"name":"laptop","url":"http://10.0.0.5:8484","token":"guest-secret"}]`+"\n")

		guests := mobGuestsFromSecrets(src)
		require.Len(t, guests, 1)
		assert.Equal(t, "laptop", guests[0].Name)
		assert.Equal(t, "http://10.0.0.5:8484", guests[0].URL)
		assert.Equal(t, "guest-secret", guests[0].Token)
	})

	t.Run("absent key yields nil", func(t *testing.T) {
		src := openSource(t, "CM_GIT_TOKEN=tok\n")
		assert.Nil(t, mobGuestsFromSecrets(src))
	})

	t.Run("malformed json degrades to nil", func(t *testing.T) {
		src := openSource(t, "CM_MOB_GUESTS=not-json\n")
		assert.Nil(t, mobGuestsFromSecrets(src), "a broken guest list must never fail the run")
	})
}

type blockingCloser struct{ release chan struct{} }

func (b *blockingCloser) Close() error {
	<-b.release

	return nil
}

func TestCloseBoundedReturnsOnHang(t *testing.T) {
	c := &blockingCloser{release: make(chan struct{})}
	defer close(c.release)

	start := time.Now()

	closeBounded(c, 20*time.Millisecond)

	assert.Less(t, time.Since(start), time.Second, "a hung Close must not block the worker exit")
}

func TestCloseBoundedWaitsForFastClose(t *testing.T) {
	c := &blockingCloser{release: make(chan struct{})}
	close(c.release)

	closeBounded(c, time.Second) // returns immediately, no panic, no leak
}

func TestSpecFromEnvParsesReadOnlyRoots(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{"absent", "", nil},
		{"single root", "/opt/python", []string{"/opt/python"}},
		{"two roots", "/a:/b", []string{"/a", "/b"}},
		{"blank and empty entries dropped", ":/a::  :/b:", []string{"/a", "/b"}},
		{"only separators", "::", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setRequired(t)
			t.Setenv("CMX_READ_ONLY_ROOTS", tc.value)

			spec, err := specFromEnv()
			require.NoError(t, err)
			assert.Equal(t, tc.want, spec.ReadOnlyRoots)
		})
	}
}

func TestSpecFromEnv_Attempt(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{"absent is the first attempt", "", 1},
		{"zero is the first attempt", "0", 1},
		{"negative is the first attempt", "-3", 1},
		{"first attempt", "1", 1},
		{"later attempt", "4", 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setRequired(t)
			t.Setenv("CMX_ATTEMPT", tc.value)

			spec, err := specFromEnv()
			require.NoError(t, err)
			assert.Equal(t, tc.want, spec.Attempt)
		})
	}

	t.Run("garbage is an error", func(t *testing.T) {
		setRequired(t)
		t.Setenv("CMX_ATTEMPT", "two")

		_, err := specFromEnv()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CMX_ATTEMPT")
	})
}
