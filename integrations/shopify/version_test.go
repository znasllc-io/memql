package shopify

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/datasync"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// version_test.go -- every mirror write carries the origin's own version
// (Task 13c).
//
// The mirror refuses a write older than the row it holds. That rule is only
// as good as the versions on both sides, and for every versioned type they
// were a clock of this engine's: no generated document selected updatedAt, so
// a delivery and a sweep stamped the time they read the object, a bulk page
// the time its line was streamed, and a relist its tombstones the time they
// were written -- through a heal that did not ask the guard at all. These are
// the races that made it wrong, each played through the real connector, the
// runtime's own applier and a mirror both write into.

// mirrorEngine is the harness engine with a mirror in it: it applies the raw
// insert both writers render (read-merge, as the engine does: a field a write
// omits keeps its stored value), answers the raw concept read both guards
// make, and lists a domain's live rows for a relist. Everything else is the
// harness's.
type mirrorEngine struct {
	*fakeEngine
	mu   stdsync.Mutex
	rows map[string]map[string]any // concept|row id -> payload
}

var (
	mirrorInsertRE = regexp.MustCompile(`^insert\(("(?:[^"\\]|\\.)*"), id=("(?:[^"\\]|\\.)*"), payload=(.*)\)$`)
	mirrorReadRE   = regexp.MustCompile(`^concept==(\S+) && id==("(?:[^"\\]|\\.)*")$`)
)

func (m *mirrorEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	q = strings.TrimSpace(q)
	if g := mirrorInsertRE.FindStringSubmatch(q); g != nil {
		concept, _ := strconv.Unquote(g[1])
		id, _ := strconv.Unquote(g[2])
		var payload map[string]any
		if err := json.Unmarshal([]byte(g[3]), &payload); err != nil {
			return nil, err
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		row := m.rows[concept+"|"+id]
		if row == nil {
			row = map[string]any{}
			m.rows[concept+"|"+id] = row
		}
		for k, v := range payload {
			row[k] = v
		}
		return &memql.ExecuteResult{}, nil
	}
	if g := mirrorReadRE.FindStringSubmatch(q); g != nil {
		id, _ := strconv.Unquote(g[2])
		m.mu.Lock()
		defer m.mu.Unlock()
		row := m.rows[g[1]+"|"+id]
		if row == nil {
			return resultWithRows(nil), nil
		}
		out := map[string]any{"id": id}
		for k, v := range row {
			out[k] = v
		}
		return resultWithRows([]map[string]any{out}), nil
	}
	for _, spec := range generated.Types {
		if spec.ForStoreFn == "" || callName(q) != spec.ForStoreFn {
			continue
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		var live []map[string]any
		prefix := generated.ConceptID(spec.Concept) + "|"
		for key, row := range m.rows {
			if strings.HasPrefix(key, prefix) && row["deleted"] != true {
				live = append(live, map[string]any{"id": strings.TrimPrefix(key, prefix), "gid": row["gid"]})
			}
		}
		return resultWithRows(live), nil
	}
	return m.fakeEngine.Execute(ctx, q)
}

// anyEngine is the runtime's engine seam over the harness's.
type anyEngine struct {
	e interface {
		Execute(context.Context, string) (*memql.ExecuteResult, error)
	}
}

func (a anyEngine) Execute(ctx context.Context, q string) (any, error) { return a.e.Execute(ctx, q) }

// mirrorRig is the harness with that mirror behind both writers: the
// runtime's applier for what Apply and a backfill return, and the
// connector's own heal for a sweep.
type mirrorRig struct {
	*testHarness
	mirror  *mirrorEngine
	applier *datasync.Applier
	specs   map[string]memqlsync.DomainSpec
}

func newMirrorRig(t *testing.T) *mirrorRig {
	t.Helper()
	h := newHarness(t)
	m := &mirrorEngine{fakeEngine: h.engine, rows: map[string]map[string]any{}}
	h.conn.engine = m
	specs := map[string]memqlsync.DomainSpec{}
	for _, spec := range h.conn.Domains() {
		specs[spec.Concept] = spec
	}
	r := &mirrorRig{
		testHarness: h,
		mirror:      m,
		applier:     datasync.NewApplier(datasync.NewStore(anyEngine{m}), datasync.NewEngineMirrorWriter(anyEngine{m})),
		specs:       specs,
	}
	// The connector's clock moves a millisecond each time it is read, from
	// whatever the test last set r.now to: two reads are never the same
	// instant, as they are not in production.
	var mu stdsync.Mutex
	var base time.Time
	ticks := 0
	h.conn.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		if !r.now.Equal(base) {
			base, ticks = r.now, 0
		}
		ticks++
		return base.Add(time.Duration(ticks) * time.Millisecond)
	}
	return r
}

// apply writes as the runtime does, behind its guard.
func (r *mirrorRig) apply(t *testing.T, writes []memqlsync.MirrorWrite) datasync.ApplyResult {
	t.Helper()
	res, err := r.applier.Apply(auth.ContextWithConnectorActor(context.Background(), ConnectorName), ConnectorName, r.specs, writes)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return res
}

// request is one staged delivery of a topic for a gid, triggered at the
// origin at `triggered`.
func request(topic, gid, webhookID string, triggered time.Time) memqlsync.InboundRequest {
	body, _ := json.Marshal(map[string]any{"admin_graphql_api_id": gid})
	return memqlsync.InboundRequest{
		RequestId: webhookID,
		Source:    "shopify-" + testStoreID,
		Topic:     topic,
		Body:      body,
		// Staged two seconds after the origin's event; a retry keeps it.
		ReceivedAt: triggered.UTC().Add(2 * time.Second),
		Headers: map[string]string{
			"x-shopify-topic":        topic,
			"x-shopify-shop-domain":  "acme-widgets.myshopify.com",
			"x-shopify-webhook-id":   webhookID,
			"x-shopify-triggered-at": triggered.UTC().Format(time.RFC3339),
		},
	}
}

// deliver works one delivery the way the dispatcher does: the connector's
// Apply, then the runtime's applier.
func (r *mirrorRig) deliver(t *testing.T, req memqlsync.InboundRequest) datasync.ApplyResult {
	t.Helper()
	writes, err := r.conn.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("Apply %s: %v", req.Topic, err)
	}
	return r.apply(t, writes)
}

