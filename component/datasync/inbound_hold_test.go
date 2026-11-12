package datasync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// ---- A pause holds its deliveries, and a resume returns them ----
//
// A connector answers memqlsync.ErrRecordingPaused for a delivery of a tenant
// a person paused. The dispatcher HOLDS such a delivery: failed, with no next
// try, the try not counted, and the sentinel's own text as its lastError. That
// is the shape of a refusal no retry changes, so the retry pass never answers
// it and the given-up read never counts it. ReturnHeld puts one source's held
// deliveries back in line, as Requeue does for given-up ones.

// pausedConnector answers every delivery as one for a paused tenant.
func pausedConnector() *forgettingConnector {
	return &forgettingConnector{fakeConnector: &fakeConnector{
		name:     "shopify",
		domains:  []memqlsync.DomainSpec{{Concept: testMirrorConcept, Direction: memqlsync.DirectionInbound}},
		applyErr: memqlsync.RecordingPaused("shopify", "store-1"),
	}}
}

func TestADeliveryOfAPausedTenantIsHeldWithItsTriesAsTheyWere(t *testing.T) {
	for _, prior := range []int{0, 2} {
		t.Run(fmt.Sprintf("after %d tries", prior), func(t *testing.T) {
			engine := newFakeEngine()
			c := pausedConnector()
			d := retryDispatcher(engine, newFakeWriter(), c)

			res, err := d.DispatchStaged(context.Background(), stagedFailure("req-1", prior))
			if err != nil {
				t.Fatalf("a held delivery was reported as a failure: %v", err)
			}
			if !res.Handled || !res.Held || res.Applied != 0 {
				t.Fatalf("result %+v, want handled and held, with nothing applied", res)
			}
			stamp := onlyStamp(t, engine)
			if stamp["status"] != "failed" {
				t.Fatalf("stamped %v, want failed: the shape the retry pass and the given-up read both leave alone", stamp)
			}
			if stamp["lastError"] != memqlsync.ErrRecordingPaused.Error() {
				t.Fatalf("lastError = %q, want exactly %q -- a held delivery is found again by this text", stamp["lastError"], memqlsync.ErrRecordingPaused.Error())
			}
			if !clearsNextAttempt(stamp) {
				t.Fatalf("stamped %v: a held delivery keeps no next time, so the retry pass never answers it", stamp)
			}
			if _, counted := stamp["attempts"]; counted {
				t.Fatalf("stamped %v: the try must not be counted, or four pauses would read as a delivery given up", stamp)
			}
			if len(c.forgot) != 1 || c.forgot[0] != "req-1" {
				t.Fatalf("the connector was told to forget %v, want [req-1]: the delivery is worked again when it returns", c.forgot)
			}
		})
	}
}

// A HELD DELIVERY IS NEITHER A RECOVERY NOR A FAILURE of the retry pass: it
// is counted as held, and the pass goes on.
func TestTheRetryPassCountsAHeldDeliveryAsHeld(t *testing.T) {
	engine := newRetryEngine()
	c := pausedConnector()
	d := retryDispatcher(engine, newFakeWriter(), c)
	engine.seed("query inboundRequestsDueForRetry", []map[string]any{dueRow("req-1", 0)})

	res, err := d.RetryDue(context.Background())
	if err != nil {
		t.Fatalf("RetryDue: %v", err)
	}
	if res != (RetryResult{Tried: 1, Held: 1}) {
		t.Fatalf("RetryDue = %+v, want one tried and held, none recovered", res)
	}
	stamps := engine.callsContaining("mutation updateInboundRequestStatus(")
	if len(stamps) != 2 {
		t.Fatalf("status writes = %v, want the claim and the hold", stamps)
	}
	hold := renderedArgs(stamps[1])
	if hold["lastError"] != memqlsync.ErrRecordingPaused.Error() || !clearsNextAttempt(hold) || hold["attempts"] != nil {
		t.Fatalf("the hold wrote %v, want the sentinel's text, no next time (the claim's window gone) and no try counted", hold)
	}
}

// The event path says what happened to a delivery it held.
func TestTheEventPathAnswersHeldForAPausedTenantsDelivery(t *testing.T) {
	engine := newFakeEngine()
	c := pausedConnector()
	i := &Integration{dispatcher: retryDispatcher(engine, newFakeWriter(), c)}
	stageInbound(engine, stagedInbound("shopify"))
	nodes, err := dispatchByID(i, "req-held")
	if err != nil {
		t.Fatalf("a held delivery failed the automation's step: %v", err)
	}
	if len(nodes) != 1 || !strings.Contains(string(nodes[0].Payload), `"outcome":"held"`) {
		t.Fatalf("result %v, want one held node", nodes)
	}
}

