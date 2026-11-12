package modelexec

import (
	"os"
	"regexp"
	"testing"

	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The worker images ship the claude binary the executor was tested against.
func TestWorkerImagePinsTheSupportedClaudeCodeVersion(t *testing.T) {
	tests := []struct {
		name string
		file string
		re   *regexp.Regexp
	}{
		{"Dockerfile ARG", "../../docker/Dockerfile.worker", regexp.MustCompile(`(?m)^ARG CLAUDE_CODE_VERSION=(\S+)$`)},
		{"Makefile default", "../../Makefile", regexp.MustCompile(`(?m)^CLAUDE_CODE_VERSION\s*\?=\s*(\S+)$`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := os.ReadFile(tt.file)
			require.NoError(t, err)

			m := tt.re.FindSubmatch(data)
			require.NotNil(t, m, "no CLAUDE_CODE_VERSION pin in %s", tt.file)
			assert.Equal(t, claudecode.Version, string(m[1]))
		})
	}
}
