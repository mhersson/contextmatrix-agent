package llm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCatalog(t *testing.T) {
	raw := `{"data":[
		{"id":"vendor/weak","context_length":8192,"pricing":{"prompt":"0.0000002","completion":"0.0000006"},"supported_parameters":["tools","tool_choice"]},
		{"id":"vendor/notools","context_length":4096,"pricing":{"prompt":"0.0000001","completion":"0.0000001"},"supported_parameters":["temperature"]}
	]}`
	cat, err := ParseCatalog(strings.NewReader(raw))
	require.NoError(t, err)
	require.Len(t, cat, 2)

	weak, ok := cat.Find("vendor/weak")
	require.True(t, ok)
	assert.True(t, weak.SupportsTools())
	assert.Equal(t, 8192, weak.ContextLength)
	assert.InEpsilon(t, 0.0000002, weak.PromptPricePerTok, 1e-12)

	nt, ok := cat.Find("vendor/notools")
	require.True(t, ok)
	assert.False(t, nt.SupportsTools())
}
