package storestats

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/money"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// ---- fixtures ----

// buyerEmail and customTitle are the free text a row carries that no error may
// repeat.
const (
	buyerEmail  = "buyer@example.com"
	customTitle = "Engraved for Ada"
)

// set is a mirrored money set. The presentment amount is always different from
// the shop amount and in another currency, so a fact that read it would show it.
func set(amount, currency string) map[string]any {
	return map[string]any{
		"shopMoney":        map[string]any{"amount": amount, "currencyCode": currency},
		"presentmentMoney": map[string]any{"amount": "999.99", "currencyCode": "USD"},
	}
}

func orderRow() map[string]any {
	return map[string]any{
		"id":                        "v1:shopify:order:o1",
		"createdAt":                 "2026-11-01T10:00:00.123456Z",
		"storeId":                   "store-a",
		"gid":                       "gid://shopify/Order/1001",
		"syncedAt":                  "2026-11-01T10:00:01Z",
		"deleted":                   false,
		"processedAt":               "2026-10-31T23:30:00Z",
		"sourceCreatedAt":           "2026-10-31T23:00:00Z",
		"cancelledAt":               nil,
		"test":                      false,
		"taxesIncluded":             false,
		"currencyCode":              "EUR",
		"customerGid":               "gid://shopify/Customer/77",
		"email":                     buyerEmail,
		"subtotalLineItemsQuantity": float64(3),
		"totalShippingPriceSet":     set("12.50", "EUR"),
		"totalTaxSet":               set("4.20", "EUR"),
		"currentSubtotalPriceSet":   set("80.00", "EUR"),
		"totalDiscountsSet":         set("8.00", "EUR"),
		"totalRefundedSet":          set("0.00", "EUR"),
	}
}

func allocation(amount, currency string) map[string]any {
	return map[string]any{"allocatedAmountSet": set(amount, currency)}
}

func lineRow() map[string]any {
	return map[string]any{
		"id":                   "v1:shopify:orderLineItem:l1",
		"createdAt":            "2026-11-01T10:00:00Z",
		"storeId":              "store-a",
		"gid":                  "gid://shopify/LineItem/501",
		"parentGid":            "gid://shopify/Order/1001",
		"deleted":              false,
		"isGiftCard":           false,
		"productGid":           "gid://shopify/Product/9",
		"title":                "Inner tube",
		"quantity":             float64(3),
		"currentQuantity":      float64(2),
		"originalUnitPriceSet": set("40.00", "EUR"),
		"discountAllocations":  []any{allocation("5.00", "EUR"), allocation("3.00", "EUR")},
	}
}

func refundLine(lineGID string, quantity any, subtotal, tax string) map[string]any {
	return map[string]any{
		"lineItem":    map[string]any{"id": lineGID},
		"quantity":    quantity,
		"subtotalSet": set(subtotal, "EUR"),
		"totalTaxSet": set(tax, "EUR"),
	}
}

func refundRow() map[string]any {
	return map[string]any{
		"id":                  "v1:shopify:refund:r1",
		"createdAt":           "2026-11-03T08:00:00Z",
		"storeId":             "store-a",
		"gid":                 "gid://shopify/Refund/700",
		"parentGid":           "gid://shopify/Order/1001",
		"syncedAt":            "2026-11-03T08:00:02Z",
		"deleted":             false,
		"processedAt":         "2026-11-02T15:00:00Z",
		"sourceCreatedAt":     "2026-11-02T14:00:00Z",
		"totalRefundedSet":    set("48.56", "EUR"),
		"duties":              []any{},
		"refundLineItems":     []any{refundLine("gid://shopify/LineItem/501", float64(1), "36.00", "7.56")},
		"refundShippingLines": []any{map[string]any{"subtotalAmountSet": set("5.00", "EUR"), "taxAmountSet": set("0.00", "EUR")}},
		"orderAdjustments":    []any{},
	}
}

