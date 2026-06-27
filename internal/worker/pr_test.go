package worker

import (
	"io"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPRCreatorCommand verifies the gh invocation the PRCreator builds: the
// argv shape, the workspace dir, the body on stdin, and an env that carries
// GH_TOKEN over the scrubbed allowlist and nothing else.
func TestPRCreatorCommand(t *testing.T) {
	t.Parallel()

	pc := NewPRCreator("/work/space", "ghs_secrettoken")

	cmd := pc.buildCmd(t.Context(), "Add the widget", "the body\nwith detail", "main", "cm/cmx-001")

	// argv: gh pr create --title <t> --body-file - --base <b> --head <h>
	require.GreaterOrEqual(t, len(cmd.Args), 9)
	assert.Equal(t, "gh", cmd.Args[0])
	assert.Equal(t, []string{
		"gh", "pr", "create",
		"--title", "Add the widget",
		"--body-file", "-",
		"--base", "main",
		"--head", "cm/cmx-001",
	}, cmd.Args)

	// Runs in the workspace.
	assert.Equal(t, "/work/space", cmd.Dir)

	// Body is fed on stdin.
	require.NotNil(t, cmd.Stdin)
	body, err := io.ReadAll(cmd.Stdin)
	require.NoError(t, err)
	assert.Equal(t, "the body\nwith detail", string(body))

	// Env carries GH_TOKEN exactly once and the scrubbed allowlist — no secrets
	// leak from the parent process.
	var ghToken string

	tokenCount := 0

	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "GH_TOKEN=") {
			ghToken = strings.TrimPrefix(kv, "GH_TOKEN=")
			tokenCount++
		}
	}

	assert.Equal(t, 1, tokenCount, "GH_TOKEN present exactly once")
	assert.Equal(t, "ghs_secrettoken", ghToken)

	// The env is the scrub helper + GH_TOKEN, nothing more: every non-GH_TOKEN
	// entry must be an allowlisted key (assert against the env-scrub helper).
	want := tools.ScrubbedEnv([]string{"GH_TOKEN=ghs_secrettoken"})
	assert.ElementsMatch(t, want, cmd.Env, "env must be exactly ScrubbedEnv + GH_TOKEN")
}

// TestParsePRURL pulls the first http(s) URL gh prints to stdout.
func TestParsePRURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
		want string
	}{
		{"plain url", "https://github.com/org/repo/pull/42\n", "https://github.com/org/repo/pull/42"},
		{"with preamble", "Creating pull request...\nhttps://github.com/org/repo/pull/7\n", "https://github.com/org/repo/pull/7"},
		{"no url", "some unexpected output", ""},
		{"trailing space", "  https://github.com/org/repo/pull/9  \n", "https://github.com/org/repo/pull/9"},
	}

	for _, tc := range tests {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, parsePRURL(tc.out))
		})
	}
}
