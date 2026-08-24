package shopify

import (
	"context"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// reconcile_test.go -- catching what live delivery lost (#4394).

func listPageReply(connection string, hasNext bool, cursor string, nodes ...map[string]any) map[string]any {
	list := make([]any, 0, len(nodes))
	for _, n := range nodes {
		list = append(list, n)
	}
	return map[string]any{connection: map[string]any{
		"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": cursor},
		"nodes":    list,
	}}
}

func TestReconcileByUpdatedAtSendsTheFilterAndMapsTheRows(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyListProduct", listPageReply("products", false, "",
		map[string]any{"id": "gid://shopify/Product/1", "title": "Shirt", "updatedAt": "2026-08-23T11:00:00Z"},
	))
	since := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)

	report, err := h.conn.Reconcile(context.Background(), "v1:shopify:product", since)
	if err != nil {
		t.Fatal(err)
	}
	if report.Checked != 1 || report.Healed != 1 {
		t.Fatalf("report = %+v", report)
	}
	seen := h.admin.seen()
	if len(seen) == 0 {
		t.Fatal("nothing was asked of the Admin API")
	}
	filter, _ := seen[0].Variables["query"].(string)
	if !strings.HasPrefix(filter, "updated_at:>") || !strings.Contains(filter, "2026-08-23T10:00:00Z") {
		t.Errorf("query filter = %q", filter)
	}
}

func TestReconcileByUpdatedAtPagesToTheEnd(t *testing.T) {
	h := newHarness(t)
	calls := 0
	// The fake answers per operation, so page two is installed after the
	// first call by swapping the reply.
	h.admin.reply("ShopifyListProduct", listPageReply("products", true, "cursor-1",
		map[string]any{"id": "gid://shopify/Product/1", "title": "One", "updatedAt": "2026-08-23T11:00:00Z"}))

	go func() {
		for {
			if len(h.admin.seen()) > 0 {
				h.admin.reply("ShopifyListProduct", listPageReply("products", false, "",
					map[string]any{"id": "gid://shopify/Product/2", "title": "Two", "updatedAt": "2026-08-23T11:00:00Z"}))
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	report, err := h.conn.Reconcile(context.Background(), "v1:shopify:product", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	calls = h.admin.countOp("ShopifyListProduct")
	if calls < 2 {
		t.Fatalf("paged %d times, want at least two", calls)
	}
	// The fake keeps answering page two once it is swapped in, so the walk
	// runs to its own cap -- which is the point being checked: it PAGES,
	// following the cursor, rather than reading one page and stopping.
	if report.Checked < 2 {
		t.Errorf("checked %d rows; the walk did not follow the cursor", report.Checked)
	}
	second := h.admin.seen()[1]
	if second.Variables["after"] != "cursor-1" {
		t.Errorf("page two was not requested after the cursor: %v", second.Variables)
	}
}

// A full re-list tombstones what the origin no longer returns. This is the
// only path that can notice a deletion in a domain Shopify publishes no
// delete topic for.
func TestAFullRelistTombstonesAnAbsentRow(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyListGiftCard", listPageReply("giftCards", false, "",
		map[string]any{"id": "gid://shopify/GiftCard/1", "note": "still here"},
	))
	h.engine.setRows("shopifyGiftCardForStore", []map[string]any{
		{"id": "shp1", "gid": "gid://shopify/GiftCard/1"},
		{"id": "shp2", "gid": "gid://shopify/GiftCard/2"},
	})

	report, err := h.conn.Reconcile(context.Background(), "v1:shopify:giftCard", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// Two healed: the row the origin still has, re-applied, and the row it
	// no longer has, retired.
	if report.Healed != 2 || report.Drifted != 2 {
		t.Fatalf("report = %+v, want the live row healed and the absent one retired", report)
	}
	var retired bool
	for _, call := range h.engine.calls() {
		if strings.Contains(call, `"deleted":true`) && strings.Contains(call, MirrorRowID(testStoreID, "gid://shopify/GiftCard/2")) {
			retired = true
		}
	}
	if !retired {
		t.Error("the absent row was not retired")
	}
}

// A pass that hits the page cap tombstones NOTHING. Tombstoning is an
// argument from absence, and the argument is only valid if the walk was
// complete; tombstoning on a partial walk would delete the tail of a large
// domain every pass and re-create it on the next.
func TestAPartialRelistTombstonesNothing(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyListGiftCard", listPageReply("giftCards", true, "always-more",
		map[string]any{"id": "gid://shopify/GiftCard/1"},
	))
	h.engine.setRows("shopifyGiftCardForStore", []map[string]any{
		{"id": "shp2", "gid": "gid://shopify/GiftCard/2"},
	})

	report, err := h.conn.Reconcile(context.Background(), "v1:shopify:giftCard", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	_ = report
	for _, call := range h.engine.calls() {
		if strings.Contains(call, `"deleted":true`) {
			t.Fatalf("a capped walk retired a row:\n%s", call)
		}
	}
}

// A domain that reconciles neither way is a no-op rather than a
// not-implemented: a child materialised with its parent has genuinely
// nothing to sweep, and the runtime should record a clean pass rather
// than an unconfigured capability.
func TestADomainThatReconcilesNeitherWayIsANoOp(t *testing.T) {
	h := newHarness(t)
	report, err := h.conn.Reconcile(context.Background(), "v1:shopify:orderLineItem", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Checked != 0 || len(h.admin.seen()) != 0 {
		t.Fatalf("report = %+v, admin calls = %d", report, len(h.admin.seen()))
	}
}

// A concept this connector does not mirror at all is NOT-IMPLEMENTED, and
// the distinction matters to the runtime: it records an unconfigured
// capability rather than a failed sweep.
func TestAnUnknownConceptIsNotImplemented(t *testing.T) {
	h := newHarness(t)
	_, err := h.conn.Reconcile(context.Background(), "v1:something:else", time.Time{})
	if !memqlsync.IsNotImplemented(err) {
		t.Fatalf("err = %v, want the contract's not-implemented error", err)
	}
}
