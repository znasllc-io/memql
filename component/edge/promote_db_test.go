package edge_test

// promote_db_test.go -- the artifact-promotion round trip (memql#3768, epic
// #3748 §4.2), against a real database.
//
// # Why this one cannot be mocked
//
// The claim under test is not "the function writes the right value" -- that a
// fake would prove. It is that the promote is ONE TRANSACTION ACROSS TWO
// SCHEMAS, which is the design's whole justification for choosing two schemas
// over two databases (D1). A fake database has no schemas and no transaction, so
// it would pass whether or not the property holds, and the property is the
// reason the epic is shaped the way it is.
//
// Per the house rule these run against a throwaway TimescaleDB container, never
// a bootstrapped cluster database.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/core/common"
)

const (
	stagingSchema = "memql_promote_staging"
	prodSchema    = "memql_promote_prod"
	siteID        = "v1:platform:site:shop"
)

// openAdmin returns a connection with no environment search path. Promote names
// its schemas in every statement, so it does not care which environment the
// connection points at -- and that is itself worth exercising here rather than
// quietly connecting as staging and never finding out.
func openAdmin(t *testing.T) *bun.DB {
	t.Helper()
	t.Setenv("MEMQL_DB_SEARCH_PATH", "")

	db, err := database.NewDatabase(common.ComponentName("promoteTest"),
		database.WithDSN(dbtest.DSN()),
		(&database.Database{}).WithMigrateOnStart(false),
	)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db.Start(ctx)

	waitCtx, waitCancel := context.WithTimeout(ctx, 15*time.Second)
	defer waitCancel()
	select {
	case <-db.Ready():
	case <-waitCtx.Done():
		dbtest.Unreachable(t, purpose, dbtest.SafeDSN(dbtest.DSN()), waitCtx.Err())
	}

	bunDB := db.BunDB()
	if bunDB == nil {
		dbtest.Unreachable(t, purpose, dbtest.SafeDSN(dbtest.DSN()), errors.New("no bun handle after Ready"))
	}
	if err := bunDB.PingContext(waitCtx); err != nil {
		dbtest.Unreachable(t, purpose, dbtest.SafeDSN(dbtest.DSN()), err)
	}
	return bunDB
}

// purpose names this suite in the skip message dbtest emits when no database is
// reachable, so "no database" is never mistaken for "the promote is broken".
const purpose = "DB-gated test for the cross-schema site promote"

// seedSchemas builds the node table in both schemas, shaped as the initial
// migration creates it. Built directly rather than by migrating: what is under
// test is which SCHEMA a row lands in, and a full migration per schema would
// make each case minutes long to assert the same thing.
func seedSchemas(t *testing.T, db *bun.DB) {
	t.Helper()
	ctx := context.Background()
	for _, schema := range []string{stagingSchema, prodSchema} {
		for _, stmt := range []string{
			`CREATE SCHEMA IF NOT EXISTS ` + schema,
			`CREATE TABLE IF NOT EXISTS ` + schema + `."MemoryNodes" (
				id TEXT NOT NULL, "createdAt" TIMESTAMPTZ NOT NULL, "createdBy" TEXT NOT NULL,
				schema JSONB NOT NULL, payload JSONB NOT NULL, metadata JSONB NOT NULL DEFAULT '{}',
				"type" TEXT NOT NULL DEFAULT 'object', concept TEXT NOT NULL,
				provenance JSONB NOT NULL DEFAULT '{}',
				PRIMARY KEY (id, "createdAt"))`,
			`DELETE FROM ` + schema + `."MemoryNodes"`,
		} {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("seed %s: %v", schema, err)
			}
		}
	}
	t.Cleanup(func() {
		for _, schema := range []string{stagingSchema, prodSchema} {
			_, _ = db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		}
	})
}

// putSite writes one version of a site row into a schema.
func putSite(t *testing.T, db *bun.DB, schema, hostname, bundleRef string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"hostname": hostname, "kind": "spa", "bundleRef": bundleRef, "status": "live",
	})
	if err != nil {
		t.Fatalf("encoding payload: %v", err)
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO `+schema+`."MemoryNodes"
		   (id, "createdAt", "createdBy", concept, "type", schema, payload, metadata, provenance)
		 VALUES (?, now(), 'v1:identity:user:tester', 'v1:platform:site', 'object', '{}', ?, '{}', '{}')`,
		siteID, string(payload)); err != nil {
		t.Fatalf("seeding site in %s: %v", schema, err)
	}
}

// siteIn reads the newest version of the site row from one schema.
func siteIn(t *testing.T, db *bun.DB, schema string) map[string]any {
	t.Helper()
	var raw json.RawMessage
	err := db.QueryRowContext(context.Background(),
		`SELECT payload FROM `+schema+`."MemoryNodes"
		  WHERE id = ? AND concept = 'v1:platform:site'
		  ORDER BY "createdAt" DESC LIMIT 1`, siteID).Scan(&raw)
	if err != nil {
		t.Fatalf("reading site in %s: %v", schema, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding site in %s: %v", schema, err)
	}
	return out
}

func versionsIn(t *testing.T, db *bun.DB, schema string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM `+schema+`."MemoryNodes" WHERE id = ?`, siteID).Scan(&n); err != nil {
		t.Fatalf("counting versions in %s: %v", schema, err)
	}
	return n
}

