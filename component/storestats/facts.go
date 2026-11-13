package storestats

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/znasllc-io/memql/core/money"
	"github.com/znasllc-io/memql/core/num"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// facts.go -- what one mirrored row is as a fact (the 2026-11-11 store
// statistics design, 5.1, 5.2 and 6.2). Pure: a row in, its fact out. The walk
// that reads the rows and the tables the facts are written to are other
// files'.
//
// # A row
//
// A row is what a shape of dsl/shopify/overlay/shapes.memql answers: row.id
// under `id`, row.createdAt under `createdAt`, every other field under its own
// name. A money value is a mirrored money set, an object whose amount is the
// decimal string at shopMoney.amount and whose currency is the code at
// shopMoney.currencyCode. The presentment money beside it, the buyer's
// currency, is never read: every figure is in the shop's currency.
//
// # Three answers
//
// A row is a live fact (rowLive), a tombstone (rowGone, recognised by
// `deleted` true and nothing else) or no fact at all (rowUnreadable). An
// unreadable row is counted and logged by the walk, never guessed at: a
// missing identity or date, an amount money.Parse refuses, an amount in
// another currency than the row's, a quantity that is not a whole number of
// units, or a field of another type than the mirror writes. The error names
// the field and the row's gid and never a value, so no buyer's email, no
// title a person typed and no amount reaches a log.
//
// # The buyer
//
// What an order fact keeps of its buyer is BuyerKey, the Shopify customer's
// gid as the row holds it, empty for a guest; nothing is derived from the
// email. Email is carried in memory only, for the wholesale judgement beside
// the fold (design D9), and is not compared by same: it is never written.

// OrderFact is one mirrored order as the figures read it.
type OrderFact struct {
	StoreID, OrderGID          string
	Version, RecordedAt        time.Time // row.createdAt; syncedAt
	OrderedAt, SourceCreatedAt time.Time
	Currency, BuyerKey         string
	// Cancelled, Test and TaxIncluded are what the order says; which orders
	// count is the derivation's.
	Cancelled, Test, TaxIncluded bool
	Shipping, Tax, Duties        money.Amount
	ExpectedQuantity             int
	// The three Shopify totals the check compares the facts with; nil when the
	// row holds no such object.
	ShopifyCurrentSubtotal, ShopifyTotalDiscounts, ShopifyTotalRefunded *money.Amount
	Email                                                               string // read in memory for the wholesale judgement; never written
}

// LineFact is one mirrored order line.
type LineFact struct {
	StoreID, OrderGID, LineGID    string
	Version                       time.Time
	Currency                      string // of its unit price
	GiftCard                      bool
	ProductKey, Title             string
	Quantity, CurrentQuantity     int
	UnitPrice, AllocatedDiscounts money.Amount
}

// RefundLineFact is one line of a refund, at its position in the refund.
type RefundLineFact struct {
	Position      int
	LineGID       string
	Quantity      int
	Subtotal, Tax money.Amount
}

// RefundFact is one mirrored refund and its lines.
type RefundFact struct {
	StoreID, RefundGID, OrderGID    string
	Version, RecordedAt, RefundedAt time.Time
	MoneyRefunded                   money.Amount
	// DetailFetched says the three inlined lists were read; LinesComplete says
	// refundLineItems held fewer than a page, so no line was left unread.
	DetailFetched, LinesComplete                  bool
	ShippingRefunded, TaxRefunded, DutiesRefunded *money.Amount // nil while the detail is not read
	Lines                                         []RefundLineFact
}

type rowKind int

const (
	rowLive       rowKind = iota // write the fact
	rowGone                      // a tombstone: keep the fact, marked deleted, at this version
	rowUnreadable                // no fact can be made; counted and logged, never guessed
)

// walkedConcepts is THE ONE LIST of the mirror's concepts the figures depend on
// (ruling R1, A34): every generated concept a shape of the walk reads. The
// walk's three sources are its first three entries, and the verdict's reasons
// are asked about the whole list (shopify.VerdictReason.StandsInTheWayOf).
// v1:shopify:store is not on it: it is the engine's own row, not a mirror
// concept, and no reason names it.
var walkedConcepts = []string{"order", "orderLineItem", "refund", "shop"}

