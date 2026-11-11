package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// partitions_test.go -- each store is a partition of the mirror.
//
// The sync runtime keeps one health row per partition of a connector that
// implements memqlsync.Partitioned. These are the connector's half: which
// stores are partitions, that every write says which store it belongs to, and
// that one store's answer is never another's.

// secondStoreID is the harness's second store, an opaque id and domain.
const secondStoreID = "store-b"

// storeRow is a harness store row with the given id, status and extras.
func storeRow(id, domain, status string, extra map[string]any) map[string]any {
	row := map[string]any{
		"id":                 id,
		"domain":             domain,
		"name":               id,
		"adminTokenRef":      "ACME_ADMIN",
		"webhookSecretRef":   "ACME_WEBHOOK",
		"apiVersion":         "",
		"protectedDataLevel": ProtectedLevel1,
		"status":             status,
		"ownerUserId":        "user-1",
	}
	for k, v := range extra {
		row[k] = v
	}
	return row
}

// withTwoStores gives the harness a second live store, answered by an Admin
// endpoint of its own, and returns that endpoint.
func withTwoStores(t *testing.T, h *testHarness) *fakeAdmin {
	t.Helper()
	h.engine.setRows("stores", []map[string]any{
		storeRow(testStoreID, "acme-widgets.myshopify.com", StatusLive, nil),
		storeRow(secondStoreID, secondStoreID, StatusLive, nil),
	})
	second := newFakeAdmin(t)
	h.conn.admin.endpoint = func(s Store) string {
		if s.ID == secondStoreID {
			return second.server.URL + "/graphql"
		}
		return h.admin.server.URL + "/graphql"
	}
	return second
}

func TestEveryMirrorWriteCarriesItsStore(t *testing.T) {
	h := newHarness(t)
	product := generated.Types["product"]
	obj := map[string]any{
		"id":        "gid://shopify/Product/1",
		"title":     "Shirt",
		"updatedAt": "2026-08-23T11:00:00Z",
		"variants": map[string]any{"nodes": []any{
			map[string]any{"id": "gid://shopify/ProductVariant/7", "title": "Small", "updatedAt": "2026-08-23T11:00:00Z"},
		}},
	}
	writes := mapObject(product, testStoreID, obj, "", h.now)
	if len(writes) < 2 {
		t.Fatalf("mapObject produced %d writes, want the product and its variant", len(writes))
	}
	for _, w := range writes {
		if w.Partition != testStoreID {
			t.Errorf("%s %s carries partition %q, want its store %q", w.Concept, w.RowId, w.Partition, testStoreID)
		}
	}
	if w := writes[len(writes)-1]; w.Concept != generated.ConceptID("productVariant") {
		t.Fatalf("the child write is %s, want a product variant", w.Concept)
	}

	gone := tombstone("product", secondStoreID, "gid://shopify/Product/2", h.now)
	if gone.Partition != secondStoreID {
		t.Errorf("a tombstone carries partition %q, want its store %q", gone.Partition, secondStoreID)
	}
}

func TestAStoreThatDoesNotIngestIsNotAPartition(t *testing.T) {
	h := newHarness(t)
	h.engine.setRows("stores", []map[string]any{
		storeRow(secondStoreID, secondStoreID, StatusConfigured, nil),
		storeRow(testStoreID, "acme-widgets.myshopify.com", StatusLive, nil),
		storeRow("store-paused", "store-paused", StatusPaused, nil),
		storeRow("store-uninstalled", "store-uninstalled", StatusLive, map[string]any{"uninstalledAt": "2026-08-20T10:00:00Z"}),
	})

	parts, err := h.conn.Partitions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{testStoreID, secondStoreID}; !slices.Equal(parts, want) {
		t.Fatalf("Partitions = %v, want the stores that ingest, sorted: %v", parts, want)
	}

	for _, id := range []string{"store-paused", "store-uninstalled", "no-such-store"} {
		_, err := h.conn.BackfillPartition(context.Background(), id, "v1:shopify:product", "")
		if err == nil || !strings.Contains(err.Error(), id) {
			t.Errorf("BackfillPartition(%q) = %v, want an error naming the store", id, err)
		}
		_, err = h.conn.ReconcilePartition(context.Background(), id, "v1:shopify:product", h.now)
		if err == nil || !strings.Contains(err.Error(), id) {
			t.Errorf("ReconcilePartition(%q) = %v, want an error naming the store", id, err)
		}
	}
	if seen := h.admin.seen(); len(seen) != 0 {
		t.Fatalf("the origin was asked %d times on behalf of stores that do not ingest", len(seen))
	}
}