// TestPromoteRoundTrip is the acceptance criterion in one case: publish to
// staging, promote, verify production carries the new bundle, roll back, verify
// it carries the old one.
//
// Rollback goes through SetBundleRef -- the SAME function promote's write half
// calls -- with the PreviousBundleRef the promote handed back. That is the
// design's "rollback is the same write with the previous value, not a distinct
// code path", exercised rather than asserted in prose.
func TestPromoteRoundTrip(t *testing.T) {
	db := openAdmin(t)
	seedSchemas(t, db)

	// The two rows share an id and differ in hostname, which is D5: a staging
	// site lives under the CLUSTER's domain, production under the customer's.
	putSite(t, db, stagingSchema, "shop.staging.memql.localhost", "blob://sites/shop/v4.3.0/")
	putSite(t, db, prodSchema, "shop.acme.com", "blob://sites/shop/v4.2.0/")

	p := edge.NewPromoter(db)
	res, err := p.Promote(context.Background(), stagingSchema, prodSchema, siteID, "")
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if res.PreviousBundleRef != "blob://sites/shop/v4.2.0/" {
		t.Errorf("previous bundleRef = %q, want the version production was on", res.PreviousBundleRef)
	}
	if res.Created || res.NoOp {
		t.Errorf("promote onto an existing row reported created=%v noop=%v, want both false", res.Created, res.NoOp)
	}

	prod := siteIn(t, db, prodSchema)
	if got := prod["bundleRef"]; got != "blob://sites/shop/v4.3.0/" {
		t.Errorf("production bundleRef = %v, want the version staging was serving", got)
	}

	// THE OTHER HALF OF THE ACCEPTANCE, and the one a careless promote breaks:
	// only the reference moves. Production keeps its own hostname, because the
	// staging hostname is not a valid production hostname and never was.
	if got := prod["hostname"]; got != "shop.acme.com" {
		t.Errorf("production hostname = %v, want it untouched -- a promote moves the bundle reference, not the row", got)
	}

	// Rollback: the same write, the previous value.
	back, err := p.SetBundleRef(context.Background(), prodSchema, siteID, res.PreviousBundleRef)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if back.BundleRef != "blob://sites/shop/v4.2.0/" {
		t.Errorf("rollback wrote %q, want the prior version", back.BundleRef)
	}
	if got := siteIn(t, db, prodSchema)["bundleRef"]; got != "blob://sites/shop/v4.2.0/" {
		t.Errorf("after rollback production serves %v, want the prior version", got)
	}

	// Staging is untouched throughout. A promote reads it; nothing writes back.
	if got := siteIn(t, db, stagingSchema)["bundleRef"]; got != "blob://sites/shop/v4.3.0/" {
		t.Errorf("staging bundleRef = %v; a promote must not write to the source environment", got)
	}
	if n := versionsIn(t, db, stagingSchema); n != 1 {
		t.Errorf("staging holds %d versions, want the 1 it was seeded with", n)
	}
}

// TestPromoteCreatesASiteAbsentFromProduction is the acceptance line
// "promoting a site absent from production creates it" -- first-publish and
// promote are the same act, so the operator does not need a separate
// bootstrapping step for a site's first trip to production.
//
// The hostname must be supplied, and that is D5 rather than an API wart: a
// production hostname is NOT derivable from a staging one, because staging
// lives under the cluster's domain and production under whatever DNS the
// customer controls.
func TestPromoteCreatesASiteAbsentFromProduction(t *testing.T) {
	db := openAdmin(t)
	seedSchemas(t, db)
	putSite(t, db, stagingSchema, "shop.staging.memql.localhost", "blob://sites/shop/v1.0.0/")

	p := edge.NewPromoter(db)
	res, err := p.Promote(context.Background(), stagingSchema, prodSchema, siteID, "shop.acme.com")
	if err != nil {
		t.Fatalf("Promote onto an absent row: %v", err)
	}
	if !res.Created {
		t.Error("promote onto an absent row did not report Created")
	}
	if res.PreviousBundleRef != "" {
		t.Errorf("PreviousBundleRef = %q on a created row, want empty -- there is nothing to roll back to", res.PreviousBundleRef)
	}

	prod := siteIn(t, db, prodSchema)
	if got := prod["bundleRef"]; got != "blob://sites/shop/v1.0.0/" {
		t.Errorf("created row bundleRef = %v", got)
	}
	if got := prod["hostname"]; got != "shop.acme.com" {
		t.Errorf("created row hostname = %v, want the supplied production hostname, never staging's", got)
	}
	// The rest of the row carries over, so the site is actually servable rather
	// than a stub the operator has to finish by hand.
	if got := prod["kind"]; got != "spa" {
		t.Errorf("created row kind = %v, want it seeded from the source row", got)
	}
}

