package shopify

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// children_test.go -- a child connection read past its first page (F9).
//
// A fetch selects a child connection one page deep (an order's lineItems
// take the first 100). An order with more lines than that is a real order,
// and the lines past the page are lines the mirror would never hear of.

const orderGID = "gid://shopify/Order/7"

func lineItem(n int) map[string]any {
	return map[string]any{"id": fmt.Sprintf("gid://shopify/LineItem/%d", n), "name": fmt.Sprintf("line %d", n), "quantity": 1}
}

func lineItemPage(from, to int, hasNext bool, cursor string) map[string]any {
	nodes := []any{}
	for n := from; n <= to; n++ {
		nodes = append(nodes, lineItem(n))
	}
	return map[string]any{
		"nodes":    nodes,
		"pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": cursor},
	}
}

func orderWithLines(page map[string]any) map[string]any {
	return map[string]any{"node": map[string]any{
		"__typename": "Order",
		"id":         orderGID,
		"name":       "#1001",
		"updatedAt":  "2026-08-23T11:00:00Z",
		"lineItems":  page,
	}}
}

// lineItemGIDs is the set of orderLineItem rows among the writes.
func lineItemGIDs(writes []memqlsync.MirrorWrite) map[string]bool {
	got := map[string]bool{}
	for _, w := range writes {
		if w.Concept == generated.ConceptID("orderLineItem") {
			gid, _ := w.Payload["gid"].(string)
			got[gid] = true
		}
	}
	return got
}

func TestAnOrderWithMoreThanAHundredLinesKeepsThemAll(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyFetchOrder", orderWithLines(lineItemPage(1, 100, true, "cursor-100")))
	h.admin.reply("ShopifyPageOrderLineItems", orderWithLines(lineItemPage(101, 101, false, "cursor-101")))

	writes, err := h.conn.Apply(context.Background(), delivery("orders/updated", orderGID))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range writes {
		if w.Concept == generated.ConceptID("orderLineItem") && (w.Payload["parentGid"] != orderGID || w.Partition != testStoreID) {
			t.Errorf("line %v: parentGid %v, partition %q", w.Payload["gid"], w.Payload["parentGid"], w.Partition)
		}
	}
	lines := lineItemGIDs(writes)
	if len(lines) != 101 {
		t.Fatalf("want 101 orderLineItem writes, got %d (admin saw %v)", len(lines), h.admin.seenOps())
	}
	if !lines["gid://shopify/LineItem/101"] {
		t.Error("the 101st line, which only the second page carries, was not written")
	}
	pages := 0
	for _, r := range h.admin.seen() {
		if r.Operation != "ShopifyPageOrderLineItems" {
			continue
		}
		pages++
		if r.Variables["id"] != orderGID || r.Variables["after"] != "cursor-100" {
			t.Errorf("the page request asked for %v, want the order's id and the first page's end cursor", r.Variables)
		}
	}
	if pages != 1 {
		t.Errorf("the second page was asked for %d times, want once", pages)
	}
}