func TestOneStoresRefusalIsItsOwn(t *testing.T) {
	h := newHarness(t)
	second := withTwoStores(t, h)
	const message = "This app is not approved to access the Order object. See https://shopify.dev/docs/apps/launch/protected-customer-data for more details."
	h.admin.reply("ShopifyListOrder", map[string]any{"orders": nil})
	h.admin.fail("ShopifyListOrder", GraphQLError{
		Message:    message,
		Extensions: map[string]any{"code": "ACCESS_DENIED"},
		Path:       []any{"orders"},
	})
	second.reply("ShopifyListOrder", listPageReply("orders", false, "",
		map[string]any{"id": "gid://shopify/Order/1", "name": "#1001", "updatedAt": "2026-08-23T11:00:00Z"},
		map[string]any{"id": "gid://shopify/Order/2", "name": "#1002", "updatedAt": "2026-08-23T11:00:00Z"},
	))

	_, err := h.conn.ReconcilePartition(context.Background(), testStoreID, "v1:shopify:order", h.now)
	if !memqlsync.IsNotGranted(err) || memqlsync.IsNotRequested(err) {
		t.Fatalf("store A's root refusal must be not granted for A, got %v", err)
	}

	report, err := h.conn.ReconcilePartition(context.Background(), secondStoreID, "v1:shopify:order", h.now)
	if err != nil {
		t.Fatalf("store B's sweep took on store A's refusal: %v", err)
	}
	if report.Checked != 2 {
		t.Fatalf("store B's report checked %d orders, want its own 2", report.Checked)
	}
	if second.countOp("ShopifyListOrder") != 1 {
		t.Fatalf("store B's endpoint was asked %d times, want once", second.countOp("ShopifyListOrder"))
	}
}

// stateRowEngine answers the health reads: syncStateById from a table of tip
// rows keyed by state id, and syncStatesAll with the whole history, which a
// reader of current state must never walk. It counts both.
type stateRowEngine struct {
	*fakeEngine
	tips    map[string]map[string]any
	history []map[string]any
	mu      stdsync.Mutex
	byID    map[string]int // state id -> reads
	listed  int
}

var stateIDArg = regexp.MustCompile(`stateId: "([^"]*)"`)

func (e *stateRowEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	switch callName(query) {
	case "syncStatesAll":
		e.mu.Lock()
		e.listed++
		e.mu.Unlock()
		return resultWithRows(e.history), nil
	case "syncStateById":
		m := stateIDArg.FindStringSubmatch(query)
		if m == nil {
			return nil, fmt.Errorf("syncStateById without a stateId: %s", query)
		}
		e.mu.Lock()
		e.byID[m[1]]++
		e.mu.Unlock()
		if row, ok := e.tips[m[1]]; ok {
			return resultWithRows([]map[string]any{row}), nil
		}
		return resultWithRows(nil), nil
	}
	return e.fakeEngine.Execute(ctx, query)
}

func (e *stateRowEngine) readsFor(store string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, concept := range generated.ApplyOrder {
		n += e.byID[memqlsync.StateID(generated.ConceptID(concept), ConnectorName, "inbound", store)]
	}
	return n
}