func mustInstant(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func amt(t *testing.T, s string) money.Amount {
	t.Helper()
	a, err := money.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// wantAmount compares by the printed string, so the scale is pinned too.
func wantAmount(t *testing.T, what string, got money.Amount, want string) {
	t.Helper()
	if got.String() != want {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

func wantPtr(t *testing.T, what string, got *money.Amount, want string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %s", what, want)
		return
	}
	wantAmount(t, what, *got, want)
}

// wantUnreadable holds that a row is rowUnreadable with an error naming the
// field and holding none of the forbidden text.
func wantUnreadable(t *testing.T, kind rowKind, err error, field string, forbidden ...string) {
	t.Helper()
	if kind != rowUnreadable {
		t.Errorf("kind = %d, want rowUnreadable", kind)
	}
	if err == nil {
		t.Fatalf("no error, want one naming %s", field)
	}
	if !strings.Contains(err.Error(), field) {
		t.Errorf("error %q does not name %s", err, field)
	}
	for _, f := range forbidden {
		if strings.Contains(err.Error(), f) {
			t.Errorf("error %q holds %q", err, f)
		}
	}
}

// ---- 5.1 ----

func TestSoldOfIsFivePointOne(t *testing.T) {
	for _, c := range []struct {
		q0, q1, qr       int
		price, allocated string
		currency         string
		sold             int
		gross, discounts string
	}{
		{2, 2, 0, "40.00", "8.00", "EUR", 2, "80.00", "8.00"},
		{3, 1, 1, "10.00", "3.00", "EUR", 2, "20.00", "2.00"}, // one refunded, one removed by an edit
		{3, 2, 0, "9.99", "1.00", "EUR", 2, "19.98", "0.67"},
		{3, 2, 0, "10.0", "1.0", "EUR", 2, "20.0", "0.67"}, // three units, two kept, a discount written with one decimal: cents, not "0.7"
		{3, 2, 0, "1000", "100", "JPY", 2, "2000", "67"},   // no decimals to round at
		{3, 2, 0, "1.000", "0.100", "KWD", 2, "2.000", "0.067"},
		{2, 1, 0, "5.00", "0.05", "EUR", 1, "5.00", "0.02"},  // half to even
		{2, 1, 0, "5.00", "0.07", "EUR", 1, "5.00", "0.04"},  // half to even, the other way
		{2, 0, 2, "5.00", "1.00", "EUR", 2, "10.00", "1.00"}, // refunded in full: still sold
		{2, 0, 0, "5.00", "1.00", "EUR", 0, "0.00", "0.00"},  // removed by an edit
		{0, 0, 0, "5.00", "1.00", "EUR", 0, "0.00", "0.00"},  // no units: nothing is divided, and no discount is counted
		{1, 1, 1, "5.00", "0.00", "EUR", 1, "5.00", "0.00"},  // never more than were ordered
		{4, 1, 2, "2.50", "1.00", "EUR", 3, "7.50", "0.75"},  // kept and refunded are both sold
	} {
		name := fmt.Sprintf("%d/%d/%d %s %s %s", c.q0, c.q1, c.qr, c.price, c.allocated, c.currency)
		sold, gross, discounts := soldOf(c.q0, c.q1, c.qr, amt(t, c.price), amt(t, c.allocated), c.currency)
		if sold != c.sold || gross.String() != c.gross || discounts.String() != c.discounts {
			t.Errorf("%s: soldOf = %d, %s, %s; want %d, %s, %s", name, sold, gross, discounts, c.sold, c.gross, c.discounts)
		}
	}
}

// ---- orders ----

func TestAnOrderRowBecomesItsFact(t *testing.T) {
	f, kind, err := orderFactOf(orderRow())
	if err != nil || kind != rowLive {
		t.Fatalf("orderFactOf = %d, %v; want rowLive", kind, err)
	}
	if f.StoreID != "store-a" || f.OrderGID != "gid://shopify/Order/1001" {
		t.Errorf("ids = %q, %q", f.StoreID, f.OrderGID)
	}
	if !f.Version.Equal(mustInstant(t, "2026-11-01T10:00:00.123456Z")) {
		t.Errorf("Version = %v", f.Version)
	}
	if !f.RecordedAt.Equal(mustInstant(t, "2026-11-01T10:00:01Z")) {
		t.Errorf("RecordedAt = %v", f.RecordedAt)
	}
	if !f.OrderedAt.Equal(mustInstant(t, "2026-10-31T23:30:00Z")) {
		t.Errorf("OrderedAt = %v", f.OrderedAt)
	}
	if f.Currency != "EUR" || f.BuyerKey != "gid://shopify/Customer/77" || f.Email != buyerEmail {
		t.Errorf("Currency, BuyerKey, Email = %q, %q, %q", f.Currency, f.BuyerKey, f.Email)
	}
	if f.Cancelled || f.Test || f.TaxIncluded {
		t.Errorf("Cancelled, Test, TaxIncluded = %v, %v, %v; want all false", f.Cancelled, f.Test, f.TaxIncluded)
	}
	wantAmount(t, "Shipping", f.Shipping, "12.50")
	wantAmount(t, "Tax", f.Tax, "4.20")
	wantAmount(t, "Duties", f.Duties, "0")
	if f.ExpectedQuantity != 3 {
		t.Errorf("ExpectedQuantity = %d, want 3", f.ExpectedQuantity)
	}
	wantPtr(t, "ShopifyCurrentSubtotal", f.ShopifyCurrentSubtotal, "80.00")
	wantPtr(t, "ShopifyTotalDiscounts", f.ShopifyTotalDiscounts, "8.00")
	wantPtr(t, "ShopifyTotalRefunded", f.ShopifyTotalRefunded, "0.00")

	// Duties when the row has them; the three Shopify totals nil without them.
	row := orderRow()
	row["originalTotalDutiesSet"] = set("1.10", "EUR")
	delete(row, "currentSubtotalPriceSet")
	row["totalDiscountsSet"] = nil
	delete(row, "totalRefundedSet")
	f, kind, err = orderFactOf(row)
	if err != nil || kind != rowLive {
		t.Fatalf("orderFactOf = %d, %v; want rowLive", kind, err)
	}
	wantAmount(t, "Duties", f.Duties, "1.10")
	if f.ShopifyCurrentSubtotal != nil || f.ShopifyTotalDiscounts != nil || f.ShopifyTotalRefunded != nil {
		t.Errorf("Shopify totals = %v, %v, %v; want nil", f.ShopifyCurrentSubtotal, f.ShopifyTotalDiscounts, f.ShopifyTotalRefunded)
	}
	// An absent amount is zero.
	delete(row, "totalShippingPriceSet")
	f, _, _ = orderFactOf(row)
	wantAmount(t, "Shipping absent", f.Shipping, "0")
}

func TestACancelledOrTestOrderSaysSo(t *testing.T) {
	row := orderRow()
	row["cancelledAt"] = "2026-11-02T09:00:00Z"
	row["test"] = true
	f, kind, err := orderFactOf(row)
	if err != nil || kind != rowLive {
		t.Fatalf("orderFactOf = %d, %v", kind, err)
	}
	if !f.Cancelled || !f.Test {
		t.Errorf("Cancelled, Test = %v, %v; want true, true", f.Cancelled, f.Test)
	}
	row["cancelledAt"] = ""
	row["test"] = false
	f, _, _ = orderFactOf(row)
	if f.Cancelled || f.Test {
		t.Errorf("an empty cancelledAt: Cancelled, Test = %v, %v; want false, false", f.Cancelled, f.Test)
	}
	row["cancelledAt"] = mustInstant(t, "2026-11-02T09:00:00Z")
	f, _, _ = orderFactOf(row)
	if !f.Cancelled {
		t.Error("cancelledAt as a time.Time: not Cancelled")
	}
}

func TestTaxIncludedInPricesIsNotAddedAgain(t *testing.T) {
	row := orderRow()
	row["taxesIncluded"] = true
	f, kind, err := orderFactOf(row)
	if err != nil || kind != rowLive {
		t.Fatalf("orderFactOf = %d, %v", kind, err)
	}
	if !f.TaxIncluded {
		t.Error("TaxIncluded = false")
	}
	wantAmount(t, "Tax", f.Tax, "0.00")
	row["taxesIncluded"] = false
	f, _, _ = orderFactOf(row)
	wantAmount(t, "Tax when prices exclude it", f.Tax, "4.20")
}

func TestAnOrderIsDatedByProcessedAtElseItsCreation(t *testing.T) {
	row := orderRow()
	f, _, _ := orderFactOf(row)
	if !f.OrderedAt.Equal(mustInstant(t, "2026-10-31T23:30:00Z")) {
		t.Errorf("with processedAt: OrderedAt = %v", f.OrderedAt)
	}
	for _, absent := range []any{nil, ""} {
		row["processedAt"] = absent
		f, kind, err := orderFactOf(row)
		if err != nil || kind != rowLive {
			t.Fatalf("processedAt %#v: %d, %v", absent, kind, err)
		}
		if !f.OrderedAt.Equal(mustInstant(t, "2026-10-31T23:00:00Z")) {
			t.Errorf("processedAt %#v: OrderedAt = %v, want sourceCreatedAt", absent, f.OrderedAt)
		}
	}
	// The same for a refund.
	r := refundRow()
	delete(r, "processedAt")
	rf, kind, err := refundFactOf(r, 50)
	if err != nil || kind != rowLive {
		t.Fatalf("refund: %d, %v", kind, err)
	}
	if !rf.RefundedAt.Equal(mustInstant(t, "2026-11-02T14:00:00Z")) {
		t.Errorf("refund: RefundedAt = %v, want sourceCreatedAt", rf.RefundedAt)
	}
}

func TestARowWithNoDateOrAnAmountThatDoesNotParseIsUnreadable(t *testing.T) {
	row := orderRow()
	delete(row, "processedAt")
	delete(row, "sourceCreatedAt")
	_, kind, err := orderFactOf(row)
	wantUnreadable(t, kind, err, "sourceCreatedAt", buyerEmail)

	row = orderRow()
	row["processedAt"] = "the last day of October"
	_, kind, err = orderFactOf(row)
	wantUnreadable(t, kind, err, "processedAt", "October", buyerEmail)

	row = orderRow()
	row["totalShippingPriceSet"] = set("12,50", "EUR")
	_, kind, err = orderFactOf(row)
	wantUnreadable(t, kind, err, "totalShippingPriceSet", "12,50", buyerEmail)

	row = orderRow()
	row["currentSubtotalPriceSet"] = set("", "EUR")
	_, kind, err = orderFactOf(row)
	wantUnreadable(t, kind, err, "currentSubtotalPriceSet", buyerEmail)

	r := refundRow()
	delete(r, "processedAt")
	delete(r, "sourceCreatedAt")
	_, kind, err = refundFactOf(r, 50)
	wantUnreadable(t, kind, err, "sourceCreatedAt")

	l := lineRow()
	l["originalUnitPriceSet"] = set("40.00 EUR", "EUR")
	l["productGid"] = nil
	l["title"] = customTitle
	_, kind, err = lineFactOf(l)
	wantUnreadable(t, kind, err, "originalUnitPriceSet", "40.00 EUR", customTitle)

	l = lineRow()
	l["discountAllocations"] = []any{allocation("5.00", "EUR"), allocation("3,00", "EUR")}
	_, kind, err = lineFactOf(l)
	wantUnreadable(t, kind, err, "discountAllocations", "3,00")

	r = refundRow()
	r["refundLineItems"] = []any{refundLine("gid://shopify/LineItem/501", float64(1), "36,00", "7.56")}
	_, kind, err = refundFactOf(r, 50)
	wantUnreadable(t, kind, err, "refundLineItems", "36,00")
}

func TestARowWithoutItsIdentityIsUnreadable(t *testing.T) {
	for _, key := range []string{"gid", "storeId", "createdAt"} {
		for _, tombstone := range []bool{false, true} {
			o, l, r := orderRow(), lineRow(), refundRow()
			for _, row := range []map[string]any{o, l, r} {
				delete(row, key)
				row["deleted"] = tombstone
			}
			_, kind, err := orderFactOf(o)
			wantUnreadable(t, kind, err, key, buyerEmail)
			_, kind, err = lineFactOf(l)
			wantUnreadable(t, kind, err, key)
			_, kind, err = refundFactOf(r, 50)
			wantUnreadable(t, kind, err, key)
		}
	}
	// A live line or refund without the order it belongs to is not a fact either.
	l, r := lineRow(), refundRow()
	delete(l, "parentGid")
	delete(r, "parentGid")
	_, kind, err := lineFactOf(l)
	wantUnreadable(t, kind, err, "parentGid")
	_, kind, err = refundFactOf(r, 50)
	wantUnreadable(t, kind, err, "parentGid")
	// And an order without its currency.
	o := orderRow()
	delete(o, "currencyCode")
	_, kind, err = orderFactOf(o)
	wantUnreadable(t, kind, err, "currencyCode", buyerEmail)
}

// A field the mirror writes as one type and the row holds as another: no
// guessing, an error naming the field and no value of it.
func TestAFieldOfTheWrongTypeIsUnreadable(t *testing.T) {
	type orderCase struct {
		key   string
		value any
		names string
	}
	for _, c := range []orderCase{
		{"totalTaxSet", "4.20", "totalTaxSet"},
		{"totalTaxSet", map[string]any{"shopMoney": "4.20"}, "totalTaxSet"},
		{"totalTaxSet", map[string]any{"presentmentMoney": map[string]any{"amount": "4.20", "currencyCode": "EUR"}}, "totalTaxSet"},
		{"totalTaxSet", map[string]any{"shopMoney": map[string]any{"amount": 4.2, "currencyCode": "EUR"}}, "totalTaxSet"},
		{"totalTaxSet", map[string]any{"shopMoney": map[string]any{"amount": json.Number("4.20"), "currencyCode": "EUR"}}, "totalTaxSet"},
		{"totalTaxSet", map[string]any{"shopMoney": map[string]any{"amount": "4.20"}}, "totalTaxSet"},
		{"totalTaxSet", set("4.20", "USD"), "totalTaxSet"}, // a currency the order is not in
		{"currencyCode", 978, "currencyCode"},
		{"customerGid", 77, "customerGid"},
		{"email", 1, "email"},
		{"test", "false", "test"},
		{"taxesIncluded", 1, "taxesIncluded"},
		{"deleted", "true", "deleted"},
		{"cancelledAt", 12, "cancelledAt"},
		{"syncedAt", "yesterday", "syncedAt"},
		{"processedAt", 1730419200, "processedAt"},
		{"createdAt", "noon", "createdAt"},
		{"gid", 1001, "gid"},
		{"subtotalLineItemsQuantity", "3", "subtotalLineItemsQuantity"},
	} {
		row := orderRow()
		row[c.key] = c.value
		_, kind, err := orderFactOf(row)
		wantUnreadable(t, kind, err, c.names, buyerEmail, "4.20", "USD", "yesterday", "noon")
	}

	for _, c := range []orderCase{
		{"originalUnitPriceSet", nil, "originalUnitPriceSet"}, // a line without a price has no currency
		{"originalUnitPriceSet", []any{}, "originalUnitPriceSet"},
		{"discountAllocations", map[string]any{}, "discountAllocations"},
		{"discountAllocations", []any{"5.00"}, "discountAllocations"},
		{"discountAllocations", []any{allocation("5.00", "USD")}, "discountAllocations"},
		{"discountAllocations", []any{map[string]any{"allocatedAmountSet": "5.00"}}, "discountAllocations"},
		{"isGiftCard", "no", "isGiftCard"},
		{"title", 7, "title"},
		{"productGid", 9, "productGid"},
		{"parentGid", 1001, "parentGid"},
		{"quantity", nil, "quantity"},
		{"currentQuantity", nil, "currentQuantity"},
	} {
		row := lineRow()
		row["productGid"] = nil
		row["title"] = customTitle
		if c.key == "title" {
			row["productGid"] = "gid://shopify/Product/9"
		}
		row[c.key] = c.value
		_, kind, err := lineFactOf(row)
		wantUnreadable(t, kind, err, c.names, customTitle, "5.00", "USD")
	}

	for _, c := range []orderCase{
		{"refundLineItems", map[string]any{"nodes": []any{}}, "refundLineItems"},
		{"refundShippingLines", "none", "refundShippingLines"},
		{"orderAdjustments", []any{nil}, "orderAdjustments"},
		{"duties", map[string]any{}, "duties"},
		{"duties", []any{map[string]any{"amountSet": set("1.00", "USD")}}, "duties"},
		{"refundLineItems", []any{map[string]any{"quantity": float64(1), "subtotalSet": set("1.00", "EUR"), "totalTaxSet": set("0.00", "EUR")}}, "refundLineItems"},
		{"refundLineItems", []any{map[string]any{"lineItem": "gid://shopify/LineItem/501", "quantity": float64(1)}}, "refundLineItems"},
		{"refundLineItems", []any{refundLine("gid://shopify/LineItem/501", float64(1.5), "1.00", "0.00")}, "refundLineItems"},
		{"refundLineItems", []any{refundLine("gid://shopify/LineItem/501", nil, "1.00", "0.00")}, "refundLineItems"},
		{"refundShippingLines", []any{map[string]any{"subtotalAmountSet": set("5.00", "USD")}}, "refundShippingLines"},
		{"totalRefundedSet", set("48.56", "USD"), "totalRefundedSet"},
	} {
		row := refundRow()
		row[c.key] = c.value
		_, kind, err := refundFactOf(row, 50)
		wantUnreadable(t, kind, err, c.names, "USD", "48.56")
	}
}

// The facts read the shop amount; the presentment amount is never read, not
// even to be checked.
func TestThePresentmentAmountIsNeverRead(t *testing.T) {
	broken := func(amount, currency string) map[string]any {
		return map[string]any{
			"shopMoney":        map[string]any{"amount": amount, "currencyCode": currency},
			"presentmentMoney": map[string]any{"amount": "not an amount", "currencyCode": 42},
		}
	}
	row := orderRow()
	row["totalShippingPriceSet"] = broken("12.50", "EUR")
	row["totalTaxSet"] = broken("4.20", "EUR")
	f, kind, err := orderFactOf(row)
	if err != nil || kind != rowLive {
		t.Fatalf("orderFactOf = %d, %v", kind, err)
	}
	wantAmount(t, "Shipping", f.Shipping, "12.50")
	wantAmount(t, "Tax", f.Tax, "4.20")

	l := lineRow()
	l["originalUnitPriceSet"] = broken("40.00", "EUR")
	l["discountAllocations"] = []any{map[string]any{"allocatedAmountSet": broken("8.00", "EUR")}}
	lf, kind, err := lineFactOf(l)
	if err != nil || kind != rowLive {
		t.Fatalf("lineFactOf = %d, %v", kind, err)
	}
	wantAmount(t, "UnitPrice", lf.UnitPrice, "40.00")
	wantAmount(t, "AllocatedDiscounts", lf.AllocatedDiscounts, "8.00")
	if lf.Currency != "EUR" {
		t.Errorf("Currency = %q, want the shop money's EUR", lf.Currency)
	}

	r := refundRow()
	r["totalRefundedSet"] = broken("48.56", "EUR")
	r["refundShippingLines"] = []any{map[string]any{"subtotalAmountSet": broken("5.00", "EUR"), "taxAmountSet": broken("0.00", "EUR")}}
	rf, kind, err := refundFactOf(r, 50)
	if err != nil || kind != rowLive {
		t.Fatalf("refundFactOf = %d, %v", kind, err)
	}
	wantAmount(t, "MoneyRefunded", rf.MoneyRefunded, "48.56")
	wantPtr(t, "ShippingRefunded", rf.ShippingRefunded, "5.00")
}

// The buyer key is the customer's gid and nothing else: a guest's order has no
// key, and the email is never the key nor inside it.
func TestTheBuyerKeyIsTheCustomerAndNeverTheEmail(t *testing.T) {
	row := orderRow()
	f, _, _ := orderFactOf(row)
	if f.BuyerKey != "gid://shopify/Customer/77" {
		t.Errorf("BuyerKey = %q, want the customer's gid", f.BuyerKey)
	}
	for _, guest := range []any{nil, ""} {
		row["customerGid"] = guest
		f, kind, err := orderFactOf(row)
		if err != nil || kind != rowLive {
			t.Fatalf("guest %#v: %d, %v", guest, kind, err)
		}
		if f.BuyerKey != "" {
			t.Errorf("guest %#v: BuyerKey = %q, want empty", guest, f.BuyerKey)
		}
		if strings.Contains(strings.ToLower(f.BuyerKey), "buyer") || strings.Contains(f.BuyerKey, "example.com") {
			t.Errorf("guest %#v: BuyerKey %q holds the email", guest, f.BuyerKey)
		}
		if f.Email != buyerEmail {
			t.Errorf("guest %#v: Email = %q, want it read in memory", guest, f.Email)
		}
	}
}

// ---- tombstones ----

func TestATombstoneIsRecognisedByDeletedAlone(t *testing.T) {
	o := orderRow()
	o["deleted"] = true
	o["versionSource"] = "read"
	o["updatedAt"] = "2026-11-05T00:00:00Z"
	o["createdAt"] = "2026-11-05T00:00:01Z"
	f, kind, err := orderFactOf(o)
	if err != nil || kind != rowGone {
		t.Fatalf("order tombstone = %d, %v; want rowGone", kind, err)
	}
	if f.StoreID != "store-a" || f.OrderGID != "gid://shopify/Order/1001" || !f.Version.Equal(mustInstant(t, "2026-11-05T00:00:01Z")) {
		t.Errorf("order tombstone = %+v", f)
	}
	if !f.RecordedAt.Equal(mustInstant(t, "2026-11-01T10:00:01Z")) {
		t.Errorf("order tombstone RecordedAt = %v, want its syncedAt", f.RecordedAt)
	}
	// Its answer is its ids and its versions and nothing of its last amounts,
	// dates or buyer.
	identity := OrderFact{StoreID: f.StoreID, OrderGID: f.OrderGID, Version: f.Version, RecordedAt: f.RecordedAt}
	if !f.same(identity) || f.Email != "" {
		t.Errorf("order tombstone = %+v, want its identity alone", f)
	}

	l := lineRow()
	l["deleted"] = true
	l["versionSource"] = "announced"
	lf, kind, err := lineFactOf(l)
	if err != nil || kind != rowGone {
		t.Fatalf("line tombstone = %d, %v; want rowGone", kind, err)
	}
	if lf.StoreID != "store-a" || lf.LineGID != "gid://shopify/LineItem/501" || lf.OrderGID != "gid://shopify/Order/1001" || lf.Version.IsZero() {
		t.Errorf("line tombstone = %+v", lf)
	}
	if !lf.same(LineFact{StoreID: lf.StoreID, OrderGID: lf.OrderGID, LineGID: lf.LineGID, Version: lf.Version}) {
		t.Errorf("line tombstone = %+v, want its identity alone", lf)
	}

	r := refundRow()
	r["deleted"] = true
	rf, kind, err := refundFactOf(r, 50)
	if err != nil || kind != rowGone {
		t.Fatalf("refund tombstone = %d, %v; want rowGone", kind, err)
	}
	if rf.StoreID != "store-a" || rf.RefundGID != "gid://shopify/Refund/700" || rf.OrderGID != "gid://shopify/Order/1001" || rf.Version.IsZero() || rf.RecordedAt.IsZero() {
		t.Errorf("refund tombstone = %+v", rf)
	}
	if !rf.same(RefundFact{StoreID: rf.StoreID, RefundGID: rf.RefundGID, OrderGID: rf.OrderGID, Version: rf.Version, RecordedAt: rf.RecordedAt}) {
		t.Errorf("refund tombstone = %+v, want its identity alone", rf)
	}

	// deleted false, or absent, is a live row whatever else it says.
	for _, live := range []any{false, nil} {
		o := orderRow()
		o["deleted"] = live
		o["versionSource"] = "read"
		if _, kind, err := orderFactOf(o); kind != rowLive || err != nil {
			t.Errorf("deleted %#v: %d, %v; want rowLive", live, kind, err)
		}
	}
}

func TestATombstoneThatCarriesOnlyItsIdentityIsStillATombstone(t *testing.T) {
	bare := func() map[string]any {
		return map[string]any{"storeId": "store-a", "gid": "gid://shopify/X/1", "createdAt": "2026-11-05T00:00:00Z", "deleted": true}
	}
	o, kind, err := orderFactOf(bare())
	if kind != rowGone || err != nil || o.OrderGID != "gid://shopify/X/1" || o.Version.IsZero() {
		t.Errorf("order: %+v, %d, %v; want rowGone", o, kind, err)
	}
	l, kind, err := lineFactOf(bare())
	if kind != rowGone || err != nil || l.LineGID != "gid://shopify/X/1" || l.Version.IsZero() {
		t.Errorf("line: %+v, %d, %v; want rowGone", l, kind, err)
	}
	r, kind, err := refundFactOf(bare(), 50)
	if kind != rowGone || err != nil || r.RefundGID != "gid://shopify/X/1" || r.Version.IsZero() {
		t.Errorf("refund: %+v, %d, %v; want rowGone", r, kind, err)
	}
	// A tombstone whose last amounts are malformed is still a tombstone: they are not read.
	ob := bare()
	ob["totalTaxSet"] = set("4,20", "EUR")
	ob["processedAt"] = "never"
	if _, kind, err := orderFactOf(ob); kind != rowGone || err != nil {
		t.Errorf("a tombstone with a malformed amount: %d, %v; want rowGone", kind, err)
	}
}

// ---- lines ----

func TestALineRowBecomesItsFact(t *testing.T) {
	f, kind, err := lineFactOf(lineRow())
	if err != nil || kind != rowLive {
		t.Fatalf("lineFactOf = %d, %v", kind, err)
	}
	if f.StoreID != "store-a" || f.OrderGID != "gid://shopify/Order/1001" || f.LineGID != "gid://shopify/LineItem/501" {
		t.Errorf("ids = %q, %q, %q", f.StoreID, f.OrderGID, f.LineGID)
	}
	if !f.Version.Equal(mustInstant(t, "2026-11-01T10:00:00Z")) {
		t.Errorf("Version = %v", f.Version)
	}
	if f.ProductKey != "gid://shopify/Product/9" || f.Title != "Inner tube" {
		t.Errorf("ProductKey, Title = %q, %q", f.ProductKey, f.Title)
	}
	if f.Quantity != 3 || f.CurrentQuantity != 2 {
		t.Errorf("Quantity, CurrentQuantity = %d, %d; want 3, 2", f.Quantity, f.CurrentQuantity)
	}
	wantAmount(t, "UnitPrice", f.UnitPrice, "40.00")
	wantAmount(t, "AllocatedDiscounts", f.AllocatedDiscounts, "8.00")
	if f.Currency != "EUR" || f.GiftCard {
		t.Errorf("Currency, GiftCard = %q, %v", f.Currency, f.GiftCard)
	}

	row := lineRow()
	delete(row, "productGid")
	row["isGiftCard"] = true
	row["discountAllocations"] = nil
	row["originalUnitPriceSet"] = set("1000", "JPY")
	f, kind, err = lineFactOf(row)
	if err != nil || kind != rowLive {
		t.Fatalf("lineFactOf = %d, %v", kind, err)
	}
	if f.ProductKey != "title:Inner tube" {
		t.Errorf("ProductKey without a product = %q, want %q", f.ProductKey, "title:Inner tube")
	}
	if !f.GiftCard {
		t.Error("GiftCard = false for a gift card line")
	}
	if f.Currency != "JPY" {
		t.Errorf("Currency = %q, want JPY from the unit price", f.Currency)
	}
	wantAmount(t, "AllocatedDiscounts with none", f.AllocatedDiscounts, "0")
}

// ---- refunds ----

func TestARefundRowBecomesItsFactAndItsLines(t *testing.T) {
	f, kind, err := refundFactOf(refundRow(), 50)
	if err != nil || kind != rowLive {
		t.Fatalf("refundFactOf = %d, %v", kind, err)
	}
	if f.StoreID != "store-a" || f.RefundGID != "gid://shopify/Refund/700" || f.OrderGID != "gid://shopify/Order/1001" {
		t.Errorf("ids = %q, %q, %q", f.StoreID, f.RefundGID, f.OrderGID)
	}
	if !f.Version.Equal(mustInstant(t, "2026-11-03T08:00:00Z")) || !f.RecordedAt.Equal(mustInstant(t, "2026-11-03T08:00:02Z")) {
		t.Errorf("Version, RecordedAt = %v, %v", f.Version, f.RecordedAt)
	}
	if !f.RefundedAt.Equal(mustInstant(t, "2026-11-02T15:00:00Z")) {
		t.Errorf("RefundedAt = %v, want processedAt", f.RefundedAt)
	}
	wantAmount(t, "MoneyRefunded", f.MoneyRefunded, "48.56")
	if !f.DetailFetched || !f.LinesComplete {
		t.Errorf("DetailFetched, LinesComplete = %v, %v", f.DetailFetched, f.LinesComplete)
	}
	wantPtr(t, "ShippingRefunded", f.ShippingRefunded, "5.00")
	wantPtr(t, "TaxRefunded", f.TaxRefunded, "7.56")
	wantPtr(t, "DutiesRefunded", f.DutiesRefunded, "0")
	if len(f.Lines) != 1 {
		t.Fatalf("Lines = %+v, want one", f.Lines)
	}
	l := f.Lines[0]
	if l.Position != 0 || l.LineGID != "gid://shopify/LineItem/501" || l.Quantity != 1 {
		t.Errorf("line = %+v", l)
	}
	wantAmount(t, "line Subtotal", l.Subtotal, "36.00")
	wantAmount(t, "line Tax", l.Tax, "7.56")

	// Several lines keep their order; shipping and duties sum; every tax counts.
	row := refundRow()
	row["refundLineItems"] = []any{
		refundLine("gid://shopify/LineItem/502", float64(2), "20.00", "4.20"),
		refundLine("gid://shopify/LineItem/501", int64(1), "36.00", "7.56"),
	}
	row["refundShippingLines"] = []any{
		map[string]any{"subtotalAmountSet": set("5.00", "EUR"), "taxAmountSet": set("1.05", "EUR")},
		map[string]any{"subtotalAmountSet": set("2.50", "EUR"), "taxAmountSet": set("0.50", "EUR")},
	}
	row["duties"] = []any{map[string]any{"amountSet": set("1.20", "EUR")}, map[string]any{"amountSet": set("0.30", "EUR")}}
	f, _, err = refundFactOf(row, 50)
	if err != nil {
		t.Fatal(err)
	}
	wantPtr(t, "ShippingRefunded", f.ShippingRefunded, "7.50")
	wantPtr(t, "TaxRefunded", f.TaxRefunded, "13.31")
	wantPtr(t, "DutiesRefunded", f.DutiesRefunded, "1.50")
	if len(f.Lines) != 2 || f.Lines[0].Position != 0 || f.Lines[0].LineGID != "gid://shopify/LineItem/502" || f.Lines[0].Quantity != 2 ||
		f.Lines[1].Position != 1 || f.Lines[1].LineGID != "gid://shopify/LineItem/501" {
		t.Errorf("Lines = %+v", f.Lines)
	}
}

func TestAnAdjustmentsTaxIsSummedWithItsSign(t *testing.T) {
	row := refundRow()
	row["refundShippingLines"] = []any{}
	row["orderAdjustments"] = []any{map[string]any{"amountSet": set("-2.00", "EUR"), "taxAmountSet": set("-0.42", "EUR"), "reason": "RESTOCK"}}
	f, _, err := refundFactOf(row, 50)
	if err != nil {
		t.Fatal(err)
	}
	wantPtr(t, "TaxRefunded", f.TaxRefunded, "7.14")
	row["orderAdjustments"] = []any{map[string]any{"amountSet": set("2.00", "EUR"), "taxAmountSet": set("0.42", "EUR")}}
	f, _, _ = refundFactOf(row, 50)
	wantPtr(t, "TaxRefunded with a positive adjustment", f.TaxRefunded, "7.98")
}

func TestARefundWhoseDetailWasNotReadSaysSo(t *testing.T) {
	for _, missing := range []string{"refundLineItems", "refundShippingLines", "orderAdjustments", "all"} {
		row := refundRow()
		if missing == "all" {
			row["refundLineItems"], row["refundShippingLines"], row["orderAdjustments"] = nil, nil, nil
		} else {
			row[missing] = nil
		}
		f, kind, err := refundFactOf(row, 50)
		if err != nil || kind != rowLive {
			t.Fatalf("%s nil: %d, %v", missing, kind, err)
		}
		if f.DetailFetched || f.LinesComplete {
			t.Errorf("%s nil: DetailFetched, LinesComplete = %v, %v; want false, false", missing, f.DetailFetched, f.LinesComplete)
		}
		if f.ShippingRefunded != nil || f.TaxRefunded != nil || f.DutiesRefunded != nil || len(f.Lines) != 0 {
			t.Errorf("%s nil: parts %v %v %v, lines %v; want nil and none", missing, f.ShippingRefunded, f.TaxRefunded, f.DutiesRefunded, f.Lines)
		}
		wantAmount(t, missing+" nil: MoneyRefunded", f.MoneyRefunded, "48.56")
	}
}

func TestARefundReadWithNothingOnItIsFetched(t *testing.T) {
	row := refundRow()
	row["refundLineItems"], row["refundShippingLines"], row["orderAdjustments"] = []any{}, []any{}, []any{}
	delete(row, "duties")
	f, kind, err := refundFactOf(row, 50)
	if err != nil || kind != rowLive {
		t.Fatalf("%d, %v", kind, err)
	}
	if !f.DetailFetched || !f.LinesComplete {
		t.Errorf("DetailFetched, LinesComplete = %v, %v; want true, true", f.DetailFetched, f.LinesComplete)
	}
	wantPtr(t, "ShippingRefunded", f.ShippingRefunded, "0")
	wantPtr(t, "TaxRefunded", f.TaxRefunded, "0")
	wantPtr(t, "DutiesRefunded", f.DutiesRefunded, "0")
	if len(f.Lines) != 0 {
		t.Errorf("Lines = %v", f.Lines)
	}
}

func TestARefundWithAFullPageOfLinesIsNotKnownToBeComplete(t *testing.T) {
	lines := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = refundLine("gid://shopify/LineItem/"+strconv.Itoa(i), float64(1), "1.00", "0.00")
		}
		return out
	}
	for _, c := range []struct {
		n, page  int
		complete bool
	}{{49, 50, true}, {50, 50, false}, {0, 50, true}, {1, 0, false}, {0, 0, false}, {0, -1, false}} {
		row := refundRow()
		row["refundLineItems"] = lines(c.n)
		f, _, err := refundFactOf(row, c.page)
		if err != nil {
			t.Fatal(err)
		}
		if f.LinesComplete != c.complete || len(f.Lines) != c.n {
			t.Errorf("%d lines, page %d: LinesComplete = %v with %d lines; want %v", c.n, c.page, f.LinesComplete, len(f.Lines), c.complete)
		}
	}
}

func TestTheRefundLinesPageIsTheGeneratedOne(t *testing.T) {
	want := 0
	for _, f := range generated.Types["refund"].Fields {
		if f.Name == "refundLineItems" {
			want = f.Page
		}
	}
	if want <= 0 {
		t.Fatalf("the generated refund.refundLineItems has no page (%d)", want)
	}
	if got := refundLinesPage(); got != want {
		t.Errorf("refundLinesPage() = %d, want %d", got, want)
	}
}

// ---- quantities and instants ----

func TestAQuantityIsAWholeNumberOrTheRowIsUnreadable(t *testing.T) {
	for _, v := range []any{float64(2), 2.0, json.Number("2"), int64(2), 2} {
		row := lineRow()
		row["quantity"] = v
		f, kind, err := lineFactOf(row)
		if err != nil || kind != rowLive || f.Quantity != 2 {
			t.Errorf("quantity %#v: %d, %d, %v; want 2", v, f.Quantity, kind, err)
		}
		o := orderRow()
		o["subtotalLineItemsQuantity"] = v
		of, kind, err := orderFactOf(o)
		if err != nil || kind != rowLive || of.ExpectedQuantity != 2 {
			t.Errorf("subtotalLineItemsQuantity %#v: %d, %d, %v; want 2", v, of.ExpectedQuantity, kind, err)
		}
	}
	for _, v := range []any{2.5, json.Number("2.5"), json.Number("two"), "2", float64(-1), int64(-1), 1e30, true} {
		row := lineRow()
		row["currentQuantity"] = v
		_, kind, err := lineFactOf(row)
		wantUnreadable(t, kind, err, "currentQuantity", "2.5", "two")
		o := orderRow()
		o["subtotalLineItemsQuantity"] = v
		_, kind, err = orderFactOf(o)
		wantUnreadable(t, kind, err, "subtotalLineItemsQuantity", "2.5", "two", buyerEmail)
		r := refundRow()
		r["refundLineItems"] = []any{refundLine("gid://shopify/LineItem/501", v, "36.00", "7.56")}
		_, kind, err = refundFactOf(r, 50)
		wantUnreadable(t, kind, err, "refundLineItems", "2.5", "two")
	}
}

func TestTheVersionOfAFactIsTheRowsOwnWriteTime(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 123456789, time.UTC)
	for _, v := range []any{at, at.Format(time.RFC3339Nano), at.In(time.FixedZone("x", 3600)).Format(time.RFC3339Nano)} {
		o, l, r := orderRow(), lineRow(), refundRow()
		for _, row := range []map[string]any{o, l, r} {
			row["createdAt"] = v
			row["updatedAt"] = "2020-01-01T00:00:00Z" // not the version
		}
		of, _, err := orderFactOf(o)
		if err != nil || !of.Version.Equal(at) {
			t.Errorf("order createdAt %#v: Version = %v, %v", v, of.Version, err)
		}
		lf, _, err := lineFactOf(l)
		if err != nil || !lf.Version.Equal(at) {
			t.Errorf("line createdAt %#v: Version = %v, %v", v, lf.Version, err)
		}
		rf, _, err := refundFactOf(r, 50)
		if err != nil || !rf.Version.Equal(at) {
			t.Errorf("refund createdAt %#v: Version = %v, %v", v, rf.Version, err)
		}
	}
}

