//go:build darwin || linux

package selfupdate

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func lockUpdate(directory string) (func(), error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("The update directory must be private to this user")
	}
	var dirStat unix.Stat_t
	if err = unix.Lstat(directory, &dirStat); err != nil || dirStat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("The update directory must belong to this user")
	}
	fd, err := unix.Open(filepath.Join(directory, "lock"), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	var s unix.Stat_t
	if err = unix.Fstat(fd, &s); err != nil || s.Uid != uint32(os.Getuid()) || s.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil, errors.New("Unsafe Cockpit update lock")
	}
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(fd)
		return nil, errors.New("Another Cockpit update is already running")
	}
	return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }, nil
}
func updateLocked(directory string) bool {
	fd, err := unix.Open(filepath.Join(directory, "lock"), unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return true
	}
	_ = unix.Flock(fd, unix.LOCK_UN)
	return false
}
func writableDirectory(directory string) error { return unix.Access(directory, unix.W_OK) }
