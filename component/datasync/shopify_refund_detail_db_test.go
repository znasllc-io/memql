package datasync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// shopify_refund_detail_db_test.go -- a refund is mirrored with its lines, its
// shipping lines and its adjustments, and a bulk line never takes them away:
// a real engine over a real Postgres, the real connector and the runtime's own
// writer, with Shopify scripted at the transport (holdRig).
//
// Here rather than in the connector's package because the integrations module
// cannot import the runtime (shopify_partitions_db_test.go says why). The
// detail arrives through a fetch or a sweep, which select it; a bulk export
// does not, and its line leaves the three keys out of the write, so the
// engine's read-merge keeps what a fetch or a sweep wrote.

// refundMoney is a money bag in one currency, as the Admin API answers one.
func refundMoney(amount string) map[string]any {
	m := map[string]any{"amount": amount, "currencyCode": "EUR"}
	return map[string]any{"shopMoney": m, "presentmentMoney": m}
}

func refundNodes(nodes ...map[string]any) map[string]any {
	list := make([]any, 0, len(nodes))
	for _, n := range nodes {
		list = append(list, n)
	}
	return map[string]any{"nodes": list}
}

// refundStore is a store's three refunds of one order: one of a line, one of
// the shipping, and one of an amount with no line.
type refundStore struct {
	order, line, lineRefund, shippingRefund, amountRefund string
}

func newRefundStore(n int) refundStore {
	return refundStore{
		order:          fmt.Sprintf("gid://shopify/Order/%d", 21000+n),
		line:           fmt.Sprintf("gid://shopify/LineItem/%d", 21000+n),
		lineRefund:     fmt.Sprintf("gid://shopify/Refund/%d1", 21000+n),
		shippingRefund: fmt.Sprintf("gid://shopify/Refund/%d2", 21000+n),
		amountRefund:   fmt.Sprintf("gid://shopify/Refund/%d3", 21000+n),
	}
}

// refunds is the order's refunds. With detail they are as a fetch or a sweep
// reads them: each with its version and its three inlined connections.
// Without, as a bulk line carries them: no inlined connection, and no version
// of their own (the scripted origin answers a nested version only where a
// document selects it at that path, and a refund in a bulk line takes its
// order's).
func (s refundStore) refunds(updatedAt string, detail bool) []any {
	refund := func(gid, total string, lines, shipping, adjustments []map[string]any) map[string]any {
		r := map[string]any{"id": gid, "note": "", "totalRefundedSet": refundMoney(total), "order": map[string]any{"id": s.order}}
		if detail {
			r["updatedAt"] = updatedAt
			r["refundLineItems"] = refundNodes(lines...)
			r["refundShippingLines"] = refundNodes(shipping...)
			r["orderAdjustments"] = refundNodes(adjustments...)
		}
		return r
	}
	return []any{
		refund(s.lineRefund, "36.00", []map[string]any{{
			"lineItem": map[string]any{"id": s.line}, "quantity": 1,
			"subtotalSet": refundMoney("36.00"), "totalTaxSet": refundMoney("0.00"),
		}}, nil, nil),
		refund(s.shippingRefund, "5.00", nil, []map[string]any{{
			"subtotalAmountSet": refundMoney("5.00"), "taxAmountSet": refundMoney("0.00"),
		}}, nil),
		refund(s.amountRefund, "10.00", nil, nil, []map[string]any{{
			"amountSet": refundMoney("10.00"), "taxAmountSet": refundMoney("0.00"), "reason": "REFUND_DISCREPANCY",
		}}),
	}
}

// node is the order as a fetch or a sweep reads it.
func (s refundStore) node(updatedAt string) map[string]any {
	return map[string]any{
		"id": s.order, "updatedAt": updatedAt, "name": "#1021",
		"lineItems": map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}, "nodes": []any{}},
		"refunds":   s.refunds(updatedAt, true),
	}
}

// bulkLine is the order's line in a bulk export: its refunds, a plain list of
// the order, carry no inlined connection, because the bulk document selects
// none.
func (s refundStore) bulkLine(updatedAt string) map[string]any {
	return map[string]any{"id": s.order, "updatedAt": updatedAt, "name": "#1021", "refunds": s.refunds(updatedAt, false)}
}