// A sweep lists orders with the same one-page child selection, so it reads
// the rest the same way: a line only the second page carries is healed.
func TestASweepReadsAnOrdersLinesPastTheFirstPage(t *testing.T) {
	h := newHarness(t)
	order := orderWithLines(lineItemPage(1, 100, true, "cursor-100"))["node"].(map[string]any)
	h.admin.reply("ShopifyListOrder", listPageReply("orders", false, "", order))
	h.admin.reply("ShopifyPageOrderLineItems", orderWithLines(lineItemPage(101, 101, false, "cursor-101")))

	if _, err := h.conn.Reconcile(context.Background(), generated.ConceptID("order"), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if n := h.admin.countOp("ShopifyPageOrderLineItems"); n != 1 {
		t.Fatalf("the sweep asked for the second page of lines %d times, want once", n)
	}
	var written bool
	for _, call := range h.engine.calls() {
		if strings.Contains(call, MirrorRowID(testStoreID, "gid://shopify/LineItem/101")) {
			written = true
		}
	}
	if !written {
		t.Error("the 101st line was not written by the sweep")
	}
}

// capturedLog points the connector's log at a buffer the test reads.
func capturedLog(h *testHarness) *bytes.Buffer {
	var buf bytes.Buffer
	h.conn.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return &buf
}

// pagedLines answers each page request with the next line and a new
// cursor, always promising more.
func pagedLines(vars map[string]any) any {
	var n int
	_, _ = fmt.Sscanf(fmt.Sprint(vars["after"]), "cursor-%d", &n)
	return orderWithLines(lineItemPage(n+1, n+1, true, fmt.Sprintf("cursor-%d", n+1)))
}

// The ceiling: a connection that always promises more is read fifty pages
// deep, the first and forty-nine after it, every page a different line;
// the order is written with each of them and the log says it was cut.
func TestAChildConnectionStopsAtItsPageCeiling(t *testing.T) {
	h := newHarness(t)
	logged := capturedLog(h)
	h.admin.reply("ShopifyFetchOrder", orderWithLines(lineItemPage(1, 1, true, "cursor-1")))
	h.admin.replyWith("ShopifyPageOrderLineItems", pagedLines)

	writes, err := h.conn.Apply(context.Background(), delivery("orders/updated", orderGID))
	if err != nil {
		t.Fatal(err)
	}
	if n := h.admin.countOp("ShopifyPageOrderLineItems"); n != maxChildPages-1 {
		t.Errorf("asked for %d further pages, want %d", n, maxChildPages-1)
	}
	lines := lineItemGIDs(writes)
	if len(lines) != maxChildPages || !lines[fmt.Sprintf("gid://shopify/LineItem/%d", maxChildPages)] {
		t.Errorf("wrote %d distinct lines, want one per page read (%d)", len(lines), maxChildPages)
	}
	if out := logged.String(); !strings.Contains(out, "longer than its page ceiling") ||
		!strings.Contains(out, "concept=order") || !strings.Contains(out, "connection=lineItems") {
		t.Errorf("the ceiling was not said in the log, naming the concept and the child:\n%s", out)
	}
}

// A page that hands back the cursor it was asked with is the origin not
// advancing: the read stops there, with no line taken twice, and says so.
func TestAChildPageThatRepeatsItsCursorStops(t *testing.T) {
	h := newHarness(t)
	logged := capturedLog(h)
	h.admin.reply("ShopifyFetchOrder", orderWithLines(lineItemPage(1, 1, true, "cursor-1")))
	h.admin.reply("ShopifyPageOrderLineItems", orderWithLines(lineItemPage(2, 2, true, "cursor-1")))

	writes, err := h.conn.Apply(context.Background(), delivery("orders/updated", orderGID))
	if err != nil {
		t.Fatal(err)
	}
	if n := h.admin.countOp("ShopifyPageOrderLineItems"); n != 1 {
		t.Errorf("asked for %d further pages, want the one that repeated its cursor", n)
	}
	lines := 0
	for _, w := range writes {
		if w.Concept == generated.ConceptID("orderLineItem") {
			lines++
		}
	}
	if lines != 2 {
		t.Errorf("wrote %d line rows, want the two distinct lines once each", lines)
	}
	if out := logged.String(); !strings.Contains(out, "repeated its cursor") ||
		!strings.Contains(out, "concept=order") || !strings.Contains(out, "connection=lineItems") {
		t.Errorf("the repeated cursor was not said in the log, naming the concept and the child:\n%s", out)
	}
}

// A bulk operation streams every node of a nested connection and refuses
// pagination on one, so no bulk document selects a pageInfo; every fetch
// document selects one on each paged child, and names its page operation.
func TestABulkDocumentCarriesNoPageInfo(t *testing.T) {
	for concept, spec := range generated.Types {
		if strings.Contains(spec.BulkDocument, "pageInfo") {
			t.Errorf("%s: the bulk document selects a pageInfo", concept)
		}
		for _, child := range spec.Children {
			if child.List {
				if child.PageOp != "" {
					t.Errorf("%s.%s: a plain list names a page operation", concept, child.Connection)
				}
				continue
			}
			if child.PageOp == "" || !strings.Contains(spec.FetchDocument, "query "+child.PageOp+"(") {
				t.Errorf("%s.%s: page operation %q is not in the fetch document", concept, child.Connection, child.PageOp)
			}
		}
	}
	order := generated.Types["order"]
	if !strings.Contains(order.FetchDocument, "lineItems(first: 100) {\n        pageInfo { hasNextPage endCursor }") {
		t.Error("the order fetch does not ask whether more lines follow")
	}
	if !strings.Contains(order.BulkDocument, "lineItems {") {
		t.Error("the order bulk document no longer streams its lines")
	}
}
