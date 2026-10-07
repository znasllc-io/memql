// Package ociregistry verifies receipt-bound OCI images and publishes their exact
// digest to an operator-configured repository. It does not select releases,
// approve candidates, create tags, or register a public DSL capability.
package ociregistry

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
)

const (
	maxArchive    = int64(2 << 30)
	maxUnpacked   = int64(8 << 30)
	maxJSON       = int64(4 << 20)
	maxEntries    = 8192
	indexMedia    = "application/vnd.oci.image.index.v1+json"
	manifestMedia = "application/vnd.oci.image.manifest.v1+json"
	configMedia   = "application/vnd.oci.image.config.v1+json"
	layerMedia    = "application/vnd.oci.image.layer.v1.tar"
)

var shaPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Expected comes from an already-authorized immutable artifact receipt and
// reviewed candidate. All fields are mandatory; SHA256 includes "sha256:".
type Expected struct {
	ArchiveSHA256 string
	ArchiveSize   int64
	ImageDigest   string
	Platform      string
}

// Limits may reduce the native ceilings. Zero selects the documented ceiling.
type Limits struct{ ArchiveBytes, UnpackedBytes int64 }

// VerifiedImage owns a private snapshot. Only Verify can construct a usable
// handle. Close removes it and waits for any in-flight publication to finish.
type VerifiedImage struct {
	mu       sync.Mutex
	dir      string
	expected Expected
	image    v1.Image
	blobs    []blob
}

type blob struct {
	Digest string
	Size   int64
}

func (v *VerifiedImage) Close() error {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	err := os.RemoveAll(v.dir)
	v.dir, v.image, v.blobs = "", nil, nil
	return err
}

// Verify snapshots the entire bounded stream, validates its exact receipt and
// all referenced compressed/uncompressed content, and performs no network I/O.
// The caller owns the reader and must make blocked reads respect ctx.
func Verify(ctx context.Context, source io.Reader, want Expected, limits Limits) (_ *VerifiedImage, err error) {
	if source == nil || !shaPattern.MatchString(want.ArchiveSHA256) || !shaPattern.MatchString(want.ImageDigest) || want.ArchiveSize <= 0 || (want.Platform != "linux/amd64" && want.Platform != "linux/arm64") {
		return nil, errors.New("exact archive receipt, image digest and supported platform are required")
	}
	if limits.ArchiveBytes == 0 {
		limits.ArchiveBytes = maxArchive
	}
	if limits.UnpackedBytes == 0 {
		limits.UnpackedBytes = maxUnpacked
	}
	if limits.ArchiveBytes <= 0 || limits.ArchiveBytes > maxArchive || limits.UnpackedBytes <= 0 || limits.UnpackedBytes > maxUnpacked || want.ArchiveSize > limits.ArchiveBytes {
		return nil, errors.New("OCI limits exceed native ceilings or receipt exceeds limit")
	}
	dir, err := os.MkdirTemp("", "memql-verified-oci-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	raw, err := os.OpenFile(filepath.Join(dir, "archive"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	digest, size, err := hashCopy(ctx, raw, source, want.ArchiveSize)
	if err != nil {
		return nil, fmt.Errorf("snapshot archive: %w", err)
	}
	if digest != want.ArchiveSHA256 || size != want.ArchiveSize {
		return nil, errors.New("archive differs from immutable receipt")
	}
	if err = checkHeaders(ctx, raw, size); err != nil {
		return nil, err
	}
	if _, err = raw.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	root := filepath.Join(dir, "layout")
	if err = os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0700); err != nil {
		return nil, err
	}
	sizes := map[string]int64{}
	seen := map[string]bool{}
	tr := tar.NewReader(raw)
	for {
		h, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, nextErr
		}
		name := h.Name
		if h.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if seen[name] {
			return nil, errors.New("duplicate OCI archive path")
		}
		seen[name] = true
		if h.Typeflag == tar.TypeDir {
			if (name != "blobs" && name != "blobs/sha256") || h.Size != 0 {
				return nil, errors.New("unsupported OCI directory")
			}
			continue
		}
		isBlob := strings.HasPrefix(name, "blobs/sha256/") && shaPattern.MatchString("sha256:"+strings.TrimPrefix(name, "blobs/sha256/"))
		if h.Typeflag != tar.TypeReg || (name != "index.json" && name != "oci-layout" && !isBlob) || h.Size < 0 || h.Size > limits.ArchiveBytes {
			return nil, errors.New("unsupported OCI archive entry")
		}
		f, createErr := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if createErr != nil {
			return nil, createErr
		}
		d, n, copyErr := hashCopy(ctx, f, tr, h.Size)
		closeErr := f.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if n != h.Size || (isBlob && d != "sha256:"+strings.TrimPrefix(name, "blobs/sha256/")) {
			return nil, errors.New("OCI blob differs from its digest name")
		}
		sizes[name] = n
	}
	files := metadataFiles{root: root, sizes: sizes}
	blobs, err := files.verify(ctx, want, limits.UnpackedBytes)
	if err != nil {
		return nil, err
	}
	p, err := layout.FromPath(root)
	if err != nil {
		return nil, err
	}
	hash, err := v1.NewHash(want.ImageDigest)
	if err != nil {
		return nil, err
	}
	img, err := p.Image(hash)
	if err != nil {
		return nil, err
	}
	// The raw transport is no longer needed; only verified, private regular files
	// back the opaque handle. No caller path is reopened during publication.
	if err = raw.Close(); err != nil {
		return nil, err
	}
	if err = os.Remove(filepath.Join(dir, "archive")); err != nil {
		return nil, err
	}
	return &VerifiedImage{dir: dir, expected: want, image: img, blobs: blobs}, nil
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
	if limit < 0 {
		return "", 0, errors.New("content exceeds byte limit")
	}
	h := sha256.New()
	if dst == nil {
		dst = io.Discard
	}
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(contextReader{ctx, source}, limit+1))
	if err != nil {
		return "", n, err
	}
	if n > limit {
		return "", n, errors.New("content exceeds byte limit")
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, nil
}

