// component/edge/bundle_test.go
package edge

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileSchemeOpensADirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	fsys, err := NewFileOpener().Open("file://" + dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b, err := fsReadFile(fsys, "index.html")
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	if string(b) != "<html>hi" {
		t.Errorf("got %q", b)
	}
}

// An unknown scheme is an error, not a silent fallback to a local path. A
// bundleRef the edge cannot honour must surface as a broken site, because the
// alternative is serving the wrong bytes from somewhere plausible.
func TestUnknownSchemeIsRefused(t *testing.T) {
	if _, err := NewFileOpener().Open("gopher://example.com/x"); err == nil {
		t.Error("Open accepted an unknown scheme")
	}
}