// heldRow is a held delivery as inboundRequestsHeld answers it.
func heldRow(id, source string, version time.Time) map[string]any {
	return map[string]any{
		"id": id, "source": source, "status": "failed", "lastError": memqlsync.ErrRecordingPaused.Error(),
		"nextAttemptAt": "", "createdAt": version.Format(time.RFC3339Nano),
	}
}

// requeueEngine is a fakeEngine that records, per requeue, whether it carried
// the row-version fence, and refuses the fenced requeues of the ids in lose as
// another writer's commit would.
type requeueEngine struct {
	*fakeEngine
	mu     sync.Mutex
	fenced map[string]bool
	lose   map[string]bool
}

func newRequeueEngine() *requeueEngine {
	return &requeueEngine{fakeEngine: newFakeEngine(), fenced: map[string]bool{}, lose: map[string]bool{}}
}

func (r *requeueEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "mutation requeueInboundRequest(") {
		id, _ := renderedArgs(q)["requestId"].(string)
		fenced := memql.HasRowVersionFence(ctx, inboundRequestConcept, id)
		r.mu.Lock()
		r.fenced[id] = fenced
		lost := fenced && r.lose[id]
		r.mu.Unlock()
		if lost {
			_, _ = r.fakeEngine.Execute(ctx, q)
			return nil, fmt.Errorf("execute: %w", memql.ErrRowVersionChanged)
		}
	}
	return r.fakeEngine.Execute(ctx, q)
}

func TestReturnHeldPutsOneSourcesHeldDeliveriesBackInLine(t *testing.T) {
	engine := newRequeueEngine()
	d := retryDispatcher(engine, newFakeWriter(), pausedConnector())
	version := testNow.Add(-time.Hour)
	engine.seed("query inboundRequestsHeld", []map[string]any{
		heldRow("req-a", "shopify-store-1", version), heldRow("req-b", "shopify-store-1", version),
		// Another writer took this one since it was read: its requeue loses the
		// fence and is left to whoever won.
		heldRow("req-taken", "shopify-store-1", version),
		// A row of another source the read should never answer; were it to, it
		// is not this call's to move.
		heldRow("req-other", "shopify-store-2", version),
	})
	engine.lose["req-taken"] = true

	n, err := d.ReturnHeld(context.Background(), "shopify-store-1")
	if err != nil {
		t.Fatalf("ReturnHeld: %v", err)
	}
	if n != 2 {
		t.Fatalf("ReturnHeld put %d deliveries back in line, want the two it won", n)
	}
	reads := engine.callsContaining("query inboundRequestsHeld(")
	if len(reads) == 0 {
		t.Fatal("the held deliveries were not read")
	}
	if args := renderedArgs(reads[0]); args["source"] != "shopify-store-1" || args["reason"] != memqlsync.ErrRecordingPaused.Error() {
		t.Fatalf("the held read asked %v, want the source and the sentinel's own text", args)
	}
	if fresh := engine.freshContaining("query inboundRequestsHeld("); !fresh[0] {
		t.Fatal("the held read was not fresh: a cached page would return what another call already put back")
	}
	requeues := engine.callsContaining("mutation requeueInboundRequest(")
	if len(requeues) != 3 {
		t.Fatalf("requeues = %v, want one for each row of the source and none for the other source's", requeues)
	}
	for _, q := range requeues {
		args := renderedArgs(q)
		id, _ := args["requestId"].(string)
		if id == "req-other" {
			t.Fatalf("another source's delivery was put back in line: %s", q)
		}
		if args["nextAttemptAt"] != testNow.Format(time.RFC3339) {
			t.Errorf("%s is due at %v, want now", id, args["nextAttemptAt"])
		}
		if !engine.fenced[id] {
			t.Errorf("the requeue of %s carried no row-version fence: a second return could move a row a pass has claimed", id)
		}
	}
	if _, err := d.ReturnHeld(context.Background(), " "); err == nil {
		t.Fatal("a return naming no source was accepted")
	}
}

// freshHeldEngine answers every held read with a page of rows nobody has
// returned yet: a source whose held deliveries outnumber one call.
type freshHeldEngine struct {
	*fakeEngine
	mu   sync.Mutex
	page int
}

