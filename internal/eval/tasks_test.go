package eval

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultTasks(t *testing.T) {
	c, err := DefaultTasks("coder")
	require.NoError(t, err)
	assert.NotEmpty(t, c)

	all, err := DefaultTasks("all")
	require.NoError(t, err)
	assert.Greater(t, len(all), len(c))

	_, err = DefaultTasks("bogus")
	assert.Error(t, err)
}
