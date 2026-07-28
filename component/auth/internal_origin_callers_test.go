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
// handler stamps CLIENT while ~29 others do not -- and the answer to that half
// is that it does not matter: OriginClient is the zero value, so an unstamped
// handler is already untrusted, and the 14 internal stamps in the tree all sit
// at the point of calling Execute, downstream of a handler, never wrapping one.
// Stamping the other 29 would buy nothing.
//
// What DOES matter is the other direction. A single ContextWithInternalOrigin in
// the wrong place is a privilege grant, and the rule against it was a comment.
// This test is that rule.
//
// # Why AST and not grep
//
// #2888's review replaced a grep-based assertion with a registry-based one for
// the same reason this parses: a grep matches the identifier inside a string
// literal. component/language/annotations/registry.go names
// ContextWithInternalOrigin in an annotation's help text, which is not a call.
//
// # Why this catches more than the compiler would
//
// go/parser ignores build constraints, so this sees files no CI lane compiles.
// app/integrations_identity.go is `//go:build identity` and does call the
// helper; per #2903 the tagged lanes only `go build`, never `go test`, so a
// violation added to a tagged file would otherwise reach main unexamined. That
// is not hypothetical in this repo -- #2903 exists because a tagged test file
// had never run.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

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
	"component/automations": "the executor decides per-automation trust and " +
		"checkpoints run as the system; #2888 established the trust rule here",
	"app": "integrations_identity wires a server-side identity lookup",
	"integrations/dailyspace": "a server-side integration acting as the system",
	"integrations/agent/worker": "the agent worker is server-side Go with no " +
		"inbound request context",

	// DECLARED EXCEPTION, not an endorsement. component/identity/admin/handlers.go
	// stamps internal on an HTTP-request context and passes a caller-supplied
	// userId to userByIdSystem -- literally the shape the doc forbids. It is gated
	// at the admin HTTP layer, so it is not a live hole, and #2889 says as much.
	//
	// It is listed so this test passes on a tree that contains it, rather than
	// being written to pass by pretending it is absent. If that HTTP gate is ever
	// loosened, nothing here will notice -- which is the point of recording it
	// where someone auditing origin will read it.
	"component/identity/admin": "GATED EXCEPTION: stamps internal on a " +
		"request-derived ctx, contrary to the doc, relying on the admin HTTP " +
		"layer as its gate (memql#2889)",
}

func TestOnlyAllowedPackagesStampInternalOrigin(t *testing.T) {
	root := moduleRoot(t)

	callers := map[string][]string{} // package dir -> file:line
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "sdk":
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

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if !isInternalOriginCall(call.Fun) {
				return true
			}
			dir := filepath.ToSlash(filepath.Dir(rel))
			pos := fset.Position(call.Pos())
			callers[dir] = append(callers[dir],
				filepath.ToSlash(rel)+":"+itoa(pos.Line))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	if len(callers) == 0 {
		t.Fatal("found no ContextWithInternalOrigin callers anywhere, which means " +
			"this test is not looking where it thinks it is -- a silent pass here " +
			"would be worse than a failure")
	}

	var offenders []string
	for dir := range callers {
		if _, ok := allowedInternalOriginCallers[dir]; !ok {
			offenders = append(offenders, dir)
		}
	}
	sort.Strings(offenders)

	for _, dir := range offenders {
		sort.Strings(callers[dir])
		t.Errorf("package %q stamps internal origin and is not on the allowlist:\n"+
			"    %s\n"+
			"  ContextWithInternalOrigin grants a call the right to reach "+
			"@serverOnly constructs. Its doc forbids calling it in a request "+
			"handler on a request-derived context, because that launders client "+
			"origin into internal for everything downstream.\n"+
			"  If this package is genuinely server-side Go stamping a context it "+
			"controls, add it to allowedInternalOriginCallers in "+
			"component/auth/internal_origin_callers_test.go with the reason. If it "+
			"is a request handler, it is the bug this test exists to catch.",
			dir, strings.Join(callers[dir], "\n    "))
	}

	// An allowlist entry whose package no longer stamps anything is stale, and a
	// stale security allowlist is how the next real caller gets waved through.
	for dir := range allowedInternalOriginCallers {
		if _, ok := callers[dir]; !ok {
			t.Errorf("allowlist entry %q no longer stamps internal origin; remove "+
				"it so the list keeps meaning what it says", dir)
		}
	}
}

// isInternalOriginCall reports whether fun names ContextWithInternalOrigin,
// either qualified (auth.ContextWithInternalOrigin, or any alias) or bare from
// inside package auth.
func isInternalOriginCall(fun ast.Expr) bool {
	switch e := fun.(type) {
	case *ast.SelectorExpr:
		return e.Sel != nil && e.Sel.Name == "ContextWithInternalOrigin"
	case *ast.Ident:
		return e.Name == "ContextWithInternalOrigin"
	}
	return false
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

// itoa avoids pulling strconv in for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
