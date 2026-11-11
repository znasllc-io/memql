package shopify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/datasync"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// version_db_test.go -- the rule that every mirror write carries the origin's
// version, against a real engine over a real Postgres: what an upgraded
// instance finds already in its mirror, and the one re-read that heals it.

// A ROW WRITTEN BEFORE THIS RULE holds a read time where the version should
// be, and no versionSource. Compared against the origin's updatedAt -- which
// is earlier than the time the row was read -- it would refuse every honest
// write until the origin changed the record again. It holds no version
// instead, so the origin's state replaces it on either writer: the runtime's
// applier and a sweep's heal. Once replaced, the row is guarded as any other.
func TestARowWrittenBeforeThisRuleIsReplacedByTheOriginsState(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	cleanMirrored(t, db, "product")
	c := &Connector{engine: eng, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
	specs := map[string]memqlsync.DomainSpec{}
	for _, spec := range c.Domains() {
		specs[spec.Concept] = spec
	}
	writer := datasync.NewEngineMirrorWriter(anyEngine{eng})
	applier := datasync.NewApplier(datasync.NewStore(anyEngine{eng}), writer)
	connCtx := auth.ContextWithConnectorActor(context.Background(), ConnectorName)
	spec := generated.Types["product"]
	product := func(n int, title, updatedAt string) map[string]any {
		return map[string]any{"id": fmt.Sprintf("gid://shopify/Product/%d", 13000+n), "title": title, "updatedAt": updatedAt}
	}
	// The row as the old rule wrote it: updatedAt and syncedAt both the time
	// it was read, 12:00, and nothing saying where the version came from.
	oldRow := func(n int) {
		t.Helper()
		gid := fmt.Sprintf("gid://shopify/Product/%d", 13000+n)
		err := writer.WriteMirror(connCtx, ConnectorName, memqlsync.MirrorWrite{
			Concept: generated.ConceptID("product"), RowId: MirrorRowID(authorityTestStoreId, gid),
			Payload: map[string]any{"storeId": authorityTestStoreId, "gid": gid, "title": "Stale",
				"updatedAt": "2026-11-11T12:00:00Z", "syncedAt": "2026-11-11T12:00:00Z", "deleted": false},
		})
		if err != nil {
			t.Fatalf("write the old row: %v", err)
		}
	}
	title := func(n int) string {
		t.Helper()
		_, found, err := writer.StoredVersion(connCtx, specs[generated.ConceptID("product")], MirrorRowID(authorityTestStoreId, fmt.Sprintf("gid://shopify/Product/%d", 13000+n)))
		if err != nil || !found {
			t.Fatalf("read the row back: found %t, %v", found, err)
		}
		var out string
		if err := db.QueryRowContext(context.Background(),
			`SELECT payload->>'title' FROM "MemoryNodes" WHERE concept = $1 AND payload->>'gid' = $2 AND payload->>'storeId' = $3 ORDER BY "createdAt" DESC LIMIT 1`,
			generated.ConceptID("product"), fmt.Sprintf("gid://shopify/Product/%d", 13000+n), authorityTestStoreId).Scan(&out); err != nil {
			t.Fatalf("read the title: %v", err)
		}
		return out
	}
	read := time.Date(2026, 11, 11, 12, 30, 0, 0, time.UTC)

	// The applier.
	oldRow(1)
	res, err := applier.Apply(connCtx, ConnectorName, specs, mapObject(spec, authorityTestStoreId, product(1, "Fresh", "2026-11-11T11:00:00Z"), "", read))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Applied != 1 || title(1) != "Fresh" {
		t.Fatalf("applied %d (stale %d), title %q: the origin's 11:00 state must replace a row read at 12:00 under the old rule", res.Applied, res.Stale, title(1))
	}
	res, err = applier.Apply(connCtx, ConnectorName, specs, mapObject(spec, authorityTestStoreId, product(1, "Older", "2026-11-11T10:00:00Z"), "", read))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Stale != 1 || title(1) != "Fresh" {
		t.Errorf("stale %d, title %q: once replaced, the row refuses an older state", res.Stale, title(1))
	}

	// A sweep's heal.
	oldRow(2)
	if healed, failed := c.heal(context.Background(), mapObject(spec, authorityTestStoreId, product(2, "Fresh", "2026-11-11T11:00:00Z"), "", read)); healed != 1 || failed != 0 || title(2) != "Fresh" {
		t.Fatalf("healed %d, failed %d, title %q: a sweep must replace a row read under the old rule", healed, failed, title(2))
	}
	if healed, _ := c.heal(context.Background(), mapObject(spec, authorityTestStoreId, product(2, "Older", "2026-11-11T10:00:00Z"), "", read)); healed != 0 || title(2) != "Fresh" {
		t.Errorf("healed %d, title %q: a sweep must refuse an older state, as the applier does", healed, title(2))
	}
}

// bootEngine is the real engine, with the datasync builtin the boot pass
// calls answered by a real runner over the same database; the queue of any
// other store is refused, because CI's database holds other packages' stores.
type bootEngine struct {
	*memql.MemQLEngine
	runner *datasync.Runner
	store  string
}

var queueArgsRE = regexp.MustCompile(`conceptId: "([^"]+)", connector: "shopify", partitionKey: "([^"]*)"`)

func (e *bootEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if callName(q) != "datasyncQueueBackfill" {
		return e.MemQLEngine.Execute(ctx, q)
	}
	m := queueArgsRE.FindStringSubmatch(q)
	if m == nil || m[2] != e.store {
		return nil, errors.New("bootEngine: a queue for another store")
	}
	if regexp.MustCompile(`restart: true`).MatchString(q) {
		return &memql.ExecuteResult{}, e.runner.RestartBackfill(ctx, ConnectorName, m[1], m[2])
	}
	return &memql.ExecuteResult{}, e.runner.QueueBackfill(ctx, ConnectorName, m[1], m[2])
}

// A DOMAIN READ BEFORE THIS RULE IS READ ONCE MORE. A complete backfill that
// began before every bulk line carried the origin's version may have put an
// older snapshot over a newer delivery, so the boot pass queues it again from
// the start, once: not again after it completes, nor at a second boot. A
// backfill running at boot is left alone -- and because its early pages were
// written under the old rule, it reads the domain once more when it reaches
// its end.
func TestADomainReadBeforeThisRuleIsQueuedOnceMore(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	seedStore(t, eng, db)
	for _, concept := range []string{"customer", "product", "productVariant"} {
		cleanMirrored(t, db, concept)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'partitionKey' = $1`, authorityTestStoreId)
	})
	_, _ = db.ExecContext(context.Background(),
		`DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:syncState' AND payload->>'partitionKey' = $1`, authorityTestStoreId)

	h := newHarness(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	state := datasync.NewStore(anyEngine{eng})
	runner := datasync.NewRunner(state, datasync.NewApplier(state, datasync.NewEngineMirrorWriter(anyEngine{eng})), logger)
	engine := &bootEngine{MemQLEngine: eng, runner: runner, store: authorityTestStoreId}
	stores := NewStoreRegistry(eng, func(context.Context, string) (string, error) { return "shpat_test", nil })
	stores.ttl = 0
	c := NewConnector(engine, logger, stores, h.conn.admin)
	c.now = func() time.Time { return h.now }
	memqlsync.UnbindForTest(ConnectorName)
	if err := memqlsync.Bind(c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(ConnectorName) })

	ctx := context.Background()
	opCtx := datasync.OperatorContext(ctx)
	row := func(concept string) datasync.SyncState {
		t.Helper()
		st, err := state.SyncStateFor(memql.ContextWithFreshRead(opCtx), generated.ConceptID(concept), ConnectorName, string(memqlsync.DirectionInbound), authorityTestStoreId)
		if err != nil {
			t.Fatalf("read %s's sync state: %v", concept, err)
		}
		return st
	}
	seed := func(concept, status, cursor string) {
		t.Helper()
		if err := state.PatchSyncState(opCtx, generated.ConceptID(concept), ConnectorName, string(memqlsync.DirectionInbound), authorityTestStoreId,
			func(datasync.SyncState) (datasync.SyncPatch, bool) {
				return datasync.SyncPatch{"backfillStatus": status, "backfillCursor": cursor}, true
			}); err != nil {
			t.Fatalf("seed %s: %v", concept, err)
		}
	}
	runs := func() int { return h.admin.countOp("ShopifyBulkRun") }
	backfill := func(concept string) {
		t.Helper()
		for i := 0; i < 5 && row(concept).BackfillStatus != "complete"; i++ {
			if _, err := runner.StartBackfill(ctx, ConnectorName, generated.ConceptID(concept), authorityTestStoreId); err != nil {
				t.Fatalf("backfill %s: %v", concept, err)
			}
		}
		if st := row(concept); st.BackfillStatus != "complete" {
			t.Fatalf("%s did not complete: %+v", concept, st)
		}
	}
	// One export per domain, each with its own file.
	exports := map[string]string{
		"gid://shopify/BulkOperation/1": h.admin.serveJSONL("/download/customers.jsonl", bulkLines(
			map[string]any{"id": "gid://shopify/Customer/13001", "firstName": "Ada", "updatedAt": "2026-11-11T09:00:00Z"})),
		"gid://shopify/BulkOperation/2": h.admin.serveJSONL("/download/products-again.jsonl", bulkLines(
			map[string]any{"id": "gid://shopify/Product/13001", "title": "Shirt", "updatedAt": "2026-11-11T09:00:00Z"})),
	}
	h.admin.replyWith("ShopifyBulkRun", func(vars map[string]any) any {
		if document, _ := vars["query"].(string); regexp.MustCompile(`query ShopifyBulkProduct\b`).MatchString(document) {
			return startedOperation("gid://shopify/BulkOperation/2")
		}
		return startedOperation("gid://shopify/BulkOperation/1")
	})
	h.admin.replyWith("ShopifyBulkStatus", func(vars map[string]any) any {
		id, _ := vars["id"].(string)
		return operationStatus(id, "COMPLETED", exports[id])
	})

	// The customers were read whole before the rule; the products are being
	// read at boot, from a cursor the old rule wrote.
	seed("customer", "complete", "")
	oldCursor := fmt.Sprintf(`{"op":"gid://shopify/BulkOperation/7","url":%q}`, h.admin.server.URL+"/download/products.jsonl")
	h.admin.serveJSONL("/download/products.jsonl", "")
	seed("product", "running", oldCursor)

	c.queueUnstartedBackfills(ctx)
	if st := row("customer"); st.BackfillStatus != "queued" || st.BackfillCursor != "" {
		t.Fatalf("customer = %s at %q, want queued from the start: its last backfill began before the rule", st.BackfillStatus, st.BackfillCursor)
	}
	if st := row("product"); st.BackfillStatus != "running" || st.BackfillCursor != oldCursor {
		t.Fatalf("product = %s at %q, want the running backfill left as it was", st.BackfillStatus, st.BackfillCursor)
	}

	before := runs()
	backfill("customer")
	if got := runs() - before; got != 1 {
		t.Errorf("the customers' re-read started %d exports, want 1", got)
	}

	// The running one reaches its end, and reads the domain once more.
	before = runs()
	backfill("product")
	if got := runs() - before; got != 1 {
		t.Errorf("the products' backfill begun under the old rule started %d exports at its end, want 1 more read", got)
	}

	// Read under the rule, both stay complete: a second boot queues neither.
	c.queueUnstartedBackfills(ctx)
	for _, concept := range []string{"customer", "product"} {
		if st := row(concept); st.BackfillStatus != "complete" {
			t.Errorf("a second boot set %s to %s, want it left complete: it was read under the rule", concept, st.BackfillStatus)
		}
	}
}

// THE MARKER MUST NOT OUTLIVE THE WRITE THAT SET IT. During a rolling
// upgrade a node on the earlier build writes a row with its read time as
// updatedAt and no versionSource; the engine's insert read-merges an omitted
// field from the stored row (component/memql/executor_mutation.go, the
// ENGINE-LEVEL READ-MERGE), so the stored versionSource survives beside a
// version that is not the origin's. The marker counts only beside the read
// time of the write that set it, so that row reads as written before the
// rule, and the origin's state replaces it.
func TestAnOlderBuildsWriteLeavesNoMarkerBehind(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	cleanMirrored(t, db, "product")
	c := &Connector{engine: eng, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
	specs := map[string]memqlsync.DomainSpec{}
	for _, spec := range c.Domains() {
		specs[spec.Concept] = spec
	}
	writer := datasync.NewEngineMirrorWriter(anyEngine{eng})
	applier := datasync.NewApplier(datasync.NewStore(anyEngine{eng}), writer)
	connCtx := auth.ContextWithConnectorActor(context.Background(), ConnectorName)
	const gid = "gid://shopify/Product/13101"
	spec := generated.Types["product"]
	apply := func(title, updatedAt string, read time.Time) datasync.ApplyResult {
		t.Helper()
		res, err := applier.Apply(connCtx, ConnectorName, specs, mapObject(spec, authorityTestStoreId, map[string]any{"id": gid, "title": title, "updatedAt": updatedAt}, "", read))
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		return res
	}
	title := func() string {
		t.Helper()
		var out string
		if err := db.QueryRowContext(context.Background(),
			`SELECT payload->>'title' FROM "MemoryNodes" WHERE concept = $1 AND payload->>'gid' = $2 AND payload->>'storeId' = $3 ORDER BY "createdAt" DESC LIMIT 1`,
			generated.ConceptID("product"), gid, authorityTestStoreId).Scan(&out); err != nil {
			t.Fatalf("read the title: %v", err)
		}
		return out
	}

	// Marked: written under the rule.
	if res := apply("Marked", "2026-11-11T11:00:00Z", time.Date(2026, 11, 11, 11, 30, 0, 0, time.UTC)); res.Applied != 1 {
		t.Fatalf("the marked write applied %d", res.Applied)
	}
	// The earlier build's write: its read time as updatedAt and syncedAt, no marker.
	if err := writer.WriteMirror(connCtx, ConnectorName, memqlsync.MirrorWrite{
		Concept: generated.ConceptID("product"), RowId: MirrorRowID(authorityTestStoreId, gid),
		Payload: map[string]any{"storeId": authorityTestStoreId, "gid": gid, "title": "Older build",
			"updatedAt": "2026-11-11T12:00:00Z", "syncedAt": "2026-11-11T12:00:00Z", "deleted": false},
	}); err != nil {
		t.Fatalf("the earlier build's write: %v", err)
	}
	// The origin's state, older than that read time: applied.
	res := apply("Origin", "2026-11-11T11:45:00Z", time.Date(2026, 11, 11, 12, 30, 0, 0, time.UTC))
	if res.Applied != 1 || title() != "Origin" {
		t.Errorf("applied %d (stale %d), title %q: a row an earlier build rewrote must not keep the marker the rule set", res.Applied, res.Stale, title())
	}
}
