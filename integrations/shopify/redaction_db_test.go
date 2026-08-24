package shopify

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// redaction_db_test.go -- customers/redact and shop/redact against a real
// Postgres (#4395).
//
// # Why this one cannot be faked
//
// The claim is "across EVERY VERSION". MemoryNodes is keyed (id, createdAt),
// so a row with history is several table rows, and the whole risk is a scrub
// that touches the newest one and leaves the customer's name in the versions
// a point-in-time read still returns. A fake with one row per id cannot
// distinguish the correct implementation from the broken one: it would pass
// either way, which is the worst kind of green.
//
// The second claim is what SURVIVES. Redaction keeps the commercial facts --
// quantities, totals, dates -- because the merchant's books are not the
// customer's personal data and destroying them is a different compliance
// failure. That is a property of the jsonb_set list, and it is asserted here
// rather than trusted.

const redactTestStore = "dbtest-acme"

func openTestDB(t *testing.T, purpose string) *sql.DB {
	t.Helper()
	dsn := dbtest.DSN()
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	// Leaked connections across the db-gated packages exhaust
	// max_connections for every suite sharing the lane's one database.
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, purpose, dsn, err)
		return nil
	}
	return db
}

// seedVersionedOrder writes THREE versions of one mirrored order, each with
// the customer's PII and each with a commercial fact that must survive.
func seedVersionedOrder(t *testing.T, db *sql.DB, rowID, customerGID string) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		payload := map[string]any{
			"storeId":        redactTestStore,
			"gid":            "gid://shopify/Order/9001",
			"customerGid":    customerGID,
			"email":          "buyer@example.com",
			"phone":          "+15550100",
			"name":           "#1001",
			"billingAddress": map[string]any{"address1": "1 Test Street", "city": "Testville"},
			"totalPriceSet":  map[string]any{"shopMoney": map[string]any{"amount": "42.00", "currencyCode": "USD"}},
			"updatedAt":      base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339),
			"syncedAt":       base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339),
			"deleted":        false,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", concept, type, schema, payload)
			 VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb)
			 ON CONFLICT (id, "createdAt") DO UPDATE SET payload = EXCLUDED.payload`,
			rowID, base.Add(time.Duration(i)*time.Hour), "system:connector:shopify",
			generated.ConceptID("order"), "object", `{"type":"object"}`, string(raw)); err != nil {
			t.Fatalf("seed version %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = $1 AND payload->>'storeId' = $2`,
			generated.ConceptID("order"), redactTestStore)
	})
}

