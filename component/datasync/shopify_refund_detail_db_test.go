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

// A REFUND ROW HOLDS ITS LINES, ITS SHIPPING AND ITS ADJUSTMENTS. That a
// refund's shipping line carries nothing a person typed (not the order's
// shipping line, not the phone at the delivery address) is held by the
// selection the generator emits, not by the mapper, which stores a node whole:
// cmd/shopifyschema's TestAnInlinedConnectionCarriesOnlyTheFieldsItNames and
// the live TestLiveARefundIsMirroredWithItsLinesAndItsShipping hold it.
func TestARefundRowHoldsItsLinesItsShippingAndItsAdjustments(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	s := newRefundStore(1)
	r.sweepOrders(s, "2026-11-10T09:00:00Z", r.at)
	r.requireDetail(s)
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

// A REFUND MIRRORED BEFORE ITS DETAIL GETS IT AT THE NEXT SWEEP. The backfill's
// bulk line wrote the refunds without their lists, and the store has a
// watermark, so its ordinary sweep asks only for what changed since, and the
// order did not change. The store's first order sweep under this version reads
// every order once more all the same, and the one after it is the ordinary
// sweep again.
func TestARefundMirroredBeforeItsDetailGetsItAtTheNextSweep(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	const updated = "2026-11-10T09:00:00Z"
	s := newRefundStore(5)
	r.backfillOrders(s, updated, r.at, 5)
	row := r.refundRow(s.lineRefund)
	if row == nil {
		t.Fatal("the backfill wrote no refund")
	}
	if _, present := row["refundLineItems"]; present {
		t.Fatalf("the backfill wrote the refund's lines %v, so this test proves nothing", row["refundLineItems"])
	}

	// Shopify answers what changed since the watermark with nothing, and the
	// whole listing with the order.
	r.origin.replyWith("ShopifyListOrder", func(vars map[string]any) any {
		if q, _ := vars["query"].(string); q != "" {
			return originListPage("orders", false, "")
		}
		return originListPage("orders", false, "", s.node(updated))
	})
	sweep := func(at, since time.Time) {
		t.Helper()
		r.clock = func() time.Time { return at }
		if _, err := r.conn.ReconcilePartition(context.Background(), r.storeID, generated.ConceptID("order"), since); err != nil {
			t.Fatalf("the sweep of orders at %s: %v", at, err)
		}
	}
	read := r.at.Add(time.Minute)
	sweep(read, r.at)
	r.requireDetail(s)
	if got := r.store().Health["refundDetailFrom"]; got != read.UTC().Format(time.RFC3339) {
		t.Errorf("the store's health refundDetailFrom = %v, want when the read began, %s", got, read.UTC().Format(time.RFC3339))
	}

	next := r.at.Add(time.Hour)
	sweep(next, read)
	var lists []originRequest
	for _, req := range r.origin.seen() {
		if req.Operation == "ShopifyListOrder" {
			lists = append(lists, req)
		}
	}
	if len(lists) != 3 {
		t.Fatalf("the two sweeps listed the orders %d times, want the check and the read, then the check alone", len(lists))
	}
	if q, want := lists[0].Variables["query"], "updated_at:>'"+r.at.UTC().Format(time.RFC3339)+"'"; q != want {
		t.Errorf("the first sweep's check was sent %v, want only what changed, %q", q, want)
	}
	if q, present := lists[1].Variables["query"]; present {
		t.Errorf("the read was sent the filter %v, want none", q)
	}
	if q, want := lists[2].Variables["query"], "updated_at:>'"+read.UTC().Format(time.RFC3339)+"'"; q != want {
		t.Errorf("the sweep after the read was sent %v, want only what changed, %q", q, want)
	}
	r.requireDetail(s)
}

// listOrders answers the order listing: what changed since a watermark with
// nothing, and the whole listing with these orders.
func (r *holdRig) listOrders(updatedAt string, stores ...refundStore) {
	r.origin.replyWith("ShopifyListOrder", func(vars map[string]any) any {
		if q, _ := vars["query"].(string); q != "" {
			return originListPage("orders", false, "")
		}
		nodes := make([]map[string]any, 0, len(stores))
		for _, s := range stores {
			nodes = append(nodes, s.node(updatedAt))
		}
		return originListPage("orders", false, "", nodes...)
	})
}

// sweepFrom runs a sweep of orders at `at` from the watermark since.
func (r *holdRig) sweepFrom(at, since time.Time) {
	r.t.Helper()
	r.clock = func() time.Time { return at }
	if _, err := r.conn.ReconcilePartition(context.Background(), r.storeID, generated.ConceptID("order"), since); err != nil {
		r.t.Fatalf("the sweep of orders at %s: %v", at, err)
	}
}

// rowVersions is how many versions the mirror holds of a row of a concept.
func (r *holdRig) rowVersions(concept, gid string) int {
	r.t.Helper()
	var n int
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM "MemoryNodes" WHERE concept = $1 AND payload->>'gid' = $2 AND payload->>'storeId' = $3`,
		generated.ConceptID(concept), gid, r.storeID).Scan(&n); err != nil {
		r.t.Fatalf("count the versions of %s: %v", gid, err)
	}
	return n
}

// A ROW THE READ FINDS UNCHANGED IS NOT WRITTEN AGAIN. The order and its
// refunds were recorded by the order's own fetch, with their detail; the read
// lists the same order, and the real write path skips every row of it.
func TestARowTheReadFindsUnchangedIsNotWrittenAgain(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	const updated = "2026-11-10T09:00:00Z"
	s := newRefundStore(6)
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
	gids := map[string]string{s.order: "order", s.lineRefund: "refund", s.shippingRefund: "refund", s.amountRefund: "refund"}
	before := map[string]int{}
	for gid, concept := range gids {
		before[gid] = r.rowVersions(concept, gid)
	}

	r.listOrders(updated, s)
	r.sweepFrom(r.at.Add(time.Minute), r.at)
	if got := r.store().Health["refundDetailFrom"]; got == nil {
		t.Fatal("the read did not end, so this test proves nothing")
	}
	for gid, concept := range gids {
		if got := r.rowVersions(concept, gid); got != before[gid] {
			t.Errorf("%s %s has %d versions after the read, want the %d it had: the read wrote a row it found unchanged", concept, gid, got, before[gid])
		}
	}
}

// RULING B: THE STAMP DOES NOT OUTLIVE THE ROWS IT DESCRIBES. A store whose
// read ended has its orders backfilled again (a purge and a reconnect, or an
// operator's backfill), and the export writes an order the mirror did not
// hold, with no refund detail. The next sweep reads every order once more.
func TestAStampedStoreWhoseOrderBackfillCompletesLaterGetsTheReadAgain(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	const updated = "2026-11-10T09:00:00Z"
	r.listOrders(updated)
	r.sweepFrom(r.at, r.at.Add(-time.Hour))
	first, _ := r.store().Health["refundDetailFrom"].(string)
	if first == "" {
		t.Fatal("the store's first read was not stamped")
	}

	s := newRefundStore(7)
	r.backfillOrders(s, updated, r.at.Add(time.Hour), 7)
	if row := r.refundRow(s.lineRefund); row == nil || row["refundLineItems"] != nil {
		t.Fatalf("the backfill's refund = %v, want one without its lines", row)
	}

	r.listOrders(updated, s)
	later := r.at.Add(2 * time.Hour)
	r.sweepFrom(later, r.at)
	r.requireDetail(s)
	if got := r.store().Health["refundDetailFrom"]; got != later.UTC().Format(time.RFC3339) {
		t.Errorf("the store's health refundDetailFrom = %v, want the new read's start %s (the first was %s)", got, later.UTC().Format(time.RFC3339), first)
	}
}
