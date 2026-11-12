package datasync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// capabilities_test.go -- the operator's sync acts are for a cluster owner or
// the engine itself.
//
// The five builtins behind Cluster > Data's buttons are @sdk with no
// @requiresCapability, and a builtin with neither has no rank floor: every
// signed-in caller reaches its handler. The page's part only hides the
// buttons. So each handler asks who is calling, before anything is read. A
// sixth, returnHeldInbound, is a connector's call as the operator and asks the
// same.

// operatorActsFixture is one fake engine behind the store, the runner and the
// dispatcher, and one connector bound under a name of this test's own, so a
// refused call that reached any of them shows up in one place.
type operatorActsFixture struct {
	engine *fakeEngine
	writer *fakeWriter
	c      *fakeConnector
	i      *Integration
}

func newOperatorActsFixture(t *testing.T) *operatorActsFixture {
	t.Helper()
	name := fmt.Sprintf("gate-test-%d", time.Now().UnixNano())
	memqlsync.Declare(name)
	c := &fakeConnector{name: name, domains: []memqlsync.DomainSpec{{Concept: testMirrorConcept, Direction: memqlsync.DirectionInbound}}}
	if err := memqlsync.Bind(c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(name) })
	engine, writer := newFakeEngine(), newFakeWriter()
	dead := outboxRow("entry-1", name, 5, "")
	dead["status"] = "dead"
	engine.seed("query outboxDeadLetters", []map[string]any{dead})
	return &operatorActsFixture{
		engine: engine, writer: writer, c: c,
		i: &Integration{
			store:      NewStore(engine),
			runner:     testRunner(engine, writer, c),
			dispatcher: testDispatcher(engine, writer, c),
			logger:     discardLogger(),
		},
	}
}

// capabilityHandler is the shape every capability handler has.
type capabilityHandler func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error)

// operatorAct is one of them: its capability name, its handler and the
// arguments a caller would send.
type operatorAct struct {
	name    string
	handler func(i *Integration) capabilityHandler
	args    func(f *operatorActsFixture) map[string]any
}

func (a operatorAct) call(f *operatorActsFixture, ctx context.Context) error {
	_, err := a.handler(f.i)(ctx, a.args(f), 0)
	return err
}

func operatorActs() []operatorAct {
	return []operatorAct{
		{"startBackfill", func(i *Integration) capabilityHandler { return i.handleStartBackfill },
			func(f *operatorActsFixture) map[string]any {
				return map[string]any{"connector": f.c.name, "conceptId": testMirrorConcept, "restart": true}
			}},
		{"setSyncPaused", func(i *Integration) capabilityHandler { return i.handleSetPaused },
			func(f *operatorActsFixture) map[string]any {
				return map[string]any{"connector": f.c.name, "conceptId": testMirrorConcept, "paused": true}
			}},
		{"retryOutboxEntry", func(i *Integration) capabilityHandler { return i.handleRetryOutboxEntry },
			func(*operatorActsFixture) map[string]any { return map[string]any{"entryId": "entry-1"} }},
		{"discardOutboxEntry", func(i *Integration) capabilityHandler { return i.handleDiscardOutboxEntry },
			func(*operatorActsFixture) map[string]any {
				return map[string]any{"entryId": "entry-1", "reason": "the row was removed at the origin"}
			}},
		{"requeueInbound", func(i *Integration) capabilityHandler { return i.handleRequeueInbound },
			func(f *operatorActsFixture) map[string]any { return map[string]any{"source": f.c.name} }},
		{"returnHeldInbound", func(i *Integration) capabilityHandler { return i.handleReturnHeldInbound },
			func(f *operatorActsFixture) map[string]any { return map[string]any{"source": f.c.name} }},
	}
}

func signedIn(userID string, role auth.Role) context.Context {
	ctx := auth.ContextWithUserActor(context.Background(), userID)
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: userID, Role: role})
}

// clusterOwnerCtx is a cluster owner's call, which the operator's sync acts
// admit.
func clusterOwnerCtx() context.Context { return signedIn("v1:identity:user:owner", auth.RoleOwner) }

func TestTheOperatorsSyncActsRefuseACallerWhoIsNotAClusterOwner(t *testing.T) {
	callers := map[string]context.Context{
		"a signed-in writer": signedIn("v1:identity:user:writer", auth.RoleWriter),
		"an admin":           signedIn("v1:identity:user:admin", auth.RoleAdmin),
		"nobody":             context.Background(),
	}
	for _, act := range operatorActs() {
		for who, ctx := range callers {
			t.Run(act.name+"/"+who, func(t *testing.T) {
				f := newOperatorActsFixture(t)
				err := act.call(f, ctx)
				want := "datasync: " + act.name + " is for a cluster owner"
				if err == nil || err.Error() != want {
					t.Fatalf("%s answered %v, want the refusal %q", act.name, err, want)
				}
				if calls := f.engine.callsContaining(""); len(calls) != 0 {
					t.Errorf("a refused call reached the engine: %v", calls)
				}
				if len(f.c.backfillSeen) != 0 || f.c.applyCalls != 0 || f.c.propagateCount() != 0 {
					t.Errorf("a refused call reached the connector: backfill %v, apply %d, propagate %d",
						f.c.backfillSeen, f.c.applyCalls, f.c.propagateCount())
				}
				if ids := f.writer.writtenIDs(); len(ids) != 0 {
					t.Errorf("a refused call wrote a mirror row: %v", ids)
				}
			})
		}
	}
}

func TestTheOperatorsSyncActsAdmitAClusterOwnerAndInternalOrigin(t *testing.T) {
	callers := map[string]context.Context{
		"a cluster owner": clusterOwnerCtx(),
		// A connector acting as the operator, an automation: the origin is the
		// engine's, whatever actor rides with it.
		"internal origin with a writer": auth.ContextWithInternalOrigin(signedIn("v1:identity:user:writer", auth.RoleWriter)),
	}
	for _, act := range operatorActs() {
		for who, ctx := range callers {
			t.Run(act.name+"/"+who, func(t *testing.T) {
				f := newOperatorActsFixture(t)
				if err := act.call(f, ctx); err != nil {
					t.Fatalf("%s refused %s: %v", act.name, who, err)
				}
				if len(f.engine.callsContaining("")) == 0 {
					t.Errorf("%s answered without reading anything", act.name)
				}
			})
		}
	}
}
