package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// walkerExemptions lists the test files that walk a tree WITHOUT calling
// repowalk.SkipDir, each with the reason it cannot descend into `.claude`.
//
// Two reasons are legitimate, and nothing else is:
//
//   - the walk is rooted at a narrow subtree that is not an ancestor of
//     `.claude` (a corpus directory, the vscode extension source, ...)
//   - the walk already skips EVERY dot-prefixed directory, which covers
//     `.claude` by construction
//
// Anything else must route through repowalk.SkipDir. A walker that descends
// into `.claude/worktrees/` sees a full second copy of this repository and
// reports duplicated results -- and does so only on machines where somebody
// happens to have a worktree checked out, so it is green in CI and red locally.
// That is memql#3678, and TestDeclaredMetadataKeysAreReadByNothing is the one
// that actually broke.
var walkerExemptions = map[string]string{
	"cmd/memql-lsp/vscodeimportrule_test.go":         "walks vscodeExtensionSrcDir, the extension's TypeScript source",
	"deploy/fleet/bundle_test.go":                    "walks deploy/fleet/dsl, the fleet DSL bundle, which contains no nested checkout",
	"deploy/k8s/components/tenant/render_test.go":    "walks a t.TempDir() a capability script just rendered into",
	"component/memql/sense/imports_test.go":          "walks corpusRoot, the DSL corpus",
	"component/memql/sense/runnable_test.go":         "walks corpusRoot, the DSL corpus",
	"docs_construct_names_test.go":                   "walks dsl/, and skips every dot-prefixed directory",
	"portal_render_path_test.go":                     "walks the portal source directory",
	"portal_view_composition_test.go":                "walks the portal view directory",
	"scripts/cidb/dbgate_test.go":                    "skips every dot-prefixed directory",
	"scripts/cidb/dsnliteral_test.go":                "skips every dot-prefixed directory",
	"scripts/citags/tags_test.go":                    "skips every dot-prefixed directory",
	"scripts/install/mkcert_pair_round_trip_test.go": "walks a t.TempDir() the install capability scripts just wrote into -- a fixture home, not an ancestor of .claude, and it must account for EVERY file in it",
	"scripts/k3d/up_rendered_manifest_test.go":       "copies deploy/k8s, a narrow subtree with no nested checkout -- and a COPY must skip nothing, or it renders a tree the repository does not have",
	"test/dslconformance/callgraph_contract_test.go": "walks the DSL tree",
	"cmd/shopifyschema/shopify_schema_drift_test.go": "walks the two GENERATED directories and the t.TempDir() they were just regenerated into -- a byte-for-byte comparison that must account for every file in both, so it can skip nothing",
}

// TestRepoWalkersShareOneSkipList is the memql#3678 gate.
//
// The skip list used to be copied into each walker, and the copies drifted:
// several covered `.git` / `vendor` / `node_modules` but not `.claude`. This
// asserts there is now ONE definition and that every walker either uses it or
// is exempt for a stated reason.
//
// It also fails on a STALE exemption -- an entry whose file no longer walks a
// tree, or which has since started using the helper. An exemption list nobody
// prunes becomes a list of claims nobody checks.
func TestRepoWalkersShareOneSkipList(t *testing.T) {
	var walkers []string

	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// This gate must observe the rule it enforces: without the skip it
			// would find every walker three times over and report each worktree
			// copy as an unexempted violation.
			if repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		src := string(body)
		if !strings.Contains(src, "filepath.Walk") && !strings.Contains(src, "filepath.WalkDir") {
			return nil
		}
		// The helper's own tests walk a temp dir to prove the skip works.
		if strings.HasPrefix(filepath.ToSlash(path), "core/repowalk/") {
			return nil
		}
		walkers = append(walkers, filepath.ToSlash(path))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(walkers) < 20 {
		t.Fatalf("found only %d repo walkers, expected 20+ -- the scan is broken, not the tree", len(walkers))
	}

	seen := map[string]bool{}
	var unexempted []string
	for _, w := range walkers {
		body, _ := os.ReadFile(w)
		usesHelper := strings.Contains(string(body), "repowalk.SkipDir")
		if _, exempt := walkerExemptions[w]; exempt {
			seen[w] = true
			if usesHelper {
				t.Errorf("%s is listed in walkerExemptions but now uses repowalk.SkipDir -- delete the exemption", w)
			}
			continue
		}
		if !usesHelper {
			unexempted = append(unexempted, w)
		}
	}

	if len(unexempted) > 0 {
		sort.Strings(unexempted)
		t.Errorf("%d test file(s) walk a tree without repowalk.SkipDir and without an exemption:\n  %s\n\n"+
			"Add `if d.IsDir() && repowalk.SkipDir(d.Name()) { return filepath.SkipDir }` to the walk callback, "+
			"or add an entry to walkerExemptions stating why the walk cannot reach .claude/ (memql#3678).",
			len(unexempted), strings.Join(unexempted, "\n  "))
	}

	for w := range walkerExemptions {
		if !seen[w] {
			t.Errorf("walkerExemptions lists %q, which no longer walks a tree -- delete the stale entry", w)
		}
	}
}
