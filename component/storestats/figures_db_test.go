package storestats

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"
)

// figures_db_test.go -- the figures of builtin storeStatistics (figures.go)
// against a real engine and a real database: a store's facts written by the
// fold, as the walk writes them, and the answer asked through the builtin.
// Every expected amount is written out by hand from the fixture beside it.
// Postgres-gated, through the harness.

// ---- fixtures ----

// madridNow is the clock of most tests here: Saturday 2025-11-08 11:17 in
// Madrid, so the 7 days are Sunday Nov 2 to Saturday Nov 8, local.
var madridNow = time.Date(2025, 11, 8, 10, 17, 0, 0, time.UTC)

var madrid = mustZone("Europe/Madrid")

func mustZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// local is a wall-clock time in a zone.
func local(loc *time.Location, y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, loc)
}

// nov is noon in Madrid on a day of November 2025.
func nov(d int) time.Time { return local(madrid, 2025, 11, d, 12, 0) }

// factVersions numbers every version the fixtures write, so a later write of
// a row is always newer than the one before it.
var factVersions atomic.Int64

func nextVersion() time.Time {
	return foldBase.Add(time.Duration(factVersions.Add(1)) * time.Millisecond)
}

// item is one line of a sale. current defaults to qty; product defaults to a
// product of its own named by the line; a line with no product is "custom".
type item struct {
	gid, product, title string
	qty, current        int
	price, discount     string
	gift, custom        bool
}

// sale is one order and its lines. currency defaults to EUR; shipping and tax
// to none; missing is how many units the order says it holds beyond its lines
// (an order whose lines were not all read).
type sale struct {
	gid                          string
	at                           time.Time
	currency                     string
	shipping, tax                string
	buyer, email                 string
	cancelled, test, taxIncluded bool
	missing                      int
	lines                        []item
}

func (s sale) currencyOr() string {
	if s.currency == "" {
		return "EUR"
	}
	return s.currency
}

func orderRowOfSale(storeID string, s sale) map[string]any {
	quantity := s.missing
	for _, l := range s.lines {
		quantity += l.qty
	}
	cur := s.currencyOr()
	var cancelledAt any
	if s.cancelled {
		cancelledAt = stampOf(s.at.Add(time.Hour))
	}
	row := orderAt(storeID, s.gid, nextVersion(),
		"processedAt", stampOf(s.at), "sourceCreatedAt", stampOf(s.at), "currencyCode", cur,
		"customerGid", s.buyer, "email", s.email, "cancelledAt", cancelledAt, "test", s.test, "taxesIncluded", s.taxIncluded,
		"subtotalLineItemsQuantity", float64(quantity),
		"currentSubtotalPriceSet", nil, "totalDiscountsSet", nil, "totalRefundedSet", nil)
	row["totalShippingPriceSet"], row["totalTaxSet"] = nil, nil
	if s.shipping != "" {
		row["totalShippingPriceSet"] = set(s.shipping, cur)
	}
	if s.tax != "" {
		row["totalTaxSet"] = set(s.tax, cur)
	}
	return row
}

func lineRowOfItem(storeID, orderGID, currency string, l item) map[string]any {
	current := l.current
	if current == 0 && l.qty > 0 {
		current = l.qty
	}
	if current < 0 {
		current = 0
	}
	product := l.product
	if product == "" && !l.custom {
		product = "gid://shopify/Product/" + strings.TrimPrefix(l.gid, "gid://shopify/LineItem/")
	}
	title := l.title
	if title == "" {
		title = "An inner tube"
	}
	allocations := []any{}
	if l.discount != "" {
		allocations = append(allocations, allocation(l.discount, currency))
	}
	return lineAt(storeID, l.gid, orderGID, nextVersion(), "quantity", float64(l.qty), "currentQuantity", float64(current),
		"originalUnitPriceSet", set(l.price, currency), "discountAllocations", allocations, "productGid", product,
		"title", title, "isGiftCard", l.gift)
}

// sell folds the sale's order and then its lines, each page as the walk
// folds it.
func sell(t *testing.T, storeID string, s sale) {
	t.Helper()
	sellAs(t, storeID, wholesaleState{}, s)
}

func sellAs(t *testing.T, storeID string, w wholesaleState, s sale) {
	t.Helper()
	if _, err := foldRows(t, storeID, "order", w, orderRowOfSale(storeID, s)); err != nil {
		t.Fatalf("fold the order %s: %v", s.gid, err)
	}
	if len(s.lines) == 0 {
		return
	}
	rows := make([]map[string]any, 0, len(s.lines))
	for _, l := range s.lines {
		rows = append(rows, lineRowOfItem(storeID, s.gid, s.currencyOr(), l))
	}
	fold(t, storeID, "line", rows...)
}

// returned is one line of a refund.
type returned struct {
	line          string
	qty           int
	subtotal, tax string
}

// refund is one refund of an order. amount is all the money that went back;
// shipping, when set, is a refunded shipping line with shippingTax. notRead
// is a refund whose detail the mirror has not read; fullPage pads its lines
// with empty ones to the page the mirror reads them with.
type refund struct {
	gid, order            string
	at                    time.Time
	currency              string
	amount                string
	lines                 []returned
	shipping, shippingTax string
	notRead, fullPage     bool
}

func refundRowOfSpec(storeID string, r refund) map[string]any {
	cur := r.currency
	if cur == "" {
		cur = "EUR"
	}
	row := refundAt(storeID, r.gid, r.order, nextVersion(), "processedAt", stampOf(r.at), "sourceCreatedAt", stampOf(r.at),
		"totalRefundedSet", set(r.amount, cur))
	if r.notRead {
		row["refundLineItems"], row["refundShippingLines"], row["orderAdjustments"], row["duties"] = nil, nil, nil, nil
		return row
	}
	lines := []any{}
	for _, l := range r.lines {
		lines = append(lines, map[string]any{"lineItem": map[string]any{"id": l.line}, "quantity": float64(l.qty),
			"subtotalSet": set(l.subtotal, cur), "totalTaxSet": set(l.tax, cur)})
	}
	if r.fullPage {
		for len(lines) < refundLinesPage() {
			lines = append(lines, map[string]any{"lineItem": map[string]any{"id": "gid://shopify/LineItem/999"}, "quantity": float64(0),
				"subtotalSet": set("0.00", cur), "totalTaxSet": set("0.00", cur)})
		}
	}
	shipping := []any{}
	if r.shipping != "" {
		shipping = append(shipping, map[string]any{"subtotalAmountSet": set(r.shipping, cur), "taxAmountSet": set(r.shippingTax, cur)})
	}
	row["refundLineItems"], row["refundShippingLines"], row["orderAdjustments"], row["duties"] = lines, shipping, []any{}, []any{}
	return row
}

func refundOf(t *testing.T, storeID string, r refund) {
	t.Helper()
	fold(t, storeID, "refund", refundRowOfSpec(storeID, r))
}

// retire folds a tombstone of the row.
func retire(t *testing.T, storeID, source, gid string) {
	t.Helper()
	fold(t, storeID, source, tombstone(storeID, gid, nextVersion()))
}

func orderGID(n int) string  { return fmt.Sprintf("gid://shopify/Order/%d", n) }
func lineGID(n int) string   { return fmt.Sprintf("gid://shopify/LineItem/%d", n) }
func refundGID(n int) string { return fmt.Sprintf("gid://shopify/Refund/%d", n) }

// one is a sale of one unit of one line at a price, at an instant.
func one(n int, at time.Time, price string) sale {
	return sale{gid: orderGID(n), at: at, lines: []item{{gid: lineGID(n), qty: 1, price: price}}}
}

// ---- reading the answer ----

func js(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(raw)
}

// figuresIn is the answer's figures, or a previous period's.
func figuresIn(t *testing.T, answer map[string]any, key string) map[string]any {
	t.Helper()
	f, ok := answer[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %v, want an object", key, answer[key])
	}
	return f
}

