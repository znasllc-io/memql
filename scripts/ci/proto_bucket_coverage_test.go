// Static guard: the `proto` bucket covers every generated wire tree that
// exists (znasllc-io/memql#3213).
//
// # The failure mode
//
// The `proto` bucket exists for the drift gate. `ci.yml` states its purpose
// directly:
//
//	A `.proto` edit WITHOUT its regenerated `.pb.go` matches here but NOT
//	`go`, so this is what makes the "forgot to regenerate" case run the check.
//
// That works only if the bucket names every tree the generator writes into.
// It listed two of the three: `component/grpc/gen` and `component/node/gen`,
// but not `component/bus/gen` -- while a comment forty lines below asserted the
// opposite, that the bucket *is* the whole D3 `wire` tier.
//
// The asymmetry is silent in both directions. `dorny/paths-filter` does not
// error on a bucket that matches nothing, and it does not error on a tree no
// bucket names either. A change confined to `component/bus/gen/*.pb.go` matched
// the `go` bucket but not `proto`, where the identical change under `grpc/gen`
// or `node/gen` matched both -- so one third of the wire tier was outside the
// drift gate with nothing reporting it.
//
// Enumerating the three trees in this guard would reproduce the original
// defect one level up: a fourth tree added later would be missing from the
// guard exactly as it was missing from the bucket. So the guard DISCOVERS the
// trees instead.
//
// # Why `*.pb.go` and not directories named `gen`
//
// The discovery predicate is "a directory containing generated protobuf
// output", not "a directory called gen". The repository contains `sdk/gen`,
// which is a hand-written package -- the importable core of the typed-SDK
// generator (`emit_go.go`, `emit_ts.go`, `resolve.go`). It is source, not
// output, and belongs in the `go` bucket where it already is.
//
// A name-based predicate would demand `sdk/gen/**` be added to a bucket
// reserved for regenerated wire code, which is both wrong and the kind of
// false positive that gets a guard deleted rather than fixed. `*.pb.go` is
// what `scripts/dev/proto-gen.sh` actually emits, so it is what the bucket
// actually needs to cover.
package ci

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// generatedProtoFiles returns every generated protobuf file in the repository,
// as slash-separated paths relative to the root -- the same shape
// `dorny/paths-filter` matches against.
func generatedProtoFiles(t *testing.T, root string) []string {
	t.Helper()

	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".pb.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		found = append(found, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}
	sort.Strings(found)
	return found
}

// TestProtoBucketCoversEveryGeneratedWireTree is the guard.
func TestProtoBucketCoversEveryGeneratedWireTree(t *testing.T) {
	root := RepoRoot()

	raw, err := ReadCIWorkflow()
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	buckets, err := ParseChangesFilters(raw)
	if err != nil {
		t.Fatalf("parse the changes job's filters block: %v", err)
	}
	patterns, ok := buckets["proto"]
	if !ok {
		t.Fatal("no `proto` bucket in the changes job's filters -- this guard " +
			"cannot verify what it was written to verify, so it fails closed")
	}
	matcher, err := CompileBucket("proto", patterns)
	if err != nil {
		t.Fatalf("compile the proto bucket: %v", err)
	}

	files := generatedProtoFiles(t, root)
	if len(files) == 0 {
		t.Fatal("found no *.pb.go anywhere in the repository -- either the " +
			"generated wire trees moved or this guard's discovery is broken. " +
			"Failing closed rather than reporting a vacuous pass")
	}

	// Report per-tree rather than per-file: three trees produce dozens of
	// files, and one line per tree is the actionable unit.
	uncovered := map[string]string{} // dir -> first uncovered file in it
	for _, f := range files {
		if matcher.Match(f) {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(f))
		if _, seen := uncovered[dir]; !seen {
			uncovered[dir] = f
		}
	}
	for _, dir := range sortedStringKeys(uncovered) {
		t.Errorf("generated wire tree %q is not covered by the `proto` bucket "+
			"(e.g. %q matches none of %v).\n"+
			"The bucket is what makes a `.proto` edit without its regenerated "+
			"`.pb.go` run the drift check. A tree outside it is outside that "+
			"gate, and paths-filter reports nothing when a tree no pattern "+
			"names changes. Add %q to the `proto` bucket in ci.yml.",
			dir, uncovered[dir], patterns, dir+"/**")
	}
}

func sortedStringKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestGeneratedWireTreesAreTheOnesTheProtoBucketNames is the reverse
// direction: every `*/gen/**` pattern in the `proto` bucket points at a tree
// that actually holds generated output.
//
// Without this, the bucket could satisfy the guard above by naming a
// directory that no longer exists or that never held `.pb.go` -- a pattern
// matching nothing, which is the memql#3163 fail-open the sibling guard in
// filter_buckets_test.go exists to catch, in the one bucket where "covers
// everything" is now enforced and could be gamed.
func TestGeneratedWireTreesAreTheOnesTheProtoBucketNames(t *testing.T) {
	root := RepoRoot()

	raw, err := ReadCIWorkflow()
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	buckets, err := ParseChangesFilters(raw)
	if err != nil {
		t.Fatalf("parse the changes job's filters block: %v", err)
	}

	trees := map[string]bool{}
	for _, f := range generatedProtoFiles(t, root) {
		trees[filepath.ToSlash(filepath.Dir(f))] = true
	}

	for _, pattern := range buckets["proto"] {
		// Only the tree-shaped entries make a claim about generated output.
		// `**/*.proto` and `scripts/dev/proto-gen.sh` name sources, not trees.
		dir, ok := strings.CutSuffix(pattern, "/**")
		if !ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir))); err != nil {
			t.Errorf("the `proto` bucket names %q, which does not exist", pattern)
			continue
		}
		if !trees[dir] {
			t.Errorf("the `proto` bucket names %q, but that tree holds no "+
				"generated protobuf output. Either it is the wrong path or it "+
				"belongs in a different bucket -- `proto` is for regenerated "+
				"wire code, and a pattern here that matches no `.pb.go` is a "+
				"lane input that will never fire.", pattern)
		}
	}
}
