//go:build linux || darwin

package appsession

import (
	"os/exec"
	"syscall"
)

// applyProcessGroup puts the child in its own process group so a cancel
// can reap the whole tree. An app session forks tools, which fork
// compilers and test runners; killing only the process the cockpit
// started leaves those running on somebody's machine.
func applyProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup signals the child's whole process group.
//
// Setpgid makes the spawned PID the group ID. Keep that original identity:
// looking it up after Wait reaps the leader fails even when descendants still
// hold its stdin or run tools. A failed prompt transfer can reach this path
// after the leader exits, so signal the group before the direct-process fallback.
func signalGroup(cmd *exec.Cmd, hard bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if hard {
		sig = syscall.SIGKILL
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err == nil {
		return
	}
	_ = cmd.Process.Signal(sig)
}