// wantValues compares each key's JSON: "81.53" is money, 2 is a count, nil is
// null, so a zero is never taken for a null nor a count for an amount.
func wantValues(t *testing.T, label string, got map[string]any, want map[string]any) {
	t.Helper()
	for k, w := range want {
		v, present := got[k]
		if !present {
			t.Errorf("%s: no %s", label, k)
			continue
		}
		if js(v) != js(w) {
			t.Errorf("%s: %s = %s, want %s", label, k, js(v), js(w))
		}
	}
}

func seriesOf(t *testing.T, answer map[string]any) []map[string]any {
	t.Helper()
	raw, ok := answer["series"].([]any)
	if !ok {
		t.Fatalf("series = %v, want a list", answer["series"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

// bucketOf is the bucket whose start is at, written as the answer writes it.
func bucketOf(t *testing.T, answer map[string]any, at string) map[string]any {
	t.Helper()
	for _, b := range seriesOf(t, answer) {
		if b["at"] == at {
			return b
		}
	}
	t.Fatalf("no bucket at %s in %s", at, js(answer["series"]))
	return nil
}

var sevenFigures = []string{"totalSales", "netSales", "orders", "grossSales", "discounts", "refunds", "unitsSold"}

func wantNullBucket(t *testing.T, label string, b map[string]any) {
	t.Helper()
	for _, k := range sevenFigures {
		if v, present := b[k]; !present || v != nil {
			t.Errorf("%s: bucket %v %s = %s, want null", label, b["at"], k, js(v))
		}
	}
}

// measuredZeros is the seven figures of a bucket in which nothing happened.
var measuredZeros = map[string]any{"totalSales": "0", "netSales": "0", "orders": 0, "grossSales": "0", "discounts": "0",
	"refunds": "0", "unitsSold": 0}

// ask is the 7 days of the store at madridNow, as its owner asks.
func ask(t *testing.T, storeID, siteID string) map[string]any {
	t.Helper()
	atNow(t, madridNow)
	return mustAsk(t, ownerCtx(), siteID, storeID, "7d")
}

// ---- the figures ----

// theWeek writes the brief's week: order A on Nov 5, order B on Nov 6, and a
// refund of A on Nov 7.
func theWeek(t *testing.T, storeID string) {
	t.Helper()
	sell(t, storeID, sale{gid: orderGID(1), at: nov(5), shipping: "5.00", tax: "21.00", buyer: "gid://shopify/Customer/1",
		lines: []item{
			{gid: lineGID(11), qty: 2, current: 1, price: "40.00", discount: "8.00", title: "A city bike bell"},
			{gid: lineGID(12), qty: 1, price: "19.99", title: "An inner tube"},
		}})
	sell(t, storeID, sale{gid: orderGID(2), at: nov(6), tax: "2.10", buyer: "gid://shopify/Customer/2",
		lines: []item{{gid: lineGID(21), qty: 1, price: "10.00", title: "A tyre lever"}}})
	refundOf(t, storeID, refund{gid: refundGID(1), order: orderGID(1), at: nov(7), amount: "48.56",
		lines: []returned{{line: lineGID(11), qty: 1, subtotal: "36.00", tax: "7.56"}}, shipping: "5.00", shippingTax: "0.00"})
}

func TestAWeekOfSalesReadsToTheCent(t *testing.T) {
	storeID, site := salesStore(t, nil)
	theWeek(t, storeID)
	got := ask(t, storeID, site)
	// The average is 101.99 / 2 = 50.995, a tie, to the even cent: 51.00.
	wantValues(t, "the week", figuresIn(t, got, "figures"), map[string]any{"totalSales": "81.53", "netSales": "65.99",
		"orders": 2, "averageOrderValue": "51.00", "grossSales": "109.99", "discounts": "8.00", "refunds": "36.00", "unitsSold": 3})
	// Order A's day: 2 x 40.00 less 8.00 and 19.99, its shipping and tax.
	wantValues(t, "Nov 5", bucketOf(t, got, "2025-11-05T00:00:00+01:00"), map[string]any{"orders": 1, "grossSales": "99.99",
		"discounts": "8.00", "refunds": "0", "netSales": "91.99", "totalSales": "117.99", "unitsSold": 3})
	wantValues(t, "Nov 6", bucketOf(t, got, "2025-11-06T00:00:00+01:00"), map[string]any{"orders": 1, "grossSales": "10.00",
		"discounts": "0", "netSales": "10.00", "totalSales": "12.10", "unitsSold": 1})
	// The refund's day: below zero, never absent.
	wantValues(t, "Nov 7", bucketOf(t, got, "2025-11-07T00:00:00+01:00"), map[string]any{"orders": 0, "grossSales": "0",
		"refunds": "36.00", "netSales": "-36.00", "totalSales": "-48.56", "unitsSold": -1})
	for _, day := range []string{"02", "03", "04", "08"} {
		wantValues(t, "Nov "+day, bucketOf(t, got, "2025-11-"+day+"T00:00:00+01:00"), measuredZeros)
	}
	wantValues(t, "the week's counts", got, map[string]any{"incompleteOrders": 0, "incompleteRefunds": 0, "refundsNotRead": 0,
		"refundsWithoutItem": "0.00", "floors": map[string]any{}})
}

func TestTheAverageDividesByTheOrdersInTheStoresCurrency(t *testing.T) {
	storeID, site := salesStore(t, nil)
	sell(t, storeID, one(1, nov(3), "30.00"))
	sell(t, storeID, one(2, nov(4), "50.00"))
	usd := one(3, nov(4), "999.00")
	usd.currency = "USD"
	sell(t, storeID, usd)
	got := ask(t, storeID, site)
	wantValues(t, "two EUR and one USD", figuresIn(t, got, "figures"), map[string]any{"orders": 3, "grossSales": "80.00",
		"averageOrderValue": "40.00", "unitsSold": 3})
	wantValues(t, "other currencies", got, map[string]any{"otherCurrencies": []any{map[string]any{"currency": "USD", "orders": 1}}})
}

func TestTheAverageIsAnsweredRoundedAtTheCurrencysDecimals(t *testing.T) {
	for _, c := range []struct {
		currency string
		prices   [3]string
		want     string
	}{
		{"EUR", [3]string{"33.00", "33.00", "34.00"}, "33.33"},
		{"JPY", [3]string{"33.0", "33.0", "34.0"}, "33"},
		{"KWD", [3]string{"0.333", "0.333", "0.334"}, "0.333"},
	} {
		storeID, site := salesStore(t, map[string]any{"currency": c.currency})
		for n, price := range c.prices {
			s := one(n+1, nov(3+n), price)
			s.currency = c.currency
			sell(t, storeID, s)
		}
		wantValues(t, c.currency, figuresIn(t, ask(t, storeID, site), "figures"), map[string]any{"orders": 3, "averageOrderValue": c.want})
	}
}

func TestTheAverageIsNullWithNoOrderInTheStoresCurrency(t *testing.T) {
	// A period holding only a refund, of an order of October 20.
	storeID, site := salesStore(t, nil)
	sell(t, storeID, one(1, local(madrid, 2025, 10, 20, 12, 0), "30.00"))
	refundOf(t, storeID, refund{gid: refundGID(1), order: orderGID(1), at: nov(4), amount: "30.00",
		lines: []returned{{line: lineGID(1), qty: 1, subtotal: "30.00", tax: "0.00"}}})
	wantValues(t, "only a refund", figuresIn(t, ask(t, storeID, site), "figures"), map[string]any{"orders": 0, "averageOrderValue": nil,
		"grossSales": "0", "refunds": "30.00", "netSales": "-30.00", "totalSales": "-30.00", "unitsSold": -1})

	// A period holding only an order in another currency.
	other, otherSite := salesStore(t, nil)
	usd := one(1, nov(4), "20.00")
	usd.currency = "USD"
	sell(t, other, usd)
	wantValues(t, "only a USD order", figuresIn(t, ask(t, other, otherSite), "figures"), map[string]any{"orders": 1,
		"averageOrderValue": nil, "grossSales": "0", "unitsSold": 1})
}

func TestOtherCurrenciesAreListedAndNotSummed(t *testing.T) {
	storeID, site := salesStore(t, nil)
	sell(t, storeID, one(1, nov(3), "10.00"))
	for n, cur := range []string{"USD", "GBP", "USD"} {
		s := one(2+n, nov(4), "500.00")
		s.currency, s.shipping, s.tax = cur, "9.00", "9.00"
		sell(t, storeID, s)
	}
	got := ask(t, storeID, site)
	wantValues(t, "EUR, USD and GBP", figuresIn(t, got, "figures"), map[string]any{"orders": 4, "grossSales": "10.00",
		"netSales": "10.00", "totalSales": "10.00", "averageOrderValue": "10.00", "unitsSold": 4})
	wantValues(t, "the list", got, map[string]any{"otherCurrencies": []any{
		map[string]any{"currency": "USD", "orders": 2}, map[string]any{"currency": "GBP", "orders": 1}}})
	wantValues(t, "Nov 4", bucketOf(t, got, "2025-11-04T00:00:00+01:00"), map[string]any{"orders": 3, "grossSales": "0", "unitsSold": 3})
}

func TestAStoreWithNoOrdersReadsMeasuredZeros(t *testing.T) {
	storeID, site := salesStore(t, nil)
	got := ask(t, storeID, site)
	wantValues(t, "no order", figuresIn(t, got, "figures"), map[string]any{"orders": 0, "grossSales": "0", "netSales": "0",
		"totalSales": "0", "discounts": "0", "refunds": "0", "unitsSold": 0, "averageOrderValue": nil})
	for _, b := range seriesOf(t, got) {
		wantValues(t, "a quiet day", b, measuredZeros)
	}
	wantValues(t, "the lists", got, map[string]any{"productsBySales": []any{}, "productsByUnits": []any{}, "productCount": 0,
		"otherCurrencies": []any{}, "buyers": map[string]any{"new": 0, "returning": 0, "unknown": 0}, "refundsWithoutItem": "0",
		"incompleteOrders": 0, "incompleteRefunds": 0, "refundsNotRead": 0})
}

func TestADeletedOrderIsInNoFigure(t *testing.T) {
	storeID, site := salesStore(t, nil)
	sell(t, storeID, sale{gid: orderGID(1), at: nov(3), buyer: "gid://shopify/Customer/9", shipping: "5.00",
		lines: []item{{gid: lineGID(1), qty: 2, price: "40.00", title: "A city bike"}}})
	refundOf(t, storeID, refund{gid: refundGID(1), order: orderGID(1), at: nov(4), amount: "40.00",
		lines: []returned{{line: lineGID(1), qty: 1, subtotal: "40.00", tax: "0.00"}}})
	sell(t, storeID, sale{gid: orderGID(2), at: nov(5), lines: []item{{gid: lineGID(2), qty: 1, price: "12.00", title: "A bell"}}})
	retire(t, storeID, "order", orderGID(1))

	got := ask(t, storeID, site)
	wantValues(t, "the live order alone", figuresIn(t, got, "figures"), map[string]any{"orders": 1, "grossSales": "12.00",
		"refunds": "0", "netSales": "12.00", "totalSales": "12.00", "unitsSold": 1, "averageOrderValue": "12.00"})
	wantValues(t, "Nov 3", bucketOf(t, got, "2025-11-03T00:00:00+01:00"), measuredZeros)
	wantValues(t, "Nov 4", bucketOf(t, got, "2025-11-04T00:00:00+01:00"), measuredZeros)
	wantValues(t, "the lists", got, map[string]any{
		"productsBySales": []any{map[string]any{"key": "gid://shopify/Product/2", "title": "A bell", "value": "12.00"}},
		"productsByUnits": []any{map[string]any{"key": "gid://shopify/Product/2", "title": "A bell", "value": 1}},
		"productCount":    1, "buyers": map[string]any{"new": 0, "returning": 0, "unknown": 1}, "refundsWithoutItem": "0"})
}

func TestADeletedLineAndADeletedRefundAreInNoFigure(t *testing.T) {
	storeID, site := salesStore(t, nil)
	sell(t, storeID, sale{gid: orderGID(1), at: nov(3), lines: []item{
		{gid: lineGID(1), qty: 1, price: "40.00", title: "A city bike"},
		{gid: lineGID(2), qty: 1, price: "7.00", title: "A bell"},
	}})
	refundOf(t, storeID, refund{gid: refundGID(1), order: orderGID(1), at: nov(4), amount: "7.00",
		lines: []returned{{line: lineGID(2), qty: 1, subtotal: "7.00", tax: "0.00"}}})
	retire(t, storeID, "line", lineGID(1))
	retire(t, storeID, "refund", refundGID(1))

	got := ask(t, storeID, site)
	wantValues(t, "the live line alone", figuresIn(t, got, "figures"), map[string]any{"orders": 1, "grossSales": "7.00",
		"refunds": "0", "netSales": "7.00", "unitsSold": 1})
	wantValues(t, "Nov 4", bucketOf(t, got, "2025-11-04T00:00:00+01:00"), measuredZeros)
	wantValues(t, "the lists", got, map[string]any{
		"productsByUnits": []any{map[string]any{"key": "gid://shopify/Product/2", "title": "A bell", "value": 1}}, "productCount": 1})
}

func TestASaleLandsOnItsStoreDayWestAndEastOfUTC(t *testing.T) {
	at := time.Date(2025, 11, 8, 3, 0, 0, 0, time.UTC)
	for _, c := range []struct{ zone, day, other string }{
		{"America/Los_Angeles", "2025-11-07T00:00:00-08:00", "2025-11-08T00:00:00-08:00"},
		{"Asia/Kolkata", "2025-11-08T00:00:00+05:30", "2025-11-07T00:00:00+05:30"},
	} {
		storeID, site := salesStore(t, map[string]any{"timezone": c.zone})
		sell(t, storeID, one(1, at, "10.00"))
		got := ask(t, storeID, site)
		wantValues(t, c.zone, bucketOf(t, got, c.day), map[string]any{"orders": 1, "grossSales": "10.00"})
		wantValues(t, c.zone+", the next day over", bucketOf(t, got, c.other), map[string]any{"orders": 0})
	}
}

func TestSalesOnADaylightSavingDayLandInThatDay(t *testing.T) {
	santiago := mustZone("America/Santiago")
	// Clocks in Santiago skip from 00:00 to 01:00 on 2023-09-03: that day
	// begins at 01:00, at -03:00.
	storeID, site := salesStore(t, map[string]any{"timezone": "America/Santiago", "currency": "CLP",
		"history_since": time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)})
	for n, at := range []time.Time{
		local(santiago, 2023, 9, 3, 1, 30),
		local(santiago, 2023, 9, 3, 23, 30),
		local(santiago, 2023, 9, 2, 23, 30),
	} {
		s := one(n+1, at, "1000")
		s.currency = "CLP"
		sell(t, storeID, s)
	}
	atNow(t, time.Date(2023, 9, 5, 15, 0, 0, 0, time.UTC))
	got := mustAsk(t, ownerCtx(), site, storeID, "7d")
	wantValues(t, "Sep 3", bucketOf(t, got, "2023-09-03T01:00:00-03:00"), map[string]any{"orders": 2, "grossSales": "2000"})
	wantValues(t, "Sep 2", bucketOf(t, got, "2023-09-02T00:00:00-04:00"), map[string]any{"orders": 1, "grossSales": "1000"})
	wantValues(t, "Sep 4", bucketOf(t, got, "2023-09-04T00:00:00-03:00"), map[string]any{"orders": 0})
}

func TestBothOfARepeatedHoursBucketsHoldTheirOwnSales(t *testing.T) {
	// New York's clocks go back from 02:00 to 01:00 at 06:00Z on 2025-11-02.
	storeID, site := salesStore(t, map[string]any{"timezone": "America/New_York", "currency": "USD"})
	for n, at := range []time.Time{time.Date(2025, 11, 2, 5, 30, 0, 0, time.UTC), time.Date(2025, 11, 2, 6, 30, 0, 0, time.UTC)} {
		s := one(n+1, at, "10.00")
		s.currency = "USD"
		sell(t, storeID, s)
	}
	atNow(t, time.Date(2025, 11, 2, 12, 10, 0, 0, time.UTC))
	got := mustAsk(t, ownerCtx(), site, storeID, "24h")
	wantValues(t, "the first 01:00", bucketOf(t, got, "2025-11-02T01:00:00-04:00"), map[string]any{"orders": 1, "grossSales": "10.00"})
	wantValues(t, "the second 01:00", bucketOf(t, got, "2025-11-02T01:00:00-05:00"), map[string]any{"orders": 1, "grossSales": "10.00"})
	wantValues(t, "the day", figuresIn(t, got, "figures"), map[string]any{"orders": 2, "grossSales": "20.00"})
}

func TestThePreviousPeriodIsNullWhenItStartsBeforeHistory(t *testing.T) {
	// The previous 7 days are Oct 26 to Nov 1.
	comparable, site := salesStore(t, nil)
	sell(t, comparable, one(1, local(madrid, 2025, 10, 28, 12, 0), "25.00"))
	sell(t, comparable, one(2, nov(3), "10.00"))
	got := ask(t, comparable, site)
	wantValues(t, "the previous week", figuresIn(t, got, "previous"), map[string]any{"orders": 1, "grossSales": "25.00",
		"netSales": "25.00", "totalSales": "25.00", "refunds": "0", "discounts": "0", "unitsSold": 1, "averageOrderValue": "25.00"})

	// History from Oct 30: the previous week is not all recorded.
	late, lateSite := salesStore(t, map[string]any{"history_since": local(madrid, 2025, 10, 30, 0, 0)})
	sell(t, late, one(1, nov(3), "10.00"))
	if v := ask(t, late, lateSite)["previous"]; v != nil {
		t.Errorf("history from Oct 30: previous = %s, want null", js(v))
	}
	// Twelve months have no previous period.
	if v := mustAsk(t, ownerCtx(), site, comparable, "12m")["previous"]; v != nil {
		t.Errorf("12m: previous = %s, want null", js(v))
	}
}

// TestAPeriodCutShortReadsNoPreviousPeriod: a frozen week of days summed only
// to its asOf is not compared against a whole previous week (ruling 44).
func TestAPeriodCutShortReadsNoPreviousPeriod(t *testing.T) {
	storeID, site := salesStore(t, map[string]any{"verdict_state": "paused", "recording_blocked": true,
		"last_clear_at": time.Date(2025, 11, 6, 11, 0, 0, 0, time.UTC)})
	sell(t, storeID, one(1, local(madrid, 2025, 10, 28, 12, 0), "25.00"))
	sell(t, storeID, one(2, nov(3), "10.00"))
	if v := ask(t, storeID, site)["previous"]; v != nil {
		t.Errorf("paused: previous = %s, want null", js(v))
	}
	storeFrame(t, storeID, recordingFrame(nil))
	wantValues(t, "recording again", figuresIn(t, ask(t, storeID, site), "previous"), map[string]any{"orders": 1, "grossSales": "25.00"})
}

func TestAnIncompleteOrderMakesFloors(t *testing.T) {
	storeID, site := salesStore(t, nil)
	s := one(1, nov(3), "10.00")
	s.missing = 2
	sell(t, storeID, s)
	sell(t, storeID, one(2, nov(4), "5.00"))
	got := ask(t, storeID, site)
	wantValues(t, "floors", got, map[string]any{"incompleteOrders": 1, "floors": map[string]any{"grossSales": true, "discounts": true,
		"netSales": true, "totalSales": true, "averageOrderValue": true, "unitsSold": true}})
	wantValues(t, "still summed", figuresIn(t, got, "figures"), map[string]any{"orders": 2, "grossSales": "15.00", "unitsSold": 2})
}

func TestARefundNotYetReadBlanksTheFourFiguresThatSubtractIt(t *testing.T) {
	storeID, site := salesStore(t, nil)
	sell(t, storeID, one(1, nov(3), "40.00"))
	sell(t, storeID, one(2, nov(4), "10.00"))
	refundOf(t, storeID, refund{gid: refundGID(1), order: orderGID(1), at: nov(5), amount: "40.00", notRead: true})
	got := ask(t, storeID, site)
	wantValues(t, "D22", figuresIn(t, got, "figures"), map[string]any{"refunds": nil, "netSales": nil, "totalSales": nil,
		"unitsSold": nil, "orders": 2, "grossSales": "50.00", "discounts": "0", "averageOrderValue": "25.00"})
	wantValues(t, "the count", got, map[string]any{"refundsNotRead": 1, "incompleteRefunds": 0, "refundsWithoutItem": nil,
		"productsBySales": nil, "productsByUnits": nil})
	wantValues(t, "the refund's day", bucketOf(t, got, "2025-11-05T00:00:00+01:00"), map[string]any{"refunds": nil, "netSales": nil,
		"totalSales": nil, "unitsSold": nil, "orders": 0, "grossSales": "0"})
	wantValues(t, "another day", bucketOf(t, got, "2025-11-04T00:00:00+01:00"), map[string]any{"refunds": "0", "netSales": "10.00",
		"totalSales": "10.00", "unitsSold": 1})
}

func TestARefundWithAFullPageOfLinesBlanksUnitsSold(t *testing.T) {
	// Enabled for wholesale, so the split is answered beside the rest.
	storeID, site := salesStore(t, map[string]any{"wholesale_enabled": true})
	sell(t, storeID, sale{gid: orderGID(1), at: nov(3), lines: []item{{gid: lineGID(1), qty: 2, price: "40.00"}}})
	refundOf(t, storeID, refund{gid: refundGID(1), order: orderGID(1), at: nov(5), amount: "40.00",
		lines: []returned{{line: lineGID(1), qty: 1, subtotal: "40.00", tax: "0.00"}}, fullPage: true})
	got := ask(t, storeID, site)
	wantValues(t, "a full page", figuresIn(t, got, "figures"), map[string]any{"unitsSold": nil, "refunds": "40.00",
		"netSales": "40.00", "grossSales": "80.00", "orders": 1})
	wantValues(t, "the count", got, map[string]any{"incompleteRefunds": 1, "refundsNotRead": 0, "productsByUnits": nil})
	wantValues(t, "the refund's day", bucketOf(t, got, "2025-11-05T00:00:00+01:00"), map[string]any{"unitsSold": nil, "refunds": "40.00"})
	// The money of the lines past the page has left its product: neither the
	// products by sales nor the refunds that name no product are known. The
	// money figures, summed from the refund's own amounts, are.
	wantValues(t, "the product money", got, map[string]any{"productsBySales": nil, "refundsWithoutItem": nil})
	wantValues(t, "the money figures", figuresIn(t, got, "figures"), map[string]any{"discounts": "0", "totalSales": "40.00",
		"averageOrderValue": "80.00"})
	// The wholesale split is summed from the orders' and refunds' own
	// amounts too, and stays measured: the order is retail.
	wantValues(t, "the wholesale split", got, map[string]any{"wholesale": map[string]any{
		"wholesale": map[string]any{"orders": 0, "netSales": "0"}, "retail": map[string]any{"orders": 1, "netSales": "40.00"}}})

	// 80.00 went back and only 40.00 of it is on the lines recorded.
	other, otherSite := salesStore(t, nil)
	sell(t, other, sale{gid: orderGID(1), at: nov(3), lines: []item{{gid: lineGID(1), qty: 2, price: "40.00"}}})
	refundOf(t, other, refund{gid: refundGID(1), order: orderGID(1), at: nov(5), amount: "80.00",
		lines: []returned{{line: lineGID(1), qty: 1, subtotal: "40.00", tax: "0.00"}}, fullPage: true})
	got = ask(t, other, otherSite)
	wantValues(t, "80.00 back, 40.00 on its lines", got, map[string]any{"productsBySales": nil, "refundsWithoutItem": nil,
		"productsByUnits": nil, "productCount": nil, "incompleteRefunds": 1})
	wantValues(t, "80.00 back, the money figures", figuresIn(t, got, "figures"), map[string]any{"refunds": "80.00",
		"netSales": "0.00", "totalSales": "0.00", "grossSales": "80.00", "unitsSold": nil})
}

// productTitles is each product's title in the list by units, by key.
func productTitles(t *testing.T, answer map[string]any) map[string]string {
	t.Helper()
	list, ok := answer["productsByUnits"].([]any)
	if !ok {
		t.Fatalf("productsByUnits = %s, want a list", js(answer["productsByUnits"]))
	}
	out := map[string]string{}
	for _, e := range list {
		p := e.(map[string]any)
		out[p["key"].(string)] = p["title"].(string)
	}
	return out
}

// A PRODUCT IS TITLED BY ITS NEWEST LINE SOLD IN THE PERIOD; one only refunded
// in it, by the line of its latest refund, which need not be its newest
// refunded line.
func TestAProductIsTitledByItsNewestLineSoldInThePeriod(t *testing.T) {
	storeID, site := salesStore(t, nil)
	oct := func(d int) time.Time { return local(madrid, 2025, 10, d, 12, 0) }
	const renamed, refundedOnly, twice = "gid://shopify/Product/801", "gid://shopify/Product/802", "gid://shopify/Product/803"
	const refundedOutOfTurn = "gid://shopify/Product/804"
	// Renamed: sold Oct 20 as "Old name", sold Nov 3 as "New name", and the
	// Oct 20 line refunded Nov 5.
	sell(t, storeID, sale{gid: orderGID(1), at: oct(20), lines: []item{{gid: lineGID(1), product: renamed, qty: 1, price: "5.00", title: "Old name"}}})
	sell(t, storeID, sale{gid: orderGID(2), at: nov(3), lines: []item{{gid: lineGID(2), product: renamed, qty: 1, price: "5.00", title: "New name"}}})
	refundOf(t, storeID, refund{gid: refundGID(1), order: orderGID(1), at: nov(5), amount: "5.00",
		lines: []returned{{line: lineGID(1), qty: 1, subtotal: "5.00", tax: "0.00"}}})
	// Only refunded in the period: lines of Oct 20 and Oct 21, refunded Nov 4
	// and Nov 5.
	sell(t, storeID, sale{gid: orderGID(3), at: oct(20), lines: []item{{gid: lineGID(3), product: refundedOnly, qty: 1, price: "3.00", title: "A tube, first"}}})
	sell(t, storeID, sale{gid: orderGID(4), at: oct(21), lines: []item{{gid: lineGID(4), product: refundedOnly, qty: 1, price: "3.00", title: "A tube, second"}}})
	refundOf(t, storeID, refund{gid: refundGID(2), order: orderGID(3), at: nov(4), amount: "3.00",
		lines: []returned{{line: lineGID(3), qty: 1, subtotal: "3.00", tax: "0.00"}}})
	refundOf(t, storeID, refund{gid: refundGID(3), order: orderGID(4), at: nov(5), amount: "3.00",
		lines: []returned{{line: lineGID(4), qty: 1, subtotal: "3.00", tax: "0.00"}}})
	// Only refunded in the period, out of turn: the newer line, of Oct 21, is
	// refunded first, Nov 4, and the older, of Oct 20, last, Nov 5. Titled by
	// the line of the latest refund, the older one, not by the newest line.
	sell(t, storeID, sale{gid: orderGID(7), at: oct(20), lines: []item{{gid: lineGID(7), product: refundedOutOfTurn, qty: 1, price: "4.00", title: "A cup, older"}}})
	sell(t, storeID, sale{gid: orderGID(8), at: oct(21), lines: []item{{gid: lineGID(8), product: refundedOutOfTurn, qty: 1, price: "4.00", title: "A cup, newer"}}})
	refundOf(t, storeID, refund{gid: refundGID(4), order: orderGID(8), at: nov(4), amount: "4.00",
		lines: []returned{{line: lineGID(8), qty: 1, subtotal: "4.00", tax: "0.00"}}})
	refundOf(t, storeID, refund{gid: refundGID(5), order: orderGID(7), at: nov(5), amount: "4.00",
		lines: []returned{{line: lineGID(7), qty: 1, subtotal: "4.00", tax: "0.00"}}})
	// Sold twice in the period under two titles.
	sell(t, storeID, sale{gid: orderGID(5), at: nov(3), lines: []item{{gid: lineGID(5), product: twice, qty: 1, price: "2.00", title: "A bell"}}})
	sell(t, storeID, sale{gid: orderGID(6), at: nov(4), lines: []item{{gid: lineGID(6), product: twice, qty: 1, price: "2.00", title: "A brass bell"}}})

	titles := productTitles(t, ask(t, storeID, site))
	for key, want := range map[string]string{renamed: "New name", refundedOnly: "A tube, second", twice: "A brass bell",
		refundedOutOfTurn: "A cup, older"} {
		if titles[key] != want {
			t.Errorf("product %s is titled %q, want %q", key, titles[key], want)
		}
	}
}

// THE NULL RULES HOLD INSIDE THE PREVIOUS PERIOD, and leave the period's own
// figures alone.
func TestThePreviousPeriodKeepsItsOwnNullRules(t *testing.T) {
	prevDay := local(madrid, 2025, 10, 29, 12, 0)
	storeID, site := salesStore(t, nil)
	sell(t, storeID, one(1, local(madrid, 2025, 10, 27, 12, 0), "40.00"))
	refundOf(t, storeID, refund{gid: refundGID(1), order: orderGID(1), at: prevDay, amount: "40.00", notRead: true})
	sell(t, storeID, one(2, nov(3), "10.00"))
	got := ask(t, storeID, site)
	wantValues(t, "a refund not read last week", figuresIn(t, got, "previous"), map[string]any{"refunds": nil, "netSales": nil,
		"totalSales": nil, "unitsSold": nil, "orders": 1, "grossSales": "40.00"})
	wantValues(t, "this week", figuresIn(t, got, "figures"), map[string]any{"refunds": "0", "netSales": "10.00", "totalSales": "10.00", "unitsSold": 1})
	wantValues(t, "this week's counts", got, map[string]any{"refundsNotRead": 0, "productsBySales": []any{
		map[string]any{"key": "gid://shopify/Product/2", "title": "An inner tube", "value": "10.00"}}})

	incomplete, incompleteSite := salesStore(t, nil)
	s := one(1, prevDay, "40.00")
	s.missing = 1
	sell(t, incomplete, s)
	sell(t, incomplete, one(2, nov(3), "10.00"))
	got = ask(t, incomplete, incompleteSite)
	wantValues(t, "an incomplete order last week", got, map[string]any{"floors": map[string]any{}, "incompleteOrders": 0})
	wantValues(t, "last week", figuresIn(t, got, "previous"), map[string]any{"orders": 1, "grossSales": "40.00"})
}

// THE PREVIOUS PERIOD ENDS WHERE ITS BOUNDS SAY, read from PrevEnd and never
// taken to be the period's start.
func TestThePreviousPeriodEndsAtItsOwnEnd(t *testing.T) {
	storeID, _ := salesStore(t, nil)
	sell(t, storeID, one(1, local(madrid, 2025, 10, 28, 12, 0), "25.00"))
	sell(t, storeID, one(2, local(madrid, 2025, 10, 31, 12, 0), "7.00"))
	sell(t, storeID, one(3, nov(3), "10.00"))
	_, db := statsEngine(t)
	var stats Statistics
	if err := readTx(context.Background(), db, func(ctx context.Context, tx bun.Tx) error {
		var err error
		if stats, err = readFrame(ctx, tx, statisticsRequest{StoreID: storeID, Period: "7d"}, madridNow); err != nil {
			return err
		}
		// A previous period that ends a day and a half before this one starts.
		stats.Bounds.PrevEnd = local(madrid, 2025, 10, 30, 0, 0)
		return readFigures(ctx, tx, &stats)
	}); err != nil {
		t.Fatal(err)
	}
	if stats.Previous == nil || stats.Previous.Orders == nil || *stats.Previous.Orders != 1 || stats.Previous.GrossSales.String() != "25.00" {
		t.Errorf("previous = %+v, want the Oct 28 order alone: the period ends at PrevEnd", stats.Previous)
	}
	if stats.Figures.Orders == nil || *stats.Figures.Orders != 1 {
		t.Errorf("figures = %+v, want this week's one order", stats.Figures)
	}
}

// A PREVIOUS PERIOD THAT RUNS PAST THE PERIOD'S START is read on its own, to
// its own end. The bounds core/statsperiod makes never do this; bounds made
// by hand can.
func TestAPreviousPeriodPastThePeriodsStartIsReadToItsOwnEnd(t *testing.T) {
	storeID, _ := salesStore(t, nil)
	sell(t, storeID, one(1, local(madrid, 2025, 10, 28, 12, 0), "25.00"))
	sell(t, storeID, one(2, local(madrid, 2025, 10, 31, 12, 0), "7.00"))
	sell(t, storeID, one(3, nov(3), "10.00"))
	_, db := statsEngine(t)
	var stats Statistics
	if err := readTx(context.Background(), db, func(ctx context.Context, tx bun.Tx) error {
		var err error
		if stats, err = readFrame(ctx, tx, statisticsRequest{StoreID: storeID, Period: "7d"}, madridNow); err != nil {
			return err
		}
		// A previous period that ends two days after this one starts.
		stats.Bounds.PrevEnd = local(madrid, 2025, 11, 4, 0, 0)
		return readFigures(ctx, tx, &stats)
	}); err != nil {
		t.Fatal(err)
	}
	if stats.Previous == nil || stats.Previous.Orders == nil || *stats.Previous.Orders != 3 || stats.Previous.GrossSales.String() != "42.00" {
		t.Errorf("previous = %+v, want the three orders to Nov 4: the period ends at PrevEnd", stats.Previous)
	}
	if stats.Figures.Orders == nil || *stats.Figures.Orders != 1 {
		t.Errorf("figures = %+v, want this week's one order", stats.Figures)
	}
}

func TestBuyersAreNewReturningOrUnknown(t *testing.T) {
	storeID, site := salesStore(t, nil)
	returning, fresh, cancelledFirst := "gid://shopify/Customer/1", "gid://shopify/Customer/2", "gid://shopify/Customer/3"
	for n, s := range []sale{
		{at: local(madrid, 2025, 10, 1, 12, 0), buyer: returning},
		{at: nov(3), buyer: returning},
		{at: nov(4), buyer: returning},
		{at: nov(5), buyer: fresh},
		{at: local(madrid, 2025, 10, 1, 12, 0), buyer: cancelledFirst, cancelled: true},
		{at: nov(5), buyer: cancelledFirst},
		{at: nov(6)},
	} {
		s.gid = orderGID(n + 1)
		s.lines = []item{{gid: lineGID(n + 1), qty: 1, price: "5.00"}}
		sell(t, storeID, s)
	}
	wantValues(t, "buyers", ask(t, storeID, site), map[string]any{"buyers": map[string]any{"new": 2, "returning": 1, "unknown": 1}})
}

func TestWholesaleIsNullUnlessTheStoreIsEnabledAndARefundIsOnItsOrdersSide(t *testing.T) {
	granted := wholesaleState{Enabled: true, Granted: map[string]bool{foldEmail("buyer-1@example.test"): true}}
	write := func(storeID string) {
		w := one(1, nov(3), "100.00")
		w.email = "buyer-1@example.test"
		sellAs(t, storeID, granted, w)
		r := one(2, nov(4), "50.00")
		r.email = "buyer-2@example.test"
		sellAs(t, storeID, granted, r)
		refundOf(t, storeID, refund{gid: refundGID(1), order: orderGID(1), at: nov(5), amount: "20.00",
			lines: []returned{{line: lineGID(1), qty: 0, subtotal: "20.00", tax: "0.00"}}})
	}
	off, offSite := salesStore(t, nil)
	write(off)
	if v := ask(t, off, offSite)["wholesale"]; v != nil {
		t.Errorf("a store not enabled: wholesale = %s, want null", js(v))
	}
	on, onSite := salesStore(t, map[string]any{"wholesale_enabled": true})
	write(on)
	wantValues(t, "an enabled store", ask(t, on, onSite), map[string]any{"wholesale": map[string]any{
		"wholesale": map[string]any{"orders": 1, "netSales": "80.00"}, "retail": map[string]any{"orders": 1, "netSales": "50.00"}}})
}

func TestACancelledOrderAndItsRefundLeaveEveryFigure(t *testing.T) {
	storeID, site := salesStore(t, nil)
	// Cancelled in this period, refunded in it.
	c := one(1, nov(3), "40.00")
	c.cancelled = true
	sell(t, storeID, c)
	refundOf(t, storeID, refund{gid: refundGID(1), order: orderGID(1), at: nov(4), amount: "40.00",
		lines: []returned{{line: lineGID(1), qty: 1, subtotal: "40.00", tax: "0.00"}}})
	// Placed in the previous period and cancelled, refunded in this one.
	p := one(2, local(madrid, 2025, 10, 28, 12, 0), "30.00")
	p.cancelled = true
	sell(t, storeID, p)
	refundOf(t, storeID, refund{gid: refundGID(2), order: orderGID(2), at: nov(5), amount: "30.00",
		lines: []returned{{line: lineGID(2), qty: 1, subtotal: "30.00", tax: "0.00"}}})
	sell(t, storeID, one(3, nov(6), "10.00"))
	got := ask(t, storeID, site)
	wantValues(t, "this week", figuresIn(t, got, "figures"), map[string]any{"orders": 1, "grossSales": "10.00", "refunds": "0",
		"netSales": "10.00", "unitsSold": 1})
	wantValues(t, "the previous week", figuresIn(t, got, "previous"), map[string]any{"orders": 0, "grossSales": "0"})
	wantValues(t, "the lists", got, map[string]any{"productCount": 1, "buyers": map[string]any{"new": 0, "returning": 0, "unknown": 1}})
}

func TestAShippingRefundLowersTotalSalesAndNotNetSales(t *testing.T) {
	storeID, site := salesStore(t, nil)
	s := one(1, nov(3), "40.00")
	s.shipping = "5.00"
	sell(t, storeID, s)
	refundOf(t, storeID, refund{gid: refundGID(1), order: orderGID(1), at: nov(4), amount: "5.00", shipping: "5.00", shippingTax: "0.00"})
	wantValues(t, "a shipping refund", figuresIn(t, ask(t, storeID, site), "figures"), map[string]any{"refunds": "0.00",
		"netSales": "40.00", "totalSales": "40.00", "unitsSold": 1})
}

func TestAnEditThatRemovesAUnitChangesTheOrdersOwnDayAndNoRefund(t *testing.T) {
	storeID, site := salesStore(t, nil)
	sell(t, storeID, sale{gid: orderGID(1), at: nov(3), lines: []item{{gid: lineGID(1), qty: 2, price: "40.00", discount: "8.00"}}})
	// Edited on Nov 5: one of the two units removed, with no refund.
	sell(t, storeID, sale{gid: orderGID(1), at: nov(3), lines: []item{{gid: lineGID(1), qty: 2, current: 1, price: "40.00", discount: "8.00"}}})
	got := ask(t, storeID, site)
	wantValues(t, "the week", figuresIn(t, got, "figures"), map[string]any{"grossSales": "40.00", "discounts": "4.00",
		"refunds": "0", "netSales": "36.00", "unitsSold": 1})
	wantValues(t, "the order's day", bucketOf(t, got, "2025-11-03T00:00:00+01:00"), map[string]any{"grossSales": "40.00", "unitsSold": 1})
	wantValues(t, "the edit's day", bucketOf(t, got, "2025-11-05T00:00:00+01:00"), measuredZeros)
}

func TestATestOrderCountsOnlyOnADevelopmentStore(t *testing.T) {
	for _, development := range []bool{false, true} {
		storeID, site := salesStore(t, map[string]any{"development": development})
		sell(t, storeID, one(1, nov(3), "10.00"))
		test := one(2, nov(4), "20.00")
		test.test = true
		sell(t, storeID, test)
		want := map[string]any{"orders": 1, "grossSales": "10.00"}
		if development {
			want = map[string]any{"orders": 2, "grossSales": "30.00"}
		}
		wantValues(t, fmt.Sprintf("development %v", development), figuresIn(t, ask(t, storeID, site), "figures"), want)
	}
}

func TestFiguresStopAtAsOfAndStartAtHistory(t *testing.T) {
	clear := time.Date(2025, 11, 6, 11, 0, 0, 0, time.UTC) // Nov 6, 12:00 in Madrid
	orderCheck := []any{map[string]any{"code": "check_failing", "sentence": "Orders are not checked."}}
	write := func(storeID string) {
		sell(t, storeID, one(1, nov(5), "10.00"))
		sell(t, storeID, one(2, local(madrid, 2025, 11, 6, 9, 0), "3.00"))
		sell(t, storeID, one(3, nov(7), "20.00")) // after the clear run
	}
	for label, frame := range map[string]map[string]any{
		"paused":               {"verdict_state": "paused", "recording_blocked": true, "last_clear_at": clear},
		"an order check fails": {"verdict_state": "needs attention", "verdict_reasons": orderCheck, "recording_blocked": true, "last_clear_at": clear},
	} {
		storeID, site := salesStore(t, frame)
		write(storeID)
		got := ask(t, storeID, site)
		wantValues(t, label, figuresIn(t, got, "figures"), map[string]any{"orders": 2, "grossSales": "13.00"})
		wantValues(t, label+", the day of the clear run", bucketOf(t, got, "2025-11-06T00:00:00+01:00"), map[string]any{"orders": 1, "grossSales": "3.00"})
		// After the clear run: null, the day with a sale and the day with none.
		wantNullBucket(t, label, bucketOf(t, got, "2025-11-07T00:00:00+01:00"))
		wantNullBucket(t, label, bucketOf(t, got, "2025-11-08T00:00:00+01:00"))
	}

	// A reason about products stands in the way of nothing.
	products := []any{map[string]any{"code": "check_failing", "sentence": "Products are not checked."}}
	storeID, site := salesStore(t, map[string]any{"verdict_state": "needs attention", "verdict_reasons": products})
	write(storeID)
	got := ask(t, storeID, site)
	wantValues(t, "a reason about products", figuresIn(t, got, "figures"), map[string]any{"orders": 3, "grossSales": "33.00"})
	for _, b := range seriesOf(t, got) {
		if b["orders"] == nil {
			t.Errorf("a reason about products: bucket %v is null", b["at"])
		}
	}

	// History from Nov 4: the days before it are null.
	storeID, site = salesStore(t, map[string]any{"history_since": local(madrid, 2025, 11, 4, 0, 0)})
	write(storeID)
	got = ask(t, storeID, site)
	wantNullBucket(t, "before history", bucketOf(t, got, "2025-11-02T00:00:00+01:00"))
	wantNullBucket(t, "before history", bucketOf(t, got, "2025-11-03T00:00:00+01:00"))
	wantValues(t, "history's first day", bucketOf(t, got, "2025-11-04T00:00:00+01:00"), measuredZeros)
	wantValues(t, "from history", figuresIn(t, got, "figures"), map[string]any{"orders": 3})

	// No figure at all.
	for label, frame := range map[string]map[string]any{
		"starting":                      {"verdict_state": "starting"},
		"rebuilding":                    {"rebuilding_since": clear},
		"purging":                       {"purging": true},
		"not walked through":            {"walked_through": nil},
		"blocked with no clear run":     {"verdict_state": "needs attention", "verdict_reasons": orderCheck, "recording_blocked": true, "last_clear_at": nil},
		"paused before the period":      {"verdict_state": "paused", "recording_blocked": true, "last_clear_at": time.Date(2025, 10, 30, 0, 0, 0, 0, time.UTC)},
		"paused at the period's start":  {"verdict_state": "paused", "recording_blocked": true, "last_clear_at": local(madrid, 2025, 11, 2, 0, 0)},
		"not connected, no clear run":   {"verdict_state": "not connected", "recording_blocked": true, "last_clear_at": nil},
		"rebuilding while blocked":      {"rebuilding_since": clear, "recording_blocked": true},
		"starting, with a clear run":    {"verdict_state": "starting", "last_clear_at": clear},
		"purging, recording_blocked no": {"purging": true, "recording_blocked": false},
	} {
		// Written while recording: a purging store takes no fold.
		storeID, site := salesStore(t, nil)
		write(storeID)
		storeFrame(t, storeID, recordingFrame(frame))
		got := ask(t, storeID, site)
		assertEightNulls(t, label, got)
		for _, b := range seriesOf(t, got) {
			wantNullBucket(t, label, b)
		}
		wantValues(t, label, got, map[string]any{"previous": nil, "productsBySales": nil, "productsByUnits": nil, "productCount": nil,
			"buyers": nil, "wholesale": nil, "refundsWithoutItem": nil, "incompleteOrders": nil, "incompleteRefunds": nil,
			"refundsNotRead": nil, "otherCurrencies": nil, "floors": map[string]any{}})
	}

	// A state not known is answered with its figures, and its state "".
	storeID, site = salesStore(t, map[string]any{"verdict_state": ""})
	write(storeID)
	got = ask(t, storeID, site)
	wantValues(t, "a state not known", figuresIn(t, got, "figures"), map[string]any{"orders": 3})
	if r := recordingOf(t, got); r["state"] != "" {
		t.Errorf("a state not known: state = %v, want \"\"", r["state"])
	}
}

func TestAStoreWithNoZoneAnswersNoFigureAndOneWithNoCurrencyNoMoney(t *testing.T) {
	for label, zone := range map[string]string{"no zone": "", "a zone that does not load": "Mars/Olympus_Mons"} {
		storeID, site := salesStore(t, map[string]any{"timezone": zone})
		sell(t, storeID, one(1, nov(5), "10.00"))
		got := ask(t, storeID, site)
		assertEightNulls(t, label, got)
		for _, b := range seriesOf(t, got) {
			wantNullBucket(t, label, b)
		}
		wantValues(t, label, got, map[string]any{"previous": nil, "productsBySales": nil, "productsByUnits": nil, "buyers": nil,
			"otherCurrencies": nil, "productCount": nil})
	}

	storeID, site := salesStore(t, map[string]any{"currency": ""})
	sell(t, storeID, one(1, nov(5), "10.00"))
	got := ask(t, storeID, site)
	wantValues(t, "no currency", figuresIn(t, got, "figures"), map[string]any{"orders": 1, "unitsSold": 1, "grossSales": nil,
		"discounts": nil, "refunds": nil, "netSales": nil, "totalSales": nil, "averageOrderValue": nil})
	wantValues(t, "no currency, the order's day", bucketOf(t, got, "2025-11-05T00:00:00+01:00"), map[string]any{"orders": 1,
		"unitsSold": 1, "grossSales": nil, "netSales": nil, "totalSales": nil, "refunds": nil, "discounts": nil})
	wantValues(t, "no currency, the lists", got, map[string]any{"productsBySales": nil, "refundsWithoutItem": nil, "otherCurrencies": nil,
		"productsByUnits": []any{map[string]any{"key": "gid://shopify/Product/1", "title": "An inner tube", "value": 1}}})
	wantValues(t, "no currency, the previous week", figuresIn(t, got, "previous"), map[string]any{"orders": 0, "grossSales": nil})
}

func TestTheLinesOfARetiredOrderCountNowhere(t *testing.T) {
	storeID, site := salesStore(t, nil)
	sell(t, storeID, sale{gid: orderGID(1), at: nov(3), lines: []item{{gid: lineGID(1), qty: 3, price: "9.00", title: "A bell"}}})
	retire(t, storeID, "order", orderGID(1))
	if facts := lineFactsOf(t, storeID); len(facts) != 1 {
		t.Fatalf("the retired order's line has %d facts, want its one live fact: %v", len(facts), facts)
	}
	got := ask(t, storeID, site)
	wantValues(t, "a retired order's lines", figuresIn(t, got, "figures"), map[string]any{"orders": 0, "grossSales": "0", "unitsSold": 0})
	wantValues(t, "the lists", got, map[string]any{"productsBySales": []any{}, "productsByUnits": []any{}, "productCount": 0})
}

func TestTopProductsCarryTwentyFiveRowsAndSayHowManyThereAre(t *testing.T) {
	storeID, site := salesStore(t, nil)
	// Thirty products: product n sold n units at 1.00, on Nov 3.
	lines := []item{}
	for n := 1; n <= 30; n++ {
		lines = append(lines, item{gid: lineGID(n), qty: n, price: "1.00", title: fmt.Sprintf("Spoke set %02d", n)})
	}
	sell(t, storeID, sale{gid: orderGID(1), at: nov(3), lines: lines})
	// Product 30 again, newer and renamed; and a custom line, grouped by its title.
	sell(t, storeID, sale{gid: orderGID(2), at: nov(4), lines: []item{
		{gid: lineGID(31), product: "gid://shopify/Product/30", qty: 1, price: "1.00", title: "Spoke set 30, black"},
		{gid: lineGID(32), custom: true, qty: 1, price: "100.00", title: "Wheel truing"},
	}})
	// Two units of product 29 returned, and an amount that names no line.
	refundOf(t, storeID, refund{gid: refundGID(1), order: orderGID(1), at: nov(5), amount: "2.00",
		lines: []returned{{line: lineGID(29), qty: 2, subtotal: "2.00", tax: "0.00"}}})
	refundOf(t, storeID, refund{gid: refundGID(2), order: orderGID(1), at: nov(5), amount: "12.00"})

	got := ask(t, storeID, site)
	wantValues(t, "the counts", got, map[string]any{"productCount": 31, "refundsWithoutItem": "12.00"})
	bySales, _ := got["productsBySales"].([]any)
	byUnits, _ := got["productsByUnits"].([]any)
	if len(bySales) != 25 || len(byUnits) != 25 {
		t.Fatalf("the lists hold %d and %d rows, want 25 each", len(bySales), len(byUnits))
	}
	wantValues(t, "first by sales", bySales[0].(map[string]any), map[string]any{"key": "title:Wheel truing", "title": "Wheel truing", "value": "100.00"})
	wantValues(t, "second by sales", bySales[1].(map[string]any), map[string]any{"key": "gid://shopify/Product/30", "title": "Spoke set 30, black", "value": "31.00"})
	wantValues(t, "third by sales", bySales[2].(map[string]any), map[string]any{"key": "gid://shopify/Product/28", "value": "28.00"})
	// Product 29 less its two units returned ties product 27: by title.
	wantValues(t, "fourth by sales", bySales[3].(map[string]any), map[string]any{"key": "gid://shopify/Product/27", "value": "27.00"})
	wantValues(t, "fifth by sales", bySales[4].(map[string]any), map[string]any{"key": "gid://shopify/Product/29", "value": "27.00"})
	wantValues(t, "first by units", byUnits[0].(map[string]any), map[string]any{"key": "gid://shopify/Product/30", "value": 31})
	wantValues(t, "second by units", byUnits[1].(map[string]any), map[string]any{"key": "gid://shopify/Product/28", "value": 28})
	wantValues(t, "third by units", byUnits[2].(map[string]any), map[string]any{"key": "gid://shopify/Product/27", "value": 27})
	wantValues(t, "fourth by units", byUnits[3].(map[string]any), map[string]any{"key": "gid://shopify/Product/29", "value": 27})
	wantValues(t, "last by units", byUnits[24].(map[string]any), map[string]any{"key": "gid://shopify/Product/6", "value": 6})
}

func TestAReadAfterAFactWriteOnTheSameNodeSeesIt(t *testing.T) {
	storeID, site := salesStore(t, nil)
	sell(t, storeID, one(1, nov(3), "10.00"))
	wantValues(t, "before", figuresIn(t, ask(t, storeID, site), "figures"), map[string]any{"orders": 1})
	sell(t, storeID, one(2, nov(4), "10.00"))
	wantValues(t, "after", figuresIn(t, ask(t, storeID, site), "figures"), map[string]any{"orders": 2})
}

func TestTheAnswerHoldsNoEmail(t *testing.T) {
	storeID, site := salesStore(t, map[string]any{"wholesale_enabled": true})
	email := "buyer-1@example.test"
	buyer := "gid://shopify/Customer/" + uniq()
	s := one(1, nov(3), "10.00")
	s.email, s.buyer = email, buyer
	sellAs(t, storeID, wholesaleState{Enabled: true, Granted: map[string]bool{foldEmail(email): true}}, s)
	unreadable(t, storeID, 1)
	raw := js(ask(t, storeID, site))
	for _, secret := range []string{email, "buyer-1", buyer} {
		if strings.Contains(raw, secret) {
			t.Errorf("the answer holds %q: %s", secret, raw)
		}
	}
}

// unreadable writes n records of rows of the store the fold could not read,
// as the fold records them.
func unreadable(t *testing.T, storeID string, n int) {
	t.Helper()
	_, db := statsEngine(t)
	for i := 0; i < n; i++ {
		if _, err := db.ExecContext(context.Background(), `INSERT INTO store_sales_unreadable
			(store_id, source, row_id, gid, version, field, recorded_at) VALUES (?, 'order', ?, ?, ?, 'totalTaxSet', ?)`,
			storeID, fmt.Sprintf("row-%d-%s", i, uniq()), orderGID(900+i), nextVersion(), nextVersion()); err != nil {
			t.Fatal(err)
		}
	}
}

func unreadableReasons(t *testing.T, answer map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range recordingOf(t, answer)["reasons"].([]any) {
		if reason := r.(map[string]any); reason["code"] == "statistics_rows_unreadable" {
			out = append(out, reason)
		}
	}
	return out
}

// TestAStoreWithUnreadableRowsReadsAReason: rows the fold could not read are
// said, never left as a silently smaller figure (ruling 38).
func TestAStoreWithUnreadableRowsReadsAReason(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{
		{0, ""},
		{1, "1 record of this store could not be read and is left out of these figures."},
		{3, "3 records of this store could not be read and are left out of these figures."},
	} {
		storeID, site := salesStore(t, nil)
		sell(t, storeID, one(1, nov(3), "10.00"))
		unreadable(t, storeID, c.n)
		got := ask(t, storeID, site)
		reasons := unreadableReasons(t, got)
		switch {
		case c.want == "" && len(reasons) != 0:
			t.Errorf("no record: reasons %s, want none", js(reasons))
		case c.want != "" && (len(reasons) != 1 || reasons[0]["sentence"] != c.want || len(reasons[0]) != 2):
			t.Errorf("%d records: reasons %s, want one saying %q", c.n, js(reasons), c.want)
		}
		// It blocks nothing: the current store keeps its figures.
		wantValues(t, fmt.Sprintf("%d records", c.n), got, map[string]any{"asOf": nil})
		wantValues(t, fmt.Sprintf("%d records", c.n), figuresIn(t, got, "figures"), map[string]any{"orders": 1})
		if r := recordingOf(t, got); r["blocked"] != false || r["state"] != "recording" {
			t.Errorf("%d records: recording = %s, want recording and not blocked", c.n, js(r))
		}
	}

	// In any state, and the count follows a record that is cleared.
	storeID, site := salesStore(t, map[string]any{"verdict_state": "starting"})
	unreadable(t, storeID, 2)
	if reasons := unreadableReasons(t, ask(t, storeID, site)); len(reasons) != 1 {
		t.Errorf("a starting store: reasons %s, want the one", js(reasons))
	}
	_, db := statsEngine(t)
	if _, err := db.ExecContext(context.Background(), `DELETE FROM store_sales_unreadable WHERE store_id = ? AND row_id IN
		(SELECT row_id FROM store_sales_unreadable WHERE store_id = ? LIMIT 1)`, storeID, storeID); err != nil {
		t.Fatal(err)
	}
	if reasons := unreadableReasons(t, ask(t, storeID, site)); len(reasons) != 1 ||
		reasons[0]["sentence"] != "1 record of this store could not be read and is left out of these figures." {
		t.Errorf("one record cleared: reasons %s, want the singular", js(reasons))
	}
}
