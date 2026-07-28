package auth

// internal_origin_callers_test.go -- makes ContextWithInternalOrigin's own
// prohibition enforceable instead of advisory (memql#2889).
//
// The doc on ContextWithInternalOrigin says:
//
//	Call this ONLY from Go that is itself the caller of a @serverOnly construct,
//	on a context that code controls. Never call it in a request handler on a
//	context derived from an inbound request: that would launder client origin
//	into internal for everything downstream.
//
// Nothing enforced that. #2889 was filed about the inverse asymmetry -- one gRPC
// handler stamps CLIENT while ~29 others do not -- and the answer to that half is
// that it does not matter: OriginClient is the zero value, so an unstamped
// handler is already untrusted, and none of the 13 internal stamps in the tree
// wraps a handler invocation. Stamping the other 29 would buy nothing.
//
// What DOES matter is the other direction. A single ContextWithInternalOrigin in
// the wrong place is a privilege grant, and the rule against it was a comment.
//
// # What this catches, and what it does not
//
// Stated precisely, because an over-claimed guarantee is worse than a modest one:
//
//   - It catches a REFERENCE to the symbol from a package not on the allowlist,
//     whether called directly, taken as a function value, or passed as an
//     argument. Reference rather than call position is deliberate: `var f =
//     auth.ContextWithInternalOrigin` followed by `f(ctx)` is the same privilege
//     grant, and an earlier version of this test that only inspected CallExpr.Fun
//     let exactly that through.
//   - It does NOT catch a new caller INSIDE an already-allowlisted package.
//     Granularity is per-package, and component/memql (183 files) and app (58)
//     are large. Within those, this is documentation rather than a gate.
//   - It does NOT catch laundering through an exported ctx-returning wrapper in
//     an allowlisted package. None exists today -- originForSource in
//     component/automations is unexported -- but exporting one would open a hole
//     this test cannot see.
//   - It does NOT assert that component/identity/admin's HTTP gate stays in
//     place; see the allowlist entry and memql#2934.
//
// # Why AST and not grep
//
// #2888's review replaced a grep-based assertion with a registry-based one for
// the same reason this parses: a grep matches the identifier inside a string
// literal, and component/language/annotations/registry.go names
// ContextWithInternalOrigin in an annotation's help text. It also matches
// comments -- component/auth/identity_resolver.go:78 mentions the symbol one line
// above the real call. Neither is a reference; the AST knows the difference.
//
// # Why this catches more than the compiler would
//
// go/parser ignores build constraints, so this sees files no CI lane compiles.
// app/integrations_identity.go is `//go:build identity` and does call the helper;
// per #2903 the tagged lanes only `go build`, never `go test`, so a violation
// added to a tagged file would otherwise reach main unexamined. That is not
// hypothetical here -- #2903 exists because a tagged test file had never run.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// internalOriginFunc is the symbol whose callers are restricted.
const internalOriginFunc = "ContextWithInternalOrigin"

// allowedInternalOriginCallers maps a package directory, relative to the module
// root, to why it may stamp internal origin.
//
// Adding an entry is a security decision, not bookkeeping. The bar is the doc's:
// the package must be server-side Go that is itself the caller of a @serverOnly
// construct, stamping a context it controls. A request handler does not qualify.
var allowedInternalOriginCallers = map[string]string{
	"component/auth": "identity_resolver resolves sub -> user before an actor " +
		"exists; that resolution IS how an AccessContext gets built, so it cannot " +
		"be caller-scoped",
	"component/identity": "the identity store reads users cross-user by design, " +
		"on contexts it constructs",
	"component/identity/pat": "personal-access-token store walks activeUsers to " +
		"resolve a token's owner",
	"component/memql": "the engine's own authoring/seed paths run as the system, " +
		"on contexts they construct",
	"component/automations": "originForSource decides per-automation trust and " +
		"checkpoints run as the system; #2888 established the trust rule here",
	"app":                     "integrations_identity wires a server-side identity lookup",
	"integrations/dailyspace": "a server-side integration acting as the system",
	"integrations/agent/worker": "the agent worker is server-side Go with no " +
		"inbound request context",

	// DECLARED EXCEPTION, not an endorsement. component/identity/admin/handlers.go
	// stamps internal on an HTTP-request context and passes a caller-supplied
	// userId to userByIdSystem -- literally the shape the doc forbids. Every route
	// reaching it is registered through gated() -> requireAdmin, so it is not a
	// live hole, and #2889 says as much.
	//
	// Note the gate is owner-OR-admin (component/identity/admin/auth.go), which is
	// wider than the codebase's IsClusterOwner() == RoleOwner. It is also per-route
	// on a shared mux, so it is one forgotten gated() from a hole, and nothing here
	// would notice. That gap is memql#2934.
	//
	// Listed so this test passes on a tree that contains it, rather than being
	// written to pass by pretending it is absent.
	"component/identity/admin": "GATED EXCEPTION: stamps internal on a " +
		"request-derived ctx, contrary to the doc, relying on the admin HTTP " +
		"layer as its gate (memql#2889, gap tracked in memql#2934)",
}

