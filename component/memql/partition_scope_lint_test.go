package memql

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoNewSpaceIdInCore guards the partition-adoption boundary (issue 2.2):
// `partition` is the canonical tenant scope, so core must not grow NEW
// dependencies on `spaceId` (a downstream-product notion; `partition_id`
// was renamed `space_id` on the wire in #2441). The files that reference
// it today are grandfathered in testdata/spaceid_core_baseline.txt and get
// re-pointed onto `partition` by Epic 3; this test fails the moment a core
// .go file OUTSIDE that baseline introduces `spaceId`.
//
// Additions-only ratchet: a baseline file that no longer references spaceId is
// fine (Epic 3 prunes the list as it cleans up). The point is purely to stop
// new leaks. See docs/public/concepts/partition-scoping.md.
func TestNoNewSpaceIdInCore(t *testing.T) {
	root := repoRoot(t)
	baseline := loadSpaceIdBaseline(t, filepath.Join(root, "component", "memql", "testdata", "spaceid_core_baseline.txt"))

	// Core directories expected to become product-agnostic.
	coreDirs := []string{"component", "app"}
	const needle = "spaceid" // case-insensitive match against spaceId / SpaceId / SpaceID

	var offenders []string
	for _, dir := range coreDirs {
		base := filepath.Join(root, dir)
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			name := info.Name()
			if !strings.HasSuffix(name, ".go") {
				return nil
			}
			// Skip tests, generated code, and the partition-scope files that
			// legitimately NAME partitionId in order to forbid it.
			if strings.HasSuffix(name, "_test.go") ||
				strings.HasSuffix(name, ".pb.go") ||
				strings.Contains(path, string(os.PathSeparator)+"gen"+string(os.PathSeparator)) ||
				strings.HasPrefix(name, "partition_scope") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if baseline[rel] {
				return nil
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if strings.Contains(strings.ToLower(string(data)), needle) {
				offenders = append(offenders, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", base, err)
		}
	}

	if len(offenders) > 0 {
		t.Fatalf("new `spaceId` use in core (%d file(s)): %s\n\n"+
			"`partition` is the canonical tenant scope -- scope via memql.PartitionScope / "+
			"ResolvePartitionFromContext, not a product id. If this is a legitimate, "+
			"unavoidable case being migrated by Epic 3, add the path to "+
			"component/memql/testdata/spaceid_core_baseline.txt. "+
			"See docs/public/concepts/partition-scoping.md.",
			len(offenders), strings.Join(offenders, ", "))
	}
}

func loadSpaceIdBaseline(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open baseline: %v", err)
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[filepath.ToSlash(line)] = true
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	return out
}

// repoRoot ascends from this test file's directory until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller path")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from test file")
		}
		dir = parent
	}
}
