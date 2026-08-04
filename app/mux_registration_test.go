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

// TestRoutesRegisterThroughRecordingHelpers checks every REFERENCE to the mux
// field, not the shape of the call that uses it (memql#3004).
//
// # Why the rule was inverted
//
// The original gate matched the literal `<x>.mux.Handle{,Func}(` and the
// original hand-off gate matched `a.mux` as a bare positional argument. Both
// were syntactic, and memql#3004 measured five ways past them, each of which
// registers a route invisibly and leaves the suite green:
//
//	m := a.mux;  m.HandleFunc(...)              // receiver is an Ident
//	h := a.mux.HandleFunc;  h(...)              // not a CallExpr.Fun selector
//	register(routerHolder{Router: a.mux})       // composite literal
//	register([]*http.ServeMux{a.mux})           // slice literal
//	holder.Router = a.mux                       // assignment, not a call
//	func (a *App) Mux() *http.ServeMux { ... }  // getter
//
// Enumerating those shapes is unbounded: each new spelling is a new arm, and
// the gate is only ever as complete as the last person's imagination. So the
// question changed from "does this call look like a registration" to **"is
// this use of the mux one of the handful we sanction"** -- and everything
// else fails, whatever it looks like.
//
// That is closed rather than open-ended because every one of those escapes has
// to NAME the field. A getter is caught by its own body; an alias is caught at
// the point it is taken. There is no way to obtain the mux without a
// `.mux` selector somewhere in this package, which is what makes a
// reference rule exact where a shape rule was a guess.
//
// The issue also floated making App.mux a private wrapper type whose only
// methods record. That is stronger -- it makes the escapes unrepresentable
// rather than merely detected -- but it is a wider refactor, because
// identity.Service.RegisterRoutes and server.RegisterConceptsEndpoint take
// *http.ServeMux concretely. This gate is the same guarantee at a fraction of
// the blast radius, and it does not preclude the wrapper later.
func TestRoutesRegisterThroughRecordingHelpers(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read app dir: %v", err)
	}

	var offenders []string
	var inspected, references int
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
		bad, seen := unsanctionedMuxRefs(fset, name, file)
		references += seen
		offenders = append(offenders, bad...)
	}

	if inspected == 0 {
		t.Fatal("parsed no app source files -- this gate has stopped resolving them " +
			"and would now pass vacuously")
	}
	// A reference floor as well as a file floor: the file loop could resolve
	// every file while the selector match silently stopped finding anything,
	// which reads identically to a compliant package.
	if references == 0 {
		t.Fatal("found no references to the mux field at all -- the selector match has stopped " +
			"resolving and this gate would now pass vacuously (memql#3004)")
	}
	for _, o := range offenders {
		t.Errorf("unsanctioned use of a.mux at %s.\n"+
			"Every route must reach the mux through a.handleRoute / a.handleRouteFunc, which is "+
			"what records it in a.registeredRoutes -- anything else is invisible to the identity "+
			"binary's boot check and therefore reachable unauthenticated with nothing failing "+
			"(memql#2939).\n"+
			"This gate judges REFERENCES rather than call shapes, so aliasing the mux, taking a "+
			"method value off it, putting it in a composite literal, assigning it to a field or "+
			"returning it from a getter are all equally rejected -- that is memql#3004. If the "+
			"use is legitimate, hand it to an enumerated callee in muxHandoffCallees with a note "+
			"on what it mounts and why that is safe unauthenticated.", o)
	}
}

