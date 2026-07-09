package worker

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSecrets writes a secrets env file carrying CM_GIT_TOKEN and returns its
// path, so PRCreator resolves GH_TOKEN from it at call time.
func writeSecrets(t *testing.T, token string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "env")
	require.NoError(t, os.WriteFile(path, []byte("CM_GIT_TOKEN="+token+"\n"), 0o600))

	return path
}

// TestPRCreatorCommand verifies the gh invocation the PRCreator builds: the
// argv shape, the workspace dir, the body on stdin, and an env that carries
// GH_TOKEN (read fresh from the secrets file) over the scrubbed allowlist and
// nothing else.
func TestPRCreatorCommand(t *testing.T) {
	t.Parallel()

	pc := NewPRCreator("/work/space", writeSecrets(t, "ghs_secrettoken"), "", "")

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
		if after, ok := strings.CutPrefix(kv, "GH_TOKEN="); ok {
			ghToken = after
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

// TestPRCreatorTokenRotation pins that the PR path resolves GH_TOKEN fresh from
// the secrets file at call time: after the file is rewritten, the next buildCmd
// carries the NEW token — an end-of-run PR uses the current token, not one
// cached at startup.
func TestPRCreatorTokenRotation(t *testing.T) {
	t.Parallel()

	secretsPath := writeSecrets(t, "ghs_initial")
	pc := NewPRCreator("/work/space", secretsPath, "", "")

	first := pc.buildCmd(t.Context(), "t", "b", "main", "cm/x-1")
	assert.Contains(t, first.Env, "GH_TOKEN=ghs_initial")

	// Rotate the secrets file the way the host refresh loop does.
	require.NoError(t, os.WriteFile(secretsPath, []byte("CM_GIT_TOKEN=ghs_rotated\n"), 0o600))

	second := pc.buildCmd(t.Context(), "t", "b", "main", "cm/x-1")
	assert.Contains(t, second.Env, "GH_TOKEN=ghs_rotated")
	assert.NotContains(t, strings.Join(second.Env, "\n"), "ghs_initial")
}

// TestPRCreatorCACert verifies the gh command's env carries the extra-CA vars
// only when a cert path is configured.
func TestPRCreatorCACert(t *testing.T) {
	t.Parallel()

	t.Run("cert set injects SSL_CERT_FILE and GH_CA_BUNDLE", func(t *testing.T) {
		t.Parallel()

		pc := NewPRCreator("/work/space", writeSecrets(t, "ghs_tok"), "/run/cm-ca/ca.crt", "")
		cmd := pc.buildCmd(t.Context(), "t", "b", "main", "cm/x-1")

		assert.Contains(t, cmd.Env, "SSL_CERT_FILE=/run/cm-ca/ca.crt")
		assert.Contains(t, cmd.Env, "GH_CA_BUNDLE=/run/cm-ca/ca.crt")
		assert.Contains(t, cmd.Env, "GH_TOKEN=ghs_tok")
	})

	t.Run("no cert omits the CA vars", func(t *testing.T) {
		t.Parallel()

		pc := NewPRCreator("/work/space", writeSecrets(t, "ghs_tok"), "", "")
		cmd := pc.buildCmd(t.Context(), "t", "b", "main", "cm/x-1")

		joined := strings.Join(cmd.Env, "\n")
		assert.NotContains(t, joined, "SSL_CERT_FILE")
		assert.NotContains(t, joined, "GH_CA_BUNDLE")
	})
}

// TestPRCreatorGHHost verifies GH_HOST is exported for a GitHub Enterprise repo
// URL (gh cannot infer such a host from the git remote) and omitted when the
// repo URL yields no host, leaving gh on its github.com default.
func TestPRCreatorGHHost(t *testing.T) {
	t.Parallel()

	t.Run("enterprise host sets GH_HOST", func(t *testing.T) {
		t.Parallel()

		pc := NewPRCreator("/work/space", writeSecrets(t, "ghs_tok"), "", "https://acme.ghe.com/org/repo.git")
		cmd := pc.buildCmd(t.Context(), "t", "b", "main", "cm/x-1")

		assert.Contains(t, cmd.Env, "GH_HOST=acme.ghe.com")
	})

	t.Run("empty repo URL omits GH_HOST", func(t *testing.T) {
		t.Parallel()

		pc := NewPRCreator("/work/space", writeSecrets(t, "ghs_tok"), "", "")
		cmd := pc.buildCmd(t.Context(), "t", "b", "main", "cm/x-1")

		assert.NotContains(t, strings.Join(cmd.Env, "\n"), "GH_HOST")
	})
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

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, parsePRURL(tc.out))
		})
	}
}
