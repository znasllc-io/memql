package retention

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

type checkedSchemaArchive struct {
	*testArchive
	readBack   func([]byte)
	downloaded map[string][]byte
}

func (a *checkedSchemaArchive) DownloadWithLimit(ctx context.Context, container, object string, limit int64) ([]byte, error) {
	raw, err := a.testArchive.DownloadWithLimit(ctx, container, object, limit)
	if err == nil {
		a.readBack(raw)
		a.downloaded[object] = append([]byte(nil), raw...)
	}
	return raw, err
}

func TestRetentionPeerArchivesReferencedHistoricalSchemasAndKeepsLatest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// The existing fixture has an empty private catalog. Install the real
	// migration in that same isolated schema so inserts use production triggers.
	if _, err := f.db.ExecContext(ctx, `DROP TABLE memory_node_schemas`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../database/memory-nodes/migrations/20261013020000_shared_row_schemas.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	peer := f.open()
	for _, node := range []string{"writer", "peer"} {
		if _, err := f.db.ExecContext(ctx, `INSERT INTO "MemoryNodes" (id,"createdAt","createdBy",concept,type,schema,payload,provenance) VALUES (?,clock_timestamp(),'test','v1:cluster:node','object','{}','{"health":"healthy"}','{}')`, "v1:cluster:node:"+node); err != nil {
			t.Fatal(err)
		}
	}
	for _, reader := range []struct {
		db *bun.DB
		id string
	}{{f.db, "writer"}, {peer, "peer"}} {
		if enabled, err := memorynodes.EnableSchemaReferencesWhenReady(ctx, reader.db, reader.id, false); err != nil || enabled {
			t.Fatalf("held reader enabled storage: %v %v", enabled, err)
		}
	}
	if enabled, err := memorynodes.EnableSchemaReferencesWhenReady(ctx, f.db, "writer", true); err != nil || !enabled {
		t.Fatalf("explicit activation: %v %v", enabled, err)
	}
	if enabled, err := memorynodes.EnableSchemaReferencesWhenReady(ctx, peer, "peer", false); err != nil || !enabled {
		t.Fatalf("peer did not observe durable activation: %v %v", enabled, err)
	}

	const historicalSchema = `{"$id":"versioned","type":"object","description":"historical 文 schema"}`
	const currentSchema = `{"$id":"versioned","type":"object","description":"current schema differs"}`
	tables := []string{"MemoryNodes", "SecretMemoryNodes"}
	before := map[string][]memorynodes.MemoryNode{}
	load := func(db *bun.DB, table string) []memorynodes.MemoryNode {
		t.Helper()
		var rows []memorynodes.MemoryNode
		if err := db.NewSelect().Table(table).ColumnExpr("*").Where("concept=?", f.concept.Name).OrderExpr(`"createdAt"`).Scan(ctx, &rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	for _, table := range tables {
		for n, schema := range []string{historicalSchema, currentSchema} {
			// Both versions are expired; only keepLatest protects the second.
			at := f.now.Add(-time.Duration(60-n*15) * 24 * time.Hour)
			if _, err := f.db.ExecContext(ctx, `INSERT INTO ? (id,"createdAt","createdBy",concept,type,schema,payload,metadata,provenance) VALUES ('record',?,'original-writer',?,'object',?::jsonb,'{"n":9007199254740993}','{"source":"original"}','{"name":"original-write"}')`, bun.Ident(table), at, f.concept.Name, schema); err != nil {
				t.Fatal(err)
			}
		}
		before[table] = load(peer, table)
		if len(before[table]) != 2 {
			t.Fatalf("%s: expected two versions", table)
		}
		for _, row := range before[table] {
			if !bytes.Contains(row.Schema, []byte(`"memql-schema:`)) || bytes.Contains(row.Schema, []byte("description")) {
				t.Fatalf("%s did not store a compact schema reference: %s", table, row.Schema)
			}
		}
		if bytes.Equal(before[table][0].Schema, before[table][1].Schema) {
			t.Fatal("historical and current schema references collapsed")
		}
	}
	archive := &checkedSchemaArchive{testArchive: f.archive, downloaded: map[string][]byte{}, readBack: func(raw []byte) {
		// Every downloaded archive must be verified before its row is deleted.
		archived := decodeSharedSchemaArchive(t, raw)
		if len(before[archived.Table]) != 2 || len(load(peer, archived.Table)) != 2 {
			t.Fatal("archive read-back occurred after its historical row was deleted")
		}
	}}
	other := *f.sweep
	other.DB, other.Archive = peer, archive
	report, err := other.Run(ctx, false)
	if err != nil || report.Archived != 2 || report.Deleted != 2 || len(archive.downloaded) != 2 || len(report.Objects) != 2 {
		t.Fatalf("peer retention: %+v read-back objects=%d err=%v", report, len(archive.downloaded), err)
	}
	seen := map[string]bool{}
	for _, object := range report.Objects {
		archived := decodeSharedSchemaArchive(t, archive.downloaded[object])
		if seen[archived.Table] || len(before[archived.Table]) != 2 {
			t.Fatalf("unexpected archive table %q", archived.Table)
		}
		seen[archived.Table] = true
		want := before[archived.Table][0]
		want.Schema = json.RawMessage(historicalSchema)
		assertSharedSchemaRowJSON(t, archived.MemoryNode, want)
	}
	for _, table := range tables {
		if !seen[table] {
			t.Fatalf("%s missing from downloaded archives", table)
		}
		remaining := load(f.db, table)
		if len(remaining) != 1 {
			t.Fatalf("%s retained %d rows, want exactly the latest", table, len(remaining))
		}
		assertSharedSchemaRowJSON(t, remaining[0], before[table][1])
	}
	if len(f.invalidated) != 2 {
		t.Fatalf("expected invalidation for both tables: %v", f.invalidated)
	}
	if again, err := other.Run(ctx, false); err != nil || again.Deleted != 0 || again.Archived != 0 {
		t.Fatalf("repeat retired protected latest: %+v %v", again, err)
	}
}

func decodeSharedSchemaArchive(t *testing.T, raw []byte) archiveRow {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	var row archiveRow
	decoder := json.NewDecoder(gz)
	if err := decoder.Decode(&row); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("archive contains unexpected extra data: %v", err)
	}
	return row
}

func assertSharedSchemaRowJSON(t *testing.T, got, want memorynodes.MemoryNode) {
	t.Helper()
	decode := func(row memorynodes.MemoryNode) any {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if !reflect.DeepEqual(decode(got), decode(want)) {
		t.Fatalf("row schema, exact payload number, provenance or version changed:\ngot %+v\nwant %+v", got, want)
	}
}
