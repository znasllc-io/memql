package packages

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// The operator knobs bounding a package source.
//
// Named as constants AND read through them, rather than composed from a
// prefix, so a plain grep finds the read -- the lesson component/server's
// env.go records about the MEMQL_SERVER_* family, where a composed spelling
// made the variables look dead to every grep and to envscan.
const (
	MaxSourceBytesEnv = "MEMQL_PACKAGES_MAX_SOURCE_BYTES"
	MaxFileBytesEnv   = "MEMQL_PACKAGES_MAX_FILE_BYTES"
	MaxFileCountEnv   = "MEMQL_PACKAGES_MAX_FILE_COUNT"
	// BuildTimeoutEnv bounds ONE deployable's build command (epic memql#4900).
	// A timeout rather than a total: a package of three apps is three
	// independent builds, and a shared budget would make the third one's
	// success depend on how slow the first two were.
	BuildTimeoutEnv = "MEMQL_PACKAGES_BUILD_TIMEOUT_SECONDS"
)

// DefaultBuildTimeoutSeconds is fifteen minutes: generous for `npm ci && npm
// run build` on a cold cache, and short enough that a wedged build does not
// hold a deployment open for an hour.
const DefaultBuildTimeoutSeconds = 900

// BuildTimeoutSeconds resolves the per-deployable build timeout.
//
// Same fallback posture as the caps above -- set-but-unparseable or
// non-positive falls back to the DEFAULT rather than to "no timeout" -- and
// for a sharper reason: an unbounded build is a workbench replica held by
// somebody else's script forever.
func BuildTimeoutSeconds() int {
	// CLAMPED, not narrowed. envInt64 answers an int64, and converting one
	// straight to `int` is the narrowing core/num exists to make a named
	// decision -- and here the answer is neither saturate nor zero but a
	// CEILING, because the value bounds how long a workbench replica may be
	// held by somebody else's script. MaxBuildTimeoutSeconds is the same two
	// hours the build surface clamps to, so the two agree.
	seconds := envInt64(BuildTimeoutEnv, DefaultBuildTimeoutSeconds)
	if seconds > MaxBuildTimeoutSeconds {
		return MaxBuildTimeoutSeconds
	}
	return int(seconds)
}

// MaxBuildTimeoutSeconds is the ceiling an operator may ask for, mirroring
// the build surface's own (workbench.MaxBuildTimeout). Two hours.
const MaxBuildTimeoutSeconds = 2 * 60 * 60

// The publisher-grade defaults (D1). They are sitePublishFromArtifact's
// numbers deliberately: a package source is an archive of roughly the same
// kind, arriving over the same kind of link, and two different answers to
// "how big may an uploaded tree be" is a difference somebody would have to
// discover by hitting one of them.
const (
	DefaultMaxSourceBytes int64 = 500 * 1024 * 1024 // 500 MB expanded
	DefaultMaxFileBytes   int64 = 25 * 1024 * 1024  // 25 MB per file
	DefaultMaxFileCount   int   = 20000
)

// Limits bounds an expanded source tree.
type Limits struct {
	MaxSourceBytes int64
	MaxFileBytes   int64
	MaxFileCount   int
}

// DefaultLimits resolves the limits from the environment.
//
// A set-but-unparseable or non-positive value falls back to the DEFAULT rather
// than to "no limit". An unbounded expansion is the one outcome a misconfigured
// cap must never produce -- the same reasoning, and the same direction, as
// LibraryMaxUploadBytes.
func DefaultLimits() Limits {
	return Limits{
		MaxSourceBytes: envInt64(MaxSourceBytesEnv, DefaultMaxSourceBytes),
		MaxFileBytes:   envInt64(MaxFileBytesEnv, DefaultMaxFileBytes),
		MaxFileCount:   int(envInt64(MaxFileCountEnv, int64(DefaultMaxFileCount))),
	}
}

