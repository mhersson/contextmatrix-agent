package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mhersson/contextmatrix-agent/internal/config"
)

func TestCapableDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, model, def, want string
	}{
		{"payload model", "vendor/a", "vendor/b", "vendor/a"},
		{"default model", "", "vendor/b", "vendor/b"},
		{"capable default", "", "", config.DefaultCapableModel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, CapableDefault(tt.model, tt.def))
		})
	}
}
