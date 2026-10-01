package shopify

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// Retry the same payloadless entry after the authoritative source changes.
// The actual queue-to-connector composition lives in component/datasync:
// importing that root component here would reverse the module dependency.
func TestPropagationReadsCurrentSourceOnEveryAttempt(t *testing.T) {
	h := newHarness(t)
	okDefinitions(h)
	entry := contentEntry(h, map[string]any{"summary": "Before retry"})
	h.conn.admin.MaxRetries = 0
	h.admin.httpStatus("ShopifyMetafieldsSet", http.StatusBadGateway)
	if _, err := h.conn.Propagate(context.Background(), entry); err == nil || memqlsync.IsPermanent(err) {
		t.Fatalf("first attempt must be retryable: %v", err)
	}
	contentEntry(h, map[string]any{"summary": "Edited while delivery was unavailable"})
	h.admin.httpStatus("ShopifyMetafieldsSet", http.StatusOK)
	h.admin.reply("ShopifyMetafieldsSet", map[string]any{"metafieldsSet": map[string]any{"userErrors": []any{}}})
	if _, err := h.conn.Propagate(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	var values []string
	for _, r := range h.admin.seen() {
		if r.Operation == "ShopifyMetafieldsSet" {
			values = append(values, renderValue(r.Variables["metafields"]))
		}
	}
	if len(values) != 2 || !strings.Contains(values[0], "Before retry") ||
		!strings.Contains(values[1], "Edited while delivery was unavailable") || strings.Contains(values[1], "Before retry") {
		t.Fatalf("retry did not read the current source: %v", values)
	}
	if len(h.engine.callsTo("productContentForPropagation")) != 2 {
		t.Fatalf("unexpected source calls: %v", h.engine.calls())
	}
}

func TestPropagationIgnoresUntrustedOrStalePayload(t *testing.T) {
	h := newHarness(t)
	okDefinitions(h)
	h.admin.reply("ShopifyMetafieldsSet", map[string]any{"metafieldsSet": map[string]any{"userErrors": []any{}}})
	entry := contentEntry(h, map[string]any{"summary": "Current authorized copy"})
	entry.Payload = map[string]any{"storeId": "another-store", "productGid": "another-product", "summary": "Stale copy"}
	if _, err := h.conn.Propagate(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	for _, r := range h.admin.seen() {
		if r.Operation == "ShopifyMetafieldsSet" {
			body := renderValue(r.Variables)
			if !strings.Contains(body, "Current authorized copy") || strings.Contains(body, "another-") || strings.Contains(body, "Stale copy") {
				t.Fatalf("entry payload influenced the delivery: %s", body)
			}
			return
		}
	}
	t.Fatal("no metafieldsSet call")
}

func TestPropagationSourceFailuresNeverCallShopify(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing or denied row", true: "temporary read failure"}[unavailable], func(t *testing.T) {
			h := newHarness(t)
			entry := contentEntry(h, map[string]any{"summary": "Unreachable"})
			h.engine.setRows("productContentForPropagation", nil)
			if unavailable {
				h.engine.fail["productContentForPropagation"] = errors.New("database temporarily unavailable")
			}
			_, err := h.conn.Propagate(context.Background(), entry)
			if err == nil || memqlsync.IsPermanent(err) == unavailable {
				t.Fatalf("wrong failure classification: %v", err)
			}
			if len(h.admin.seen()) != 0 {
				t.Fatal("Shopify was called without an admitted source row")
			}
		})
	}
}
