package datasync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// shopify_hold_db_test.go -- a pause holds, and what is held comes back by
// state: a real engine over a real Postgres, the sync runtime and the Shopify
// connector both registered on it as on a node, the runtime's own dispatcher
// and retry pass, and Shopify scripted at the transport.
//
// Composed here for the reason shopify_partitions_db_test.go is: the runtime
// depends on the connector, and the integrations module cannot import back
// into the runtime. The fakes cannot say that the held read finds what the
// hold wrote, that the builtin the connector calls admits it, or that a
// queued backfill reads back as queued; this does.

// countingSeam is the engine as the registered sync runtime reaches it,
// counting the reads of held deliveries and the writes that put one back in
// line: what a watch pass costs a store.
type countingSeam struct {
	engineSeam
	mu     stdsync.Mutex
	reads  int
	writes int
}

func (c *countingSeam) Execute(ctx context.Context, q string) (any, error) {
	c.mu.Lock()
	switch {
	case strings.HasPrefix(q, "query inboundRequestsHeld("):
		c.reads++
	case strings.HasPrefix(q, "mutation requeueInboundRequest("):
		c.writes++
	}
	c.mu.Unlock()
	return c.engineSeam.Execute(ctx, q)
}

func (c *countingSeam) counts() (reads, writes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads, c.writes
}

// storeList is the engine as the connector's store registry reaches it: only
// this test's stores, and with a switch -- while it is away the list cannot
// be read at all, which is what a connector that cannot tell whose a delivery
// is looks like from outside.
type storeList struct {
	scopedStoreEngine
	away atomic.Bool
}

func (l *storeList) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if l.away.Load() && strings.HasPrefix(q, "storesForConnector(") {
		return nil, errors.New("the store list is away")
	}
	return l.scopedStoreEngine.Execute(ctx, q)
}

// holdRig is one store of this test's own on a real engine, with the runtime
// and the connector registered on it.
type holdRig struct {
	t       *testing.T
	eng     *memql.MemQLEngine
	db      *sql.DB
	conn    *shopify.Connector
	stores  *shopify.StoreRegistry
	list    *storeList
	origin  *scriptedOrigin
	runtime *countingSeam
	d       *Dispatcher
	runner  *Runner
	state   *Store
	// boot is the connector's integration, for the node's boot sequence
	// (shopify.Bootstrap).
	boot    *shopify.Integration
	storeID string
	source  string
	owner   string
	at      time.Time
	staged  int
	// clock is what time the connector says it read the origin at: the wall
	// clock, unless a test that decides something by the order of two reads
	// sets it.
	clock func() time.Time
}

// holdSeedCtx is server code acting as a cluster owner: how a test writes the
// rows a receiver or an operator would.
func holdSeedCtx() context.Context {
	claims := map[string]any{"sub": "dbtest-owner", "role": "owner"}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: "dbtest-owner", Role: auth.RoleOwner})
	return auth.ContextWithInternalOrigin(ctx)
}