// unsanctionedMuxRefs returns the positions of every unsanctioned reference to
// the mux field in one parsed file, plus how many references it saw at all.
//
// ONE definition, called by the sweep above and by the fixture test below.
// That is not a style point: this repo has twice shipped a pin test carrying
// its own copy of a pattern, which pins a string existing only inside itself --
// sabotaging the sweep left the pin, and the whole suite, green (memql#3044).
// Here the risk is sharper still, because the fixture is the ONLY thing that
// proves the five documented escapes are caught; a fixture running different
// code from the sweep would prove nothing about the sweep.
func unsanctionedMuxRefs(fset *token.FileSet, name string, file *ast.File) (offenders []string, references int) {
	// Parent links, so a reference can be judged by the context it sits in.
	// ast.Inspect alone gives no parent, and the whole point here is that the
	// SAME expression `a.mux` is fine as an argument to an enumerated callee
	// and not fine anywhere else.
	parent := map[ast.Node]ast.Node{}
	funcOf := map[ast.Node]*ast.FuncDecl{}
	var enclosing []*ast.FuncDecl
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			if len(stack) > 0 {
				if fd, ok := stack[len(stack)-1].(*ast.FuncDecl); ok &&
					len(enclosing) > 0 && enclosing[len(enclosing)-1] == fd {
					enclosing = enclosing[:len(enclosing)-1]
				}
				stack = stack[:len(stack)-1]
			}
			return false
		}
		if len(stack) > 0 {
			parent[n] = stack[len(stack)-1]
		}
		if len(enclosing) > 0 {
			funcOf[n] = enclosing[len(enclosing)-1]
		}
		if fd, ok := n.(*ast.FuncDecl); ok {
			enclosing = append(enclosing, fd)
			funcOf[n] = fd
		}
		stack = append(stack, n)
		return true
	})

	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "mux" {
			return true
		}
		references++
		if sanctionedMuxUse(name, sel, parent, funcOf) == "" {
			offenders = append(offenders, fset.Position(sel.Pos()).String())
		}
		return true
	})
	return offenders, references
}

// isNewServeMuxCall reports whether e is literally `http.NewServeMux()`.
//
// Narrow on purpose. Anything else assigned to the mux field -- a helper's
// return, a parameter, a package-level var -- is a mux this gate did not watch
// being built, and its routes were registered out of sight. The point is not
// that such a helper is necessarily malicious; it is that the gate cannot see
// inside it, so sanctioning it would be sanctioning an unknown.
func isNewServeMuxCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "NewServeMux" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
}

