package pipelinerun

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// internal_origin_test.go -- the two halves of this package's place on the
// internal-origin allowlist (call_origin_conformance_test.go): the stamp is
// applied in ONE place, inline, so it never escapes its call; and everything
// a person reaches is downstream of an owner-scoped read under their own actor.

// TestTheStampNeverEscapesItsCall pins the memql#2879 escalation shape: a
// trusted frame stamps internal, binds it to a variable, and a later frame
// runs on the inherited context. ContextWithInternalOrigin appears exactly
// once in this package, inline as the argument to dslStore.executeInternal's
// one Execute.
func TestTheStampNeverEscapesItsCall(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sites := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			t.Fatalf("%s: %v", name, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				if node.Sel.Name == "ContextWithInternalOrigin" {
					sites++
				}
			case *ast.AssignStmt:
				for _, rhs := range node.Rhs {
					if call, ok := rhs.(*ast.CallExpr); ok {
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ContextWithInternalOrigin" {
							t.Errorf("%s binds ContextWithInternalOrigin to a variable; keep it INLINE as the argument to the one Execute that needs it (memql#2879)", name)
						}
					}
				}
			}
			return true
		})
	}
	if sites != 1 {
		t.Errorf("want exactly one ContextWithInternalOrigin site in this package (dslStore.executeInternal), found %d", sites)
	}
}

// TestNoPersonReachesAnotherOwnersRows drives every person-facing act as a
// person who owns nothing, against the REAL dsl store over an engine that
// answers every read empty -- which is what the owner-scoped constructs answer
// a stranger -- and asserts that nothing past the owner-scoped read is reached:
// no server-only read, no write, no stamp. That is the precondition the
// allowlist entry rests on.
func TestNoPersonReachesAnotherOwnersRows(t *testing.T) {
	engine := newRecordingEngine()
	integ := New(Deps{
		Store:  NewDSLStore(engine),
		GitHub: newFakeGitHub(),
		Gate:   newKeyedGate().run,
		Now:    func() time.Time { return testNow },
	})
	stranger := personCtx(otherID)
	acts := map[string]func() error{
		"connect": func() error {
			_, err := integ.Connect(stranger, ConnectRequest{PackageID: packageID, Delivery: DeliveryWebhook})
			return err
		},
		"disconnect": func() error { _, err := integ.Disconnect(stranger, "p1"); return err },
		"rerun":      func() error { _, err := integ.Rerun(stranger, "r1"); return err },
		"cancel": func() error {
			_, err := integ.handleCancel(stranger, map[string]any{"runId": "r1"}, 0)
			return err
		},
	}
	for name, act := range acts {
		before := len(engine.recorded())
		if err := act(); err == nil {
			t.Errorf("%s: a person who owns nothing was not refused", name)
		}
		calls := engine.recorded()[before:]
		if len(calls) != 1 {
			t.Errorf("%s: want exactly the one owner-scoped read, got %d calls", name, len(calls))
		}
		for _, c := range calls {
			if c.origin.IsInternal() {
				t.Errorf("%s reached a stamped call before ownership was proved: %s", name, c.query)
			}
			if c.actor == nil || c.actor.UserId != otherID {
				t.Errorf("%s read under somebody other than the caller: %+v", name, c.actor)
			}
		}
	}
}

// TestTheRunnersEntryPointsReadAsTheSystemActor: a delivery, a poll and a
// substrate cancel have no person behind them, so every read they make is the
// server-only kind -- whoever the context they arrive on claims to be.
func TestTheRunnersEntryPointsReadAsTheSystemActor(t *testing.T) {
	engine := newRecordingEngine()
	integ := New(Deps{Store: NewDSLStore(engine), GitHub: newFakeGitHub(), Gate: newKeyedGate().run})
	caller := personCtx(otherID)

	if _, err := integ.Trigger(caller, "github", prDelivery(t, "opened", 1, shaA, repoName, testInstallation), ""); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if _, err := integ.Poll(caller); err != nil {
		t.Fatalf("poll: %v", err)
	}
	_ = integ.RequestCancel(caller, "r1", "substrate")
	calls := engine.recorded()
	if len(calls) != 3 {
		t.Fatalf("calls = %d (%v)", len(calls), calls)
	}
	for _, c := range calls {
		if !c.origin.IsInternal() || c.actor == nil || c.actor.UserId != "system:pipelines" {
			t.Errorf("%s ran as %+v (internal %v); want the pipelines system actor", c.query, c.actor, c.origin.IsInternal())
		}
	}
	if !calls[2].fresh {
		t.Errorf("the cancel's read decides a write under the gate, so it is fresh")
	}
}