// deliverMeanwhile is deliver from inside a scripted origin answer, which runs
// on the server's goroutine and so reports rather than stops.
func (r *mirrorRig) deliverMeanwhile(t *testing.T, req memqlsync.InboundRequest) {
	writes, err := r.conn.Apply(context.Background(), req)
	if err != nil {
		t.Errorf("Apply %s: %v", req.Topic, err)
		return
	}
	if _, err := r.applier.Apply(auth.ContextWithConnectorActor(context.Background(), ConnectorName), ConnectorName, r.specs, writes); err != nil {
		t.Errorf("apply %s: %v", req.Topic, err)
	}
}

// row is the mirror's row for a gid of a concept, or nil.
func (r *mirrorRig) row(concept, gid string) map[string]any {
	r.mirror.mu.Lock()
	defer r.mirror.mu.Unlock()
	return r.mirror.rows[generated.ConceptID(concept)+"|"+MirrorRowID(testStoreID, gid)]
}

// held is a live order the mirror holds, written under the rule: its version
// the origin's, read at `read`.
func (r *mirrorRig) held(concept, gid, updatedAt string, read time.Time) {
	r.put(concept, gid, map[string]any{"name": "held", "updatedAt": updatedAt, "versionSource": "origin",
		"syncedAt": memqlsync.ReadTime(read), "versionReadAt": memqlsync.ReadTime(read), "deleted": false})
}

// put writes a row straight into the mirror, as an earlier writer left it.
func (r *mirrorRig) put(concept, gid string, payload map[string]any) {
	r.mirror.mu.Lock()
	defer r.mirror.mu.Unlock()
	row := map[string]any{"storeId": testStoreID, "gid": gid}
	for k, v := range payload {
		row[k] = v
	}
	r.mirror.rows[generated.ConceptID(concept)+"|"+MirrorRowID(testStoreID, gid)] = row
}

// exportStarted starts a domain's bulk export: the origin takes its snapshot
// from here.
func (r *mirrorRig) exportStarted(t *testing.T, concept string) string {
	t.Helper()
	r.admin.reply("ShopifyBulkRun", startedOperation("gid://shopify/BulkOperation/1"))
	page, err := r.conn.BackfillStore(context.Background(), r.store(t), generated.ConceptID(concept), "")
	if err != nil {
		t.Fatalf("start the export: %v", err)
	}
	return page.NextCursor
}

// exportStreamed finishes the export with these lines and applies its page.
func (r *mirrorRig) exportStreamed(t *testing.T, concept, cursor string, lines ...map[string]any) (datasync.ApplyResult, memqlsync.BackfillPage) {
	t.Helper()
	url := r.admin.serveJSONL("/download/"+concept+".jsonl", bulkLines(lines...))
	r.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "COMPLETED", url))
	page, err := r.conn.BackfillStore(context.Background(), r.store(t), generated.ConceptID(concept), cursor)
	if err != nil {
		t.Fatalf("stream the export: %v", err)
	}
	return r.apply(t, page.Writes), page
}

func at(clock string) time.Time {
	t, err := time.Parse(time.RFC3339, "2026-08-23T"+clock+"Z")
	if err != nil {
		panic(err)
	}
	return t
}

// orderNode is an order as the origin answers it, with one line.
func orderNode(updatedAt string, quantity int) map[string]any {
	return map[string]any{
		"__typename": "Order",
		"id":         "gid://shopify/Order/1",
		"name":       "#1001",
		"updatedAt":  updatedAt,
		"lineItems": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			"nodes":    []any{map[string]any{"id": "gid://shopify/LineItem/1", "name": "Tee", "quantity": quantity}},
		},
	}
}

// orderLines are an order and its line as a bulk export streams them.
func orderLines(updatedAt string, quantity int) []map[string]any {
	return []map[string]any{
		{"id": "gid://shopify/Order/1", "name": "#1001", "updatedAt": updatedAt},
		{"id": "gid://shopify/LineItem/1", "name": "Tee", "quantity": quantity, "__parentId": "gid://shopify/Order/1"},
	}
}

