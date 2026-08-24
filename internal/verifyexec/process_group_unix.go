//go:build !windows

package verifyexec

import (
	"os/exec"
	"syscall"
)

// setupProcessGroup configures cmd to run in its own process group (the child
// is the group leader: pgid == child pid) and sets cmd.Cancel to kill the
// entire tree on timeout or context cancellation. Without this,
// exec.CommandContext kills only the direct child, leaving grandchildren
// holding the output pipe.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}

		// Negated pgid kills every process in the group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
