package shopify

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// connector_reads_db_test.go -- the connector reading back the mirror rows
// it wrote, against a real engine over a real Postgres (F10).
//
// The generated mirror reads filter on `actor.isClusterOwner`, and the
// connector writes its mirror as a connector actor. A fake engine never
// lowers that conjunct, so only a real one can say whether the connector's
// own reads of its own rows answer anything: the full re-list's tombstone
// pass, the analytics walk and the product capability all depend on it.

// mirrorReadsStore is the store these rows are written for, carried as the
// Store value the connector's methods take.
func mirrorReadsStore() Store {
	return Store{
		ID:            authorityTestStoreId,
		Domain:        authorityTestDomain,
		AdminTokenRef: "ACME_ADMIN",
		Status:        StatusLive,
	}
}

// mirrorReadsConnector wires a connector against the real engine and, for
// the Admin side, the package's fake origin.
func mirrorReadsConnector(t *testing.T, eng memql.IntegrationEngineAccess) (*Connector, *fakeAdmin) {
	t.Helper()
	admin := newFakeAdmin(t)
	registry := NewStoreRegistry(eng, func(context.Context, string) (string, error) { return "shpat_test", nil })
	client := NewAdminClient()
	client.endpoint = func(Store) string { return admin.server.URL + "/graphql" }
	client.sleep = func(context.Context, time.Duration) error { return nil }
	return NewConnector(eng, slog.New(slog.DiscardHandler), registry, client), admin
}

// writeMirrored writes obj through the mirror write path: mapped as the
// apply path maps it, written with writeMirror.
func writeMirrored(t *testing.T, c *Connector, concept string, obj map[string]any) {
	t.Helper()
	for _, w := range mapObject(generated.Types[concept], authorityTestStoreId, obj, "", time.Now()) {
		if err := c.writeMirror(context.Background(), w); err != nil {
			t.Fatalf("write %s %v through the mirror path: %v", concept, obj["id"], err)
		}
	}
}

