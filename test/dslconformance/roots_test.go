package dslconformance

// roots_test.go -- path resolution for a suite that reads the repository.
//
// This suite used to live IN `dsl/`, so "the DSL tree" was the working
// directory and "the docs tree" was `../docs`. memql#3242 moved it out: `dsl`
// is a module now, and a test that imports `component/memql` (as this suite's
// conformance lanes do) cannot sit inside the module `component/memql`
// requires. The suite moved up to the root module; the cwd-relative paths it
// carried moved with it and had to become root-relative.
//
// The root is found by walking to `go.work`, NOT to the nearest `go.mod`. The
// tree is ~30 modules since memql#3228, so a `go.mod` walk finds the module the
// test happens to sit in -- which is how three earlier gates silently narrowed
// their scan to their own directory (`core/baseparser`,
// `component/architecture`, `component/database/memory-nodes`). `go.work`
// exists exactly once, at the repository root, which is the thing meant here.

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// repoRoot is the repository root, resolved from this file's compiled-in path.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.work not found walking up from test file")
		}
		dir = parent
	}
}

// repoPath joins a repo-relative path onto the repository root.
func repoPath(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join(repoRoot(t), rel)
}

// dslPath joins a `dsl/`-relative path -- the paths this suite carried from
// when it ran with the DSL tree as its working directory.
func dslPath(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "dsl", rel)
}
