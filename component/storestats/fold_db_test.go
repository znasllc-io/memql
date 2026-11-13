package storestats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// fold_db_test.go -- the fold and the derivation (fold.go, derive.go): a page
// of walked rows written as facts, and each touched order's derived amounts,
// against the real database; and a whole run of the rollup, through the real
// engine, for what only a run shows. Postgres-gated, through the harness.

// ---- fixtures ----

// foldBase is the instant the folds of this file write their versions from.
var foldBase = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

// at0 is version n of a row: foldBase plus n minutes.
func ver(n int) time.Time { return foldBase.Add(time.Duration(n) * time.Minute) }

func stampOf(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

const (
	theOrder  = "gid://shopify/Order/1001"
	lineOne   = "gid://shopify/LineItem/501"
	lineTwo   = "gid://shopify/LineItem/502"
	lineGift  = "gid://shopify/LineItem/503"
	theRefund = "gid://shopify/Refund/700"
)

// withFields sets the row's identity and version and then each key/value
// pair over it.
func withFields(row map[string]any, storeID, gid string, version time.Time, kv []any) map[string]any {
	row["storeId"], row["gid"], row["createdAt"] = storeID, gid, stampOf(version)
	// The row's own id, as the mirror makes it: one per row.
	row["id"] = "v1:shopify:row:" + shopify.MirrorRowID(storeID, gid)
	for n := 0; n+1 < len(kv); n += 2 {
		row[kv[n].(string)] = kv[n+1]
	}
	return row
}

func orderAt(storeID, gid string, version time.Time, kv ...any) map[string]any {
	return withFields(orderRow(), storeID, gid, version, kv)
}

func lineAt(storeID, gid, orderGID string, version time.Time, kv ...any) map[string]any {
	row := withFields(lineRow(), storeID, gid, version, kv)
	if _, set := kvHas(kv, "parentGid"); !set {
		row["parentGid"] = orderGID
	}
	return row
}

func refundAt(storeID, gid, orderGID string, version time.Time, kv ...any) map[string]any {
	row := withFields(refundRow(), storeID, gid, version, kv)
	if _, set := kvHas(kv, "parentGid"); !set {
		row["parentGid"] = orderGID
	}
	return row
}

func kvHas(kv []any, key string) (any, bool) {
	for n := 0; n+1 < len(kv); n += 2 {
		if kv[n] == key {
			return kv[n+1], true
		}
	}
	return nil, false
}

// tombstone is a retired row carrying only its identity, as the mirror can
// write one.
func tombstone(storeID, gid string, version time.Time, kv ...any) map[string]any {
	return withFields(map[string]any{"deleted": true}, storeID, gid, version, kv)
}

// the three-line order: 2 x 40.00 less 8.00, 1 x 19.99, and a 25.00 gift
// card; subtotalLineItemsQuantity 4.
func threeLines(storeID string, version time.Time) (map[string]any, []map[string]any) {
	order := orderAt(storeID, theOrder, version, "subtotalLineItemsQuantity", float64(4))
	lines := []map[string]any{
		lineAt(storeID, lineOne, theOrder, version, "quantity", float64(2), "currentQuantity", float64(2),
			"originalUnitPriceSet", set("40.00", "EUR"), "discountAllocations", []any{allocation("8.00", "EUR")}),
		lineAt(storeID, lineTwo, theOrder, version, "quantity", float64(1), "currentQuantity", float64(1),
			"originalUnitPriceSet", set("19.99", "EUR"), "discountAllocations", []any{}, "productGid", "gid://shopify/Product/10"),
		lineAt(storeID, lineGift, theOrder, version, "quantity", float64(1), "currentQuantity", float64(1), "isGiftCard", true,
			"originalUnitPriceSet", set("25.00", "EUR"), "discountAllocations", []any{}, "productGid", "gid://shopify/Product/11"),
	}
	return order, lines
}

// foldStore is a store row of the test's own, with no store in the engine:
// what a direct fold needs. Its rows in every store_sales_ table are deleted
// when the test ends.
func foldStore(t *testing.T) string {
	t.Helper()
	_, db := statsEngine(t)
	if db == nil {
		t.FailNow()
	}
	id := fmt.Sprintf("dbtest-storestats-fold-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		for _, table := range statsTableNames() {
			_, _ = db.ExecContext(context.Background(), "DELETE FROM "+table+" WHERE store_id = ?", id)
		}
	})
	if err := writeTx(context.Background(), db, id, anyGeneration, func(ctx context.Context, tx bun.Tx) error {
		return ensureStore(ctx, tx, id)
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// foldRows folds one page of rows of a source into the store, as one walked
// page is folded: read before the transaction, written in one writeTx under
// the store's generation.
func foldRows(t *testing.T, storeID, source string, wholesale wholesaleState, rows ...map[string]any) (pageResult, error) {
	t.Helper()
	_, db := statsEngine(t)
	ctx := context.Background()
	page, err := readPage(storeID, source, rows, wholesale)
	if err != nil {
		return pageResult{}, err
	}
	generation, err := generationOf(ctx, db, storeID)
	if err != nil {
		return pageResult{}, err
	}
	var res pageResult
	err = writeTx(ctx, db, storeID, generation, func(ctx context.Context, tx bun.Tx) error {
		var err error
		res, err = foldPage(ctx, tx, storeID, source, page)
		return err
	})
	return res, err
}

func fold(t *testing.T, storeID, source string, rows ...map[string]any) pageResult {
	t.Helper()
	res, err := foldRows(t, storeID, source, wholesaleState{}, rows...)
	if err != nil {
		t.Fatalf("fold a page of %s: %v", source, err)
	}
	return res
}

// cell is a column as text: a NUMERIC's own digits, a time as Postgres wrote
// it, "null" for null.
func cell(row map[string]any, col string) string {
	switch v := row[col].(type) {
	case nil:
		return "null"
	case json.Number:
		return v.String()
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func instantCell(t *testing.T, row map[string]any, col string) time.Time {
	t.Helper()
	s, ok := row[col].(string)
	if !ok {
		t.Fatalf("%s is %v, not a time", col, row[col])
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("%s: %v", col, err)
	}
	return v.UTC()
}

// want holds each column/value pair of a row, compared as text.
func want(t *testing.T, what string, row map[string]any, kv ...string) {
	t.Helper()
	if row == nil {
		t.Errorf("%s: no fact", what)
		return
	}
	for n := 0; n+1 < len(kv); n += 2 {
		if got := cell(row, kv[n]); got != kv[n+1] {
			t.Errorf("%s: %s = %s, want %s", what, kv[n], got, kv[n+1])
		}
	}
}

func wantVersion(t *testing.T, what string, row map[string]any, col string, v time.Time) {
	t.Helper()
	if row == nil {
		t.Errorf("%s: no fact", what)
		return
	}
	if got := instantCell(t, row, col); !got.Equal(v) {
		t.Errorf("%s: %s = %v, want %v", what, col, got, v)
	}
}

// snapshot is every fact of the store, as text, without checked_at: the
// check marks every fact it compares (check.go), and the check a rebuild's
// end begins runs in an ordinary run.
func snapshot(t *testing.T, storeID string) string {
	t.Helper()
	all := map[string]map[string]map[string]any{
		"order":       orderFactsOf(t, storeID),
		"line":        lineFactsOf(t, storeID),
		"refund":      refundFactsOf(t, storeID),
		"refund_line": refundLineFactsOf(t, storeID),
	}
	for _, table := range all {
		for _, row := range table {
			delete(row, "checked_at")
		}
	}
	raw, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// ---- what a fold writes ----

func TestAnOrderWithThreeLinesWritesItsFacts(t *testing.T) {
	id := foldStore(t)
	order, lines := threeLines(id, ver(0))
	// The lines first and the order after them: the order's page derives it
	// too.
	fold(t, id, sourceLine, lines...)
	res := fold(t, id, sourceOrder, order)
	if res.Facts != 1 || res.Unreadable != 0 || !slices.Equal(res.Orders, []string{theOrder}) {
		t.Errorf("the order's page answered %+v, want one fact touching the order", res)
	}
	o := orderFactsOf(t, id)[theOrder]
	want(t, "the order", o, "gross", "99.99", "discounts", "8.00", "units", "3", "line_count", "3", "lines_complete", "true",
		"deleted", "false", "currency", "EUR", "shipping", "12.50", "tax", "4.20", "duties", "0", "expected_quantity", "4",
		"buyer_key", "gid://shopify/Customer/77", "wholesale", "false", "cancelled", "false", "test", "false", "taxes_included", "false",
		"shopify_current_subtotal", "80.00", "shopify_total_discounts", "8.00", "shopify_total_refunded", "0.00", "checked_at", "null")
	wantVersion(t, "the order", o, "order_version", ver(0))
	wantVersion(t, "the order", o, "lines_version", ver(0))
	wantVersion(t, "the order", o, "ordered_at", mustInstant(t, "2026-10-31T23:30:00Z"))
	wantVersion(t, "the order", o, "recorded_at", mustInstant(t, "2026-11-01T10:00:01Z"))
	l := lineFactsOf(t, id)
	want(t, "line one", l[lineOne], "order_gid", theOrder, "sold_quantity", "2", "gross", "80.00", "discounts", "8.00", "refunded_quantity", "0",
		"unit_price", "40.00", "allocated_discounts", "8.00", "currency", "EUR", "gift_card", "false", "product_key", "gid://shopify/Product/9", "title", "Inner tube")
	want(t, "line two", l[lineTwo], "sold_quantity", "1", "gross", "19.99", "discounts", "0")
	want(t, "the gift card", l[lineGift], "gift_card", "true", "sold_quantity", "1", "gross", "25.00")
}

func TestALineRefundIsWrittenOnItsOwnDayAndTheOrdersDayKeepsItsSale(t *testing.T) {
	id := foldStore(t)
	order, lines := threeLines(id, ver(0))
	lines[0]["currentQuantity"] = float64(1) // the refunded unit is no longer on the order
	fold(t, id, sourceOrder, order)
	fold(t, id, sourceLine, lines...)
	// The refund's page alone: it re-derives its order.
	fold(t, id, sourceRefund, refundAt(id, theRefund, theOrder, ver(1)))
	r := refundFactsOf(t, id)[theRefund]
	want(t, "the refund", r, "item_refunds", "36.00", "money_refunded", "48.56", "shipping_refunded", "5.00", "tax_refunded", "7.56",
		"duties_refunded", "0", "gift_card_refunded", "0", "units_returned", "1", "detail_fetched", "true", "lines_complete", "true", "deleted", "false")
	wantVersion(t, "the refund", r, "refunded_at", mustInstant(t, "2026-11-02T15:00:00Z"))
	wantVersion(t, "the order", orderFactsOf(t, id)[theOrder], "ordered_at", mustInstant(t, "2026-10-31T23:30:00Z"))
	want(t, "line one", lineFactsOf(t, id)[lineOne], "refunded_quantity", "1", "sold_quantity", "2", "gross", "80.00")
	want(t, "the order", orderFactsOf(t, id)[theOrder], "gross", "99.99", "units", "3")
	rl := refundLineFactsOf(t, id)
	want(t, "the refund's line", rl[theRefund+"/0"], "line_gid", lineOne, "order_gid", theOrder, "quantity", "1", "subtotal", "36.00", "tax", "7.56")
	if len(rl) != 1 {
		t.Errorf("the refund has %d line facts, want one", len(rl))
	}
}

func TestAShippingRefundIsNoRefundOfItems(t *testing.T) {
	id := foldStore(t)
	order, lines := threeLines(id, ver(0))
	fold(t, id, sourceOrder, order)
	fold(t, id, sourceLine, lines...)
	fold(t, id, sourceRefund, refundAt(id, theRefund, theOrder, ver(1), "totalRefundedSet", set("5.00", "EUR"), "refundLineItems", []any{}))
	want(t, "the refund", refundFactsOf(t, id)[theRefund], "shipping_refunded", "5.00", "item_refunds", "0.00", "units_returned", "0")
}

func TestAnAmountOnlyRefundCountsItsAmountLessItsTax(t *testing.T) {
	id := foldStore(t)
	order, lines := threeLines(id, ver(0))
	fold(t, id, sourceOrder, order)
	fold(t, id, sourceLine, lines...)
	fold(t, id, sourceRefund, refundAt(id, theRefund, theOrder, ver(1), "totalRefundedSet", set("12.10", "EUR"),
		"refundLineItems", []any{}, "refundShippingLines", []any{},
		"orderAdjustments", []any{map[string]any{"taxAmountSet": set("2.10", "EUR")}}))
	want(t, "the refund", refundFactsOf(t, id)[theRefund], "item_refunds", "10.00", "tax_refunded", "2.10", "units_returned", "0")
}

func TestARefundOfAnOrderWhosePricesIncludeTaxKeepsItsTax(t *testing.T) {
	id := foldStore(t)
	order, lines := threeLines(id, ver(0))
	order["taxesIncluded"] = true
	fold(t, id, sourceLine, lines...)
	fold(t, id, sourceRefund, refundAt(id, theRefund, theOrder, ver(1)))
	// The order arrives last: its page re-derives its refunds with its tax setting.
	fold(t, id, sourceOrder, order)
	want(t, "the refund", refundFactsOf(t, id)[theRefund], "item_refunds", "43.56", "tax_refunded", "7.56")
}

func TestAGiftCardRefundIsNoRefundOfItems(t *testing.T) {
	id := foldStore(t)
	order, lines := threeLines(id, ver(0))
	fold(t, id, sourceOrder, order)
	fold(t, id, sourceLine, lines...)
	fold(t, id, sourceRefund, refundAt(id, theRefund, theOrder, ver(1), "totalRefundedSet", set("25.00", "EUR"),
		"refundLineItems", []any{refundLine(lineGift, float64(1), "25.00", "0.00")}, "refundShippingLines", []any{}))
	want(t, "the refund", refundFactsOf(t, id)[theRefund], "gift_card_refunded", "25.00", "units_returned", "0", "item_refunds", "0.00")
}

func TestAnEditThatRemovesAUnitLowersGrossAndWritesNoRefund(t *testing.T) {
	id := foldStore(t)
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(0), "subtotalLineItemsQuantity", float64(1)))
	fold(t, id, sourceLine, lineAt(id, lineOne, theOrder, ver(0), "quantity", float64(2), "currentQuantity", float64(1),
		"originalUnitPriceSet", set("40.00", "EUR"), "discountAllocations", []any{}))
	want(t, "the line", lineFactsOf(t, id)[lineOne], "sold_quantity", "1", "gross", "40.00", "refunded_quantity", "0")
	want(t, "the order", orderFactsOf(t, id)[theOrder], "gross", "40.00", "units", "1", "lines_complete", "true")
	if n := len(refundFactsOf(t, id)); n != 0 {
		t.Errorf("an edit wrote %d refund facts, want none", n)
	}
}

func TestADiscountWrittenWithOneDecimalIsSharedInCents(t *testing.T) {
	id := foldStore(t)
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(0)))
	fold(t, id, sourceLine, lineAt(id, lineOne, theOrder, ver(0), "quantity", float64(3), "currentQuantity", float64(2),
		"originalUnitPriceSet", set("10.00", "EUR"), "discountAllocations", []any{allocation("1.0", "EUR")}))
	want(t, "the line", lineFactsOf(t, id)[lineOne], "discounts", "0.67", "sold_quantity", "2", "gross", "20.00")
	want(t, "the order", orderFactsOf(t, id)[theOrder], "discounts", "0.67")
}

func TestAnEditThatAddsALineRaisesGross(t *testing.T) {
	id := foldStore(t)
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(0), "subtotalLineItemsQuantity", float64(1)))
	one := lineAt(id, lineOne, theOrder, ver(0), "quantity", float64(1), "currentQuantity", float64(1),
		"originalUnitPriceSet", set("40.00", "EUR"), "discountAllocations", []any{})
	fold(t, id, sourceLine, one)
	want(t, "the order before the edit", orderFactsOf(t, id)[theOrder], "gross", "40.00", "line_count", "1")
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(2), "subtotalLineItemsQuantity", float64(2)))
	fold(t, id, sourceLine, lineAt(id, lineTwo, theOrder, ver(2), "quantity", float64(1), "currentQuantity", float64(1),
		"originalUnitPriceSet", set("19.99", "EUR"), "discountAllocations", []any{}))
	o := orderFactsOf(t, id)[theOrder]
	want(t, "the order after the edit", o, "gross", "59.99", "units", "2", "line_count", "2", "lines_complete", "true", "expected_quantity", "2")
	wantVersion(t, "the order after the edit", o, "lines_version", ver(2))
}

