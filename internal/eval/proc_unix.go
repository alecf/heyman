//go:build unix

package eval

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts cmd in its own process group and kills the whole
// group on cancellation, so pipelines and background jobs die on timeout.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
