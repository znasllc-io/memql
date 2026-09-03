//go:build !unix

package workbench

import "os/exec"

// isolateProcessGroup is a no-op where process groups are not a thing; the
// context cancellation still kills the shell.
func isolateProcessGroup(_ *exec.Cmd) {}
