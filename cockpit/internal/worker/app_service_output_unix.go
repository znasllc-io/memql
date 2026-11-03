//go:build unix

package worker

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// redirectServiceOutput points stdout and stderr at the worker log where
// launchd gave them nowhere to go. The agent registered from the bundle
// has no StandardOutPath, so launchd connects both to /dev/null. A per-user
// plist's StandardOutPath and StandardErrorPath have already opened a file,
// and that file stays: the owner may have chosen it. A log that cannot be
// opened leaves them where launchd put them.
func redirectServiceOutput(path string) {
	var targets []int
	for _, stream := range []struct {
		fd   int
		file *os.File
	}{{1, os.Stdout}, {2, os.Stderr}} {
		if !isRegularFile(stream.file) {
			targets = append(targets, stream.fd)
		}
	}
	if len(targets) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	for _, fd := range targets {
		_ = unix.Dup2(int(file.Fd()), fd)
	}
}

// isRegularFile reports whether an open stream already writes to a file.
func isRegularFile(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode().IsRegular()
}