func payloadsFor(t *testing.T, db *sql.DB, rowID string) []map[string]any {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT payload::text FROM "MemoryNodes" WHERE id = $1 ORDER BY "createdAt"`, rowID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, payload)
	}
	return out
}

func TestRedactCustomerScrubsEveryVersionAndKeepsTheCommercialFacts(t *testing.T) {
	db := openTestDB(t, "customers/redact across every version (memql#4395)")
	if db == nil {
		return
	}
	const rowID = "shp-redact-1"
	const customerGID = "gid://shopify/Customer/9991"
	seedVersionedOrder(t, db, rowID, customerGID)

	before := payloadsFor(t, db, rowID)
	if len(before) != 3 {
		t.Fatalf("seeded %d versions, want 3 -- the whole point is that there is history", len(before))
	}

	h := newHarness(t)
	h.conn.WithDatabase(func() *sql.DB { return db })
	store := Store{ID: redactTestStore, Domain: "acme.myshopify.com"}

	if _, err := h.conn.RedactCustomer(context.Background(), store, customerGID); err != nil {
		t.Fatal(err)
	}

	after := payloadsFor(t, db, rowID)
	if len(after) != 3 {
		t.Fatalf("redaction changed the version count to %d -- it rewrites history, it does not delete it", len(after))
	}
	for i, payload := range after {
		for _, field := range []string{"email", "phone", "name", "billingAddress"} {
			if got := payload[field]; got != RedactionMarker {
				t.Errorf("version %d kept %s = %v, want %q", i, field, got, RedactionMarker)
			}
		}
		// WHAT SURVIVES. The opaque GID stays so the row is still
		// attributable to a customer record Shopify has itself redacted,
		// and the money stays because the merchant's books are not the
		// customer's personal data.
		if got := payload["customerGid"]; got != customerGID {
			t.Errorf("version %d lost the opaque GID: %v", i, got)
		}
		total, _ := payload["totalPriceSet"].(map[string]any)
		shop, _ := total["shopMoney"].(map[string]any)
		if shop["amount"] != "42.00" {
			t.Errorf("version %d lost the order total: %v", i, payload["totalPriceSet"])
		}
		if payload["storeId"] != redactTestStore {
			t.Errorf("version %d lost its store scope", i)
		}
	}
}

// A field the concept does not carry must be left ALONE rather than invented.
// create_missing=false on every jsonb_set is what makes that true, and
// without it a redaction would add a dozen "[redacted]" keys to every row of
// every concept it touched.
func TestRedactionInventsNoFields(t *testing.T) {
	db := openTestDB(t, "customers/redact invents no fields (memql#4395)")
	if db == nil {
		return
	}
	const rowID = "shp-redact-2"
	const customerGID = "gid://shopify/Customer/9992"
	seedVersionedOrder(t, db, rowID, customerGID)

	before := payloadsFor(t, db, rowID)
	h := newHarness(t)
	h.conn.WithDatabase(func() *sql.DB { return db })
	if _, err := h.conn.RedactCustomer(context.Background(), Store{ID: redactTestStore}, customerGID); err != nil {
		t.Fatal(err)
	}
	after := payloadsFor(t, db, rowID)

	for i := range after {
		for key := range after[i] {
			if _, present := before[i][key]; !present {
				t.Errorf("version %d gained the key %q, which the row never had", i, key)
			}
		}
	}
}

// A customer the row does not mention must not be scrubbed by somebody
// else's request. The LIKE on the whole payload is what scopes it, and this
// is the test that keeps that scoping honest.
func TestRedactionLeavesAnotherCustomersRowsAlone(t *testing.T) {
	db := openTestDB(t, "customers/redact is scoped to one customer (memql#4395)")
	if db == nil {
		return
	}
	const rowID = "shp-redact-3"
	seedVersionedOrder(t, db, rowID, "gid://shopify/Customer/9993")

	h := newHarness(t)
	h.conn.WithDatabase(func() *sql.DB { return db })
	if _, err := h.conn.RedactCustomer(context.Background(), Store{ID: redactTestStore}, "gid://shopify/Customer/OTHER"); err != nil {
		t.Fatal(err)
	}
	for i, payload := range payloadsFor(t, db, rowID) {
		if payload["email"] != "buyer@example.com" {
			t.Errorf("version %d was scrubbed by another customer's request: %v", i, payload["email"])
		}
	}
}

func TestPurgeStoreRemovesEveryVersionAndItsSyncState(t *testing.T) {
	db := openTestDB(t, "shop/redact purge (memql#4395)")
	if db == nil {
		return
	}
	const rowID = "shp-purge-1"
	seedVersionedOrder(t, db, rowID, "gid://shopify/Customer/9994")

	ctx := context.Background()
	// The runtime owns v1:platform:syncState and keys it by (concept,
	// connector, direction) rather than by store; the purge removes the
	// rows for this connector's concepts, which is what a re-install must
	// not resume a backfill from.
	syncID := "syn-" + redactTestStore
	syncPayload, _ := json.Marshal(map[string]any{
		"connector": ConnectorName, "storeId": redactTestStore,
		"conceptId": generated.ConceptID("order"), "direction": "inbound",
	})
	if _, err := db.ExecContext(ctx,
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", concept, type, schema, payload)
		 VALUES ($1, $2, $3, 'v1:platform:syncState', 'object', '{"type":"object"}'::jsonb, $4::jsonb)
		 ON CONFLICT (id, "createdAt") DO UPDATE SET payload = EXCLUDED.payload`,
		syncID, time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC), "system:connector:shopify", string(syncPayload)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id = $1`, syncID)
	})

	h := newHarness(t)
	h.conn.WithDatabase(func() *sql.DB { return db })
	removed, err := h.conn.PurgeStore(ctx, Store{ID: redactTestStore, Domain: "acme.myshopify.com"})
	if err != nil {
		t.Fatal(err)
	}
	if removed < 3 {
		t.Fatalf("removed %d table rows, want at least the three versions", removed)
	}
	if got := payloadsFor(t, db, rowID); len(got) != 0 {
		t.Fatalf("%d version(s) survived the purge", len(got))
	}
	var syncRows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id = $1`, syncID).Scan(&syncRows); err != nil {
		t.Fatal(err)
	}
	if syncRows != 0 {
		t.Error("the store's sync state survived the purge, so a re-install would resume a backfill of rows that are gone")
	}
	// The store row is stamped rather than deleted: it is the audit record
	// that a purge happened.
	if len(h.engine.callsTo("markStoreRedacted")) != 1 {
		t.Errorf("the purge was not recorded on the store row: %v", h.engine.calls())
	}
	if !strings.Contains(strings.Join(h.engine.callsTo("markStoreRedacted"), " "), "redactedAt") {
		t.Error("markStoreRedacted carried no timestamp")
	}
}
