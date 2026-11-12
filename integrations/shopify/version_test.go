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

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// version_test.go -- every mirror write carries the origin's own version
// (Task 13c): the connector's half.
//
// The mirror refuses a write older than the row it holds. That rule is only
// as good as the versions on both sides, and for every versioned type they
// were a clock of this engine's: no generated document selected updatedAt, so
// a delivery and a sweep stamped the time they read the object, a bulk page
// the time its line was streamed, and a relist its tombstones the time they
// were written -- through a heal that did not ask the guard at all.
//
// THE RACES THAT MADE IT WRONG ARE PLAYED IN THE ROOT MODULE
// (component/datasync/shopify_version_test.go and its neighbours), through
// this connector, the runtime's own applier and a mirror both write into.
// They cannot live here: this module does not import the runtime
// (scripts/ci/module-boundaries.sh). What stays is what needs the connector
// alone -- the version a write carries, and the fake Admin's own honesty --
// and the mirror rig the connector's other tests share.

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

// mirrorRig is the harness with that mirror behind the connector: its own
// heal writes a sweep's rows into it, and its guards read them back.
type mirrorRig struct {
	*testHarness
	mirror *mirrorEngine
}

func newMirrorRig(t *testing.T) *mirrorRig {
	t.Helper()
	h := newHarness(t)
	m := &mirrorEngine{fakeEngine: h.engine, rows: map[string]map[string]any{}}
	h.conn.engine = m
	r := &mirrorRig{testHarness: h, mirror: m}
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

// exportStreamed finishes the export with these lines and returns its page:
// the writes the runtime would apply.
func (r *mirrorRig) exportStreamed(t *testing.T, concept, cursor string, lines ...map[string]any) memqlsync.BackfillPage {
	t.Helper()
	url := r.admin.serveJSONL("/download/"+concept+".jsonl", bulkLines(lines...))
	r.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "COMPLETED", url))
	page, err := r.conn.BackfillStore(context.Background(), r.store(t), generated.ConceptID(concept), cursor)
	if err != nil {
		t.Fatalf("stream the export: %v", err)
	}
	return page
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
		page := r.exportStreamed(t, "order", cursor, orderLines(origin, 1)...)
		if w := lineOf(t, page.Writes); w.Version != origin {
			t.Errorf("the bulk line's version = %s, want its order's %s", w.Version, origin)
		}
	})
	t.Run("bulk, resumed between the order and its line", func(t *testing.T) {
		r := newMirrorRig(t)
		url := r.admin.serveJSONL("/download/resumed.jsonl", bulkLines(orderLines(origin, 1)...))
		r.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "COMPLETED", url))
		cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", Line: 1, Rule: originVersionRule, At: exportStartedAt}
		page, err := r.conn.BackfillStore(context.Background(), r.store(t), generated.ConceptID("order"), cur.encode())
		if err != nil {
			t.Fatal(err)
		}
		if w := lineOf(t, page.Writes); w.Version != origin {
			t.Errorf("the resumed line's version = %s, want its order's %s", w.Version, origin)
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