// sweepOrders runs a store's first sweep of orders (no watermark) at `at`, which
// lists every order.
func (r *holdRig) sweepOrders(s refundStore, updatedAt string, at time.Time) {
	r.t.Helper()
	r.clock = func() time.Time { return at }
	r.origin.reply("ShopifyListOrder", originListPage("orders", false, "", s.node(updatedAt)))
	if _, err := r.conn.ReconcilePartition(context.Background(), r.storeID, generated.ConceptID("order"), time.Time{}); err != nil {
		r.t.Fatalf("the sweep of orders: %v", err)
	}
}

// backfillOrders queues and runs a backfill of orders at `at`, whose export
// holds the order's bulk line; n names its operation and its file.
func (r *holdRig) backfillOrders(s refundStore, updatedAt string, at time.Time, n int) {
	r.t.Helper()
	r.clock = func() time.Time { return at }
	op := fmt.Sprintf("gid://shopify/BulkOperation/21%d", n)
	link := r.origin.serveJSONL(fmt.Sprintf("/download/orders-%d.jsonl", n), originBulkLines(s.bulkLine(updatedAt)))
	r.origin.reply("ShopifyBulkRun", originStartedOperation(op))
	r.origin.reply("ShopifyBulkStatus", originOperationStatus(op, "COMPLETED", link))
	if err := r.state.PatchSyncState(OperatorContext(context.Background()), generated.ConceptID("order"), shopify.ConnectorName, string(memqlsync.DirectionInbound), r.storeID,
		func(SyncState) (SyncPatch, bool) {
			return SyncPatch{"backfillStatus": "queued", "backfillCursor": ""}, true
		}); err != nil {
		r.t.Fatalf("queue the backfill of orders: %v", err)
	}
	r.backfillToItsEnd("order")
}

