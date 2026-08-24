//go:build !windows

package verifyexec

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecTimeoutKillsProcessGroup pins that a verify timeout kills the whole
// process tree, not just the direct child.
//
// The shell backgrounds a grandchild that outlives its parent and inherits
// stdout (the CombinedOutput pipe), then execs a longer-running child. Two
// distinct things must hold, and they fail independently:
//
//   - Exec returns promptly rather than blocking on the inherited pipe.
//   - The grandchild is actually dead.
//
// The second assertion is the one that pins setupProcessGroup. cmd.WaitDelay
// alone satisfies the first - Wait returns once the delay elapses even while a
// grandchild still holds the pipe - but leaves the grandchild running, which is
// precisely the leak this fix exists to prevent: a timed-out `go test` or `node`
// would keep burning container CPU for the rest of the run.
func TestExecTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	// The grandchild sleeps far longer than the assertions below wait, so a
	// pass can never come from it exiting on its own. It stays short enough
	// that a genuine failure does not strand a long-lived orphan.
	start := time.Now()

	out := Exec(context.Background(), dir, []string{
		"sh", "-c", "sleep 30 & echo $! > " + pidFile + "; exec sleep 5",
	}, 100*time.Millisecond, nil)

	elapsed := time.Since(start)

	assert.Less(t, elapsed, 5*time.Second,
		"expected completion in ~3s (timeout + WaitDelay), took %v: a grandchild is blocking the output pipe", elapsed)
	assert.True(t, out.TimedOut,
		"expected TimedOut=true, got ExitCode=%d StartErr=%t", out.ExitCode, out.StartErr)
	assert.Equal(t, -1, out.ExitCode)

	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err, "the shell must have recorded the grandchild pid")

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err, "grandchild pid must parse")
	require.Positive(t, pid)

	// Signal 0 probes liveness without delivering anything. Poll rather than
	// probing once: the grandchild is reparented on its parent's death and may
	// be briefly reapable-but-present after the group kill lands.
	require.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) != nil
	}, 3*time.Second, 50*time.Millisecond,
		"grandchild %d survived the verify timeout: the process group was not killed", pid)
}