func envInt64(key string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// normalized returns l with any zero field replaced by its default, so a
// caller passing Limits{} gets the documented behaviour rather than a tree
// that refuses its own first file.
func (l Limits) normalized() Limits {
	if l.MaxSourceBytes <= 0 {
		l.MaxSourceBytes = DefaultMaxSourceBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultMaxFileBytes
	}
	if l.MaxFileCount <= 0 {
		l.MaxFileCount = DefaultMaxFileCount
	}
	return l
}

// safeArchivePath validates one archive entry name and returns it in the
// slash-separated, root-relative form fs.FS wants.
//
// A zip.Reader used as an fs.FS already DROPS an entry whose name is not a
// valid fs path -- which is safe and silent, and silence is the problem. A
// package whose archive carries `../../etc/cron.d/x` would analyze cleanly
// against the entries that remained, and the person who built it would never
// learn their archive was malformed. So every entry is checked explicitly and
// a bad one REFUSES the whole source.
func safeArchivePath(name string) (string, error) {
	clean := strings.TrimSpace(name)
	// Windows-built archives use backslashes; a path that means "escape" in
	// one separator convention must not read as an ordinary name in the other.
	clean = strings.ReplaceAll(clean, `\`, "/")
	if clean == "" {
		return "", refuse(CodeBundlePathInvalid, "the archive holds an entry with an empty name")
	}
	if strings.HasPrefix(clean, "/") {
		return "", refuse(CodeBundlePathInvalid,
			"the archive holds an absolute path (%q). Every entry must be relative to the root of the package.", name)
	}
	if strings.Contains(clean, "\x00") {
		return "", refuse(CodeBundlePathInvalid, "the archive holds an entry with a NUL byte in its name")
	}
	clean = strings.TrimSuffix(clean, "/")
	if clean == "" {
		// The root directory entry itself; harmless and not a file.
		return "", nil
	}
	cleaned := path.Clean(clean)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", refuse(CodeBundlePathInvalid,
			"the archive holds an entry that escapes the package root (%q). This source was not built from a plain tree.", name)
	}
	if !fs.ValidPath(cleaned) {
		return "", refuse(CodeBundlePathInvalid, "the archive holds an entry this cluster will not read (%q).", name)
	}
	return cleaned, nil
}

// OpenZip validates a zip source and returns it as an fs.FS over the tree.
//
// Nothing is expanded to disk: analysis reads through the returned FS, which
// is what makes re-analysing an already-stored snapshot cheap (design section
// I -- "an analysis pass is repeatable from the stored snapshot without
// refetching"). The build stage, which genuinely needs files, extracts
// separately.
func OpenZip(ra io.ReaderAt, size int64, limits Limits) (fs.FS, error) {
	limits = limits.normalized()

	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, refuse(CodeSourceUnreadable,
			"this source is not a zip archive this cluster can open: %v", err)
	}

	var (
		files int
		total int64
	)
	for _, f := range zr.File {
		clean, perr := safeArchivePath(f.Name)
		if perr != nil {
			return nil, perr
		}
		if clean == "" || strings.HasSuffix(f.Name, "/") {
			continue // a directory entry
		}
		files++
		if files > limits.MaxFileCount {
			return nil, refuse(CodeSourceTooLarge,
				"this source holds more than %d files. Trim it, or raise %s.",
				limits.MaxFileCount, MaxFileCountEnv)
		}
		// The DECLARED uncompressed size, checked before a byte is read, so a
		// zip bomb is refused rather than materialized. The publisher checks
		// the same number for the same reason.
		if int64(f.UncompressedSize64) > limits.MaxFileBytes {
			return nil, refuse(CodeSourceTooLarge,
				"%q expands to more than %d bytes. Trim it, or raise %s.",
				clean, limits.MaxFileBytes, MaxFileBytesEnv)
		}
		total += int64(f.UncompressedSize64)
		if total > limits.MaxSourceBytes {
			return nil, refuse(CodeSourceTooLarge,
				"this source expands to more than %d bytes. Trim it, or raise %s.",
				limits.MaxSourceBytes, MaxSourceBytesEnv)
		}
	}

	return zr, nil
}

// ExtractTarGz expands a gzip'd tar -- what the GitHub tarball API returns --
// into destDir under the same limits, and reports the directory holding the
// package tree.
//
// GitHub wraps the whole repository in ONE synthesized top-level directory
// named `<owner>-<repo>-<sha>`, which is not part of the tree the author
// wrote. Stripping it is what makes D1's "a zip of exactly the same tree"
// literally true: after this, the repo form and the zip form are the same FS
// with the manifest at the root, and every rule below applies once.
//
// The strip is CONDITIONAL on there being exactly one top-level directory and
// no top-level files. A tarball that is already root-relative is left alone
// rather than having its first directory eaten.
func ExtractTarGz(r io.Reader, destDir string, limits Limits) (string, error) {
	limits = limits.normalized()

	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", refuse(CodeSourceUnreadable,
			"this source is not a gzip archive this cluster can open: %v", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var (
		files    int
		total    int64
		topLevel = map[string]bool{} // name -> isDir
	)
	for {
		hdr, nerr := tr.Next()
		if nerr == io.EOF {
			break
		}
		if nerr != nil {
			return "", refuse(CodeSourceUnreadable, "this source could not be read: %v", nerr)
		}

		clean, perr := safeArchivePath(hdr.Name)
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

		target := filepath.Join(destDir, filepath.FromSlash(clean))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if mkErr := os.MkdirAll(target, 0o755); mkErr != nil {
				return "", mkErr
			}
			continue
		case tar.TypeReg:
			// fall through
		default:
			// Symlinks, hardlinks, devices and the rest are DROPPED, never
			// materialized. A symlink is the traversal check's blind spot:
			// its own name is a clean relative path while its target is
			// arbitrary, so honouring one would reintroduce exactly the
			// escape safeArchivePath refuses. Nothing in a package needs one.
			continue
		}

		files++
		if files > limits.MaxFileCount {
			return "", refuse(CodeSourceTooLarge,
				"this source holds more than %d files. Trim it, or raise %s.",
				limits.MaxFileCount, MaxFileCountEnv)
		}
		if hdr.Size > limits.MaxFileBytes {
			return "", refuse(CodeSourceTooLarge,
				"%q is larger than %d bytes. Trim it, or raise %s.",
				clean, limits.MaxFileBytes, MaxFileBytesEnv)
		}
		if total+hdr.Size > limits.MaxSourceBytes {
			return "", refuse(CodeSourceTooLarge,
				"this source expands to more than %d bytes. Trim it, or raise %s.",
				limits.MaxSourceBytes, MaxSourceBytesEnv)
		}

		if mkErr := os.MkdirAll(filepath.Dir(target), 0o755); mkErr != nil {
			return "", mkErr
		}
		out, oerr := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if oerr != nil {
			return "", oerr
		}
		// LimitReader as well as the header check: the header's Size is the
		// archive's claim, and a lying header must not be able to write past
		// the cap. The publisher guards both ends for the same reason.
		written, cerr := io.Copy(out, io.LimitReader(tr, limits.MaxFileBytes+1))
		closeErr := out.Close()
		if cerr != nil {
			return "", cerr
		}
		if closeErr != nil {
			return "", closeErr
		}
		if written > limits.MaxFileBytes {
			return "", refuse(CodeSourceTooLarge,
				"%q is larger than %d bytes. Trim it, or raise %s.",
				clean, limits.MaxFileBytes, MaxFileBytesEnv)
		}
		total += written
		if total > limits.MaxSourceBytes {
			return "", refuse(CodeSourceTooLarge,
				"this source expands to more than %d bytes. Trim it, or raise %s.",
				limits.MaxSourceBytes, MaxSourceBytesEnv)
		}
	}

	if len(topLevel) == 1 {
		for name, isDir := range topLevel {
			if isDir {
				return filepath.Join(destDir, filepath.FromSlash(name)), nil
			}
		}
	}
	return destDir, nil
}