func TestInstantOf(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 5, time.UTC)
	for _, c := range []struct {
		v  any
		ok bool
	}{
		{at, true}, {at.Format(time.RFC3339Nano), true}, {"2026-11-01T10:00:00Z", true},
		{"2026-11-01", false}, {"", false}, {nil, false}, {time.Time{}, false}, {1730455200, false}, {"soon", false},
	} {
		got, ok := instantOf(c.v)
		if ok != c.ok {
			t.Errorf("instantOf(%#v) ok = %v, want %v", c.v, ok, c.ok)
		}
		if ok && got.IsZero() {
			t.Errorf("instantOf(%#v) = zero", c.v)
		}
	}
	if got, _ := instantOf(at.Format(time.RFC3339Nano)); !got.Equal(at) {
		t.Errorf("instantOf keeps nanoseconds: %v", got)
	}
}

// ---- same ----

func TestTwoFactsAreTheSameByContentNotByText(t *testing.T) {
	o1, _, _ := orderFactOf(orderRow())
	row := orderRow()
	row["totalShippingPriceSet"] = set("12.5", "EUR")
	row["email"] = "someone.else@example.com"
	row["createdAt"] = "2026-11-01T11:00:00.123456+01:00" // the same instant
	o2, _, _ := orderFactOf(row)
	if !o1.same(o2) || !o2.same(o1) {
		t.Errorf("orders that differ by the text of an amount, an instant's zone and the email are not the same")
	}
	for name, change := range map[string]func(*OrderFact){
		"StoreID":   func(f *OrderFact) { f.StoreID = "store-b" },
		"OrderGID":  func(f *OrderFact) { f.OrderGID = "gid://shopify/Order/2" },
		"Version":   func(f *OrderFact) { f.Version = f.Version.Add(time.Nanosecond) },
		"Recorded":  func(f *OrderFact) { f.RecordedAt = f.RecordedAt.Add(time.Second) },
		"OrderedAt": func(f *OrderFact) { f.OrderedAt = f.OrderedAt.Add(time.Second) },
		"Currency":  func(f *OrderFact) { f.Currency = "USD" },
		"BuyerKey":  func(f *OrderFact) { f.BuyerKey = "" },
		"Cancelled": func(f *OrderFact) { f.Cancelled = true },
		"Test":      func(f *OrderFact) { f.Test = true },
		"TaxIncl":   func(f *OrderFact) { f.TaxIncluded = true },
		"Shipping":  func(f *OrderFact) { f.Shipping = amt(t, "12.51") },
		"Tax":       func(f *OrderFact) { f.Tax = amt(t, "0") },
		"Duties":    func(f *OrderFact) { f.Duties = amt(t, "0.01") },
		"Expected":  func(f *OrderFact) { f.ExpectedQuantity = 4 },
		"Subtotal":  func(f *OrderFact) { f.ShopifyCurrentSubtotal = nil },
		"Discounts": func(f *OrderFact) { v := amt(t, "8.01"); f.ShopifyTotalDiscounts = &v },
		"Refunded":  func(f *OrderFact) { f.ShopifyTotalRefunded = nil },
	} {
		g := o1
		change(&g)
		if o1.same(g) || g.same(o1) {
			t.Errorf("orders that differ by %s are the same", name)
		}
	}
	// nil with nil is the same.
	a, b := o1, o2
	a.ShopifyCurrentSubtotal, b.ShopifyCurrentSubtotal = nil, nil
	if !a.same(b) {
		t.Error("two nil subtotals are not the same")
	}

	l1, _, _ := lineFactOf(lineRow())
	lrow := lineRow()
	lrow["originalUnitPriceSet"] = set("40.0", "EUR")
	l2, _, _ := lineFactOf(lrow)
	if !l1.same(l2) {
		t.Error("lines that differ by the text of an amount are not the same")
	}
	for name, change := range map[string]func(*LineFact){
		"StoreID":   func(f *LineFact) { f.StoreID = "x" },
		"OrderGID":  func(f *LineFact) { f.OrderGID = "x" },
		"LineGID":   func(f *LineFact) { f.LineGID = "x" },
		"Version":   func(f *LineFact) { f.Version = f.Version.Add(time.Second) },
		"Currency":  func(f *LineFact) { f.Currency = "USD" },
		"GiftCard":  func(f *LineFact) { f.GiftCard = true },
		"Product":   func(f *LineFact) { f.ProductKey = "title:Inner tube" },
		"Title":     func(f *LineFact) { f.Title = "Tyre" },
		"Quantity":  func(f *LineFact) { f.Quantity = 4 },
		"Current":   func(f *LineFact) { f.CurrentQuantity = 1 },
		"UnitPrice": func(f *LineFact) { f.UnitPrice = amt(t, "40.01") },
		"Allocated": func(f *LineFact) { f.AllocatedDiscounts = amt(t, "0") },
	} {
		g := l1
		change(&g)
		if l1.same(g) || g.same(l1) {
			t.Errorf("lines that differ by %s are the same", name)
		}
	}

	r1, _, _ := refundFactOf(refundRow(), 50)
	if len(r1.Lines) != 1 {
		t.Fatalf("the refund fixture makes %d lines, want one", len(r1.Lines))
	}
	rrow := refundRow()
	rrow["refundLineItems"] = []any{refundLine("gid://shopify/LineItem/501", float64(1), "36.0", "7.560")}
	r2, _, _ := refundFactOf(rrow, 50)
	if !r1.same(r2) {
		t.Error("refunds whose lines differ by the text of an amount are not the same")
	}
	for name, change := range map[string]func(*RefundFact){
		"StoreID":     func(f *RefundFact) { f.StoreID = "x" },
		"RefundGID":   func(f *RefundFact) { f.RefundGID = "x" },
		"OrderGID":    func(f *RefundFact) { f.OrderGID = "x" },
		"Version":     func(f *RefundFact) { f.Version = f.Version.Add(time.Second) },
		"RecordedAt":  func(f *RefundFact) { f.RecordedAt = time.Time{} },
		"RefundedAt":  func(f *RefundFact) { f.RefundedAt = f.RefundedAt.Add(time.Second) },
		"Money":       func(f *RefundFact) { f.MoneyRefunded = amt(t, "48.55") },
		"Fetched":     func(f *RefundFact) { f.DetailFetched = false },
		"Complete":    func(f *RefundFact) { f.LinesComplete = false },
		"Shipping":    func(f *RefundFact) { f.ShippingRefunded = nil },
		"Tax":         func(f *RefundFact) { v := amt(t, "7.57"); f.TaxRefunded = &v },
		"Duties":      func(f *RefundFact) { f.DutiesRefunded = nil },
		"no lines":    func(f *RefundFact) { f.Lines = nil },
		"a line more": func(f *RefundFact) { f.Lines = append(append([]RefundLineFact(nil), f.Lines...), f.Lines[0]) },
		"line qty": func(f *RefundFact) {
			f.Lines = []RefundLineFact{{Position: 0, LineGID: f.Lines[0].LineGID, Quantity: 2, Subtotal: f.Lines[0].Subtotal, Tax: f.Lines[0].Tax}}
		},
		"line gid": func(f *RefundFact) {
			f.Lines = []RefundLineFact{{Position: 0, LineGID: "x", Quantity: 1, Subtotal: f.Lines[0].Subtotal, Tax: f.Lines[0].Tax}}
		},
		"line pos": func(f *RefundFact) {
			f.Lines = []RefundLineFact{{Position: 1, LineGID: f.Lines[0].LineGID, Quantity: 1, Subtotal: f.Lines[0].Subtotal, Tax: f.Lines[0].Tax}}
		},
		"line sub": func(f *RefundFact) {
			f.Lines = []RefundLineFact{{Position: 0, LineGID: f.Lines[0].LineGID, Quantity: 1, Subtotal: amt(t, "36.01"), Tax: f.Lines[0].Tax}}
		},
		"line tax": func(f *RefundFact) {
			f.Lines = []RefundLineFact{{Position: 0, LineGID: f.Lines[0].LineGID, Quantity: 1, Subtotal: f.Lines[0].Subtotal, Tax: amt(t, "0")}}
		},
	} {
		g := r1
		change(&g)
		if r1.same(g) || g.same(r1) {
			t.Errorf("refunds that differ by %s are the same", name)
		}
	}
	// Two refunds whose detail was not read are the same.
	nrow := refundRow()
	nrow["refundLineItems"] = nil
	n1, _, _ := refundFactOf(nrow, 50)
	n2, _, _ := refundFactOf(nrow, 50)
	if !n1.same(n2) {
		t.Error("two unread refunds are not the same")
	}
}

