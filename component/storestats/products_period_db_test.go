package storestats

import (
	"testing"
	"time"
)

func TestProductPeriodFollowsItsParentOrder(t *testing.T) {
	storeID, site := salesStore(t, nil)
	s := one(1, nov(3), "12.00")
	line := s.lines[0]
	fold(t, storeID, "line", lineRowOfItem(storeID, s.gid, s.currencyOr(), line))
	if got := lineFactsOf(t, storeID)[line.gid]["order_ordered_at"]; got != nil {
		t.Fatalf("a line folded before its order has date %v, want none", got)
	}
	dateIs := func(want time.Time) {
		t.Helper()
		raw, _ := lineFactsOf(t, storeID)[line.gid]["order_ordered_at"].(string)
		got, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil || !got.Equal(want) {
			t.Fatalf("line's order date = %v, want %v", got, want)
		}
	}
	fold(t, storeID, "order", orderRowOfSale(storeID, s))
	dateIs(nov(3))
	wantValues(t, "the order arrives in the period", ask(t, storeID, site), map[string]any{"productCount": 1})

	// A newer parent version moves the sale out of the requested week without
	// requiring the unchanged line to be read again.
	s.at = local(madrid, 2025, 10, 20, 12, 0)
	fold(t, storeID, "order", orderRowOfSale(storeID, s))
	dateIs(s.at)
	wantValues(t, "the corrected order is outside the period", ask(t, storeID, site), map[string]any{"productCount": 0})

	// Returns belong to the refund's period even when the sale was earlier.
	refundOf(t, storeID, refund{gid: refundGID(1), order: s.gid, at: nov(5), amount: "12.00",
		lines: []returned{{line: line.gid, qty: 1, subtotal: "12.00", tax: "0.00"}}})
	wantValues(t, "the earlier sale's return is in this period", ask(t, storeID, site), map[string]any{
		"productCount": 1, "productsBySales": []any{map[string]any{"key": "gid://shopify/Product/1", "title": "An inner tube", "value": "-12.00"}}})
	s.cancelled = true
	fold(t, storeID, "order", orderRowOfSale(storeID, s))
	wantValues(t, "a cancelled parent contributes no product", ask(t, storeID, site), map[string]any{"productCount": 0})
	s.cancelled, s.at = false, nov(3)
	fold(t, storeID, "order", orderRowOfSale(storeID, s))
	dateIs(s.at)
	wantValues(t, "the parent returns to the period", ask(t, storeID, site), map[string]any{"productCount": 1})
	fold(t, storeID, "order", tombstone(storeID, s.gid, nextVersion()))
	if got := lineFactsOf(t, storeID)[line.gid]["order_ordered_at"]; got != nil {
		t.Fatalf("a line of a deleted parent has date %v, want none", got)
	}
	wantValues(t, "a deleted parent contributes no product", ask(t, storeID, site), map[string]any{"productCount": 0})
}