// TestPromoteWithoutAHostnameRefusesRatherThanGuessing pins the other side of
// D5. Deriving `shop.acme.com` from `shop.staging.memql.localhost` is not
// possible, so the only alternatives to refusing are inventing a hostname or
// copying staging's -- and copying staging's would publish production at a host
// under the cluster's own domain, silently.
func TestPromoteWithoutAHostnameRefusesRatherThanGuessing(t *testing.T) {
	db := openAdmin(t)
	seedSchemas(t, db)
	putSite(t, db, stagingSchema, "shop.staging.memql.localhost", "blob://sites/shop/v1.0.0/")

	_, err := edge.NewPromoter(db).Promote(context.Background(), stagingSchema, prodSchema, siteID, "")
	if err == nil {
		t.Fatal("promote created a production row with no hostname")
	}
	if !strings.Contains(err.Error(), "hostname is required") {
		t.Errorf("error = %v, want it to name the missing hostname", err)
	}
	if n := versionsIn(t, db, prodSchema); n != 0 {
		t.Errorf("production holds %d versions after a refused promote, want 0", n)
	}
}

// TestPromoteIsOneTransaction is the design's justification, asserted.
//
// A failing promote must leave production exactly as it was. The failure is
// induced at the point that matters -- after the source has been read, while the
// target write is in flight -- by removing the target table underneath the
// transaction, so the write raises and the transaction rolls back. If the read
// and the write were separate transactions, production would be left carrying
// whatever the write managed before the error.
func TestPromoteIsOneTransaction(t *testing.T) {
	db := openAdmin(t)
	seedSchemas(t, db)
	putSite(t, db, stagingSchema, "shop.staging.memql.localhost", "blob://sites/shop/v4.3.0/")
	putSite(t, db, prodSchema, "shop.acme.com", "blob://sites/shop/v4.2.0/")

	// Promote into a schema that exists but whose table does not: the source
	// read succeeds, the target write fails.
	if _, err := db.ExecContext(context.Background(),
		`DROP TABLE `+prodSchema+`."MemoryNodes"`); err != nil {
		t.Fatalf("removing the target table: %v", err)
	}

	if _, err := edge.NewPromoter(db).Promote(
		context.Background(), stagingSchema, prodSchema, siteID, ""); err == nil {
		t.Fatal("promote reported success against a missing target table")
	}

	// The source is intact: a half-applied promote would have consumed it.
	if got := siteIn(t, db, stagingSchema)["bundleRef"]; got != "blob://sites/shop/v4.3.0/" {
		t.Errorf("staging bundleRef = %v after a failed promote, want it untouched", got)
	}
}

// TestPromotingTheSameVersionTwiceWritesNoNewVersion. The graph's row history IS
// a site's version list -- that is why the site concept carries no version
// field -- so an idempotent re-promote that appended would put a deploy in the
// operator's timeline that never happened.
func TestPromotingTheSameVersionTwiceWritesNoNewVersion(t *testing.T) {
	db := openAdmin(t)
	seedSchemas(t, db)
	putSite(t, db, stagingSchema, "shop.staging.memql.localhost", "blob://sites/shop/v4.3.0/")
	putSite(t, db, prodSchema, "shop.acme.com", "blob://sites/shop/v4.2.0/")

	p := edge.NewPromoter(db)
	ctx := context.Background()
	if _, err := p.Promote(ctx, stagingSchema, prodSchema, siteID, ""); err != nil {
		t.Fatalf("first promote: %v", err)
	}
	after := versionsIn(t, db, prodSchema)

	res, err := p.Promote(ctx, stagingSchema, prodSchema, siteID, "")
	if err != nil {
		t.Fatalf("second promote: %v", err)
	}
	if !res.NoOp {
		t.Error("re-promoting the pinned version did not report NoOp")
	}
	if n := versionsIn(t, db, prodSchema); n != after {
		t.Errorf("production holds %d versions after a re-promote, want %d -- a no-op must not append", n, after)
	}
}

// TestPromoteRefusesAnInvalidSchemaName. Schema names reach SQL by string
// interpolation, because a schema cannot be a bound parameter. That makes the
// validator the thing standing between an operator-supplied environment name and
// the query text, so it is tested as the security control it is rather than as
// an input-validation nicety.
func TestPromoteRefusesAnInvalidSchemaName(t *testing.T) {
	db := openAdmin(t)
	seedSchemas(t, db)

	p := edge.NewPromoter(db)
	for _, bad := range []string{
		`memql_prod"; DROP TABLE "MemoryNodes`,
		"memql prod",
		"MemqlProd",
		"",
		"1memql",
	} {
		if _, err := p.Promote(context.Background(), stagingSchema, bad, siteID, ""); err == nil {
			t.Errorf("promote admitted %q as a target schema", bad)
		}
		if _, err := p.Promote(context.Background(), bad, prodSchema, siteID, ""); err == nil {
			t.Errorf("promote admitted %q as a source schema", bad)
		}
	}
}