func TestACancelledOrderIsMarkedAndKeepsItsFact(t *testing.T) {
	id := foldStore(t)
	order, lines := threeLines(id, ver(0))
	fold(t, id, sourceLine, lines...)
	fold(t, id, sourceOrder, order)
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(1), "subtotalLineItemsQuantity", float64(4), "cancelledAt", "2026-11-02T00:00:00Z"))
	o := orderFactsOf(t, id)[theOrder]
	want(t, "the cancelled order", o, "cancelled", "true", "deleted", "false", "gross", "99.99")
	wantVersion(t, "the cancelled order", o, "order_version", ver(1))
}

// ---- tombstones and versions ----

func TestATombstoneMarksItsFactDeletedAndKeepsIt(t *testing.T) {
	id := foldStore(t)
	order, lines := threeLines(id, ver(0))
	fold(t, id, sourceOrder, order)
	fold(t, id, sourceLine, lines...)
	fold(t, id, sourceRefund, refundAt(id, theRefund, theOrder, ver(0)))
	if n := len(refundLineFactsOf(t, id)); n != 1 {
		t.Fatalf("the refund has %d line facts before its tombstone, want one", n)
	}

	fold(t, id, sourceOrder, tombstone(id, theOrder, ver(5)))
	fold(t, id, sourceLine, tombstone(id, lineOne, ver(5)))
	fold(t, id, sourceRefund, tombstone(id, theRefund, ver(5)))

	o := orderFactsOf(t, id)[theOrder]
	want(t, "the order", o, "deleted", "true", "currency", "EUR", "shipping", "12.50")
	wantVersion(t, "the order", o, "order_version", ver(5))
	l := lineFactsOf(t, id)[lineOne]
	want(t, "the line", l, "deleted", "true", "order_gid", theOrder, "title", "Inner tube")
	wantVersion(t, "the line", l, "line_version", ver(5))
	r := refundFactsOf(t, id)[theRefund]
	want(t, "the refund", r, "deleted", "true", "money_refunded", "48.56")
	wantVersion(t, "the refund", r, "refund_version", ver(5))
	if n := len(refundLineFactsOf(t, id)); n != 0 {
		t.Errorf("a deleted refund kept %d line facts, want none", n)
	}
}

func TestATombstoneMetInALaterWindowThanTheLiveVersionLeavesTheFactDeleted(t *testing.T) {
	id := foldStore(t)
	live := orderAt(id, theOrder, ver(0))
	fold(t, id, sourceOrder, live)                            // the first window
	fold(t, id, sourceOrder, tombstone(id, theOrder, ver(3))) // the second
	fold(t, id, sourceOrder, live)                            // the first, read again
	o := orderFactsOf(t, id)[theOrder]
	want(t, "the order", o, "deleted", "true")
	wantVersion(t, "the order", o, "order_version", ver(3))
}

func TestAnOlderLiveVersionArrivingAfterTheTombstoneChangesNothing(t *testing.T) {
	id := foldStore(t)
	order, lines := threeLines(id, ver(0))
	fold(t, id, sourceOrder, order)
	fold(t, id, sourceLine, lines...)
	fold(t, id, sourceOrder, tombstone(id, theOrder, ver(4)))
	before := snapshot(t, id)
	// Another node hands the live version written between the two.
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(2), "totalShippingPriceSet", set("99.00", "EUR")))
	if after := snapshot(t, id); after != before {
		t.Errorf("an older live version moved the facts:\nbefore %s\nafter  %s", before, after)
	}
	want(t, "the order", orderFactsOf(t, id)[theOrder], "deleted", "true", "shipping", "12.50")
}

func TestATombstoneOfARowNeverFoldedIsKeptAsADeletedFact(t *testing.T) {
	id := foldStore(t)
	fold(t, id, sourceOrder, tombstone(id, theOrder, ver(4)))
	fold(t, id, sourceLine, tombstone(id, lineOne, ver(4)))
	fold(t, id, sourceRefund, tombstone(id, theRefund, ver(4)))
	o, l, r := orderFactsOf(t, id)[theOrder], lineFactsOf(t, id)[lineOne], refundFactsOf(t, id)[theRefund]
	want(t, "the order", o, "deleted", "true")
	want(t, "the line", l, "deleted", "true", "order_gid", "")
	want(t, "the refund", r, "deleted", "true")
	wantVersion(t, "the order", o, "order_version", ver(4))
	wantVersion(t, "the line", l, "line_version", ver(4))
	wantVersion(t, "the refund", r, "refund_version", ver(4))

	before := snapshot(t, id)
	order, lines := threeLines(id, ver(1))
	fold(t, id, sourceOrder, order)
	fold(t, id, sourceLine, lines[0])
	fold(t, id, sourceRefund, refundAt(id, theRefund, theOrder, ver(1)))
	if after := snapshot(t, id); after != before {
		t.Errorf("the live rows older than the tombstones were written:\nbefore %s\nafter  %s", before, after)
	}
}

