package shopify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/datasync"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
	"google.golang.org/protobuf/types/known/structpb"
)

// pause_hold_db_test.go -- a pause holds and a resume returns, end to end:
// a real engine over a real Postgres, the sync runtime and this connector
// both registered on it as on a node, the runtime's own dispatcher and retry
// pass, and the origin faked over HTTP. The fakes cannot say that the held
// read finds what the hold wrote, that the builtin the connector calls admits
// it, or that a queued backfill reads back as queued; this does.

// scopedStores lets the connector see only this test's store: the database is
// shared, and the watch walks every store it is given.
type scopedStores struct {
	memql.IntegrationEngineAccess
	ids map[string]bool
}

func (e scopedStores) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	res, err := e.IntegrationEngineAccess.Execute(ctx, q)
	if err != nil || !strings.HasPrefix(q, "storesForConnector(") {
		return res, err
	}
	var nodes []*memqlv1.MemoryNode
	for _, row := range memql.MaterializeRows(res) {
		id, _ := row["id"].(string)
		if !e.ids[memql.BareShortId(id)] {
			continue
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		payload := map[string]any{}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, err
		}
		delete(payload, "id")
		pb, err := structpb.NewStruct(payload)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, &memqlv1.MemoryNode{Id: id, Payload: pb})
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}, Meta: res.Meta}, nil
}

// syncRuntimeOn registers the sync runtime on the engine, as every node does,
// and binds c as the Shopify connector for the test: a resume reaches the
// runtime's builtins through the engine, and the runtime finds the connector
// by its name.
func syncRuntimeOn(t *testing.T, eng *memql.MemQLEngine, c *Connector) {
	t.Helper()
	if err := eng.RegisterIntegration(datasync.NewIntegration(anyEngine{eng}, slog.New(slog.DiscardHandler))); err != nil {
		t.Fatalf("register the sync runtime: %v", err)
	}
	memqlsync.UnbindForTest(ConnectorName)
	if err := memqlsync.Bind(c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(ConnectorName) })
}

// cleanStoreRows removes what a test left under one store: its staged
// deliveries, its sync state, its mirror rows and its audit events.
func cleanStoreRows(t *testing.T, db *sql.DB, storeID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND payload->>'source' = $1`, storeInboundSource(storeID))
		_, _ = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'partitionKey' = $1`, storeID)
		_, _ = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept LIKE 'v1:shopify:%' AND payload->>'storeId' = $1`, storeID)
		_, _ = db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1`, storeID)
	})
}

type pauseRig struct {
	t       *testing.T
	eng     *memql.MemQLEngine
	db      *sql.DB
	c       *Connector
	d       *datasync.Dispatcher
	state   *datasync.Store
	admin   *fakeAdmin
	storeID string
	source  string
	owner   string
	at      time.Time
}

func newPauseRig(t *testing.T) *pauseRig {
	t.Helper()
	eng, db := authorityEngine(t)
	if eng == nil {
		return nil
	}
	t.Setenv("MEMQL_DOMAIN", "example.test")
	suffix := fmt.Sprint(time.Now().UnixNano())
	r := &pauseRig{t: t, eng: eng, db: db, storeID: "dbtest-pause-" + suffix, owner: "dbtest-pause-owner-" + suffix,
		at: time.Now().UTC().Truncate(time.Second)}
	r.source = storeInboundSource(r.storeID)
	seedOwnedStore(t, eng, db, r.storeID, r.owner)
	cleanStoreRows(t, db, r.storeID)

	h := newHarness(t)
	r.admin = h.admin
	logger := slog.New(slog.DiscardHandler)
	stores := NewStoreRegistry(scopedStores{IntegrationEngineAccess: eng, ids: map[string]bool{r.storeID: true}},
		func(context.Context, string) (string, error) { return "shpat_test", nil })
	stores.ttl = 0
	r.c = NewConnector(eng, logger, stores, h.conn.admin)
	if err := eng.RegisterIntegration(NewIntegration(r.c)); err != nil {
		t.Fatal(err)
	}
	syncRuntimeOn(t, eng, r.c)
	r.state = datasync.NewStore(anyEngine{eng})
	r.d = datasync.NewDispatcher(r.state, datasync.NewApplier(r.state, datasync.NewEngineMirrorWriter(anyEngine{eng})))
	return r
}

