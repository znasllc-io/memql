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
			if softDisabled(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if walkedMemqlFile(d.Name()) {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(out)
	return out, nil
}

// HoldsMemqlFile reports whether dir, a directory of root, holds a .memql file
// WalkMemqlFiles would return, at any depth. It is the ONE answer to "is this
// directory a DSL domain" (memql#5426 review): the offline mounts ask it of a
// root's top-level entries, and the editor's workspace graph asks it when it
// looks for the directory the domains sit in. The editor used to require a
// .memql DIRECTLY in the directory, so a product domain holding nothing but
// sub-namespace directories was a domain to boot and to memqllint and not one
// to the editor, which then resolved every `use` line against the wrong root.
//
// It stops at the first file it finds.
func HoldsMemqlFile(root fs.FS, dir string) bool {
	sub, err := fs.Sub(root, dir)
	if err != nil {
		return false
	}
	found := false
	_ = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if d.IsDir() {
			if softDisabled(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if walkedMemqlFile(d.Name()) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// softDisabled reports whether a directory or file name is soft-disabled: a
// leading `_` takes it, and everything below it, out of the tree.
func softDisabled(name string) bool {
	return strings.HasPrefix(name, "_")
}

// walkedMemqlFile reports whether a file name is one WalkMemqlFiles returns.
func walkedMemqlFile(name string) bool {
	return strings.HasSuffix(name, ".memql") && !softDisabled(name)
}

// FileBasename returns the file's basename without the `.memql`
// extension. Used as the default alias when an import omits `as`.
// Equivalent to `strings.TrimSuffix(path.Base(p), ".memql")` but kept
// as a helper so the rule has one home.
func FileBasename(p string) string {
	return strings.TrimSuffix(path.Base(p), ".memql")
}