func TestTheLinesOfARetiredOrderKeepTheirFactsLive(t *testing.T) {
	id := foldStore(t)
	order, lines := threeLines(id, ver(0))
	fold(t, id, sourceOrder, order)
	fold(t, id, sourceLine, lines...)
	fold(t, id, sourceRefund, refundAt(id, theRefund, theOrder, ver(0)))
	fold(t, id, sourceOrder, tombstone(id, theOrder, ver(5)))
	want(t, "the order", orderFactsOf(t, id)[theOrder], "deleted", "true")
	for gid, l := range lineFactsOf(t, id) {
		want(t, gid, l, "deleted", "false")
	}
	want(t, "the refund", refundFactsOf(t, id)[theRefund], "deleted", "false")
}

func TestADerivationLeavesDeletedFactsOut(t *testing.T) {
	id := foldStore(t)
	order, lines := threeLines(id, ver(0))
	fold(t, id, sourceOrder, order)
	fold(t, id, sourceLine, lines...)
	refundB := "gid://shopify/Refund/701"
	fold(t, id, sourceRefund,
		refundAt(id, theRefund, theOrder, ver(0)),
		refundAt(id, refundB, theOrder, ver(0), "totalRefundedSet", set("19.99", "EUR"),
			"refundLineItems", []any{refundLine(lineTwo, float64(1), "19.99", "0.00")}, "refundShippingLines", []any{}))
	want(t, "line two before", lineFactsOf(t, id)[lineTwo], "refunded_quantity", "1")

	fold(t, id, sourceLine, tombstone(id, lineOne, ver(3)))
	fold(t, id, sourceRefund, tombstone(id, refundB, ver(3)))
	o := orderFactsOf(t, id)[theOrder]
	// Line two (19.99) and the gift card are left; the gift card is in
	// line_count and not in gross or units.
	want(t, "the order", o, "gross", "19.99", "discounts", "0", "units", "1", "line_count", "2", "lines_complete", "false")
	want(t, "line two", lineFactsOf(t, id)[lineTwo], "refunded_quantity", "0", "sold_quantity", "1")
}

func TestAnOrderThatComesBackAfterATombstoneIsWrittenAgain(t *testing.T) {
	id := foldStore(t)
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(0)))
	fold(t, id, sourceOrder, tombstone(id, theOrder, ver(2)))
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(4), "totalShippingPriceSet", set("7.00", "EUR")))
	o := orderFactsOf(t, id)[theOrder]
	want(t, "the order", o, "deleted", "false", "shipping", "7.00")
	wantVersion(t, "the order", o, "order_version", ver(4))
}

func TestAnOlderVersionArrivingAfterANewerOneChangesNothing(t *testing.T) {
	id := foldStore(t)
	order, lines := threeLines(id, ver(2))
	fold(t, id, sourceOrder, order)
	fold(t, id, sourceLine, lines...)
	fold(t, id, sourceRefund, refundAt(id, theRefund, theOrder, ver(2)))
	before := snapshot(t, id)
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(1), "totalShippingPriceSet", set("1.00", "EUR"), "subtotalLineItemsQuantity", float64(9)))
	fold(t, id, sourceLine, lineAt(id, lineOne, theOrder, ver(1), "quantity", float64(7), "currentQuantity", float64(7)))
	fold(t, id, sourceRefund, refundAt(id, theRefund, theOrder, ver(1), "refundLineItems", []any{}))
	if after := snapshot(t, id); after != before {
		t.Errorf("older versions moved the facts:\nbefore %s\nafter  %s", before, after)
	}
}

// ---- exact ----

// AN AMOUNT WIDER THAN A FLOAT. A unit price of twenty significant digits,
// three of them sold less a third of a discount: every derived column reads
// back digit for digit.
func TestAnAmountWiderThanAFloatIsDerivedDigitForDigit(t *testing.T) {
	id := foldStore(t)
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(0), "subtotalLineItemsQuantity", float64(3)))
	fold(t, id, sourceLine, lineAt(id, lineOne, theOrder, ver(0), "quantity", float64(3), "currentQuantity", float64(2),
		"originalUnitPriceSet", set("123456789012345678.91", "EUR"), "discountAllocations", []any{allocation("100000000000000000.01", "EUR")}))
	want(t, "the line", lineFactsOf(t, id)[lineOne], "unit_price", "123456789012345678.91", "allocated_discounts", "100000000000000000.01",
		"gross", "246913578024691357.82", "discounts", "66666666666666666.67", "sold_quantity", "2")
	want(t, "the order", orderFactsOf(t, id)[theOrder], "gross", "246913578024691357.82", "discounts", "66666666666666666.67")
}

// ---- idempotence: the property ----

// propModel is one generated store: every version of every row, in the
// generator's own terms, so the test can compute what the facts must be
// without the package's rules.
type propVersion struct {
	source     string
	gid        string
	at         time.Time
	gone       bool
	unreadable bool
	row        map[string]any

	// the model of a live version
	order                       string // a line's or a refund's order
	quantity, current           int
	price                       string
	allocs                      []string
	gift                        bool
	expected                    int
	taxesIncluded               bool
	refunded, shipping, taxBack string
	detail                      bool
	refundLines                 []propRefundLine
}

type propRefundLine struct {
	line     string
	quantity int
	subtotal string
}

var minorOf = map[string]int{"EUR": 2, "JPY": 0}

// amountIn is a random amount in currency at a scale of its minor units, or
// one more when extra.
func amountIn(r *rand.Rand, currency string, max int, extra bool) string {
	scale := minorOf[currency]
	if extra {
		scale++
	}
	n := r.IntN(max*pow10i(scale) + 1)
	return decimalText(n, scale)
}

func pow10i(n int) int {
	p := 1
	for range n {
		p *= 10
	}
	return p
}

func decimalText(n, scale int) string {
	if scale == 0 {
		return fmt.Sprint(n)
	}
	s := fmt.Sprintf("%0*d", scale+1, n)
	return s[:len(s)-scale] + "." + s[len(s)-scale:]
}

// genStore generates one store's versions. The clock moves a second per
// version, so every version of the store is at an instant of its own.
func genStore(r *rand.Rand, storeID string) []propVersion {
	currency := "EUR"
	if r.IntN(4) == 0 {
		currency = "JPY"
	}
	clock := foldBase
	tick := func() time.Time { clock = clock.Add(time.Second); return clock }
	var out []propVersion
	// versionsOf writes a row 1 to 4 times; any version may be its
	// tombstone, so a row can come back after one; any but a tombstone may be
	// unreadable, its newest included.
	versionsOf := func(make func(n int) propVersion) {
		count := 1 + r.IntN(4)
		for n := range count {
			v := make(n)
			v.at = tick()
			if r.IntN(6) == 0 {
				v = propVersion{source: v.source, gid: v.gid, at: v.at, gone: true}
			} else if r.IntN(8) == 0 {
				v.unreadable = true
			}
			out = append(out, v)
		}
	}
	for o := range 1 + r.IntN(3) {
		orderGID := fmt.Sprintf("gid://shopify/Order/%d", o+1)
		var lineGIDs []string
		var gifts []bool
		nLines := 1 + r.IntN(3)
		for l := range nLines {
			lineGIDs = append(lineGIDs, fmt.Sprintf("gid://shopify/LineItem/%d%d", o+1, l+1))
			gifts = append(gifts, r.IntN(5) == 0)
		}
		versionsOf(func(int) propVersion {
			return propVersion{source: sourceOrder, gid: orderGID, expected: r.IntN(3 * nLines), taxesIncluded: r.IntN(3) == 0}
		})
		for l, lineGID := range lineGIDs {
			versionsOf(func(int) propVersion {
				q := r.IntN(4)
				v := propVersion{source: sourceLine, gid: lineGID, order: orderGID, quantity: q, current: r.IntN(q + 1),
					price: amountIn(r, currency, 90, false), gift: gifts[l]}
				for range r.IntN(3) {
					v.allocs = append(v.allocs, amountIn(r, currency, 9, r.IntN(2) == 0))
				}
				return v
			})
		}
		for f := range r.IntN(3) {
			refundGID := fmt.Sprintf("gid://shopify/Refund/%d%d", o+1, f+1)
			versionsOf(func(int) propVersion {
				v := propVersion{source: sourceRefund, gid: refundGID, order: orderGID, detail: r.IntN(5) != 0,
					refunded: amountIn(r, currency, 200, false), shipping: amountIn(r, currency, 10, false), taxBack: amountIn(r, currency, 10, false)}
				for range r.IntN(3) {
					v.refundLines = append(v.refundLines, propRefundLine{line: lineGIDs[r.IntN(len(lineGIDs))], quantity: r.IntN(3),
						subtotal: amountIn(r, currency, 50, false)})
				}
				return v
			})
		}
	}
	for n := range out {
		out[n].row = propRow(storeID, currency, out[n])
	}
	return out
}

// propRow is a version as the walk would hand it.
func propRow(storeID, currency string, v propVersion) map[string]any {
	if v.gone {
		return tombstone(storeID, v.gid, v.at)
	}
	var row map[string]any
	switch v.source {
	case sourceOrder:
		row = orderAt(storeID, v.gid, v.at, "currencyCode", currency, "subtotalLineItemsQuantity", float64(v.expected),
			"taxesIncluded", v.taxesIncluded, "totalShippingPriceSet", set("1", currency), "totalTaxSet", set("1", currency),
			"currentSubtotalPriceSet", nil, "totalDiscountsSet", nil, "totalRefundedSet", nil)
		if v.unreadable {
			row["totalShippingPriceSet"] = set("1,00", currency)
		}
	case sourceLine:
		allocs := []any{}
		for _, a := range v.allocs {
			allocs = append(allocs, allocation(a, currency))
		}
		row = lineAt(storeID, v.gid, v.order, v.at, "quantity", float64(v.quantity), "currentQuantity", float64(v.current),
			"originalUnitPriceSet", set(v.price, currency), "discountAllocations", allocs, "isGiftCard", v.gift)
		if v.unreadable {
			row["originalUnitPriceSet"] = set("1,00", currency)
		}
	case sourceRefund:
		row = refundAt(storeID, v.gid, v.order, v.at, "totalRefundedSet", set(v.refunded, currency), "duties", []any{})
		if v.detail {
			items := []any{}
			for _, l := range v.refundLines {
				items = append(items, map[string]any{"lineItem": map[string]any{"id": l.line}, "quantity": float64(l.quantity),
					"subtotalSet": set(l.subtotal, currency), "totalTaxSet": set("0", currency)})
			}
			row["refundLineItems"] = items
			row["refundShippingLines"] = []any{map[string]any{"subtotalAmountSet": set(v.shipping, currency), "taxAmountSet": set("0", currency)}}
			row["orderAdjustments"] = []any{map[string]any{"taxAmountSet": set(v.taxBack, currency)}}
		} else {
			row["refundLineItems"], row["refundShippingLines"], row["orderAdjustments"], row["duties"] = nil, nil, nil, nil
		}
		if v.unreadable {
			row["totalRefundedSet"] = set("1,0", currency)
		}
	}
	return row
}

