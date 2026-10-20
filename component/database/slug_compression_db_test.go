package database_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A catalog repair must preserve whole rows even when old history has been
// compressed. Timescale refuses UPDATE of the segment/order columns before
// evaluating the row predicate, including when no rows match it.
func TestSlugMigrationPreservesCompressedHistory(t *testing.T) {
	dsn := scratchDatabase(t)
	db := openScratch(t, dsn)
	execScratch(t, db, `CREATE EXTENSION timescaledb`)
	execScratch(t, db, `CREATE TABLE "MemoryNodes" (id text NOT NULL, "createdAt" timestamptz NOT NULL, "createdBy" text NOT NULL, schema jsonb NOT NULL, payload jsonb NOT NULL, metadata jsonb NOT NULL, type text NOT NULL, concept text NOT NULL, PRIMARY KEY(id,"createdAt"))`)
	execScratch(t, db, `SELECT create_hypertable('"MemoryNodes"'::regclass, 'createdAt')`)
	execScratch(t, db, `ALTER TABLE "MemoryNodes" SET (timescaledb.compress, timescaledb.compress_segmentby='concept', timescaledb.compress_orderby='"createdAt" DESC,id')`)
	execScratch(t, db, `INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, metadata, type, concept)
 VALUES ('legacy_role', now() - interval '180 days', 'test', '{}', '{"slug":"legacy_role","lockedDomainIds":["a_b"],"keep":17}', '{"keep":true}', 'node', 'v1:agents:agentRole'),
 ('untouched', now() - interval '180 days', 'test', '{}', '{"value":"a_b"}', '{}', 'node', 'v1:test:untouched')`)
	execScratch(t, db, `SELECT compress_chunk(c, true) FROM show_chunks('"MemoryNodes"'::regclass, older_than => interval '100 days') c`)
	var compressed int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM timescaledb_information.chunks WHERE hypertable_name = 'MemoryNodes' AND is_compressed`).Scan(&compressed); err != nil || compressed == 0 {
		t.Fatalf("compressed history fixture: chunks=%d error=%v", compressed, err)
	}
	body, err := os.ReadFile(filepath.Join("memory-nodes", "migrations", "20260520000000_slug_kebab_case.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		for _, statement := range strings.Split(string(body), "\n--bun:split\n") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err := db.ExecContext(context.Background(), statement); err != nil {
				t.Fatalf("migration attempt %d: %v", attempt, err)
			}
		}
	}
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE (id='legacy-role' AND payload->>'slug'='legacy-role' AND payload->'lockedDomainIds'='["a-b"]'::jsonb AND payload->>'keep'='17' AND metadata->>'keep'='true' AND "createdBy"='test') OR (id='untouched' AND payload->>'value'='a_b')`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("preserved rows: count=%d error=%v", count, err)
	}
	// A down migration must also tolerate the same compressed history.
	down, err := os.ReadFile(filepath.Join("memory-nodes", "migrations", "20260520000000_slug_kebab_case.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range strings.Split(string(down), "\n--bun:split\n") {
		if strings.TrimSpace(statement) != "" {
			execScratch(t, db, statement)
		}
	}
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE id='legacy_role' AND payload->>'slug'='legacy_role' AND payload->>'keep'='17' AND metadata->>'keep'='true' AND "createdBy"='test'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("down migration preserved row: count=%d error=%v", count, err)
	}
	// A colliding canonical key must fail atomically without losing the old row.
	execScratch(t, db, `INSERT INTO "MemoryNodes" SELECT 'legacy-role', "createdAt", "createdBy", schema, payload, metadata, type, concept FROM "MemoryNodes" WHERE id='legacy_role'`)
	var collision error
	for _, statement := range strings.Split(string(body), "\n--bun:split\n") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			collision = err
			break
		}
	}
	if collision == nil || !strings.Contains(collision.Error(), "23505") {
		t.Fatalf("expected primary-key collision, got %v", collision)
	}
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM "MemoryNodes" WHERE id IN ('legacy_role','legacy-role')`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("failed migration lost a row: count=%d error=%v", count, err)
	}
}
