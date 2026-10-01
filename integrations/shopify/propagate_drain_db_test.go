package shopify

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/datasync"
	"github.com/znasllc-io/memql/component/memql"
)

// Other packages share CI's database. Read through the real outbox query but
// hand the worker only this test's entries, so it cannot deliver another
// package's fixtures through our fake Shopify endpoint.
type scopedPropagationQueue struct {
	engine memql.IntegrationEngineAccess
	rowID  string
}

func (q scopedPropagationQueue) Execute(ctx context.Context, query string) (any, error) {
	res, err := q.engine.Execute(ctx, query)
	if err != nil || callName(query) != "outboxPending" {
		return res, err
	}
	var rows []map[string]any
	for _, row := range memql.MaterializeRows(res) {
		if memql.BareShortId(mapString(row, "rowRef")) == q.rowID {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// The fake-engine drain regression cannot enforce row admission or parse the
// DSL reads. This one writes an actual productContent row, drains its actual
// transactional outbox, and asserts the external request and stored outcome.
func TestProductContentDrainsToShopifyWithSourceAuthority(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	rowID := fmt.Sprintf("dbtest-content-drain-%d", time.Now().UnixNano())
	canonicalID := "v1:commerce:productContent:" + rowID
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:platform:outboxEntry' AND payload->>'rowRef' = $1`, canonicalID)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:commerce:productContent' AND id = $1`, canonicalID)
	})
	args := map[string]any{
		"contentId": rowID, "storeId": testStoreID, "productGid": "gid://shopify/Product/1",
		"summary": "Earlier copy before an edit", "description": "A product description",
		"keywords": []any{"dental care"}, "blocks": map[string]any{"tagline": "Source authority"},
	}
	if _, err := eng.Execute(ownerCtx(), "mutation "+renderCall("upsertProductContent", args)); err != nil {
		t.Fatalf("write product content and queue its delivery: %v", err)
	}

	read := "query " + renderCall("productContentForPropagation", map[string]any{"rowId": rowID})
	res, err := eng.Execute(connectorContext(context.Background()), read)
	if err != nil || len(memql.MaterializeRows(res)) != 1 {
		t.Fatalf("named Shopify connector cannot read its source: %v, rows=%v", err, memql.MaterializeRows(res))
	}
	// Internal origin is not broad row authority: another connector still
	// cannot see a concept whose mirroredTo names Shopify alone.
	other := auth.ContextWithInternalOrigin(auth.ContextWithConnectorActor(context.Background(), "quickBooks"))
	res, err = eng.Execute(other, read)
	if err == nil && len(memql.MaterializeRows(res)) != 0 {
		t.Fatal("another connector could read Shopify's source")
	}
	if _, err := eng.Execute(ownerCtx(), read); err == nil {
		t.Fatal("the server-only propagation read was exposed to a browser caller")
	}
	// The first query primed the normal read cache. Both queued versions must
	// now deliver this edit, not the first row or a cached projection of it.
	args["summary"] = "Current copy from the real source"
	if _, err := eng.Execute(ownerCtx(), "mutation "+renderCall("upsertProductContent", args)); err != nil {
		t.Fatalf("edit content before delivery: %v", err)
	}

	h := newHarness(t)
	h.conn.engine = eng
	okDefinitions(h)
	h.admin.reply("ShopifyMetafieldsSet", map[string]any{"metafieldsSet": map[string]any{
		"metafields": []any{map[string]any{"id": "gid://shopify/Metafield/1"}}, "userErrors": []any{},
	}})
	bindPropagation(t, h.conn)
	worker := datasync.NewWorker(scopedPropagationQueue{engine: eng, rowID: rowID}, nil, h.conn.logger)
	worker.DrainOnce(context.Background())
	var contentDelivered bool
	for _, request := range h.admin.seen() {
		if request.Operation == "ShopifyMetafieldsSet" {
			body := renderValue(request.Variables)
			if strings.Contains(body, "Earlier copy before an edit") {
				t.Fatal("an old queued version delivered stale source copy")
			}
			contentDelivered = strings.Contains(body, "Current copy from the real source")
		}
	}
	if !contentDelivered {
		t.Fatalf("real outbox never delivered the source row: operations=%v", h.admin.seenOps())
	}
	// Query the actual persisted outcome under the operator authority. A
	// mocked markOutboxDelivered call would miss a refused status write.
	var delivered int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:platform:outboxEntry' AND payload->>'rowRef' = $1 AND payload->>'status' = 'delivered'`, canonicalID).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != 2 {
		t.Fatalf("persisted delivered count = %d, want both queued versions delivered", delivered)
	}
}
