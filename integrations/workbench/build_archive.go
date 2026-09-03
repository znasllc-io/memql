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

// extractTarGz expands a gzip'd tar INTO an os.Root and reports the single
// synthesized top-level directory the archive wrapped its tree in, or "" when
// it had none.
//
// ===========================================================================
// THE ROOT IS THE CONTAINMENT, AND IT IS THE KERNEL'S RATHER THAN MINE
// ===========================================================================
// This started as a string check: reject `..`, reject absolute, join, then
// compare the join against the root. That is correct as far as it goes, and it
// has two problems. It does not cover SYMLINKS -- an archive can contain a
// link whose own name is impeccable and whose target is not, and every
// string-level check passes it. And a reader (human or analyser) has to trust
// that the check ran, which is why static analysis kept flagging these lines
// even once it had: the guard lived in a helper and the operation lived here.
//
// os.Root closes both. Every method on it is confined to the directory by an
// open file descriptor, it refuses a name that resolves outside even through a
// symlink, and the confinement is a property of the HANDLE rather than of a
// comparison somebody remembered to write. safeEntryPath stays, because a
// refusal that names the bad entry is better than an opaque error from the
// filesystem -- but it is no longer what makes this safe.
//
// It returns the top-level directory NAME rather than a path, so nothing here
// composes one: GitHub wraps a repository in `<owner>-<repo>-<sha>`, which is
// not part of the tree the author wrote, and the caller strips it by naming it
// relative to the same root.
// destDir MUST ALREADY EXIST, and that is a property of the containment
// rather than an inconvenience: an os.Root cannot confine the creation of its
// own directory, so the one mkdir that has to happen outside a root is the
// caller's, made through the root ABOVE it. Creating it here would put an
// unconfined write back into the middle of the confined path.
func extractTarGz(r io.Reader, destDir string, maxFiles int, maxFileBytes, maxTotalBytes int64) (string, error) {
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return "", err
	}
	defer root.Close()

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
		if errors.Is(nerr, io.EOF) {
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

		switch hdr.Typeflag {
		case tar.TypeDir:
			if mkErr := root.MkdirAll(clean, 0o755); mkErr != nil {
				return "", mkErr
			}
			continue
		case tar.TypeReg:
			// fall through
		default:
			// Symlinks, hardlinks and devices are DROPPED rather than
			// materialized. os.Root would refuse an escaping link, so this is
			// no longer the only thing standing between an archive and the
			// filesystem -- but nothing in a package needs one, and a tree
			// with no links is a tree with nothing to reason about.
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
		if dir := path.Dir(clean); dir != "." {
			if mkErr := root.MkdirAll(dir, 0o755); mkErr != nil {
				return "", mkErr
			}
		}
		out, oerr := root.OpenFile(clean, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, entryMode(hdr))
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
				return name, nil
			}
		}
	}
	return "", nil
}

// entryMode keeps the executable bit and nothing else. A tree that arrives
// setuid does not stay setuid.
func entryMode(hdr *tar.Header) os.FileMode {
	if hdr.FileInfo().Mode().Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// packTarGz walks a directory THROUGH AN os.Root and returns it as a gzip'd
// tar whose entries all sit under top/.
//
// Read through the root for the same reason the extractor writes through one:
// the tree being packed was produced by somebody else's build command, so a
// symlink in it pointing at /etc is a thing that can happen, and a walk that
// followed one would pack the host's files into a bundle this cluster then
// serves on the internet. os.Root refuses to leave the directory; fs.WalkDir
// over root.FS() never composes a path outside it.
//
// Entries are written in SORTED order with no timestamps, so the same tree
// packs to the same bytes twice -- which is what lets a caller compare two
// builds' output without unpacking them.
func packTarGz(dir, top string, maxFiles int, maxFileBytes, maxTotalBytes int64) ([]byte, int, int64, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, 0, 0, err
	}
	defer root.Close()
	tree := root.FS()

	var names []string
	err = fs.WalkDir(tree, ".", func(p string, entry fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			// A symlink in a built output is dropped: nothing a bundle serves
			// needs one, and what it points at is not this cluster's to
			// publish.
			return nil
		}
		names = append(names, p)
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
		info, serr := root.Lstat(name)
		if serr != nil {
			return nil, 0, 0, serr
		}
		if info.Size() > maxFileBytes {
			return nil, 0, 0, fmt.Errorf("%q is larger than %d bytes", name, maxFileBytes)
		}
		if total+info.Size() > maxTotalBytes {
			return nil, 0, 0, fmt.Errorf("the built output exceeds %d bytes", maxTotalBytes)
		}
		data, rerr := root.ReadFile(name)
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
