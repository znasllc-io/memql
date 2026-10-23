//go:build linux || darwin

package processprobe

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Configure gives a probe its own process group, including version-manager children. Unlike
// a session, no descendant is intended to outlive this short-lived question.
func Configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return Terminate(cmd) }
}

// Terminate stops a probe and any remaining children after completion or cancellation.
func Terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// The group id is the pid assigned at Start. Looking it up after the
	// leader exits fails even when a child still holds the output pipe open.
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