func (r *mirrorRig) orderFetches(node map[string]any) {
	r.admin.reply("ShopifyFetchOrder", map[string]any{"node": node})
}

func stringAt(row map[string]any, key string) string {
	s, _ := row[key].(string)
	return s
}

func TestAWriteCarriesTheOriginsUpdatedAt(t *testing.T) {
	const origin = "2026-08-23T11:00:00Z"
	check := func(t *testing.T, path, version string, row map[string]any) {
		t.Helper()
		if version != origin {
			t.Errorf("%s: the write's version is %s, want the origin's updatedAt %s", path, version, origin)
		}
		if row != nil && (stringAt(row, "updatedAt") != origin || stringAt(row, "versionSource") != memqlsync.VersionFromOrigin) {
			t.Errorf("%s: the row holds updatedAt %q from %q, want the origin's %s", path, row["updatedAt"], row["versionSource"], origin)
		}
	}
	t.Run("delivery", func(t *testing.T) {
		r := newMirrorRig(t)
		r.admin.reply("ShopifyFetchProduct", map[string]any{"node": map[string]any{"__typename": "Product", "id": "gid://shopify/Product/1", "title": "Shirt", "updatedAt": origin}})
		writes, err := r.conn.Apply(context.Background(), request("products/update", "gid://shopify/Product/1", "wh-1", at("11:00:00")))
		if err != nil || len(writes) == 0 {
			t.Fatalf("Apply = %v, %v", writes, err)
		}
		r.apply(t, writes)
		check(t, "a delivery", writes[0].Version, r.row("product", "gid://shopify/Product/1"))
	})
	t.Run("sweep", func(t *testing.T) {
		r := newMirrorRig(t)
		r.admin.reply("ShopifyListProduct", listPageReply("products", false, "", map[string]any{"id": "gid://shopify/Product/1", "title": "Shirt", "updatedAt": origin}))
		if _, err := r.conn.reconcileByUpdatedAt(context.Background(), r.store(t), generated.Types["product"], at("10:00:00")); err != nil {
			t.Fatal(err)
		}
		row := r.row("product", "gid://shopify/Product/1")
		check(t, "a sweep", stringAt(row, "updatedAt"), row)
	})
	t.Run("relist", func(t *testing.T) {
		r := newMirrorRig(t)
		r.admin.reply("ShopifyListOnlineStoreTheme", listPageReply("themes", false, "", map[string]any{"id": "gid://shopify/OnlineStoreTheme/1", "name": "Dawn", "updatedAt": origin}))
		if _, err := r.conn.relist(context.Background(), r.store(t), generated.Types["onlineStoreTheme"]); err != nil {
			t.Fatal(err)
		}
		row := r.row("onlineStoreTheme", "gid://shopify/OnlineStoreTheme/1")
		check(t, "a relist", stringAt(row, "updatedAt"), row)
	})
	t.Run("bulk line", func(t *testing.T) {
		r := newMirrorRig(t)
		cursor := r.exportStarted(t, "product")
		_, page := r.exportStreamed(t, "product", cursor, map[string]any{"id": "gid://shopify/Product/1", "title": "Shirt", "updatedAt": origin})
		if len(page.Writes) != 1 {
			t.Fatalf("the export wrote %d rows, want 1", len(page.Writes))
		}
		check(t, "a bulk line", page.Writes[0].Version, r.row("product", "gid://shopify/Product/1"))
	})
}

// A line item publishes no version, but it is part of its order, and Shopify
// moves the order's updatedAt when a line changes. So the line carries its
// order's version on every path, and an older snapshot of the order cannot
// put its older lines back either. In a bulk export the line is a line of its
// own, after its order's; a batch that resumes between them still knows the
// order it belongs to.
func TestAChildWithNoVersionOfItsOwnTakesItsParents(t *testing.T) {
	const origin = "2026-08-23T11:00:00Z"
	lineOf := func(t *testing.T, writes []memqlsync.MirrorWrite) memqlsync.MirrorWrite {
		t.Helper()
		for _, w := range writes {
			if w.Concept == generated.ConceptID("orderLineItem") {
				return w
			}
		}
		t.Fatalf("no line item among %d writes", len(writes))
		return memqlsync.MirrorWrite{}
	}
	if generated.Types["orderLineItem"].VersionField != "" {
		t.Fatal("orderLineItem has a version of its own now, so this test no longer exercises the rule")
	}
	t.Run("delivery", func(t *testing.T) {
		r := newMirrorRig(t)
		r.orderFetches(orderNode(origin, 1))
		writes, err := r.conn.Apply(context.Background(), request("orders/updated", "gid://shopify/Order/1", "wh-1", at("11:00:00")))
		if err != nil {
			t.Fatal(err)
		}
		if w := lineOf(t, writes); w.Version != origin || w.Payload["versionSource"] != memqlsync.VersionFromOrigin {
			t.Errorf("the line's version = %s from %v, want its order's %s", w.Version, w.Payload["versionSource"], origin)
		}
	})
	t.Run("bulk", func(t *testing.T) {
		r := newMirrorRig(t)
		cursor := r.exportStarted(t, "order")
		_, page := r.exportStreamed(t, "order", cursor, orderLines(origin, 1)...)
		if w := lineOf(t, page.Writes); w.Version != origin {
			t.Errorf("the bulk line's version = %s, want its order's %s", w.Version, origin)
		}
	})
	t.Run("bulk, resumed between the order and its line", func(t *testing.T) {
		r := newMirrorRig(t)
		url := r.admin.serveJSONL("/download/resumed.jsonl", bulkLines(orderLines(origin, 1)...))
		cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", URL: url, Line: 1, Rule: originVersionRule, At: exportStartedAt}
		page, err := r.conn.BackfillStore(context.Background(), r.store(t), generated.ConceptID("order"), cur.encode())
		if err != nil {
			t.Fatal(err)
		}
		if w := lineOf(t, page.Writes); w.Version != origin {
			t.Errorf("the resumed line's version = %s, want its order's %s", w.Version, origin)
		}
	})
}

