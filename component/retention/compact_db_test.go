package retention

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// compact_db_test.go -- the closed-chunk compaction against TimescaleDB.
//
// Each test builds its own hypertable shaped like "MemoryNodes" (LIKE ... INCLUDING
// ALL, so the rewrite rebuilds every production index) in a schema of its own, in
// one-day chunks so "closed a day ago" falls on a day boundary: today's chunk is
// open, yesterday's closed less than a day ago, and the day before that is the
// first a pass may take. The pass runs on a real maintenance session
// (database.OpenMaintenanceSessionDSN), with its search_path on the test schema
// first, so "MemoryNodes" and chunk_compaction_progress resolve to the test's own.

type compactFixture struct {
	db     *bun.DB
	schema string
	now    time.Time
	// expected change counter per day, for settleStats.
	changes map[time.Time]int64
}

func newCompactFixture(t *testing.T) *compactFixture {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("compact_%d", time.Now().UnixNano())
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		dbtest.Unreachable(t, "chunk compaction", dbtest.DSN(), err)
		return nil
	}
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA ?`, bun.Ident(schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA ? CASCADE`, bun.Ident(schema)); _ = admin.Close() })
	// pgdriver SETs a connection parameter as ONE quoted value, so search_path
	// names the test schema alone here and TimescaleDB's functions are qualified.
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
	db.SetMaxOpenConns(1) // one session, so pg_stat_force_next_flush reaches the backend that wrote
	t.Cleanup(func() { _ = db.Close() })
	var ts string
	if err := db.QueryRowContext(ctx, `SELECT quote_ident(n.nspname) FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'timescaledb'`).Scan(&ts); err != nil {
		t.Fatalf("TimescaleDB is not installed in the test database: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`,
		`SELECT ` + ts + `.create_hypertable(to_regclass('"MemoryNodes"'), 'createdAt', chunk_time_interval => interval '1 day')`,
		`CREATE TABLE chunk_compaction_progress (LIKE public.chunk_compaction_progress INCLUDING ALL)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return &compactFixture{db: db, schema: schema, now: time.Now().UTC(), changes: map[time.Time]int64{}}
}

// day is midnight UTC, n days before today.
func (f *compactFixture) day(n int) time.Time {
	return f.now.Truncate(24*time.Hour).AddDate(0, 0, -n)
}

// fill writes rows into day's chunk, every third one carrying a payload large and
// random enough to be stored out of line in TOAST, as the real table's are.
func (f *compactFixture) fill(t *testing.T, day time.Time, rows int) {
	t.Helper()
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", concept, type, schema, payload, metadata, provenance)
		SELECT 'v1:test:row:' || to_char(?::timestamptz, 'YYYYMMDD') || ':' || g, ?::timestamptz + g * interval '20 seconds', 'test', 'v1:test:row', 'object',
		       '{"$id":"s"}'::jsonb,
		       jsonb_build_object('n', g, 'blob', CASE WHEN g % 3 = 0 THEN (SELECT string_agg(md5(random()::text || g::text || x::text), '') FROM generate_series(1, 120) x) ELSE md5(g::text) END),
		       '{}'::jsonb, '{"name":"x"}'::jsonb
		  FROM generate_series(1, ?) g`, day, day, rows)
	if err != nil {
		t.Fatal(err)
	}
	f.changes[day] += int64(rows)
}