type propPage struct {
	source string
	rows   []map[string]any
}

// pagesOf is the store's versions as a walk would page them: each source in
// write-time order, in pages of 1 to 4.
func pagesOf(r *rand.Rand, versions []propVersion) []propPage {
	var pages []propPage
	for _, source := range []string{sourceOrder, sourceLine, sourceRefund} {
		var rows []map[string]any
		for _, v := range versions {
			if v.source == source {
				rows = append(rows, v.row)
			}
		}
		for len(rows) > 0 {
			n := min(len(rows), 1+r.IntN(4))
			pages = append(pages, propPage{source: source, rows: rows[:n]})
			rows = rows[n:]
		}
	}
	return pages
}

// finalOf is each row's newest readable version: what its fact must hold.
func finalOf(versions []propVersion) map[string]propVersion {
	out := map[string]propVersion{}
	for _, v := range versions {
		if v.unreadable {
			continue
		}
		if had, ok := out[v.source+v.gid]; !ok || v.at.After(had.at) {
			out[v.source+v.gid] = v
		}
	}
	return out
}

func ratOf(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("%q is not a decimal", s)
	}
	return r
}

func scaleOfText(s string) int {
	if _, frac, ok := strings.Cut(s, "."); ok {
		return len(frac)
	}
	return 0
}

// FOLDED IN ANY ORDER, ANY NUMBER OF TIMES, THE FACTS ARE THE SAME. For a
// few hundred seeded stores -- orders with lines, discounts, refunds, rows
// rewritten, retired and unreadable -- the walked pages are folded once in
// order into one store and shuffled, some twice, into another. The two end
// the same, and every derived column equals what this test computes with
// big.Rat from each row's newest readable version.
//
// A deleted fact is compared by its key, its version and deleted alone: what
// else it holds depends on whether its live version was folded before it, and
// is never read.
func TestFoldsInAnyOrderMatchAnIndependentComputation(t *testing.T) {
	stores := 200
	if testing.Short() {
		stores = 40
	}
	for seed := range stores {
		r := rand.New(rand.NewPCG(uint64(seed), 13))
		inOrder, shuffled := foldStore(t), foldStore(t)
		versions := genStore(r, "")
		pages := pagesOf(r, versions)
		for _, p := range pages {
			foldProp(t, inOrder, p)
		}
		var again []propPage
		for _, p := range pages {
			again = append(again, p)
			if r.IntN(3) == 0 {
				again = append(again, p)
			}
		}
		r.Shuffle(len(again), func(a, b int) { again[a], again[b] = again[b], again[a] })
		for _, p := range again {
			foldProp(t, shuffled, p)
		}
		if a, b := comparable(t, inOrder), comparable(t, shuffled); a != b {
			t.Fatalf("seed %d: the shuffled fold differs from the fold in order:\nin order %s\nshuffled %s", seed, a, b)
		}
		checkDerived(t, seed, shuffled, finalOf(versions))
		checkDeleted(t, seed, shuffled, finalOf(versions))
		checkRecords(t, seed, shuffled, versions)
	}
}

// newestOf is each row's newest version met, readable or not.
func newestOf(versions []propVersion) map[string]propVersion {
	out := map[string]propVersion{}
	for _, v := range versions {
		if had, ok := out[v.source+v.gid]; !ok || v.at.After(had.at) {
			out[v.source+v.gid] = v
		}
	}
	return out
}

// checkDeleted holds that each row whose newest readable version is its
// tombstone has a fact marked deleted at that version.
func checkDeleted(t *testing.T, seed int, storeID string, final map[string]propVersion) {
	t.Helper()
	facts := map[string]map[string]map[string]any{
		sourceOrder: orderFactsOf(t, storeID), sourceLine: lineFactsOf(t, storeID), sourceRefund: refundFactsOf(t, storeID),
	}
	versionColumn := map[string]string{sourceOrder: "order_version", sourceLine: "line_version", sourceRefund: "refund_version"}
	for _, v := range final {
		if !v.gone {
			continue
		}
		fact := facts[v.source][v.gid]
		if fact == nil || cell(fact, "deleted") != "true" {
			t.Errorf("seed %d: the %s %s, retired at its newest readable version, has fact %v, want one marked deleted", seed, v.source, v.gid, fact)
			continue
		}
		if got := instantCell(t, fact, versionColumn[v.source]); !got.Equal(v.at) {
			t.Errorf("seed %d: the deleted %s %s is at version %v, want its tombstone's %v", seed, v.source, v.gid, got, v.at)
		}
	}
}

// checkRecords holds that a row has a record exactly when the newest version
// met of it could not be read, at that version.
func checkRecords(t *testing.T, seed int, storeID string, versions []propVersion) {
	t.Helper()
	records := recordsOf(t, storeID)
	want := 0
	for _, v := range newestOf(versions) {
		r := records[v.source+"/"+v.gid]
		if !v.unreadable {
			if r != nil {
				t.Errorf("seed %d: the %s %s, readable at its newest version, has a record %v", seed, v.source, v.gid, r)
			}
			continue
		}
		want++
		if r == nil {
			t.Errorf("seed %d: the %s %s, unreadable at its newest version, has no record", seed, v.source, v.gid)
			continue
		}
		if got := instantCell(t, r, "version"); !got.Equal(v.at) || cell(r, "field") == "" {
			t.Errorf("seed %d: the record of the %s %s is %v, want its newest version %v and a field", seed, v.source, v.gid, r, v.at)
		}
	}
	if len(records) != want {
		t.Errorf("seed %d: the store keeps %d records, want %d", seed, len(records), want)
	}
}

// foldProp folds a generated page into a store: the rows carry no store id
// until here.
func foldProp(t *testing.T, storeID string, p propPage) {
	t.Helper()
	rows := make([]map[string]any, len(p.rows))
	for n, row := range p.rows {
		c := map[string]any{}
		for k, v := range row {
			c[k] = v
		}
		c["storeId"] = storeID
		rows[n] = c
	}
	if _, err := foldRows(t, storeID, p.source, wholesaleState{}, rows...); err != nil {
		t.Fatalf("fold: %v", err)
	}
}

