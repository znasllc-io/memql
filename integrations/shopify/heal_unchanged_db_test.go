package shopify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// heal_unchanged_db_test.go -- a reconciliation pass writes only the rows that
// changed, through the real engine and the real raw read (heal, mirrorHolds).
//
// The shape is the one that grew production's mirror: a price list re-listed
// every hour with its prices and quantity rules materialised beneath it, all
// three types publishing no version of their own, so every row of every pass
// arrived with a new syncedAt AND a new updatedAt. Before the check each pass
// appended a version of all three rows; this asserts none, then exactly the
// row whose origin value moved. A fake engine cannot measure it: whether a
// stored row compares equal depends on what the engine actually stores and
// hands back (numbers through a protobuf Struct, an omitted nil field, a
// relationship field holding a Shopify GID).
func TestAReconcilePassWritesOnlyTheRowsThatChanged(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	const store = "dbtest-heal-unchanged"
	concepts := []string{"v1:shopify:priceList", "v1:shopify:priceListPrice", "v1:shopify:quantityRule"}
	clear := func() {
		for _, concept := range concepts {
			_, _ = db.ExecContext(context.Background(),
				`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'storeId' = $2`, concept, store)
		}
	}
	clear()
	t.Cleanup(clear)
	versions := func(concept string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM "MemoryNodes" WHERE concept = $1 AND payload->>'storeId' = $2`, concept, store).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", concept, err)
		}
		return n
	}

	c := &Connector{engine: eng, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	spec := generated.Types["priceList"]
	listed := func(minimum int, amount string) map[string]any {
		return map[string]any{
			"id": "gid://shopify/PriceList/9001", "name": "Wholesale", "currency": "USD",
			"fixedPricesCount": json.Number("1"),
			"catalog":          map[string]any{"id": "gid://shopify/CompanyLocationCatalog/1"},
			// 10.0 is stored as a number and handed back as 10: equal.
			"parent": map[string]any{"adjustment": map[string]any{"type": "PERCENTAGE_DECREASE", "value": json.Number("10.0")}},
			"prices": map[string]any{"nodes": []any{map[string]any{
				"variant":        map[string]any{"id": "gid://shopify/ProductVariant/7"},
				"price":          map[string]any{"amount": amount, "currencyCode": "USD"},
				"compareAtPrice": nil,
				"originType":     "FIXED",
			}}},
			"quantityRules": map[string]any{"nodes": []any{map[string]any{
				"productVariant": map[string]any{"id": "gid://shopify/ProductVariant/7"},
				"minimum":        json.Number(strconv.Itoa(minimum)),
				"maximum":        nil,
				"increment":      json.Number("1"),
				"isDefault":      false,
				"originType":     "FIXED",
			}}},
		}
	}
	pass := func(at time.Time, obj map[string]any) int {
		t.Helper()
		healed, failed := c.heal(context.Background(), mapObject(spec, store, obj, "", at))
		if failed != 0 {
			t.Fatalf("%d row(s) failed to heal", failed)
		}
		return healed
	}
	want := func(priceList, price, rule int) {
		t.Helper()
		got := []int{versions(concepts[0]), versions(concepts[1]), versions(concepts[2])}
		if got[0] != priceList || got[1] != price || got[2] != rule {
			t.Fatalf("versions (priceList, priceListPrice, quantityRule) = %v, want [%d %d %d]", got, priceList, price, rule)
		}
	}

	start := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	if got := pass(start, listed(2, "9.50")); got != 3 {
		t.Fatalf("the first pass must write the price list and both children, healed %d", got)
	}
	want(1, 1, 1)

	// Three hourly passes over a price list nothing changed at the origin.
	for hour := 1; hour <= 3; hour++ {
		if got := pass(start.Add(time.Duration(hour)*time.Hour), listed(2, "9.50")); got != 0 {
			t.Fatalf("pass %d healed %d unchanged row(s); a fresh syncedAt and fetch-time updatedAt are not drift", hour, got)
		}
	}
	want(1, 1, 1)

	// A changed quantity rule is a write of that row alone, then a changed
	// price of that one.
	if got := pass(start.Add(4*time.Hour), listed(3, "9.50")); got != 1 {
		t.Fatalf("a changed minimum must heal exactly its quantity rule, healed %d", got)
	}
	want(1, 1, 2)
	if got := pass(start.Add(5*time.Hour), listed(3, "9.75")); got != 1 {
		t.Fatalf("a changed price must heal exactly its price row, healed %d", got)
	}
	want(1, 2, 2)
}