// sanctionedMuxUse returns a non-empty reason when this reference to the mux
// field is one of the uses the package sanctions, and "" when it is not.
//
// The list is deliberately short, and each entry is a real use in the tree
// today. Adding an entry is a decision about the unauthenticated surface, which
// is the point of making it explicit.
func sanctionedMuxUse(
	file string,
	sel *ast.SelectorExpr,
	parent map[ast.Node]ast.Node,
	funcOf map[ast.Node]*ast.FuncDecl,
) string {
	// 1. The recording helpers themselves. Scoped to the two functions by
	//    name, not to app.go wholesale -- exempting the file would let any
	//    other function in it register a route the boot check never sees.
	if fd := funcOf[sel]; fd != nil && file == "app.go" &&
		(fd.Name.Name == "handleRoute" || fd.Name.Name == "handleRouteFunc") {
		return "recording helper body"
	}

	p := parent[sel]

	// 2. Construction: `a.mux = http.NewServeMux()`, and ONLY that.
	//
	//    Deliberately checks Lhs only: `holder.Router = a.mux` puts the mux on
	//    the RIGHT and is exactly escape 3, so it must NOT pass here.
	//
	//    The RHS check is memql#3004's fourth escape, found reviewing this
	//    gate. The rule used to sanction any assignment whose target was the
	//    mux, reasoning that "being the TARGET of an assignment cannot leak
	//    it". True of LEAKING and irrelevant to the actual risk, which is
	//    INJECTING: a mux arrives pre-loaded and its routes were registered
	//    somewhere this gate never looks.
	//
	//        m := http.NewServeMux()
	//        m.HandleFunc("POST /internal/whatever", h)  // no `.mux` selector
	//        a.mux = m
	//
	//    scanned clean, and so did `a.mux = buildMux()`. config.go already
	//    reads `a.mux = http.NewServeMux()`, so the evasion is one expression
	//    away and reads as a refactor -- escape 2 again, one syntactic layer
	//    up, which is the shape #3004 was filed about.
	if as, ok := p.(*ast.AssignStmt); ok {
		for i, lhs := range as.Lhs {
			if lhs != ast.Expr(sel) {
				continue
			}
			// Paired RHS. A multi-value assign (a, b = f()) has one Rhs for
			// several Lhs and cannot be a mux construction, so it falls
			// through to offender rather than being waved past.
			if len(as.Rhs) != len(as.Lhs) {
				return ""
			}
			if isNewServeMuxCall(as.Rhs[i]) {
				return "assignment target (constructed inline)"
			}
			return ""
		}
	}

	// 3. A nil comparison -- `if a.mux == nil` guards ordering in
	//    transportIdentity. Reads the pointer, cannot hand it anywhere.
	if bin, ok := p.(*ast.BinaryExpr); ok {
		other := bin.X
		if other == ast.Expr(sel) {
			other = bin.Y
		}
		if id, ok := other.(*ast.Ident); ok && id.Name == "nil" {
			return "nil comparison"
		}
	}

	// 4. A positional argument to an enumerated hand-off callee. This is the
	//    one that actually passes the mux out of the package, and it is why
	//    muxHandoffCallees exists: each entry is a written decision about a
	//    surface the boot check cannot see.
	if call, ok := p.(*ast.CallExpr); ok {
		for _, arg := range call.Args {
			if arg != ast.Expr(sel) {
				continue
			}
			// A BARE identifier is never a hand-off out of this package --
			// it is a function defined right here, whose body this gate does
			// not read. Sanctioning it on the name alone let
			//
			//     func RegisterRoutes(m *http.ServeMux) { m.HandleFunc(...) }
			//     RegisterRoutes(a.mux)
			//
			// scan clean: two ordinary-looking lines mounting an undeclared
			// route on the identity binary (memql#3004, found reviewing this
			// gate). Requiring a qualifier keeps the three real hand-offs --
			// server.WithBaseRouter, server.RegisterConceptsEndpoint,
			// svc.RegisterRoutes -- and rejects the in-package impostor.
			//
			// HONEST LIMIT: this matches the terminal identifier of a
			// qualified call, so a method of the same NAME on some other type
			// still passes. Closing that needs go/types to resolve the
			// receiver, which is a bigger change than this gate carries today;
			// the residue is recorded rather than implied.
			callee := ""
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				callee = fn.Sel.Name
			case *ast.Ident:
				// Deliberately NOT sanctioned -- see the note above.
				callee = ""
			}
			if muxHandoffCallees[callee] {
				return "enumerated hand-off"
			}
			// A call with an UNKNOWN callee is not sanctioned, and falls
			// through to the failure -- which is the old hand-off gate's job,
			// now folded in here so both live under one rule.
			return ""
		}
	}

	// 5. The field declaration itself (`mux *http.ServeMux`) is a Field, not a
	//    SelectorExpr, so it never reaches this function. Nothing to allow.

	return ""
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