// T1: the export's snapshot. T2: a delivery records the order as it is now,
// with two of the line. T3: the export's file is streamed, holding T1's order
// with one. The row stays T2's, and both of the snapshot's rows are counted
// stale -- before, the line was stamped T3, newer than everything, and the
// order and its line went back to T1 until the origin changed them again.
func TestAnOlderBulkSnapshotDoesNotReplaceANewerDelivery(t *testing.T) {
	r := newMirrorRig(t)
	r.now = at("10:00:30")
	cursor := r.exportStarted(t, "order") // the snapshot is taken at T1 = 10:00

	r.now = at("10:05:10")
	r.orderFetches(orderNode("2026-08-23T10:05:00Z", 2))
	r.deliver(t, request("orders/updated", "gid://shopify/Order/1", "wh-t2", at("10:05:00")))

	r.now = at("10:10:00")
	res, _ := r.exportStreamed(t, "order", cursor, orderLines("2026-08-23T10:00:00Z", 1)...)

	order, line := r.row("order", "gid://shopify/Order/1"), r.row("orderLineItem", "gid://shopify/LineItem/1")
	if got := stringAt(order, "updatedAt"); got != "2026-08-23T10:05:00Z" {
		t.Errorf("the order holds updatedAt %s, want the delivery's 10:05: an older snapshot replaced it", got)
	}
	if got, _ := line["quantity"].(float64); got != 2 {
		t.Errorf("the line holds quantity %v, want the delivery's 2: an older snapshot replaced it", line["quantity"])
	}
	if res.Applied != 0 || res.Stale != 2 {
		t.Errorf("the snapshot applied %d and was stale %d, want 0 and 2 (the order and its line)", res.Applied, res.Stale)
	}
}

// The order is deleted after the export's snapshot and before its file is
// streamed. The tombstone carries the origin's event time; the snapshot's
// order carries its own older updatedAt, so it does not bring the order back.
func TestABulkSnapshotDoesNotBringBackADeletedRecord(t *testing.T) {
	r := newMirrorRig(t)
	r.now = at("09:00:05")
	r.orderFetches(orderNode("2026-08-23T08:59:00Z", 1))
	r.deliver(t, request("orders/updated", "gid://shopify/Order/1", "wh-0", at("08:59:00")))

	r.now = at("10:00:30")
	cursor := r.exportStarted(t, "order")

	r.now = at("10:05:02")
	r.deliver(t, request("orders/delete", "gid://shopify/Order/1", "wh-del", at("10:05:00")))
	if r.row("order", "gid://shopify/Order/1")["deleted"] != true {
		t.Fatal("the delete did not retire the order, so this test proves nothing")
	}

	r.now = at("10:10:00")
	r.exportStreamed(t, "order", cursor, orderLines("2026-08-23T08:59:00Z", 1)...)
	if r.row("order", "gid://shopify/Order/1")["deleted"] != true {
		t.Error("an export whose snapshot predates the delete brought the deleted order back")
	}
}

// A sweep lists the product as it was; while its page is out, a delivery
// records the product as it is now. The sweep's heal refuses the older state
// by the applier's own comparison instead of writing it over the delivery's.
func TestASweepDoesNotReplaceWhatADeliveryWroteMeanwhile(t *testing.T) {
	r := newMirrorRig(t)
	r.now = at("10:05:30")
	r.admin.reply("ShopifyFetchProduct", map[string]any{"node": map[string]any{"__typename": "Product", "id": "gid://shopify/Product/1", "title": "Two", "updatedAt": "2026-08-23T10:05:00Z"}})
	r.admin.replyWith("ShopifyListProduct", func(map[string]any) any {
		// The page holds the product as the origin listed it; the delivery
		// lands before the sweep writes it.
		r.deliverMeanwhile(t, request("products/update", "gid://shopify/Product/1", "wh-meanwhile", at("10:05:00")))
		return listPageReply("products", false, "", map[string]any{"id": "gid://shopify/Product/1", "title": "One", "updatedAt": "2026-08-23T10:00:00Z"})
	})
	if _, err := r.conn.reconcileByUpdatedAt(context.Background(), r.store(t), generated.Types["product"], at("09:00:00")); err != nil {
		t.Fatal(err)
	}
	row := r.row("product", "gid://shopify/Product/1")
	if stringAt(row, "title") != "Two" || stringAt(row, "updatedAt") != "2026-08-23T10:05:00Z" {
		t.Errorf("the product holds %q at %s, want the delivery's \"Two\" at 10:05: the sweep wrote an older state over it", row["title"], row["updatedAt"])
	}
}

