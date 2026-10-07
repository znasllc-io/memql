package pipelinesteps

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ArtifactSnapshot is a private, bounded local snapshot of a completed
// artifact stream. Close removes only its own scratch directory. No untrusted
// path becomes a filesystem path. A complete snapshot must be verified and
// durably filed before its producing Job may be acknowledged and cleaned up.
type ArtifactSnapshot struct {
	Files     []SnapshotFile
	directory string
}

type SnapshotFile struct {
	Path   string
	Size   int64
	SHA256 string
	local  string
}

func (f SnapshotFile) Open() (*os.File, error) { return os.Open(f.local) }
func (s *ArtifactSnapshot) Close() error       { return os.RemoveAll(s.directory) }

// SnapshotArtifacts consumes an uncompressed tar stream, including its end
// and the upstream transport's final error. It admits only declared regular
// files and ordinary directories, refuses duplicate names, links, traversal,
// excessive entries/bytes and nonzero trailing data, and requires every
// declared path to match a file. No partial snapshot escapes on failure.
//
// The entire wire stream is bounded at maxBytes, at most 2 GiB including tar
// overhead. Memory is bounded independently of file size. The source must
// honor ctx when blocked; a pipe's producer is responsible for closing it
// with the transport error, even after the tar end marker was received.
func SnapshotArtifacts(ctx context.Context, source io.Reader, declared []string, maxBytes int64) (snapshot *ArtifactSnapshot, err error) {
	if source == nil || maxBytes <= 0 || maxBytes > 2<<30 || len(declared) == 0 || len(declared) > extractMaxFiles {
		return nil, errors.New("artifact snapshot requires declared paths and a positive bound through 2 GiB")
	}
	for _, pattern := range declared {
		if problem := artifactProblem(pattern); problem != "" {
			return nil, fmt.Errorf("artifact path %q: %s", pattern, problem)
		}
	}
	directory, err := os.MkdirTemp("", "memql-artifacts-")
	if err != nil {
		return nil, err
	}
	result := &ArtifactSnapshot{directory: directory}
	defer func() {
		if err != nil {
			_ = result.Close()
		}
	}()
	input := &artifactSnapshotReader{ctx: ctx, source: source, remaining: maxBytes}
	reader := tar.NewReader(input)
	patterns := extractPatterns(declared)
	seen := map[string]bool{}
	matched := make([]bool, len(patterns))
	buffer := make([]byte, 64<<10)
	for entries := 0; ; entries++ {
		before := input.remaining
		header, readErr := reader.Next()
		if errors.Is(readErr, io.EOF) {
			if before-input.remaining < 1024 || input.trailingZeros < 1024 {
				return nil, errors.New("artifact archive lacks its complete end marker")
			}
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("artifact archive: %w", readErr)
		}
		if entries >= extractMaxEntries {
			return nil, ErrArtifactsTooLarge
		}
		name, reason := extractEntryName(header.Name)
		if reason != "" || len(name) > 4096 {
			return nil, errors.New("artifact archive contains an unsafe path")
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg || len(header.PAXRecords) != 0 || len(header.Xattrs) != 0 {
			return nil, errors.New("artifact snapshot requires plain regular files without extended metadata")
		}
		if seen[name] {
			return nil, errors.New("artifact archive contains a duplicate path")
		}
		seen[name] = true
		if !extractDeclared(patterns, name) {
			return nil, errors.New("artifact archive contains an undeclared file")
		}
		if len(result.Files) >= extractMaxFiles || header.Size < 0 || header.Size > input.remaining {
			return nil, ErrArtifactsTooLarge
		}
		local := filepath.Join(directory, fmt.Sprintf("file-%04d", len(result.Files)))
		file, openErr := os.OpenFile(local, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if openErr != nil {
			return nil, openErr
		}
		hash := sha256.New()
		size, copyErr := io.CopyBuffer(io.MultiWriter(file, hash), reader, buffer)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return nil, errors.Join(copyErr, closeErr)
		}
		if size != header.Size {
			return nil, errors.New("artifact file was truncated")
		}
		result.Files = append(result.Files, SnapshotFile{Path: name, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil)), local: local})
		for i, pattern := range patterns {
			if pattern.matches(name) {
				matched[i] = true
			}
		}
	}
	// tar stops at its zero blocks. Drain the bounded input to distinguish a
	// complete transport from a lost remote exit status or appended payload.
	for {
		n, readErr := input.Read(buffer)
		for _, value := range buffer[:n] {
			if value != 0 {
				return nil, errors.New("artifact archive has trailing nonzero data")
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	for _, hit := range matched {
		if !hit {
			return nil, errors.New("a declared artifact path matched no regular file")
		}
	}
	slices.SortFunc(result.Files, func(a, b SnapshotFile) int { return strings.Compare(a.Path, b.Path) })
	return result, nil
}

type artifactSnapshotReader struct {
	ctx           context.Context
	source        io.Reader
	remaining     int64
	trailingZeros int64
}

func (r *artifactSnapshotReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.remaining == 0 {
		var extra [1]byte
		n, err := r.source.Read(extra[:])
		if n != 0 {
			return 0, ErrArtifactsTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.source.Read(p)
	r.remaining -= int64(n)
	for _, value := range p[:n] {
		if value == 0 {
			r.trailingZeros++
		} else {
			r.trailingZeros = 0
		}
	}
	return n, err
}