// The gate must actually catch the escapes memql#3004 documented. Without
// this, the reference rule could be wrong in a way that sanctions everything
// and the sweep above would report clean forever -- the silent-disable shape
// every gate in this repo is now written to avoid.
//
// It runs unsanctionedMuxRefs, the SAME function the sweep calls, over
// synthetic sources. Each `bad` case is one of the five spellings the issue
// measured as passing the old shape-matching gates.
func TestMuxGateCatchesTheDocumentedEscapes(t *testing.T) {
	const hdr = "package app\n\nimport \"net/http\"\n\ntype App struct{ mux *http.ServeMux }\n\n"

	bad := []struct{ name, src string }{
		{
			// Escape 2a: the receiver is an Ident, not a SelectorExpr, so the
			// old `<x>.mux.Handle(` match never fired.
			name: "aliased into a local then registered on",
			src:  "func (a *App) f() { m := a.mux; m.HandleFunc(\"POST /x\", nil) }",
		},
		{
			// Escape 2b: a method VALUE. There is no CallExpr.Fun selector at
			// the registration site at all.
			name: "method value taken off the mux",
			src:  "func (a *App) f() { h := a.mux.HandleFunc; h(\"POST /x\", nil) }",
		},
		{
			// Escape 3a: composite literal -- the mux is not a positional arg,
			// so the old hand-off gate's arg loop never saw it.
			name: "handed over inside a composite literal",
			src:  "type holder struct{ Router *http.ServeMux }\nfunc (a *App) f() { register(holder{Router: a.mux}) }\nfunc register(any) {}",
		},
		{
			// Escape 3b: slice literal, same reason.
			name: "handed over inside a slice literal",
			src:  "func (a *App) f() { register([]*http.ServeMux{a.mux}) }\nfunc register(any) {}",
		},
		{
			// Escape 3c: assignment, not a call. The mux is on the RIGHT, which
			// is why the "assignment target" sanction checks Lhs only.
			name: "assigned onto another struct's field",
			src:  "type holder struct{ Router *http.ServeMux }\nfunc (a *App) f(h *holder) { h.Router = a.mux }",
		},
		{
			// Escape 3d: a getter. Caught by its own body -- there is no way to
			// hand the mux out without naming it here.
			name: "returned from a getter",
			src:  "func (a *App) Mux() *http.ServeMux { return a.mux }",
		},
		{
			// Not in the issue, and the reason the callee list is a closed
			// allowlist rather than "any call": handing the mux to an
			// un-enumerated function is the original defect with a new name.
			name: "handed to a callee that is not enumerated",
			src:  "func (a *App) f() { mountEverything(a.mux) }\nfunc mountEverything(*http.ServeMux) {}",
		},
		{
			// memql#3004's fourth escape, and this fixture used to sit in the
			// ALLOWED list -- the gate encoded the hole as correct behaviour.
			//
			// An enumerated NAME with no package qualifier is a function
			// defined in this package, whose body the gate never reads. Two
			// ordinary-looking lines then mount an undeclared route on the
			// identity binary. All three real hand-offs are qualified
			// (server.WithBaseRouter, server.RegisterConceptsEndpoint,
			// svc.RegisterRoutes), so requiring the qualifier costs nothing
			// and closes this.
			name: "enumerated callee name, but defined in-package (no qualifier)",
			src:  "func (a *App) f() { WithBaseRouter(a.mux) }\nfunc WithBaseRouter(m *http.ServeMux) { m.HandleFunc(\"POST /pwn\", nil) }",
		},
		{
			// The plain case the ORIGINAL gate caught, kept so the rewrite
			// cannot regress what it replaced.
			name: "registered directly, outside the helpers",
			src:  "func (a *App) f() { a.mux.HandleFunc(\"POST /x\", nil) }",
		},
	}

	for _, tc := range bad {
		t.Run("caught: "+tc.name, func(t *testing.T) {
			offenders, refs := parseAndScan(t, "transport.go", hdr+tc.src)
			if refs == 0 {
				t.Fatal("the fixture contains no mux reference at all, so it proves nothing")
			}
			if len(offenders) == 0 {
				t.Errorf("the gate accepted this, so a route could be registered invisibly and "+
					"served unauthenticated on identity with nothing failing (memql#3004):\n%s",
					tc.src)
			}
		})
	}

	// The sanctioned uses must NOT fire, or the gate gets suppressed and then
	// stops catching the real case.
	ok := []struct{ name, file, src string }{
		{
			name: "construction",
			file: "config.go",
			src:  "func (a *App) f() { a.mux = http.NewServeMux() }",
		},
		{
			name: "nil guard",
			file: "integrations_identity.go",
			src:  "func (a *App) f() bool { return a.mux == nil }",
		},
		{
			name: "enumerated hand-off, package-qualified callee",
			file: "database.go",
			src:  "func (a *App) f() { srv.RegisterConceptsEndpoint(a.mux, nil) }\nvar srv struct{ RegisterConceptsEndpoint func(*http.ServeMux, any) }",
		},
		{
			name: "the recording helper itself",
			file: "app.go",
			src:  "func (a *App) handleRoute(p string, h http.Handler) { a.mux.Handle(p, h) }",
		},
		{
			name: "the other recording helper",
			file: "app.go",
			src:  "func (a *App) handleRouteFunc(p string, h http.HandlerFunc) { a.mux.HandleFunc(p, h) }",
		},
	}

	for _, tc := range ok {
		t.Run("allowed: "+tc.name, func(t *testing.T) {
			offenders, refs := parseAndScan(t, tc.file, hdr+tc.src)
			if refs == 0 {
				t.Fatal("the fixture contains no mux reference at all, so it proves nothing")
			}
			if len(offenders) != 0 {
				t.Errorf("the gate fired on a legitimate use, which is how a gate gets "+
					"suppressed and then stops catching the real case:\n%s", tc.src)
			}
		})
	}

	// The helper carve-out is scoped to two FUNCTIONS, not to app.go. Pinned
	// separately because widening it to the file is the natural-looking
	// simplification, and it would let anything else in app.go register
	// invisibly while every case above stayed green.
	t.Run("the app.go carve-out is per-function, not per-file", func(t *testing.T) {
		offenders, _ := parseAndScan(t, "app.go",
			hdr+"func (a *App) somethingElse() { a.mux.HandleFunc(\"POST /x\", nil) }")
		if len(offenders) == 0 {
			t.Error("a non-helper function in app.go registered directly and was accepted. " +
				"The carve-out must name handleRoute / handleRouteFunc, not the file.")
		}
	})
}