// A relist retires what its listing did not name. A theme a delivery wrote
// after the listing began is not in the listing and not gone: the relist's
// tombstone carries the time its listing began, older than that theme's
// version, and is refused. A theme that was there before the listing began
// and is not in it is retired as before.
func TestARelistDoesNotRetireWhatADeliveryWroteAfterItBegan(t *testing.T) {
	r := newMirrorRig(t)
	r.now = at("10:00:00")
	r.put("onlineStoreTheme", "gid://shopify/OnlineStoreTheme/1", map[string]any{"name": "Dawn", "updatedAt": "2026-08-23T08:00:00Z", "versionSource": "origin", "deleted": false})
	r.put("onlineStoreTheme", "gid://shopify/OnlineStoreTheme/3", map[string]any{"name": "Gone", "updatedAt": "2026-08-23T08:00:00Z", "versionSource": "origin", "deleted": false})
	r.admin.reply("ShopifyFetchOnlineStoreTheme", map[string]any{"node": map[string]any{"__typename": "OnlineStoreTheme", "id": "gid://shopify/OnlineStoreTheme/2", "name": "New", "updatedAt": "2026-08-23T10:00:20Z"}})
	r.admin.replyWith("ShopifyListOnlineStoreTheme", func(map[string]any) any {
		r.deliverMeanwhile(t, request("themes/create", "gid://shopify/OnlineStoreTheme/2", "wh-new", at("10:00:20")))
		return listPageReply("themes", false, "", map[string]any{"id": "gid://shopify/OnlineStoreTheme/1", "name": "Dawn", "updatedAt": "2026-08-23T08:00:00Z"})
	})
	if _, err := r.conn.relist(context.Background(), r.store(t), generated.Types["onlineStoreTheme"]); err != nil {
		t.Fatal(err)
	}
	if row := r.row("onlineStoreTheme", "gid://shopify/OnlineStoreTheme/2"); row == nil || row["deleted"] == true {
		t.Errorf("the relist retired the theme a delivery wrote after its listing began: %v", row)
	}
	if row := r.row("onlineStoreTheme", "gid://shopify/OnlineStoreTheme/3"); row["deleted"] != true {
		t.Errorf("the relist kept a theme its listing no longer names: %v", row)
	}
	if row := r.row("onlineStoreTheme", "gid://shopify/OnlineStoreTheme/1"); row["deleted"] == true {
		t.Errorf("the relist retired a theme its listing names: %v", row)
	}
}

// A delete retried after the record was written back by a page whose
// snapshot predates the delete, and an update retried after the record was
// deleted whose fetch now finds nothing: both retire the record.
func TestARetriedDeleteStillRetiresTheRecord(t *testing.T) {
	t.Run("a delete retried after an older snapshot was applied", func(t *testing.T) {
		r := newMirrorRig(t)
		r.now = at("09:00:05")
		r.orderFetches(orderNode("2026-08-23T08:59:00Z", 1))
		r.deliver(t, request("orders/updated", "gid://shopify/Order/1", "wh-0", at("08:59:00")))
		r.now = at("10:00:30")
		cursor := r.exportStarted(t, "order")

		// The delete arrives at 10:05 and its write fails; the runtime tries
		// it again later.
		r.now = at("10:05:02")
		del := request("orders/delete", "gid://shopify/Order/1", "wh-del", at("10:05:00"))
		if _, err := r.conn.Apply(context.Background(), del); err != nil {
			t.Fatal(err)
		}
		r.conn.ForgetInbound(context.Background(), del)

		r.now = at("10:10:00")
		r.exportStreamed(t, "order", cursor, orderLines("2026-08-23T08:59:00Z", 1)...)

		r.now = at("10:11:00")
		r.deliver(t, del)
		if row := r.row("order", "gid://shopify/Order/1"); row["deleted"] != true {
			t.Errorf("the retried delete left the deleted order live: updatedAt %v", row["updatedAt"])
		}
	})
	t.Run("an update retried after the record was deleted", func(t *testing.T) {
		r := newMirrorRig(t)
		// A (10:00) fails and waits for its retry; B (10:01) lands.
		r.now = at("10:01:05")
		r.orderFetches(orderNode("2026-08-23T10:01:00Z", 1))
		r.deliver(t, request("orders/updated", "gid://shopify/Order/1", "wh-b", at("10:01:00")))

		// The order is deleted at 10:02 and the delete delivery is lost. A's
		// retry fetches nothing.
		r.now = at("10:03:00")
		r.orderFetches(nil)
		r.deliver(t, request("orders/updated", "gid://shopify/Order/1", "wh-a", at("10:00:00")))
		if row := r.row("order", "gid://shopify/Order/1"); row["deleted"] != true {
			t.Errorf("an update whose fetch found nothing left the deleted order live: updatedAt %v", row["updatedAt"])
		}
	})
}