func (f *freshHeldEngine) Execute(ctx context.Context, q string) (any, error) {
	if _, err := f.fakeEngine.Execute(ctx, q); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(q, "query inboundRequestsHeld(") {
		return []map[string]any{}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.page++
	rows := make([]map[string]any, 0, 50)
	for i := 0; i < 50; i++ {
		rows = append(rows, heldRow(fmt.Sprintf("req-%d-%d", f.page, i), "shopify-store-1", testNow.Add(-time.Hour)))
	}
	return rows, nil
}

// ONE CALL IS BOUNDED, AND SAYS SO. A return that stopped at its bound with
// deliveries still held is not a return: it answers an error, so whoever
// asked for it asks again rather than believing the store's held deliveries
// are all back in line.
func TestReturnHeldSaysWhenMoreRemain(t *testing.T) {
	engine := &freshHeldEngine{fakeEngine: newFakeEngine()}
	d := retryDispatcher(engine, newFakeWriter(), pausedConnector())
	n, err := d.ReturnHeld(context.Background(), "shopify-store-1")
	if n != maxRequeuePages*50 {
		t.Fatalf("one call put %d deliveries back in line, want its bound of %d", n, maxRequeuePages*50)
	}
	if err == nil || !strings.Contains(err.Error(), "more remain") {
		t.Fatalf("err = %v, want it to say more remain", err)
	}
}

func TestReturnHeldRefusesACallerWhoIsNotAClusterOwner(t *testing.T) {
	args := map[string]any{"source": "shopify-store-1"}
	for name, ctx := range map[string]context.Context{
		"a signed-in person": auth.ContextWithUserActor(context.Background(), "v1:identity:user:someone"),
		"an admin": auth.ContextWithAccess(context.Background(),
			&auth.AccessContext{UserId: "v1:identity:user:admin", Role: auth.RoleAdmin}),
		"nobody": context.Background(),
	} {
		t.Run(name, func(t *testing.T) {
			engine := newFakeEngine()
			i := &Integration{dispatcher: testDispatcher(engine, newFakeWriter(), &fakeConnector{name: "shopify"}), logger: discardLogger()}
			_, err := i.handleReturnHeldInbound(ctx, args, 0)
			if err == nil || !strings.Contains(err.Error(), "is for a cluster owner") {
				t.Fatalf("err = %v, want the gate's refusal", err)
			}
			if n := len(engine.callsContaining("")); n != 0 {
				t.Fatalf("a refused call reached the engine %d times", n)
			}
		})
	}
	for name, ctx := range map[string]context.Context{
		"a cluster owner": auth.ContextWithAccess(context.Background(),
			&auth.AccessContext{UserId: "v1:identity:user:owner", Role: auth.RoleOwner}),
		"internal origin": auth.ContextWithInternalOrigin(context.Background()),
	} {
		t.Run(name, func(t *testing.T) {
			engine := newFakeEngine()
			i := &Integration{dispatcher: testDispatcher(engine, newFakeWriter(), &fakeConnector{name: "shopify"}), logger: discardLogger()}
			if _, err := i.handleReturnHeldInbound(ctx, args, 0); err != nil {
				t.Fatalf("refused: %v", err)
			}
			if engine.countContaining(`query inboundRequestsHeld(source: "shopify-store-1"`) != 1 {
				t.Fatalf("the held deliveries of the source were not read: %v", engine.callsContaining(""))
			}
		})
	}
}

// The builtin is declared for the capability that serves it, and says in its
// description who may call it.
func TestReturnHeldIsACapabilityThatSaysWhoMayCallIt(t *testing.T) {
	for _, c := range (&Integration{}).Capabilities() {
		if c.Name != "returnHeldInbound" {
			continue
		}
		if !strings.Contains(c.Description, "cluster owner") || !strings.Contains(c.Description, "internal origin") {
			t.Fatalf("the description does not say who may call it: %q", c.Description)
		}
		return
	}
	t.Fatal("the sync runtime has no returnHeldInbound capability")
}

// A requeue of a given-up delivery is fenced too: the two share their code.
func TestRequeueIsFencedOnTheVersionItRead(t *testing.T) {
	engine := newRequeueEngine()
	d := retryDispatcher(engine, newFakeWriter(), pausedConnector())
	row := heldRow("req-given-up", "shopify", testNow.Add(-time.Hour))
	row["attempts"], row["lastError"] = 4, "origin timed out"
	engine.seed("query inboundRequestsGivenUp", []map[string]any{row})
	n, err := d.Requeue(context.Background(), "shopify")
	if err != nil || n != 1 {
		t.Fatalf("Requeue = %d, %v; want 1", n, err)
	}
	if !engine.fenced["req-given-up"] {
		t.Fatal("the requeue carried no row-version fence")
	}
	if errors.Is(err, memql.ErrRowVersionChanged) {
		t.Fatal("a lost fence reached the caller as an error")
	}
}