func newHoldRig(t *testing.T) *holdRig {
	t.Helper()
	eng, db := healthEngine(t)
	if eng == nil {
		return nil
	}
	t.Setenv("MEMQL_DOMAIN", "example.test")
	suffix := fmt.Sprint(time.Now().UnixNano())
	r := &holdRig{t: t, eng: eng, db: db, storeID: "dbtest-hold-" + suffix, owner: "dbtest-hold-owner-" + suffix,
		at: time.Now().UTC().Truncate(time.Second)}
	r.source = shopify.ConnectorName + "-" + r.storeID
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND id = 'v1:shopify:store:' || $1`,
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND payload->>'source' LIKE 'shopify-' || $1 || '%'`,
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'partitionKey' = $1`,
			`DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:shopify:%' AND payload->>'storeId' = $1`,
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1`,
		} {
			_, _ = db.ExecContext(ctx, q, r.storeID)
		}
	})
	// The store as Connect Shopify leaves it: owned by a person who is not a
	// cluster owner, and in createStore's status, configured.
	if _, err := eng.Execute(holdSeedCtx(), call("mutation", "createStore",
		arg{"storeId", r.storeID}, arg{"domain", "acme-widgets.myshopify.com"}, arg{"name", r.storeID},
		arg{"adminTokenRef", "shopify-admin-" + r.storeID}, arg{"apiVersion", generated.APIVersion},
		arg{"ownerUserId", r.owner})); err != nil {
		t.Fatalf("write the store: %v", err)
	}

	r.origin = newScriptedOrigin(t)
	logger := slog.New(slog.DiscardHandler)
	r.list = &storeList{scopedStoreEngine: scopedStoreEngine{IntegrationEngineAccess: eng, stores: map[string]bool{r.storeID: true}}}
	r.stores = shopify.NewStoreRegistry(r.list, func(context.Context, string) (string, error) { return "shpat_test", nil })
	r.clock = time.Now
	r.conn = shopify.NewConnector(eng, logger, r.stores, shopify.NewAdminClient()).WithClock(func() time.Time { return r.clock() })
	r.boot = shopify.NewIntegration(r.conn)
	if err := eng.RegisterIntegration(r.boot); err != nil {
		t.Fatalf("register the connector: %v", err)
	}
	// The sync runtime, as every node registers it: a resume reaches its
	// builtins through the engine, and it finds the connector by its name.
	r.runtime = &countingSeam{engineSeam: engineSeam{eng}}
	if err := eng.RegisterIntegration(NewIntegration(r.runtime, logger)); err != nil {
		t.Fatalf("register the sync runtime: %v", err)
	}
	memqlsync.UnbindForTest(shopify.ConnectorName)
	if err := memqlsync.Bind(r.conn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(shopify.ConnectorName) })
	r.state = NewStore(engineSeam{eng})
	applier := NewApplier(r.state, NewEngineMirrorWriter(engineSeam{eng}))
	r.d = NewDispatcher(r.state, applier)
	r.runner = NewRunner(r.state, applier, logger)
	return r
}

// opCtx is the operator, as the runtime's passes run. It is NOT a fresh read:
// the connector's store registry reads its whole list again for one, and a
// dispatch asks it twice.
func (r *holdRig) opCtx() context.Context { return OperatorContext(context.Background()) }

// freshCtx is the operator reading past the result cache: for a row this test
// just wrote and reads back.
func (r *holdRig) freshCtx() context.Context { return memql.ContextWithFreshRead(r.opCtx()) }

// store is the store's row as the connector reads it now.
func (r *holdRig) store() shopify.Store {
	r.t.Helper()
	r.stores.Invalidate()
	s, ok := r.stores.ByID(context.Background(), r.storeID)
	if !ok {
		r.t.Fatal("the store did not resolve")
	}
	return s
}

// setStatus writes the store's status directly, as an operator's own
// setStoreStatus does: no act of the Store page, and nothing but the status.
func (r *holdRig) setStatus(status string) {
	r.t.Helper()
	if _, err := r.eng.Execute(holdSeedCtx(), call("mutation", "setStoreStatus", arg{"storeId", r.storeID}, arg{"status", status})); err != nil {
		r.t.Fatalf("set the status to %s: %v", status, err)
	}
	r.stores.Invalidate()
}

// setRecording is the Store page's Pause recording and Resume, as the store's
// owner: a developer, not a cluster owner, calling from a client.
func (r *holdRig) setRecording(recording bool) map[string]any {
	r.t.Helper()
	ctx := auth.ContextWithClaims(context.Background(), map[string]any{"sub": r.owner, "role": string(auth.RoleDeveloper), "sid": "sess-" + r.owner})
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: r.owner, Role: auth.RoleDeveloper})
	ctx = auth.ContextWithClientOrigin(auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: r.owner}))
	return r.builtin(ctx, call("builtin", "shopifyStoreSetRecording", arg{"storeId", r.storeID}, arg{"recording", recording}))
}

// builtin runs one builtin and returns its one reply.
func (r *holdRig) builtin(ctx context.Context, q string) map[string]any {
	r.t.Helper()
	res, err := r.eng.Execute(ctx, q)
	if err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
	nodes, _ := res.OutputPayload().(map[string]memorynodes.MemoryNode)
	if len(nodes) != 1 {
		r.t.Fatalf("%s answered %d nodes", q, len(nodes))
	}
	var out map[string]any
	for _, n := range nodes {
		if err := json.Unmarshal(n.Payload, &out); err != nil {
			r.t.Fatalf("%s: decode: %v", q, err)
		}
	}
	return out
}

// storeListAway makes the connector's store list unreadable, or readable
// again.
func (r *holdRig) storeListAway(away bool) {
	r.list.away.Store(away)
	r.stores.Invalidate()
}

// stage writes one delivery of the store's own source as the receiver stages
// it, received now, and returns its id. Now matters for a delete notice: it is
// the time of the origin's announcement, and it stands against every read
// made before it.
func (r *holdRig) stage(name, topic, gid string) string {
	r.t.Helper()
	return r.stageOn(r.source, name, topic, gid)
}

// stageOn is stage for a delivery of any source.
func (r *holdRig) stageOn(source, name, topic, gid string) string {
	r.t.Helper()
	r.staged++
	id := source + "-" + name
	body, _ := json.Marshal(map[string]any{"admin_graphql_api_id": gid})
	headers, _ := json.Marshal(map[string]string{"x-shopify-topic": topic, "x-shopify-webhook-id": fmt.Sprintf("wh-%s-%d", name, r.staged)})
	if _, err := r.state.exec(r.opCtx(), call("mutation", "stageInboundRequest",
		arg{"requestId", id}, arg{"source", source}, arg{"medium", "webhook"},
		arg{"body", string(body)}, arg{"headersJson", string(headers)},
		arg{"signatureVerified", true}, arg{"verifiedBy", memqlsync.VerifiedByConnector},
		arg{"receivedAt", time.Now().UTC().Format(time.RFC3339Nano)})); err != nil {
		r.t.Fatalf("stage %s: %v", name, err)
	}
	return id
}

// dispatch works the delivery as it is stored now, the way the event path and
// the retry pass both do.
func (r *holdRig) dispatch(id string) DispatchResult {
	r.t.Helper()
	res, _ := r.dispatchErr(id)
	return res
}

// dispatchErr is dispatch, with the failure the dispatcher returned.
func (r *holdRig) dispatchErr(id string) (DispatchResult, error) {
	r.t.Helper()
	staged, found, err := r.d.Staged(r.opCtx(), id)
	if err != nil || !found {
		r.t.Fatalf("read delivery %s: found %t, %v", id, found, err)
	}
	return r.d.DispatchStaged(r.opCtx(), staged)
}

// versions counts the stored versions of one delivery: every write of it.
func (r *holdRig) versions(id string) int {
	r.t.Helper()
	var n int
	if err := r.db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE id = $1`, "v1:platform:inboundRequest:"+id).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// delivery is one staged delivery's stored fields.