// comparable is the store's facts with every deleted one cut to its key, its
// version and deleted, and no store id.
func comparable(t *testing.T, storeID string) string {
	t.Helper()
	cut := func(rows map[string]map[string]any, version string) map[string]map[string]any {
		for k, row := range rows {
			delete(row, "store_id")
			if row["deleted"] == true {
				rows[k] = map[string]any{"deleted": true, version: row[version]}
			}
		}
		return rows
	}
	lines := refundLineFactsOf(t, storeID)
	for _, row := range lines {
		delete(row, "store_id")
	}
	records := recordsOf(t, storeID)
	for _, row := range records {
		delete(row, "store_id")
	}
	raw, err := json.Marshal(map[string]any{
		"order":       cut(orderFactsOf(t, storeID), "order_version"),
		"line":        cut(lineFactsOf(t, storeID), "line_version"),
		"refund":      cut(refundFactsOf(t, storeID), "refund_version"),
		"refund_line": lines,
		"unreadable":  records,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// checkDerived computes every derived column from the final versions with
// big.Rat, and compares the store's facts with it.
func checkDerived(t *testing.T, seed int, storeID string, final map[string]propVersion) {
	t.Helper()
	orders, lines, refunds := orderFactsOf(t, storeID), lineFactsOf(t, storeID), refundFactsOf(t, storeID)
	live := func(source string) []propVersion {
		var out []propVersion
		for _, v := range final {
			if v.source == source && !v.gone {
				out = append(out, v)
			}
		}
		sort.Slice(out, func(a, b int) bool { return out[a].gid < out[b].gid })
		return out
	}
	eq := func(what, got string, wantRat *big.Rat) {
		t.Helper()
		if ratOf(t, got).Cmp(wantRat) != 0 {
			t.Errorf("seed %d: %s = %s, want %s", seed, what, got, wantRat.FloatString(6))
		}
	}
	eqInt := func(what, got string, wantInt int) {
		t.Helper()
		if got != fmt.Sprint(wantInt) {
			t.Errorf("seed %d: %s = %s, want %d", seed, what, got, wantInt)
		}
	}

	// Each live line: refunded, sold, gross and discounts.
	refundedOf := map[string]int{}
	for _, f := range live(sourceRefund) {
		if !f.detail {
			continue
		}
		for _, l := range f.refundLines {
			refundedOf[l.line] += l.quantity
		}
	}
	type sums struct {
		gross, discounts  *big.Rat
		units, count, qty int
		version           time.Time
	}
	byOrder := map[string]*sums{}
	giftLine, deletedLine := map[string]bool{}, map[string]bool{}
	for _, v := range final {
		if v.source == sourceLine && v.gone {
			deletedLine[v.gid] = true
		}
	}
	for _, l := range live(sourceLine) {
		giftLine[l.gid] = l.gift
		refunded := refundedOf[l.gid]
		sold := min(l.quantity, l.current+refunded)
		gross := new(big.Rat).Mul(ratOf(t, l.price), big.NewRat(int64(sold), 1))
		alloc, scale := new(big.Rat), 0
		for _, a := range l.allocs {
			alloc.Add(alloc, ratOf(t, a))
			scale = max(scale, scaleOfText(a))
		}
		discounts := new(big.Rat)
		if l.quantity > 0 {
			share := new(big.Rat).Mul(alloc, big.NewRat(int64(sold), int64(l.quantity)))
			discounts = halfEven(share, max(scale, minorOf[currencyOfLine(lines[l.gid])]))
		}
		fact := lines[l.gid]
		if fact == nil {
			t.Errorf("seed %d: line %s has no fact", seed, l.gid)
			continue
		}
		eqInt("refunded_quantity of "+l.gid, cell(fact, "refunded_quantity"), refunded)
		eqInt("sold_quantity of "+l.gid, cell(fact, "sold_quantity"), sold)
		eq("gross of "+l.gid, cell(fact, "gross"), gross)
		eq("discounts of "+l.gid, cell(fact, "discounts"), discounts)
		s := byOrder[l.order]
		if s == nil {
			s = &sums{gross: new(big.Rat), discounts: new(big.Rat)}
			byOrder[l.order] = s
		}
		s.count++
		s.qty += l.quantity
		if l.at.After(s.version) {
			s.version = l.at
		}
		if !l.gift {
			s.gross.Add(s.gross, gross)
			s.discounts.Add(s.discounts, discounts)
			s.units += sold
		}
	}

	// Each live order: its sums over its live lines.
	taxesIncluded := map[string]bool{}
	for _, o := range final {
		if o.source == sourceOrder {
			taxesIncluded[o.gid] = !o.gone && o.taxesIncluded
		}
	}
	for _, o := range live(sourceOrder) {
		fact := orders[o.gid]
		if fact == nil {
			t.Errorf("seed %d: order %s has no fact", seed, o.gid)
			continue
		}
		s := byOrder[o.gid]
		if s == nil {
			s = &sums{gross: new(big.Rat), discounts: new(big.Rat)}
		}
		eq("gross of "+o.gid, cell(fact, "gross"), s.gross)
		eq("discounts of "+o.gid, cell(fact, "discounts"), s.discounts)
		eqInt("units of "+o.gid, cell(fact, "units"), s.units)
		eqInt("line_count of "+o.gid, cell(fact, "line_count"), s.count)
		if got, wantComplete := cell(fact, "lines_complete"), fmt.Sprint(s.qty >= o.expected); got != wantComplete {
			t.Errorf("seed %d: lines_complete of %s = %s, want %s", seed, o.gid, got, wantComplete)
		}
		if s.count == 0 {
			if cell(fact, "lines_version") != "null" {
				t.Errorf("seed %d: lines_version of %s = %s, want null", seed, o.gid, cell(fact, "lines_version"))
			}
		} else if got := instantCell(t, fact, "lines_version"); !got.Equal(s.version) {
			t.Errorf("seed %d: lines_version of %s = %v, want %v", seed, o.gid, got, s.version)
		}
	}

	// Each live refund.
	for _, f := range live(sourceRefund) {
		fact := refunds[f.gid]
		if fact == nil {
			t.Errorf("seed %d: refund %s has no fact", seed, f.gid)
			continue
		}
		if !f.detail {
			for _, col := range []string{"gift_card_refunded", "item_refunds", "units_returned", "shipping_refunded"} {
				if cell(fact, col) != "null" {
					t.Errorf("seed %d: %s of %s, whose detail was not read, = %s, want null", seed, col, f.gid, cell(fact, col))
				}
			}
			continue
		}
		gift, units := new(big.Rat), 0
		for _, l := range f.refundLines {
			switch {
			case giftLine[l.line]:
				gift.Add(gift, ratOf(t, l.subtotal))
			case deletedLine[l.line]:
			default:
				units += l.quantity
			}
		}
		item := new(big.Rat).Sub(ratOf(t, f.refunded), ratOf(t, f.shipping))
		item.Sub(item, gift)
		if !taxesIncluded[f.order] {
			item.Sub(item, ratOf(t, f.taxBack))
		}
		eq("gift_card_refunded of "+f.gid, cell(fact, "gift_card_refunded"), gift)
		eqInt("units_returned of "+f.gid, cell(fact, "units_returned"), units)
		eq("item_refunds of "+f.gid, cell(fact, "item_refunds"), item)
	}
}

func currencyOfLine(fact map[string]any) string {
	if fact == nil {
		return "EUR"
	}
	return cell(fact, "currency")
}

// ---- the wholesale judgement ----

// AN ORDER IS FOLDED WITH ITS WHOLESALE JUDGEMENT: a granted buyer's order is
// wholesale, another's is not.
func TestAnOrderOfAGrantedBuyerIsFoldedWholesale(t *testing.T) {
	id := foldStore(t)
	state := wholesaleState{Enabled: true, Granted: map[string]bool{buyerEmail: true}}
	other := "gid://shopify/Order/1002"
	if _, err := foldRows(t, id, sourceOrder, state, orderAt(id, theOrder, ver(0)), orderAt(id, other, ver(0), "email", "other@example.com")); err != nil {
		t.Fatal(err)
	}
	facts := orderFactsOf(t, id)
	want(t, "the granted buyer's order", facts[theOrder], "wholesale", "true")
	want(t, "another buyer's order", facts[other], "wholesale", "false")
}

// ---- the fields read ----

// THE WALK READS NO FIELD OUTSIDE ITS SHAPES: every key readPage reads of a
// row of each source is a field of that source's shape.
func TestTheWalkReadsNoFieldOutsideItsShapes(t *testing.T) {
	shapes := readShapes(t)
	order, lines := threeLines("store-a", ver(0))
	cases := []struct {
		source, shape string
		rows          []map[string]any
	}{
		{sourceOrder, "storeStatisticsOrder", []map[string]any{order, tombstone("store-a", theOrder, ver(1)), orderAt("store-a", "gid://shopify/Order/2", ver(0), "totalTaxSet", set("1,0", "EUR"))}},
		{sourceLine, "storeStatisticsLine", append(lines, tombstone("store-a", lineOne, ver(1)))},
		{sourceRefund, "storeStatisticsRefund", []map[string]any{refundAt("store-a", theRefund, theOrder, ver(0)), tombstone("store-a", theRefund, ver(1)),
			refundAt("store-a", "gid://shopify/Refund/2", theOrder, ver(0), "refundLineItems", nil)}},
	}
	for _, c := range cases {
		ok := map[string]bool{}
		for _, f := range shapes[c.shape].fields {
			ok[strings.TrimPrefix(f, "row.")] = true
		}
		seen := map[string]bool{}
		rows := make([]fields, len(c.rows))
		for n, row := range c.rows {
			rows[n] = recording{m: row, seen: seen}
		}
		if _, err := readPageFrom("store-a", c.source, rows, wholesaleState{Enabled: true, Granted: map[string]bool{buyerEmail: true}}); err != nil {
			t.Fatalf("%s: %v", c.source, err)
		}
		if len(seen) == 0 {
			t.Errorf("%s: nothing was read", c.source)
		}
		var outside []string
		for k := range seen {
			if !ok[k] {
				outside = append(outside, k)
			}
		}
		sort.Strings(outside)
		if len(outside) > 0 {
			t.Errorf("%s: read %v, which %s does not project", c.source, outside, c.shape)
		}
	}
}

// ---- runs through the real engine ----

// payloadOf is a fixture row less what the engine and mirror() write, and
// less every field it holds as null.
func payloadOf(row map[string]any) map[string]any {
	for _, k := range []string{"id", "createdAt", "storeId", "gid", "syncedAt"} {
		delete(row, k)
	}
	// The mirror writes an absent field by leaving it out.
	for k, v := range row {
		if v == nil {
			delete(row, k)
		}
	}
	return row
}

func mirrorOrder(t *testing.T, storeID, gid string, kv ...any) {
	t.Helper()
	row := payloadOf(orderAt(storeID, gid, ver(0), kv...))
	if _, set := kvHas(kv, "cancelledAt"); !set {
		delete(row, "cancelledAt")
	}
	mirror(t, orderConcept, storeID, gid, row)
}

func mirrorLine(t *testing.T, storeID, gid, orderGID string, kv ...any) {
	t.Helper()
	mirror(t, "v1:shopify:orderLineItem", storeID, gid, payloadOf(lineAt(storeID, gid, orderGID, ver(0), kv...)))
}

func mirrorRefund(t *testing.T, storeID, gid, orderGID string, kv ...any) {
	t.Helper()
	mirror(t, "v1:shopify:refund", storeID, gid, payloadOf(refundAt(storeID, gid, orderGID, ver(0), kv...)))
}

// mirrorThreeLines mirrors the three-line order and a refund of one unit of
// its first line.
func mirrorThreeLines(t *testing.T, storeID string) {
	t.Helper()
	mirrorOrder(t, storeID, theOrder, "subtotalLineItemsQuantity", float64(4))
	mirrorLine(t, storeID, lineOne, theOrder, "quantity", float64(2), "currentQuantity", float64(1),
		"originalUnitPriceSet", set("40.00", "EUR"), "discountAllocations", []any{allocation("8.00", "EUR")})
	mirrorLine(t, storeID, lineTwo, theOrder, "quantity", float64(1), "currentQuantity", float64(1),
		"originalUnitPriceSet", set("19.99", "EUR"), "discountAllocations", []any{})
	mirrorLine(t, storeID, lineGift, theOrder, "quantity", float64(1), "currentQuantity", float64(1), "isGiftCard", true,
		"originalUnitPriceSet", set("25.00", "EUR"), "discountAllocations", []any{})
	mirrorRefund(t, storeID, theRefund, theOrder)
}

// wantThreeLines holds the facts of mirrorThreeLines.
func wantThreeLines(t *testing.T, storeID string) {
	t.Helper()
	want(t, "the order", orderFactsOf(t, storeID)[theOrder], "gross", "99.99", "discounts", "8.00", "units", "3", "line_count", "3", "lines_complete", "true")
	want(t, "line one", lineFactsOf(t, storeID)[lineOne], "refunded_quantity", "1", "sold_quantity", "2")
	want(t, "the refund", refundFactsOf(t, storeID)[theRefund], "item_refunds", "36.00", "units_returned", "1")
}

// runStore is an Integration whose engine is the shared one, wrapped, and a
// store of its own whose rows in every store_sales_ table go when the test
// ends.
func runStore(t *testing.T) (*Integration, string) {
	t.Helper()
	i := testIntegration(t)
	return i, keptStore(t)
}

// hookEngine answers from the real engine and calls before (when set) with
// each read before it is made, and after with each read once it answered.
type hookEngine struct {
	inner  Engine
	before func(query string) error
	after  func(query string)
}

func (e *hookEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if e.before != nil {
		if err := e.before(query); err != nil {
			return nil, err
		}
	}
	res, err := e.inner.Execute(ctx, query)
	if err == nil && e.after != nil {
		e.after(query)
	}
	return res, err
}

// A WALKED ROW IS FOLDED FROM THE REAL ENGINE: a run reads the store's rows
// through the engine and the facts carry their write times.
func TestARunFoldsTheRowsTheEngineWalks(t *testing.T) {
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	rep, err := run(t, i)
	if err != nil {
		t.Fatal(err)
	}
	wantThreeLines(t, id)
	wantVersion(t, "the order", orderFactsOf(t, id)[theOrder], "order_version", writtenAt(t, orderConcept, id, theOrder))
	wantVersion(t, "line one", lineFactsOf(t, id)[lineOne], "line_version", writtenAt(t, "v1:shopify:orderLineItem", id, lineOne))
	wantVersion(t, "the refund", refundFactsOf(t, id)[theRefund], "refund_version", writtenAt(t, "v1:shopify:refund", id, theRefund))
	if rep.Facts != 5 || rep.Unreadable != 0 || rep.Rows[sourceOrder] != 1 || rep.Rows[sourceLine] != 3 || rep.Rows[sourceRefund] != 1 {
		t.Errorf("the run reported %+v, want 5 facts of 1 order, 3 lines and 1 refund", rep)
	}
	w := watermarksOf(t, id)
	for _, source := range []string{sourceOrder, sourceLine, sourceRefund} {
		if w[source] == nil || cell(w[source], "walked_through") == "null" {
			t.Errorf("the %s watermark is %v, want one walked to the clock", source, w[source])
		}
	}
	// A second run reads the overlap again and changes nothing.
	before := snapshot(t, id)
	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, id); after != before {
		t.Errorf("a second run moved the facts:\nbefore %s\nafter  %s", before, after)
	}
}

func TestTwoRunsAtOnceWriteTheSameFacts(t *testing.T) {
	a, id := runStore(t)
	b := testIntegration(t)
	mirrorThreeLines(t, id)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for n, i := range []*Integration{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[n] = run(t, i)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	wantThreeLines(t, id)
	before := snapshot(t, id)
	if _, err := run(t, a); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, id); after != before {
		t.Errorf("a run after two at once moved the facts:\nbefore %s\nafter  %s", before, after)
	}
}

func TestARebuildLandingBetweenAPagesReadAndItsWriteEndsWithTheFullFacts(t *testing.T) {
	i, id := runStore(t)
	for n := range 600 {
		mirrorOrder(t, id, fmt.Sprintf("gid://shopify/Order/%d", 10000+n))
	}
	// One run first, so the store's own rebuild (its first fill) is behind it.
	exec(t, `UPDATE store_sales_store SET definition = ? WHERE store_id = ?`, storeStatsDefinition, id)
	var mu sync.Mutex
	pages, struck := 0, false
	i.engine = &hookEngine{inner: i.engine, after: func(query string) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.Contains(query, "storeStatisticsOrdersChanged") || struck {
			return
		}
		pages++
		if pages == 2 {
			struck = true
			markRebuild(t, id, time.Now())
		}
	}}
	if _, err := run(t, i); err != nil {
		t.Fatalf("the run under a rebuild answered %v; a rebuild is no failure", err)
	}
	if !struck {
		t.Fatal("the rebuild never landed: the walk read fewer than two pages of orders")
	}
	if n := len(orderFactsOf(t, id)); n > 250 {
		t.Errorf("after the rebuild landed under the second page the store holds %d order facts, want the first page's at most", n)
	}
	for range 3 {
		if _, err := run(t, i); err != nil {
			t.Fatal(err)
		}
	}
	facts := orderFactsOf(t, id)
	if len(facts) != 600 {
		t.Errorf("the runs after the rebuild end with %d order facts, want 600", len(facts))
	}
	if k := keptRow(t, id); k.purging || !k.walkedThrough.Valid {
		t.Errorf("after the runs the store is purging %v, walked through %v; want neither purging nor unwalked", k.purging, k.walkedThrough)
	}
}

func TestAStoreIsNotWalkedThroughUntilEverySourceReachedTheClock(t *testing.T) {
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	failing := true
	i.engine = &hookEngine{inner: i.engine, before: func(query string) error {
		if failing && strings.Contains(query, "storeStatisticsRefundsChanged") {
			return errors.New("the refunds read is down")
		}
		return nil
	}}
	if _, err := run(t, i); err == nil {
		t.Fatal("a run whose refunds read failed answered no error")
	}
	w := watermarksOf(t, id)
	if cell(w[sourceOrder], "walked_through") == "null" || cell(w[sourceLine], "walked_through") == "null" {
		t.Errorf("the order and line sources are not walked through: %v", w)
	}
	if k := keptRow(t, id); k.walkedThrough.Valid {
		t.Errorf("the store is walked through %v while its refunds were never read", k.walkedThrough.Time)
	}
	failing = false
	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
	k := keptRow(t, id)
	if !k.walkedThrough.Valid {
		t.Fatal("the store is not walked through once every source reached the clock")
	}
	w = watermarksOf(t, id)
	least := instantCell(t, w[sourceOrder], "walked_through")
	for _, s := range []string{sourceLine, sourceRefund} {
		if at := instantCell(t, w[s], "walked_through"); at.Before(least) {
			least = at
		}
	}
	if !k.walkedThrough.Time.Equal(least) {
		t.Errorf("the store is walked through %v, want the least of its sources', %v", k.walkedThrough.Time, least)
	}
}

func TestARefundWrittenByABulkLineHasNoDetailAndOneReadEmptyHasIt(t *testing.T) {
	i, id := runStore(t)
	mirrorOrder(t, id, theOrder)
	bulk := payloadOf(refundAt(id, theRefund, theOrder, ver(0), "totalRefundedSet", set("12.00", "EUR")))
	for _, k := range []string{"refundLineItems", "refundShippingLines", "orderAdjustments", "duties"} {
		delete(bulk, k)
	}
	mirror(t, "v1:shopify:refund", id, theRefund, bulk)
	empty := "gid://shopify/Refund/701"
	mirrorRefund(t, id, empty, theOrder, "totalRefundedSet", set("12.00", "EUR"),
		"refundLineItems", []any{}, "refundShippingLines", []any{}, "orderAdjustments", []any{}, "duties", []any{})
	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
	f := refundFactsOf(t, id)
	want(t, "the bulk line's refund", f[theRefund], "detail_fetched", "false", "item_refunds", "null", "gift_card_refunded", "null",
		"units_returned", "null", "shipping_refunded", "null", "money_refunded", "12.00")
	want(t, "the refund read empty", f[empty], "detail_fetched", "true", "item_refunds", "12.00", "gift_card_refunded", "0",
		"units_returned", "0", "shipping_refunded", "0")
}

func TestARowThatCannotBeReadIsCountedAndTheWalkGoesOn(t *testing.T) {
	i, id := runStore(t)
	logs := captureLogs(i)
	good, bad := "gid://shopify/Order/2001", "gid://shopify/Order/2002"
	mirrorOrder(t, id, good)
	mirrorOrder(t, id, bad, "totalShippingPriceSet", set("12,50", "EUR"))
	rep, err := run(t, i)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unreadable != 1 {
		t.Errorf("the run counted %d unreadable rows, want one", rep.Unreadable)
	}
	facts := orderFactsOf(t, id)
	if facts[good] == nil {
		t.Error("the order after the unreadable one has no fact: the walk did not go on")
	}
	if facts[bad] != nil {
		t.Errorf("the unreadable order has a fact: %v", facts[bad])
	}
	var lines []string
	for _, l := range logs() {
		if strings.Contains(l, bad) {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("the unreadable order is in %d log lines, want one: %v", len(lines), logs())
	}
	if !strings.Contains(lines[0], "totalShippingPriceSet") || !strings.Contains(lines[0], id) || !strings.Contains(lines[0], `"source":"order"`) {
		t.Errorf("the log line does not name the field, the store and the source: %s", lines[0])
	}
	if strings.Contains(lines[0], "12,50") || strings.Contains(lines[0], buyerEmail) {
		t.Errorf("the log line holds a value of the row: %s", lines[0])
	}
}

// refuseWrites makes every write of the store to table fail, with a message
// that holds the columns named as the row has them, until the test ends.
func refuseWrites(t *testing.T, table, storeID string, columns ...string) {
	t.Helper()
	name := fmt.Sprintf("dbtest_storestats_refuse_%d", time.Now().UnixNano())
	format, args := "refused", ""
	for _, c := range columns {
		format += " %"
		args += ", NEW." + c
	}
	exec(t, `CREATE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION '`+format+`'`+args+`; END $$`)
	exec(t, `CREATE TRIGGER `+name+` BEFORE INSERT OR UPDATE ON `+table+` FOR EACH ROW WHEN (NEW.store_id = '`+storeID+`') EXECUTE FUNCTION `+name+`()`)
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		delete(refusals, storeID+table)
		exec(t, `DROP TRIGGER IF EXISTS `+name+` ON `+table)
		exec(t, `DROP FUNCTION IF EXISTS `+name+`()`)
	}
	t.Cleanup(drop)
	refusals[storeID+table] = drop
}

// refusals are the refuseWrites of the running test, so it can lift one.
var refusals = map[string]func(){}

func allowWrites(table, storeID string) {
	if drop := refusals[storeID+table]; drop != nil {
		drop()
	}
}

// NO ROW VALUE IN AN ERROR, THE REPORT OR A LOG. A line whose write the
// database refuses with a message quoting its title and its price: the run's
// error names the store and the source, and neither it nor any log line holds
// the title, the price or the buyer's email.
func TestAFoldThatFailsPutsNoRowValueInItsError(t *testing.T) {
	i, id := runStore(t)
	logs := captureLogs(i)
	n := time.Now().UnixNano()
	email := fmt.Sprintf("refused-buyer-%d@example.com", n)
	title := fmt.Sprintf("Engraved for Ada %d", n)
	price := "4321.09"
	i.wholesaleFor = func(context.Context, []string) (map[string]wholesaleState, error) {
		return map[string]wholesaleState{id: {Enabled: true, Granted: map[string]bool{email: true}}}, nil
	}
	mirrorOrder(t, id, theOrder, "email", email, "subtotalLineItemsQuantity", float64(1))
	mirrorLine(t, id, lineOne, theOrder, "title", title, "productGid", nil, "quantity", float64(1), "currentQuantity", float64(1),
		"originalUnitPriceSet", set(price, "EUR"), "discountAllocations", []any{})
	refuseWrites(t, "store_sales_line_fact", id, "title", "unit_price")
	_, err := run(t, i)
	if err == nil {
		t.Fatal("a run whose line write was refused answered no error")
	}
	text := err.Error()
	if !strings.Contains(text, id) || !strings.Contains(text, sourceLine) {
		t.Errorf("the error does not name the store and the source: %s", text)
	}
	drawn := []string{email, title, price, "Engraved for Ada"}
	for _, d := range drawn {
		if strings.Contains(text, d) {
			t.Errorf("the error holds %q: %s", d, text)
		}
		for _, line := range logs() {
			if strings.Contains(line, d) {
				t.Errorf("a log line holds %q: %s", d, line)
			}
		}
	}
	// The order's page was its own transaction: it is folded, and judged
	// wholesale with the email it was read with.
	want(t, "the order", orderFactsOf(t, id)[theOrder], "wholesale", "true")
	if l := lineFactsOf(t, id); len(l) != 0 {
		t.Errorf("the refused line has a fact: %v", l)
	}

	allowWrites("store_sales_line_fact", id)
	rep, err := run(t, i)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rep)
	for _, d := range drawn {
		if strings.Contains(string(raw), d) {
			t.Errorf("the report holds %q: %s", d, raw)
		}
	}
	want(t, "the line", lineFactsOf(t, id)[lineOne], "title", title, "gross", price)
}