// fields is a row as the fact functions read it, one key at a time, so a test
// can see every key they read.
type fields interface{ get(key string) any }

type mapFields map[string]any

func (m mapFields) get(key string) any { return m[key] }

// orderFactOf is the fact of one row of storeStatisticsOrder.
func orderFactOf(row map[string]any) (OrderFact, rowKind, error) {
	return orderFactFrom(mapFields(row))
}

func orderFactFrom(row fields) (OrderFact, rowKind, error) {
	r := newReader(row, "order")
	f := OrderFact{StoreID: r.required("storeId"), OrderGID: r.gid, Version: r.version()}
	gone := r.boolean("deleted")
	f.RecordedAt = r.instant("syncedAt")
	if r.err != nil {
		return OrderFact{}, rowUnreadable, r.err
	}
	if gone {
		return f, rowGone, nil
	}
	f.OrderedAt = r.dated()
	f.SourceCreatedAt = r.instant("sourceCreatedAt")
	f.Currency = r.required("currencyCode")
	r.currency, r.currencyFrom = f.Currency, "currencyCode"
	f.BuyerKey = r.str("customerGid")
	f.Email = r.str("email")
	f.Cancelled = r.notEmpty("cancelledAt")
	f.Test = r.boolean("test")
	f.TaxIncluded = r.boolean("taxesIncluded")
	f.Shipping, _ = r.amount("totalShippingPriceSet", row.get("totalShippingPriceSet"))
	f.Tax, _ = r.amount("totalTaxSet", row.get("totalTaxSet"))
	if f.TaxIncluded {
		// Tax included in the prices is already inside the gross: zero, at the
		// scale Shopify wrote the tax.
		f.Tax = f.Tax.Mul(0)
	}
	f.Duties, _ = r.amount("originalTotalDutiesSet", row.get("originalTotalDutiesSet"))
	f.ExpectedQuantity = r.quantity("subtotalLineItemsQuantity", row.get("subtotalLineItemsQuantity"))
	f.ShopifyCurrentSubtotal = r.optionalAmount("currentSubtotalPriceSet")
	f.ShopifyTotalDiscounts = r.optionalAmount("totalDiscountsSet")
	f.ShopifyTotalRefunded = r.optionalAmount("totalRefundedSet")
	if r.err != nil {
		return OrderFact{}, rowUnreadable, r.err
	}
	return f, rowLive, nil
}

// lineFactOf is the fact of one row of storeStatisticsLine.
func lineFactOf(row map[string]any) (LineFact, rowKind, error) { return lineFactFrom(mapFields(row)) }

func lineFactFrom(row fields) (LineFact, rowKind, error) {
	r := newReader(row, "orderLineItem")
	f := LineFact{StoreID: r.required("storeId"), LineGID: r.gid, Version: r.version()}
	gone := r.boolean("deleted")
	f.OrderGID = r.str("parentGid")
	if r.err != nil {
		return LineFact{}, rowUnreadable, r.err
	}
	if gone {
		return f, rowGone, nil
	}
	if f.OrderGID == "" {
		r.fail("parentGid", "is missing")
	}
	f.GiftCard = r.boolean("isGiftCard")
	f.Title = r.str("title")
	f.ProductKey = r.str("productGid")
	if f.ProductKey == "" {
		f.ProductKey = "title:" + f.Title
	}
	f.Quantity = r.quantity("quantity", row.get("quantity"))
	f.CurrentQuantity = r.quantity("currentQuantity", row.get("currentQuantity"))
	// The unit price is read first: its currency is the line's, and every
	// discount on the line must be in it.
	var priced bool
	f.UnitPrice, priced = r.amount("originalUnitPriceSet", row.get("originalUnitPriceSet"))
	if !priced {
		r.fail("originalUnitPriceSet", "is missing")
	}
	f.Currency = r.currency
	allocations, _ := r.list("discountAllocations")
	for i, v := range allocations {
		field := indexed("discountAllocations", i)
		if m := r.object(field, v); m != nil {
			a, _ := r.amount(field+".allocatedAmountSet", m["allocatedAmountSet"])
			f.AllocatedDiscounts = f.AllocatedDiscounts.Add(a)
		}
	}
	if r.err != nil {
		return LineFact{}, rowUnreadable, r.err
	}
	return f, rowLive, nil
}

