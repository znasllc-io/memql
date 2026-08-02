package identity

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/server"
)

// Every /.well-known/ route identity mounts must be DECLARED, not merely
// unauthenticated-by-design (memql#2939).
//
// These routes reach the mux through Service.RegisterRoutes, one of the
// enumerated handoffs in app/mux_registration_test.go, so the identity
// binary's boot assertion never sees them -- the handoff is exempt by
// construction. That exemption is justified in writing by the claim that
// identity's well-knowns are in PublicPaths(). It was true of jwks.json and
// false of /.well-known/memql-config.json and
// /.well-known/oauth-authorization-server: both mounted here, both reachable
// without credentials, and named in no declaration anywhere.
//
// The claim is now true, and this is what keeps it true. Adding a fourth
// well-known route without declaring it fails here.
//
// Read from the SOURCE rather than a hand-kept list, for the same reason
// ContractRoutes() is verified against the real registration: a list that is
// maintained by remembering to maintain it is the thing that failed.
// RegisterRoutes takes a concrete *http.ServeMux, which cannot be intercepted
// with a recorder, and ServeMux exposes no way to enumerate what was
// registered -- so the registration is read statically, the same technique
// app/mux_registration_test.go already uses to police direct mux use.
//
// Scope is deliberately the well-knowns only. The mounter surfaces
// (magic-link, OAuth, /me, /setup, /legal, admin) are a different question
// with a different answer -- the admin routes carry their own cluster-role
// gate, the auth routes are covered by AuthPaths() -- and folding them in
// here would make this test about everything and therefore about nothing.
func TestWellKnownRoutesAreDeclaredPublic(t *testing.T) {
	mounted := wellKnownRoutesMountedBy(t, "identity.go", "RegisterRoutes")
	if len(mounted) == 0 {
		t.Fatal("parsed no /.well-known/ registrations out of RegisterRoutes -- the parse " +
			"has drifted from the source and this test is no longer checking anything")
	}

	declared := map[string]bool{}
	for _, p := range server.PublicPaths() {
		declared[p] = true
	}

	for _, route := range mounted {
		if !declared[route] {
			t.Errorf("identity mounts %q with no auth middleware in front of it, and it is in "+
				"no declaration: add it to server.IdentityDiscoveryPaths() (or another "+
				"PublicPaths() contributor) so the unauthenticated surface stays declared "+
				"rather than incidental (#2939)", route)
		}
	}
}

// wellKnownRoutesMountedBy returns the /.well-known/ path of every
// mux.Handle("<METHOD> <path>", ...) call inside the named function.
func wellKnownRoutesMountedBy(t *testing.T, filename, funcName string) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	var routes []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != funcName {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			pattern, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			// Patterns are "<METHOD> <path>"; the path is what is declared.
			if i := strings.LastIndex(pattern, " "); i >= 0 {
				pattern = pattern[i+1:]
			}
			if strings.HasPrefix(pattern, "/.well-known/") {
				routes = append(routes, pattern)
			}
			return true
		})
	}
	return routes
}
