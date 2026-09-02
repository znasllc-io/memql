package repowalk

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSkipDirCoversTheDirectoriesThatBreakWalkers(t *testing.T) {
	for _, name := range []string{".git", ".claude", ".superpowers", "vendor", "node_modules"} {
		if !SkipDir(name) {
			t.Errorf("SkipDir(%q) = false, want true", name)
		}
	}
}

func TestSkipDirDoesNotSkipOrdinarySourceDirectories(t *testing.T) {
	// A skip list that is too wide silently narrows every walker's coverage,
	// which is a worse failure than the one it prevents: the walk still passes,
	// having looked at less than the author believes.
	for _, name := range []string{"component", "core", "dsl", "docs", "scripts", "integrations", "testdata", "gen", "bin"} {
		if SkipDir(name) {
			t.Errorf("SkipDir(%q) = true, want false -- this is ordinary source", name)
		}
	}
}

// TestSkipDirStopsAWalkEnteringAWorktree is the behavioural test: it builds the
// exact layout that broke TestDeclaredMetadataKeysAreReadByNothing -- a file at
// the real path, and an identical copy under .claude/worktrees/ -- and asserts a
// walk using SkipDir sees it once.
func TestSkipDirStopsAWalkEnteringAWorktree(t *testing.T) {
	root := t.TempDir()

	real := filepath.Join(root, "component", "thing")
	worktreeCopy := filepath.Join(root, ".claude", "worktrees", "lane-b", "component", "thing")
	for _, dir := range []string{real, worktreeCopy} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "source.go"), []byte("package thing\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	var seen []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Base(path) == "source.go" {
			seen = append(seen, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(seen) != 1 {
		t.Errorf("walk saw source.go %d time(s), want 1:\n  %v", len(seen), seen)
	}
}

func TestSkippedNamesIsSortedAndComplete(t *testing.T) {
	got := SkippedNames()
	want := []string{".claude", ".git", ".superpowers", "node_modules", "vendor"}
	if len(got) != len(want) {
		t.Fatalf("SkippedNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SkippedNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