func (r *pauseRig) store() Store {
	r.t.Helper()
	s, found, err := r.c.storeByID(memql.ContextWithFreshRead(operatorContext(context.Background())), r.storeID)
	if err != nil || !found {
		r.t.Fatalf("read the store: found %t, %v", found, err)
	}
	return s
}

// setStatus writes the store's status directly, as an operator's own
// setStoreStatus does: no act of the Store page, and nothing but the status.
func (r *pauseRig) setStatus(status string) {
	r.t.Helper()
	if _, err := r.eng.Execute(seedCtx(), renderCall("setStoreStatus", map[string]any{"storeId": r.storeID, "status": status})); err != nil {
		r.t.Fatalf("set the status to %s: %v", status, err)
	}
	r.c.stores.Invalidate()
}

// setRecording is the Store page's Pause recording and Resume, as the store's
// owner: a developer, not a cluster owner.
func (r *pauseRig) setRecording(recording bool) {
	r.t.Helper()
	builtinReply(r.t, r.eng, clientCall(r.owner, auth.RoleDeveloper),
		fmt.Sprintf("builtin shopifyStoreSetRecording(storeId: %q, recording: %t)", r.storeID, recording))
}

// holds writes a product the mirror holds, read an hour ago.
func (r *pauseRig) holds(gid, title string) {
	r.t.Helper()
	obj := map[string]any{"id": gid, "title": title, "updatedAt": r.at.Add(-2 * time.Hour).Format(time.RFC3339)}
	for _, w := range mapObject(generated.Types["product"], r.storeID, obj, "", r.at.Add(-time.Hour)) {
		if err := r.c.writeMirror(context.Background(), w); err != nil {
			r.t.Fatalf("write %s: %v", gid, err)
		}
	}
}

