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
// answers such a delivery as paused and the runtime holds it. What is held
// comes back BY STATE: every pass of the store watch returns the held
// deliveries of every store that ingests, whatever resumed the store and
// however the last return ended. The act that resumes a store returns one page
// before it answers, and the pass that sees a store leave its pause queues the
// backfills the store never started.

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
// resume owes the store is done as the operator -- ONE PAGE of its held
// deliveries is put back in line, so the person is answered at once, and the
// backfills it never started are queued. The rest of what is held is the
// watch's.
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
	if want := `builtin datasyncReturnHeldInbound(limit: 50, source: "shopify-` + testStoreID + `")`; returns[0].query != want {
		t.Errorf("the return = %s, want %s: one page, and no more before the act answers", returns[0].query, want)
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
	h.conn.resumeStore(context.Background(), h.store(t))
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

// THE ACT NEVER FAILS FOR WHAT IT OWES. A return that fails, or a backfill that
// cannot be queued, does not undo the resume or fail the act: the store is
// recording, the person is told so, and the watch does the rest.
func TestAFailedReturnDoesNotUndoTheResume(t *testing.T) {
	for _, failing := range []string{"datasyncReturnHeldInbound", "datasyncQueueBackfill"} {
		h, e := pauseHarness(t, StatusPaused, nil)
		delete(e.states, memqlsync.StateID(generated.ConceptID("customer"), ConnectorName, string(memqlsync.DirectionInbound), testStoreID))
		h.engine.fail[failing] = errString("the database is away")
		reply := invoke(t, actorCtx("user-1", auth.RoleWriter), NewIntegration(h.conn).handleStoreSetRecording, map[string]any{"storeId": testStoreID, "recording": true})
		if reply["status"] != StatusLive || e.row["status"] != StatusLive {
			t.Fatalf("%s failing: reply %v, stored status %v; want the resume to stand", failing, reply, e.row["status"])
		}
		if n := len(e.callsNamed("createAuditEvent")); n != 1 {
			t.Errorf("%s failing: the resume was audited %d times, want once", failing, n)
		}
	}
}

// watchPass is one pass of the store watch at `at`.
func watchPass(t *testing.T, h *testHarness, at time.Time) {
	t.Helper()
	h.now = at
	if err := h.conn.WatchStores(context.Background()); err != nil {
		t.Fatalf("the pass: %v", err)
	}
}

// recordedState is the state of the verdict the watch last recorded.
func recordedState(e *pauseEngine) string {
	v, _ := e.row["health"].(map[string]any)["verdict"].(map[string]any)
	return mapString(v, "state")
}

// EVERY PASS RETURNS, BY STATE. A store that ingests is asked for its held
// deliveries on every pass of the watch, whatever the watch last recorded of
// it: a store paused and resumed by two direct status writes between two
// passes was never seen paused, and a return that stopped at its bound has
// more to give. The call names no bound -- the runtime's own -- and runs as
// the operator. When nothing is held it is one read.
func TestEveryWatchPassReturnsTheHeldDeliveriesOfAStoreThatIngests(t *testing.T) {
	for name, previous := range map[string]map[string]any{
		"recording, as last recorded":                 recordedAs(verdictIn(VerdictRecording, watchNow.Add(-time.Hour))),
		"never watched":                               nil,
		"recorded as paused, resumed since that pass": recordedAs(verdictIn(VerdictPaused, watchNow.Add(-2*time.Hour))),
	} {
		var health map[string]any
		if previous != nil {
			health = map[string]any{"verdict": previous}
		}
		h, e := pauseHarness(t, StatusLive, health)
		for pass := 1; pass <= 3; pass++ {
			watchPass(t, h, watchNow.Add(time.Duration(pass-1)*5*time.Minute))
			returns := e.callsNamed("datasyncReturnHeldInbound")
			if len(returns) != pass {
				t.Fatalf("%s: after %d passes the held deliveries were asked for %d times, want once a pass", name, pass, len(returns))
			}
			last := returns[len(returns)-1]
			if want := `builtin datasyncReturnHeldInbound(source: "shopify-` + testStoreID + `")`; last.query != want {
				t.Errorf("%s: the return = %s, want %s: the store's own source and the runtime's own bound", name, last.query, want)
			}
			if !last.internal || !auth.IsSystemActorId(last.userId) {
				t.Errorf("%s: the return ran as %q (internal %t), want the operator", name, last.userId, last.internal)
			}
		}
	}
}

// A STORE THAT DOES NOT INGEST IS NEVER ASKED: its deliveries stay held, or
// are nobody's to return. A paused store is not resumed by being watched.
func TestAWatchPassAsksNothingOfAStoreThatDoesNotIngest(t *testing.T) {
	recording := recordedAs(verdictIn(VerdictRecording, watchNow.Add(-time.Hour)))
	for name, tc := range map[string]struct {
		status string
		over   map[string]any
	}{
		"still paused":               {StatusPaused, nil},
		"paused since the last pass": {StatusPaused, nil},
		"uninstalled":                {StatusLive, map[string]any{"uninstalledAt": "2026-11-10T10:00:00Z"}},
		"purged":                     {StatusPaused, map[string]any{"redactedAt": "2026-11-10T10:00:00Z"}},
		"in error":                   {StatusError, nil},
	} {
		previous := recording
		if name == "still paused" {
			previous = recordedAs(verdictIn(VerdictPaused, watchNow.Add(-time.Hour)))
		}
		h, e := pauseHarness(t, tc.status, map[string]any{"verdict": previous})
		for k, v := range tc.over {
			e.row[k] = v
		}
		for pass := 0; pass < 2; pass++ {
			watchPass(t, h, watchNow.Add(time.Duration(pass)*5*time.Minute))
		}
		if n := len(e.callsNamed("datasyncReturnHeldInbound")); n != 0 {
			t.Errorf("%s: held deliveries were asked for %d times", name, n)
		}
		if got := e.queued(); len(got) != 0 {
			t.Errorf("%s: queued %v", name, got)
		}
	}
}

// A FAILED RETURN HOLDS NOTHING BACK. The pass records the store as it is --
// here, that it left its pause -- whether or not the return worked, and the
// next pass asks again, because it asks by state.
func TestAFailedReturnDoesNotStopThePassRecordingTheVerdict(t *testing.T) {
	h, e := pauseHarness(t, StatusLive, map[string]any{"verdict": recordedAs(verdictIn(VerdictPaused, watchNow.Add(-2*time.Hour)))})
	returns := func() int { return len(e.callsNamed("datasyncReturnHeldInbound")) }

	h.engine.fail["datasyncReturnHeldInbound"] = errString("the database is away")
	watchPass(t, h, watchNow)
	if returns() != 1 || recordedState(e) == VerdictPaused {
		t.Fatalf("after a pass whose return failed: %d returns, recorded %q; want one try and the resume recorded all the same", returns(), recordedState(e))
	}
	if resumed := mapString(e.row["health"].(map[string]any)["verdict"].(map[string]any), "resumedAt"); resumed == "" {
		t.Error("the recorded verdict carries no resumedAt")
	}

	delete(h.engine.fail, "datasyncReturnHeldInbound")
	watchPass(t, h, watchNow.Add(5*time.Minute))
	if returns() != 2 {
		t.Fatalf("the pass after a failed return asked %d times in all, want a second try", returns())
	}
}

// THE EDGE KEEPS ONE JOB: the pass that sees a store leave its pause queues
// the backfills the store never started, once. A pass after the resume was
// recorded queues nothing, even for a domain that reads as never backfilled:
// that is the boot pass's to find, not every pass's.
func TestTheWatchQueuesAResumedStoresUnstartedBackfillsOnce(t *testing.T) {
	h, e := pauseHarness(t, StatusLive, map[string]any{"verdict": recordedAs(verdictIn(VerdictPaused, watchNow.Add(-2*time.Hour)))})
	customers := memqlsync.StateID(generated.ConceptID("customer"), ConnectorName, string(memqlsync.DirectionInbound), testStoreID)
	delete(e.states, customers)

	watchPass(t, h, watchNow)
	if got := strings.Join(e.queued(), ","); got != "customer" {
		t.Fatalf("the pass that saw the store resume queued %q, want the one domain no backfill ever read", got)
	}
	if recordedState(e) == VerdictPaused {
		t.Fatal("the resume was not recorded")
	}

	e.mu.Lock()
	delete(e.states, customers)
	e.mu.Unlock()
	for pass := 1; pass <= 2; pass++ {
		watchPass(t, h, watchNow.Add(time.Duration(pass)*5*time.Minute))
	}
	if got := strings.Join(e.queued(), ","); got != "customer" {
		t.Errorf("passes after the resume was recorded queued again: %q", got)
	}
}

// A RESUME WHOSE BACKFILLS COULD NOT BE QUEUED IS NOT RECORDED, so the next
// pass still sees the store leaving its pause and queues them then.
func TestAResumeWhoseBackfillsCouldNotBeQueuedIsSeenAgainByTheNextPass(t *testing.T) {
	h, e := pauseHarness(t, StatusLive, map[string]any{"verdict": recordedAs(verdictIn(VerdictPaused, watchNow.Add(-2*time.Hour)))})
	customers := memqlsync.StateID(generated.ConceptID("customer"), ConnectorName, string(memqlsync.DirectionInbound), testStoreID)
	delete(e.states, customers)
	queued := func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.states[customers] != nil
	}

	h.engine.fail["datasyncQueueBackfill"] = errString("the database is away")
	watchPass(t, h, watchNow)
	if queued() || recordedState(e) != VerdictPaused {
		t.Fatalf("a pass that could not queue the store's backfills: queued %t, recorded %q; want the store still recorded as paused", queued(), recordedState(e))
	}

	delete(h.engine.fail, "datasyncQueueBackfill")
	watchPass(t, h, watchNow.Add(5*time.Minute))
	if !queued() || recordedState(e) == VerdictPaused {
		t.Fatalf("the next pass: queued %t, recorded %q; want the customers queued and the resume recorded", queued(), recordedState(e))
	}
}