// refundRow is the mirror's newest version of a refund, or nil.
func (r *holdRig) refundRow(gid string) map[string]any {
	r.t.Helper()
	var raw []byte
	err := r.db.QueryRowContext(context.Background(),
		`SELECT payload FROM "MemoryNodes" WHERE concept = $1 AND payload->>'gid' = $2 AND payload->>'storeId' = $3 ORDER BY "createdAt" DESC LIMIT 1`,
		generated.ConceptID("refund"), gid, r.storeID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		r.t.Fatalf("read %s: %v", gid, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// detailText is a value inside a stored refund as text, "" when absent.
func detailText(v any, path ...any) string {
	for _, step := range path {
		switch k := step.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			list, _ := v.([]any)
			if k >= len(list) {
				return ""
			}
			v = list[k]
		}
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// listLen is the length of a stored list, -1 when the key is absent or not a
// list.
func listLen(row map[string]any, key string) int {
	list, ok := row[key].([]any)
	if !ok {
		return -1
	}
	return len(list)
}

// keysAnywhere is every key of a stored value, at any depth.
func keysAnywhere(v any, into map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, item := range x {
			into[k] = true
			keysAnywhere(item, into)
		}
	case []any:
		for _, item := range x {
			keysAnywhere(item, into)
		}
	}
}

// requireDetail holds a store's three refund rows to what the order's refunds
// were: the refunded line, the refunded shipping, and the adjustment of an
// amount with no line.
func (r *holdRig) requireDetail(s refundStore) {
	r.t.Helper()
	line, shipping, amount := r.refundRow(s.lineRefund), r.refundRow(s.shippingRefund), r.refundRow(s.amountRefund)
	if line == nil || shipping == nil || amount == nil {
		r.t.Fatalf("the refunds are not all mirrored: line %v, shipping %v, amount %v", line, shipping, amount)
	}
	if listLen(line, "refundLineItems") != 1 || detailText(line, "refundLineItems", 0, "quantity") != "1" ||
		detailText(line, "refundLineItems", 0, "subtotalSet", "shopMoney", "amount") != "36.00" ||
		detailText(line, "refundLineItems", 0, "lineItem", "id") != s.line {
		r.t.Errorf("the line refund holds %v, want one line of 1 unit at 36.00 naming %s", line["refundLineItems"], s.line)
	}
	if listLen(line, "refundShippingLines") != 0 || listLen(line, "orderAdjustments") != 0 {
		r.t.Errorf("the line refund holds shipping %v and adjustments %v, want both read and empty", line["refundShippingLines"], line["orderAdjustments"])
	}
	if listLen(shipping, "refundShippingLines") != 1 || detailText(shipping, "refundShippingLines", 0, "subtotalAmountSet", "shopMoney", "amount") != "5.00" {
		r.t.Errorf("the shipping refund holds %v, want the shipping at 5.00", shipping["refundShippingLines"])
	}
	if listLen(shipping, "refundLineItems") != 0 {
		r.t.Errorf("the shipping refund holds lines %v, want none", shipping["refundLineItems"])
	}
	if listLen(amount, "orderAdjustments") != 1 || detailText(amount, "orderAdjustments", 0, "amountSet", "shopMoney", "amount") != "10.00" ||
		detailText(amount, "orderAdjustments", 0, "reason") != "REFUND_DISCREPANCY" {
		r.t.Errorf("the amount-only refund holds %v, want one adjustment of 10.00", amount["orderAdjustments"])
	}
	if listLen(amount, "refundLineItems") != 0 || listLen(amount, "refundShippingLines") != 0 {
		r.t.Errorf("the amount-only refund holds lines %v and shipping %v, want both read and empty", amount["refundLineItems"], amount["refundShippingLines"])
	}
}

// A REFUND ROW HOLDS ITS LINES, ITS SHIPPING AND ITS ADJUSTMENTS, and nothing
// a person typed: no refund row carries a shipping line, or the phone number
// at the delivery address that a shipping line holds.
func TestARefundRowHoldsItsLinesItsShippingAndItsAdjustments(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	s := newRefundStore(1)
	r.sweepOrders(s, "2026-11-10T09:00:00Z", r.at)
	r.requireDetail(s)
	for _, gid := range []string{s.lineRefund, s.shippingRefund, s.amountRefund} {
		keys := map[string]bool{}
		keysAnywhere(r.refundRow(gid), keys)
		for _, never := range []string{"shippingLine", "phone"} {
			if keys[never] {
				t.Errorf("refund %s carries a key %q", gid, never)
			}
		}
	}
}

// O3: the detail arrives whichever of the backfill and a store's first sweep
// runs first. The backfill's bulk line carries none of it; the sweep, which
// lists every order on a store's first pass, carries all of it.
func TestRefundDetailArrivesWhicheverOfTheBackfillAndTheFirstSweepRunsFirst(t *testing.T) {
	const updated = "2026-11-10T09:00:00Z"
	t.Run("the backfill first", func(t *testing.T) {
		r := newHoldRig(t)
		if r == nil {
			return
		}
		s := newRefundStore(2)
		r.backfillOrders(s, updated, r.at, 2)
		row := r.refundRow(s.lineRefund)
		if row == nil {
			t.Fatal("the backfill wrote no refund")
		}
		for _, name := range []string{"refundLineItems", "refundShippingLines", "orderAdjustments"} {
			if _, present := row[name]; present {
				t.Errorf("the backfill wrote %s = %v: a list it did not read", name, row[name])
			}
		}
		r.sweepOrders(s, updated, r.at.Add(time.Minute))
		r.requireDetail(s)
	})
	t.Run("the first sweep first", func(t *testing.T) {
		r := newHoldRig(t)
		if r == nil {
			return
		}
		s := newRefundStore(3)
		r.sweepOrders(s, updated, r.at)
		r.backfillOrders(s, updated, r.at.Add(time.Minute), 3)
		r.requireDetail(s)
	})
}

// A BULK LINE DOES NOT ERASE DETAIL A FETCH RECORDED. The export is read after
// the fetch and carries the same version, so its write is applied -- the
// refund's read time moves to the export's -- and the three lists stay.
func TestABulkLineDoesNotEraseDetailAFetchRecorded(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	const updated = "2026-11-10T09:00:00Z"
	s := newRefundStore(4)
	r.clock = func() time.Time { return r.at }
	r.origin.reply("ShopifyFetchOrder", map[string]any{"node": func() map[string]any {
		n := s.node(updated)
		n["__typename"] = "Order"
		return n
	}()})
	if res := r.dispatch(r.stage("fetched", "orders/updated", s.order)); res.Applied == 0 {
		t.Fatalf("the fetch of the order applied nothing: %+v", res)
	}
	r.requireDetail(s)
	fetched := detailText(r.refundRow(s.lineRefund), "syncedAt")

	later := r.at.Add(time.Hour)
	r.backfillOrders(s, updated, later, 4)
	row := r.refundRow(s.lineRefund)
	if got := detailText(row, "syncedAt"); got == fetched {
		t.Fatalf("the export's write of the refund was not applied (syncedAt still %s), so this test proves nothing", got)
	}
	r.requireDetail(s)
}