func cleanMirrored(t *testing.T, db *sql.DB, concept string) {
	t.Helper()
	clean := func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'storeId' = $2`,
			generated.ConceptID(concept), authorityTestStoreId)
	}
	clean()
	t.Cleanup(clean)
}

// liveGIDs is what a cluster owner reads of the store's live rows: the
// reachable positive the connector's own read is held against.
func liveGIDs(t *testing.T, eng *memql.MemQLEngine, concept string) []string {
	t.Helper()
	res, err := eng.Execute(ownerCtx(), renderCall(generated.Types[concept].ForStoreFn, map[string]any{"storeId": authorityTestStoreId}))
	if err != nil {
		t.Fatalf("read %s as a cluster owner: %v", concept, err)
	}
	var out []string
	for _, row := range memql.MaterializeRows(res) {
		out = append(out, mapString(row, "gid"))
	}
	sort.Strings(out)
	return out
}

func TestTheConnectorReadsBackTheMirrorRowsItWrote(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	cleanMirrored(t, db, "product")
	cleanMirrored(t, db, "order")
	cleanMirrored(t, db, "orderLineItem")
	c, _ := mirrorReadsConnector(t, eng)
	const gid = "gid://shopify/Product/5610"
	writeMirrored(t, c, "product", map[string]any{
		"id": gid, "title": "Read-back probe", "handle": "read-back-probe", "updatedAt": "2026-09-01T00:00:00Z",
	})

	// The row is there for somebody: without this an empty answer below
	// would read the same for a refused read and a write that never landed.
	if got := liveGIDs(t, eng, "product"); fmt.Sprint(got) != fmt.Sprint([]string{gid}) {
		t.Fatalf("a cluster owner reads %v, want the one product written", got)
	}

	// The connector's own read of its rows: the one the full re-list's
	// tombstone pass makes, with nothing seen, so every live row is named.
	absent, err := c.liveRowsAbsentFrom(context.Background(), mirrorReadsStore(), generated.Types["product"], map[string]bool{})
	if err != nil {
		t.Fatalf("the connector's read of its own mirror: %v", err)
	}
	if fmt.Sprint(absent) != fmt.Sprint([]string{gid}) {
		t.Errorf("the connector reads %v of the rows it wrote, want [%s]: its read of %s answers nothing it wrote",
			absent, gid, generated.Types["product"].ForStoreFn)
	}

	// The analytics walk reads an order's lines the same way.
	const orderGid = "gid://shopify/Order/5610"
	writeMirrored(t, c, "order", map[string]any{
		"id": orderGid, "name": "#5610", "updatedAt": "2026-09-01T00:00:00Z",
		"lineItems": map[string]any{"nodes": []any{map[string]any{"id": "gid://shopify/LineItem/5610", "name": "probe line", "quantity": 1}}},
	})
	lines, _, err := c.walk(context.Background(), "lineItemsForOrder", map[string]any{"storeId": authorityTestStoreId, "orderGid": orderGid})
	if err != nil || len(lines) != 1 || mapString(lines[0], "gid") != "gid://shopify/LineItem/5610" {
		t.Errorf("the analytics walk reads %v (%v) of the order's lines, want the one line written", lines, err)
	}

	// The product capability reads the same rows by handle and by gid.
	for _, by := range []struct{ gid, handle string }{{gid: gid}, {handle: "read-back-probe"}} {
		row, ok, err := c.mirroredProduct(context.Background(), mirrorReadsStore(), by.gid, by.handle)
		if err != nil || !ok || mapString(row, "gid") != gid {
			t.Errorf("mirroredProduct(gid %q, handle %q) = %v, %t, %v; want the product written", by.gid, by.handle, row, ok, err)
		}
	}
}

// otherStoreID holds a row of the same concept that store A's origin never
// lists: A's re-list must not reach it.
const otherStoreID = "dbtest-authority-other"

// latestRow is a row id's newest stored version: its payload, and how many
// versions there are.
func latestRow(t *testing.T, db *sql.DB, concept, rowID string) (string, int) {
	t.Helper()
	var payload string
	var versions int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) OVER (), payload::text FROM "MemoryNodes" WHERE concept = $1 AND id LIKE $2 ORDER BY "createdAt" DESC LIMIT 1`,
		generated.ConceptID(concept), "%"+rowID).Scan(&versions, &payload); err != nil {
		t.Fatalf("read %s %s: %v", concept, rowID, err)
	}
	return payload, versions
}

func latestDeleted(t *testing.T, db *sql.DB, concept, storeID, gid string) bool {
	t.Helper()
	var deleted bool
	if err := db.QueryRowContext(context.Background(),
		`SELECT COALESCE((payload->>'deleted')::boolean, false) FROM "MemoryNodes" WHERE concept = $1 AND payload->>'storeId' = $2 AND payload->>'gid' = $3 ORDER BY "createdAt" DESC LIMIT 1`,
		generated.ConceptID(concept), storeID, gid).Scan(&deleted); err != nil {
		t.Fatalf("read %s %s: %v", concept, gid, err)
	}
	return deleted
}

