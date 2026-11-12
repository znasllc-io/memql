package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// pause_hold_test.go -- a pause holds, it does not lose.
//
// A paused store's delivery used to be answered "nothing to write" and
// stamped processed. The sweep after a resume reads what changed, but a
// domain swept by its last-changed time cannot see a deletion, so a record
// deleted during a pause stayed in the mirror for good. The connector now
// answers such a delivery as paused, the runtime holds it, and a resume
// returns what was held and queues the backfills the store never started.

// pauseEngine is the harness engine with what a pause and a resume read back
// kept: each domain's sync state by its id (a queued backfill reads as
// queued), the store's status and its health. Each call is recorded with who
// made it.
type pauseEngine struct {
	*fakeEngine
	row    map[string]any
	states map[string]map[string]any

	mu  stdsync.Mutex
	log []connectCall
}

var (
	queueArgRE  = regexp.MustCompile(`conceptId: "([^"]+)", connector: "shopify", partitionKey: "([^"]*)"`)
	statusArgRE = regexp.MustCompile(`status: "([^"]+)"`)
)

func (e *pauseEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	call := connectCall{query: q, internal: auth.OriginFromContext(ctx).IsInternal()}
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		call.userId, call.role = ac.UserId, ac.Role
	}
	e.mu.Lock()
	e.log = append(e.log, call)
	e.mu.Unlock()
	switch callName(q) {
	case "syncStateById":
		var id string
		_ = json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(q, "syncStateById(stateId: "), ")")), &id)
		e.mu.Lock()
		row := e.states[id]
		e.mu.Unlock()
		if row == nil {
			return resultWithRows(nil), nil
		}
		return resultWithRows([]map[string]any{row}), nil
	case "datasyncQueueBackfill":
		if res, err := e.fakeEngine.Execute(ctx, q); err != nil {
			return res, err
		}
		m := queueArgRE.FindStringSubmatch(q)
		if m == nil {
			return nil, fmt.Errorf("the harness could not read the queue it was sent: %s", q)
		}
		id := memqlsync.StateID(m[1], ConnectorName, string(memqlsync.DirectionInbound), m[2])
		e.mu.Lock()
		e.states[id] = map[string]any{"conceptId": m[1], "partitionKey": m[2], "backfillStatus": "queued"}
		e.mu.Unlock()
		return &memql.ExecuteResult{}, nil
	case "setStoreStatus":
		if m := statusArgRE.FindStringSubmatch(q); m != nil {
			e.fakeEngine.mu.Lock()
			e.row["status"] = m[1]
			e.fakeEngine.mu.Unlock()
		}
	case "recordStoreHealth":
		body := strings.TrimPrefix(q, "recordStoreHealth(health: ")
		body = body[:strings.LastIndex(body, ", storeId: ")]
		var health map[string]any
		if err := json.Unmarshal([]byte(body), &health); err != nil {
			return nil, fmt.Errorf("the harness could not read the health it was sent: %w", err)
		}
		e.fakeEngine.mu.Lock()
		e.row["health"] = health
		e.fakeEngine.mu.Unlock()
	}
	return e.fakeEngine.Execute(ctx, q)
}

func (e *pauseEngine) callsNamed(name string) []connectCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []connectCall
	for _, c := range e.log {
		if callName(c.query) == name {
			out = append(out, c)
		}
	}
	return out
}

// state sets one domain's sync-state row of the harness store.
func (e *pauseEngine) state(concept string, fields map[string]any) {
	row := map[string]any{"conceptId": generated.ConceptID(concept), "partitionKey": testStoreID}
	for k, v := range fields {
		row[k] = v
	}
	e.mu.Lock()
	e.states[memqlsync.StateID(generated.ConceptID(concept), ConnectorName, string(memqlsync.DirectionInbound), testStoreID)] = row
	e.mu.Unlock()
}

