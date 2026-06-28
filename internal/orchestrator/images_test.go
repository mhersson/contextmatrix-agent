package orchestrator

import (
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-agent/internal/cmclient"
)

func TestDataURLs(t *testing.T) {
	assert.Nil(t, dataURLs(nil))

	got := dataURLs([]cmclient.ImageBlob{
		{MIME: "image/png", Data: []byte{1, 2, 3}},
		{MIME: "image/jpeg", Data: []byte{4, 5}},
	})
	require.Equal(t, []llm.ImageURL{
		{URL: "data:image/png;base64,AQID"},
		{URL: "data:image/jpeg;base64,BAU="},
	}, got)
}
