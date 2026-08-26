package shopify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// backfill_test.go -- Bulk Operations, streamed and resumable (#4394).

func bulkLines(lines ...map[string]any) string {
	var b strings.Builder
	for _, l := range lines {
		raw, _ := json.Marshal(l)
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

func startedOperation(id string) map[string]any {
	return map[string]any{"bulkOperationRunQuery": map[string]any{
		"bulkOperation": map[string]any{"id": id, "status": "CREATED"},
		"userErrors":    []any{},
	}}
}

func operationStatus(id, status, url string) map[string]any {
	return map[string]any{"node": map[string]any{
		"id": id, "status": status, "url": url, "objectCount": "2",
	}}
}

func TestBackfillStartsPollsThenStreams(t *testing.T) {
	h := newHarness(t)
	store := h.store(t)
	ctx := context.Background()

	// 1. No cursor -> the operation is started and nothing is written yet.
	h.admin.reply("ShopifyBulkRun", startedOperation("gid://shopify/BulkOperation/1"))
	page, err := h.conn.BackfillStore(ctx, store, "v1:shopify:product", "")
	if err != nil {
		t.Fatal(err)
	}
	if page.Done || len(page.Writes) != 0 || page.NextCursor == "" {
		t.Fatalf("start step = %+v", page)
	}

	// 2. Still running -> the cursor is unchanged and nothing is written.
	h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "RUNNING", ""))
	page, err = h.conn.BackfillStore(ctx, store, "v1:shopify:product", page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if page.Done || len(page.Writes) != 0 {
		t.Fatalf("polling step = %+v", page)
	}

	url := h.admin.serveJSONL("/download/products.jsonl", bulkLines(
		map[string]any{"id": "gid://shopify/Product/1", "title": "Shirt", "updatedAt": "2026-08-23T10:00:00Z"},
		map[string]any{"id": "gid://shopify/ProductVariant/9", "title": "M", "__parentId": "gid://shopify/Product/1", "updatedAt": "2026-08-23T10:00:00Z"},
		map[string]any{"id": "gid://shopify/Product/2", "title": "Hat", "updatedAt": "2026-08-23T10:00:00Z"},
	))
	// 3. Completed -> the same step remembers the URL and streams it. One
	// step rather than two on purpose: a poll that found a finished
	// operation and then returned without reading it would cost a whole
	// runner tick for nothing.
	h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "COMPLETED", url))
	page, err = h.conn.BackfillStore(ctx, store, "v1:shopify:product", page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Done {
		t.Fatalf("the whole file should have been consumed in one batch: %+v", page)
	}
	if len(page.Writes) != 3 {
		t.Fatalf("got %d writes, want two products and one variant", len(page.Writes))
	}
	// A __parentId line becomes a CHILD of the concept its own GID names --
	// the JSONL interleaves connections, so position identifies nothing.
	var variant, product int
	for _, w := range page.Writes {
		switch w.Concept {
		case "v1:shopify:productVariant":
			variant++
			if w.Payload["parentGid"] != "gid://shopify/Product/1" {
				t.Errorf("variant parent = %q", w.Payload["parentGid"])
			}
		case "v1:shopify:product":
			product++
		}
	}
	if product != 2 || variant != 1 {
		t.Errorf("products=%d variants=%d", product, variant)
	}
}

// The whole point of the cursor: a crash mid-file resumes at the line it
// reached rather than re-applying the prefix or, worse, restarting.
func TestBackfillResumesFromTheLineOffset(t *testing.T) {
	h := newHarness(t)
	store := h.store(t)
	ctx := context.Background()

	var lines []map[string]any
	for i := 1; i <= 5; i++ {
		lines = append(lines, map[string]any{
			"id": "gid://shopify/Product/" + string(rune('0'+i)), "title": "P", "updatedAt": "2026-08-23T10:00:00Z",
		})
	}
	url := h.admin.serveJSONL("/download/five.jsonl", bulkLines(lines...))

	cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", URL: url, Line: 3}
	page, err := h.conn.BackfillStore(ctx, store, "v1:shopify:product", cur.encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Writes) != 2 {
		t.Fatalf("resumed with %d writes, want the two lines after the offset", len(page.Writes))
	}
	if !page.Done {
		t.Error("the file ended, so the step must report done")
	}
}

