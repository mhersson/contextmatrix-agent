package orchestrator

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFinishToolIsTerminal(t *testing.T) {
	ft := NewFinishTool()

	assert.Equal(t, "finish", ft.Name())

	term, ok := ft.(tools.Terminal)
	require.True(t, ok, "finish must implement tools.Terminal")
	assert.True(t, term.Terminal())

	res, err := ft.Execute(context.Background(), map[string]any{})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Text)
}

// guard: the finish schema must steer the model to put its summary in the
// commit message rather than emit a prose recap in the same turn (discarded
// tokens the harness never surfaces).
func TestFinishSchemaDiscouragesProseRecap(t *testing.T) {
	desc := NewFinishTool().Schema().Function.Description
	assert.Contains(t, desc, "commit_message only")
	assert.Contains(t, desc, "discarded")
}

func TestFinishCommitMessage(t *testing.T) {
	cases := []struct {
		name string
		args json.RawMessage
		want string
	}{
		{"valid", json.RawMessage(`{"commit_message":"feat(api): add health endpoint"}`), "feat(api): add health endpoint"},
		{"trims", json.RawMessage(`{"commit_message":"  feat: x  "}`), "feat: x"},
		{"empty object", json.RawMessage(`{}`), ""},
		{"nil", nil, ""},
		{"json null", json.RawMessage("null"), ""},
		{"malformed", json.RawMessage(`{bad`), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, finishCommitMessage(tc.args))
		})
	}
}
