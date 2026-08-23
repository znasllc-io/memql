package memql

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
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

	var offenders []string
	for _, dir := range coreDirs {
		base := filepath.Join(root, dir)
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if repowalk.SkipDir(info.Name()) {
					return filepath.SkipDir
				}
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
			if containsBareSpaceID(string(data)) {
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

// repoRoot ascends from this test file's directory to the REPOSITORY root.
//
// It looks for go.work, not go.mod. Since memql#3228 the tree is ~30 modules
// and `component/memql` is one of them (memql#3242), so a go.mod walk stops at
// this package's own directory -- and every caller that joins a repo-relative
// path onto the result then reads `component/memql/dsl/...`, which does not
// exist. go.work exists exactly once, at the repository root, which is the
// thing these callers mean. Fourth instance of this shape after
// core/baseparser, component/architecture and component/database/memory-nodes.
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

// wordsEndingInSpace are English words that END in "space" and whose "...Id"
// form is therefore NOT a MemQL space id.
//
// This exists because the match is a case-insensitive substring one and
// `workspaceId` contains `spaceId` (memql#4334, which introduced Anthropic's
// federation `workspaceId` -- an id in ANOTHER vendor's account model, with no
// relationship to MemQL's tenant scope). A lint that flags it is simply wrong
// there, and the cost of being wrong is worse than a miss: the documented
// escape hatch is the Epic 3 migration baseline, so obeying it would record a
// false claim that these files hold a spaceId awaiting migration.
//
// The exemption is deliberately a short CLOSED list rather than a general
// "preceded by a letter" boundary. The latter would also exempt
// `voiceAgentSpaceId` and `ownerSpaceId`, which are exactly the leaks this
// test is for.
var wordsEndingInSpace = []string{"work", "name", "sub", "white", "air", "aero", "hyper"}

// containsBareSpaceID reports whether the source names a MemQL space id --
// spaceId / SpaceId / SpaceID in any casing -- as opposed to the tail of a
// longer word that happens to end in "space".
func containsBareSpaceID(src string) bool {
	lower := strings.ToLower(src)
	const needle = "spaceid"
	for i := 0; ; {
		idx := strings.Index(lower[i:], needle)
		if idx < 0 {
			return false
		}
		at := i + idx
		if !precededByWordEndingInSpace(lower[:at]) {
			return true
		}
		i = at + len(needle)
	}
}

func precededByWordEndingInSpace(before string) bool {
	for _, w := range wordsEndingInSpace {
		if strings.HasSuffix(before, w) {
			return true
		}
	}
	return false
}

// TestContainsBareSpaceIDStillCatchesTheRealThing is the reachable positive
// for the exemption above: relaxing a lint is only safe if it can still be
// shown to fire.
func TestContainsBareSpaceIDStillCatchesTheRealThing(t *testing.T) {
	hits := []string{
		`spaceId := ""`,
		`SpaceID string`,
		`cfg.SpaceId`,
		`voiceAgentSpaceId`, // the compound leak the boundary must NOT exempt
		`ownerSpaceID`,
	}
	for _, src := range hits {
		if !containsBareSpaceID(src) {
			t.Fatalf("the lint no longer catches %q", src)
		}
	}
	misses := []string{
		`WorkspaceID string`,
		`option.FederationOptions{WorkspaceID: id}`,
		`namespaceId`,
		`partition := ""`,
	}
	for _, src := range misses {
		if containsBareSpaceID(src) {
			t.Fatalf("the lint false-positives on %q", src)
		}
	}
}
