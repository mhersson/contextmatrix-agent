package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseArgs(t *testing.T) {
	m, err := parseArgs(`{"path":"x","n":2}`)
	require.NoError(t, err)
	assert.Equal(t, "x", m["path"])
	assert.InDelta(t, float64(2), m["n"], 1e-9)

	_, err = parseArgs("")
	require.Error(t, err)

	_, err = parseArgs(`{"path":`) // truncated
	require.Error(t, err)
}

func TestRepairMessage(t *testing.T) {
	msg := repairMessage("read", assertErr("bad json"))
	assert.Contains(t, msg, "read")
	assert.Contains(t, msg, "bad json")
	assert.Contains(t, msg, "valid JSON")
}

type strErr string

func (e strErr) Error() string { return string(e) }
func assertErr(s string) error { return strErr(s) }
