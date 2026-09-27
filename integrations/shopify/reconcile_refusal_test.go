package shopify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// reconcile_refusal_test.go -- the sweep's answers to the origin's refusals.
//
// Every case here is one a live cluster hit on every ten-minute tick, logged
// as a sweep failure each time, none of which the next tick could change.

func denied(field string, path ...any) GraphQLError {
	return GraphQLError{
		Message:    "Access denied for " + field + " field. Required access: `read_publications` access scope.",
		Extensions: map[string]any{"code": "ACCESS_DENIED"},
		Path:       path,
	}
}

func withScopes(t *testing.T, h *testHarness, scopes ...string) {
	t.Helper()
	h.engine.setRows("stores", []map[string]any{{
		"id":                 testStoreID,
		"domain":             "acme-widgets.myshopify.com",
		"name":               "Acme Widgets",
		"adminTokenRef":      "ACME_ADMIN",
		"webhookSecretRef":   "ACME_WEBHOOK",
		"apiVersion":         "",
		"protectedDataLevel": ProtectedLevel1,
		"status":             StatusLive,
		"ownerUserId":        "user-1",
		"scopesGranted":      toAny(scopes),
	}})
}

func TestAStoreGrantedNoneOfADomainsScopesIsNotAsked(t *testing.T) {
	h := newHarness(t)
	withScopes(t, h, "read_products", "read_orders")

	_, err := h.conn.Reconcile(context.Background(), "v1:shopify:blog", h.now)
	if !memqlsync.IsNotGranted(err) {
		t.Fatalf("a domain under read_content on a store without it must be not-granted, got %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "read_content") {
		t.Errorf("the refusal does not name the scope a grant would need: %s", got)
	}
	if seen := h.admin.seen(); len(seen) != 0 {
		t.Fatalf("the origin was asked %d times about a domain the grant cannot reach", len(seen))
	}
}

func TestAStoreGrantedSomeOfADomainsScopesIsStillAsked(t *testing.T) {
	// orders lists under read_orders; read_all_orders only widens the window.
	h := newHarness(t)
	withScopes(t, h, "read_orders")
	h.admin.reply("ShopifyListOrder", listPageReply("orders", false, ""))

	if _, err := h.conn.Reconcile(context.Background(), "v1:shopify:order", h.now); err != nil {
		t.Fatalf("a domain the store holds one scope for must be swept: %v", err)
	}
	if seen := h.admin.seen(); len(seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(seen))
	}
}

func TestAFieldLevelDenialIsPrunedFromTheDocumentAndTheRowsStillLand(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyListProductVariant", listPageReply("productVariants", false, "",
		map[string]any{"id": "gid://shopify/ProductVariant/1", "title": "Small", "updatedAt": "2026-08-23T11:00:00Z"},
	))
	// Non-null fields the grant does not cover: GraphQL null-propagates the
	// refusal up to the connection, so the first answer carries no page.
	h.admin.denyField("ShopifyListProductVariant", "availablePublicationsCount", map[string]any{"productVariants": nil})
	h.admin.denyField("ShopifyListProductVariant", "deliveryProfile", map[string]any{"productVariants": nil})

	report, err := h.conn.Reconcile(context.Background(), "v1:shopify:productVariant", h.now)
	if err != nil {
		t.Fatalf("a denial of two fields was treated as a failure: %v", err)
	}
	if report.Checked != 1 || report.Healed != 1 {
		t.Fatalf("report = %+v; the row must land once the denied fields are out of the document", report)
	}
	seen := h.admin.seen()
	if len(seen) != 2 {
		t.Fatalf("requests = %d, want the refused ask and the pruned re-ask", len(seen))
	}
	for _, field := range []string{"availablePublicationsCount", "deliveryProfile"} {
		if !strings.Contains(seen[0].Query, field) {
			t.Errorf("the first document did not select %s", field)
		}
		if strings.Contains(seen[1].Query, field) {
			t.Errorf("the re-ask still selects %s", field)
		}
	}
	if !strings.Contains(seen[1].Query, "title") || !strings.Contains(seen[1].Query, "pageInfo") {
		t.Errorf("pruning took more than the denied fields:\n%s", seen[1].Query)
	}
	got := h.conn.FieldsDenied(testStoreID)["v1:shopify:productVariant"]
	if len(got) != 2 || got[0] != "availablePublicationsCount" || got[1] != "deliveryProfile" {
		t.Errorf("FieldsDenied = %v; the health page needs to name what the grant left out", got)
	}
	// A later sweep starts from the pruned document: one request, no re-ask.
	h.admin.requests = nil
	if _, err := h.conn.Reconcile(context.Background(), "v1:shopify:productVariant", h.now); err != nil {
		t.Fatal(err)
	}
	if again := h.admin.seen(); len(again) != 1 || strings.Contains(again[0].Query, "deliveryProfile") {
		t.Errorf("the second sweep did not start from the pruned document: %d requests", len(again))
	}
}

