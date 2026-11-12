package shopify

import (
	"context"
	"log/slog"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// store_health_db_test.go -- two writers of a store's health keep each
// other's key, against a real engine.
//
// A store's health is one object on its row, and recordStoreHealth replaces
// the whole of it. The subscription pass wrote it from a copy of the store it
// was handed -- for a connect, the copy read before the connect -- so a key
// any other writer had added in between was erased by the next pass. Each
// writer now re-reads the row and replaces one key. Only the engine's own
// read-merge can show the second write kept the first's key.

func TestTwoStoreHealthKeysWrittenOneAfterTheOtherBothSurvive(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	seedStore(t, eng, db)
	c := NewConnector(eng, slog.New(slog.DiscardHandler), NewStoreRegistry(eng, nil), NewAdminClient())
	ctx := context.Background()

	if err := c.recordStoreHealthKey(ctx, authorityTestStoreId, "ordersFrom", "2026-07-25", nil); err != nil {
		t.Fatalf("the first key: %v", err)
	}
	if err := c.recordStoreHealthKey(ctx, authorityTestStoreId, "subscriptions", map[string]any{"desired": 3}, nil); err != nil {
		t.Fatalf("the second key: %v", err)
	}

	store, found, err := c.storeByID(memql.ContextWithFreshRead(operatorContext(ctx)), authorityTestStoreId)
	if err != nil || !found {
		t.Fatalf("read the store back: found %t, %v", found, err)
	}
	if got := mapString(store.Health, "ordersFrom"); got != "2026-07-25" {
		t.Errorf("ordersFrom = %q after the subscriptions key was written: the second writer erased the first's key (health %v)", got, store.Health)
	}
	if subs, ok := store.Health["subscriptions"].(map[string]any); !ok || mapInt(subs, "desired") != 3 {
		t.Errorf("subscriptions = %v, want the second key as written", store.Health["subscriptions"])
	}
}