// deleteMost deletes three of every four of day's rows, as a retention sweep
// leaves a closed chunk: the same size, mostly free.
func (f *compactFixture) deleteMost(t *testing.T, day time.Time) {
	t.Helper()
	result, err := f.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes"
		WHERE "createdAt" >= ? AND "createdAt" < ?::timestamptz + interval '1 day' AND split_part(id, ':', 5)::int % 4 <> 0`, day, day)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := result.RowsAffected()
	f.changes[day] += n
	if _, err := f.db.ExecContext(context.Background(), `VACUUM "MemoryNodes"`); err != nil {
		t.Fatal(err)
	}
}

// chunkOf names day's chunk.
func (f *compactFixture) chunkOf(t *testing.T, day time.Time) string {
	t.Helper()
	var name string
	if err := f.db.QueryRowContext(context.Background(), `SELECT format('%I.%I', chunk_schema, chunk_name) FROM timescaledb_information.chunks
		WHERE hypertable_schema = ? AND hypertable_name = 'MemoryNodes' AND range_start = ?`, f.schema, day).Scan(&name); err != nil {
		t.Fatalf("no chunk for %s: %v", day, err)
	}
	return name
}

// settleStats waits until every day's change counter reads what the fixture
// wrote: statistics reach other sessions asynchronously, and a pass compares the
// counter with the one its measurement stored.
func (f *compactFixture) settleStats(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.db.ExecContext(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for day, want := range f.changes {
		chunk := f.chunkOf(t, day)
		for {
			var got sql.NullInt64
			if err := f.db.QueryRowContext(ctx, `SELECT n_tup_ins + n_tup_upd + n_tup_del FROM pg_stat_user_tables WHERE relid = to_regclass(?)`, chunk).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got.Valid && got.Int64 == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s's change counter reads %v, want %d", chunk, got, want)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

type chunkState struct {
	Filenode int64
	Total    int64
}

func (f *compactFixture) state(t *testing.T, chunk string) chunkState {
	t.Helper()
	var s chunkState
	if err := f.db.QueryRowContext(context.Background(), `SELECT pg_relation_filenode(to_regclass(?)), pg_total_relation_size(to_regclass(?))`, chunk, chunk).Scan(&s.Filenode, &s.Total); err != nil {
		t.Fatal(err)
	}
	return s
}

// fingerprint is every row of the table, every column, in one digest: what a
// rewrite must leave exactly as it was.
func (f *compactFixture) fingerprint(t *testing.T) string {
	t.Helper()
	var n int64
	var digest string
	if err := f.db.QueryRowContext(context.Background(), `SELECT count(*), md5(coalesce(string_agg(concat_ws('|', id, "createdAt", "createdBy", concept, type, schema, payload, metadata, provenance), ',' ORDER BY id, "createdAt"), ''))
		FROM "MemoryNodes"`).Scan(&n, &digest); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d rows %s", n, digest)
}

func (f *compactFixture) progressRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.QueryRowContext(context.Background(), `SELECT count(*) FROM chunk_compaction_progress`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// session opens the pass's maintenance session on the test schema.
func (f *compactFixture) session(ctx context.Context) (*sql.Conn, func(), error) {
	conn, closeSession, err := database.OpenMaintenanceSessionDSN(ctx, dbtest.DSN())
	if err != nil {
		return nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, `SET search_path = "`+f.schema+`", public`); err != nil {
		closeSession()
		return nil, nil, err
	}
	return conn, closeSession, nil
}

// roomy is a fresh database-health report with room for anything.
func roomy(now func() time.Time) func(context.Context) (StorageReading, error) {
	return func(context.Context) (StorageReading, error) {
		return StorageReading{ObservedAt: now(), DataCapacityBytes: 1 << 40, DataBytes: 1 << 30, WALCapacityBytes: 1 << 40, WALBytes: 0}, nil
	}
}

func (f *compactFixture) compactor() *Compactor {
	c := &Compactor{Session: f.session, Tables: []string{"MemoryNodes"}, RetryPause: time.Millisecond}
	c.Storage = roomy(c.now)
	return c
}

// testPolicy is the nightly policy scaled to a fixture of a few megabytes.
var testPolicy = CompactOptions{MinReclaimBytes: 512 << 10, MinReclaimPercent: 30, MeasureChunks: 16}

func TestCompactionShrinksTheClosedChunkAndKeepsEveryRow(t *testing.T) {
	f := newCompactFixture(t)
	old, intact, today := f.day(3), f.day(2), f.day(0)
	for _, day := range []time.Time{old, intact, today} {
		f.fill(t, day, 3000)
	}
	f.deleteMost(t, old)
	f.deleteMost(t, today) // as free as old, and open: never a candidate
	f.settleStats(t)
	oldChunk, intactChunk, todayChunk := f.chunkOf(t, old), f.chunkOf(t, intact), f.chunkOf(t, today)
	before := map[string]chunkState{oldChunk: f.state(t, oldChunk), intactChunk: f.state(t, intactChunk), todayChunk: f.state(t, todayChunk)}
	rows := f.fingerprint(t)

	report, err := f.compactor().Run(context.Background(), testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "compacted" || report.Chunk == nil || report.Chunk.Chunk != oldChunk {
		t.Fatalf("report = %+v", report)
	}
	if report.Closed != 2 || len(report.Measured) != 2 || report.Eligible != 1 {
		t.Errorf("closed %d measured %d eligible %d, want 2, 2 and 1 (today's chunk is open)", report.Closed, len(report.Measured), report.Eligible)
	}
	after := f.state(t, oldChunk)
	if after.Filenode == before[oldChunk].Filenode || after.Total >= before[oldChunk].Total {
		t.Fatalf("the chunk was not rewritten smaller: before %+v after %+v", before[oldChunk], after)
	}
	if report.Chunk.AfterBytes != after.Total || report.Chunk.ReclaimedBytes != before[oldChunk].Total-after.Total {
		t.Errorf("the report's sizes %+v disagree with the catalog's %d -> %d", report.Chunk, before[oldChunk].Total, after.Total)
	}
	// Three quarters of the rows went; the estimate of what comes back must be
	// in the measured ballpark, not merely positive.
	if report.Chunk.ReclaimedBytes < report.Chunk.ReclaimableBytes/2 {
		t.Errorf("reclaimed %d of an estimated %d", report.Chunk.ReclaimedBytes, report.Chunk.ReclaimableBytes)
	}
	if got := f.fingerprint(t); got != rows {
		t.Fatalf("the rewrite changed the rows: %s, was %s", got, rows)
	}
	for _, untouched := range []string{intactChunk, todayChunk} {
		if got := f.state(t, untouched); got != before[untouched] {
			t.Errorf("%s was touched: %+v, was %+v", untouched, got, before[untouched])
		}
	}
	// VACUUM (ANALYZE) ran: the visibility map index-only reads need is set.
	var allVisible int64
	var analyzed sql.NullTime
	if err := f.db.QueryRowContext(context.Background(), `SELECT c.relallvisible, s.last_analyze FROM pg_class c JOIN pg_stat_user_tables s ON s.relid = c.oid WHERE c.oid = to_regclass(?)`, oldChunk).Scan(&allVisible, &analyzed); err != nil {
		t.Fatal(err)
	}
	if report.Chunk.Vacuum != "done" || allVisible == 0 || !analyzed.Valid {
		t.Errorf("vacuum %q relallvisible %d analyzed %v", report.Chunk.Vacuum, allVisible, analyzed)
	}
	var outcome string
	var failures int
	var compacted sql.NullTime
	if err := f.db.QueryRowContext(context.Background(), `SELECT outcome, failures, compacted_at FROM chunk_compaction_progress WHERE chunk = ?`, oldChunk).Scan(&outcome, &failures, &compacted); err != nil {
		t.Fatal(err)
	}
	if outcome != "compacted" || failures != 0 || !compacted.Valid {
		t.Errorf("progress row: outcome %q failures %d compacted %v", outcome, failures, compacted)
	}

	// The next pass has nothing to do and scans nothing: both measurements still
	// describe their chunks, the rewritten one as it now is.
	again, err := f.compactor().Run(context.Background(), testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if again.Outcome != "skipped" || again.Reason != "no_candidate" || len(again.Measured) != 0 {
		t.Fatalf("second pass = %+v", again)
	}
}

func TestCompactionDryRunChangesNothing(t *testing.T) {
	f := newCompactFixture(t)
	old := f.day(3)
	f.fill(t, old, 3000)
	f.deleteMost(t, old)
	f.settleStats(t)
	oldChunk := f.chunkOf(t, old)
	before, rows := f.state(t, oldChunk), f.fingerprint(t)

	dry := testPolicy
	dry.DryRun = true
	report, err := f.compactor().Run(context.Background(), dry)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "planned" || report.Chunk == nil || report.Chunk.Chunk != oldChunk || !strings.Contains(report.Detail, "would rewrite") {
		t.Fatalf("report = %+v", report)
	}
	if report.Room == nil || report.WAL == nil || report.Chunk.ReclaimableBytes <= 0 {
		t.Errorf("a dry run did not report what a real pass would judge: %+v", report)
	}
	if got := f.state(t, oldChunk); got != before {
		t.Fatalf("a dry run rewrote the chunk: %+v, was %+v", got, before)
	}
	if got := f.fingerprint(t); got != rows {
		t.Fatal("a dry run changed the rows")
	}
	if n := f.progressRows(t); n != 0 {
		t.Fatalf("a dry run recorded %d progress rows", n)
	}
}

func TestCompactionNeverChoosesAnOpenOrJustClosedChunk(t *testing.T) {
	f := newCompactFixture(t)
	yesterday, today := f.day(1), f.day(0)
	for _, day := range []time.Time{yesterday, today} {
		f.fill(t, day, 3000)
		f.deleteMost(t, day)
	}
	f.settleStats(t)
	report, err := f.compactor().Run(context.Background(), testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "skipped" || report.Reason != "no_candidate" || report.Closed != 0 || len(report.Measured) != 0 {
		t.Fatalf("a chunk open or closed less than a day ago was considered: %+v", report)
	}
}

func TestCompactionLockTimeoutLeavesTheChunkIntact(t *testing.T) {
	f := newCompactFixture(t)
	old := f.day(3)
	f.fill(t, old, 3000)
	f.deleteMost(t, old)
	f.settleStats(t)
	oldChunk := f.chunkOf(t, old)
	before, rows := f.state(t, oldChunk), f.fingerprint(t)

	// A reader holding the chunk open: the copy proceeds beside it (reads and
	// the rewrite's ExclusiveLock are compatible), and the swap's AccessExclusive
	// lock waits on it until lock_timeout ends the statement.
	reader := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	defer reader.Close()
	tx, err := reader.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	if err := tx.QueryRowContext(context.Background(), `SELECT count(*) FROM `+oldChunk).Scan(&seen); err != nil {
		t.Fatal(err)
	}

	c := f.compactor()
	c.Attempts = 2
	started := time.Now()
	report, err := c.Run(context.Background(), testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "failed" || report.Reason != "lock_timeout" || report.Chunk == nil || report.Chunk.Attempts != 2 {
		t.Fatalf("report = %+v", report)
	}
	// Two attempts, each a second of lock wait plus a few-megabyte copy.
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("the failing pass took %s", elapsed)
	}
	if got := f.state(t, oldChunk); got != before {
		t.Fatalf("a failed rewrite changed the chunk: %+v, was %+v", got, before)
	}
	// The reader's own view is undisturbed, and so is everybody else's.
	if err := tx.QueryRowContext(context.Background(), `SELECT count(*) FROM `+oldChunk).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := f.fingerprint(t); got != rows {
		t.Fatal("a failed rewrite changed the rows")
	}
	var outcome string
	var failures int
	if err := f.db.QueryRowContext(context.Background(), `SELECT outcome, failures FROM chunk_compaction_progress WHERE chunk = ?`, oldChunk).Scan(&outcome, &failures); err != nil {
		t.Fatal(err)
	}
	if outcome != "lock_timeout" || failures != 1 {
		t.Fatalf("progress row: outcome %q failures %d", outcome, failures)
	}

	// The chunk backs off for a day: tonight's next pass leaves it.
	report, err = f.compactor().Run(context.Background(), testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "skipped" || report.Reason != "no_candidate" {
		t.Fatalf("a chunk that failed minutes ago was retried: %+v", report)
	}
	// A night later, with the reader gone, the quiet retry succeeds.
	later := f.compactor()
	later.Now = func() time.Time { return time.Now().Add(26 * time.Hour) }
	later.Storage = roomy(later.now)
	report, err = later.Run(context.Background(), testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "compacted" || report.Chunk.Chunk != oldChunk {
		t.Fatalf("the retry = %+v", report)
	}
	if got := f.fingerprint(t); got != rows {
		t.Fatal("the retry changed the rows")
	}
}

// TestCompactionIsOnePassClusterWideAndSharesWhatItMeasured is the cross-replica
// half: a pass holds a lock in the database, not in a process, so a second
// replica is refused while it runs; and what one replica measured is in the
// database too, so another one reuses it instead of scanning the chunk again.
func TestCompactionIsOnePassClusterWideAndSharesWhatItMeasured(t *testing.T) {
	f := newCompactFixture(t)
	old := f.day(3)
	f.fill(t, old, 3000)
	f.deleteMost(t, old)
	f.settleStats(t)
	oldChunk := f.chunkOf(t, old)
	before := f.state(t, oldChunk)

	// With no reading of the volume at all a real pass refuses before it scans
	// anything: whatever it measured, it could rewrite nothing.
	blind := f.compactor()
	blind.Storage = func(context.Context) (StorageReading, error) {
		return StorageReading{}, errors.New("Storage monitoring is unavailable on this node.")
	}
	report, err := blind.Run(context.Background(), testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "skipped" || report.Reason != "room_unknown" || !strings.Contains(report.Detail, "unavailable on this node") || len(report.Measured) != 0 {
		t.Fatalf("a pass with no reading = %+v", report)
	}
	if n := f.progressRows(t); n != 0 {
		t.Fatalf("a pass with no reading recorded %d measurements", n)
	}

	// Replica A measures, and finds no room: the volume is all but full.
	a := f.compactor()
	a.Storage = func(context.Context) (StorageReading, error) {
		return StorageReading{ObservedAt: a.now(), DataCapacityBytes: 2 << 30, DataBytes: 2 << 30, WALCapacityBytes: 1 << 30}, nil
	}
	report, err = a.Run(context.Background(), testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "skipped" || report.Reason != "no_room" || len(report.Measured) != 1 || report.Room == nil || report.Room.NeedBytes <= 0 {
		t.Fatalf("replica A = %+v", report)
	}
	if got := f.state(t, oldChunk); got != before {
		t.Fatal("a pass without room touched the chunk")
	}

	// Another session holds the pass's lock: replica B is refused before it
	// reads anything.
	holder, closeHolder, err := f.session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var held bool
	if err := holder.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock($1)`, compactionLockKey).Scan(&held); err != nil || !held {
		t.Fatalf("could not take the pass's lock: %v %v", held, err)
	}
	b := f.compactor()
	report, err = b.Run(context.Background(), testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "skipped" || report.Reason != "already_running" || report.Closed != 0 {
		t.Fatalf("replica B ran beside a held lock: %+v", report)
	}
	closeHolder()

	// With the lock free, B compacts on A's measurement without scanning again.
	report, err = b.Run(context.Background(), testPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "compacted" || report.Chunk.Chunk != oldChunk || len(report.Measured) != 0 {
		t.Fatalf("replica B = %+v", report)
	}
	if report.WAL == nil || report.WAL.ArchiveMode == "" {
		t.Errorf("the WAL guard did not read the archiver: %+v", report.WAL)
	}
}
