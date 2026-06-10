package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReadOnlyRegistryExcludesMutatingTools(t *testing.T) {
	reg := NewReadOnlyRegistry(t.TempDir())
	for _, name := range []string{"read", "grep", "glob", "git"} {
		_, ok := reg.Get(name)
		assert.True(t, ok, "expected read-only tool %q", name)
	}

	for _, name := range []string{"write", "edit", "bash"} {
		_, ok := reg.Get(name)
		assert.False(t, ok, "mutating tool %q must NOT be in the read-only registry", name)
	}
}
