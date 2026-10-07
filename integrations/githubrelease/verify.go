// Package githubrelease verifies receipt-bound files and publishes one exact
// asset to an operator-selected existing GitHub draft release. Release choice,
// creation, approval, promotion and recovery policy belong to the DSL caller.
package githubrelease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// GitHub requires each release asset to be strictly smaller than 2 GiB.
const maxFileBytes = int64(2<<30) - 1

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Expected is an already-authorized immutable artifact receipt. SHA256 includes
// the "sha256:" prefix. Empty files are supported, with their exact digest.
type Expected struct {
	SHA256 string
	Size   int64
}

// Limits may reduce the native ceiling. Zero selects the ceiling.
type Limits struct{ FileBytes int64 }

// VerifiedFile owns a private snapshot. Only Verify constructs a usable handle;
// Close waits for any publication and removes the snapshot. No caller path is
// reopened while publishing. The caller retains the durable source separately.
type VerifiedFile struct {
	mu       sync.Mutex
	dir      string
	expected Expected
}

func (v *VerifiedFile) Close() error {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dir == "" {
		return nil
	}
	if err := os.RemoveAll(v.dir); err != nil {
		return err
	}
	v.dir = ""
	return nil
}

// Verify snapshots all bytes through EOF without network activity. The caller
// owns source and must make any blocked Read respect ctx.
func Verify(ctx context.Context, source io.Reader, want Expected, limits Limits) (_ *VerifiedFile, err error) {
	if source == nil || !digestPattern.MatchString(want.SHA256) || want.Size < 0 {
		return nil, errors.New("exact asset checksum and size are required")
	}
	if limits.FileBytes == 0 {
		limits.FileBytes = maxFileBytes
	}
	if limits.FileBytes < 0 || limits.FileBytes > maxFileBytes || want.Size > limits.FileBytes {
		return nil, errors.New("asset exceeds native or caller byte limit")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "memql-verified-release-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	f, err := os.OpenFile(filepath.Join(dir, "asset"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	digest, size, copyErr := hashCopy(ctx, f, source, want.Size)
	closeErr := f.Close()
	if err = errors.Join(copyErr, closeErr); err != nil {
		return nil, err
	}
	if digest != want.SHA256 || size != want.Size {
		return nil, errors.New("asset differs from immutable receipt")
	}
	return &VerifiedFile{dir: dir, expected: want}, nil
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}

func hashCopy(ctx context.Context, dst io.Writer, source io.Reader, limit int64) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(contextReader{ctx, source}, limit+1))
	if err != nil {
		return "", n, err
	}
	if n > limit {
		return "", n, errors.New("asset exceeds expected byte length")
	}
	if err := ctx.Err(); err != nil {
		return "", n, err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, nil
}
