package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/runinput"
	"github.com/mhersson/contextmatrix-agent/internal/runkind"
	"github.com/mhersson/contextmatrix-agent/internal/runoutcome"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setRunKindEnv(t *testing.T) {
	t.Helper()

	t.Setenv("CM_CARD_ID", "ABC-012")
	t.Setenv("CM_PROJECT", "abc")
	t.Setenv("CM_REPO_URL", "https://github.com/org/repo")
	t.Setenv("CM_BASE_BRANCH", "main")
	t.Setenv("CM_MCP_URL", "")
	t.Setenv("CM_MCP_API_KEY", "")
}

func TestRunKindSpecFromEnv(t *testing.T) {
	setRunKindEnv(t)
	t.Setenv("CM_MODEL", "vendor/model-a")
	t.Setenv("CM_MAX_COST_USD", "0.5")
	t.Setenv("CMX_SELECTION", `{"blacklist":["vendor/model-b"]}`)
	t.Setenv("CMX_CA_CERT_FILE", "/etc/cm/ca.pem")

	spec, err := runKindSpecFromEnv(protocol.RunKindInvestigate)
	require.NoError(t, err, "a run kind needs no MCP URL or key")

	assert.Equal(t, protocol.RunKindInvestigate, spec.Kind)
	assert.Equal(t, "ABC-012", spec.CardID)
	assert.Equal(t, "abc", spec.Project)
	assert.Equal(t, "https://github.com/org/repo", spec.RepoURL)
	assert.Equal(t, "main", spec.BaseBranch)
	assert.Equal(t, "vendor/model-a", spec.Model)
	assert.InDelta(t, 0.5, spec.MaxCostUSD, 1e-9)
	require.NotNil(t, spec.Selection)
	assert.Equal(t, []string{"vendor/model-b"}, spec.Selection.Blacklist)
	assert.Equal(t, "/etc/cm/ca.pem", spec.CACertFile)
	assert.Equal(t, cmEnvFile, spec.SecretsPath)
	assert.Equal(t, filepath.Join(runinput.MountPath, runinput.RunLogFile), spec.RunLogPath)
	assert.Equal(t, "/home/user/workspace", spec.Workspace)
	assert.Equal(t, "/home/user/investigation", spec.InputsDir)
}

func TestRunKindSpecFromEnvRejects(t *testing.T) {
	tests := []struct {
		name, key, value string
	}{
		{"a card id that is not one", "CM_CARD_ID", "../x"},
		{"a missing card id", "CM_CARD_ID", ""},
		{"a missing project", "CM_PROJECT", ""},
		{"a missing repo", "CM_REPO_URL", ""},
		{"a cost that is not a number", "CM_MAX_COST_USD", "lots"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRunKindEnv(t)
			t.Setenv(tt.key, tt.value)

			_, err := runKindSpecFromEnv(protocol.RunKindCheck)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.key)
		})
	}
}

func TestReadRunKindFiles(t *testing.T) {
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, "env")
	inputPath := filepath.Join(dir, "input.json")

	require.NoError(t, os.WriteFile(secretsPath, []byte(
		"CM_GIT_TOKEN=ghs_tok\nLLM_API_KEY=file-key\nLLM_TYPE=claude_code\nLLM_OAUTH_TOKEN=file-oauth\nLLM_EFFORT=high\n"), 0o600))
	require.NoError(t, os.WriteFile(inputPath, []byte(`{"current_command":"./ci/verify"}`), 0o600))

	t.Setenv("LLM_API_KEY", "env-key")
	t.Setenv("LLM_OAUTH_TOKEN", "env-oauth")
	t.Setenv("CM_GIT_TOKEN", "env-tok")

	spec, err := readRunKindFiles(runkind.Spec{SecretsPath: secretsPath}, inputPath)
	require.NoError(t, err)

	assert.Equal(t, "ghs_tok", spec.GitToken, "the git token is file-only")
	assert.Equal(t, "env-key", spec.LLMKey, "the LLM key resolves env first")
	assert.Equal(t, "claude_code", spec.LLMType)
	assert.Equal(t, "file-oauth", spec.LLMOAuthToken, "the OAuth token is file-only")
	assert.Equal(t, "high", spec.LLMEffort)
	assert.JSONEq(t, `{"current_command":"./ci/verify"}`, string(spec.Input))

	_, err = readRunKindFiles(runkind.Spec{SecretsPath: secretsPath}, filepath.Join(dir, "missing.json"))
	require.Error(t, err)

	_, err = readRunKindFiles(runkind.Spec{SecretsPath: filepath.Join(dir, "missing-env")}, inputPath)
	require.Error(t, err)
}

func TestWorkEntersTheRunKindPath(t *testing.T) {
	setRunKindEnv(t)
	t.Setenv("CM_RUN_KIND", protocol.RunKindCheck)
	t.Setenv("CM_CARD_ID", "../x")

	var out bytes.Buffer

	root := NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"work"})

	require.Error(t, root.Execute())

	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	d, ok := runoutcome.Parse(lines[len(lines)-1])
	require.True(t, ok, "a run kind that cannot start still ends its stream with an outcome")
	assert.Equal(t, protocol.StopCauseError, d.Cause)
	assert.Nil(t, d.Result)
}
