//go:build !linux && !darwin

package tools

import (
	"os"
	"os/exec"
)

// applyShellSysProcAttr on non-Unix platforms is a no-op. Setpgid,
// Credential, and rlimits all live under syscall packages that
// only build on linux/darwin in the standard library; Windows
// would need a different code path entirely. Until the worker
// supports Windows, this stub keeps the build green on dev
// machines that aren't linux/darwin.
func applyShellSysProcAttr(_ *exec.Cmd, _ ShellLimits) error {
	return nil
}

func prepareShellLimits(_ *exec.Cmd, _ ShellLimits) error { return nil }

// ownProcessGroup has no process group to make here; see exec_unix.go.
func ownProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup can reach only the process itself here, and only to
// kill it.
func signalProcessGroup(cmd *exec.Cmd, _ bool) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// exitStatusOf is the process's exit code.
func exitStatusOf(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	return state.ExitCode()
}