// ---- no float ----

// TestNoAmountGoesThroughAFloat reads this package's own source: an amount is
// exact from the row's string to the fact, so no float conversion, no float
// parsing or printing and no big.Float appears anywhere. The one float64 the
// package may name is the arm of quantityOf that reads a JSON number, whose
// whole value core/num takes, and which never touches an amount.
func TestNoAmountGoesThroughAFloat(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		read++
		for _, site := range floatSites(t, path, src) {
			t.Errorf("%s", site)
		}
	}
	if read == 0 {
		t.Fatal("no source file read")
	}
	// The detector sees what it is for.
	bad := []byte(`package x
import ("math/big"; "strconv")
func a(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
func b(n int) float64 { return float64(n) }
func c() *big.Float { return big.NewFloat(1) }
func d(v any) float32 { x, _ := v.(float32); return x }
`)
	if got := floatSites(t, "bad.go", bad); len(got) < 6 {
		t.Errorf("the detector found %d sites in a file with at least six: %v", len(got), got)
	}
}

func floatSites(t *testing.T, path string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, decl := range file.Decls {
		fn, isFunc := decl.(*ast.FuncDecl)
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name == "float32" || x.Name == "float64" {
					if isFunc && fn.Name.Name == "quantityOf" && x.Name == "float64" {
						return true
					}
					out = append(out, fmt.Sprintf("%s: %s", fset.Position(x.Pos()), x.Name))
				}
			case *ast.SelectorExpr:
				switch x.Sel.Name {
				case "ParseFloat", "FormatFloat", "AppendFloat", "Float", "NewFloat", "Float64", "Float32":
					out = append(out, fmt.Sprintf("%s: %s", fset.Position(x.Pos()), x.Sel.Name))
				}
			}
			return true
		})
	}
	return out
}

