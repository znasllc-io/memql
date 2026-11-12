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

	n, more, err := d.ReturnHeld(context.Background(), "shopify-store-1", 0)
	if err != nil {
		t.Fatalf("ReturnHeld: %v", err)
	}
	if n != 2 || more {
		t.Fatalf("ReturnHeld put %d deliveries back in line (more %t), want the two it won and no more to do", n, more)
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
	if _, _, err := d.ReturnHeld(context.Background(), " ", 0); err == nil {
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

// ONE CALL IS BOUNDED BY WHAT ITS CALLER NAMES, AND SAYS WHEN IT STOPPED THERE.
// The act that resumes a store returns one page before it answers and leaves
// the rest to the watch; the watch names nothing and gets the runtime's own
// bound. Stopping at the bound is not a failure: the next pass returns what is
// still held, because it asks by state.
func TestReturnHeldStopsAtTheBoundItsCallerNames(t *testing.T) {
	for _, tc := range []struct {
		name      string
		limit     int
		want      int
		wantReads int
	}{
		{"one page, as the act that resumes a store asks", 50, 50, 1},
		{"fewer than a page", 20, 20, 1},
		{"a page and a half", 75, 75, 2},
		{"no bound named: the runtime's own", 0, maxBackInLine, maxBackInLine / 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &freshHeldEngine{fakeEngine: newFakeEngine()}
			d := retryDispatcher(engine, newFakeWriter(), pausedConnector())
			n, more, err := d.ReturnHeld(context.Background(), "shopify-store-1", tc.limit)
			if err != nil {
				t.Fatalf("a return that stopped at its bound failed: %v", err)
			}
			if n != tc.want || !more {
				t.Fatalf("returned %d (more %t), want %d and more left", n, more, tc.want)
			}
			if reads := engine.countContaining("query inboundRequestsHeld("); reads != tc.wantReads {
				t.Errorf("the held deliveries were read %d times, want %d: nothing is read past the bound", reads, tc.wantReads)
			}
			if writes := engine.countContaining("mutation requeueInboundRequest("); writes != tc.want {
				t.Errorf("%d deliveries were written, want %d", writes, tc.want)
			}
		})
	}
}

// A RETURN OF NOTHING IS ONE READ AND NO WRITE. The store watch asks for every
// store that ingests on every pass, and nearly always nothing is held.
func TestAReturnOfNothingIsOneReadAndNoWrite(t *testing.T) {
	engine := newFakeEngine()
	d := retryDispatcher(engine, newFakeWriter(), pausedConnector())
	n, more, err := d.ReturnHeld(context.Background(), "shopify-store-1", 0)
	if err != nil || n != 0 || more {
		t.Fatalf("ReturnHeld = %d, more %t, %v; want nothing returned and nothing left", n, more, err)
	}
	calls := engine.callsContaining("")
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "query inboundRequestsHeld(") {
		t.Fatalf("a return of nothing made %v, want one read of the held deliveries and nothing else", calls)
	}
}

// THE BOUND IS AN ARGUMENT OF THE BUILTIN, VALIDATED BEFORE ANYTHING IS READ.
// Left out, the runtime's own bound; named, a whole number from one to that
// bound. The reply says how many went back and whether the call stopped at
// its bound.
func TestTheReturnsBoundIsValidatedInTheHandler(t *testing.T) {
	internal := auth.ContextWithInternalOrigin(context.Background())
	for name, limit := range map[string]any{
		"zero": 0, "negative": -1, "past the runtime's bound": maxBackInLine + 1,
		"not whole": 1.5, "not a number": "fifty",
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			engine := newFakeEngine()
			i := &Integration{dispatcher: testDispatcher(engine, newFakeWriter(), &fakeConnector{name: "shopify"}), logger: discardLogger()}
			_, err := i.handleReturnHeldInbound(internal, map[string]any{"source": "shopify-store-1", "limit": limit}, 0)
			if err == nil || !strings.Contains(err.Error(), "limit") {
				t.Fatalf("err = %v, want the limit refused", err)
			}
			if n := len(engine.callsContaining("")); n != 0 {
				t.Fatalf("a refused limit reached the engine %d times", n)
			}
		})
	}
	for name, tc := range map[string]struct {
		limit any
		want  string
	}{
		"left out":              {nil, fmt.Sprintf("source=shopify-store-1 returned=%d more=true", maxBackInLine)},
		"one page":              {50, "source=shopify-store-1 returned=50 more=true"},
		"as the engine decodes": {float64(50), "source=shopify-store-1 returned=50 more=true"},
		"as an int64":           {int64(20), "source=shopify-store-1 returned=20 more=true"},
	} {
		t.Run("admitted: "+name, func(t *testing.T) {
			engine := &freshHeldEngine{fakeEngine: newFakeEngine()}
			i := &Integration{dispatcher: retryDispatcher(engine, newFakeWriter(), pausedConnector()), logger: discardLogger()}
			args := map[string]any{"source": "shopify-store-1"}
			if tc.limit != nil {
				args["limit"] = tc.limit
			}
			nodes, err := i.handleReturnHeldInbound(internal, args, 0)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if len(nodes) != 1 || !strings.Contains(string(nodes[0].Payload), `"outcome":"returned"`) || !strings.Contains(string(nodes[0].Payload), tc.want) {
				t.Fatalf("reply %s, want returned with %q", nodes[0].Payload, tc.want)
			}
		})
	}
	// Nothing held: the reply says nothing is left.
	engine := newFakeEngine()
	i := &Integration{dispatcher: testDispatcher(engine, newFakeWriter(), &fakeConnector{name: "shopify"}), logger: discardLogger()}
	nodes, err := i.handleReturnHeldInbound(internal, map[string]any{"source": "shopify-store-1"}, 0)
	if err != nil || len(nodes) != 1 || !strings.Contains(string(nodes[0].Payload), "returned=0 more=false") {
		t.Fatalf("a return of nothing answered %v, %v", nodes, err)
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