// pauseHarness is the harness store, managed and owned by user-1, in a
// status, behind a pauseEngine. Every bulk domain of its connection starts as
// read whole under the rule, so a resume has nothing to queue until a test
// says otherwise.
func pauseHarness(t *testing.T, status string, health map[string]any) (*testHarness, *pauseEngine) {
	t.Helper()
	h := newHarness(t)
	row := map[string]any{
		"id": testStoreID, "domain": watchDomain, "name": "Acme Widgets",
		"adminTokenRef": "SHOPIFY_ACME_OFFLINE_GRANT", "webhookSecretRef": "ACME_WEBHOOK",
		"apiVersion": generated.APIVersion, "protectedDataLevel": ProtectedLevel1,
		"status": status, "ownerUserId": "user-1", "health": map[string]any{"ordersFrom": "2026-09-12"},
	}
	for k, v := range health {
		row["health"].(map[string]any)[k] = v
	}
	h.engine.setRows("stores", []map[string]any{row})
	h.engine.setRows("storeById", []map[string]any{row})
	e := &pauseEngine{fakeEngine: h.engine, row: row, states: map[string]map[string]any{}}
	h.conn.engine = e
	h.conn.stores.engine = e
	h.now = watchNow
	swept := watchNow.Add(-time.Minute).Format(time.RFC3339)
	for _, concept := range generated.ApplyOrder {
		spec := generated.Types[concept]
		if spec == nil || !domainInConnection(h.store(t), spec) {
			continue
		}
		e.state(concept, map[string]any{
			"backfillStatus": "complete", "backfillCursor": backfillCursor{Rule: originVersionRule}.encode(),
			"lastReconcileAt": swept, "lastAttemptAt": swept,
		})
	}
	return h, e
}

// queued is the concepts a resume asked the runtime to backfill, in order.
func (e *pauseEngine) queued() []string {
	var out []string
	for _, c := range e.callsNamed("datasyncQueueBackfill") {
		if m := queueArgRE.FindStringSubmatch(c.query); m != nil {
			out = append(out, generated.ConceptFromID(m[1]))
		}
	}
	return out
}

func TestAPausedStoresDeliveryIsAnsweredAsPausedAndNothingIsAsked(t *testing.T) {
	for _, topic := range []string{"products/delete", "products/update", "orders/create"} {
		h, _ := pauseHarness(t, StatusPaused, nil)
		writes, err := h.conn.Apply(context.Background(), delivery(topic, "gid://shopify/Product/1"))
		if !memqlsync.IsRecordingPaused(err) {
			t.Fatalf("%s for a paused store: writes %v, err %v; want it answered as paused, to be held", topic, writes, err)
		}
		if len(writes) != 0 {
			t.Errorf("%s for a paused store produced %d writes", topic, len(writes))
		}
		if n := len(h.admin.seen()); n != 0 {
			t.Errorf("%s for a paused store asked the origin %d times", topic, n)
		}
	}
}

// ONLY A PAUSE A PERSON CHOSE HOLDS. A store Shopify reported uninstalled, one
// whose data was purged (the purge leaves its status paused as well) and one
// in a status that records nothing are not waiting for a resume: their
// deliveries are answered with nothing to write, as before, and the runtime
// stamps them processed.
func TestAStoreThatIsNotConnectedStillDropsItsDeliveries(t *testing.T) {
	for name, over := range map[string]map[string]any{
		"uninstalled":            {"status": StatusLive, "uninstalledAt": "2026-08-20T10:00:00Z"},
		"uninstalled and paused": {"status": StatusPaused, "uninstalledAt": "2026-08-20T10:00:00Z"},
		"purged":                 {"status": StatusPaused, "redactedAt": "2026-08-20T10:00:00Z"},
		"in error":               {"status": StatusError},
	} {
		h := newHarness(t)
		withStoreRow(h, over)
		for _, topic := range []string{"products/delete", "products/update"} {
			writes, err := h.conn.Apply(context.Background(), delivery(topic, "gid://shopify/Product/1"))
			if err != nil || len(writes) != 0 {
				t.Errorf("%s, %s: writes %v, err %v; want nothing to write and no error, as before", name, topic, writes, err)
			}
		}
		if n := len(h.admin.seen()); n != 0 {
			t.Errorf("%s: the origin was asked %d times", name, n)
		}
	}
}

