//go:build windows

package execx

import (
	"os/exec"
	"strconv"
	"syscall"
)

const createNoWindow = 0x08000000

func applyPlatformAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// PrepareTree makes cancellation kill the whole process tree; Windows has no
// process groups, so taskkill /T walks the children.
func PrepareTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		applyPlatformAttrs(kill)
		return kill.Run()
	}
}
