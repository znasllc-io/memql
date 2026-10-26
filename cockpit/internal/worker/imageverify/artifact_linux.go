//go:build linux

package imageverify

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

type receiptDestination struct {
	workspace, directory *os.File
	path                 string
	identity             os.FileInfo
}

// Reserve the evidence directory before any source work. A checkout-controlled
// directory or symlink is refused, and later writes use this retained descriptor
// with O_NOFOLLOW/O_EXCL rather than resolving a path through the repository.
func reserveReceiptDestination(workspace string) (*receiptDestination, error) {
	fd, err := syscall.Open(workspace, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parent := os.NewFile(uintptr(fd), workspace)
	if err = syscall.Mkdirat(fd, ".memql-image-build", 0700); err != nil {
		parent.Close()
		return nil, errors.New("image receipt destination must be new")
	}
	child, err := syscall.Openat(fd, ".memql-image-build", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		parent.Close()
		return nil, err
	}
	dir := os.NewFile(uintptr(child), filepath.Join(workspace, ".memql-image-build"))
	stat, err := dir.Stat()
	if err != nil {
		parent.Close()
		dir.Close()
		return nil, err
	}
	return &receiptDestination{parent, dir, dir.Name(), stat}, nil
}
func (d *receiptDestination) Close() { _ = d.directory.Close(); _ = d.workspace.Close() }
func (d *receiptDestination) unchanged() error {
	stat, err := os.Lstat(d.path)
	if err != nil || !stat.IsDir() || !os.SameFile(stat, d.identity) {
		return errors.New("image receipt destination changed")
	}
	return nil
}
func (d *receiptDestination) Write(data []byte) error {
	if len(data) == 0 || len(data) > MaxReceiptBytes {
		return errors.New("image receipt is unbounded")
	}
	if err := d.unchanged(); err != nil {
		return err
	}
	fd, err := syscall.Openat(int(d.directory.Fd()), "verification.json", syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "verification.json")
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = errors.New("short receipt write")
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return d.unchanged()
}
