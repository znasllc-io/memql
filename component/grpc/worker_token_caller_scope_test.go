// Asserts the precondition the worker-token internal-origin stamp rests on
// (znasllc-io/memql#3063).
//
// component/identity/workertoken.ListForUser stamps internal origin so it can
// read workerTokensForUser, which is @serverOnly because it projects
// identityFull -- keyHash, registeredBy, lastSeenAt, lastConnectedFromIP --
// behind a filter keyed on a caller-supplied userId with NO actor check. The
// root call_origin_conformance_test.go allowlists component/identity/workertoken
// for that stamp, which by design makes that gate PASS.
//
// But read the allowlist's own definition of who belongs in it: "boot,
// migration, reconciliation, or a system query on behalf of no caller. None of
// them is a request handler." ListForUser is reached from exactly one place,
// and it is a request handler -- handleRevokeWorkerToken, on
// s.stream.Context(). That is the REQUEST-DERIVED stamp memql#2989 refused and
// test/dslconformance/server_only_parsed_test.go names as refuted. Only component/identity/admin
// carries that exception today, and it was made to earn it with
// component/identity/admin/route_gate_test.go (memql#2934).
//
// This is the same missing link. The exception is sound ONLY because the
// handler passes caller.Subject -- the verified token's own subject -- rather
// than a field off the request payload. Nothing asserted those two facts were
// connected: hand the same call an attacker-controlled id and another user's
// worker-token digest and last-connect IP are back on the wire, with the
// annotation, the allowlist gate, and the stamp test all still green. That was
// measured during the #3072 review, not theorised:
//
//	store.ListForUser(ctx, strings.TrimSpace(msg.GetOwnerUserId()))
//	=> ok github.com/znasllc-io/memql, ok .../dsl, ok .../workertoken
//
// So: pin the argument. Every call into workertoken's ListForUser must pass an
// identifier assigned, in the same function, from the authenticated caller's
// Subject. A payload field, a literal, or an unresolvable expression fails
// here -- which is the failure the mutation above should have produced.
//
// Scoped to the worker-token store deliberately. Other packages expose a
// ListForUser (pat, badge) whose queries are not @serverOnly and whose reads do
// not stamp internal origin, so they carry neither the exposure nor the
// exception.
package memql

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const workerTokenPkgPath = "component/identity/workertoken"

// callerSubjectField is the AccessContext field naming the authenticated
// subject. An argument is caller-derived iff it traces back to this.
const callerSubjectField = "Subject"

func TestWorkerTokenListForUserIsAlwaysCallerScoped(t *testing.T) {
	root, err := repoRootFromGRPC()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	fset := token.NewFileSet()
	sites := 0

	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "sdk":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// parser.ParseFile ignores build constraints, so a call added under
		// any tag -- including tags no CI lane runs -- is still seen.
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // not our job to police unparseable files
		}
		if !importsWorkerTokenStore(f) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		sites += checkListForUserCallsInFile(t, fset, f, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// A scan that sees nothing asserts nothing. The revoke ownership check is
	// the known call site; if it is gone, this test must be re-aimed rather
	// than left silently passing over an empty set.
	if sites == 0 {
		t.Fatal("found no workertoken ListForUser call sites. Either the revoke ownership " +
			"check moved and this precondition test no longer covers the internal-origin " +
			"stamp it was written for (memql#3063), or the detection below stopped matching. " +
			"Re-aim it; do not delete it -- the allowlist entry in " +
			"call_origin_conformance_test.go is what it protects.")
	}
}