// A resume through the Store page: the status is written, and then what the
// resume owes the store is done as the operator -- its held deliveries are
// put back in line, and the backfills it never started are queued.
func TestResumingAStoreReturnsItsHeldDeliveriesAndQueuesWhatItNeverRead(t *testing.T) {
	h, e := pauseHarness(t, StatusPaused, nil)
	// The customers were never backfilled: the store was paused before the
	// continuing pass reached them. A sweep wrote the products' row and no
	// backfill ever did. The orders are read whole.
	delete(e.states, memqlsync.StateID(generated.ConceptID("customer"), ConnectorName, string(memqlsync.DirectionInbound), testStoreID))
	e.state("product", map[string]any{"backfillStatus": "none", "lastReconcileAt": "2026-11-11T11:00:00Z"})

	reply := invoke(t, actorCtx("user-1", auth.RoleWriter), NewIntegration(h.conn).handleStoreSetRecording, map[string]any{"storeId": testStoreID, "recording": true})
	if reply["status"] != StatusLive {
		t.Fatalf("resume answered %v, want live", reply)
	}
	returns := e.callsNamed("datasyncReturnHeldInbound")
	if len(returns) != 1 {
		t.Fatalf("the held deliveries were returned %d times, want once: %v", len(returns), returns)
	}
	if want := `builtin datasyncReturnHeldInbound(source: "shopify-` + testStoreID + `")`; returns[0].query != want {
		t.Errorf("the return = %s, want %s", returns[0].query, want)
	}
	if !returns[0].internal || !auth.IsSystemActorId(returns[0].userId) {
		t.Errorf("the return ran as %q (internal %t), want the operator: the builtin admits nobody else but a cluster owner", returns[0].userId, returns[0].internal)
	}
	// After the status was written: a delivery put back in line before the
	// store ingests again would only be held again.
	var order []string
	for _, c := range e.log {
		if name := callName(c.query); name == "setStoreStatus" || name == "datasyncReturnHeldInbound" {
			order = append(order, name)
		}
	}
	if strings.Join(order, ",") != "setStoreStatus,datasyncReturnHeldInbound" {
		t.Errorf("order = %v, want the status written before the held deliveries are returned", order)
	}
	if got := strings.Join(e.queued(), ","); got != "customer,product" {
		t.Errorf("queued %q, want the two domains no backfill ever read (customer, product) and not the ones read whole", got)
	}

	// Done twice, it does nothing twice: the queued domains read as queued.
	store := h.store(t)
	if err := h.conn.resumeStore(context.Background(), store); err != nil {
		t.Fatalf("a second resume: %v", err)
	}
	if got := strings.Join(e.queued(), ","); got != "customer,product" {
		t.Errorf("a second resume queued again: %q", got)
	}
}

// A pause returns nothing, and neither does asking for the state the store
// is in: only a resume that was written owes the store anything.
func TestOnlyAResumeThatWasWrittenReturnsHeldDeliveries(t *testing.T) {
	for _, tc := range []struct {
		status    string
		recording bool
	}{
		{StatusLive, false},   // a pause
		{StatusLive, true},    // already recording
		{StatusPaused, false}, // already paused
	} {
		h, e := pauseHarness(t, tc.status, nil)
		invoke(t, actorCtx("user-1", auth.RoleWriter), NewIntegration(h.conn).handleStoreSetRecording, map[string]any{"storeId": testStoreID, "recording": tc.recording})
		if n := len(e.callsNamed("datasyncReturnHeldInbound")); n != 0 {
			t.Errorf("status %s, recording %t: held deliveries were returned %d times", tc.status, tc.recording, n)
		}
		if got := e.queued(); len(got) != 0 {
			t.Errorf("status %s, recording %t: queued %v", tc.status, tc.recording, got)
		}
	}
}

