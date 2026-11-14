package cardid_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mhersson/contextmatrix-agent/internal/cardid"
)

func TestValid(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"ABC-12", true},
		{"my-proj-001", true},
		{"", false},
		{"ABC", false},
		{"-1", false},
		{"ABC-1/x", false},
		{"ABC-1 ", false},
	}

	for _, tc := range cases {
		assert.Equal(t, tc.want, cardid.Valid(tc.id), "%q", tc.id)
	}
}