func (r *holdRig) delivery(id string) map[string]any {
	r.t.Helper()
	rows, err := r.state.rows(r.freshCtx(), call("query", "inboundRequestById", arg{"requestId", id}))
	if err != nil || len(rows) != 1 {
		r.t.Fatalf("delivery %s read %d rows, %v", id, len(rows), err)
	}
	return rows[0]
}

// isHeld reports whether a delivery's row is held: failed, the pause as its
// reason, no next try, and `attempts` tries counted.
func isHeld(row map[string]any, attempts int) bool {
	return stringField(row, "status") == "failed" && stringField(row, "lastError") == memqlsync.ErrRecordingPaused.Error() &&
		stringField(row, "nextAttemptAt") == "" && intField(row, "attempts") == attempts
}

// isBackInLine reports whether a delivery's row was put back in line and not
// yet worked: failed, due, no tries counted.
func isBackInLine(row map[string]any) bool {
	return stringField(row, "status") == "failed" && stringField(row, "nextAttemptAt") != "" && intField(row, "attempts") == 0
}

func (r *holdRig) held(id string, attempts int) bool { return isHeld(r.delivery(id), attempts) }

func (r *holdRig) backInLine(id string) bool { return isBackInLine(r.delivery(id)) }

// watch is one pass of the store watch.
func (r *holdRig) watch() {
	r.t.Helper()
	r.stores.Invalidate()
	if err := r.conn.WatchStores(context.Background()); err != nil {
		r.t.Fatalf("the watch pass: %v", err)
	}
}

