package pipelinerun

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// internal_origin_test.go -- the three halves of this package's place on the
// internal-origin allowlist (call_origin_conformance_test.go): the stamp is
// applied in ONE place, inline, so it never escapes its call; everything a
// person reaches is downstream of an owner-scoped read under their own actor;
// and the runner's own capabilities -- the trigger and the poll, builtins any
// client's query can name -- refuse every call that did not arrive with
// internal origin.

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
		GitHub: newFakeGitHub(t),
		Gate:   newKeyedGate(t).run,
		Now:    func() time.Time { return testNow },
	})
	stranger := personCtx(otherID)
	acts := map[string]func() error{
		"connect": func() error {
			_, err := integ.Connect(stranger, ConnectRequest{PackageID: packageID, Delivery: DeliveryWebhook})
			return err
		},
		"disconnect": func() error { _, err := integ.Disconnect(stranger, "p1"); return err },
		"rerun":      func() error { _, err := integ.Rerun(stranger, "r1", false); return err },
		"runLatest":  func() error { _, err := integ.RunLatest(stranger, "p1"); return err },
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

// TestOnlyAClusterOwnersDisconnectReachesAnotherOwnersPipeline is the one
// person-facing exception, and its precondition: a cluster owner's disconnect
// of a pipeline they do not own reads it server-side, stamped, as the
// pipelines system actor -- but only after their own owner-scoped read came
// back empty, only on the strength of their own verified role, and the write
// it reaches is the status alone, under the row owner's authority. A
// synthetic owner is no person, and reaches nothing.
func TestOnlyAClusterOwnersDisconnectReachesAnotherOwnersPipeline(t *testing.T) {
	engine := newRecordingEngine()
	engine.answers[qPipelineByID] = []any{map[string]any{
		"id": "v1:pipelines:pipeline:p1", "ownerUserId": "v1:identity:user:" + ownerID, "repository": repoName, "status": "active",
	}}
	integ := New(Deps{Store: NewDSLStore(engine), GitHub: newFakeGitHub(t), Gate: newKeyedGate(t).run, Now: func() time.Time { return testNow }})

	if _, err := integ.Disconnect(clusterOwnerCtx(otherID), "p1"); err != nil {
		t.Fatalf("a cluster owner's disconnect: %v", err)
	}
	calls := engine.recorded()
	if len(calls) != 3 {
		t.Fatalf("calls = %d (%v); want the owner-scoped read, the server-only read, the status write", len(calls), calls)
	}
	if c := calls[0]; constructOf(c.query) != qPipelineForOwner || c.origin.IsInternal() || c.actor == nil || c.actor.UserId != otherID {
		t.Errorf("first, the caller's own read, unstamped: %s %+v", c.query, c.actor)
	}
	if c := calls[1]; constructOf(c.query) != qPipelineByID || !c.origin.IsInternal() || c.actor == nil || c.actor.UserId != "system:pipelines" {
		t.Errorf("then the server-only read, as the system actor: %s %+v", c.query, c.actor)
	}
	if c := calls[2]; c.query != `mutation updatePipeline(pipelineId: "p1", status: "disconnected")` ||
		!c.origin.IsInternal() || c.actor == nil || c.actor.UserId != "v1:identity:user:"+ownerID {
		t.Errorf("the status alone, under the row owner's authority: %s %+v", c.query, c.actor)
	}

	before := len(engine.recorded())
	synthetic := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "operator-key", Role: auth.RoleOwner, Synthetic: true})
	if _, err := integ.Disconnect(synthetic, "p1"); !errors.Is(err, ErrNoCaller) {
		t.Errorf("a synthetic owner: %v, want ErrNoCaller", err)
	}
	if n := len(engine.recorded()) - before; n != 0 {
		t.Errorf("a synthetic owner reached %d calls", n)
	}
}

