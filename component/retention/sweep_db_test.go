package retention

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
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

func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

type fixture struct {
	db          *bun.DB
	open        func() *bun.DB
	concept     *memorynodes.Concept
	sweep       *Sweeper
	archive     *testArchive
	invalidated []string
	now         time.Time
}
type testArchive struct {
	objects     map[string][]byte
	corrupt     bool
	afterUpload func()
}

func (a *testArchive) Upload(_ context.Context, _, object string, b []byte, _ string) (string, error) {
	a.objects[object] = append([]byte(nil), b...)
	if a.afterUpload != nil {
		a.afterUpload()
	}
	return object, nil
}
func (a *testArchive) DownloadWithLimit(_ context.Context, _, object string, _ int64) ([]byte, error) {
	if a.corrupt {
		return []byte("bad"), nil
	}
	return a.objects[object], nil
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "concept retention", dbtest.DSN(), err)
	}
	schema := fmt.Sprintf("retention6059_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA ?`, bun.Ident(schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA ? CASCADE`, bun.Ident(schema)); _ = admin.Close() })
	open := func() *bun.DB {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	db := open()
	for _, table := range []string{"MemoryNodes", "SecretMemoryNodes", "memory_node_schemas", "concept_retention_progress"} {
		if _, err := db.ExecContext(ctx, `CREATE TABLE ? (LIKE public.? INCLUDING ALL)`, bun.Ident(table), bun.Ident(table)); err != nil {
			t.Fatal(err)
		}
	}
	c := &memorynodes.Concept{Name: "v1:test:retained", Retention: &memorynodes.RetentionPolicy{TTL: 30 * 24 * time.Hour, KeepLatest: true, Archive: "required"}}
	f := &fixture{db: db, open: open, concept: c, archive: &testArchive{objects: map[string][]byte{}}, now: time.Now().UTC().Truncate(time.Microsecond)}
	f.sweep = &Sweeper{DB: db, Concepts: memorynodes.NewRegistry(map[string]*memorynodes.Concept{c.Name: c}), Admit: func(context.Context, memorynodes.MemoryNode) bool { return true }, Invalidate: func(c string) { f.invalidated = append(f.invalidated, c) }, Archive: f.archive, Container: "test", Now: func() time.Time { return f.now }}
	return f
}
func (f *fixture) insert(t *testing.T, table, id string, age time.Duration, payload string) {
	t.Helper()
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO ? (id,"createdAt","createdBy",concept,type,schema,payload,provenance) VALUES (?,?,'test',?,'object','{"$id":"original","description":"historical schema"}',?::jsonb,'{"name":"original"}')`, bun.Ident(table), id, f.now.Add(-age), f.concept.Name, payload)
	if err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRowContext(context.Background(), `SELECT count(*) FROM ?`, bun.Ident(table)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRetentionKeepsNewestIncludingTombstonesAndArchivesExactVersions(t *testing.T) {
	f := newFixture(t)
	for _, table := range []string{"MemoryNodes", "SecretMemoryNodes"} {
		f.insert(t, table, "record", 60*24*time.Hour, `{"n":9007199254740993}`)
		f.insert(t, table, "record", 45*24*time.Hour, `{"deleted":true}`)
		f.insert(t, table, "recent", time.Hour, `{"value":"fresh"}`)
	}
	report, err := f.sweep.Run(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Deleted != 2 || report.Archived != 2 || f.count(t, "MemoryNodes") != 2 || f.count(t, "SecretMemoryNodes") != 2 {
		t.Fatalf("report=%+v", report)
	}
	for _, data := range f.archive.objects {
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(gz)
		_ = gz.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(raw, []byte("9007199254740993")) || !bytes.Contains(raw, []byte("historical schema")) || !bytes.Contains(raw, []byte(`"table"`)) {
			t.Fatalf("incomplete archive: %s", raw)
		}
	}
	if len(f.invalidated) != 2 {
		t.Fatal("physical deletion did not invalidate reads")
	}
	// No newer version may disappear on a later pass and revive its predecessor.
	report, err = f.sweep.Run(context.Background(), false)
	if err != nil || report.Deleted != 0 {
		t.Fatalf("repeat=%+v %v", report, err)
	}
}

func TestRetentionArchiveFailureAndAdmissionKeepRecords(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt", "denied"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.concept.Retention.KeepLatest = false
			f.insert(t, "MemoryNodes", "old", 60*24*time.Hour, `{"value":"keep"}`)
			switch mode {
			case "missing":
				f.sweep.Archive = nil
			case "corrupt":
				f.archive.corrupt = true
			case "denied":
				f.sweep.Admit = func(context.Context, memorynodes.MemoryNode) bool { return false }
			}
			report, err := f.sweep.Run(context.Background(), false)
			if err == nil || report.Deleted != 0 || f.count(t, "MemoryNodes") != 1 {
				t.Fatalf("%s: %+v %v", mode, report, err)
			}
		})
	}
}

func TestRetentionDisposablePolicyAndDryRun(t *testing.T) {
	f := newFixture(t)
	f.concept.Retention.KeepLatest = false
	f.concept.Retention.Archive = "none"
	f.sweep.Archive = nil
	f.insert(t, "MemoryNodes", "old", 60*24*time.Hour, `{}`)
	f.insert(t, "MemoryNodes", "future", -time.Hour, `{}`)
	report, err := f.sweep.Run(context.Background(), true)
	if err != nil || report.Eligible != 1 || report.Deleted != 0 || f.count(t, "MemoryNodes") != 2 {
		t.Fatalf("dry %+v %v", report, err)
	}
	var checkpoints int
	if err := f.db.QueryRowContext(context.Background(), `SELECT count(*) FROM concept_retention_progress`).Scan(&checkpoints); err != nil || checkpoints != 0 {
		t.Fatalf("dry run moved progress: %d %v", checkpoints, err)
	}
	report, err = f.sweep.Run(context.Background(), false)
	if err != nil || report.Deleted != 1 || report.Archived != 0 || f.count(t, "MemoryNodes") != 1 {
		t.Fatalf("disposable %+v %v", report, err)
	}
}

func TestRetentionResumesOnAnotherReplicaAndFencesAStaleClaim(t *testing.T) {
	f := newFixture(t)
	f.concept.Retention.KeepLatest = false
	for n := 0; n < pageSize+1; n++ {
		f.insert(t, "MemoryNodes", fmt.Sprintf("row-%03d", n), 60*24*time.Hour, `{}`)
	}
	const key = "resume-test"
	report := Report{}
	complete, err := f.sweep.batch(context.Background(), key, "MemoryNodes", f.concept, f.now.Add(-f.concept.Retention.TTL), false, &report)
	if err != nil || complete || report.Deleted != pageSize {
		t.Fatalf("first %+v %v %v", report, complete, err)
	}
	other := *f.sweep
	other.DB = f.open()
	report = Report{}
	complete, err = other.batch(context.Background(), key, "MemoryNodes", f.concept, f.now.Add(-f.concept.Retention.TTL), false, &report)
	if err != nil || !complete || report.Deleted != 1 {
		t.Fatalf("replacement %+v %v %v", report, complete, err)
	}
	f.insert(t, "MemoryNodes", "stale", 60*24*time.Hour, `{}`)
	f.archive.afterUpload = func() {
		// A replacement claimant has only persisted state, no process-local lock.
		_, err := other.DB.ExecContext(context.Background(), `UPDATE concept_retention_progress SET lease_until=clock_timestamp() WHERE policy=?`, key)
		if err != nil {
			t.Fatal(err)
		}
		claim, err := other.claim(context.Background(), key)
		if err != nil || claim == nil {
			t.Fatalf("replacement could not claim expired work: %v", err)
		}
	}
	report = Report{}
	_, err = f.sweep.batch(context.Background(), key, "MemoryNodes", f.concept, f.now.Add(-f.concept.Retention.TTL), false, &report)
	if err == nil || !strings.Contains(err.Error(), "claim changed") || f.count(t, "MemoryNodes") != 1 {
		t.Fatalf("stale claim deleted: %+v %v", report, err)
	}
}

func TestRetentionRefusesToDeleteChangedArchiveSnapshot(t *testing.T) {
	f := newFixture(t)
	f.concept.Retention.KeepLatest = false
	f.insert(t, "MemoryNodes", "old", 60*24*time.Hour, `{"value":"before"}`)
	f.archive.afterUpload = func() {
		_, err := f.db.ExecContext(context.Background(), `UPDATE "MemoryNodes" SET payload='{"value":"changed"}' WHERE id='old'`)
		if err != nil {
			t.Fatal(err)
		}
	}
	report, err := f.sweep.Run(context.Background(), false)
	if err != nil || report.Deleted != 0 || f.count(t, "MemoryNodes") != 1 {
		t.Fatalf("changed snapshot %+v %v", report, err)
	}
	var payload json.RawMessage
	if err := f.db.QueryRowContext(context.Background(), `SELECT payload FROM "MemoryNodes" WHERE id='old'`).Scan(&payload); err != nil || !bytes.Contains(payload, []byte("changed")) {
		t.Fatalf("payload=%s %v", payload, err)
	}
}

func TestRetentionHandlerRejectsUnprivilegedCaller(t *testing.T) {
	i := &Integration{}
	if _, err := i.sweep(context.Background(), nil, 0); err == nil {
		t.Fatal("unauthenticated retention admitted")
	}
}

func TestRetentionSplitsArchiveBudgetAndPreservesOversizedVersions(t *testing.T) {
	f := newFixture(t)
	f.concept.Retention.KeepLatest = false
	medium := `{"value":"` + strings.Repeat("x", 18<<20) + `"}`
	f.insert(t, "MemoryNodes", "a", 60*24*time.Hour, medium)
	f.insert(t, "MemoryNodes", "b", 60*24*time.Hour, medium)
	oversized := `{"value":"` + strings.Repeat("x", archiveBudget+1) + `"}`
	f.insert(t, "MemoryNodes", "c", 60*24*time.Hour, oversized)
	f.insert(t, "MemoryNodes", "d", 60*24*time.Hour, `{"value":"small"}`)
	report, err := f.sweep.Run(context.Background(), false)
	if err != nil || report.Deleted != 3 || report.Oversized != 1 || f.count(t, "MemoryNodes") != 1 || len(report.Objects) != 2 {
		t.Fatalf("budget split: %+v %v", report, err)
	}
}