func TestATruncatedErrorListIsStillOnlyDenials(t *testing.T) {
	ae := &AdminError{StatusCode: 200, Errors: []GraphQLError{
		denied("availablePublicationsCount", "productVariants", "nodes", 0, "availablePublicationsCount"),
		denied("availablePublicationsCount", "productVariants", "nodes", 1, "availablePublicationsCount"),
		{Message: "Too many execution errors, max error limit reached. Results truncated"},
	}}
	if !ae.DeniedOnly() {
		t.Fatal("the truncation notice is about the error list, not a further failure")
	}
	if fields := ae.DeniedFields(); len(fields) != 1 || fields[0] != "availablePublicationsCount" {
		t.Errorf("DeniedFields = %v", fields)
	}
}

func TestPruneSelectionsDropsOnlyTheNamedFields(t *testing.T) {
	doc := strings.Join([]string{
		"query ShopifyFetchOrder($id: ID!) {",
		"  node(id: $id) {",
		"    ... on Order {",
		"      id",
		"      availablePublicationsCount { count precision }",
		"      lineItems(first: 100) {",
		"        nodes {",
		"          id",
		"          deliveryProfile { id }",
		"          title",
		"        }",
		"      }",
		"      name",
		"    }",
		"  }",
		"}",
	}, "\n")
	got := pruneSelections(doc, []string{"availablePublicationsCount", "deliveryProfile", "lineItems"})
	for _, gone := range []string{"availablePublicationsCount", "deliveryProfile", "lineItems", "nodes {"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q survived pruning:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"query ShopifyFetchOrder", "node(id: $id)", "... on Order", "      id", "      name", "    }\n  }\n}"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q was lost:\n%s", kept, got)
		}
	}
	if pruneSelections(doc, []string{"id", "nodes", "node"}) != doc {
		t.Error("a reserved selection was pruned")
	}
}

func TestADenialOfTheWholeConnectionIsNotGranted(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyListBlog", map[string]any{"blogs": nil})
	h.admin.fail("ShopifyListBlog", GraphQLError{
		Message:    "Access denied for blogs field. Required access: `read_content` access scope.",
		Extensions: map[string]any{"code": "ACCESS_DENIED"},
		Path:       []any{"blogs"},
	})

	_, err := h.conn.Reconcile(context.Background(), "v1:shopify:blog", h.now)
	if !memqlsync.IsNotGranted(err) {
		t.Fatalf("a connection the grant does not reach must be not-granted, got %v", err)
	}
}

func TestAQueryTheOriginRejectsIsPermanent(t *testing.T) {
	h := newHarness(t)
	h.admin.fail("ShopifyListProduct", GraphQLError{
		Message:    "Field 'id' doesn't exist on type 'ResourcePublication'",
		Extensions: map[string]any{"code": "undefinedField"},
	})

	_, err := h.conn.Reconcile(context.Background(), "v1:shopify:product", h.now)
	if !memqlsync.IsPermanent(err) {
		t.Fatalf("a rejected query must be permanent so the runtime holds it to the cadence, got %v", err)
	}
	var ae *AdminError
	if !errors.As(err, &ae) {
		t.Errorf("the origin's own words were lost: %v", err)
	}
}

func TestAPageOverTheCostCeilingShrinksAndIsRemembered(t *testing.T) {
	h := newHarness(t)
	h.admin.failOnce("ShopifyListProduct", GraphQLError{
		Message:    "Query cost is 1649, which exceeds the single query max cost limit (1000).",
		Extensions: map[string]any{"code": "MAX_COST_EXCEEDED"},
	})
	h.admin.reply("ShopifyListProduct", listPageReply("products", false, "",
		map[string]any{"id": "gid://shopify/Product/1", "title": "Shirt", "updatedAt": "2026-08-23T11:00:00Z"},
	))

	report, err := h.conn.Reconcile(context.Background(), "v1:shopify:product", h.now)
	if err != nil {
		t.Fatalf("a cost refusal must be backed away from, not failed: %v", err)
	}
	if report.Checked != 1 {
		t.Fatalf("report = %+v", report)
	}
	seen := h.admin.seen()
	if len(seen) != 2 {
		t.Fatalf("requests = %d, want the refused page and the smaller retry", len(seen))
	}
	first, _ := seen[0].Variables["first"].(float64)
	second, _ := seen[1].Variables["first"].(float64)
	if !(second < first) || second > 1000.0/1649.0*first {
		t.Errorf("retry page = %v after %v was refused at cost 1649/1000; it must fit the ceiling", second, first)
	}
	if got := h.conn.pageSizeFor(h.store(t), generated.Types["product"]); got != int(second) {
		t.Errorf("the size that fit was not remembered for the next sweep: %d vs %v", got, second)
	}
}

