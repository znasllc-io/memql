package magiclink

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// bootstrap_stamp_test.go -- znasllc-io/memql#3591.
//
// WHAT IS BEING PINNED. When a bootstrap magic link is consumed, the cluster gets
// stamped `bootstrappedAt` -- that stamp is what makes /setup 404 and what tells
// the auto-bootstrap guard the cluster is claimed.
//
// That stamp used to live INSIDE the "first login, so create the user" branch. It
// worked because the only way an owner row could appear was that very branch. The
// env bootstrap now names the owner up front (app/integrations_identity.go), so a
// bootstrap link is normally consumed by an owner who ALREADY has a row -- and in
// that shape the old placement stamped nothing. The claim would leave /setup
// reachable on a claimed cluster until the next boot's self-heal noticed.
//
// WHY THIS IS A STRUCTURAL TEST AND NOT A BEHAVIOURAL ONE, stated plainly:
// `Verifier.Store` is a concrete *identity.Store wired to a real engine, so there
// is no seam to fake and this package has no behavioural tests at all. The choice
// is between asserting the structure and asserting nothing. The invariant is
// simple enough to state exactly -- the stamp is not nested inside a conditional
// -- so it is asserted over the parsed AST rather than by matching source text,
// which would be a test of gofmt.
//
// If a seam is ever introduced here, this should be replaced by the obvious
// behavioural case: consume a bootstrap link for an owner who already exists, and
// assert the cluster came out stamped.

func TestBootstrapStampIsNotGatedOnTheUserBeingNew(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "verifier.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse verifier.go: %v", err)
	}

	// The statement we care about: the `if` whose body stamps the cluster.
	// Located by the call, so a renamed variable or a reordered body cannot make
	// this pass by accident.
	var found bool
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		for _, stmt := range fn.Body.List {
			ifStmt, ok := stmt.(*ast.IfStmt)
			if !ok || !containsStampCall(ifStmt.Body) {
				continue
			}
			// A DIRECT statement of the function body -- not nested in the
			// user-creation branch, which is exactly the bug.
			found = true
			if ident, ok := ifStmt.Cond.(*ast.Ident); !ok || ident.Name != "bootstrap" {
				t.Errorf("the stamp is guarded by something other than `bootstrap`; if that is "+
					"deliberate this test needs rewriting, and if it is not, the stamp now runs "+
					"on ordinary sign-ins: %T", ifStmt.Cond)
			}
		}
	}

	if !found {
		t.Error("no top-level `if bootstrap { ... StampClusterBootstrapped ... }` in any function " +
			"in verifier.go.\n" +
			"Either the stamp moved back inside the create-the-user branch -- where a bootstrap " +
			"link consumed by an already-named owner stamps nothing, leaving /setup reachable on " +
			"a claimed cluster (memql#3591) -- or the stamp is gone entirely, which means nothing " +
			"marks a cluster claimed at the moment it is claimed.")
	}
}

// containsStampCall reports whether the block calls StampClusterBootstrapped at
// any depth. Depth inside the block is irrelevant: what matters is what the block
// is nested in, which the caller checks.
func containsStampCall(block *ast.BlockStmt) bool {
	stamped := false
	ast.Inspect(block, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && sel.Sel != nil && sel.Sel.Name == "StampClusterBootstrapped" {
			stamped = true
			return false
		}
		return !stamped
	})
	return stamped
}
