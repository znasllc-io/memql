package automations

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// TestTheFleetPriceListMaterializes boots an engine on a database with the
// fleet bundle (deploy/fleet/dsl) mounted, runs the real SeedMaterializer over
// the bundle's price list, and requires every `seed tierSpec` in seeds.memql to
// land as a v1:fleet:tierSpec row carrying the seed's numbers (memql#5437
// review).
//
// The price list could never materialize before createTierSpec existed: the
// materializer writes a seed through `create<Concept>` and nothing else, and
// the bundle deliberately had no tier mutation -- so every tier seed failed at
// boot with the mutation missing, and the pricing page, Orbit and the allowance
// read an empty catalog while the bundle's other gates stayed green over the
// seeds' TEXT.
//
// The other half is the reason the mutation is @serverOnly: a client-origin
// call to it is refused, so the only writer of the price list is the seed path.
//
// It lives HERE rather than beside the bundle because this package runs in the
// db-tests lane and deploy/fleet does not; a database test outside that lane
// runs only where somebody happens to have Postgres up. The bundle's other
// gates stay in deploy/fleet. Only the price list is materialized: the whole
// sweep would write every embedded catalog into a database the lane's other
// packages share.
func TestTheFleetPriceListMaterializes(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "fleet price list materialization", dsn, err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	_, _, unmount := memqldsl.MountOverlayDomains(quiet, os.DirFS(filepath.Join("..", "..", "deploy", "fleet", "dsl")))
	t.Cleanup(func() {
		unmount()
		memorynodes.ReplaceAll(nil)
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
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine Init with the fleet bundle mounted: %v", err)
	}

	priceList := memql.NewSeedRegistry()
	for _, def := range eng.Seeds().All() {
		if def.UseConcept == "tierSpec" {
			if err := priceList.Upsert(def); err != nil {
				t.Fatalf("seed %s: %v", def.Name, err)
			}
		}
	}
	seeds := parseFleetTierSeeds(t)
	if got := len(priceList.All()); got != len(seeds) || got < 5 {
		t.Fatalf("the engine loaded %d tierSpec seeds and seeds.memql declares %d; five were measured", got, len(seeds))
	}
	// No row of the concept may predate this run: a local database keeps the
	// rows an earlier run wrote, and a price list read back from those would
	// pass with the mutation gone. Nothing but this test writes the concept.
	clearPriceList := func() {
		if _, err := db.NewDelete().TableExpr(`"MemoryNodes"`).Where("concept = ?", "v1:fleet:tierSpec").Exec(ctx); err != nil {
			t.Fatalf("clear the price list: %v", err)
		}
	}
	clearPriceList()
	t.Cleanup(clearPriceList)
	materializer := memql.NewSeedMaterializer(eng, priceList)
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

	for tier, seed := range seeds {
		row, ok := byTier[tier]
		if !ok {
			t.Errorf("seed for tier %q did not materialize: no v1:fleet:tierSpec row carries it. The materializer writes a seed through create<Concept>, so without createTierSpec the price list is empty.", tier)
			continue
		}
		for field, want := range seed.numbers {
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

	// A client cannot reach the price list's one mutation.
	client := auth.ContextWithUserActor(ctx, "fleet-pricelist-client")
	_, err = eng.Execute(client, `mutation createTierSpec(tierSpecId: "clientMinted", tier: "node", displayName: "Minted", monthlyPriceUsd: 0, messageCredits: 999999, overageMessagesUsdPer1k: 0, overagePolicy: "meter", instanceProfile: "dedicated", dbPreset: "top", status: "available")`)
	if err == nil || !strings.Contains(err.Error(), "server-only") {
		t.Errorf("a client-origin createTierSpec was not refused as server-only (err=%v). A caller who can write a tier row can mint themselves an allowance.", err)
	}
}

// fleetTierSeed is one row of the price list as seeds.memql spells it.
type fleetTierSeed struct {
	fields  map[string]string
	numbers map[string]float64
}

var (
	fleetSeedDecl  = regexp.MustCompile(`(?m)^seed\s+tierSpec\s+(\w+)\s*\{`)
	fleetSeedField = regexp.MustCompile(`(?m)^\s*(\w+):\s*(.+?)\s*$`)
)

// parseFleetTierSeeds reads the price list out of the bundle's seeds.memql, by
// tier -- the same reading deploy/fleet's launch gates compare everything else
// against. The TEXT is the expectation, so a row that disagrees with it fails
// here rather than agreeing with itself.
func parseFleetTierSeeds(t *testing.T) map[string]fleetTierSeed {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "fleet", "dsl", "fleet", "seeds.memql"))
	if err != nil {
		t.Fatalf("read the fleet price list: %v", err)
	}
	src := string(raw)
	locs := fleetSeedDecl.FindAllStringIndex(src, -1)
	out := map[string]fleetTierSeed{}
	for i, loc := range locs {
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		seed := fleetTierSeed{fields: map[string]string{}, numbers: map[string]float64{}}
		for _, m := range fleetSeedField.FindAllStringSubmatch(src[loc[0]:end], -1) {
			key, val := m[1], strings.TrimSuffix(strings.TrimSpace(m[2]), ",")
			seed.fields[key] = strings.Trim(val, `"`)
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				seed.numbers[key] = f
			}
		}
		tier := seed.fields["tier"]
		if tier == "" {
			t.Fatalf("a tierSpec seed in seeds.memql declares no tier:\n%s", src[loc[0]:end])
		}
		out[tier] = seed
	}
	if len(out) == 0 {
		t.Fatal("parsed no tierSpec seeds from seeds.memql -- the file moved or this parse stopped matching")
	}
	return out
}
