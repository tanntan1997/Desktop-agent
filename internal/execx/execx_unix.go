//go:build !windows

package execx

import (
	"os/exec"
	"syscall"
)

func applyPlatformAttrs(cmd *exec.Cmd) {}

// PrepareTree runs cmd in its own process group so cancelling the context
// terminates the command and every child it spawned (pytest, gradle, ...).
func PrepareTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}
