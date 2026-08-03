package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Keeps HTTP route registration going through handleRoute/handleRouteFunc, so
// the identity binary's boot check sees the whole surface (znasllc-io/memql#2939).
//
// A bare `a.mux.HandleFunc("POST /internal/whatever", ...)` is invisible to
// a.registeredRoutes, so it would be reachable unauthenticated on identity with
// nothing failing anywhere. That is exactly the silence #2939 exists to remove,
// and it is a one-line addition that looks entirely ordinary in review.
//
// Two call sites legitimately hand the mux to another package and cannot be
// captured this way. They are named explicitly rather than pattern-matched, so
// a THIRD one is a test failure and a deliberate decision:
//
//   - server.RegisterConceptsEndpoint -- mounts /api/concepts*, already in
//     PublicPaths().
//
//   - identity Service.RegisterRoutes -- identity's OWN auth surface
//     (magic-link, OAuth, JWKS, discovery, admin). The admin routes carry their
//     own cluster-role gate (see component/identity/admin, memql#2934); the
//     auth, JWKS and discovery endpoints are in PublicPaths() -- the last via
//     IdentityDiscoveryPaths() -- and must be reachable unauthenticated to
//     function.
//
//     This rationale used to read "the auth and JWKS endpoints are in
//     PublicPaths()". True of jwks.json, false of
//     /.well-known/memql-config.json and
//     /.well-known/oauth-authorization-server: both are mounted here, both are
//     unauthenticated by design, and neither appeared in any declaration. An
//     exemption is only as good as the reason written beside it, so the reason
//     was made true rather than the exemption quietly narrowed.
var muxHandoffCallees = map[string]bool{
	"RegisterConceptsEndpoint": true,
	"RegisterRoutes":           true,
	// server.WithBaseRouter -- hands the mux to the HTTP server as the base
	// router, which is how HandlerWithOptions mounts the OpenAPI contract onto
	// it. Those five routes are covered separately by server.ContractRoutes(),
	// itself verified against the real registration.
	"WithBaseRouter": true,
}

func TestRoutesRegisterThroughRecordingHelpers(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read app dir: %v", err)
	}

	var offenders []string
	var inspected int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			continue // build-tagged files that do not parse standalone are not this gate's business
		}
		inspected++
		enclosingFunc := ""
		ast.Inspect(file, func(n ast.Node) bool {
			if fd, ok := n.(*ast.FuncDecl); ok {
				enclosingFunc = fd.Name.Name
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
				return true
			}
			// Match `<something>.mux.Handle...`; the receiver is a.mux.
			recv, ok := sel.X.(*ast.SelectorExpr)
			if !ok || recv.Sel.Name != "mux" {
				return true
			}
			// Only the helper bodies may call the mux directly -- exempting
			// app.go wholesale would let any other function in that file
			// register a route the boot check never sees.
			if name == "app.go" && enclosingFunc != "" &&
				(enclosingFunc == "handleRoute" || enclosingFunc == "handleRouteFunc") {
				return true
			}
			offenders = append(offenders, fset.Position(call.Pos()).String())
			return true
		})
	}

	if inspected == 0 {
		t.Fatal("parsed no app source files -- this gate has stopped resolving them " +
			"and would now pass vacuously")
	}
	for _, o := range offenders {
		t.Errorf("route registered directly on a.mux at %s. Use a.handleRoute / "+
			"a.handleRouteFunc instead: a direct registration is invisible to "+
			"a.registeredRoutes, so on the identity binary it is reachable "+
			"unauthenticated with nothing failing (memql#2939).", o)
	}
}

// The two hand-offs that pass the mux to another package are enumerated, so a
// third one is a deliberate decision rather than an oversight -- routes mounted
// that way are equally invisible to the boot check.
func TestMuxHandoffsAreEnumerated(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read app dir: %v", err)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			for _, arg := range call.Args {
				sel, ok := arg.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "mux" {
					continue
				}
				callee := ""
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					callee = fn.Sel.Name
				case *ast.Ident:
					callee = fn.Name
				}
				if callee == "" || muxHandoffCallees[callee] {
					continue
				}
				t.Errorf("%s hands a.mux to %s() at %s, which may register routes the "+
					"boot check cannot see. Add it to muxHandoffCallees with a note on "+
					"what it mounts and why that is safe unauthenticated, or register "+
					"through a.handleRoute (memql#2939).",
					name, callee, fset.Position(call.Pos()).String())
			}
			return true
		})
	}
}