// refundFactOf is the fact of one row of storeStatisticsRefund. linesPage is
// the page refundLineItems is read with (refundLinesPage).
func refundFactOf(row map[string]any, linesPage int) (RefundFact, rowKind, error) {
	return refundFactFrom(mapFields(row), linesPage)
}

func refundFactFrom(row fields, linesPage int) (RefundFact, rowKind, error) {
	r := newReader(row, "refund")
	f := RefundFact{StoreID: r.required("storeId"), RefundGID: r.gid, Version: r.version()}
	gone := r.boolean("deleted")
	f.OrderGID = r.str("parentGid")
	f.RecordedAt = r.instant("syncedAt")
	if r.err != nil {
		return RefundFact{}, rowUnreadable, r.err
	}
	if gone {
		return f, rowGone, nil
	}
	if f.OrderGID == "" {
		r.fail("parentGid", "is missing")
	}
	f.RefundedAt = r.dated()
	f.MoneyRefunded, _ = r.amount("totalRefundedSet", row.get("totalRefundedSet"))

	// A shape projects an absent field as nil: a refund whose detail was not
	// read has nil lists, and one read with nothing on it has empty ones.
	items, readItems := r.list("refundLineItems")
	shipping, readShipping := r.list("refundShippingLines")
	adjustments, readAdjustments := r.list("orderAdjustments")
	f.DetailFetched = readItems && readShipping && readAdjustments
	if f.DetailFetched {
		var shipped, tax, duties money.Amount
		for i, v := range items {
			field := indexed("refundLineItems", i)
			m := r.object(field, v)
			if m == nil {
				continue
			}
			l := RefundLineFact{Position: i, LineGID: r.lineItemGID(field+".lineItem", m["lineItem"])}
			l.Quantity = r.quantity(field+".quantity", m["quantity"])
			l.Subtotal, _ = r.amount(field+".subtotalSet", m["subtotalSet"])
			l.Tax, _ = r.amount(field+".totalTaxSet", m["totalTaxSet"])
			tax = tax.Add(l.Tax)
			f.Lines = append(f.Lines, l)
		}
		for i, v := range shipping {
			field := indexed("refundShippingLines", i)
			if m := r.object(field, v); m != nil {
				s, _ := r.amount(field+".subtotalAmountSet", m["subtotalAmountSet"])
				t, _ := r.amount(field+".taxAmountSet", m["taxAmountSet"])
				shipped, tax = shipped.Add(s), tax.Add(t)
			}
		}
		for i, v := range adjustments {
			field := indexed("orderAdjustments", i)
			if m := r.object(field, v); m != nil {
				// With the sign Shopify wrote: a positive tax went back to the buyer.
				t, _ := r.amount(field+".taxAmountSet", m["taxAmountSet"])
				tax = tax.Add(t)
			}
		}
		dutyList, _ := r.list("duties")
		for i, v := range dutyList {
			field := indexed("duties", i)
			if m := r.object(field, v); m != nil {
				d, _ := r.amount(field+".amountSet", m["amountSet"])
				duties = duties.Add(d)
			}
		}
		f.ShippingRefunded, f.TaxRefunded, f.DutiesRefunded = &shipped, &tax, &duties
		f.LinesComplete = linesPage > 0 && len(items) < linesPage
	}
	if r.err != nil {
		return RefundFact{}, rowUnreadable, r.err
	}
	return f, rowLive, nil
}

