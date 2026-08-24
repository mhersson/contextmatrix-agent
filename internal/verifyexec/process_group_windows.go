//go:build windows

package verifyexec

import (
	"os/exec"
)

// setupProcessGroup configures cmd to cancel by killing the process directly.
// Windows does not support POSIX process groups, so cmd.Cancel falls back to
// per-process kill.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}

		return cmd.Process.Kill()
	}
}
