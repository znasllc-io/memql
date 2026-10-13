package database_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestSharedRowSchemasPreserveVersionsAndDeduplicateAcrossTables(t *testing.T) {
	db := conceptIndexDB(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE memory_schema_storage SET references_enabled=true`); err != nil {
		t.Fatal(err)
	}
	schemaID := fmt.Sprintf("schema6059-%d", time.Now().UnixNano())
	schemas := []string{
		fmt.Sprintf(`{"$id":%q,"type":"object","description":%q}`, schemaID, strings.Repeat("old schema ", 1000)),
		fmt.Sprintf(`{"$id":%q,"type":"object","description":"new schema"}`, schemaID),
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for n := 0; n < 3; n++ {
		table := "MemoryNodes"
		if n == 2 {
			table = "SecretMemoryNodes"
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO ? (id,"createdAt","createdBy",concept,type,schema,payload,provenance) VALUES (?,?,?,'v1:test:schema','object',?::jsonb,'{}','{}')`, bun.Ident(table), schemaID, now.Add(time.Duration(n)*time.Second), "test", schemas[n%2])
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memory_node_schemas WHERE document->>'$id'=?`, schemaID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("schema versions=%d, want two shared across three rows: %v", count, err)
	}
	var nodes []memorynodes.MemoryNode
	if err := tx.NewSelect().Model(&nodes).Where("id=?", schemaID).OrderExpr(`"createdAt"`).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if len(node.Schema) > 200 || !strings.Contains(string(node.Schema), "memql-schema:") || !strings.Contains(string(node.Schema), schemaID) {
			t.Fatalf("stored schema is not a compact version reference: %s", node.Schema)
		}
	}
	if err := memorynodes.HydrateSchemas(ctx, tx, nodes); err != nil {
		t.Fatal(err)
	}
	for n, node := range nodes {
		var got, want any
		if err := json.Unmarshal(node.Schema, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(schemas[n]), &want); err != nil {
			t.Fatal(err)
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Fatal("historical schema was replaced by current schema")
		}
	}
	runID := "v1:work:run:" + schemaID
	if _, err := tx.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,"createdAt","createdBy",concept,type,schema,payload,provenance) VALUES (?,now(),'test','v1:work:run','object','{}','{"status":"running"}','{}')`, runID); err != nil {
		t.Fatal(err)
	}
	countEvents := func(want int) {
		t.Helper()
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM work_run_head_events WHERE id=?`, runID).Scan(&count); err != nil || count != want {
			t.Fatalf("recovery events=%d want %d: %v", count, want, err)
		}
	}
	countEvents(1)
	if _, err := tx.ExecContext(ctx, `UPDATE "MemoryNodes" SET schema='{}' WHERE id=?`, runID); err != nil {
		t.Fatal(err)
	}
	countEvents(1)
	if _, err := tx.ExecContext(ctx, `UPDATE "MemoryNodes" SET payload='{"status":"waiting"}' WHERE id=?`, runID); err != nil {
		t.Fatal(err)
	}
	countEvents(2)
	// An unresolved reference cannot silently enter either row table.
	_, err = tx.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,"createdAt","createdBy",concept,type,schema,payload,provenance) VALUES (?,now(),'test','v1:test:schema','object','{"$ref":"memql-schema:missing"}','{}','{}')`, schemaID+"bad")
	if err == nil {
		t.Fatal("missing schema reference accepted")
	}
}

func TestSchemaCompactionResumesOnAnotherReplicaWithoutChangingRows(t *testing.T) {
	admin := conceptIndexDB(t)
	ctx := context.Background()
	schema := fmt.Sprintf("schemacompact_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA ?`, bun.Ident(schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA ? CASCADE`, bun.Ident(schema)) })
	open := func() *bun.DB {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db := open()
	for _, table := range []string{"MemoryNodes", "SecretMemoryNodes"} {
		if _, err := db.ExecContext(ctx, `CREATE TABLE ? (LIKE public.? INCLUDING ALL)`, bun.Ident(table), bun.Ident(table)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,"createdAt","createdBy",concept,type,schema,payload,provenance) SELECT lpad(n::text,4,'0'),'2026-10-01'::timestamptz,'test','v1:test:schema','object','{"$id":"old","type":"object"}','{"value":123}','{"kind":"mutation","name":"original"}' FROM generate_series(1,501) n`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("memory-nodes/migrations/20261013020000_shared_row_schemas.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	if n, err := memorynodes.CompactSchemaBatch(ctx, db); err != nil || n != 0 {
		t.Fatalf("conversion ran before readers were ready: %d %v", n, err)
	}
	for _, id := range []string{"reader-a", "reader-b"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,"createdAt","createdBy",concept,type,schema,payload,provenance) VALUES (?,now(),'test','v1:cluster:node','object','{}','{"health":"healthy"}','{}')`, "v1:cluster:node:"+id); err != nil {
			t.Fatal(err)
		}
	}
	if ready, err := memorynodes.EnableSchemaReferencesWhenReady(ctx, db, "reader-a"); err != nil || ready {
		t.Fatalf("an older registered reader must hold conversion: %v %v", ready, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE memory_schema_storage SET next_check_at=now()`); err != nil {
		t.Fatal(err)
	}
	if ready, err := memorynodes.EnableSchemaReferencesWhenReady(ctx, open(), "reader-b"); err != nil || !ready {
		t.Fatalf("all registered readers should enable references: %v %v", ready, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept='v1:cluster:node'`); err != nil {
		t.Fatal(err)
	}
	// Convert only the populated table on this first tick.
	if _, err := db.ExecContext(ctx, `UPDATE memory_schema_compaction SET completed=true WHERE table_name='SecretMemoryNodes'`); err != nil {
		t.Fatal(err)
	}
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(ctx, `SELECT table_name FROM memory_schema_compaction WHERE table_name='MemoryNodes' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	other := open()
	skipped, err := memorynodes.CompactSchemaBatch(ctx, other)
	if err != nil || skipped != 0 {
		t.Fatalf("replica did not skip a held batch: %d %v", skipped, err)
	}
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	n, err := memorynodes.CompactSchemaBatch(ctx, db)
	if err != nil || n != 500 {
		t.Fatalf("first bounded batch=%d: %v", n, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE memory_schema_compaction SET next_at=now()`); err != nil {
		t.Fatal(err)
	}
	// A new connection pool knows only the persisted cursor.
	n, err = memorynodes.CompactSchemaBatch(ctx, open())
	if err != nil || n != 1 {
		t.Fatalf("resumed batch=%d: %v", n, err)
	}
	var changed int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE "createdAt"<>'2026-10-01'::timestamptz OR payload<>'{"value":123}'::jsonb OR provenance<>'{"kind":"mutation","name":"original"}'::jsonb OR schema->>'$ref' NOT LIKE 'memql-schema:%'`).Scan(&changed); err != nil || changed != 0 {
		t.Fatalf("conversion changed row contents or missed schemas: %d %v", changed, err)
	}
	n, err = memorynodes.CompactSchemaBatch(ctx, db)
	if err != nil || n != 0 {
		t.Fatalf("completed conversion restarted: %d %v", n, err)
	}
}
