//go:build darwin || linux

package transfer

import (
	"os"
	"path/filepath"
	"syscall"
)

// LockLocal excludes a second process from the same durable checkpoint. The
// kernel releases it after a crash; the lock's inode must never be removed.
func LockLocal(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrRunning
	}
	return func() { f.Close() }, nil
}
