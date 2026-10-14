package memoryNodes

import (
	"context"
	"database/sql"
	"github.com/uptrace/bun/driver/pgdriver"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestFleetCapabilityRetirementIsScopedAndIdempotent(t *testing.T) {
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("MEMQL_DATABASE_DSN is required")
		}
		t.Skip("requires Postgres")
	}
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TEMP TABLE "MemoryNodes" (concept text, id text, payload jsonb) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	fixtures := []struct {
		concept, role, resource string
		predefined, want        bool
	}{
		{"v1:rbac:capability", "admin", "app:fleet", true, false},
		{"v1:rbac:capability", "user", "app:fleet", true, false},
		{"v1:rbac:capability", "viewer", "app:fleet", true, false},
		{"v1:rbac:capability", "admin", "app:fleet/logs", true, false},
		{"v1:rbac:capability", "owner", "app:fleet", true, true},
		{"v1:rbac:capability", "developer", "app:fleet", true, true},
		{"v1:rbac:capability", "admin", "app:files", true, true},
		{"v1:rbac:capability", "admin", "app:fleet", false, true},
		{"v1:other:capability", "admin", "app:fleet", true, true},
	}
	for i, f := range fixtures {
		if _, err = tx.ExecContext(ctx, `INSERT INTO "MemoryNodes" VALUES($1,$2,jsonb_build_object('roleSlug',$3::text,'resourceType',$4::text,'predefined',$5::boolean,'verb','read','active',true))`, f.concept, strconv.Itoa(i), f.role, f.resource, f.predefined); err != nil {
			t.Fatal(err)
		}
	}
	migration, err := os.ReadFile("migrations/20261014230000_fleet_operator_capabilities.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		if _, err = tx.ExecContext(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
		for i, f := range fixtures {
			var got bool
			if err = tx.QueryRowContext(ctx, `SELECT (payload->>'active')::boolean FROM "MemoryNodes" WHERE id=$1`, strconv.Itoa(i)).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != f.want {
				t.Fatalf("pass %d fixture %d: active=%t want=%t", pass, i, got, f.want)
			}
		}
	}
}
