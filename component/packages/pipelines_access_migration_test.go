package packages

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
)

// Exercise the real SQL against isolated tables. A navigation move must not
// resurrect a revoked grant, turn a deny into an allow, or replace a newer
// destination decision. Use the runtime's grant identity for the readback.
func TestPipelinesAccessMigrationPreservesDecisions(t *testing.T) {
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("MEMQL_DATABASE_DSN is required")
		}
		t.Skip("requires Postgres")
	}
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `CREATE TEMP TABLE "MemoryNodes" (
	 id text, "createdAt" timestamptz, "createdBy" text DEFAULT 'fixture',
	 schema jsonb DEFAULT '{}', payload jsonb, metadata jsonb DEFAULT '{}',
	 "type" text DEFAULT 'object', concept text, provenance jsonb DEFAULT '{}',
	 PRIMARY KEY(id,"createdAt")) ON COMMIT DROP`)
	if err != nil {
		t.Fatal(err)
	}
	write := func(subject, resource, effect string, active bool, version int) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"subjectKind": "user", "subjectId": subject, "verb": "execute", "resourceType": resource, "effect": effect, "active": active})
		id := "v1:rbac:grant:" + auth.GrantRowID("user", subject, "execute", resource)
		if _, err := tx.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,"createdAt",payload,concept) VALUES($1,$2,$3,'v1:rbac:grant')`, id, time.Unix(int64(version), 0), string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	old, target := "app:deployables/sources", "app:pipelines/repositories"
	write("denied", old, "deny", true, 1)
	write("allowed", old, "allow", true, 1)
	write("revoked", old, "allow", true, 1)
	write("revoked", old, "allow", false, 2)
	write("newer", old, "allow", true, 1)
	write("newer", target, "deny", true, 3)
	if _, err = tx.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,"createdAt",payload,concept) VALUES
	 ('predefined',now(),'{"roleSlug":"developer","verb":"execute","resourceType":"app:deployables/sources","predefined":true,"active":true}','v1:rbac:capability'),
	 ('custom',now(),'{"roleSlug":"custom","verb":"execute","resourceType":"app:deployables/sources","effect":"deny","active":true}','v1:rbac:capability'),
	 ('unrelated',now(),'{"resourceType":"app:deployables/sources","predefined":true,"active":true}','v1:other:capability')`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../database/memory-nodes/migrations/20261022230000_pipelines_access.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		if _, err = tx.ExecContext(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
		for _, f := range []struct {
			subject, effect string
			active          bool
		}{{"denied", "deny", true}, {"allowed", "allow", true}, {"revoked", "allow", false}, {"newer", "deny", true}} {
			var effect string
			var active bool
			var versions int
			id := "v1:rbac:grant:" + auth.GrantRowID("user", f.subject, "execute", target)
			if err = tx.QueryRowContext(ctx, `SELECT payload->>'effect',(payload->>'active')::boolean,count(*) OVER() FROM "MemoryNodes" WHERE id=$1 ORDER BY "createdAt" DESC LIMIT 1`, id).Scan(&effect, &active, &versions); err != nil {
				t.Fatal(err)
			}
			if effect != f.effect || active != f.active || versions != 1 {
				t.Fatalf("pass %d %s: %s/%t versions=%d", pass, f.subject, effect, active, versions)
			}
		}
		var active bool
		if err = tx.QueryRowContext(ctx, `SELECT (payload->>'active')::boolean FROM "MemoryNodes" WHERE id='predefined'`).Scan(&active); err != nil || active {
			t.Fatalf("retired seed still grants access: %t %v", active, err)
		}
		if err = tx.QueryRowContext(ctx, `SELECT (payload->>'active')::boolean FROM "MemoryNodes" WHERE id='unrelated'`).Scan(&active); err != nil || !active {
			t.Fatalf("unrelated concept changed: %t %v", active, err)
		}
		var denials int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE concept='v1:rbac:capability' AND payload->>'roleSlug'='custom' AND payload->>'resourceType'=$1 AND payload->>'effect'='deny'`, target).Scan(&denials); err != nil || denials != 1 {
			t.Fatalf("custom denial lost: %d %v", denials, err)
		}
	}
}