// A signed URL is valid for one week. Past that the download 403s, and the
// only recovery is a fresh operation -- said as an error the runner turns
// into a restart rather than as a silent zero.
func TestAnExpiredDownloadIsAnErrorNotAnEmptyPage(t *testing.T) {
	h := newHarness(t)
	cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", URL: h.admin.server.URL + "/download/gone.jsonl"}
	_, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur.encode())
	if err == nil || !strings.Contains(err.Error(), "one week") {
		t.Fatalf("err = %v, want an expiry explanation", err)
	}
}

// A bulk operation must finish within ten days and can be cancelled or fail.
// Restarting is the only recovery, and it is safe because every write is
// idempotent on the row id.
func TestAnExpiredOperationRestarts(t *testing.T) {
	for _, status := range []string{"EXPIRED", "CANCELED", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			h := newHarness(t)
			h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", status, ""))
			cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1"}
			page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur.encode())
			if err != nil {
				t.Fatal(err)
			}
			if page.Done {
				t.Fatal("a failed operation must not report the domain done")
			}
			if decodeBackfillCursor(page.NextCursor).OperationID != "" {
				t.Error("the cursor still names the dead operation, so the restart never happens")
			}
		})
	}
}

// A completed operation with no URL means the query matched nothing. That is
// a legitimately empty domain and the step is DONE, not stuck.
func TestAnEmptyDomainCompletes(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "COMPLETED", ""))
	cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1"}
	page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur.encode())
	if err != nil {
		t.Fatal(err)
	}
	if !page.Done {
		t.Fatalf("page = %+v, want done", page)
	}
}

func TestADomainWithNoBulkQuerySaysSo(t *testing.T) {
	h := newHarness(t)
	page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:orderLineItem", "")
	if err != nil {
		t.Fatal(err)
	}
	if !page.Done || len(page.Writes) != 0 {
		t.Fatalf("page = %+v, want a done page with no writes -- the domain is filled by its parent", page)
	}
}

// bulkOperationRunQuery takes a query STRING and Shopify refuses a document
// carrying more than one operation, so a split domain has to hand over
// exactly the part being run.
func TestOperationBodyExtractsOneOperation(t *testing.T) {
	document := "query A($query: String) {\n  a { b }\n}\n\nquery B($query: String) {\n  c { d }\n}\n"
	body, err := operationBody(document, "B")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "query A") {
		t.Errorf("extracted both operations:\n%s", body)
	}
	if !strings.Contains(body, "c { d }") {
		t.Errorf("extracted the wrong body:\n%s", body)
	}
	if _, err := operationBody(document, "Missing"); err == nil {
		t.Error("a missing operation must be an error, not an empty query")
	}
}

// The runner orders domains parents-first: a child row carries parentGid, and
// a backfill that wrote line items before their order would leave a window in
// which the mirror answers "this line belongs to nothing".
func TestApplyOrderPutsParentsFirst(t *testing.T) {
	position := map[string]int{}
	for i, concept := range generated.ApplyOrder {
		position[concept] = i
	}
	if len(position) != len(generated.Types) {
		t.Fatalf("ApplyOrder covers %d of %d concepts", len(position), len(generated.Types))
	}
	for concept, spec := range generated.Types {
		if spec.Parent == "" {
			continue
		}
		if position[spec.Parent] > position[concept] {
			t.Errorf("%s is applied before its parent %s", concept, spec.Parent)
		}
	}
}

// The harness disables the bulk-download guard so its plaintext loopback
// server is reachable. That seam is also the one way streamBulk could stop
// consulting the guard without a test noticing -- so this removes it and
// checks the refusal comes back, against the same loopback URL every other
// backfill test streams happily.
func TestTheBulkDownloadGuardIsConsulted(t *testing.T) {
	h := newHarness(t)
	h.conn.admin.downloadGuard = nil
	store := h.store(t)
	ctx := context.Background()

	h.admin.reply("ShopifyBulkRun", startedOperation("gid://shopify/BulkOperation/1"))
	page, err := h.conn.BackfillStore(ctx, store, "v1:shopify:product", "")
	if err != nil {
		t.Fatal(err)
	}

	url := h.admin.serveJSONL("/download/products.jsonl", bulkLines(
		map[string]any{"id": "gid://shopify/Product/1", "title": "Shirt", "updatedAt": "2026-08-23T10:00:00Z"},
	))
	h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "COMPLETED", url))
	_, err = h.conn.BackfillStore(ctx, store, "v1:shopify:product", page.NextCursor)
	if err == nil {
		t.Fatal("the download was fetched with no guard in front of it")
	}
	if !strings.Contains(err.Error(), "must be https") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}