func TestStoreHealthShowsEachStoreItsOwnDomains(t *testing.T) {
	h := newHarness(t)
	withTwoStores(t, h)
	product, order := generated.ConceptID("product"), generated.ConceptID("order")
	id := func(concept, store string) string {
		return memqlsync.StateID(concept, ConnectorName, "inbound", store)
	}
	tip := func(concept, store string, fields map[string]any) map[string]any {
		row := map[string]any{"id": id(concept, store), "conceptId": concept, "connector": ConnectorName, "direction": "inbound", "partitionKey": store}
		for k, v := range fields {
			row[k] = v
		}
		return row
	}
	engine := &stateRowEngine{fakeEngine: h.engine, byID: map[string]int{}, tips: map[string]map[string]any{
		id(product, testStoreID):   tip(product, testStoreID, map[string]any{"lastError": "store A failed", "driftCount": 3}),
		id(product, secondStoreID): tip(product, secondStoreID, map[string]any{"backfillStatus": "queued", "driftCount": 1}),
		id(order, secondStoreID):   tip(order, secondStoreID, map[string]any{"backfillStatus": "running"}),
		// The shared row written before partitions belongs to no store.
		id(order, ""): tip(order, "", map[string]any{"lastError": "the shared row", "driftCount": 50}),
	}}
	// Store A has a long history behind its tip; store B has none.
	for i := 0; i < 250; i++ {
		engine.history = append(engine.history, tip(product, testStoreID, map[string]any{"lastError": "an older row of A", "driftCount": 9}))
	}
	h.conn.engine = engine

	nodes, err := NewIntegration(h.conn).handleStoreHealth(context.Background(), map[string]any{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if engine.listed != 0 {
		t.Fatalf("store health walked the history in syncStatesAll %d times; current state is read by id", engine.listed)
	}
	for _, store := range []string{testStoreID, secondStoreID} {
		if got, want := engine.readsFor(store), len(generated.ApplyOrder); got != want {
			t.Errorf("store %s cost %d by-id reads, want one for each of its %d domains", store, got, want)
		}
	}

	type domain struct {
		Concept   string `json:"concept"`
		Phase     string `json:"phase"`
		LastError string `json:"lastError"`
		DriftLast int    `json:"driftLast"`
	}
	var reply struct {
		Stores []struct {
			StoreID   string   `json:"storeId"`
			DriftLast int      `json:"driftLast"`
			Domains   []domain `json:"domains"`
		} `json:"stores"`
	}
	if len(nodes) != 1 || json.Unmarshal(nodes[0].Payload, &reply) != nil || len(reply.Stores) != 2 {
		t.Fatalf("storeHealth replied %d nodes: %v", len(nodes), nodes)
	}
	byStore := map[string][]domain{}
	drift := map[string]int{}
	for _, s := range reply.Stores {
		byStore[s.StoreID] = s.Domains
		drift[s.StoreID] = s.DriftLast
	}

	a := byStore[testStoreID]
	if len(a) != 1 || a[0].Concept != product || a[0].LastError != "store A failed" || a[0].Phase != "error" {
		t.Errorf("store A's domains = %+v, want only its own product row, at its tip", a)
	}
	if drift[testStoreID] != 3 {
		t.Errorf("store A's drift = %d, want its own 3", drift[testStoreID])
	}

	b := byStore[secondStoreID]
	if len(b) != 2 {
		t.Fatalf("store B's domains = %+v, want its own two rows", b)
	}
	for _, d := range b {
		if d.LastError != "" {
			t.Errorf("store B's %s carries an error that is not its own: %q", d.Concept, d.LastError)
		}
		if d.Phase != "backfilling" {
			t.Errorf("store B's %s phase = %q; queued and running both read backfilling", d.Concept, d.Phase)
		}
	}
	if drift[secondStoreID] != 1 {
		t.Errorf("store B's drift = %d, want its own 1", drift[secondStoreID])
	}
}

// ---- a store connected before this version gets its past (U3) ----

// stateRowsEngine answers syncStateById by the id it names, so a test can say
// which of a store's domains already has its row; every other call is the
// harness's.
type stateRowsEngine struct {
	*fakeEngine
	byID map[string]map[string]any
}

var stateIDArgRE = regexp.MustCompile(`stateId: "([^"]*)"`)

func (e *stateRowsEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	res, err := e.fakeEngine.Execute(ctx, q)
	if err != nil || callName(q) != "syncStateById" {
		return res, err
	}
	m := stateIDArgRE.FindStringSubmatch(q)
	if m == nil || e.byID[m[1]] == nil {
		return resultWithRows(nil), nil
	}
	return resultWithRows([]map[string]any{e.byID[m[1]]}), nil
}

// bootHarness is the harness with one managed store that ingests, one that is
// paused, and health rows answered by id from rows.
func bootHarness(t *testing.T, rows map[string]map[string]any) *testHarness {
	t.Helper()
	h := newHarness(t)
	scopes := []any{}
	for _, s := range managedConnectScopes() {
		scopes = append(scopes, s)
	}
	h.engine.setRows("stores", []map[string]any{
		storeRow(testStoreID, "acme-widgets.myshopify.com", StatusLive, map[string]any{"adminTokenRef": "ACME_OFFLINE_GRANT", "scopesGranted": scopes}),
		storeRow(secondStoreID, secondStoreID, StatusPaused, map[string]any{"adminTokenRef": "B_OFFLINE_GRANT", "scopesGranted": scopes}),
	})
	h.conn.stores.Invalidate()
	h.conn.engine = &stateRowsEngine{fakeEngine: h.engine, byID: rows}
	return h
}

var bootQueuedRE = regexp.MustCompile(`conceptId: "v1:shopify:(\w+)", connector: "shopify", partitionKey: "([^"]*)"`)

// bootQueued is the domains queued, as store/concept.
func bootQueued(t *testing.T, h *testHarness) []string {
	t.Helper()
	var out []string
	for _, q := range h.engine.callsTo("datasyncQueueBackfill") {
		m := bootQueuedRE.FindStringSubmatch(q)
		if m == nil || !strings.HasPrefix(q, "builtin ") {
			t.Fatalf("a queue is not the builtin naming a concept, the connector and a store: %s", q)
		}
		out = append(out, m[2]+"/"+m[1])
	}
	return out
}

func TestAStoreConnectedBeforeThisVersionHasItsBackfillsQueuedAtBoot(t *testing.T) {
	h := bootHarness(t, nil)
	Bootstrap(context.Background(), NewIntegration(h.conn), nil)

	var want []string
	for _, concept := range commerceBulkDomains {
		want = append(want, testStoreID+"/"+concept)
	}
	if got := bootQueued(t, h); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("queued at boot %v,\nwant every bulk domain of the ingesting store's connection %v", got, want)
	}
}

func TestBootQueuesNothingForADomainThatHasItsRow(t *testing.T) {
	has := func(concept string) (string, map[string]any) {
		id := memqlsync.StateID(generated.ConceptID(concept), ConnectorName, string(memqlsync.DirectionInbound), testStoreID)
		return id, map[string]any{"id": id, "conceptId": generated.ConceptID(concept), "connector": ConnectorName, "partitionKey": testStoreID, "backfillStatus": "complete"}
	}
	rows := map[string]map[string]any{}
	for _, concept := range []string{"order", "customer"} {
		id, row := has(concept)
		rows[id] = row
	}
	h := bootHarness(t, rows)
	Bootstrap(context.Background(), NewIntegration(h.conn), nil)

	got := bootQueued(t, h)
	for _, concept := range []string{"order", "customer"} {
		if slices.Contains(got, testStoreID+"/"+concept) {
			t.Errorf("boot queued %s, which has its row", concept)
		}
	}
	if len(got) != len(commerceBulkDomains)-2 {
		t.Errorf("boot queued %d domains, want the %d without a row: %v", len(got), len(commerceBulkDomains)-2, got)
	}

	// Every domain with its row: a second boot, or a second node, queues nothing.
	all := map[string]map[string]any{}
	for _, concept := range commerceBulkDomains {
		id, row := has(concept)
		all[id] = row
	}
	h = bootHarness(t, all)
	Bootstrap(context.Background(), NewIntegration(h.conn), nil)
	if got := bootQueued(t, h); len(got) != 0 {
		t.Errorf("a boot with every row in place queued %v", got)
	}
}

// A bulk backfill Shopify refuses is the origin's standing answer, as a sweep
// of the same domain is: not requested when the scope it names is outside the
// connection, not granted when a person could grant it. Inventory shipments
// and transfers are inside the connection by their generated scope and
// refused all the same.
func TestABulkBackfillShopifyRefusesIsAStandingAnswer(t *testing.T) {
	t.Run("outside the connection", func(t *testing.T) {
		h := newHarness(t)
		h.admin.userError("ShopifyBulkRun", "bulkOperationRunQuery",
			"Access denied for inventoryShipments field. Required access: `read_inventory_shipments` access scope.")
		_, err := h.conn.BackfillPartition(context.Background(), testStoreID, generated.ConceptID("inventoryShipment"), "")
		if !memqlsync.IsNotRequested(err) {
			t.Fatalf("err = %v, want not requested", err)
		}
	})
	t.Run("a scope the connection asks for", func(t *testing.T) {
		h := newHarness(t)
		h.admin.graphQLError("ShopifyBulkRun", "ACCESS_DENIED",
			"Access denied for customers field. Required access: `read_customers` access scope.")
		_, err := h.conn.BackfillPartition(context.Background(), testStoreID, generated.ConceptID("customer"), "")
		if !memqlsync.IsNotGranted(err) {
			t.Fatalf("err = %v, want not granted", err)
		}
	})
	t.Run("a grant that reaches none of the domain's scopes", func(t *testing.T) {
		h := newHarness(t)
		h.engine.setRows("stores", []map[string]any{storeRow(testStoreID, "acme-widgets.myshopify.com", StatusLive, map[string]any{"scopesGranted": []any{"read_products"}})})
		h.conn.stores.Invalidate()
		_, err := h.conn.BackfillPartition(context.Background(), testStoreID, generated.ConceptID("customer"), "")
		if !memqlsync.IsNotGranted(err) || !strings.Contains(err.Error(), "read_customers") {
			t.Fatalf("err = %v, want not granted naming read_customers", err)
		}
		if n := h.admin.countOp("ShopifyBulkRun"); n != 0 {
			t.Errorf("Shopify was asked %d times for a domain the grant cannot reach", n)
		}
	})
	t.Run("a real failure", func(t *testing.T) {
		h := newHarness(t)
		h.admin.httpStatus("ShopifyBulkRun", http.StatusBadGateway)
		_, err := h.conn.BackfillPartition(context.Background(), testStoreID, generated.ConceptID("customer"), "")
		if err == nil || memqlsync.IsNotGranted(err) || memqlsync.IsNotRequested(err) {
			t.Fatalf("err = %v, want a failure that is not a standing answer", err)
		}
	})
}
