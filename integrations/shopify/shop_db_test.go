package shopify

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// shop_db_test.go -- a connected store's shop row, read from the origin and
// written through the real engine with no SHOP_UPDATE ever delivered.

func TestAConnectedStoreHasItsCurrencyAndTimeZoneWithoutAShopUpdate(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	cleanMirrored(t, db, "shop")
	c, admin := mirrorReadsConnector(t, eng)
	store := mirrorReadsStore()
	admin.reply(shopFetchOp, shopAnswer("EUR", "Europe/Madrid", "2026-08-01T00:00:00Z"))

	readShop := func() map[string]any {
		t.Helper()
		res, err := eng.Execute(ownerCtx(), renderCall(generated.Types["shop"].ForStoreFn, map[string]any{"storeId": store.ID}))
		if err != nil {
			t.Fatalf("read the store's shop as a cluster owner: %v", err)
		}
		rows := memql.MaterializeRows(res)
		if len(rows) != 1 {
			t.Fatalf("the mirror holds %d shop rows for the store, want one", len(rows))
		}
		return rows[0]
	}

	if err := c.refreshShop(context.Background(), store); err != nil {
		t.Fatalf("refreshShop: %v", err)
	}
	row := readShop()
	if got := mapString(row, "currencyCode"); got != "EUR" {
		t.Errorf("currencyCode = %q, want EUR", got)
	}
	if got := mapString(row, "ianaTimezone"); got != "Europe/Madrid" {
		t.Errorf("ianaTimezone = %q, want Europe/Madrid", got)
	}
	plan, _ := row["plan"].(map[string]any)
	if got := mapString(plan, "publicDisplayName"); got != "Basic" {
		t.Errorf("plan = %v, want the shop's plan as the origin answered it", row["plan"])
	}

	// The connector's own read sees it too: a boot does not read it again.
	if has, err := c.hasShopRow(context.Background(), store); err != nil || !has {
		t.Errorf("hasShopRow = %v, %v; want the row the read wrote", has, err)
	}

	// A second read of the same answer writes nothing: a boot on every node,
	// or a resume seen by both the act and the watch, writes once.
	versions := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM "MemoryNodes" WHERE concept = $1 AND payload->>'storeId' = $2`,
			generated.ConceptID("shop"), store.ID).Scan(&n); err != nil {
			t.Fatalf("count the shop's versions: %v", err)
		}
		return n
	}
	before := versions()
	if err := c.refreshShop(context.Background(), store); err != nil {
		t.Fatalf("refreshShop again: %v", err)
	}
	if after := versions(); after != before {
		t.Errorf("an unchanged shop was written again: %d versions, then %d", before, after)
	}

	// A SHOP_UPDATE that recorded a newer state is not undone by a read
	// whose answer is older: the shop's own updatedAt is its version.
	newer := shopAnswer("USD", "America/Chicago", "2026-09-01T00:00:00Z")["shop"].(map[string]any)
	writeMirrored(t, c, "shop", newer)
	if err := c.refreshShop(context.Background(), store); err != nil {
		t.Fatalf("refreshShop with an older answer: %v", err)
	}
	if got := mapString(readShop(), "currencyCode"); got != "USD" {
		t.Errorf("currencyCode = %q after an older read, want the newer delivery's USD", got)
	}
}
