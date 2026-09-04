package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clearCMXEnv unsets any CMX_-prefixed variables so validation does not pick
// up the loader's CMX_ environment layer from a developer's shell or runner.
func clearCMXEnv(t *testing.T) {
	t.Helper()

	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "CMX_") {
			continue
		}

		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}
}

func runConfig(t *testing.T, args ...string) (string, error) {
	t.Helper()

	root := NewRootCmd()

	var out bytes.Buffer

	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"config"}, args...))

	err := root.Execute()

	return out.String(), err
}

func TestConfigDefaults(t *testing.T) {
	out, err := runConfig(t, "defaults")
	require.NoError(t, err)
	assert.Contains(t, out, "port: 9092")
}

func TestConfigValidate(t *testing.T) {
	clearCMXEnv(t)

	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	require.NoError(t, os.WriteFile(good, []byte(
		"contextmatrix_url: http://localhost:18080\n"+
			"container_contextmatrix_url: http://172.17.0.1:18080\n"+
			"api_key: 0123456789abcdef0123456789abcdef\n"+
			"base_image: contextmatrix-agent-worker:abc1234\n"), 0o600))

	out, err := runConfig(t, "validate", good)
	require.NoError(t, err)
	assert.Contains(t, out, "ok")

	bad := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(bad, []byte("api_key: short\n"), 0o600))

	_, err = runConfig(t, "validate", bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contextmatrix_url")

	_, err = runConfig(t, "validate", filepath.Join(dir, "missing.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing.yaml")
}