// skipDirs are not searched. Deliberately short: sdk/ is NOT skipped even though
// it is generated, because it is part of this module and
// sdk/go/client/generated_logics.go imports component/auth -- a generator change
// that introduced a stamp there would otherwise be invisible.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
}

func TestOnlyAllowedPackagesStampInternalOrigin(t *testing.T) {
	root := moduleRoot(t)

	refs := map[string][]string{} // package dir -> file:line
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			// A file that does not parse is not this test's problem; the build
			// will say so far more clearly than a failure here would.
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		if rel == filepath.Join("component", "auth", "call_origin.go") {
			return nil // the definition itself
		}

		dir := filepath.ToSlash(filepath.Dir(rel))
		record := func(pos token.Pos) {
			refs[dir] = append(refs[dir],
				filepath.ToSlash(rel)+":"+strconv.Itoa(fset.Position(pos).Line))
		}

		// Match a REFERENCE to the symbol anywhere, not only in call position.
		// Returning false on a matching SelectorExpr stops the walk descending
		// into its Sel, which would otherwise double-count as a bare Ident.
		ast.Inspect(f, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.SelectorExpr:
				if e.Sel != nil && e.Sel.Name == internalOriginFunc {
					record(e.Sel.Pos())
					return false
				}
			case *ast.Ident:
				if e.Name == internalOriginFunc {
					record(e.Pos())
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	if len(refs) == 0 {
		t.Fatal("found no ContextWithInternalOrigin references anywhere, which " +
			"means this test is not looking where it thinks it is -- a silent pass " +
			"here would be worse than a failure")
	}

	var offenders []string
	for dir := range refs {
		if _, ok := allowedInternalOriginCallers[dir]; !ok {
			offenders = append(offenders, dir)
		}
	}
	sort.Strings(offenders)

	for _, dir := range offenders {
		sort.Strings(refs[dir])
		t.Errorf("package %q references %s and is not on the allowlist:\n"+
			"    %s\n"+
			"  %s grants a call the right to reach @serverOnly constructs. Its doc "+
			"forbids calling it in a request handler on a request-derived context, "+
			"because that launders client origin into internal for everything "+
			"downstream.\n"+
			"  Taking it as a function value counts: assigning it to a variable and "+
			"calling through that is the same grant.\n"+
			"  If this package is genuinely server-side Go stamping a context it "+
			"controls, add it to allowedInternalOriginCallers in "+
			"component/auth/internal_origin_callers_test.go with the reason. If it "+
			"is a request handler, it is the bug this test exists to catch.",
			dir, internalOriginFunc, strings.Join(refs[dir], "\n    "), internalOriginFunc)
	}

	// An allowlist entry whose package no longer references the symbol is stale,
	// and a stale security allowlist is how the next real caller gets waved
	// through.
	for dir := range allowedInternalOriginCallers {
		if _, ok := refs[dir]; !ok {
			t.Errorf("allowlist entry %q no longer references %s; remove it so the "+
				"list keeps meaning what it says", dir, internalOriginFunc)
		}
	}
}

// moduleRoot walks up from this test's directory to the go.mod, so the test does
// not depend on where `go test` was invoked from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the test directory")
		}
		dir = parent
	}
}
