//go:build !linux && !darwin

package appsession

import "os/exec"

// applyProcessGroup is a no-op off Unix: Setpgid lives in a syscall
// package that only builds on linux/darwin. The worker does not support
// Windows yet, and this keeps the build green on dev machines that are
// neither.
func applyProcessGroup(_ *exec.Cmd) {}

// signalGroup falls back to killing the direct child. The caveat is
// stated rather than hidden: without a process group, a cancel here does
// not reach grandchildren.
func signalGroup(cmd *exec.Cmd, _ bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
