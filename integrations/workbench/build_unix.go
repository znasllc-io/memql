//go:build unix

package workbench

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup puts the build in its own process group and makes a
// context cancellation kill the whole group rather than the shell alone --
// `npm run build` is a tree of processes, and killing only its root leaves
// vite holding the directory the teardown is about to remove.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
