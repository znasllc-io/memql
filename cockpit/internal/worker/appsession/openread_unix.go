//go:build linux || darwin

package appsession

import (
	"os"
	"syscall"
)

// openForRecord opens a file for the recording to read.
//
// O_NONBLOCK so that a path that became a named pipe after the policy
// looked at it opens at once instead of waiting for a writer that may
// never come -- it is then refused as not regular, on the descriptor.
// O_NOFOLLOW so that a final component that became a link in the same
// window is refused rather than followed out of the workspace.
func openForRecord(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
