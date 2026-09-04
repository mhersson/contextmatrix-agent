package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