// TestTheRunnersEntryPointsRefuseAClientAndReadAsTheSystemActor is the other
// half of the allowlist argument. The trigger and the poll are BUILTINS, which
// any signed-in client's query can name (@sdk has no engine effect), so each
// refuses a client-origin call before its first read -- through the
// capability and through the Go method alike. Reached the way the shipped
// automations reach them, with internal origin, every read they make is the
// server-only kind, as the pipelines system actor, whoever the context
// otherwise claims to be; and the trigger's first read is the STAGED ROW, so
// no delivery its caller could hand it is ever read. The substrate's cancel
// is a Go API no client reaches, and reads the same way.
func TestTheRunnersEntryPointsRefuseAClientAndReadAsTheSystemActor(t *testing.T) {
	engine := newRecordingEngine()
	integ := New(Deps{Store: NewDSLStore(engine), GitHub: newFakeGitHub(t), Gate: newKeyedGate(t).run})
	caller := personCtx(otherID)

	if _, err := integ.Trigger(caller, "inbound-1"); !errors.Is(err, ErrClientOrigin) {
		t.Errorf("a person's trigger: %v, want ErrClientOrigin", err)
	}
	if _, err := integ.handleTrigger(caller, map[string]any{
		"inboundRequestId": "inbound-1", "source": "github",
		"body": prDelivery(t, "opened", 1, shaA, repoName, testInstallation),
	}, 0); !errors.Is(err, ErrClientOrigin) {
		t.Errorf("a person's trigger capability: %v, want ErrClientOrigin", err)
	}
	if _, err := integ.Poll(caller); !errors.Is(err, ErrClientOrigin) {
		t.Errorf("a person's poll: %v, want ErrClientOrigin", err)
	}
	if _, err := integ.Schedule(caller); !errors.Is(err, ErrClientOrigin) {
		t.Errorf("a person's scheduled scan: %v, want ErrClientOrigin", err)
	}
	if _, err := integ.handlePoll(caller, nil, 0); !errors.Is(err, ErrClientOrigin) {
		t.Errorf("a person's poll capability: %v, want ErrClientOrigin", err)
	}
	if _, err := integ.handleSchedule(caller, nil, 0); !errors.Is(err, ErrClientOrigin) {
		t.Errorf("a person's scheduled scan capability: %v, want ErrClientOrigin", err)
	}
	if calls := engine.recorded(); len(calls) != 0 {
		t.Fatalf("a refused client call read %d rows first: %v", len(calls), calls)
	}

	// The automation's context: internal origin, here even beside a person.
	automation := auth.ContextWithInternalOrigin(caller)
	engine.answers[qInboundRequestByID] = []any{map[string]any{
		"id": "v1:platform:inboundRequest:inbound-1", "source": "github", "signatureVerified": true,
		"body":        prDelivery(t, "opened", 1, shaA, repoName, testInstallation),
		"headersJson": `{"x-github-event":"pull_request","x-github-delivery":"d1"}`,
	}}
	if _, err := integ.Trigger(automation, "v1:platform:inboundRequest:inbound-1"); err != nil {
		t.Fatalf("the automation's trigger: %v", err)
	}
	if _, err := integ.Poll(automation); err != nil {
		t.Fatalf("the automation's poll: %v", err)
	}
	_ = integ.RequestCancel(caller, "r1", "substrate")
	calls := engine.recorded()
	want := []string{qInboundRequestByID, qPipelinesForRepository, qPipelinesPolled, qPipelineRunByID}
	if len(calls) != len(want) {
		t.Fatalf("calls = %d (%v), want %v", len(calls), calls, want)
	}
	for i, c := range calls {
		if got := constructOf(c.query); got != want[i] {
			t.Errorf("call %d is %s, want %s", i, got, want[i])
		}
		if !c.origin.IsInternal() || c.actor == nil || c.actor.UserId != "system:pipelines" {
			t.Errorf("%s ran as %+v (internal %v); want the pipelines system actor", c.query, c.actor, c.origin.IsInternal())
		}
	}
	if calls[0].query != `query inboundRequestById(requestId: "inbound-1")` {
		t.Errorf("the staged row is read by its bare id: %s", calls[0].query)
	}
	if !calls[3].fresh {
		t.Errorf("the cancel's read decides a write under the gate, so it is fresh")
	}
}