// refundLinesPage is the page refund.refundLineItems is read with in the
// generated model: a refund holding that many lines may hold more. 0 when the
// model has none.
func refundLinesPage() int {
	spec := generated.Types["refund"]
	if spec == nil {
		return 0
	}
	for _, f := range spec.Fields {
		if f.Name == "refundLineItems" {
			return f.Page
		}
	}
	return 0
}

// soldOf is 5.1 for one line: the units sold are those not removed by an edit,
// min(quantity, currentQuantity + refundedQuantity), a refunded unit being a
// sale and a refund; the gross is the unit price times them; and the discounts
// are the share of the line's allocated discounts that fell on them. The share
// is the one amount this component computes by division, rounded half to even
// at the larger of the scale Shopify wrote and the currency's minor units (a
// discount written "1.0" over two of three units is "0.67"). The quantities
// are non-negative: the readers refuse a negative one.
func soldOf(quantity, currentQuantity, refundedQuantity int, unitPrice, allocated money.Amount, currency string) (sold int, gross, discounts money.Amount) {
	sold = min(quantity, currentQuantity+refundedQuantity)
	gross = unitPrice.Mul(int64(sold))
	switch {
	case quantity == 0:
		discounts = allocated.Mul(0)
	case sold == quantity:
		discounts = allocated
	default:
		// MulRatio's errors are a zero denominator, answered above, and a
		// negative scale, which RoundingScale never gives.
		discounts, _ = allocated.MulRatio(int64(sold), int64(quantity), money.RoundingScale(allocated, currency))
	}
	return sold, gross, discounts
}

// instantOf reads an instant the way the engine hands one over: a time.Time,
// or an RFC 3339 string. A zero time and an empty string are no instant.
func instantOf(v any) (time.Time, bool) {
	switch x := v.(type) {
	case time.Time:
		if x.IsZero() {
			return time.Time{}, false
		}
		return x.UTC(), true
	case string:
		t, err := time.Parse(time.RFC3339Nano, x)
		if err != nil {
			return time.Time{}, false
		}
		return t.UTC(), true
	}
	return time.Time{}, false
}

// quantityOf reads a whole, non-negative number of units, however the row's
// decoder wrote it: an int, an int64, a float64 holding a whole number, or a
// json.Number written as an integer.
func quantityOf(v any) (int, bool) {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int64:
		n = x
	case float64:
		whole, ok := num.WholeInt64(x)
		if !ok {
			return 0, false
		}
		n = whole
	case json.Number:
		parsed, err := x.Int64()
		if err != nil {
			return 0, false
		}
		n = parsed
	default:
		return 0, false
	}
	// A count too large for an int answers -1, and so does a negative one:
	// neither is a number of units.
	q := num.Int64Or(n, -1)
	if q < 0 {
		return 0, false
	}
	return q, true
}

// ---- same ----

// same reports whether two order facts hold the same content: every field the
// fold writes, instants by Equal, amounts by value, a nil amount only with a
// nil one. Email is left out; the check judges wholesale beside it.
func (f OrderFact) same(g OrderFact) bool {
	return f.StoreID == g.StoreID && f.OrderGID == g.OrderGID &&
		f.Version.Equal(g.Version) && f.RecordedAt.Equal(g.RecordedAt) && f.OrderedAt.Equal(g.OrderedAt) && f.SourceCreatedAt.Equal(g.SourceCreatedAt) &&
		f.Currency == g.Currency && f.BuyerKey == g.BuyerKey &&
		f.Cancelled == g.Cancelled && f.Test == g.Test && f.TaxIncluded == g.TaxIncluded &&
		f.Shipping.Equal(g.Shipping) && f.Tax.Equal(g.Tax) && f.Duties.Equal(g.Duties) &&
		f.ExpectedQuantity == g.ExpectedQuantity &&
		sameAmount(f.ShopifyCurrentSubtotal, g.ShopifyCurrentSubtotal) &&
		sameAmount(f.ShopifyTotalDiscounts, g.ShopifyTotalDiscounts) &&
		sameAmount(f.ShopifyTotalRefunded, g.ShopifyTotalRefunded)
}

