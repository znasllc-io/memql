//go:build darwin || linux

// Package filetxn coordinates private configuration writes across local clients.
// The lock is a permanent sibling inode, so replacing the data file cannot
// release another process's lock. The kernel releases locks on process exit.
package filetxn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const MaxBytes = 1 << 20

var ErrConflict = errors.New("file changed while being edited")

// Update serializes the entire read-modify-write against other cooperating
// clients. The callback receives nil for an absent file. Errors leave it intact.
func Update(ctx context.Context, path string, change func([]byte) ([]byte, error)) error {
	if !filepath.IsAbs(path) {
		return errors.New("configuration path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	fd, err := syscall.Open(path+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("open configuration lock: %w", err)
	}
	lock := os.NewFile(uintptr(fd), path+".lock")
	defer lock.Close()
	if err := lock.Chmod(0600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("configuration is busy: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := read(path)
	if err != nil {
		return err
	}
	after, err := change(before)
	if err != nil {
		return err
	}
	if len(after) > MaxBytes {
		return errors.New("configuration exceeds 1 MiB")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".memql-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(after); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func read(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("configuration must be a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if len(b) > MaxBytes {
		return nil, errors.New("configuration exceeds 1 MiB")
	}
	return b, err
}

// CompareAndSwap lets a client preserve its own YAML document/comments. A nil
// expected digest means absent; otherwise it is the SHA-256 of the exact bytes
// read. A conflict makes the client repeat its edit against the new document.
func CompareAndSwap(ctx context.Context, path string, expected *string, replacement []byte) error {
	return Update(ctx, path, func(before []byte) ([]byte, error) {
		if expected == nil {
			if before != nil {
				return nil, ErrConflict
			}
		} else {
			sum := sha256.Sum256(before)
			if before == nil || *expected != hex.EncodeToString(sum[:]) {
				return nil, ErrConflict
			}
		}
		return replacement, nil
	})
}
