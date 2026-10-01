package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// tarball renders files as a gzipped tar that is the same bytes on every
// build of the same commit: entries sorted, names `./`-prefixed (the shape
// `tar -C dir .` produced, which the verify step and the site's unpacker
// read), one fixed mtime, uid and gid 0, no user or group names, and a gzip
// header with no name and no time.
func tarball(files map[string][]byte, mtime time.Time) ([]byte, error) {
	mtime = mtime.UTC().Truncate(time.Second)
	dirs := map[string]bool{".": true}
	for name := range files {
		for d := path.Dir(name); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	var names []string
	for d := range dirs {
		if d == "." {
			names = append(names, "./")
		} else {
			names = append(names, "./"+d+"/")
		}
	}
	for name := range files {
		names = append(names, "./"+name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(gz)
	for _, name := range names {
		hdr := &tar.Header{Name: name, ModTime: mtime, Format: tar.FormatPAX}
		if strings.HasSuffix(name, "/") {
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Mode = 0o644
			hdr.Size = int64(len(files[strings.TrimPrefix(name, "./")]))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(files[strings.TrimPrefix(name, "./")]); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeFileAtomic writes data to dst through a temporary file in the same
// directory, so a reader never sees half a tarball.
func writeFileAtomic(dst string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, dst)
}

// writeTree writes files under dir, replacing a previous bundle there. It
// refuses a directory that holds anything but a bundle -- one with files and
// no manifest.json -- rather than delete what it did not write.
func writeTree(dir string, files map[string][]byte) error {
	entries, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return err
	case len(entries) > 0:
		if _, err := os.Stat(filepath.Join(dir, ManifestFile)); err != nil {
			return &RefusedOutError{Dir: dir}
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	for name, data := range files {
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// RefusedOutError is the refusal to clear an output directory that does not
// hold a previous bundle.
type RefusedOutError struct{ Dir string }

func (e *RefusedOutError) Error() string {
	return e.Dir + " is not empty and holds no " + ManifestFile + ", so it is not a previous bundle; refusing to replace it (pick an empty or new -out directory)"
}