// ---- the property ----

// TestTheFactsKeepTheirIdentitiesOverGeneratedOrders builds a few thousand
// orders from valid parts -- lines with quantities kept, refunded and removed,
// discounts, refunds with lines, shipping and signed adjustments -- runs them
// through the fact functions and soldOf, and checks each against big.Rat
// arithmetic done here:
//
//   - gross is the unit price times the units sold, exactly;
//   - a line's discount share is allocated * sold / quantity rounded half to
//     even at the larger of the written scale and the currency's minor units,
//     and the whole allocation when every unit is sold;
//   - an order with no units removed by an edit has every unit sold, so its
//     lines' discounts sum to its totalDiscountsSet, and its lines' gross less
//     their discounts is its subtotal before refunds;
//   - a refund's tax is its lines' tax, its shipping's tax and its
//     adjustments' tax with their signs, and its shipping is its shipping
//     lines' subtotals.
func TestTheFactsKeepTheirIdentitiesOverGeneratedOrders(t *testing.T) {
	rng := rand.New(rand.NewPCG(20261111, 10))
	currencies := []struct {
		code  string
		minor int
	}{{"EUR", 2}, {"JPY", 0}, {"KWD", 3}}
	// written prints a whole number of the smallest written unit at a scale.
	written := func(unscaled int64, scale int) string {
		return new(big.Rat).SetFrac(big.NewInt(unscaled), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)).FloatString(scale)
	}
	rat := func(s string) *big.Rat {
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			t.Fatalf("not a rational: %q", s)
		}
		return r
	}
	ratOf := func(a money.Amount) *big.Rat { return rat(a.String()) }
	sum := func(xs ...*big.Rat) *big.Rat {
		out := new(big.Rat)
		for _, x := range xs {
			out.Add(out, x)
		}
		return out
	}

	const orders = 3000
	for i := 0; i < orders; i++ {
		cur := currencies[rng.IntN(len(currencies))]
		scaleOf := func() int { // the store writes the minor units, or one fewer
			if cur.minor > 0 && rng.IntN(4) == 0 {
				return cur.minor - 1
			}
			return cur.minor
		}
		orderGID := fmt.Sprintf("gid://shopify/Order/%d", i)
		nLines := 1 + rng.IntN(4)
		removedAny := false
		type genLine struct {
			gid         string
			q, cq, qr   int
			unit, alloc string
			allocParts  []string
			allocScale  int
		}
		var gl []genLine
		var lineRows []map[string]any
		expected := 0
		subtotal := new(big.Rat)  // the lines' unit price times quantity, less every discount
		discounts := new(big.Rat) // every allocation
		for j := 0; j < nLines; j++ {
			q := rng.IntN(7)
			cq := 0
			if q > 0 {
				cq = rng.IntN(q + 1)
			}
			qr := 0
			if q-cq > 0 {
				qr = rng.IntN(q - cq + 1)
			}
			if rng.IntN(3) > 0 { // most orders have no edits
				qr = q - cq
			}
			if cq+qr < q {
				removedAny = true
			}
			us := scaleOf()
			unit := written(rng.Int64N(100000), us)
			var parts []string
			allocTotal := new(big.Rat)
			allocScale := 0
			for k := rng.IntN(3); k > 0; k-- {
				as := scaleOf()
				p := written(rng.Int64N(int64(q)*500+1), as)
				parts = append(parts, p)
				allocScale = max(allocScale, as)
				allocTotal.Add(allocTotal, rat(p))
			}
			gid := fmt.Sprintf("gid://shopify/LineItem/%d-%d", i, j)
			gl = append(gl, genLine{gid: gid, q: q, cq: cq, qr: qr, unit: unit, alloc: allocTotal.FloatString(cur.minor + 1), allocParts: parts, allocScale: allocScale})
			allocs := make([]any, len(parts))
			for k, p := range parts {
				allocs[k] = allocation(p, cur.code)
			}
			lineRows = append(lineRows, map[string]any{
				"createdAt": "2026-11-01T10:00:00Z", "storeId": "store-p", "gid": gid, "parentGid": orderGID,
				"isGiftCard": rng.IntN(10) == 0, "productGid": "gid://shopify/Product/1", "title": "Item",
				"quantity": float64(q), "currentQuantity": json.Number(strconv.Itoa(cq)),
				"originalUnitPriceSet": set(unit, cur.code), "discountAllocations": allocs,
			})
			expected += q
			subtotal.Add(subtotal, new(big.Rat).Mul(rat(unit), new(big.Rat).SetInt64(int64(q))))
			subtotal.Sub(subtotal, allocTotal)
			discounts.Add(discounts, allocTotal)
		}
		taxIncluded := rng.IntN(2) == 0
		oRow := map[string]any{
			"createdAt": "2026-11-01T10:00:00Z", "storeId": "store-p", "gid": orderGID, "processedAt": "2026-11-01T09:00:00Z",
			"taxesIncluded": taxIncluded, "currencyCode": cur.code, "subtotalLineItemsQuantity": float64(expected),
			"totalTaxSet":       set(written(rng.Int64N(5000), cur.minor), cur.code),
			"totalDiscountsSet": set(discounts.FloatString(cur.minor+1), cur.code),
		}
		if !removedAny {
			oRow["currentSubtotalPriceSet"] = set(subtotal.FloatString(cur.minor+1), cur.code)
		}
		of, kind, err := orderFactOf(oRow)
		if err != nil || kind != rowLive {
			t.Fatalf("order %d: %d, %v", i, kind, err)
		}
		if of.ExpectedQuantity != expected {
			t.Fatalf("order %d: ExpectedQuantity %d, want %d", i, of.ExpectedQuantity, expected)
		}
		if taxIncluded && of.Tax.Sign() != 0 {
			t.Fatalf("order %d: tax %s counted for prices that include it", i, of.Tax)
		}

		// Refunds: each line's refunded units split over one or two refunds.
		refunded := map[string]int{}
		for r := 0; r < 2; r++ {
			var items []any
			taxes := new(big.Rat)
			for _, l := range gl {
				n := l.qr
				if r == 0 {
					n = l.qr / 2
				} else {
					n = l.qr - l.qr/2
				}
				if n == 0 {
					continue
				}
				tax := written(rng.Int64N(2000), cur.minor)
				items = append(items, map[string]any{
					"lineItem": map[string]any{"id": l.gid}, "quantity": float64(n),
					"subtotalSet": set(written(rng.Int64N(20000), cur.minor), cur.code), "totalTaxSet": set(tax, cur.code),
				})
				taxes.Add(taxes, rat(tax))
			}
			ship := written(rng.Int64N(1500), cur.minor)
			shipTax := written(rng.Int64N(300), cur.minor)
			adjTax := written(rng.Int64N(400)-200, cur.minor)
			total := written(rng.Int64N(50000), cur.minor)
			rRow := map[string]any{
				"createdAt": "2026-11-02T10:00:00Z", "storeId": "store-p", "gid": fmt.Sprintf("gid://shopify/Refund/%d-%d", i, r),
				"parentGid": orderGID, "processedAt": "2026-11-02T09:00:00Z", "totalRefundedSet": set(total, cur.code),
				"duties":              []any{},
				"refundLineItems":     items,
				"refundShippingLines": []any{map[string]any{"subtotalAmountSet": set(ship, cur.code), "taxAmountSet": set(shipTax, cur.code)}},
				"orderAdjustments":    []any{map[string]any{"amountSet": set(written(rng.Int64N(400)-200, cur.minor), cur.code), "taxAmountSet": set(adjTax, cur.code)}},
			}
			if items == nil {
				rRow["refundLineItems"] = []any{}
			}
			rf, kind, err := refundFactOf(rRow, refundLinesPage())
			if err != nil || kind != rowLive {
				t.Fatalf("order %d refund %d: %d, %v", i, r, kind, err)
			}
			if rf.TaxRefunded == nil || rf.ShippingRefunded == nil {
				t.Fatalf("order %d refund %d: a part is nil for a refund whose detail was read", i, r)
			}
			if ratOf(rf.MoneyRefunded).Cmp(rat(total)) != 0 {
				t.Fatalf("order %d refund %d: MoneyRefunded %s, want %s", i, r, rf.MoneyRefunded, total)
			}
			if ratOf(*rf.TaxRefunded).Cmp(sum(taxes, rat(shipTax), rat(adjTax))) != 0 {
				t.Fatalf("order %d refund %d: TaxRefunded %s, want %s", i, r, rf.TaxRefunded, sum(taxes, rat(shipTax), rat(adjTax)).FloatString(cur.minor))
			}
			if ratOf(*rf.ShippingRefunded).Cmp(rat(ship)) != 0 {
				t.Fatalf("order %d refund %d: ShippingRefunded %s, want %s", i, r, rf.ShippingRefunded, ship)
			}
			if !rf.LinesComplete {
				t.Fatalf("order %d refund %d: %d lines not complete", i, r, len(rf.Lines))
			}
			for _, rl := range rf.Lines {
				refunded[rl.LineGID] += rl.Quantity
			}
		}

		lineGross, lineDiscounts := new(big.Rat), new(big.Rat)
		for j, l := range gl {
			lf, kind, err := lineFactOf(lineRows[j])
			if err != nil || kind != rowLive {
				t.Fatalf("order %d line %d: %d, %v", i, j, kind, err)
			}
			if lf.Quantity != l.q || lf.CurrentQuantity != l.cq || ratOf(lf.UnitPrice).Cmp(rat(l.unit)) != 0 || lf.Currency != cur.code {
				t.Fatalf("order %d line %d: %+v, want %+v", i, j, lf, l)
			}
			if ratOf(lf.AllocatedDiscounts).Cmp(rat(l.alloc)) != 0 {
				t.Fatalf("order %d line %d: AllocatedDiscounts %s, want %s", i, j, lf.AllocatedDiscounts, l.alloc)
			}
			if refunded[l.gid] != l.qr {
				t.Fatalf("order %d line %d: the refunds hold %d units, want %d", i, j, refunded[l.gid], l.qr)
			}
			sold, gross, disc := soldOf(lf.Quantity, lf.CurrentQuantity, refunded[l.gid], lf.UnitPrice, lf.AllocatedDiscounts, lf.Currency)
			wantSold := min(l.q, l.cq+l.qr)
			if sold != wantSold {
				t.Fatalf("order %d line %d: sold %d, want %d", i, j, sold, wantSold)
			}
			if ratOf(gross).Cmp(new(big.Rat).Mul(rat(l.unit), new(big.Rat).SetInt64(int64(sold)))) != 0 {
				t.Fatalf("order %d line %d: gross %s", i, j, gross)
			}
			var want *big.Rat
			switch {
			case l.q == 0:
				want = new(big.Rat)
			case sold == l.q:
				want = rat(l.alloc)
			default:
				scale := max(l.allocScale, cur.minor)
				want = halfEven(new(big.Rat).Mul(rat(l.alloc), big.NewRat(int64(sold), int64(l.q))), scale)
			}
			if ratOf(disc).Cmp(want) != 0 {
				t.Fatalf("order %d line %d: discounts %s, want %s (allocated %s, %d of %d)", i, j, disc, want.FloatString(6), l.alloc, sold, l.q)
			}
			lineGross.Add(lineGross, ratOf(gross))
			lineDiscounts.Add(lineDiscounts, ratOf(disc))
		}
		if !removedAny {
			if of.ShopifyTotalDiscounts == nil || of.ShopifyCurrentSubtotal == nil {
				t.Fatalf("order %d: a Shopify total is nil", i)
			}
			if ratOf(*of.ShopifyTotalDiscounts).Cmp(lineDiscounts) != 0 {
				t.Fatalf("order %d: lines' discounts %s, order's %s", i, lineDiscounts.FloatString(4), of.ShopifyTotalDiscounts)
			}
			if ratOf(*of.ShopifyCurrentSubtotal).Cmp(new(big.Rat).Sub(lineGross, lineDiscounts)) != 0 {
				t.Fatalf("order %d: lines' gross less discounts %s, order's subtotal %s", i, new(big.Rat).Sub(lineGross, lineDiscounts).FloatString(4), of.ShopifyCurrentSubtotal)
			}
		}
	}
}

// halfEven rounds x to scale decimals, a tie to the even last digit.
func halfEven(x *big.Rat, scale int) *big.Rat {
	p := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	y := new(big.Rat).Mul(x, new(big.Rat).SetInt(p))
	neg := y.Sign() < 0
	y.Abs(y)
	q, r := new(big.Int).QuoRem(y.Num(), y.Denom(), new(big.Int))
	twice := new(big.Int).Lsh(r, 1)
	if c := twice.Cmp(y.Denom()); c > 0 || (c == 0 && q.Bit(0) == 1) {
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	return new(big.Rat).SetFrac(q, p)
}