// THE FAKE ADMIN ANSWERS ONLY WHAT IT WAS ASKED. The fault this task fixes
// lived for months behind a green suite because every fake handed updatedAt
// back whether the document selected it or not. A reply or a bulk line that
// carries a version the document does not select is now a failure of the
// test that scripted it, and the field is not in the answer.
func TestTheFakeAdminRefusesAVersionTheDocumentDidNotSelect(t *testing.T) {
	h := newHarness(t)
	var told []string
	h.admin.mu.Lock()
	h.admin.unselected = func(message string) { told = append(told, message) }
	h.admin.mu.Unlock()
	ctx := context.Background()

	h.admin.reply("Probe", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1", "updatedAt": "2026-08-23T11:00:00Z"}})
	resp, err := h.conn.adminCall(ctx, h.store(t), "query Probe {\n  shop {\n    id\n  }\n}", "Probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := resp.DataMap()
	if len(told) != 1 || !strings.Contains(told[0], "updatedAt") {
		t.Errorf("a reply carrying an unselected updatedAt was told %v, want one failure naming it", told)
	}
	if shop, _ := data["shop"].(map[string]any); shop["updatedAt"] != nil {
		t.Errorf("the unselected updatedAt was answered: %v", shop)
	}

	told = nil
	resp, err = h.conn.adminCall(ctx, h.store(t), "query Probe {\n  shop {\n    id\n    updatedAt\n  }\n}", "Probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = resp.DataMap()
	if shop, _ := data["shop"].(map[string]any); len(told) != 0 || shop["updatedAt"] != "2026-08-23T11:00:00Z" {
		t.Errorf("a selected updatedAt: told %v, answered %v", told, shop)
	}

	// Selected for a nested object only: the parent's is still unselected.
	told = nil
	resp, err = h.conn.adminCall(ctx, h.store(t), "query Probe {\n  shop {\n    id\n    owner {\n      updatedAt\n    }\n  }\n}", "Probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = resp.DataMap()
	if shop, _ := data["shop"].(map[string]any); len(told) != 1 || shop["updatedAt"] != nil {
		t.Errorf("an updatedAt selected only for a nested object: told %v, answered %v", told, shop)
	}

	told = nil
	h.admin.mu.Lock()
	h.admin.bulkDocuments = append(h.admin.bulkDocuments, "query ShopifyBulkProbe {\n  things {\n    edges {\n      node {\n        id\n      }\n    }\n  }\n}")
	h.admin.mu.Unlock()
	url := h.admin.serveJSONL("/download/probe.jsonl", bulkLines(map[string]any{"id": "gid://shopify/Thing/1", "updatedAt": "2026-08-23T11:00:00Z"}))
	got, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	var line map[string]any
	_ = json.NewDecoder(got.Body).Decode(&line)
	if len(told) != 1 || line["updatedAt"] != nil {
		t.Errorf("a bulk line carrying an unselected updatedAt: told %v, served %v", told, line)
	}
}

// ---- fix round 1: a record's existence is judged by when the origin was read ----

// Only a clean answer is a deletion: `node: null` and no error at all. Every
// other way a fetch can come back without the object -- the origin refusing
// the node itself (how Shopify refuses protected customer data on
// node(id:)), a denial left after the prune limit, a reply with no data or
// no node, an error that is not a denial -- retires nothing, and is a
// refusal or a failure the retry lane and the store's reasons already know.
func TestEveryAnswerOfAFetchIsADeletionOnlyWhenClean(t *testing.T) {
	protected := GraphQLError{
		Message:    "This app is not approved to access the Order object. See https://shopify.dev/docs/apps/launch/protected-customer-data for more details.",
		Extensions: map[string]any{"code": "ACCESS_DENIED"},
		Path:       []any{"node"},
	}
	cases := []struct {
		name      string
		reply     any
		errs      []GraphQLError
		retired   bool
		refused   bool // not granted, and never tried again
		retryable bool
	}{
		{name: "clean: node null, no error", reply: map[string]any{"node": nil}, retired: true},
		{name: "the node refused (protected data)", reply: map[string]any{"node": nil}, errs: []GraphQLError{protected}, refused: true},
		{name: "a denied field still nulling the node after the prune limit", reply: map[string]any{"node": nil}, errs: []GraphQLError{denied("name", "node", "name")}, refused: true},
		{name: "data null, no error", reply: nil, retryable: true},
		{name: "no node in the data, no error", reply: map[string]any{}, retryable: true},
		{name: "an error that is not a denial", reply: map[string]any{"node": nil},
			errs: []GraphQLError{{Message: "Internal error", Extensions: map[string]any{"code": "INTERNAL_SERVER_ERROR"}}}, retryable: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newMirrorRig(t)
			r.now = at("10:00:00")
			r.held("order", "gid://shopify/Order/1", "2026-08-23T09:00:00Z", at("09:30:00"))
			r.admin.reply("ShopifyFetchOrder", tc.reply)
			if tc.errs != nil {
				r.admin.fail("ShopifyFetchOrder", tc.errs...)
			}
			writes, err := r.conn.Apply(context.Background(), request("orders/updated", "gid://shopify/Order/1", "wh-1", at("09:59:00")))
			if err == nil {
				r.apply(t, writes)
			}
			retired := r.row("order", "gid://shopify/Order/1")["deleted"] == true
			if retired != tc.retired {
				t.Fatalf("retired = %t (err %v), want %t", retired, err, tc.retired)
			}
			switch {
			case tc.refused && !(memqlsync.IsNotGranted(err) && memqlsync.IsPermanent(err)):
				t.Errorf("err = %v, want a refusal (not granted, not retried)", err)
			case tc.retryable && (err == nil || memqlsync.IsPermanent(err)):
				t.Errorf("err = %v, want a failure the retry lane tries again", err)
			case tc.retired && err != nil:
				t.Errorf("err = %v on a clean answer", err)
			}
		})
	}
}

// A create delivery can arrive before its record is readable. An empty fetch
// for a record the mirror does not hold has nothing to retire, so it writes
// nothing; the record arrives with the next sweep that lists it.
func TestAnEmptyFetchForARecordNotHeldWritesNothing(t *testing.T) {
	r := newMirrorRig(t)
	r.now = at("10:00:00")
	r.orderFetches(nil)
	writes, err := r.conn.Apply(context.Background(), request("orders/create", "gid://shopify/Order/1", "wh-create", at("10:00:00")))
	if err != nil {
		t.Fatal(err)
	}
	r.apply(t, writes)
	if row := r.row("order", "gid://shopify/Order/1"); row != nil {
		t.Fatalf("an empty fetch for a record the mirror never held wrote %v", row)
	}
	r.now = at("10:10:00")
	r.admin.reply("ShopifyListOrder", listPageReply("orders", false, "", map[string]any{"id": "gid://shopify/Order/1", "name": "#1001", "updatedAt": "2026-08-23T10:00:00Z"}))
	if _, err := r.conn.reconcileByUpdatedAt(context.Background(), r.store(t), generated.Types["order"], at("09:00:00")); err != nil {
		t.Fatal(err)
	}
	if row := r.row("order", "gid://shopify/Order/1"); row == nil || row["deleted"] == true {
		t.Errorf("the sweep did not write the record: %v", row)
	}
}

// A clean empty fetch at F can still be wrong (a record briefly unreadable).
// The tombstone it writes is a statement about F, so the next read of the
// origin after F that finds the record puts it back -- although its updatedAt
// has not moved, and is older than F.
func TestARecordWronglySeenAsGoneComesBackOnTheNextRead(t *testing.T) {
	for _, path := range []string{"sweep", "delivery"} {
		t.Run(path, func(t *testing.T) {
			r := newMirrorRig(t)
			r.now = at("09:30:00")
			r.held("order", "gid://shopify/Order/1", "2026-08-23T09:00:00Z", at("09:30:00"))
			r.now = at("10:00:00")
			r.orderFetches(nil)
			r.deliver(t, request("orders/updated", "gid://shopify/Order/1", "wh-f", at("09:59:00")))
			if r.row("order", "gid://shopify/Order/1")["deleted"] != true {
				t.Fatal("the clean empty fetch did not retire the held order, so this test proves nothing")
			}
			r.now = at("10:05:00")
			switch path {
			case "sweep":
				r.admin.reply("ShopifyListOrder", listPageReply("orders", false, "", map[string]any{"id": "gid://shopify/Order/1", "name": "#1001", "updatedAt": "2026-08-23T09:00:00Z"}))
				if _, err := r.conn.reconcileByUpdatedAt(context.Background(), r.store(t), generated.Types["order"], at("08:00:00")); err != nil {
					t.Fatal(err)
				}
			case "delivery":
				r.orderFetches(orderNode("2026-08-23T09:00:00Z", 1))
				r.deliver(t, request("orders/updated", "gid://shopify/Order/1", "wh-back", at("10:04:00")))
			}
			if row := r.row("order", "gid://shopify/Order/1"); row["deleted"] == true {
				t.Errorf("a %s that read the order after the empty fetch left it retired", path)
			}
		})
	}
}

// A bulk line is a statement about the moment its export was started. One
// started before a tombstone's time does not bring the record back -- even
// when its updatedAt is the very second of the tombstone -- whether the
// tombstone came from a delete topic or from a clean empty fetch.
func TestABulkLineExportedBeforeATombstoneDoesNotBringTheRecordBack(t *testing.T) {
	for _, kind := range []string{"delete topic", "empty fetch"} {
		t.Run(kind, func(t *testing.T) {
			r := newMirrorRig(t)
			r.now = at("09:30:00")
			r.held("order", "gid://shopify/Order/1", "2026-08-23T09:00:00Z", at("09:30:00"))
			r.now = at("10:00:30")
			cursor := r.exportStarted(t, "order")
			// Updated and deleted inside one second, 10:05:00.
			r.now = at("10:05:00")
			switch kind {
			case "delete topic":
				r.deliver(t, request("orders/delete", "gid://shopify/Order/1", "wh-del", at("10:04:58")))
			case "empty fetch":
				r.orderFetches(nil)
				r.deliver(t, request("orders/updated", "gid://shopify/Order/1", "wh-upd", at("10:04:58")))
			}
			r.now = at("10:10:00")
			r.exportStreamed(t, "order", cursor, orderLines("2026-08-23T10:05:00Z", 1)...)
			if r.row("order", "gid://shopify/Order/1")["deleted"] != true {
				t.Error("a line from an export started before the tombstone brought the order back")
			}
		})
	}
}

// One started after the tombstone's time read the origin after it, and the
// record in it is there: it comes back, though its updatedAt is older.
func TestABulkLineExportedAfterATombstoneBringsTheRecordBack(t *testing.T) {
	r := newMirrorRig(t)
	r.now = at("09:30:00")
	r.held("order", "gid://shopify/Order/1", "2026-08-23T09:00:00Z", at("09:30:00"))
	r.now = at("10:00:00")
	r.orderFetches(nil)
	r.deliver(t, request("orders/updated", "gid://shopify/Order/1", "wh-f", at("09:59:00")))
	r.now = at("10:30:00")
	cursor := r.exportStarted(t, "order")
	r.now = at("10:40:00")
	r.exportStreamed(t, "order", cursor, orderLines("2026-08-23T09:00:00Z", 1)...)
	if r.row("order", "gid://shopify/Order/1")["deleted"] == true {
		t.Error("a line from an export started after the tombstone left the order retired")
	}
}

// A version that is a read time and one that is the origin's are never
// compared. A reverse fulfilment order publishes no version and its return is
// versioned by createdAt, which never moves, so it hands nothing down: read
// directly by its own topic it holds a read time, and a later read of it
// under its return is applied. A bulk line whose parent line was not seen
// holds a read time, and the parent's delivery is applied over it.
func TestVersionsOfDifferentKindsAreNeverCompared(t *testing.T) {
	t.Run("a reverse fulfilment order, then its return", func(t *testing.T) {
		r := newMirrorRig(t)
		const rfo = "gid://shopify/ReverseFulfillmentOrder/1"
		r.now = at("10:00:00")
		r.admin.reply("ShopifyFetchReverseFulfillmentOrder", map[string]any{"node": map[string]any{"__typename": "ReverseFulfillmentOrder", "id": rfo, "status": "OPEN"}})
		r.deliver(t, request("reverse_fulfillment_orders/dispose", rfo, "wh-rfo", at("10:00:00")))
		r.now = at("10:05:00")
		r.admin.reply("ShopifyFetchShopifyReturn", map[string]any{"node": map[string]any{
			"__typename": "Return", "id": "gid://shopify/Return/1", "createdAt": "2026-08-20T00:00:00Z", "order": map[string]any{"id": "gid://shopify/Order/1"},
			"reverseFulfillmentOrders": map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
				"nodes": []any{map[string]any{"id": rfo, "status": "CLOSED"}}},
		}})
		res := r.deliver(t, request("returns/update", "gid://shopify/Return/1", "wh-ret", at("10:05:00")))
		if row := r.row("reverseFulfillmentOrder", rfo); stringAt(row, "status") != "CLOSED" {
			t.Errorf("the reverse fulfilment order holds %v (stale %d), want the later read's CLOSED", row["status"], res.Stale)
		}
	})
	t.Run("a bulk line without its parent, then the parent's delivery", func(t *testing.T) {
		r := newMirrorRig(t)
		r.now = at("10:00:00")
		// The batch resumes at a line whose parent line is not the last one
		// before it: the line is read with no version of its parent's.
		url := r.admin.serveJSONL("/download/orphan.jsonl", bulkLines(
			map[string]any{"id": "gid://shopify/Order/2", "name": "#1002", "updatedAt": "2026-08-23T09:00:00Z"},
			map[string]any{"id": "gid://shopify/LineItem/1", "name": "Tee", "quantity": 1, "__parentId": "gid://shopify/Order/1"},
		))
		cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", URL: url, Line: 1, Rule: originVersionRule, At: memqlsync.ReadTime(at("09:59:00"))}
		page, err := r.conn.BackfillStore(context.Background(), r.store(t), generated.ConceptID("order"), cur.encode())
		if err != nil {
			t.Fatal(err)
		}
		r.apply(t, page.Writes)
		if line := r.row("orderLineItem", "gid://shopify/LineItem/1"); stringAt(line, "versionSource") == memqlsync.VersionFromOrigin {
			t.Fatalf("the line with no parent seen holds an origin version, so this test proves nothing: %v", line)
		}
		r.now = at("10:05:00")
		r.orderFetches(orderNode("2026-08-23T09:00:00Z", 3))
		r.deliver(t, request("orders/updated", "gid://shopify/Order/1", "wh-parent", at("10:05:00")))
		if line := r.row("orderLineItem", "gid://shopify/LineItem/1"); line["quantity"] != float64(3) {
			t.Errorf("the line holds quantity %v, want its order's delivery's 3", line["quantity"])
		}
	})
}