func TestAFullRelistTombstonesWhatTheOriginNoLongerHas(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	cleanMirrored(t, db, "giftCard")
	cleanStoreMirror(t, db, otherStoreID, "giftCard")
	rec := &recordingEngine{MemQLEngine: eng}
	c, admin := mirrorReadsConnector(t, rec)
	const kept, gone, others = "gid://shopify/GiftCard/5610", "gid://shopify/GiftCard/5611", "gid://shopify/GiftCard/5612"
	for _, gid := range []string{kept, gone} {
		writeMirrored(t, c, "giftCard", map[string]any{"id": gid, "note": "mirrored", "updatedAt": "2026-09-01T00:00:00Z"})
	}
	writeStoreMirrored(t, c, otherStoreID, "giftCard", map[string]any{"id": others, "note": "another store's", "updatedAt": "2026-09-01T00:00:00Z"})
	if got := liveGIDs(t, eng, "giftCard"); fmt.Sprint(got) != fmt.Sprint([]string{kept, gone}) {
		t.Fatalf("a cluster owner reads %v before the re-list, want both gift cards", got)
	}
	otherBefore, otherVersions := latestRow(t, db, "giftCard", MirrorRowID(otherStoreID, others))

	admin.reply("ShopifyListGiftCard", listPageReply("giftCards", false, "",
		map[string]any{"id": kept, "note": "mirrored", "updatedAt": "2026-09-01T00:00:00Z"}))
	rec.reset()
	if _, err := c.relist(context.Background(), mirrorReadsStore(), generated.Types["giftCard"]); err != nil {
		t.Fatalf("relist: %v", err)
	}

	if got := liveGIDs(t, eng, "giftCard"); fmt.Sprint(got) != fmt.Sprint([]string{kept}) {
		t.Errorf("after the re-list a cluster owner reads %v, want only %s: the card the origin no longer lists was not tombstoned", got, kept)
	}
	if !latestDeleted(t, db, "giftCard", authorityTestStoreId, gone) {
		t.Errorf("the absent card's latest row is not marked deleted")
	}

	// The other store's card: never in A's listing, still live and
	// unchanged, because A's pass reads and retires A's rows only.
	otherAfter, otherVersionsAfter := latestRow(t, db, "giftCard", MirrorRowID(otherStoreID, others))
	if otherAfter != otherBefore || otherVersionsAfter != otherVersions || latestDeleted(t, db, "giftCard", otherStoreID, others) {
		t.Errorf("store A's re-list changed another store's row:\n before (%d versions) %s\n after (%d versions) %s",
			otherVersions, otherBefore, otherVersionsAfter, otherAfter)
	}
	retired := 0
	for _, stmt := range rec.statements() {
		if !strings.Contains(stmt, `"deleted":true`) {
			continue
		}
		retired++
		if !strings.Contains(stmt, MirrorRowID(authorityTestStoreId, gone)) || !strings.Contains(stmt, `"storeId":"`+authorityTestStoreId+`"`) {
			t.Errorf("a tombstone that is not store A's absent card was written: %s", stmt)
		}
	}
	if retired != 1 {
		t.Errorf("%d tombstones were written, want the one absent card", retired)
	}
}

// The mirror's read pages 100 rows, newest first, and the oldest row is
// the likeliest to be gone: a tombstone pass that read one page would never
// compare it.
func TestAFullRelistTombstonesAnAbsentRowPastTheFirstPageOfTheMirror(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	cleanMirrored(t, db, "giftCard")
	c, admin := mirrorReadsConnector(t, eng)
	const cards = 101
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var listed []map[string]any
	oldest := ""
	for n := 0; n < cards; n++ {
		gid := fmt.Sprintf("gid://shopify/GiftCard/%d", 7000+n)
		card := map[string]any{"id": gid, "note": "mirrored", "updatedAt": base.Add(time.Duration(n) * time.Minute).Format(time.RFC3339)}
		writeMirrored(t, c, "giftCard", card)
		if n == 0 {
			oldest = gid
			continue
		}
		listed = append(listed, card)
	}

	admin.reply("ShopifyListGiftCard", listPageReply("giftCards", false, "", listed...))
	if _, err := c.relist(context.Background(), mirrorReadsStore(), generated.Types["giftCard"]); err != nil {
		t.Fatalf("relist: %v", err)
	}
	if !latestDeleted(t, db, "giftCard", authorityTestStoreId, oldest) {
		t.Errorf("the oldest card, on the mirror read's second page and absent from the origin, was not tombstoned")
	}
	var live int
	if err := db.QueryRowContext(context.Background(), `
		SELECT count(*) FROM (
		  SELECT DISTINCT ON (id) COALESCE((payload->>'deleted')::boolean, false) AS deleted
		  FROM "MemoryNodes" WHERE concept = $1 AND payload->>'storeId' = $2 ORDER BY id, "createdAt" DESC
		) latest WHERE NOT deleted`, generated.ConceptID("giftCard"), authorityTestStoreId).Scan(&live); err != nil {
		t.Fatalf("count live cards: %v", err)
	}
	if live != cards-1 {
		t.Errorf("%d cards are live after the re-list, want the %d the origin lists", live, cards-1)
	}
}
