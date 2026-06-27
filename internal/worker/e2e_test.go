package worker

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These helpers are shared with e2e_orchestrator_test.go, which exercises
// the real autonomous orchestrator FSM end-to-end. HITL runs the FSM in gated
// mode, so the authoritative HITL end-to-end coverage is the FSM suite.

// jsonString quotes s as a JSON string literal (handles the content/args
// escaping the wire format requires).
func jsonString(s string) string {
	var b strings.Builder

	b.WriteByte('"')

	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}

	b.WriteByte('"')

	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	neg := n < 0
	if neg {
		n = -n
	}

	var buf [20]byte

	i := len(buf)

	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	if neg {
		i--
		buf[i] = '-'
	}

	return string(buf[i:])
}

// branchFile reads a file's content from a branch in the bare remote.
func branchFile(t *testing.T, remote, branch, path string) string {
	t.Helper()

	//nolint:gosec // G204: test-controlled branch/path, reading from a temp bare repo
	cmd := exec.Command("git", "show", branch+":"+path)
	cmd.Dir = remote
	cmd.Env = gitEnv()

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git show %s:%s: %s", branch, path, out)

	return string(out)
}