// A return that fails does not undo the resume: the store is recording, the
// person is told so, and the watch returns what is still held.
func TestAFailedReturnDoesNotUndoTheResume(t *testing.T) {
	h, e := pauseHarness(t, StatusPaused, nil)
	h.engine.fail["datasyncReturnHeldInbound"] = errString("the database is away")
	reply := invoke(t, actorCtx("user-1", auth.RoleWriter), NewIntegration(h.conn).handleStoreSetRecording, map[string]any{"storeId": testStoreID, "recording": true})
	if reply["status"] != StatusLive || e.row["status"] != StatusLive {
		t.Fatalf("reply %v, stored status %v; want the resume to stand", reply, e.row["status"])
	}
	if n := len(e.callsNamed("createAuditEvent")); n != 1 {
		t.Errorf("the resume was audited %d times, want once", n)
	}
}

// THE WATCH RETURNS WHAT THE ACT COULD NOT. A store resumed by a raw status
// change never ran the act, and an act's return can fail half-way. The pass
// that records that the store left its pause returns its held deliveries; a
// pass whose return failed does not record the resume, so the next pass sees
// the store leaving its pause again and returns what is left. Once recorded,
// no later pass returns anything.
func TestTheWatchReturnsHeldDeliveriesWhenItRecordsThatAStoreResumed(t *testing.T) {
	paused := verdictIn(VerdictPaused, watchNow.Add(-2*time.Hour))
	h, e := pauseHarness(t, StatusLive, map[string]any{"verdict": recordedAs(paused)})
	recorded := func() string {
		v, _ := e.row["health"].(map[string]any)["verdict"].(map[string]any)
		return mapString(v, "state")
	}
	pass := func() {
		t.Helper()
		if err := h.conn.WatchStores(context.Background()); err != nil {
			t.Fatalf("the pass: %v", err)
		}
	}
	returns := func() int { return len(e.callsNamed("datasyncReturnHeldInbound")) }

	// The return fails: the resume is not recorded.
	h.engine.fail["datasyncReturnHeldInbound"] = errString("the database is away")
	pass()
	if returns() != 1 || recorded() != VerdictPaused {
		t.Fatalf("after a pass whose return failed: %d returns, recorded %q; want one try and the store still recorded as paused", returns(), recorded())
	}

	// The next pass tries again, succeeds, and records the store as it is.
	delete(h.engine.fail, "datasyncReturnHeldInbound")
	h.now = watchNow.Add(5 * time.Minute)
	pass()
	if returns() != 2 || recorded() == VerdictPaused {
		t.Fatalf("after the next pass: %d returns, recorded %q; want a second try and the resume recorded", returns(), recorded())
	}
	if resumed := mapString(e.row["health"].(map[string]any)["verdict"].(map[string]any), "resumedAt"); resumed == "" {
		t.Error("the recorded verdict carries no resumedAt")
	}

	// Recorded: later passes return nothing.
	for i := 2; i < 4; i++ {
		h.now = watchNow.Add(time.Duration(i) * 5 * time.Minute)
		pass()
	}
	if returns() != 2 {
		t.Errorf("passes after the resume was recorded returned held deliveries %d more times", returns()-2)
	}
}

// A store the watch never saw paused has nothing held to return, and a store
// still paused is not resumed by being watched.
func TestTheWatchReturnsNothingForAStoreThatDidNotResume(t *testing.T) {
	recording := verdictIn(VerdictRecording, watchNow.Add(-time.Hour))
	for name, tc := range map[string]struct {
		status   string
		previous map[string]any
	}{
		"recording, as last recorded": {StatusLive, recordedAs(recording)},
		"never watched":               {StatusLive, nil},
		"still paused":                {StatusPaused, recordedAs(verdictIn(VerdictPaused, watchNow.Add(-time.Hour)))},
		"paused since the last pass":  {StatusPaused, recordedAs(recording)},
	} {
		var health map[string]any
		if tc.previous != nil {
			health = map[string]any{"verdict": tc.previous}
		}
		h, e := pauseHarness(t, tc.status, health)
		if err := h.conn.WatchStores(context.Background()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n := len(e.callsNamed("datasyncReturnHeldInbound")); n != 0 {
			t.Errorf("%s: held deliveries were returned %d times", name, n)
		}
	}
}
