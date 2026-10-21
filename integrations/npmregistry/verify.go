// Package npmregistry publishes an already-approved immutable npm archive.
// Selection, source qualification, approval and effect journals belong to its
// caller. Package scripts are never executed and credentials never enter builds.
package npmregistry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"strings"
)

const maxArchive = 64 << 20
const maxExpanded = 256 << 20
const maxMetadata = 1 << 20

var namePattern = regexp.MustCompile(`^(?:@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type Expected struct {
	Name, Version, SHA256 string
	Size                  int64
}

// VerifiedPackage owns a private bounded copy. No caller path or mutable byte
// slice is consulted during publication. Its zero value is unusable.
type VerifiedPackage struct {
	expected Expected
	archive  []byte
	manifest map[string]json.RawMessage
}

func validIdentity(name, version string) bool {
	if len(name) > 214 || len(version) > 128 || !namePattern.MatchString(name) || !versionPattern.MatchString(version) {
		return false
	}
	if _, pre, ok := strings.Cut(version, "-"); ok {
		for _, part := range strings.Split(pre, ".") {
			if len(part) > 1 && part[0] == '0' && strings.Trim(part, "0123456789") == "" {
				return false
			}
		}
	}
	return true
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// Verify checks the complete receipt and archive without network access. The
// caller must make a blocked source Read honor ctx. The declared package name
// and version must come from reviewed configuration, never the archive itself.
func Verify(ctx context.Context, source io.Reader, want Expected) (*VerifiedPackage, error) {
	if source == nil || !validIdentity(want.Name, want.Version) || !digestPattern.MatchString(want.SHA256) || want.Size <= 0 || want.Size > maxArchive {
		return nil, errors.New("npm publication requires a bounded archive and exact package identity and digest")
	}
	archive, err := io.ReadAll(io.LimitReader(contextReader{ctx, source}, want.Size+1))
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(archive)
	if int64(len(archive)) != want.Size || "sha256:"+hex.EncodeToString(digest[:]) != want.SHA256 {
		return nil, errors.New("npm archive differs from its immutable receipt")
	}
	z, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, errors.New("npm archive is not gzip")
	}
	defer z.Close()
	bounded := &io.LimitedReader{R: contextReader{ctx, z}, N: maxExpanded + 1}
	r := tar.NewReader(bounded)
	seen := map[string]bool{}
	var manifest map[string]json.RawMessage
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("npm archive is malformed")
		}
		name := strings.TrimSuffix(h.Name, "/")
		inside := strings.HasPrefix(name, "package/") || (name == "package" && h.Typeflag == tar.TypeDir)
		if len(seen) >= 100000 || seen[name] || path.Clean(name) != name || !inside || strings.ContainsAny(name, "\\\x00") || h.Size < 0 || h.Size > maxExpanded || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) {
			return nil, errors.New("npm archive has duplicate, unsafe or unsupported entries")
		}
		seen[name] = true
		if name == "package/package.json" {
			if h.Typeflag != tar.TypeReg || h.Size > maxMetadata {
				return nil, errors.New("npm manifest is invalid or oversized")
			}
			body, err := io.ReadAll(r)
			if err != nil {
				return nil, err
			}
			if err := uniqueJSON(body); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(body, &manifest); err != nil || manifest == nil {
				return nil, errors.New("npm manifest is not an object")
			}
		}
	}
	// Consume through gzip EOF to check its checksum and the decompression bound.
	if _, err := io.Copy(zeroPadding{}, bounded); err != nil || bounded.N <= 0 {
		return nil, errors.New("npm archive exceeds its expansion limit or has an invalid checksum")
	}
	var name, version string
	if json.Unmarshal(manifest["name"], &name) != nil || json.Unmarshal(manifest["version"], &version) != nil || name != want.Name || version != want.Version {
		return nil, errors.New("npm manifest differs from the approved package name or version")
	}
	if raw, ok := manifest["private"]; ok && string(raw) != "false" {
		return nil, errors.New("npm package is marked private")
	}
	if _, ok := manifest["packageExtensions"]; ok {
		return nil, errors.New("npm package contains root-only packageExtensions")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &VerifiedPackage{expected: want, archive: archive, manifest: manifest}, nil
}

type zeroPadding struct{}

func (zeroPadding) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != 0 {
			return 0, errors.New("npm archive has data after its tar terminator")
		}
	}
	return len(p), nil
}

func uniqueJSON(body []byte) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := uniqueValue(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("npm JSON contains trailing data")
	}
	return nil
}
func uniqueValue(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("npm JSON exceeds its depth limit")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				t, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := t.(string)
				if !ok || seen[key] {
					return errors.New("npm JSON contains duplicate keys")
				}
				seen[key] = true
				if err := uniqueValue(d, depth+1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := uniqueValue(d, depth+1); err != nil {
					return err
				}
			}
		default:
			return errors.New("npm JSON contains an unexpected delimiter")
		}
		_, err = d.Token()
		return err
	}
	return nil
}
