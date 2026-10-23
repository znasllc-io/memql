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
// The negative pid is the point: kill(-pgid) reaches every descendant.
// It falls back to the direct process if the group is already gone,
// which happens when the child exited between the decision and the
// signal.
func signalGroup(cmd *exec.Cmd, hard bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if hard {
		sig = syscall.SIGKILL
	}
	pid := cmd.Process.Pid
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid > 0 {
		if err := syscall.Kill(-pgid, sig); err == nil {
			return
		}
	}
	_ = cmd.Process.Signal(sig)
}
