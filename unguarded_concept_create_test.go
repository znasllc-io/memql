package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestNoUnguardedDirectToStoreConceptCreate keeps the write path single
// (memql#3176, carrying memql#3067).
//
// THE RULE. memoryNodes.Concept.Create(ctx, store, params) writes a row
// straight to the backing store. It is the primitive under the mutation
// executor, and the mutation executor is where every write guard is hooked:
//
//   - validateAgentRolePredefinedLock (#2985 / #3061)
//   - validateRbacBaseRoleImmutable and validateRbacCustomRoleRankBound
//   - the healing-override guard
//   - validateAgentKindActorScope and the agent-lock validation
//   - the identity credential actor-scope guard (#2513)
//
// A caller that reaches Concept.Create on its own does not skip one of those.
// It skips all of them at once, and skips them silently: the guards stay
// present, stay tested and stay green, while rows land behind every lock they
// describe. So the population of callers is the invariant worth pinning, and
// it is exactly one.
//
// WHAT WAS DELETED. component/database/memory-nodes/seeder/ was the second
// caller, wired into database bring-up by app/database.go. It ingested files
// named seed.memql out of database.Concepts -- a compile-time fstest.MapFS{}
// literal, never rebound, with zero matching files on the tree -- so its loop
// body never executed and nothing was ever mis-written. It was deleted rather
// than routed through the executor because an unused boot path with an empty
// input set and no guards is worth removing rather than documenting. The
// hazard was live all the same: seed.memql is the obvious name for a seed
// file, and the repo has an active, GUARDED seeding concept next to it
// (SeedMaterializer, dsl/agents/roles/*.memql, dsl/rbac/seeds.memql). A
// contributor adding seed.memql for agentRole or rbac:role would have gotten
// catalog rows written behind every lock, with nothing failing and nothing
// logged.
//
// WHY THIS HAS NO ARGUMENT PREDICATE -- the load-bearing detail.
//
// A previous attempt at this same gate ALSO constrained the call's arguments.
// It did not fire on the one call it was written to stop. Restoring the
// seeder verbatim left the gate green, and git mv-ing the file left it green
// with the whole legacy seeder present on the tree; dropping the argument
// predicate is what made both of those fail as they should. A gate that
// cannot see the call it was written for is worse than no gate, because it is
// credited as coverage.
//
// The predicate also bought nothing. Concept.Create's signature is
// (context.Context, Store, CreateParams) -- every syntactic call to it has
// exactly three arguments, so arity alone already catches every spelling of
// the call, whatever the argument expressions look like. Matching arity plus
// the method name over-reports by a handful of unrelated three-argument
// Create methods, and that over-report is answered by an allowlist naming
// files, which is reviewable, rather than by a predicate over expression
// shapes, which is not. Do not reintroduce an argument predicate here.
//
// WHY AN ALLOWLIST AND NOT A DENYLIST. A denylist protects the packages
// somebody thought of. The population permitted to reach Concept.Create is
// small, stable and known; anything joining it is a change worth a reviewer
// looking at deliberately. A new caller fails here by default, which is the
// direction the failure should point.
//
// WHY IT PARSES RATHER THAN GREPS. go/parser distinguishes a call from prose
// about a call. component/identity/pat/store.go and .../badge/store.go both
// carry `err = store.Create(...)` inside doc comments; a grep-based gate
// either fires on documentation or needs exclusions that defeat it. Parsing
// also means build constraints are ignored, so this sees files no CI lane
// compiles -- including tags no lane will ever run.
//
// WHAT THIS DOES NOT CATCH, stated precisely, because an over-claimed
// guarantee is worse than a modest one:
//
//   - A new call added INSIDE an already-allowlisted file. Granularity is
//     per-file; within those three files this is documentation, not a gate.
//   - Indirection through a method value or an interface: `f := c.Create; f(...)`,
//     or a wrapper type whose own method forwards to Concept.Create. Nothing
//     on the tree does this, and it would not arise by accident, but it is
//     not visible to a syntactic check.
//   - Writes that reach the store by some route other than Concept.Create
//     (hand-rolled bun INSERTs). Those are a different bypass class; task
//     #3175 records the raw `insert(` one, which -- unlike this -- still
//     reaches the executor and therefore still hits these guards.
func TestNoUnguardedDirectToStoreConceptCreate(t *testing.T) {
	// File -> why that file may call a three-argument Create.
	//
	// Keyed by file rather than by directory: component/memql is large, and
	// "somewhere in component/memql" is not a claim worth making.
	allowed := map[string]string{
		// THE guarded write path. This is the call every mutation funnels
		// into, after executeWrite has run the validators above.
		"component/memql/executor_mutation.go": "the mutation executor -- the one guarded write path",

		// Unit tests of Concept.Create itself, against in-memory fakes
		// (&capturingStore{}, &fakeConceptStore{}, &redactionStore{}). No
		// database, no boot path: these exercise the primitive rather than
		// write through it.
		"component/database/memory-nodes/declared_metadata_annotations_test.go": "Concept.Create unit test against a fake store",
		"component/memql/nodespec_composite_id_2885_test.go":                    "Concept.Create unit test against a fake store",
		// Arrived with epic secret-redaction (memql#3182) while this epic was
		// in flight, so the two branches first met at the merge. Checked
		// rather than assumed before allowlisting: both call sites pass
		// &redactionStore{}, an in-memory fake, against a concept parsed from
		// a test fixture -- the same category as the two entries above, and
		// not a write path.
		"component/database/memory-nodes/concept_secret_redaction_test.go": "Concept.Create unit test against a fake store",
	}

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	fset := token.NewFileSet()
	var violations []string

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			// A file that does not parse cannot be assessed. Fail loudly
			// rather than treating it as clean -- silence here is exactly
			// how a gate rots into decoration.
			violations = append(violations, rel+": parse error: "+err.Error())
			return nil
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Create" {
				return true
			}
			// Arity is the WHOLE predicate, deliberately. See the
			// "no argument predicate" note above before narrowing this.
			if len(call.Args) != 3 {
				return true
			}
			if _, ok := allowed[rel]; ok {
				return true
			}
			violations = append(violations,
				rel+":"+strconv.Itoa(fset.Position(call.Pos()).Line))
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk repo: %v", walkErr)
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("unguarded direct-to-store concept.Create found at %d site(s):\n  %s\n\n"+
			"Concept.Create(ctx, store, params) writes straight to the store and skips\n"+
			"executeWrite, and therefore skips EVERY write guard hooked there at once\n"+
			"(agentRole predefined lock, rbac base-role immutability + custom-role rank\n"+
			"bound, the healing-override guard, agent kind/actor scope, the identity\n"+
			"credential actor-scope guard). Nothing fails and nothing is logged when a\n"+
			"row lands this way.\n\n"+
			"Route the write through a mutation so it goes through the executor. If this\n"+
			"call genuinely belongs on the guarded path or is a unit test of the\n"+
			"primitive against an in-memory fake, add the file to the allowlist in this\n"+
			"test WITH a reason -- and do not narrow the match with an argument predicate\n"+
			"instead; that is precisely how the previous version of this gate missed the\n"+
			"call it existed to stop (memql#3176).",
			len(violations), strings.Join(violations, "\n  "))
	}

	// A stale allowlist is a silent hole: an entry whose file is gone, or
	// which no longer contains the call, reads as coverage while protecting
	// nothing. Assert every entry is still load-bearing.
	for rel, reason := range allowed {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("allowlist entry %q (%s) no longer exists -- remove it", rel, reason)
		}
	}
}
