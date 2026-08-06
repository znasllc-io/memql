package dslfs

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// WalkMemqlFiles returns every .memql file path inside `root`,
// sorted alphabetically and filtered by the soft-disable convention.
//
// Soft-disable rules (Go-style):
//   - A file whose basename starts with `_` is skipped.
//   - A file inside a directory whose name starts with `_` is skipped
//     (any depth — `_disabled/foo/bar.memql` is also skipped).
//
// Only files ending in `.memql` are returned. Helper files like
// `*.tmpl`, `*.md`, `*.jsonc` are ignored by the walker.
//
// The function is filesystem-flavor agnostic: an embedded fs.FS, a
// disk-backed os.DirFS, or a test in-memory FS all work. The returned
// paths are slash-separated and rooted at the FS root (no leading
// slash). Callers feed them into ResolveImport / the import graph.
func WalkMemqlFiles(root fs.FS) ([]string, error) {
	if root == nil {
		return nil, fmt.Errorf("WalkMemqlFiles: root fs.FS cannot be nil")
	}

	var out []string
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), "_") {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".memql") {
			return nil
		}
		if strings.HasPrefix(name, "_") {
			return nil
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(out)
	return out, nil
}

// FileBasename returns the file's basename without the `.memql`
// extension. Used as the default alias when an import omits `as`.
// Equivalent to `strings.TrimSuffix(path.Base(p), ".memql")` but kept
// as a helper so the rule has one home.
func FileBasename(p string) string {
	return strings.TrimSuffix(path.Base(p), ".memql")
}
