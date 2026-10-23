//go:build unix

package worker

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// redirectServiceOutput points stdout and stderr at the worker log, as the
// per-user plist's StandardOutPath and StandardErrorPath did. A log that
// cannot be opened leaves them where launchd put them.
func redirectServiceOutput(path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_ = unix.Dup2(int(file.Fd()), 1)
	_ = unix.Dup2(int(file.Fd()), 2)
}