// record is the mirror's latest version of a product, or nil.
func (r *pauseRig) record(gid string) map[string]any {
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

// stage writes one delivery of the store's own source as the receiver stages
// it, received `ago` before now, and returns its id.
func (r *pauseRig) stage(name, topic, gid string, ago time.Duration) string {
	r.t.Helper()
	id := r.source + "-" + name
	body, _ := json.Marshal(map[string]any{"admin_graphql_api_id": gid})
	headers, _ := json.Marshal(map[string]string{"x-shopify-topic": topic, "x-shopify-webhook-id": "wh-" + name})
	if _, err := r.eng.Execute(seedCtx(), renderCall("stageInboundRequest", map[string]any{
		"requestId": id, "source": r.source, "medium": "webhook", "body": string(body), "headersJson": string(headers),
		"signatureVerified": true, "verifiedBy": memqlsync.VerifiedByConnector, "receivedAt": r.at.Add(-ago).Format(time.RFC3339),
	})); err != nil {
		r.t.Fatalf("stage %s: %v", name, err)
	}
	return id
}

// dispatch works the delivery as it is stored now, the way the event path and
// the retry pass both do.
func (r *pauseRig) dispatch(id string) datasync.DispatchResult {
	r.t.Helper()
	ctx := datasync.OperatorContext(context.Background())
	staged, found, err := r.d.Staged(ctx, id)
	if err != nil || !found {
		r.t.Fatalf("read delivery %s: found %t, %v", id, found, err)
	}
	res, _ := r.d.DispatchStaged(ctx, staged)
	return res
}

// delivery is one staged delivery's stored fields.
func (r *pauseRig) delivery(id string) map[string]any {
	r.t.Helper()
	res, err := r.eng.Execute(memql.ContextWithFreshRead(operatorContext(context.Background())), "query "+renderCall("inboundRequestById", map[string]any{"requestId": id}))
	if err != nil {
		r.t.Fatalf("read delivery %s: %v", id, err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		r.t.Fatalf("delivery %s read %d rows", id, len(rows))
	}
	return rows[0]
}

// versions counts the stored versions of one delivery: every write of it.
func (r *pauseRig) versions(id string) int {
	r.t.Helper()
	var n int
	if err := r.db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE id = $1`, "v1:platform:inboundRequest:"+id).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// held reports whether a delivery is held: failed, the pause as its reason,
// no next try, and `attempts` tries counted.
func (r *pauseRig) held(id string, attempts int) bool {
	row := r.delivery(id)
	return mapString(row, "status") == "failed" && mapString(row, "lastError") == memqlsync.ErrRecordingPaused.Error() &&
		mapString(row, "nextAttemptAt") == "" && mapInt(row, "attempts") == attempts
}

// retryPass runs the runtime's retry pass until it tries nothing more.
func (r *pauseRig) retryPass() {
	r.t.Helper()
	ctx := datasync.OperatorContext(context.Background())
	for pass := 0; pass < 20; pass++ {
		res, err := r.d.RetryDue(ctx)
		if err != nil {
			r.t.Fatalf("the retry pass: %v", err)
		}
		if res.Tried == 0 {
			return
		}
	}
	r.t.Fatal("the retry pass kept finding deliveries to try")
}

// backfill is one domain's backfill status on the store's own row.
func (r *pauseRig) backfill(concept string) string {
	r.t.Helper()
	st, err := r.state.SyncStateFor(memql.ContextWithFreshRead(datasync.OperatorContext(context.Background())),
		generated.ConceptID(concept), ConnectorName, string(memqlsync.DirectionInbound), r.storeID)
	if err != nil {
		r.t.Fatalf("read %s's sync state: %v", concept, err)
	}
	if !st.Stored {
		return "no row"
	}
	return st.BackfillStatus
}

// readWhole records a domain as backfilled whole, under the rule.
func (r *pauseRig) readWhole(concept string) {
	r.t.Helper()
	if err := r.state.PatchSyncState(datasync.OperatorContext(context.Background()), generated.ConceptID(concept), ConnectorName,
		string(memqlsync.DirectionInbound), r.storeID, func(datasync.SyncState) (datasync.SyncPatch, bool) {
			return datasync.SyncPatch{"backfillStatus": "complete", "backfillCursor": backfillCursor{Rule: originVersionRule}.encode()}, true
		}); err != nil {
		r.t.Fatalf("record %s as read whole: %v", concept, err)
	}
}

func (r *pauseRig) reasons() []string {
	r.t.Helper()
	v, err := r.c.storeVerdict(context.Background(), r.store())
	if err != nil {
		r.t.Fatalf("the verdict: %v", err)
	}
	out := []string{v.State}
	for _, reason := range v.Reasons {
		out = append(out, reason.Code)
	}
	return out
}

// A DELIVERY THAT ARRIVES DURING A PAUSE IS HELD, AND WORKED WHEN THE STORE
// RESUMES. A delete notice and an update notice arrive while the store is
// paused: each is held with the pause as its reason and no try counted, the
// origin is not asked, and the mirror is untouched. Resume through the Store
// page's act puts both back in line and queues the backfill the store never
// started; the retry pass then retires the deleted record and writes the
// updated one. Done twice, the resume moves nothing twice.
func TestADeliveryThatArrivesDuringAPauseIsHeldAndWorkedOnResume(t *testing.T) {
	r := newPauseRig(t)
	if r == nil {
		return
	}
	const gone, changed = "gid://shopify/Product/7001", "gid://shopify/Product/7002"
	r.holds(gone, "Deleted during the pause")
	r.holds(changed, "Before the pause")
	// The products were read whole before the pause; the customers were never
	// reached.
	r.readWhole("product")

	r.setRecording(false)
	if got := r.store().Status; got != StatusPaused {
		t.Fatalf("after Pause recording the store reads %q", got)
	}
	r.admin.reply("ShopifyFetchProduct", productReply(changed, "Changed during the pause", r.at.Add(-29*time.Minute).Format(time.RFC3339)))
	deleted := r.stage("deleted", "products/delete", gone, 30*time.Minute)
	updated := r.stage("updated", "products/update", changed, 29*time.Minute)
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
	if row := r.record(changed); row["title"] != "Before the pause" {
		t.Fatalf("an update notice of a paused store wrote %v", row["title"])
	}
	if n := len(r.admin.seen()); n != 0 {
		t.Fatalf("the origin was asked %d times for a paused store", n)
	}
	if got := r.reasons(); len(got) != 1 || got[0] != VerdictPaused {
		t.Fatalf("the verdict of the paused store is %v, want paused and no reason", got)
	}

	r.setRecording(true)
	for _, id := range []string{deleted, updated} {
		row := r.delivery(id)
		if mapString(row, "status") != "failed" || mapString(row, "nextAttemptAt") == "" || mapInt(row, "attempts") != 0 {
			t.Fatalf("after Resume delivery %s is %v, want back in line: failed, due, no tries counted", id, row)
		}
	}
	if got := r.backfill("customer"); got != "queued" {
		t.Errorf("the customers, never backfilled before the pause, read %q after Resume, want queued", got)
	}
	if got := r.backfill("product"); got != "complete" {
		t.Errorf("the products, read whole before the pause, read %q after Resume, want them left complete", got)
	}
	if got := r.reasons(); len(got) > 0 && contains(got[1:], ReasonChangesNotRecorded) {
		t.Errorf("after Resume the verdict counts the returned deliveries as changes that could not be recorded: %v", got)
	}

	// The watch notices the same resume on its next pass, and the act may be
	// repeated: neither moves a delivery that is already back in line.
	before := r.versions(deleted) + r.versions(updated)
	if err := r.c.resumeStore(context.Background(), r.store()); err != nil {
		t.Fatalf("a second resume: %v", err)
	}
	if after := r.versions(deleted) + r.versions(updated); after != before {
		t.Fatalf("a second resume wrote the deliveries %d more times", after-before)
	}
	if got := r.backfill("customer"); got != "queued" {
		t.Errorf("a second resume left the customers %q", got)
	}

	r.retryPass()
	for _, id := range []string{deleted, updated} {
		if row := r.delivery(id); mapString(row, "status") != "processed" || mapInt(row, "attempts") != 1 {
			t.Fatalf("after the retry pass delivery %s is %v, want processed at its one counted try", id, row)
		}
	}
	if row := r.record(gone); row["deleted"] != true {
		t.Errorf("the record deleted during the pause is still live after Resume: %v", row)
	}
	if row := r.record(changed); row["title"] != "Changed during the pause" || row["deleted"] == true {
		t.Errorf("the record changed during the pause is %v after Resume", row)
	}
	if n := r.admin.countOp("ShopifyFetchProduct"); n != 1 {
		t.Errorf("the changed record was fetched %d times, want once: nothing was dispatched twice", n)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// A RESUME NOBODY ACTED cannot run the act: the status was written directly.
// The watch recorded the store as paused, so the pass that sees it recording
// again returns what was held; a pass after that returns nothing more.
func TestAStoreResumedByARawStatusChangeGetsItsHeldDeliveriesAtTheNextWatchPass(t *testing.T) {
	r := newPauseRig(t)
	if r == nil {
		return
	}
	const gone = "gid://shopify/Product/7101"
	r.holds(gone, "Deleted during the pause")
	ctx := context.Background()

	r.setStatus(StatusPaused)
	if err := r.c.WatchStores(ctx); err != nil {
		t.Fatalf("the pass over the paused store: %v", err)
	}
	if v, _ := r.store().Health["verdict"].(map[string]any); mapString(v, "state") != VerdictPaused {
		t.Fatalf("the watch recorded %v, want paused", v)
	}
	deleted := r.stage("deleted", "products/delete", gone, 10*time.Minute)
	if res := r.dispatch(deleted); !res.Held || !r.held(deleted, 0) {
		t.Fatalf("the delivery is %v (%+v), want held", r.delivery(deleted), res)
	}

	r.setStatus(StatusLive)
	// Nothing has returned it yet, and it is not a reason either way.
	if !r.held(deleted, 0) {
		t.Fatalf("a raw status change moved the delivery: %v", r.delivery(deleted))
	}
	if got := r.reasons(); contains(got, ReasonChangesNotRecorded) {
		t.Fatalf("a held delivery of a resumed store is counted as a change that could not be recorded: %v", got)
	}

	if err := r.c.WatchStores(ctx); err != nil {
		t.Fatalf("the pass that sees the store resume: %v", err)
	}
	if row := r.delivery(deleted); mapString(row, "nextAttemptAt") == "" || mapInt(row, "attempts") != 0 {
		t.Fatalf("after the watch pass the delivery is %v, want back in line", row)
	}
	recorded, _ := r.store().Health["verdict"].(map[string]any)
	if mapString(recorded, "state") == VerdictPaused || mapString(recorded, "resumedAt") == "" {
		t.Fatalf("the watch recorded %v, want the resume", recorded)
	}
	r.retryPass()
	if row := r.record(gone); row["deleted"] != true {
		t.Errorf("the record deleted during the pause is still live: %v", row)
	}

	written := r.versions(deleted)
	if err := r.c.WatchStores(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.versions(deleted); got != written {
		t.Errorf("a pass after the resume was recorded wrote the delivery %d more times", got-written)
	}
}

// A HELD DELIVERY IS NEVER "A CHANGE THAT COULD NOT BE RECORDED". The verdict
// counts the deliveries that used their four tries, and a hold does not count
// a try: a delivery three tries in when its store is paused is held at three,
// so neither the paused store nor the resumed one shows a reason for it.
func TestAHeldDeliveryIsNotAChangeThatCouldNotBeRecorded(t *testing.T) {
	r := newPauseRig(t)
	if r == nil {
		return
	}
	const flaky = "gid://shopify/Product/7201"
	r.holds(flaky, "Unreachable three times")
	r.admin.httpStatus("ShopifyFetchProduct", http.StatusServiceUnavailable)
	id := r.stage("flaky", "products/update", flaky, 50*time.Minute)
	for try := 1; try <= 3; try++ {
		r.dispatch(id)
		if got := mapInt(r.delivery(id), "attempts"); got != try {
			t.Fatalf("after try %d the delivery counts %d tries: %v", try, got, r.delivery(id))
		}
	}
	r.setStatus(StatusPaused)
	if res := r.dispatch(id); !res.Held || !r.held(id, 3) {
		t.Fatalf("on its fourth dispatch, with the store paused, the delivery is %v (%+v); want held with its three tries as they were", r.delivery(id), res)
	}
	if got := r.reasons(); len(got) != 1 || got[0] != VerdictPaused {
		t.Fatalf("paused: the verdict is %v, want paused and no reason", got)
	}
	r.setStatus(StatusLive)
	if got := r.reasons(); contains(got, ReasonChangesNotRecorded) {
		t.Fatalf("resumed: the verdict is %v; a held delivery is counted as a change that could not be recorded", got)
	}
}

// A STORE THAT IS NOT CONNECTED IS NOT WAITING FOR A RESUME. A delivery of a
// store Shopify reported uninstalled is worked as before: nothing is asked,
// nothing is written, and the delivery is processed.
func TestAnUninstalledStoresDeliveryIsProcessedWithNothingWritten(t *testing.T) {
	r := newPauseRig(t)
	if r == nil {
		return
	}
	const gid = "gid://shopify/Product/7301"
	r.holds(gid, "Still here")
	if _, err := r.eng.Execute(seedCtx(), renderCall("markStoreUninstalled", map[string]any{
		"storeId": r.storeID, "uninstalledAt": r.at.Add(-time.Hour).Format(time.RFC3339),
	})); err != nil {
		t.Fatalf("mark the store uninstalled: %v", err)
	}
	r.c.stores.Invalidate()
	id := r.stage("after-uninstall", "products/delete", gid, 5*time.Minute)
	if res := r.dispatch(id); res.Held || !res.Handled {
		t.Fatalf("the delivery of an uninstalled store: %+v, want handled and not held", res)
	}
	if row := r.delivery(id); mapString(row, "status") != "processed" || mapString(row, "nextAttemptAt") != "" {
		t.Fatalf("the delivery is %v, want processed", row)
	}
	if row := r.record(gid); row["deleted"] == true {
		t.Errorf("a delivery of an uninstalled store retired the record: %v", row)
	}
	if n := len(r.admin.seen()); n != 0 {
		t.Errorf("the origin was asked %d times for an uninstalled store", n)
	}
}