func TestAnUnfilterableConnectionIsSweptWithoutAWatermark(t *testing.T) {
	// The generated list operation declares `$query` only for a connection
	// that accepts it; sending the variable anyway was "variableNotUsed"
	// from the origin, on every tick, for seven domains.
	h := newHarness(t)
	base := generated.Types["product"]
	spec := *base
	spec.ListFilterable = false
	h.admin.reply(spec.ListOp, listPageReply(spec.ListQuery, false, "",
		map[string]any{"id": "gid://shopify/Product/1", "title": "Shirt", "updatedAt": "2026-08-23T11:00:00Z"},
	))

	report, err := h.conn.reconcileByUpdatedAt(context.Background(), h.store(t), &spec, h.now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.Checked != 1 {
		t.Fatalf("report = %+v", report)
	}
	seen := h.admin.seen()
	if len(seen) != 1 {
		t.Fatalf("requests = %d", len(seen))
	}
	if _, sent := seen[0].Variables["query"]; sent {
		t.Errorf("a `query` variable was sent to an operation that does not declare one: %v", seen[0].Variables)
	}
}

func TestAPageThatCostsTooMuchShrinksAllTheWayToOne(t *testing.T) {
	// An order with its line items and fulfilments can cost a quarter of the
	// ceiling; the sweep on a real store went 100 -> 20 -> 7 -> 5 and was
	// refused at five, which a floor of five could not get under.
	h := newHarness(t)
	cost := func(n int) GraphQLError {
		return GraphQLError{
			Message:    fmt.Sprintf("Query cost is %d, which exceeds the single query max cost limit (1000).", n),
			Extensions: map[string]any{"code": "MAX_COST_EXCEEDED"},
		}
	}
	for _, n := range []int{3935, 2187, 1313, 1100} {
		h.admin.failOnce("ShopifyListOrder", cost(n))
	}
	h.admin.reply("ShopifyListOrder", listPageReply("orders", false, "",
		map[string]any{"id": "gid://shopify/Order/1", "name": "#1001", "updatedAt": "2026-08-23T11:00:00Z"},
	))

	report, err := h.conn.Reconcile(context.Background(), "v1:shopify:order", h.now)
	if err != nil {
		t.Fatalf("four cost refusals must be backed away from, not failed: %v", err)
	}
	if report.Checked != 1 {
		t.Fatalf("report = %+v", report)
	}
	seen := h.admin.seen()
	if len(seen) != 5 {
		t.Fatalf("requests = %d, want four refused pages and the one that fit", len(seen))
	}
	last := -1.0
	for i, req := range seen {
		first, _ := req.Variables["first"].(float64)
		if i > 0 && !(first < last) {
			t.Errorf("request %d asked for %v after %v was refused", i, first, last)
		}
		last = first
	}
	if last >= 5 {
		t.Errorf("the page that fit was %v; the floor has to reach below five", last)
	}
}

func TestAWriteScopeGrantsTheDomainItsReadScopeWould(t *testing.T) {
	// Shopify reports write_products for a grant that includes reading
	// products; a store connected that way is not "without read_products".
	h := newHarness(t)
	withScopes(t, h, "write_products")
	h.admin.reply("ShopifyListProduct", listPageReply("products", false, ""))

	if _, err := h.conn.Reconcile(context.Background(), "v1:shopify:product", h.now); err != nil {
		t.Fatalf("a write grant must count as its read grant: %v", err)
	}
	if seen := h.admin.seen(); len(seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(seen))
	}
}

func TestOnlyAStandingRefusalIsPermanent(t *testing.T) {
	internal := &AdminError{StatusCode: 200, Errors: []GraphQLError{{Message: "Internal error. Looks like something went wrong on our end.", Extensions: map[string]any{"code": "INTERNAL_SERVER_ERROR"}}}}
	if internal.Standing() {
		t.Error("Shopify's own INTERNAL_SERVER_ERROR names the moment, not the document; it must be retried")
	}
	unnamed := &AdminError{StatusCode: 200, Errors: []GraphQLError{{Message: "Timeout"}}}
	if unnamed.Standing() {
		t.Error("an error with no code is not known to be standing")
	}
	validation := &AdminError{StatusCode: 200, Errors: []GraphQLError{{Message: "Field 'id' doesn't exist on type 'ResourcePublication'", Extensions: map[string]any{"code": "undefinedField"}}}}
	if !validation.Standing() {
		t.Error("a document the schema refuses is refused tomorrow too")
	}
	notFound := &AdminError{StatusCode: 404}
	if !notFound.Standing() {
		t.Error("a 404 is standing")
	}

	h := newHarness(t)
	h.admin.fail("ShopifyListProduct", GraphQLError{Message: "Internal error.", Extensions: map[string]any{"code": "INTERNAL_SERVER_ERROR"}})
	_, err := h.conn.Reconcile(context.Background(), "v1:shopify:product", h.now)
	if err == nil || memqlsync.IsPermanent(err) || memqlsync.IsNotGranted(err) {
		t.Fatalf("a transient origin failure must surface as a plain error the next tick retries, got %v", err)
	}
}

func TestANotGrantedDomainIsNotAnErrorOnTheStorePanel(t *testing.T) {
	if got := phaseOf(map[string]any{"lastError": memqlsync.NotGranted("shopify", "read_content").Error()}); got != "not granted" {
		t.Errorf("phase = %q, want a standing fact, not an error", got)
	}
	if got := phaseOf(map[string]any{"lastError": "shopify admin: HTTP 502"}); got != "error" {
		t.Errorf("phase = %q for a real failure", got)
	}
}