func parseAndScan(t *testing.T, name, src string) ([]string, int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return unsanctionedMuxRefs(fset, name, f)
}

// The middleware channel is a route source too, and memql#3004's first escape
// was about the CLASS rather than the one path.
//
// `POST /memql/query` was invisible because it is served from middleware ahead
// of the mux: it reaches neither a.registeredRoutes nor ContractRoutes(), so
// it appeared in no declaration and failed no check. The fix records it via
// a.middlewareRoutes -- which closes that instance. It does not, on its own,
// stop the NEXT middleware being equally invisible: `a.middlewares = append(...)`
// appears at five sites and exactly one of them declares any path.
//
// So this gate makes the channel explicit. Every middleware registration is
// either known to serve no request path of its own, or must declare what it
// claims. Adding a middleware that intercepts a path and saying nothing here
// fails, which is the property #3004 asked for and the difference between
// fixing an instance and closing a class.
func TestEveryMiddlewareRegistrationIsAccountedFor(t *testing.T) {
	// site -> why it serves no path, or how it declares the ones it does.
	//
	// Keyed by the middleware CONSTRUCTOR rather than by file:line, so the
	// entry survives the code moving and breaks when the thing itself changes.
	type middlewareNote struct {
		why string
		// claimsPaths marks a middleware that serves request paths of its own.
		// Those are the dangerous ones: they run AHEAD of the mux, so the path
		// reaches neither a.registeredRoutes nor ContractRoutes(). Setting this
		// requires the registering function to also feed a.middlewareRoutes,
		// which is asserted below.
		claimsPaths bool
	}
	accounted := map[string]middlewareNote{
		"PanicRecoveryMiddleware":            {why: "wraps the chain; serves no path of its own"},
		"SecurityHeadersMiddleware":          {why: "sets response headers; serves no path of its own"},
		"authMiddleware":                     {why: "the verifier; gates paths, claims none"},
		"NewSessionRevocationHTTPMiddleware": {why: "inspects the token on every request; claims no path"},
		"Middleware":                         {why: "the gRPC gateway -- CLAIMS POST /memql/query via memqlgrpc.InterceptedPaths()", claimsPaths: true},
	}
	// Functions that feed a.middlewareRoutes, by name. Collected in the same
	// walk so a claimsPaths middleware can be required to sit in one.
	declaring := map[string]bool{}
	// Function name -> the claims-paths middlewares registered inside it.
	claimingIn := map[string][]string{}

	fset := token.NewFileSet()
	found := map[string]bool{}
	var unaccounted []string

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read app dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(fset, name, nil, 0)
		if parseErr != nil {
			continue // build-tagged files that do not parse standalone
		}
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			sel, ok := as.Lhs[0].(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "middlewareRoutes" {
				declaring[enclosingFuncName(f, as.Pos())] = true
				return true
			}
			if !ok || sel.Sel.Name != "middlewares" {
				return true
			}
			call, ok := as.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, isIdent := call.Fun.(*ast.Ident); !isIdent || id.Name != "append" {
				return true
			}
			for _, arg := range call.Args[1:] {
				ctor := middlewareConstructorName(arg)
				if ctor == "" {
					unaccounted = append(unaccounted,
						fset.Position(arg.Pos()).String()+" (could not name the middleware)")
					continue
				}
				found[ctor] = true
				note, ok := accounted[ctor]
				if !ok {
					unaccounted = append(unaccounted,
						fset.Position(arg.Pos()).String()+" -> "+ctor)
					continue
				}
				if note.claimsPaths {
					claimingIn[enclosingFuncName(f, arg.Pos())] = append(
						claimingIn[enclosingFuncName(f, arg.Pos())], ctor)
				}
			}
			return true
		})
	}

	for _, u := range unaccounted {
		t.Errorf("middleware registered at %s is not accounted for.\n"+
			"A middleware runs AHEAD of the mux, so any path it claims reaches neither "+
			"a.registeredRoutes nor ContractRoutes() and is invisible to the boot assertion -- "+
			"which is exactly how POST /memql/query came to be served on the verifier-less "+
			"identity binary while appearing in no declaration at all (memql#3004).\n"+
			"Add an entry to `accounted` saying either that it serves no path of its own, or "+
			"which declaration carries the paths it claims. If it claims paths, they must also "+
			"be appended to a.middlewareRoutes, or the boot check will not see them.", u)
	}

	// A middleware that claims paths must be registered in a function that
	// also feeds a.middlewareRoutes.
	//
	// This is the assertion the collector line had none of. Removing
	// `a.middlewareRoutes = append(..., memqlgrpc.InterceptedPaths()...)` from
	// transportBase left every test green -- including under the identity tag,
	// the binary #3004 is about -- because the only test of the field builds
	// `&App{middlewareRoutes: ...}` by hand and asserts the echo. That pins the
	// assembly and says nothing about whether anything populates it, which is
	// the #3044 shape: a pin carrying its own copy of the thing it pins.
	for fn, ctors := range claimingIn {
		if declaring[fn] {
			continue
		}
		t.Errorf("%s registers %v, which claims request paths, but never appends to "+
			"a.middlewareRoutes.\nA middleware runs ahead of the mux, so a path it claims is "+
			"invisible to the boot assertion unless it is declared -- which is exactly how "+
			"POST /memql/query came to be served on the verifier-less identity binary while "+
			"appearing in no declaration (memql#3004). Feed its paths into a.middlewareRoutes "+
			"in this function, or mark it claimsPaths:false and say why it serves none.",
			fn, ctors)
	}

	// Anti-vacuity, both directions.
	if len(found) == 0 {
		t.Fatal("no middleware registrations found at all -- the scan shape changed and this " +
			"gate has silently stopped protecting anything")
	}
	for ctor := range accounted {
		if !found[ctor] {
			t.Errorf("%q is in `accounted` but is registered nowhere. If the middleware was "+
				"removed, drop the entry in the same change: a stale entry pre-authorises a "+
				"name that could come back meaning something else.", ctor)
		}
	}
}

// enclosingFuncName returns the name of the FuncDecl containing pos.
func enclosingFuncName(f *ast.File, pos token.Pos) string {
	name := "<file scope>"
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if pos >= fd.Pos() && pos <= fd.End() {
			return fd.Name.Name
		}
	}
	return name
}

// middlewareConstructorName returns the function name a middleware expression
// is built from, or "" when it cannot be named. Handles `pkg.New(...)`,
// `New(...)`, `x.Method()` and a bare identifier (a middleware built earlier
// and assigned to a variable, as authMiddleware is).
func middlewareConstructorName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.CallExpr:
		return middlewareConstructorName(v.Fun)
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.Ident:
		return v.Name
	}
	return ""
}