// floorAt sets every store's floor to at for the rest of the test.
func floorAt(t *testing.T, at time.Time) {
	t.Helper()
	was := floorForTests
	floorForTests = at
	t.Cleanup(func() { floorForTests = was })
}

// A PAGE IS FOLDED WITH ITS WATERMARK OR NOT AT ALL. The run's clock is three
// days ahead, so the store's rows lie early in its first window, a past window
// whose watermark moves to its end, a day after them: a watermark kept apart
// from its page would leave them behind for good. When the page's transaction
// fails after its facts, before its watermark, nothing of it is kept; when it
// fails at its facts, the watermark does not move past rows it did not fold.
// Either way the next run reads the page again and ends as a run that never
// failed.
func TestAPageIsFoldedWithItsWatermarkOrNotAtAll(t *testing.T) {
	realNow := time.Now().UTC()
	floorAt(t, realNow.Add(-time.Hour))
	for _, table := range []string{"store_sales_watermark", "store_sales_order_fact"} {
		t.Run(table, func(t *testing.T) {
			i, id := runStore(t)
			i.now = func() time.Time { return realNow.Add(3 * 24 * time.Hour) }
			mirrorThreeLines(t, id)
			refuseWrites(t, table, id)
			if _, err := run(t, i); err == nil {
				t.Fatal("a run whose page was refused answered no error")
			}
			if table == "store_sales_watermark" {
				if n := len(orderFactsOf(t, id)) + len(lineFactsOf(t, id)) + len(refundFactsOf(t, id)); n != 0 {
					t.Errorf("a page whose watermark was refused kept %d facts", n)
				}
			}
			if w := watermarksOf(t, id)[sourceOrder]; w != nil && instantCell(t, w, "through").After(realNow) {
				t.Errorf("the order watermark moved to %v, past the order it did not fold", instantCell(t, w, "through"))
			}
			allowWrites(table, id)
			if _, err := run(t, i); err != nil {
				t.Fatal(err)
			}
			wantThreeLines(t, id)
		})
	}
}

