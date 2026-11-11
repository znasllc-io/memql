package shopify

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
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

// pagedStatesEngine answers syncStatesAll a page at a time, by the cursor the
// context carries, so a read that stops at the first page is caught.
type pagedStatesEngine struct {
	*fakeEngine
	pages map[string]*memql.ExecuteResult
}

func (e *pagedStatesEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if callName(query) != "syncStatesAll" {
		return e.fakeEngine.Execute(ctx, query)
	}
	page, ok := e.pages[memql.CursorFromContext(ctx)]
	if !ok {
		return resultWithRows(nil), nil
	}
	return page, nil
}

func TestStoreHealthShowsEachStoreItsOwnDomains(t *testing.T) {
	h := newHarness(t)
	withTwoStores(t, h)
	paged := &pagedStatesEngine{fakeEngine: h.engine}
	h.conn.engine = paged

	// Newest first, as the query sorts. The shared row -- written before
	// partitions -- belongs to no store. Store B's rows are on the second
	// page.
	first := resultWithRows([]map[string]any{
		{"id": "s1", "conceptId": "v1:shopify:product", "partitionKey": testStoreID, "connector": ConnectorName, "lastError": "store A failed", "driftCount": 3},
		{"id": "s2", "conceptId": "v1:shopify:product", "partitionKey": testStoreID, "connector": ConnectorName, "lastError": "an older row of A", "driftCount": 9},
		{"id": "s3", "conceptId": "v1:shopify:order", "connector": ConnectorName, "lastError": "the shared row", "driftCount": 50},
	})
	first.Meta = &memql.ResultMeta{Cursor: "page-2", HasMore: true}
	second := resultWithRows([]map[string]any{
		{"id": "s4", "conceptId": "v1:shopify:product", "partitionKey": secondStoreID, "connector": ConnectorName, "backfillStatus": "queued", "driftCount": 1},
		{"id": "s5", "conceptId": "v1:shopify:order", "partitionKey": secondStoreID, "connector": ConnectorName, "backfillStatus": "running"},
	})
	paged.pages = map[string]*memql.ExecuteResult{"": first, "page-2": second}

	nodes, err := NewIntegration(h.conn).handleStoreHealth(context.Background(), map[string]any{}, 0)
	if err != nil {
		t.Fatal(err)
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
	if len(a) != 1 || a[0].Concept != "v1:shopify:product" || a[0].LastError != "store A failed" || a[0].Phase != "error" {
		t.Errorf("store A's domains = %+v, want only its own product row, at its tip", a)
	}
	if drift[testStoreID] != 3 {
		t.Errorf("store A's drift = %d, want its own 3", drift[testStoreID])
	}

	b := byStore[secondStoreID]
	if len(b) != 2 {
		t.Fatalf("store B's domains = %+v, want its two rows from the second page", b)
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
