package workbench

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// build_archive.go is the tar half of the build entry: the source in, the
// output out, under the same bounds in both directions.
//
// A SECOND COPY of shapes component/packages already has (ExtractTarGz,
// bundleFromTree), and deliberately: component/packages is in the root module
// and imports component/edge, so importing it from integrations/workbench
// would make the module graph a cycle -- the exact reason component/sitepublish
// and component/packages live where they do (memql#4345). What crosses the
// boundary instead is a tarball and a JSON envelope, which is a contract two
// packages can share without a shared type.
//
// The bounds are the point rather than the parsing: every entry is checked
// against the file cap before a byte is read AND against what was actually
// read, and the running total is checked at both, so a lying header and a zip
// bomb are refused rather than materialized.

// safeEntryPath validates one archive entry name as a relative, non-escaping
// slash path, or answers "" for a directory entry that names nothing.
func safeEntryPath(name string) (string, error) {
	clean := strings.ReplaceAll(strings.TrimSpace(name), `\`, "/")
	if clean == "" {
		return "", errors.New("the archive holds an entry with an empty name")
	}
	if strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("the archive holds an absolute path (%q)", name)
	}
	if strings.Contains(clean, "\x00") {
		return "", errors.New("the archive holds an entry with a NUL byte in its name")
	}
	clean = strings.TrimSuffix(clean, "/")
	if clean == "" {
		return "", nil
	}
	cleaned := path.Clean(clean)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || !fs.ValidPath(cleaned) {
		return "", fmt.Errorf("the archive holds an entry that escapes the root (%q)", name)
	}
	return cleaned, nil
}

// containedJoin joins one relative path under root and REFUSES anything whose
// result is not inside it.
//
// The check is on the JOINED path rather than on the entry name, and that is
// the difference between a validator a reader believes and one a machine can
// verify. safeEntryPath already rejects `..`, absolute paths and NUL, so this
// is defence in depth -- but it is the check that actually closes Zip Slip,
// because it constrains the thing the filesystem call receives rather than the
// string it was derived from. It is also the shape static analysis recognises,
// which matters: a sanitiser nothing can see is one every future reader has to
// re-derive by hand.
func containedJoin(root, rel string) (string, error) {
	// AN ABSOLUTE INPUT IS REFUSED, not reinterpreted. filepath.Join would
	// treat "/etc/passwd" as relative and quietly produce "<root>/etc/passwd"
	// -- safe, in that nothing escapes, and dishonest: the caller named one
	// path and the filesystem would receive another. The callers all validate
	// for this already; refusing here is what makes the function true on its
	// own, which is the property a security primitive has to have.
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("the path %q is absolute", rel)
	}
	cleanRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	joined := filepath.Join(cleanRoot, filepath.FromSlash(rel))
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	if abs != cleanRoot && !strings.HasPrefix(abs, cleanRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("the path %q resolves outside %q", rel, root)
	}
	return abs, nil
}

// extractTarGz expands a gzip'd tar into destDir and reports the directory
// holding the tree -- stripping ONE synthesized top-level directory when the
// archive has exactly one and no top-level files, which is the shape GitHub's
// tarball API returns and the shape packTarGz writes.
func extractTarGz(r io.Reader, destDir string, maxFiles int, maxFileBytes, maxTotalBytes int64) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var (
		files    int
		total    int64
		topLevel = map[string]bool{}
	)
	for {
		hdr, nerr := tr.Next()
		if nerr == io.EOF {
			break
		}
		if nerr != nil {
			return "", nerr
		}
		clean, perr := safeEntryPath(hdr.Name)
		if perr != nil {
			return "", perr
		}
		if clean == "" {
			continue
		}
		first, _, nested := strings.Cut(clean, "/")
		if !nested {
			topLevel[first] = hdr.Typeflag == tar.TypeDir
		} else if _, seen := topLevel[first]; !seen {
			topLevel[first] = true
		}

		target, terr := containedJoin(destDir, clean)
		if terr != nil {
			return "", terr
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if mkErr := os.MkdirAll(target, 0o755); mkErr != nil {
				return "", mkErr
			}
			continue
		case tar.TypeReg:
			// fall through
		default:
			// Symlinks, hardlinks and devices are DROPPED rather than
			// materialized, exactly as component/packages drops them: a
			// symlink's own name is a clean relative path while its target is
			// arbitrary, which is the traversal check's blind spot.
			continue
		}

		files++
		if files > maxFiles {
			return "", fmt.Errorf("the archive holds more than %d files", maxFiles)
		}
		if hdr.Size > maxFileBytes {
			return "", fmt.Errorf("%q is larger than %d bytes", clean, maxFileBytes)
		}
		if total+hdr.Size > maxTotalBytes {
			return "", fmt.Errorf("the archive expands to more than %d bytes", maxTotalBytes)
		}
		if mkErr := os.MkdirAll(filepath.Dir(target), 0o755); mkErr != nil {
			return "", mkErr
		}
		out, oerr := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, entryMode(hdr))
		if oerr != nil {
			return "", oerr
		}
		written, cerr := io.Copy(out, io.LimitReader(tr, maxFileBytes+1))
		closeErr := out.Close()
		if cerr != nil {
			return "", cerr
		}
		if closeErr != nil {
			return "", closeErr
		}
		if written > maxFileBytes {
			return "", fmt.Errorf("%q is larger than %d bytes", clean, maxFileBytes)
		}
		total += written
		if total > maxTotalBytes {
			return "", fmt.Errorf("the archive expands to more than %d bytes", maxTotalBytes)
		}
	}

	if len(topLevel) == 1 {
		for name, isDir := range topLevel {
			if isDir {
				return containedJoin(destDir, name)
			}
		}
	}
	return destDir, nil
}

// entryMode keeps the executable bit and nothing else. A tree that arrives
// setuid does not stay setuid.
func entryMode(hdr *tar.Header) os.FileMode {
	if hdr.FileInfo().Mode().Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// packTarGz walks root and returns it as a gzip'd tar whose entries all sit
// under top/, the shape extractTarGz strips on the way back.
//
// Entries are written in SORTED order, so the same tree packs to the same
// bytes twice -- which is what lets a caller compare two builds' output
// without unpacking them.
func packTarGz(root, top string, maxFiles int, maxFileBytes, maxTotalBytes int64) ([]byte, int, int64, error) {
	var names []string
	err := filepath.WalkDir(root, func(p string, entry os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			// A symlink in a built output is dropped for the reason the
			// extractor drops one: nothing a bundle serves needs it, and its
			// target is not checkable from here.
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		names = append(names, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, 0, 0, err
	}
	sort.Strings(names)
	if len(names) > maxFiles {
		return nil, 0, 0, fmt.Errorf("the built output holds more than %d files", maxFiles)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	var total int64
	for _, name := range names {
		full, jerr := containedJoin(root, name)
		if jerr != nil {
			return nil, 0, 0, jerr
		}
		info, serr := os.Lstat(full)
		if serr != nil {
			return nil, 0, 0, serr
		}
		if info.Size() > maxFileBytes {
			return nil, 0, 0, fmt.Errorf("%q is larger than %d bytes", name, maxFileBytes)
		}
		if total+info.Size() > maxTotalBytes {
			return nil, 0, 0, fmt.Errorf("the built output exceeds %d bytes", maxTotalBytes)
		}
		data, rerr := os.ReadFile(full)
		if rerr != nil {
			return nil, 0, 0, rerr
		}
		mode := int64(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		// NO TIMESTAMPS. A tarball carrying mtimes hashes differently on two
		// builds of the same tree, which would make an unchanged output look
		// like a new one to anything that content-addresses it.
		if werr := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     path.Join(top, name),
			Mode:     mode,
			Size:     int64(len(data)),
			Format:   tar.FormatPAX,
		}); werr != nil {
			return nil, 0, 0, werr
		}
		if _, werr := tw.Write(data); werr != nil {
			return nil, 0, 0, werr
		}
		total += int64(len(data))
	}
	if cerr := tw.Close(); cerr != nil {
		return nil, 0, 0, cerr
	}
	if cerr := gz.Close(); cerr != nil {
		return nil, 0, 0, cerr
	}
	return buf.Bytes(), len(names), total, nil
}