// ==== fix round 1 ====

func recordsOf(t *testing.T, storeID string) map[string]map[string]any {
	return salesRows(t, "store_sales_unreadable", storeID, "source", "gid")
}

// wantRecord holds the store's one record of a source's row.
func wantRecord(t *testing.T, storeID, source, gid string, version time.Time, field string) {
	t.Helper()
	r := recordsOf(t, storeID)[source+"/"+gid]
	if r == nil {
		t.Errorf("no record of the %s %s that could not be read; records %v", source, gid, recordsOf(t, storeID))
		return
	}
	want(t, "the record of "+gid, r, "field", field)
	wantVersion(t, "the record of "+gid, r, "version", version)
	if cell(r, "row_id") == "" || strings.Contains(cell(r, "row_id"), ":") {
		t.Errorf("the record of %s keeps row_id %q, want the row's bare id", gid, cell(r, "row_id"))
	}
}

func wantNoRecord(t *testing.T, storeID string) {
	t.Helper()
	if r := recordsOf(t, storeID); len(r) != 0 {
		t.Errorf("the store keeps %d records of rows that could not be read, want none: %v", len(r), r)
	}
}

// 1. A PAGE THAT CANNOT BE READ AT ALL OPENS NO TRANSACTION: the page is read
// into facts before its transaction begins.
func TestAPageThatCannotBeReadOpensNoTransaction(t *testing.T) {
	_, db := statsEngine(t)
	id := foldStore(t)
	generation, err := generationOf(context.Background(), db, id)
	if err != nil {
		t.Fatal(err)
	}
	queries := storeSalesQueries(t)
	if _, err := keepPage(context.Background(), db, id, "nonsense", generation, walkPage{Rows: []map[string]any{orderAt(id, theOrder, ver(0))}, Through: ver(0)}, wholesaleState{}); err == nil {
		t.Fatal("a page of a source the walk does not have was folded")
	}
	locks := 0
	for _, q := range queries() {
		if strings.HasPrefix(q, "LOCK TABLE") {
			locks++
		}
	}
	if locks != 0 {
		t.Errorf("a page that could not be read took %d locked transactions, want none", locks)
	}
}

// 2. A FIRST WINDOW THAT TIMES OUT KEEPS ITS HALVED WIDTH. A source with no
// watermark yet whose first read outlasts a read keeps the halved width at the
// floor, and the next run asks the narrower window.
func TestAFirstWindowThatTimesOutKeepsItsHalvedWidth(t *testing.T) {
	floor := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	floorAt(t, floor)
	i, id := runStore(t)
	mirrorOrder(t, id, theOrder)
	fails := 1
	var asked []string
	i.engine = &hookEngine{inner: i.engine, before: func(query string) error {
		if strings.Contains(query, "storeStatisticsOrdersChanged") {
			asked = append(asked, query)
			if fails > 0 {
				fails--
				return errors.New("ERROR: canceling statement due to statement timeout")
			}
		}
		return nil
	}}
	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
	w := watermarksOf(t, id)[sourceOrder]
	if w == nil {
		t.Fatal("no order watermark after a first window timed out: the halved width is lost")
	}
	want(t, "the order watermark", w, "window_seconds", "43200")
	wantVersion(t, "the order watermark", w, "through", floor)
	asked = nil
	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
	if len(asked) == 0 || !strings.Contains(asked[0], quoteInstant(floor.Add(12*time.Hour))) {
		t.Errorf("the next run's first order read is %v, want the window to the floor plus twelve hours", asked)
	}
}

// 3. A STORE ID IS COMPARED BARE: a row holding the walked store's id in its
// prefixed form is the store's; a row of another store is not.
func TestARowOfTheStoreInItsPrefixedFormIsFolded(t *testing.T) {
	id := foldStore(t)
	other := "gid://shopify/Order/1002"
	res := fold(t, id, sourceOrder, orderAt(id, theOrder, ver(0), "storeId", "v1:shopify:store:"+id),
		orderAt(id, other, ver(0), "storeId", "dbtest-storestats-another"))
	facts := orderFactsOf(t, id)
	if facts[theOrder] == nil {
		t.Error("a row naming the store in its prefixed form was not folded")
	}
	if facts[other] != nil || res.Unreadable != 1 {
		t.Errorf("a row of another store was folded (%v) or not counted (%d unreadable)", facts[other], res.Unreadable)
	}
}

// 4. EACH SOURCE RECORDS A ROW IT CANNOT READ, by its bare row id, its gid,
// its version and the name of the field.
func TestEachSourceRecordsARowItCannotRead(t *testing.T) {
	id := foldStore(t)
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(1), "totalShippingPriceSet", set("12,50", "EUR")))
	fold(t, id, sourceLine, lineAt(id, lineOne, theOrder, ver(1), "originalUnitPriceSet", set("1,00", "EUR")))
	fold(t, id, sourceRefund, refundAt(id, theRefund, theOrder, ver(1), "totalRefundedSet", set("1,0", "EUR")))
	wantRecord(t, id, sourceOrder, theOrder, ver(1), "totalShippingPriceSet.shopMoney.amount")
	wantRecord(t, id, sourceLine, lineOne, ver(1), "originalUnitPriceSet.shopMoney.amount")
	wantRecord(t, id, sourceRefund, theRefund, ver(1), "totalRefundedSet.shopMoney.amount")
	if n := len(recordsOf(t, id)); n != 3 {
		t.Errorf("the store keeps %d records, want three", n)
	}
}

func TestANewerVersionThatCannotBeReadKeepsTheOlderFactAndIsRecorded(t *testing.T) {
	id := foldStore(t)
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(0)))
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(2), "totalShippingPriceSet", set("12,50", "EUR")))
	wantVersion(t, "the order", orderFactsOf(t, id)[theOrder], "order_version", ver(0))
	wantRecord(t, id, sourceOrder, theOrder, ver(2), "totalShippingPriceSet.shopMoney.amount")

	// An older readable version, met after it, leaves the record.
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(1)))
	wantRecord(t, id, sourceOrder, theOrder, ver(2), "totalShippingPriceSet.shopMoney.amount")

	// Readable again at a later version: no record.
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(3)))
	wantNoRecord(t, id)
	wantVersion(t, "the order", orderFactsOf(t, id)[theOrder], "order_version", ver(3))
}

func TestAnOlderUnreadableVersionMetAfterAReadableOneIsNotRecorded(t *testing.T) {
	id := foldStore(t)
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(2)))
	fold(t, id, sourceOrder, orderAt(id, theOrder, ver(1), "totalShippingPriceSet", set("12,50", "EUR")))
	wantNoRecord(t, id)
}

func TestATombstoneAtALaterVersionClearsTheRecord(t *testing.T) {
	id := foldStore(t)
	fold(t, id, sourceLine, lineAt(id, lineOne, theOrder, ver(1), "originalUnitPriceSet", set("1,00", "EUR")))
	wantRecord(t, id, sourceLine, lineOne, ver(1), "originalUnitPriceSet.shopMoney.amount")
	fold(t, id, sourceLine, tombstone(id, lineOne, ver(2)))
	wantNoRecord(t, id)
	want(t, "the line", lineFactsOf(t, id)[lineOne], "deleted", "true")
}

// ONLY A ROW WITH NO ID OF ITS OWN, OR NO WRITE TIME, IS LEFT UNRECORDED: one
// with no gid, one with no store and one of another store are each recorded;
// one with no row id and one with no write time are counted and have no
// record (unreadableRow.recordable).
func TestOnlyARowWithNoIdOrNoWriteTimeIsLeftUnrecorded(t *testing.T) {
	id := foldStore(t)
	noGID := orderAt(id, "", ver(1))
	delete(noGID, "gid")
	noGID["id"] = "v1:shopify:order:shp-no-gid"
	noStore := orderAt(id, "gid://shopify/Order/3", ver(1))
	delete(noStore, "storeId")
	another := orderAt(id, "gid://shopify/Order/4", ver(1), "storeId", "dbtest-storestats-another")
	noID := orderAt(id, "gid://shopify/Order/5", ver(1), "totalTaxSet", set("1,0", "EUR"))
	delete(noID, "id")
	noTime := orderAt(id, "gid://shopify/Order/6", ver(1))
	delete(noTime, "createdAt")
	res := fold(t, id, sourceOrder, noGID, noStore, another, noID, noTime)
	if res.Unreadable != 5 {
		t.Errorf("the page counted %d unreadable rows, want five", res.Unreadable)
	}
	records := recordsOf(t, id)
	want(t, "the row with no gid", records[sourceOrder+"/"], "row_id", "shp-no-gid", "field", "gid")
	want(t, "the row with no store", records[sourceOrder+"/gid://shopify/Order/3"], "field", "storeId")
	want(t, "the row of another store", records[sourceOrder+"/gid://shopify/Order/4"], "field", "storeId")
	if records[sourceOrder+"/gid://shopify/Order/5"] != nil || records[sourceOrder+"/gid://shopify/Order/6"] != nil || len(records) != 3 {
		t.Errorf("records %v, want the three rows that have an id of their own and a write time", records)
	}
	if facts := orderFactsOf(t, id); facts["gid://shopify/Order/6"] != nil {
		t.Errorf("the row with no write time has a fact: %v", facts["gid://shopify/Order/6"])
	}
}

