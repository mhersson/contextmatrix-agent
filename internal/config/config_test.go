package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadPrecedenceDefaultsFileEnvFlags(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("model: file/model\nmax-turns: 10\n"), 0o644))

	t.Setenv("CMX_MAX_TURNS", "20") // env beats file

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.String("model", "", "")
	fs.Int("max-turns", 0, "")
	require.NoError(t, fs.Parse([]string{"--model=flag/model"})) // flag beats file; max_turns NOT passed

	c, err := Load(fs, cfgPath)
	require.NoError(t, err)
	require.NotNil(t, c.Model)
	assert.Equal(t, "flag/model", *c.Model) // flag wins
	require.NotNil(t, c.MaxTurns)
	assert.Equal(t, 20, *c.MaxTurns) // env beats file; flag not passed for this key
	require.NotNil(t, c.MaxCostUSD)
	assert.Equal(t, 0.50, *c.MaxCostUSD) // default survives (untouched anywhere)
}

func TestDefaultsAndValidate(t *testing.T) {
	d := Defaults()
	require.NotNil(t, d.MaxTurns)
	assert.Equal(t, 30, *d.MaxTurns)
	require.NoError(t, d.Validate())

	bad := 0
	c := Config{MaxTurns: &bad}
	err := c.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max-turns") // user-facing hyphenated key

	badCost := -1.0
	cc := Config{MaxCostUSD: &badCost}
	errc := cc.Validate()
	require.Error(t, errc)
	assert.Contains(t, errc.Error(), "max-cost")
}

func TestLoadUnsetStaysNil(t *testing.T) {
	c, err := Load(nil, "")
	require.NoError(t, err)
	assert.Nil(t, c.Provider)  // never set anywhere
	assert.Nil(t, c.Reasoning) // never set anywhere
}

func TestPrintRedacted(t *testing.T) {
	var buf bytes.Buffer
	PrintRedacted(&buf, Defaults())
	out := buf.String()
	assert.Contains(t, out, "redacted")
	assert.NotContains(t, out, "sk-")
}

func TestCapabilitiesFileLoadsAndPrints(t *testing.T) {
	t.Setenv("CMX_CAPABILITIES_FILE", "/tmp/caps.json")
	c, err := Load(nil, "")
	require.NoError(t, err)
	require.NotNil(t, c.CapabilitiesFile)
	assert.Equal(t, "/tmp/caps.json", *c.CapabilitiesFile)

	var buf bytes.Buffer
	PrintRedacted(&buf, c)
	assert.Contains(t, buf.String(), "capabilities-file: /tmp/caps.json")
}
