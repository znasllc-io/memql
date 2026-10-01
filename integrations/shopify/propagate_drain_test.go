package shopify

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/datasync"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// The actual drain deliberately omits Payload. The old Propagate-only tests
// supplied it themselves, so they passed while every queued row dead-lettered
// with an empty store id. Keep the real drain and connector joined here.
// outboxPending has a shape, unlike the bare mirror reads the shared fake
// engine models. Give the drain that same flat result shape in this fixture.
type fakeOutboxEngine struct{ engine *fakeEngine }

func (a fakeOutboxEngine) Execute(ctx context.Context, query string) (any, error) {
	res, err := a.engine.Execute(ctx, query)
	rows := memql.MaterializeRows(res)
	for i, row := range rows {
		if payload, ok := row["payload"].(map[string]any); ok {
			payload["id"] = row["id"]
			rows[i] = payload
		}
	}
	return rows, err
}

func bindPropagation(t *testing.T, c *Connector) {
	t.Helper()
	if err := memqlsync.Bind(c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(c.Name()) })
}

func TestOutboxDrainReadsCurrentShopifySourceOnEveryAttempt(t *testing.T) {
	h := newHarness(t)
	okDefinitions(h)
	contentEntry(h, map[string]any{"summary": "Before retry"})
	h.engine.setRows("outboxPending", []map[string]any{{
		"id": "obx1", "conceptId": "v1:commerce:productContent", "rowRef": "pc1",
		"target": ConnectorName, "action": "upsert", "status": "pending",
		"attempts": 0, "idempotencyKey": "pc1-first-version",
	}})
	bindPropagation(t, h.conn)
	w := datasync.NewWorker(fakeOutboxEngine{h.engine}, nil, h.conn.logger)
	h.conn.admin.MaxRetries = 0
	h.admin.httpStatus("ShopifyMetafieldsSet", http.StatusBadGateway)
	w.DrainOnce(context.Background())
	if delivered, failed, dead := w.Counters(); delivered != 0 || failed != 1 || dead != 0 {
		t.Fatalf("first attempt = %d/%d/%d, want retry, not a missing-payload dead letter", delivered, failed, dead)
	}

	// Edit the source before the same queued entry is retried. A cached source
	// row or an append-time snapshot would send the first value again.
	contentEntry(h, map[string]any{"summary": "Edited while delivery was unavailable"})
	h.admin.httpStatus("ShopifyMetafieldsSet", http.StatusOK)
	h.admin.reply("ShopifyMetafieldsSet", map[string]any{
		"metafieldsSet": map[string]any{"userErrors": []any{}},
	})
	w.DrainOnce(context.Background())
	if delivered, failed, dead := w.Counters(); delivered != 1 || failed != 1 || dead != 0 {
		t.Fatalf("retry = %d/%d/%d, want delivered", delivered, failed, dead)
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
	if len(h.engine.callsTo("markOutboxDelivered")) != 1 || len(h.engine.callsTo("productContentForPropagation")) != 2 {
		t.Fatalf("unexpected source/delivery calls: %v", h.engine.calls())
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
