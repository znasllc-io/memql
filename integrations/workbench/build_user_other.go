//go:build !unix

package workbench

import "os/exec"

// applyBuildUser is a no-op where there is no uid to drop to. The engine does
// not run in production anywhere this file compiles; a developer's machine
// reaches it, and their own account is already not the one worth protecting.
func applyBuildUser(_ *exec.Cmd, _ string) (int, string) {
	return 0, "this platform has no build user to drop to, so the build runs as this process"
}