// checkListForUserCallsInFile reports how many workertoken ListForUser call
// sites it examined, failing t for each one whose userId argument is not
// caller-derived.
func checkListForUserCallsInFile(t *testing.T, fset *token.FileSet, f *ast.File, rel string) int {
	t.Helper()
	found := 0

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		// Which locals in this function hold a workertoken.Store, and which
		// hold a caller-derived subject. Both are function-scoped on purpose:
		// a subject resolved in some other function is not evidence about
		// this call.
		stores := workerTokenStoreLocals(fn)
		subjects := callerSubjectLocals(fn)

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ListForUser" {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok || !stores[recv.Name] {
				return true // some other package's ListForUser
			}
			found++

			pos := fset.Position(call.Pos())
			where := rel + ":" + itoa(pos.Line)

			if len(call.Args) != 2 {
				t.Errorf("%s: workertoken ListForUser called with %d args, expected (ctx, userId). "+
					"This test cannot verify the userId is caller-derived, so it fails rather "+
					"than passing blind.", where, len(call.Args))
				return true
			}

			arg, ok := call.Args[1].(*ast.Ident)
			if !ok {
				t.Errorf("%s: workertoken ListForUser's userId argument is an expression, not a "+
					"local identifier, so it cannot be traced to the authenticated caller. "+
					"That query is @serverOnly and projects keyHash + lastConnectedFromIP "+
					"(memql#3063); the internal-origin stamp inside ListForUser reads it "+
					"regardless of who asked. Assign caller.%s to a local and pass that.",
					where, callerSubjectField)
				return true
			}
			if !subjects[arg.Name] {
				t.Errorf("%s: workertoken ListForUser is passed %q, which is not assigned from "+
					"the authenticated caller's %s in %s. If that value comes off the request "+
					"payload, any caller reads another user's worker-token keyHash and "+
					"lastConnectedFromIP -- the exact exposure memql#3063 closed, reopened "+
					"underneath the @serverOnly annotation. The annotation gates the WIRE; "+
					"the stamp in ListForUser deliberately bypasses it; only a caller-derived "+
					"argument keeps that safe, and component/identity/workertoken's line in "+
					"the root call_origin_conformance_test.go allowlist is granted on that "+
					"basis.", where, arg.Name, callerSubjectField, fn.Name.Name)
			}
			return true
		})
	}
	return found
}

// workerTokenStoreLocals names the locals in fn assigned a workertoken.Store.
// Matches `x := &workertoken.Store{...}` and the non-pointer form.
func workerTokenStoreLocals(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range as.Rhs {
			if i >= len(as.Lhs) {
				break
			}
			lhs, ok := as.Lhs[i].(*ast.Ident)
			if !ok {
				continue
			}
			if isWorkerTokenStoreLit(rhs) {
				out[lhs.Name] = true
			}
		}
		return true
	})
	return out
}

func isWorkerTokenStoreLit(e ast.Expr) bool {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	cl, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	sel, ok := cl.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Store" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "workertoken"
}

// callerSubjectLocals names the locals in fn whose value traces to the
// authenticated caller's Subject. One hop of indirection is followed, so
// `s := caller.Subject` and `u := strings.TrimSpace(caller.Subject)` and
// `v := u` all count, while anything reached from a request message does not.
func callerSubjectLocals(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	// Two passes so an alias assigned before its source is still resolved.
	for pass := 0; pass < 2; pass++ {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, rhs := range as.Rhs {
				if i >= len(as.Lhs) {
					break
				}
				lhs, ok := as.Lhs[i].(*ast.Ident)
				if !ok || lhs.Name == "_" {
					continue
				}
				if exprIsCallerSubject(rhs, out) {
					out[lhs.Name] = true
				}
			}
			return true
		})
	}
	return out
}

// exprIsCallerSubject reports whether e reads the caller's Subject, directly
// or through already-known caller-derived locals. Deliberately conservative:
// an expression it cannot account for is NOT caller-derived, so an unfamiliar
// shape fails the gate rather than slipping through it.
func exprIsCallerSubject(e ast.Expr, known map[string]bool) bool {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		return v.Sel.Name == callerSubjectField
	case *ast.Ident:
		return known[v.Name]
	case *ast.CallExpr:
		// String-shaping wrappers (strings.TrimSpace, strings.ToLower, ...)
		// preserve provenance; a call taking nothing caller-derived does not.
		for _, a := range v.Args {
			if exprIsCallerSubject(a, known) {
				return true
			}
		}
		return false
	case *ast.BinaryExpr:
		return exprIsCallerSubject(v.X, known) || exprIsCallerSubject(v.Y, known)
	case *ast.ParenExpr:
		return exprIsCallerSubject(v.X, known)
	default:
		return false
	}
}

func importsWorkerTokenStore(f *ast.File) bool {
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		if strings.HasSuffix(strings.Trim(imp.Path.Value, `"`), workerTokenPkgPath) {
			return true
		}
	}
	return false
}

// repoRootFromGRPC walks up from this package to the module root.
func repoRootFromGRPC() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
