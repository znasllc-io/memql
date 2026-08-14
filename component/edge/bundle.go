package edge

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// BundleOpener turns a site's bundleRef into a filesystem to serve from. The
// scheme is what makes "shipped in the image" and "uploaded to storage" a
// difference in DATA rather than in code path -- which is what lets the portal
// be site #1 with no special case.
type BundleOpener interface {
	Open(ref string) (fs.FS, error)
}

type fileOpener struct{}

// NewFileOpener handles file:// -- the bundle the image ships (the portal) and
// a working tree (the dev inner loop). Task 4 composes this with blob://.
func NewFileOpener() BundleOpener { return fileOpener{} }

func (fileOpener) Open(ref string) (fs.FS, error) {
	const scheme = "file://"
	if !strings.HasPrefix(ref, scheme) {
		return nil, fmt.Errorf("edge: bundleRef %q is not a file:// reference", ref)
	}
	dir := strings.TrimPrefix(ref, scheme)
	if dir == "" {
		return nil, fmt.Errorf("edge: bundleRef %q names no directory", ref)
	}
	// os.DirFS is rooted: a path that would escape the directory is refused by
	// the filesystem itself.
	return os.DirFS(dir), nil
}

func fsReadFile(fsys fs.FS, name string) ([]byte, error) { return fs.ReadFile(fsys, name) }
