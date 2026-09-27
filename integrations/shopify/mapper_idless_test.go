package shopify

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// mapper_idless_test.go -- children that declare no id still become rows.
//
// PriceListPrice and QuantityRule are not Nodes. The generator used to ask
// them for an id anyway and the Admin API refused the whole price-list
// document; now that it asks for none, the children arrive without one,
// and a mapper that keys every row on `id` would drop them in silence --
// the parent lands, its prices do not, and nothing says so. The wholesale
// pack reads both concepts.

func TestAnIdlessChildIsKeyedByItsParentAndTheVariantItPrices(t *testing.T) {
	spec := generated.Types["priceList"]
	if spec == nil {
		t.Skip("priceList is not in the generated model")
	}
	obj := map[string]any{
		"id":   "gid://shopify/PriceList/1",
		"name": "Wholesale",
		"prices": map[string]any{"nodes": []any{map[string]any{
			"price":   map[string]any{"amount": "9.50", "currencyCode": "USD"},
			"variant": map[string]any{"id": "gid://shopify/ProductVariant/7"},
		}}},
		"quantityRules": map[string]any{"nodes": []any{map[string]any{
			"minimum":        float64(6),
			"increment":      float64(6),
			"productVariant": map[string]any{"id": "gid://shopify/ProductVariant/7"},
		}}},
	}
	writes := mapObject(spec, "acme", obj, "", time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC))
	byConcept := map[string]int{}
	for _, w := range writes {
		byConcept[w.Concept]++
	}
	if byConcept["v1:shopify:priceListPrice"] != 1 || byConcept["v1:shopify:quantityRule"] != 1 {
		t.Fatalf("writes by concept = %v; both id-less children must become rows", byConcept)
	}
	for _, w := range writes[1:] {
		gid, _ := w.Payload["gid"].(string)
		want := "gid://shopify/PriceList/1#gid://shopify/ProductVariant/7"
		if gid != want {
			t.Errorf("%s gid = %q, want %q", w.Concept, gid, want)
		}
		if w.RowId != MirrorRowID("acme", want) {
			t.Errorf("%s row id is not derived from the composite key", w.Concept)
		}
		if pg, _ := w.Payload["parentGid"].(string); pg != "gid://shopify/PriceList/1" {
			t.Errorf("%s lost its parent: %q", w.Concept, pg)
		}
	}
}

func TestAnIdlessBulkLineIsTheChildWhoseKeyItCarries(t *testing.T) {
	h := newHarness(t)
	spec := generated.Types["priceList"]
	if spec == nil {
		t.Skip("priceList is not in the generated model")
	}
	price := h.conn.mapBulkLine(h.store(t), spec, map[string]any{
		"__parentId": "gid://shopify/PriceList/1",
		"price":      map[string]any{"amount": "9.50", "currencyCode": "USD"},
		"variant":    map[string]any{"id": "gid://shopify/ProductVariant/7"},
	})
	if len(price) != 1 || price[0].Concept != "v1:shopify:priceListPrice" {
		t.Fatalf("price line -> %+v", price)
	}
	rule := h.conn.mapBulkLine(h.store(t), spec, map[string]any{
		"__parentId":     "gid://shopify/PriceList/1",
		"minimum":        float64(6),
		"productVariant": map[string]any{"id": "gid://shopify/ProductVariant/7"},
	})
	if len(rule) != 1 || rule[0].Concept != "v1:shopify:quantityRule" {
		t.Fatalf("rule line -> %+v", rule)
	}
	// A line no child can claim is dropped, not guessed.
	if orphan := h.conn.mapBulkLine(h.store(t), spec, map[string]any{"__parentId": "gid://shopify/PriceList/1", "minimum": float64(6)}); len(orphan) != 0 {
		t.Errorf("an unclaimable line became %+v", orphan)
	}
	_ = context.Background
}
