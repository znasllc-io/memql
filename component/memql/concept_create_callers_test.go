package memql

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// concept_create_callers_test.go -- memql#3067.
//
// `Concept.Create(ctx, store, params)` writes a row STRAIGHT TO THE STORE. It
// is not the write path -- `executeWrite` is -- and every write guard in the
// engine is hooked there rather than on Create:
//
//   - validateAgentRolePredefinedLock (#2985 / #3061)
//   - validateRbacBaseRoleImmutable / validateRbacCustomRoleRankBound
//   - the healing-override guard
//   - validateAgentKindActorScope + the agent-lock validation
//   - the identity credential actor-scope guard (#2513)
//
// So a second caller of Create does not weaken one guard. It skips ALL of
// them, silently, while every guard still reads as present and every test of
// them stays green.
//
// The legacy `seed.memql` seeder was exactly that caller and is deleted in this
// change. It was already dead -- it walked a typed-empty FS that existed only
// to feed it -- so nothing broke; the hazard was entirely in someone adding the
// first `seed.memql` and getting catalog rows written behind every lock.
//
// This gate is what makes the deletion durable. Deleting dead code proves
// nothing about the next person who writes the same thing, and "route it
// through executeWrite" is not a rule anyone can see from Create's signature.

// createCallSite is one `<recv>.Create(ctx, store, ...)` call.
type createCallSite struct {
	path string
	line int
}

func (c createCallSite) String() string { return c.path + ":" + strconv.Itoa(c.line) }

// TestConceptCreateHasExactlyOneWritePath asserts that the store-writing
// `Create` is reached from exactly one non-test location: the engine's mutation
// executor, which is downstream of executeWrite and therefore of every guard.
//
// Matched on the CALL SHAPE -- a three-argument `X.Create(...)` whose middle
// argument is a `store`-named identifier -- rather than on a package path, so a
// new caller is caught wherever it is written.
func TestConceptCreateHasExactlyOneWritePath(t *testing.T) {
	const allowed = "component/memql/executor_mutation.go"

	root := repoRootForCreateScan(t)
	var sites []createCallSite
	parsed := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "sdk", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := goparser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // unparseable file is not this gate's business
		}
		parsed++
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 3 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "Create" {
				return true
			}
			// The store argument is what makes this the raw write rather than
			// one of the many unrelated three-arg Create methods in the tree
			// (PATs, badges, worker tokens, os.Create...).
			mid, ok := call.Args[1].(*ast.Ident)
			if !ok || !strings.Contains(strings.ToLower(mid.Name), "store") {
				return true
			}
			sites = append(sites, createCallSite{path: filepath.ToSlash(rel), line: fset.Position(call.Pos()).Line})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if parsed == 0 {
		t.Fatal("no non-test Go files parsed, so this gate measures nothing")
	}

	const remedy = "\n\nConcept.Create writes STRAIGHT TO THE STORE and skips executeWrite, so it " +
		"skips every write guard at once -- the agentRole predefined lock, the RBAC base-role and " +
		"rank guards, the healing-override guard, the agent kind/actor-scope checks and the " +
		"identity credential actor-scope guard. None of them fail; they simply never run.\n" +
		"Route the write through the engine (executeWrite) instead. If a second raw-store writer " +
		"is genuinely required, it needs its own review and this gate updated in the same change " +
		"(memql#3067)."

	if len(sites) != 1 {
		var got []string
		for _, s := range sites {
			got = append(got, s.String())
		}
		t.Fatalf("Concept.Create(ctx, store, ...) has %d non-test callers, want exactly 1 (%s).\n  found: %s%s",
			len(sites), allowed, strings.Join(got, ", "), remedy)
	}
	if !strings.HasPrefix(sites[0].path, allowed) {
		t.Fatalf("the sole Concept.Create caller moved from %s to %s. If the write path genuinely "+
			"moved, update this gate; if a second writer appeared, it bypasses every guard.%s",
			allowed, sites[0], remedy)
	}
}

// The legacy seeder package must stay deleted. Its own walk source
// (database.Concepts, a typed-empty FS) is gone with it, so re-adding the
// package would not even compile -- but the gate states the intent, because
// "someone re-adds a seed.memql walk" is the specific thing #3067 is about and
// a compile error does not explain why.
func TestLegacySeedMemqlSeederStaysDeleted(t *testing.T) {
	root := repoRootForCreateScan(t)
	for _, gone := range []string{
		"component/database/memory-nodes/seeder",
		"component/database/embed.go",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(gone))); err == nil {
			t.Errorf("%s is back. It was deleted in memql#3067 because it wrote through "+
				"Concept.Create directly against the store, skipping executeWrite and every "+
				"guard hooked there, over a `seed.memql` input set that was empty.\n"+
				"SeedMaterializer is the live seeding path and goes through the guarded route.", gone)
		}
	}
}

func repoRootForCreateScan(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the repo root (no go.mod found walking up)")
	return ""
}