// same reports whether two line facts hold the same content.
func (f LineFact) same(g LineFact) bool {
	return f.StoreID == g.StoreID && f.OrderGID == g.OrderGID && f.LineGID == g.LineGID &&
		f.Version.Equal(g.Version) && f.Currency == g.Currency && f.GiftCard == g.GiftCard &&
		f.ProductKey == g.ProductKey && f.Title == g.Title &&
		f.Quantity == g.Quantity && f.CurrentQuantity == g.CurrentQuantity &&
		f.UnitPrice.Equal(g.UnitPrice) && f.AllocatedDiscounts.Equal(g.AllocatedDiscounts)
}

// same reports whether two refund facts hold the same content, their lines
// too, in order.
func (f RefundFact) same(g RefundFact) bool {
	if !(f.StoreID == g.StoreID && f.RefundGID == g.RefundGID && f.OrderGID == g.OrderGID &&
		f.Version.Equal(g.Version) && f.RecordedAt.Equal(g.RecordedAt) && f.RefundedAt.Equal(g.RefundedAt) &&
		f.MoneyRefunded.Equal(g.MoneyRefunded) &&
		f.DetailFetched == g.DetailFetched && f.LinesComplete == g.LinesComplete &&
		sameAmount(f.ShippingRefunded, g.ShippingRefunded) &&
		sameAmount(f.TaxRefunded, g.TaxRefunded) &&
		sameAmount(f.DutiesRefunded, g.DutiesRefunded) &&
		len(f.Lines) == len(g.Lines)) {
		return false
	}
	for i, l := range f.Lines {
		m := g.Lines[i]
		if l.Position != m.Position || l.LineGID != m.LineGID || l.Quantity != m.Quantity ||
			!l.Subtotal.Equal(m.Subtotal) || !l.Tax.Equal(m.Tax) {
			return false
		}
	}
	return true
}

func sameAmount(a, b *money.Amount) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// ---- reading a row ----

// reader reads one row and keeps the first thing wrong with it. Every message
// names the concept, the row's gid and the field, and never a value.
type reader struct {
	row     fields
	concept string
	gid     string
	err     error
	// currency is the one currency every amount of the row is in, and
	// currencyFrom the field it was taken from: the order's currencyCode, or
	// the first amount read of a line or a refund.
	currency, currencyFrom string
}

func newReader(row fields, concept string) *reader {
	r := &reader{row: row, concept: concept}
	r.gid = r.required("gid")
	return r
}

func (r *reader) fail(field, what string) {
	if r.err != nil {
		return
	}
	r.err = &rowError{Concept: r.concept, GID: r.gid, Field: field, what: what}
}

// rowError is why a row could not be read: the concept, the row's gid (empty
// when it has none) and the NAME of the field the rules stopped on, each a
// value of its own, so a record of the row keeps the field without cutting it
// out of a sentence. It holds no value of the row.
type rowError struct {
	Concept, GID, Field string
	what                string // a fixed phrase: "is missing", "is not an amount", ...
}

func (e *rowError) Error() string {
	if e.GID == "" {
		return fmt.Sprintf("storestats: %s: %s %s", e.Concept, e.Field, e.what)
	}
	return fmt.Sprintf("storestats: %s %s: %s %s", e.Concept, e.GID, e.Field, e.what)
}

// str is a string field; absent is "".
func (r *reader) str(key string) string {
	switch v := r.row.get(key).(type) {
	case nil:
		return ""
	case string:
		return v
	}
	r.fail(key, "is not a string")
	return ""
}

func (r *reader) required(key string) string {
	s := r.str(key)
	if s == "" {
		r.fail(key, "is missing")
	}
	return s
}

// boolean is a boolean field; absent is false.
func (r *reader) boolean(key string) bool {
	switch v := r.row.get(key).(type) {
	case nil:
		return false
	case bool:
		return v
	}
	r.fail(key, "is not a boolean")
	return false
}

// instant is an instant field; absent (nil or "") is the zero time.
func (r *reader) instant(key string) time.Time {
	v := r.row.get(key)
	if v == nil || v == "" {
		return time.Time{}
	}
	t, ok := instantOf(v)
	if !ok {
		r.fail(key, "is not an RFC 3339 instant")
	}
	return t
}