// retryPass runs the runtime's retry pass until it tries nothing more.
func (r *holdRig) retryPass() {
	r.t.Helper()
	for pass := 0; pass < 40; pass++ {
		res, err := r.d.RetryDue(r.opCtx())
		if err != nil {
			r.t.Fatalf("the retry pass: %v", err)
		}
		if res.Tried == 0 {
			return
		}
	}
	r.t.Fatal("the retry pass kept finding deliveries to try")
}

// records has the mirror hold a product, by the way it comes to hold one: a
// delivery of the store while it records, fetched from the origin and applied.
func (r *holdRig) records(gid, title string) {
	r.t.Helper()
	updated := r.at.Add(-2 * time.Hour).Format(time.RFC3339)
	r.origin.reply("ShopifyFetchProduct", map[string]any{"node": map[string]any{"__typename": "Product", "id": gid, "title": title, "updatedAt": updated}})
	id := r.stage("recorded-"+strings.TrimPrefix(gid, "gid://shopify/Product/"), "products/update", gid)
	if res := r.dispatch(id); res.Applied == 0 {
		r.t.Fatalf("the delivery that records %s applied nothing: %+v, row %v", gid, res, r.delivery(id))
	}
}

// record is the mirror's latest version of a product, or nil.
func (r *holdRig) record(gid string) map[string]any {
	r.t.Helper()
	var raw []byte
	err := r.db.QueryRowContext(context.Background(),
		`SELECT payload FROM "MemoryNodes" WHERE concept = $1 AND payload->>'gid' = $2 AND payload->>'storeId' = $3 ORDER BY "createdAt" DESC LIMIT 1`,
		generated.ConceptID("product"), gid, r.storeID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		r.t.Fatalf("read %s: %v", gid, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// title is a product's title in the mirror, "" for one it does not hold.
func (r *holdRig) title(gid string) string {
	title, _ := r.record(gid)["title"].(string)
	return title
}

// syncState is one domain's health row of the store.
func (r *holdRig) syncState(concept string) SyncState {
	r.t.Helper()
	st, err := r.state.SyncStateFor(r.freshCtx(), generated.ConceptID(concept), shopify.ConnectorName, string(memqlsync.DirectionInbound), r.storeID)
	if err != nil {
		r.t.Fatalf("read %s's sync state: %v", concept, err)
	}
	return st
}

// backfill is one domain's backfill status on the store's own row, or "no
// row".
func (r *holdRig) backfill(concept string) string {
	r.t.Helper()
	st := r.syncState(concept)
	if !st.Stored {
		return "no row"
	}
	return st.BackfillStatus
}

// backfillToItsEnd runs one domain's backfill through the runtime's runner
// until its row reads complete: the export is asynchronous, so the first step
// only starts it.
func (r *holdRig) backfillToItsEnd(concept string) {
	r.t.Helper()
	for i := 0; i < 5 && r.backfill(concept) != "complete"; i++ {
		if _, err := r.runner.StartBackfill(context.Background(), shopify.ConnectorName, generated.ConceptID(concept), r.storeID); err != nil {
			r.t.Fatalf("backfill %s: %v", concept, err)
		}
	}
	if st := r.syncState(concept); st.BackfillStatus != "complete" {
		r.t.Fatalf("%s did not complete: %+v", concept, st)
	}
}

// reasons is the store's verdict as the Store page is answered it: its state,
// then the code of each reason.
func (r *holdRig) reasons() []string {
	r.t.Helper()
	r.stores.Invalidate()
	reply := r.builtin(holdSeedCtx(), call("builtin", "shopifyStoreVerdict", arg{"storeId", r.storeID}))
	out := []string{stringField(reply, "state")}
	list, _ := reply["reasons"].([]any)
	for _, item := range list {
		if reason, ok := item.(map[string]any); ok {
			out = append(out, stringField(reason, "code"))
		}
	}
	return out
}

// recordedVerdict is the verdict the watch last recorded on the store.
func (r *holdRig) recordedVerdict() map[string]any {
	v, _ := r.store().Health["verdict"].(map[string]any)
	return v
}

// A STORE PAUSED AND RESUMED BETWEEN TWO PASSES, BY TWO DIRECT WRITES, GETS ITS
// HELD DELIVERY BACK. Nothing saw the pause: no act of the Store page ran, and
// the watch never recorded the store as paused, so there is no resume for
// anything to notice. The delivery held in between is returned all the same by
// the next pass, because the pass asks by state: the store ingests, and it
// holds one.
func TestAStorePausedAndResumedBetweenTwoPassesGetsItsHeldDeliveryBack(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	const gone = "gid://shopify/Product/8101"
	r.records(gone, "Deleted during the pause")
	r.watch()
	if state := stringField(r.recordedVerdict(), "state"); state == shopify.VerdictPaused || state == "" {
		t.Fatalf("the first pass recorded %q, want the store as it records", state)
	}

	r.setStatus(shopify.StatusPaused)
	deleted := r.stage("deleted", "products/delete", gone)
	if res := r.dispatch(deleted); !res.Held || !r.held(deleted, 0) {
		t.Fatalf("the delivery of the paused store is %v (%+v), want held", r.delivery(deleted), res)
	}
	r.setStatus(shopify.StatusLive)
	if !r.held(deleted, 0) {
		t.Fatalf("a direct status write moved the delivery: %v", r.delivery(deleted))
	}

	r.watch()
	if !r.backInLine(deleted) {
		t.Fatalf("after the next pass the delivery is %v, want back in line: failed, due, no tries counted", r.delivery(deleted))
	}
	// The watch never saw a pause, so it recorded no resume: the return was
	// not the edge's.
	if resumed := stringField(r.recordedVerdict(), "resumedAt"); resumed != "" {
		t.Fatalf("the watch recorded a resume at %s; this test must return the delivery without one", resumed)
	}
	r.retryPass()
	if row := r.record(gone); row["deleted"] != true {
		t.Errorf("the record deleted during the pause is still live: %v", row)
	}
	if row := r.delivery(deleted); stringField(row, "status") != "processed" {
		t.Errorf("the delivery is %v, want processed", row)
	}
}

// THE ACT RETURNS ONE PAGE AND ANSWERS; THE WATCH RETURNS THE REST. A store
// with a hundred and twenty held deliveries, resumed through the Store page:
// fifty are back in line when the act answers, and the other seventy after
// the next pass. While the store is paused a pass returns none of them.
func TestAResumeThroughTheActReturnsOnePageAndTheWatchReturnsTheRest(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	const held = 120
	if reply := r.setRecording(false); reply["status"] != shopify.StatusPaused {
		t.Fatalf("Pause recording answered %v", reply)
	}
	ids := make([]string, 0, held)
	for i := 0; i < held; i++ {
		id := r.stage(fmt.Sprintf("held-%03d", i), "products/delete", fmt.Sprintf("gid://shopify/Product/%d", 820000+i))
		if res := r.dispatch(id); !res.Held {
			t.Fatalf("delivery %d of the paused store: %+v, want held", i, res)
		}
		ids = append(ids, id)
	}
	count := func() (back, stillHeld int) {
		for _, id := range ids {
			switch row := r.delivery(id); {
			case isBackInLine(row):
				back++
			case isHeld(row, 0):
				stillHeld++
			}
		}
		return back, stillHeld
	}

	// A pass over the paused store returns nothing: it does not ingest.
	r.watch()
	if back, stillHeld := count(); back != 0 || stillHeld != held {
		t.Fatalf("a pass over the paused store left %d back in line and %d held, want none and all %d", back, stillHeld, held)
	}
	if reads, writes := r.runtime.counts(); reads != 0 || writes != 0 {
		t.Fatalf("a pass over the paused store read the held deliveries %d times and wrote %d, want it never asked", reads, writes)
	}

	if reply := r.setRecording(true); reply["status"] != shopify.StatusLive {
		t.Fatalf("Resume answered %v", reply)
	}
	if back, stillHeld := count(); back != 50 || stillHeld != held-50 {
		t.Fatalf("when Resume answered, %d were back in line and %d held; want one page of fifty and the other %d still held", back, stillHeld, held-50)
	}
	if reads, _ := r.runtime.counts(); reads != 1 {
		t.Errorf("Resume read the held deliveries %d times, want once: it does not wait for more", reads)
	}

	r.watch()
	if back, stillHeld := count(); back != held || stillHeld != 0 {
		t.Fatalf("after the next pass %d are back in line and %d held; want all %d back and none held", back, stillHeld, held)
	}
}

// A PASS OVER A STORE WITH NOTHING HELD IS ONE READ AND NO WRITE, however
// often it is made. The cost of one such return against the real engine is
// logged (go test -v), because every store that ingests pays it every pass.
func TestAPassOverAStoreWithNothingHeldIsOneReadAndNoWrite(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	versions := func() int {
		var n int
		if err := r.db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND payload->>'source' = $1`, r.source).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// A delivery that is neither held nor due: processed, as most are.
	r.records("gid://shopify/Product/8301", "Recorded")
	before := versions()
	const passes = 3
	for i := 0; i < passes; i++ {
		r.watch()
	}
	if reads, writes := r.runtime.counts(); reads != passes || writes != 0 {
		t.Fatalf("%d passes read the held deliveries %d times and wrote %d; want one read a pass and no write", passes, reads, writes)
	}
	if after := versions(); after != before {
		t.Fatalf("the passes wrote %d versions of the store's deliveries", after-before)
	}

	const calls = 200
	start := time.Now()
	for i := 0; i < calls; i++ {
		if n, more, err := r.d.ReturnHeld(r.opCtx(), r.source, 0); err != nil || n != 0 || more {
			t.Fatalf("a return of nothing answered %d, more %t, %v", n, more, err)
		}
	}
	t.Logf("one return of nothing, against the real engine: %s (mean of %d)", (time.Since(start) / calls).Round(10*time.Microsecond), calls)
}

// ---- moved from the connector's package (it needed the runtime) ----

// productNode is a product as the origin answers a fetch of it.
func productNode(gid, title, updatedAt string) map[string]any {
	return map[string]any{"node": map[string]any{"__typename": "Product", "id": gid, "title": title, "updatedAt": updatedAt}}
}

func containsReason(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// A DELIVERY THAT ARRIVES DURING A PAUSE IS HELD, AND WORKED WHEN THE STORE
// RESUMES. A delete notice and an update notice arrive while the store is
// paused: each is held with the pause as its reason and no try counted, the
// origin is not asked, and the mirror is untouched. Resume through the Store
// page's act puts both back in line and queues the backfill the store never
// started; the retry pass then retires the deleted record and writes the
// updated one. The watch's next pass and a second Resume move nothing twice.
func TestADeliveryThatArrivesDuringAPauseIsHeldAndWorkedOnResume(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	const gone, changed = "gid://shopify/Product/7001", "gid://shopify/Product/7002"
	r.records(gone, "Deleted during the pause")
	r.records(changed, "Before the pause")
	// The products were read whole before the pause, by an export that held
	// nothing more; the customers were never reached.
	r.origin.reply("ShopifyBulkRun", originStartedOperation("gid://shopify/BulkOperation/"+r.storeID))
	r.origin.reply("ShopifyBulkStatus", map[string]any{"node": map[string]any{"id": "gid://shopify/BulkOperation/" + r.storeID, "status": "COMPLETED", "url": nil}})
	r.backfillToItsEnd("product")

	if reply := r.setRecording(false); reply["status"] != shopify.StatusPaused {
		t.Fatalf("Pause recording answered %v", reply)
	}
	if got := r.store().Status; got != shopify.StatusPaused {
		t.Fatalf("after Pause recording the store reads %q", got)
	}
	r.origin.reply("ShopifyFetchProduct", productNode(changed, "Changed during the pause", time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)))
	asked := len(r.origin.seen())
	deleted := r.stage("deleted", "products/delete", gone)
	updated := r.stage("updated", "products/update", changed)
	for _, id := range []string{deleted, updated} {
		if res := r.dispatch(id); !res.Held {
			t.Fatalf("delivery %s of a paused store: %+v, want it held", id, res)
		}
		if !r.held(id, 0) {
			t.Fatalf("delivery %s is %v, want held: failed, the pause as its reason, no next time and no try counted", id, r.delivery(id))
		}
	}
	if row := r.record(gone); row["deleted"] == true {
		t.Fatal("a delete notice of a paused store retired the record")
	}
	if got := r.title(changed); got != "Before the pause" {
		t.Fatalf("an update notice of a paused store wrote %q", got)
	}
	if n := len(r.origin.seen()) - asked; n != 0 {
		t.Fatalf("the origin was asked %d times for a paused store", n)
	}
	if got := r.reasons(); len(got) != 1 || got[0] != shopify.VerdictPaused {
		t.Fatalf("the verdict of the paused store is %v, want paused and no reason", got)
	}

	if reply := r.setRecording(true); reply["status"] != shopify.StatusLive {
		t.Fatalf("Resume answered %v", reply)
	}
	for _, id := range []string{deleted, updated} {
		if !r.backInLine(id) {
			t.Fatalf("after Resume delivery %s is %v, want back in line: failed, due, no tries counted", id, r.delivery(id))
		}
	}
	if got := r.backfill("customer"); got != "queued" {
		t.Errorf("the customers, never backfilled before the pause, read %q after Resume, want queued", got)
	}
	if got := r.backfill("product"); got != "complete" {
		t.Errorf("the products, read whole before the pause, read %q after Resume, want them left complete", got)
	}
	if got := r.reasons(); containsReason(got, shopify.ReasonChangesNotRecorded) {
		t.Errorf("after Resume the verdict counts the returned deliveries as changes that could not be recorded: %v", got)
	}

	// The watch notices the same resume on its next pass, and the act may be
	// repeated: neither moves a delivery that is already back in line.
	before := r.versions(deleted) + r.versions(updated)
	r.watch()
	if reply := r.setRecording(true); reply["status"] != shopify.StatusLive {
		t.Fatalf("a second Resume answered %v", reply)
	}
	if after := r.versions(deleted) + r.versions(updated); after != before {
		t.Fatalf("the watch's pass and a second Resume wrote the deliveries %d more times", after-before)
	}
	if got := r.backfill("customer"); got != "queued" {
		t.Errorf("the watch's pass and a second Resume left the customers %q", got)
	}

	fetched := r.origin.countOp("ShopifyFetchProduct")
	r.retryPass()
	for _, id := range []string{deleted, updated} {
		if row := r.delivery(id); stringField(row, "status") != "processed" || intField(row, "attempts") != 1 {
			t.Fatalf("after the retry pass delivery %s is %v, want processed at its one counted try", id, row)
		}
	}
	if row := r.record(gone); row["deleted"] != true {
		t.Errorf("the record deleted during the pause is still live after Resume: %v", row)
	}
	if row := r.record(changed); row["title"] != "Changed during the pause" || row["deleted"] == true {
		t.Errorf("the record changed during the pause is %v after Resume", row)
	}
	if n := r.origin.countOp("ShopifyFetchProduct") - fetched; n != 1 {
		t.Errorf("the changed record was fetched %d times, want once: nothing was dispatched twice", n)
	}
}

// A RESUME NOBODY ACTED cannot run the act: the status was written directly.
// The watch recorded the store as paused, so the pass that sees it recording
// again records the resume, and returns what was held; a pass after that
// writes nothing more.
func TestAStoreResumedByARawStatusChangeGetsItsHeldDeliveriesAtTheNextWatchPass(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	const gone = "gid://shopify/Product/7101"
	r.records(gone, "Deleted during the pause")

	r.setStatus(shopify.StatusPaused)
	r.watch()
	if v := r.recordedVerdict(); stringField(v, "state") != shopify.VerdictPaused {
		t.Fatalf("the watch recorded %v, want paused", v)
	}
	deleted := r.stage("deleted", "products/delete", gone)
	if res := r.dispatch(deleted); !res.Held || !r.held(deleted, 0) {
		t.Fatalf("the delivery is %v (%+v), want held", r.delivery(deleted), res)
	}

	r.setStatus(shopify.StatusLive)
	// Nothing has returned it yet, and it is not a reason either way.
	if !r.held(deleted, 0) {
		t.Fatalf("a raw status change moved the delivery: %v", r.delivery(deleted))
	}
	if got := r.reasons(); containsReason(got, shopify.ReasonChangesNotRecorded) {
		t.Fatalf("a held delivery of a resumed store is counted as a change that could not be recorded: %v", got)
	}

	r.watch()
	if !r.backInLine(deleted) {
		t.Fatalf("after the watch pass the delivery is %v, want back in line", r.delivery(deleted))
	}
	recorded := r.recordedVerdict()
	if stringField(recorded, "state") == shopify.VerdictPaused || stringField(recorded, "resumedAt") == "" {
		t.Fatalf("the watch recorded %v, want the resume", recorded)
	}
	r.retryPass()
	if row := r.record(gone); row["deleted"] != true {
		t.Errorf("the record deleted during the pause is still live: %v", row)
	}

	written := r.versions(deleted)
	r.watch()
	if got := r.versions(deleted); got != written {
		t.Errorf("a pass after the resume was recorded wrote the delivery %d more times", got-written)
	}
}

// A HELD DELIVERY IS NEVER "A CHANGE THAT COULD NOT BE RECORDED". The verdict
// counts the deliveries that used their four tries, and a hold does not count
// a try: a delivery three tries in when its store is paused is held at three,
// so neither the paused store nor the resumed one shows a reason for it.
func TestAHeldDeliveryIsNotAChangeThatCouldNotBeRecorded(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	const flaky = "gid://shopify/Product/7201"
	r.records(flaky, "Unreachable three times")
	r.origin.httpStatus("ShopifyFetchProduct", http.StatusServiceUnavailable)
	id := r.stage("flaky", "products/update", flaky)
	for try := 1; try <= 3; try++ {
		r.dispatch(id)
		if got := intField(r.delivery(id), "attempts"); got != try {
			t.Fatalf("after try %d the delivery counts %d tries: %v", try, got, r.delivery(id))
		}
	}
	r.setStatus(shopify.StatusPaused)
	if res := r.dispatch(id); !res.Held || !r.held(id, 3) {
		t.Fatalf("on its fourth dispatch, with the store paused, the delivery is %v (%+v); want held with its three tries as they were", r.delivery(id), res)
	}
	if got := r.reasons(); len(got) != 1 || got[0] != shopify.VerdictPaused {
		t.Fatalf("paused: the verdict is %v, want paused and no reason", got)
	}
	r.setStatus(shopify.StatusLive)
	if got := r.reasons(); containsReason(got, shopify.ReasonChangesNotRecorded) {
		t.Fatalf("resumed: the verdict is %v; a held delivery is counted as a change that could not be recorded", got)
	}
}

// A STORE THAT IS NOT CONNECTED IS NOT WAITING FOR A RESUME. A delivery of a
// store Shopify reported uninstalled is worked as before: nothing is asked,
// nothing is written, and the delivery is processed.
func TestAnUninstalledStoresDeliveryIsProcessedWithNothingWritten(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	const gid = "gid://shopify/Product/7301"
	r.records(gid, "Still here")
	if _, err := r.eng.Execute(holdSeedCtx(), call("mutation", "markStoreUninstalled",
		arg{"storeId", r.storeID}, arg{"uninstalledAt", r.at.Add(-time.Hour).Format(time.RFC3339)})); err != nil {
		t.Fatalf("mark the store uninstalled: %v", err)
	}
	r.stores.Invalidate()
	asked := len(r.origin.seen())
	id := r.stage("after-uninstall", "products/delete", gid)
	if res := r.dispatch(id); res.Held || !res.Handled {
		t.Fatalf("the delivery of an uninstalled store: %+v, want handled and not held", res)
	}
	if row := r.delivery(id); stringField(row, "status") != "processed" || stringField(row, "nextAttemptAt") != "" {
		t.Fatalf("the delivery is %v, want processed", row)
	}
	if row := r.record(gid); row["deleted"] == true {
		t.Errorf("a delivery of an uninstalled store retired the record: %v", row)
	}
	if n := len(r.origin.seen()) - asked; n != 0 {
		t.Errorf("the origin was asked %d times for an uninstalled store", n)
	}
}
