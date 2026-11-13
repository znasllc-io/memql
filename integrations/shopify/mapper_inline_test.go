package shopify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// mapper_inline_test.go -- a refund's detail (its lines, its shipping lines
// and its adjustments) is an inlined connection, and a list the document did
// not select is NOT READ, never empty.
//
// A bulk export selects no inlined connection, so a refund's bulk line has
// none of the three keys. Written as [] they would blank the detail a fetch
// recorded, and a reader could not tell a refund nobody read from one with
// nothing on it. Left out, a new row lacks them and an existing row keeps
// what a fetch wrote (the engine read-merges a field a write omits). Every
// other list keeps the [] it is written today.

var inlineRead = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

// decodeOrigin is a reply as the connector decodes one: numbers as
// json.Number, so a 64-bit id keeps its digits.
func decodeOrigin(t *testing.T, raw string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("the scripted reply is not JSON: %v", err)
	}
	return out
}

// refundDetail is the three inlined fields of the generated refund.
var refundDetail = []string{"refundLineItems", "refundShippingLines", "orderAdjustments"}

func TestAnInlinedListTheDocumentDidNotSelectIsLeftOutOfTheWrite(t *testing.T) {
	spec := generated.Types["refund"]
	for _, name := range refundDetail {
		f, ok := fieldNamed(spec, name)
		if !ok || f.Kind != generated.KindInlineList {
			t.Fatalf("the generated refund has no inlined %s (%+v)", name, f)
		}
	}
	// A refund as an order's bulk line carries it: no inlined connection.
	line := decodeOrigin(t, `{"id":"gid://shopify/Refund/71","updatedAt":"2026-11-10T08:00:00Z","note":"",
		"totalRefundedSet":{"shopMoney":{"amount":"41.00","currencyCode":"EUR"},"presentmentMoney":{"amount":"41.00","currencyCode":"EUR"}}}`)
	writes := mapObjectUnder(spec, "store-1", line, "gid://shopify/Order/7", "", inlineRead)
	if len(writes) != 1 {
		t.Fatalf("writes = %d, want the refund's one", len(writes))
	}
	payload := writes[0].Payload
	for _, name := range refundDetail {
		if v, present := payload[name]; present {
			t.Errorf("%s = %#v: a list the document did not select was written, and would blank what a fetch recorded", name, v)
		}
	}
	// duties is a plain list of the refund, which a bulk line does select:
	// it is written empty as before.
	if v, present := payload["duties"]; !present {
		t.Error("duties, a plain list, was left out: only an inlined connection is")
	} else if list, ok := v.([]any); !ok || len(list) != 0 {
		t.Errorf("duties = %#v, want []", v)
	}
}

func TestAnInlinedListThatWasReadAndIsEmptyIsWrittenEmpty(t *testing.T) {
	obj := decodeOrigin(t, `{"id":"gid://shopify/Refund/72","updatedAt":"2026-11-10T08:00:00Z",
		"refundLineItems":{"nodes":[]},"refundShippingLines":{"nodes":[]},"orderAdjustments":{"nodes":[]}}`)
	writes := mapObjectUnder(generated.Types["refund"], "store-1", obj, "gid://shopify/Order/7", "", inlineRead)
	if len(writes) != 1 {
		t.Fatalf("writes = %d, want the refund's one", len(writes))
	}
	for _, name := range refundDetail {
		v, present := writes[0].Payload[name]
		list, ok := v.([]any)
		if !present || !ok || len(list) != 0 {
			t.Errorf("%s = %#v (present %t): a connection that was read and holds nothing is written []", name, v, present)
		}
	}
}

// A fetched order carries each refund's detail inside refunds { }, and the
// refund's write holds it: the refunded line, its quantity and amounts, and
// the refunded shipping.
func TestARefundsDetailIsWrittenWithItsOrder(t *testing.T) {
	order := decodeOrigin(t, `{"id":"gid://shopify/Order/7","updatedAt":"2026-11-10T09:00:00Z","name":"#1007",
		"refunds":[{"id":"gid://shopify/Refund/73","updatedAt":"2026-11-10T09:00:00Z",
			"totalRefundedSet":{"shopMoney":{"amount":"41.00","currencyCode":"EUR"},"presentmentMoney":{"amount":"41.00","currencyCode":"EUR"}},
			"refundLineItems":{"nodes":[{"lineItem":{"id":"gid://shopify/LineItem/701"},"quantity":1,
				"subtotalSet":{"shopMoney":{"amount":"36.00","currencyCode":"EUR"},"presentmentMoney":{"amount":"36.00","currencyCode":"EUR"}},
				"totalTaxSet":{"shopMoney":{"amount":"0.00","currencyCode":"EUR"},"presentmentMoney":{"amount":"0.00","currencyCode":"EUR"}}}]},
			"refundShippingLines":{"nodes":[{
				"subtotalAmountSet":{"shopMoney":{"amount":"5.00","currencyCode":"EUR"},"presentmentMoney":{"amount":"5.00","currencyCode":"EUR"}},
				"taxAmountSet":{"shopMoney":{"amount":"0.00","currencyCode":"EUR"},"presentmentMoney":{"amount":"0.00","currencyCode":"EUR"}}}]},
			"orderAdjustments":{"nodes":[]}}]}`)
	writes := mapObject(generated.Types["order"], "store-1", order, "", inlineRead)
	var refund map[string]any
	for _, w := range writes {
		if w.Concept == generated.ConceptID("refund") {
			refund = w.Payload
		}
	}
	if refund == nil {
		t.Fatalf("no refund write among %d", len(writes))
	}
	lines, _ := refund["refundLineItems"].([]any)
	if len(lines) != 1 {
		t.Fatalf("refundLineItems = %#v, want the one refunded line", refund["refundLineItems"])
	}
	line, _ := lines[0].(map[string]any)
	if got := inlineText(line, "quantity"); got != "1" {
		t.Errorf("the refunded line's quantity = %s, want 1", got)
	}
	if got := inlineText(line, "subtotalSet", "shopMoney", "amount"); got != "36.00" {
		t.Errorf("the refunded line's subtotal = %s, want 36.00", got)
	}
	if got := inlineText(line, "lineItem", "id"); got != "gid://shopify/LineItem/701" {
		t.Errorf("the refunded line names %s, want its order's line", got)
	}
	shipping, _ := refund["refundShippingLines"].([]any)
	if len(shipping) != 1 {
		t.Fatalf("refundShippingLines = %#v, want the refunded shipping", refund["refundShippingLines"])
	}
	if got := inlineText(shipping[0].(map[string]any), "subtotalAmountSet", "shopMoney", "amount"); got != "5.00" {
		t.Errorf("the refunded shipping = %s, want 5.00", got)
	}
	if adj, _ := refund["orderAdjustments"].([]any); adj == nil || len(adj) != 0 {
		t.Errorf("orderAdjustments = %#v, want [] (read, and nothing on it)", refund["orderAdjustments"])
	}
	if refund["parentGid"] != "gid://shopify/Order/7" || refund["versionSource"] != memqlsync.VersionFromOrigin {
		t.Errorf("the refund's write is %v, want it under its order with the origin's version", refund)
	}
}

// inlineText is a nested value as text, "" when absent.
func inlineText(m map[string]any, path ...string) string {
	var v any = m
	for _, k := range path {
		mm, _ := v.(map[string]any)
		v = mm[k]
	}
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	}
	b, _ := json.Marshal(v)
	return string(b)
}