// version is the row's createdAt, the instant the mirror wrote this version.
func (r *reader) version() time.Time {
	t := r.instant("createdAt")
	if t.IsZero() {
		r.fail("createdAt", "is missing")
	}
	return t
}

// dated is processedAt, else sourceCreatedAt: the business date.
func (r *reader) dated() time.Time {
	if t := r.instant("processedAt"); !t.IsZero() || r.err != nil {
		return t
	}
	t := r.instant("sourceCreatedAt")
	if t.IsZero() {
		r.fail("sourceCreatedAt", "is missing, and so is processedAt")
	}
	return t
}

// notEmpty says a field holds something: a non-empty string or a time.
func (r *reader) notEmpty(key string) bool {
	switch v := r.row.get(key).(type) {
	case nil:
		return false
	case string:
		return v != ""
	case time.Time:
		return !v.IsZero()
	}
	r.fail(key, "is not a string")
	return false
}

func (r *reader) quantity(field string, v any) int {
	if v == nil {
		r.fail(field, "is missing")
		return 0
	}
	q, ok := quantityOf(v)
	if !ok {
		r.fail(field, "is not a whole number of units")
	}
	return q
}

// list is a list field: nil is not read (false), a list is read, empty or not.
func (r *reader) list(key string) ([]any, bool) {
	switch v := r.row.get(key).(type) {
	case nil:
		return nil, false
	case []any:
		return v, true
	case []map[string]any:
		out := make([]any, len(v))
		for i, m := range v {
			out[i] = m
		}
		return out, true
	}
	r.fail(key, "is not a list")
	return nil, false
}

// object is an element or a value that must be an object.
func (r *reader) object(field string, v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	r.fail(field, "is not an object")
	return nil
}

// lineItemGID is a reference an inlined list keeps as {id}.
func (r *reader) lineItemGID(field string, v any) string {
	if v == nil {
		r.fail(field, "is missing")
		return ""
	}
	m := r.object(field, v)
	if m == nil {
		return ""
	}
	id, ok := m["id"].(string)
	if !ok || id == "" {
		r.fail(field+".id", "is missing")
	}
	return id
}

// amount is the shop amount of a money set, and whether the set was there:
// absent is zero. The amount must be a decimal string money.Parse takes, with
// a currency code, in the row's one currency. money.Parse's own error quotes
// the input, so it is not passed on.
func (r *reader) amount(field string, v any) (money.Amount, bool) {
	if v == nil {
		return money.Amount{}, false
	}
	set := r.object(field, v)
	if set == nil {
		return money.Amount{}, false
	}
	shop, ok := set["shopMoney"]
	if !ok || shop == nil {
		r.fail(field+".shopMoney", "is missing")
		return money.Amount{}, false
	}
	m := r.object(field+".shopMoney", shop)
	if m == nil {
		return money.Amount{}, false
	}
	s, ok := m["amount"].(string)
	if !ok {
		r.fail(field+".shopMoney.amount", "is not a string")
		return money.Amount{}, false
	}
	a, err := money.Parse(s)
	if err != nil {
		r.fail(field+".shopMoney.amount", "is not an amount")
		return money.Amount{}, false
	}
	code, ok := m["currencyCode"].(string)
	if !ok || code == "" {
		r.fail(field+".shopMoney.currencyCode", "is missing")
		return money.Amount{}, false
	}
	switch {
	case r.currency == "":
		r.currency, r.currencyFrom = code, field
	case code != r.currency:
		r.fail(field, "is in another currency than "+r.currencyFrom)
		return money.Amount{}, false
	}
	return a, true
}

// optionalAmount is a money set that may be absent: nil then.
func (r *reader) optionalAmount(key string) *money.Amount {
	a, ok := r.amount(key, r.row.get(key))
	if !ok {
		return nil
	}
	return &a
}

func indexed(key string, i int) string { return key + "[" + strconv.Itoa(i) + "]" }
