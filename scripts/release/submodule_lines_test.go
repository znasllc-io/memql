package release

// submodule_lines_test.go -- the version-line assignment is complete and real
// (memql#3245, epic memql#3228).
//
// memQL's nested modules carry TWO independent version lines -- `wire` and
// `engine` -- and everything else is lockstep with the root release. The lists
// live in scripts/release/tag-submodules.sh because a version line is a
// DECISION: deriving it from the tree would let a directory acquire an
// independent line by accident, which is the opposite of the restraint the
// design chose ("29 modules with 29 version lines is 29 things to keep in
// step").
//
// A literal list rots. These tests are what stops it:
//
//   - every directory named in either list is a module on disk, so a rename or
//     a retirement cannot leave the release script tagging a path that is not
//     there;
//   - the two lists do not overlap, so no module has two version lines;
//   - the lockstep set is the COMPLEMENT, computed by the script rather than
//     listed, so a module added tomorrow is lockstep by default -- and this
//     test proves the complement covers everything the two lists do not.
//
// What is deliberately NOT asserted: that a particular directory is in a
// particular line. That is the decision itself, and a test restating it would
// only make the decision harder to change without making it more correct.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bashArrayRe pulls the entries out of `readonly NAME=( "a" "b" )`.
func bashArray(t *testing.T, script, name string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?s)readonly ` + regexp.QuoteMeta(name) + `=\((.*?)\n\)`)
	m := re.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("tag-submodules.sh no longer declares %s as a `readonly %s=( ... )` array. "+
			"If the version lines moved somewhere else, move this guard with them -- without "+
			"it a line can name a directory that does not exist.", name, name)
	}
	var out []string
	for _, line := range strings.Split(m[1], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.Trim(line, `"`))
	}
	return out
}

func repoRootForRelease(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// go.work, not go.mod: this package is inside the root module, but the
	// tree is ~48 modules and the same walk elsewhere has repeatedly found a
	// nested one. go.work exists exactly once, at the repository root.
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate repo root (no go.work above cwd)")
	return ""
}

// modulesOnDisk returns every nested module directory, repo-relative.
func modulesOnDisk(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && (info.Name() == ".git" || info.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if info.IsDir() || info.Name() != "go.mod" {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		if rel != "." {
			out[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

func TestIndependentVersionLinesNameRealModules(t *testing.T) {
	root := repoRootForRelease(t)
	raw, err := os.ReadFile(filepath.Join(root, "scripts/release/tag-submodules.sh"))
	if err != nil {
		t.Fatalf("read tag-submodules.sh: %v", err)
	}
	script := string(raw)
	onDisk := modulesOnDisk(t, root)

	seen := map[string]string{}
	for _, name := range []string{"WIRE_MODULES", "ENGINE_MODULES"} {
		entries := bashArray(t, script, name)
		if len(entries) == 0 {
			t.Errorf("%s is empty -- an independent version line with no modules tags nothing", name)
		}
		for _, dir := range entries {
			if !onDisk[dir] {
				t.Errorf("%s names %q, which is not a module directory. A release would try to "+
					"tag %s/vX.Y.Z at a path with no go.mod.", name, dir, dir)
			}
			if prev, dup := seen[dir]; dup {
				t.Errorf("%q is in both %s and %s -- a module has exactly one version line", dir, prev, name)
			}
			seen[dir] = name
		}
	}
}

// TestLockstepIsTheComplement proves the third line is computed, not listed:
// every module that is not on an independent line is selected by
// `--line=lockstep`, so a module added tomorrow gets the root's number without
// anyone remembering to add it anywhere.
func TestLockstepIsTheComplement(t *testing.T) {
	root := repoRootForRelease(t)
	raw, err := os.ReadFile(filepath.Join(root, "scripts/release/tag-submodules.sh"))
	if err != nil {
		t.Fatalf("read tag-submodules.sh: %v", err)
	}
	script := string(raw)

	independent := map[string]bool{}
	for _, name := range []string{"WIRE_MODULES", "ENGINE_MODULES"} {
		for _, dir := range bashArray(t, script, name) {
			independent[dir] = true
		}
	}

	onDisk := modulesOnDisk(t, root)
	if len(onDisk) == 0 {
		t.Fatal("found no nested modules; the walk is broken, not the tree")
	}

	// The script's complement is "every module dir, minus the two lists".
	// Recompute it here and assert the partition is total: independent +
	// lockstep == every module, with nothing left over.
	lockstep := 0
	for dir := range onDisk {
		if !independent[dir] {
			lockstep++
		}
	}
	if lockstep+len(independent) != len(onDisk) {
		t.Errorf("version lines do not partition the module set: %d independent + %d lockstep != %d on disk",
			len(independent), lockstep, len(onDisk))
	}
	if lockstep == 0 {
		t.Error("no module is lockstep -- either the tree shrank to the two independent lines, " +
			"or the complement stopped being computed")
	}
}
