package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// TestThePriceListMaterializes boots an engine on a database with this bundle
// mounted, runs the real SeedMaterializer, and requires every `seed tierSpec`
// in seeds.memql to land as a v1:fleet:tierSpec row carrying the seed's
// numbers (memql#5437 review).
//
// The price list could never materialize before createTierSpec existed: the
// materializer writes a seed through `create<Concept>` and nothing else, and
// the bundle deliberately had no tier mutation -- so every tier seed failed at
// boot with the mutation missing, and the pricing page, Orbit and the allowance
// read an empty catalog while this bundle's other gates stayed green over the
// seeds' TEXT.
//
// The other half is the reason the mutation is @serverOnly: a client-origin
// call to it is refused, so the only writer of the price list is the seed path.
func TestThePriceListMaterializes(t *testing.T) {
	ctx := context.Background()
	reachable, err := dbtest.EnsureSchema(ctx)
	if err != nil {
		t.Fatalf("dbtest.EnsureSchema: %v", err)
	}
	dsn := dbtest.DSN()
	if !reachable {
		dbtest.Unreachable(t, "fleet price list materialization", dsn, errors.New("no database reachable"))
		return
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "fleet price list materialization", dsn, err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	_, _, unmount := memqldsl.MountOverlayDomains(quiet, os.DirFS(bundleRoot(t)))
	t.Cleanup(func() {
		unmount()
		concept.ReplaceAll(nil)
		_, _ = memql.LoadUnifiedConcepts(quiet)
	})
	if _, err := memql.LoadUnifiedConcepts(quiet); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(db)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = quiet
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init with the fleet bundle mounted: %v", err)
	}

	materializer := memql.NewSeedMaterializer(eng, eng.Seeds())
	if err := materializer.Start(ctx); err != nil {
		t.Fatalf("SeedMaterializer.Start: %v", err)
	}
	t.Cleanup(func() { _ = materializer.Stop(ctx) })

	// Every stored version of the concept, newest first, folded to the latest
	// per id -- the table is append-only.
	var rows []struct {
		ID      string          `bun:"id"`
		Payload json.RawMessage `bun:"payload"`
	}
	if err := db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("id, payload").
		Where("concept = ?", "v1:fleet:tierSpec").
		OrderExpr(`"createdAt" DESC`).Scan(ctx, &rows); err != nil {
		t.Fatalf("read the price list back: %v", err)
	}
	byTier := map[string]map[string]any{}
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		var p map[string]any
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", r.ID, err)
		}
		if tier, _ := p["tier"].(string); tier != "" {
			byTier[tier] = p
		}
	}

	seeds := parseTierSeeds(t)
	for tier, seed := range seeds {
		row, ok := byTier[tier]
		if !ok {
			t.Errorf("seed for tier %q did not materialize: no v1:fleet:tierSpec row carries it. The materializer writes a seed through create<Concept>, so without createTierSpec the price list is empty.", tier)
			continue
		}
		for field, want := range seed.numbers {
			if field == "tier" {
				continue
			}
			if got, _ := row[field].(float64); got != want {
				t.Errorf("tier %q: %s = %v, the seed says %v", tier, field, row[field], want)
			}
		}
		for _, field := range []string{"overagePolicy", "instanceProfile", "dbPreset", "status", "displayName"} {
			if got, _ := row[field].(string); got != seed.fields[field] {
				t.Errorf("tier %q: %s = %q, the seed says %q", tier, field, got, seed.fields[field])
			}
		}
	}
	if len(seeds) < 5 {
		t.Fatalf("parsed %d tier seeds; five were measured -- the seed read has stopped reaching them", len(seeds))
	}

	// A client cannot reach the price list's one mutation.
	client := auth.ContextWithUserActor(ctx, "fleet-pricelist-client")
	_, err = eng.Execute(client, `mutation createTierSpec(tierSpecId: "clientMinted", tier: "node", displayName: "Minted", monthlyPriceUsd: 0, messageCredits: 999999, overageMessagesUsdPer1k: 0, overagePolicy: "meter", instanceProfile: "dedicated", dbPreset: "top", status: "available")`)
	if err == nil || !strings.Contains(err.Error(), "server-only") {
		t.Errorf("a client-origin createTierSpec was not refused as server-only (err=%v). A caller who can write a tier row can mint themselves an allowance.", err)
	}
}
