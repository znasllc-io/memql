//go:build linux

package appsession

import (
	"os"
	"strconv"
)

// openedPath is where the kernel says an open file is: the link
// /proc/self/fd/N names the file the descriptor holds, whatever path led
// to it. false when /proc cannot say -- not mounted, in some containers --
// and the caller falls back to resolving the path again.
func openedPath(f *os.File) (string, bool) {
	rc, err := f.SyscallConn()
	if err != nil {
		return "", false
	}
	var where string
	var readErr error
	// Control rather than Fd: Fd would switch the descriptor back to
	// blocking mode, and the open was non-blocking on purpose.
	if err := rc.Control(func(fd uintptr) {
		where, readErr = os.Readlink("/proc/self/fd/" + strconv.FormatUint(uint64(fd), 10))
	}); err != nil || readErr != nil || where == "" || where[0] != '/' {
		return "", false
	}
	return where, true
}