// Reject extension headers BEFORE archive/tar can allocate their payloads.
func checkHeaders(ctx context.Context, raw *os.File, size int64) error {
	var header [512]byte
	for offset, entries := int64(0), 0; offset < size; entries++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := raw.ReadAt(header[:], offset); err != nil {
			return err
		}
		if bytes.Equal(header[:], make([]byte, 512)) {
			if size-offset < 1024 {
				return errors.New("missing tar end marker")
			}
			_, err := io.Copy(zeroWriter{}, contextReader{ctx, io.NewSectionReader(raw, offset, size-offset)})
			return err
		}
		if entries >= maxEntries || (header[156] != '0' && header[156] != 0 && header[156] != '5') {
			return errors.New("unsupported tar extension or entry count")
		}
		field := strings.Trim(string(header[124:136]), "\x00 ")
		if field == "" {
			field = "0"
		}
		for _, c := range field {
			if c < '0' || c > '7' {
				return errors.New("unsupported tar size encoding")
			}
		}
		n, err := strconv.ParseInt(field, 8, 64)
		if err != nil || n > size {
			return errors.New("invalid tar entry size")
		}
		offset += 512 + ((n+511)/512)*512
		if offset > size {
			return errors.New("truncated tar entry")
		}
	}
	return errors.New("missing tar end marker")
}

type zeroWriter struct{}

func (zeroWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != 0 {
			return 0, errors.New("nonzero bytes after tar end marker")
		}
	}
	return len(p), nil
}

// Validate keys before ordinary typed JSON decoding; duplicate fields must not
// acquire different meanings in the verifier and registry library.
func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 128 {
		return errors.New("JSON nesting exceeds limit")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := k.(string)
			if !ok || seen[strings.ToLower(key)] {
				return errors.New("duplicate JSON field")
			}
			seen[strings.ToLower(key)] = true
			for _, reserved := range []string{"mediaType", "schemaVersion", "digest", "size", "urls", "data", "subject", "artifactType", "platform", "os", "architecture", "variant", "os.features", "os.version", "manifests", "config", "layers", "rootfs", "type", "diff_ids", "imageLayoutVersion"} {
				if strings.EqualFold(key, reserved) && key != reserved {
					return errors.New("noncanonical OCI metadata field")
				}
			}
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}
