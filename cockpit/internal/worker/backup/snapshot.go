package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// snapshot reads a stable source into a private disk spool, never RAM. Uploads
// read this immutable copy, so editing a host file halfway through a large
// upload cannot produce a composite of two versions. A new process rebuilds
// the snapshot and resumes only when its digest matches the saved session.
func snapshot(ctx context.Context, stateDir, path string) (string, Stamp, error) {
	approved, err := os.Lstat(path)
	if err != nil {
		return "", Stamp{}, err
	}
	if !approved.Mode().IsRegular() {
		return "", Stamp{}, fmt.Errorf("backup: source is not a regular file")
	}
	source, err := os.Open(path)
	if err != nil {
		return "", Stamp{}, err
	}
	defer source.Close()
	before, err := source.Stat()
	if err != nil {
		return "", Stamp{}, err
	}
	if !before.Mode().IsRegular() || !os.SameFile(approved, before) {
		return "", Stamp{}, fmt.Errorf("backup: source is not a regular file")
	}
	dir := filepath.Join(stateDir, "backup", "spool")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", Stamp{}, err
	}
	// Spools left by an abrupt process death expire without touching sources.
	// Active attempts younger than the server's staging lifetime stay untouched.
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".snapshot-*"))
	for _, leftover := range leftovers {
		if info, err := os.Stat(leftover); err == nil && time.Since(info.ModTime()) > 7*24*time.Hour {
			_ = os.Remove(leftover)
		}
	}
	target, err := os.CreateTemp(dir, ".snapshot-*")
	if err != nil {
		return "", Stamp{}, err
	}
	name := target.Name()
	keep := false
	defer func() {
		target.Close()
		if !keep {
			os.Remove(name)
		}
	}()
	hash := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", Stamp{}, err
		}
		n, readErr := source.Read(buf)
		if n > 0 {
			if _, err := target.Write(buf[:n]); err != nil {
				return "", Stamp{}, err
			}
			hash.Write(buf[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", Stamp{}, readErr
		}
	}
	after, err := source.Stat()
	if err != nil {
		return "", Stamp{}, err
	}
	current, err := os.Stat(path)
	if err != nil {
		return "", Stamp{}, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !os.SameFile(before, current) || !after.ModTime().Equal(current.ModTime()) {
		return "", Stamp{}, fmt.Errorf("backup: source changed while preparing its snapshot; retry after it finishes saving")
	}
	if err := target.Close(); err != nil {
		return "", Stamp{}, err
	}
	keep = true
	return name, Stamp{Identity: fileIdentity(before), Size: before.Size(), ModUnix: before.ModTime().Unix(), ModNano: before.ModTime().UnixNano(), SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}
