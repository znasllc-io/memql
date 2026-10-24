//go:build !linux && !darwin

package processprobe

import "os/exec"

// Configure leaves platform-specific process settings unchanged off Unix.
func Configure(_ *exec.Cmd) {}

// Terminate stops the direct process; descendant cleanup requires Unix groups.
func Terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
