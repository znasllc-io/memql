package ci

import (
	"os"
	"path/filepath"
	"testing"
)

// Direct coverage for the nested-checkout skip in generatedProtoFiles
// (znasllc-io/memql#3346).
//
// The bug it pins: the walk collected `*.pb.go` from git worktrees living under
// `.claude/worktrees/` -- the location this repo's own .gitignore and CLAUDE.md
// document -- and reported them as uncovered wire trees of THIS repository. The
// result was `go test ./...` failing for exactly the developers following the
// documented layout, with a remediation message advising them to add their
// local directory name to ci.yml's `proto` bucket.
//
// Tested against a synthetic tree rather than the real repository, because the
// real one only exhibits the bug when the developer running the suite happens
// to have a worktree checked out -- i.e. the coverage would come and go with
// the machine. A synthetic root makes both directions unconditional.
func TestGeneratedProtoFilesSkipsNestedCheckouts(t *testing.T) {
	root := t.TempDir()

	// A generated tree belonging to THIS repo -- must be found.
	mustWrite(t, filepath.Join(root, "component", "node", "gen", "real.pb.go"), "package gen")

	// A git WORKTREE (".git" is a FILE pointing at the real gitdir) carrying its
	// own copy -- must be skipped.
	worktree := filepath.Join(root, ".claude", "worktrees", "feature-x")
	mustWrite(t, filepath.Join(worktree, ".git"), "gitdir: /somewhere/.git/worktrees/feature-x")
	mustWrite(t, filepath.Join(worktree, "component", "node", "gen", "copy.pb.go"), "package gen")

	// A full CLONE (".git" is a DIRECTORY) dropped in a subdirectory -- also
	// skipped. This is why the check tests for the `.git` entry rather than for
	// the `.claude` directory name.
	clone := filepath.Join(root, "vendorish", "someclone")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir clone/.git: %v", err)
	}
	mustWrite(t, filepath.Join(clone, "component", "grpc", "gen", "copy.pb.go"), "package gen")

	got := generatedProtoFiles(t, root)

	want := []string{"component/node/gen/real.pb.go"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("generatedProtoFiles(synthetic root) = %v, want %v.\n"+
			"A nested checkout's generated files are not files of this "+
			"repository; counting them fails `go test ./...` for any developer "+
			"with a worktree under .claude/worktrees/ (#3346).", got, want)
	}
}

// The skip must not swallow the real thing: a generated tree inside this
// repository that no bucket pattern covers still has to be reported. Without
// this, "skip nested checkouts" could quietly become "skip everything" and the
// guard would pass vacuously forever.
func TestGeneratedProtoFilesStillFindsUncoveredTrees(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "component", "bus", "gen", "a.pb.go"), "package gen")
	mustWrite(t, filepath.Join(root, "some", "new", "gen", "b.pb.go"), "package gen")

	got := generatedProtoFiles(t, root)
	if len(got) != 2 {
		t.Errorf("generatedProtoFiles found %v; both in-repo generated files must "+
			"be reported, including one in a tree no bucket names yet -- that is "+
			"the case the guard exists for (#3346)", got)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