// A PURGE, A REMOVAL AND A REBUILD LEAVE NO RECORD.
func TestAPurgeARemovalAndARebuildLeaveNoRecord(t *testing.T) {
	_, db := statsEngine(t)
	ctx := sweepCtx(t)
	for _, how := range []string{"rebuild", "removal"} {
		t.Run(how, func(t *testing.T) {
			i := testIntegration(t)
			id := foldStore(t)
			fold(t, id, sourceOrder, orderAt(id, theOrder, ver(1), "totalShippingPriceSet", set("12,50", "EUR")))
			if len(recordsOf(t, id)) != 1 {
				t.Fatal("no record to delete")
			}
			switch how {
			case "rebuild":
				markRebuild(t, id, time.Now())
				if done, err := purgeStore(ctx, i, id, purgeBudget); err != nil || !done {
					t.Fatalf("purge: done %v, %v", done, err)
				}
			case "removal":
				if _, err := i.removeStore(ctx, db, id, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			wantNoRecord(t, id)
		})
	}
}

// AN UNREADABLE ROW READ AGAIN IS ONE RECORD AND ONE LOG LINE. Two runs read
// the same unreadable order, the second inside the overlap: one record, and
// one Warn line in all; the report of each run counts what it met.
func TestAnUnreadableRowReadAgainIsOneRecordAndOneLogLine(t *testing.T) {
	i, id := runStore(t)
	logs := captureLogs(i)
	bad := "gid://shopify/Order/2002"
	mirrorOrder(t, id, bad, "totalShippingPriceSet", set("12,50", "EUR"))
	for n := range 2 {
		rep, err := run(t, i)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Unreadable != 1 {
			t.Errorf("run %d counted %d unreadable rows, want the one it met", n+1, rep.Unreadable)
		}
	}
	lines := 0
	for _, l := range logs() {
		if strings.Contains(l, bad) {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("the unreadable order is in %d log lines over two runs, want one", lines)
	}
	if r := recordsOf(t, id); len(r) != 1 {
		t.Errorf("the store keeps %d records, want one: %v", len(r), r)
	}
	wantRecord(t, id, sourceOrder, bad, writtenAt(t, orderConcept, id, bad), "totalShippingPriceSet.shopMoney.amount")
}

// A RECORD HOLDS NO VALUE OF ITS ROW: an order whose amount and email are
// recognisable is recorded, and neither the record, a log line nor the report
// holds either.
func TestARecordHoldsNoValueOfItsRow(t *testing.T) {
	i, id := runStore(t)
	logs := captureLogs(i)
	n := time.Now().UnixNano()
	email := fmt.Sprintf("recorded-buyer-%d@example.com", n)
	amount := fmt.Sprintf("%d,4321", n%100000)
	mirrorOrder(t, id, theOrder, "email", email, "totalShippingPriceSet", set(amount, "EUR"))
	rep, err := run(t, i)
	if err != nil {
		t.Fatal(err)
	}
	records := recordsOf(t, id)
	if len(records) != 1 {
		t.Fatalf("the store keeps %d records, want one", len(records))
	}
	raw, _ := json.Marshal(records)
	report, _ := json.Marshal(rep)
	for _, d := range []string{email, amount} {
		if strings.Contains(string(raw), d) {
			t.Errorf("the record holds %q: %s", d, raw)
		}
		if strings.Contains(string(report), d) {
			t.Errorf("the report holds %q: %s", d, report)
		}
		for _, line := range logs() {
			if strings.Contains(line, d) {
				t.Errorf("a log line holds %q: %s", d, line)
			}
		}
	}
}

// MINOR 2. A page's last recorded time takes the fallback recorded_at takes:
// an order with no syncedAt moves it to its version.
func TestTheLastRecordedTimeOfAPageFallsBackToTheVersion(t *testing.T) {
	id := foldStore(t)
	res := fold(t, id, sourceOrder, orderAt(id, theOrder, ver(3), "syncedAt", nil))
	if !res.LastRecorded.Equal(ver(3)) {
		t.Errorf("the page's last recorded time is %v, want the order's version %v", res.LastRecorded, ver(3))
	}
}

// MINOR 3. Facts counts the facts a fold wrote: a version the guard refused
// is not one.
func TestFactsCountsTheFactsAFoldWrote(t *testing.T) {
	id := foldStore(t)
	if res := fold(t, id, sourceOrder, orderAt(id, theOrder, ver(2))); res.Facts != 1 {
		t.Errorf("a new order wrote %d facts, want one", res.Facts)
	}
	if res := fold(t, id, sourceOrder, orderAt(id, theOrder, ver(1))); res.Facts != 0 {
		t.Errorf("an older version the guard refused counted %d facts, want none", res.Facts)
	}
}

// MINOR 4. A refund line naming a deleted line, or a gift-card line, is not
// an item return.
func TestARefundLineNamingADeletedOrGiftCardLineIsNoItemReturn(t *testing.T) {
	id := foldStore(t)
	order, lines := threeLines(id, ver(0))
	fold(t, id, sourceOrder, order)
	fold(t, id, sourceLine, lines...)
	fold(t, id, sourceLine, tombstone(id, lineTwo, ver(1)))
	fold(t, id, sourceRefund, refundAt(id, theRefund, theOrder, ver(2), "totalRefundedSet", set("84.99", "EUR"),
		"refundLineItems", []any{refundLine(lineOne, float64(1), "40.00", "0.00"), refundLine(lineTwo, float64(1), "19.99", "0.00"),
			refundLine(lineGift, float64(1), "25.00", "0.00")}, "refundShippingLines", []any{}))
	want(t, "the refund", refundFactsOf(t, id)[theRefund], "units_returned", "1", "gift_card_refunded", "25.00")
}

// MINOR 6. WHAT A QUIET STORE'S WALK COSTS. A store walked to the clock with
// nothing new: one turn, the real walk, counted.
//
//   - Locked transactions: keepStore's one (the store's row); one page per
//     source, each a read to the clock that holds no row and is folded with its
//     watermark (three); and keepWalked's one. Five.
//   - Engine reads: the shop row and the verdict (keepStore), and one read
//     to the clock per source. Five. The floor is kept, so it asks nothing.
//
// The wholesale pass (wholesale.go) adds nothing to either, in both cases
// here: a store the pack is off for and kept not enabled, and an enabled
// store whose granted set is the one it was last judged against, with no pass
// under way. Each reads its row's wholesale columns once (wholesalePass's
// wholesaleKept), a read that takes no lock and asks no engine, and then has
// nothing to do: one unlocked read a turn, asserted below. The pack's own two reads are the run's,
// made once for every store before the first turn (rollup.go, "Who is
// wholesale"), and are not a turn's.
func TestAQuietStoresWalkCosts(t *testing.T) {
	for _, c := range []struct {
		name  string
		state wholesaleState
	}{
		{"the pack off", wholesaleState{}},
		{"enabled, its pass done", wholesaleState{Enabled: true, Granted: map[string]bool{buyerEmail: true}, Digest: grantedDigest(map[string]bool{buyerEmail: true})}},
	} {
		t.Run(c.name, func(t *testing.T) {
			i, id := runStore(t)
			answerVerdict(i, verdictReply("recording", time.Now()))
			_, db := statsEngine(t)
			mirrorThreeLines(t, id)
			for range 2 {
				if _, err := run(t, i); err != nil {
					t.Fatal(err)
				}
			}
			if c.state.Enabled {
				exec(t, `UPDATE store_sales_store SET wholesale_enabled = true, wholesale_digest = ?, wholesale_cursor = '' WHERE store_id = ?`, c.state.Digest, id)
			}
			engine := &contextEngine{inner: i.engine}
			i.engine = engine
			verdicts := 0
			answer := i.verdictOf
			i.verdictOf = func(ctx context.Context, storeID string) (storeVerdict, error) {
				verdicts++
				return answer(ctx, storeID)
			}
			queries := storeSalesQueries(t)
			rep := RollupReport{Rows: map[string]int{}}
			if err := i.rollStore(sweepCtx(t), db, storeOf(id, nil), time.Now(), c.state, true, &rep); err != nil {
				t.Fatal(err)
			}
			locks, wholesaleReads := 0, 0
			for _, q := range queries() {
				if strings.HasPrefix(q, "LOCK TABLE") {
					locks++
				}
				if strings.HasPrefix(q, "SELECT") && strings.Contains(q, "wholesale_cursor") {
					wholesaleReads++
				}
			}
			// The engine reads are the shop row and the three reads to the
			// clock; the verdict is asked of verdictOf, once.
			if locks != 5 || engine.asked() != 4 || verdicts != 1 {
				t.Errorf("a quiet store's turn with its walk took %d locked transactions, %d engine reads and %d verdict reads; want 5, 4 and 1",
					locks, engine.asked(), verdicts)
			}
			if wholesaleReads != 1 {
				t.Errorf("the turn read the store's wholesale columns %d times, want once, unlocked", wholesaleReads)
			}
		})
	}
}

// MINOR 4, THE PURE RULE. derive counts a refund line as an item return only
// when it names a live line that is not a gift card; a deleted line's derived
// columns are not written.
func TestDeriveCountsNoItemReturnForADeletedOrGiftCardLine(t *testing.T) {
	line := func(gid string, gift, deleted bool) deriveLine {
		return deriveLine{LineGID: gid, OrderGID: "o", GiftCard: gift, Deleted: deleted, Quantity: 1, CurrentQuantity: 1,
			UnitPrice: "10.00", AllocatedDiscounts: "0", Currency: "EUR", Gross: "0", Discounts: "0"}
	}
	zero := sql.NullString{String: "0", Valid: true}
	d, err := derive(nil,
		[]deriveLine{line("a", false, false), line("g", true, false), line("d", false, true)},
		[]deriveRefund{{RefundGID: "r", OrderGID: "o", DetailFetched: true, MoneyRefunded: "45.00",
			ShippingRefunded: zero, TaxRefunded: zero, DutiesRefunded: zero}},
		[]deriveRefundLine{{RefundGID: "r", LineGID: "a", Quantity: 1, Subtotal: "10.00"},
			{RefundGID: "r", LineGID: "g", Quantity: 1, Subtotal: "25.00"},
			{RefundGID: "r", LineGID: "d", Quantity: 2, Subtotal: "10.00"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.refunds) != 1 {
		t.Fatalf("derive changed %d refunds, want one", len(d.refunds))
	}
	r := d.refunds[0]
	if r.UnitsReturned.Int64 != 1 || r.GiftCardRefunded.String != "25.00" || r.ItemRefunds.String != "20.00" {
		t.Errorf("the refund derives units %d, gift cards %s, items %s; want 1, 25.00 and 20.00",
			r.UnitsReturned.Int64, r.GiftCardRefunded.String, r.ItemRefunds.String)
	}
	for _, l := range d.lines {
		if l.Deleted {
			t.Errorf("derive wrote the deleted line %s", l.LineGID)
		}
	}
}
