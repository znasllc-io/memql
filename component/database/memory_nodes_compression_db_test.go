package database_test

// memory_nodes_compression_db_test.go -- "MemoryNodes" gets the compression
// 20260609 intended, on a fresh install and on an install from before it, and a
// later boot changes nothing (memql#5421).
//
// Every case runs on a database of its OWN, created from template0 -- no
// extensions at all, the shape CloudNativePG's initdb hands the engine -- and
// taken through the REAL lifecycle: NewTimescaleDBDatabase, Start, the whole
// migration set, the post-migration hook. The shared test database cannot stand
// in for one: whichever package reached it first already migrated it.
//
// The catalog is read through values this code owns -- the relation's own
// schema and name, the policy's compress_after -- never through a job or view
// name TimescaleDB chooses, which moves between versions.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/core/common"
)

// scratchDatabase creates an empty database beside the one dbtest.DSN names and
// drops it when the test ends. It needs CREATEDB, which the db-tests lane's
// service user has.
func scratchDatabase(t *testing.T) string {
	t.Helper()
	adminDSN := dbtest.DSN()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(adminDSN))), pgdialect.New())
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ping); err != nil {
		_ = admin.Close()
		dbtest.Unreachable(t, "MemoryNodes compression on a fresh install", adminDSN, err)
		return ""
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "memql_compression_" + hex.EncodeToString(suffix)
	if _, err := admin.ExecContext(context.Background(), `CREATE DATABASE `+name+` TEMPLATE template0`); err != nil {
		_ = admin.Close()
		t.Fatalf("create a scratch database: %v", err)
	}
	t.Cleanup(func() {
		// Housekeeping, not an assertion: DROP DATABASE forces a checkpoint,
		// which a busy machine can hold up for a long time, and a slow drop
		// says nothing about what the case tested. A drop that does not land
		// is named so it can be done by hand.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Logf("could not drop the scratch database %s (drop it by hand): %v", name, err)
		}
		_ = admin.Close()
	})
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse %s: %v", dbtest.SafeDSN(adminDSN), err)
	}
	u.Path = "/" + name
	return u.String()
}

func openScratch(t *testing.T, dsn string) *bun.DB {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func execScratch(t *testing.T, db *bun.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", strings.Join(strings.Fields(q), " "), err)
	}
}

// bootScratch takes the database through one node start: migrations, then hooks.
func bootScratch(t *testing.T, dsn string) {
	t.Helper()
	var base database.Database
	db, err := database.NewTimescaleDBDatabase(common.ComponentName("compression-test"),
		database.WithDSN(dsn), base.WithLoggerWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	db.Start(context.Background())
	defer db.Stop(context.Background())
	select {
	case <-db.Ready():
	case <-time.After(3 * time.Minute):
		t.Fatal("the database did not become ready within three minutes")
	}
	if err := db.MigrationError(); err != nil {
		t.Fatalf("boot: %v", err)
	}
}

// compressionPolicy is one compression job on a table.
type compressionPolicy struct {
	jobID         int64
	schedule      string
	compressAfter string
}

// tableCompression is what the catalog says about one public table.
type tableCompression struct {
	hypertable bool
	enabled    bool
	// settings is attname:segmentby:orderby:asc for each configured column,
	// sorted -- empty when compression has no settings.
	settings string
	policies []compressionPolicy
}

func readCompression(t *testing.T, db *bun.DB, table string) tableCompression {
	t.Helper()
	ctx := context.Background()
	var c tableCompression
	err := db.QueryRowContext(ctx, `SELECT compression_enabled FROM timescaledb_information.hypertables
		 WHERE hypertable_schema = 'public' AND hypertable_name = ?`, table).Scan(&c.enabled)
	switch {
	case err == sql.ErrNoRows:
		return c
	case err != nil:
		t.Fatalf("read %s from the hypertable catalog: %v", table, err)
	}
	c.hypertable = true
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(string_agg(format('%s:%s:%s:%s', attname,
		       COALESCE(segmentby_column_index::text, ''), COALESCE(orderby_column_index::text, ''), COALESCE(orderby_asc::text, '')),
		       ',' ORDER BY attname), '')
		  FROM timescaledb_information.compression_settings
		 WHERE hypertable_schema = 'public' AND hypertable_name = ?`, table).Scan(&c.settings); err != nil {
		t.Fatalf("read %s's compression settings: %v", table, err)
	}
	rows, err := db.QueryContext(ctx, `SELECT job_id, schedule_interval::text, config->>'compress_after'
		  FROM timescaledb_information.jobs
		 WHERE hypertable_schema = 'public' AND hypertable_name = ? AND config->>'compress_after' IS NOT NULL
		 ORDER BY job_id`, table)
	if err != nil {
		t.Fatalf("read %s's compression policies: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var p compressionPolicy
		if err := rows.Scan(&p.jobID, &p.schedule, &p.compressAfter); err != nil {
			t.Fatal(err)
		}
		c.policies = append(c.policies, p)
	}
	return c
}

// compressionOutcome is what the last boot recorded about MemoryNodes compression
// on the hook's status node.
func compressionOutcome(t *testing.T, db *bun.DB) string {
	t.Helper()
	var outcome sql.NullString
	if err := db.QueryRowContext(context.Background(), `SELECT payload->'compression'->>'MemoryNodes'
		  FROM "MemoryNodes" WHERE id = 'system.timescaledb.status' ORDER BY "createdAt" DESC LIMIT 1`).Scan(&outcome); err != nil {
		t.Fatalf("read the TimescaleDB status node: %v", err)
	}
	return outcome.String
}

// The settings 20260609000000_document_version_history.up.sql states: segment
// by concept, order by "createdAt" DESC.
const intendedSettings = "concept:1::,createdAt::1:false"

// workRunHeadDelete is the trigger 20260926020000_work_run_heads puts on
// MemoryNodes: statement-level, AFTER DELETE, with a transition table -- the one
// kind TimescaleDB will not compress beside.
const workRunHeadDelete = "work_run_head_delete"

// TestAFreshInstallCompressesMemoryNodesOnceNothingForbidsIt takes one fresh
// install through five boots. While work_run_head_delete stands, every boot
// says compression is blocked and by what, and changes nothing. Once it is gone
// the next boot enables exactly what 20260609 intended, a later boot changes
// nothing, and a policy the operator removed stays removed.
func TestAFreshInstallCompressesMemoryNodesOnceNothingForbidsIt(t *testing.T) {
	dsn := scratchDatabase(t)
	db := openScratch(t, dsn)

	bootScratch(t, dsn)
	var schema string
	if err := db.QueryRowContext(context.Background(), `SELECT n.nspname FROM pg_extension e
		JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'timescaledb'`).Scan(&schema); err != nil || schema != "public" {
		t.Fatalf("timescaledb is installed in %q (%v); this case is about the fresh-install shape, where it is in public", schema, err)
	}
	blocked := readCompression(t, db, "MemoryNodes")
	if !blocked.hypertable || blocked.enabled || blocked.settings != "" || len(blocked.policies) != 0 {
		t.Fatalf("with %s on the table, a fresh install left MemoryNodes at %+v; want a hypertable with nothing compressed", workRunHeadDelete, blocked)
	}
	if got := compressionOutcome(t, db); got != database.CompressionBlockedPrefix+workRunHeadDelete {
		t.Fatalf("the first boot recorded %q, want %q", got, database.CompressionBlockedPrefix+workRunHeadDelete)
	}
	bootScratch(t, dsn)
	if again := readCompression(t, db, "MemoryNodes"); again.enabled || again.settings != "" || len(again.policies) != 0 {
		t.Fatalf("a second blocked boot changed MemoryNodes: %+v", again)
	}

	// The conflict resolved: a DELETE capture TimescaleDB can compress beside.
	execScratch(t, db, `DROP TRIGGER `+workRunHeadDelete+` ON "MemoryNodes"`)
	bootScratch(t, dsn)
	first := readCompression(t, db, "MemoryNodes")
	if !first.hypertable || !first.enabled {
		t.Fatalf("once nothing forbids it, MemoryNodes is hypertable=%v compression_enabled=%v, want both", first.hypertable, first.enabled)
	}
	if first.settings != intendedSettings {
		t.Errorf("MemoryNodes compression settings = %q, want %q", first.settings, intendedSettings)
	}
	if len(first.policies) != 1 || first.policies[0].compressAfter != "90 days" || first.policies[0].schedule == "" {
		t.Fatalf("MemoryNodes compression policies = %+v, want exactly one compressing after 90 days", first.policies)
	}
	t.Logf("policy job %d runs every %s", first.policies[0].jobID, first.policies[0].schedule)
	if got := compressionOutcome(t, db); got != database.CompressionEnabledThisBoot {
		t.Errorf("the enabling boot recorded %q, want %q", got, database.CompressionEnabledThisBoot)
	}
	// Nothing intends compression for SecretMemoryNodes, so nothing gives it any.
	if secret := readCompression(t, db, "SecretMemoryNodes"); !secret.hypertable || secret.enabled || len(secret.policies) != 0 {
		t.Errorf("SecretMemoryNodes = %+v, want a hypertable with no compression", secret)
	}

	bootScratch(t, dsn)
	second := readCompression(t, db, "MemoryNodes")
	if second.enabled != first.enabled || second.settings != first.settings || len(second.policies) != 1 || second.policies[0] != first.policies[0] {
		t.Fatalf("a later boot changed MemoryNodes compression:\n before %+v\n after  %+v", first, second)
	}
	if got := compressionOutcome(t, db); got != database.CompressionAlreadyEnabled {
		t.Errorf("the later boot recorded %q, want %q", got, database.CompressionAlreadyEnabled)
	}

	// The operator's lever: remove the POLICY and keep the settings. The node
	// must not put it back.
	execScratch(t, db, `SELECT remove_compression_policy('"MemoryNodes"'::regclass)`)
	bootScratch(t, dsn)
	third := readCompression(t, db, "MemoryNodes")
	if !third.enabled || third.settings != intendedSettings || len(third.policies) != 0 {
		t.Fatalf("after the operator removed the policy, a boot left MemoryNodes at %+v; want the settings kept and no policy re-added", third)
	}
	if got := compressionOutcome(t, db); got != database.CompressionAlreadyEnabled {
		t.Errorf("the boot after the policy's removal recorded %q, want %q", got, database.CompressionAlreadyEnabled)
	}
}

// TestAnInstallFromBefore20260609ConvergesWhenItsMigrationReplays: an install
// from before 20260609 had MemoryNodes as a hypertable -- the hook converted it
// at its first boot -- by the time the upgrade ran 20260609. That is the path
// the migration's own guard was written for. Wound back to exactly that state
// (with nothing forbidding compression) and booted, it must end compressed,
// once.
func TestAnInstallFromBefore20260609ConvergesWhenItsMigrationReplays(t *testing.T) {
	dsn := scratchDatabase(t)
	db := openScratch(t, dsn)
	bootScratch(t, dsn)
	execScratch(t, db, `DROP TRIGGER `+workRunHeadDelete+` ON "MemoryNodes"`)
	bootScratch(t, dsn)

	execScratch(t, db, `SELECT remove_compression_policy('"MemoryNodes"'::regclass, if_exists => TRUE)`)
	execScratch(t, db, `SELECT decompress_chunk(c, true) FROM show_chunks('"MemoryNodes"'::regclass) c`)
	execScratch(t, db, `ALTER TABLE "MemoryNodes" SET (timescaledb.compress = false)`)
	execScratch(t, db, `DELETE FROM bun_migrations WHERE name = '20260609000000'`)
	if before := readCompression(t, db, "MemoryNodes"); !before.hypertable || before.enabled || len(before.policies) != 0 {
		t.Fatalf("the wind-back did not leave a hypertable with no compression: %+v", before)
	}

	bootScratch(t, dsn)
	var replayed int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM bun_migrations WHERE name = '20260609000000'`).Scan(&replayed); err != nil || replayed != 1 {
		t.Fatalf("20260609000000 was not replayed by the boot (%d rows, %v)", replayed, err)
	}
	got := readCompression(t, db, "MemoryNodes")
	if !got.enabled || got.settings != intendedSettings || len(got.policies) != 1 || got.policies[0].compressAfter != "90 days" {
		t.Fatalf("after 20260609 replayed on a hypertable, MemoryNodes = %+v; want the intended settings and exactly one 90-day policy", got)
	}
	// Who turned it on is the migration's business, not this case's -- but it
	// is worth seeing: "enabled" means the replayed migration did not.
	t.Logf("the replay boot recorded %q", compressionOutcome(t, db))
}

// TestCompressionAnswersEveryStateItCanFind calls the hook's step directly:
// no extension, a plain table, a same-named hypertable in another schema, a
// hypertable carrying a DELETE trigger with a transition table, and then one
// with nothing in the way. Each is answered, and only the last is changed.
func TestCompressionAnswersEveryStateItCanFind(t *testing.T) {
	dsn := scratchDatabase(t)
	db := openScratch(t, dsn)
	ctx := context.Background()
	logs := &levelLog{}
	ensure := func() string {
		logs.reset()
		return database.EnsureMemoryNodesCompression(ctx, db, slog.New(logs))
	}

	if got := ensure(); got != database.CompressionNoTimescaleDB {
		t.Fatalf("without the timescaledb extension: %q, want %q", got, database.CompressionNoTimescaleDB)
	}

	execScratch(t, db, `CREATE EXTENSION IF NOT EXISTS timescaledb`)
	execScratch(t, db, `CREATE TABLE "MemoryNodes" (id TEXT NOT NULL, "createdAt" TIMESTAMPTZ NOT NULL, concept TEXT NOT NULL, PRIMARY KEY (id, "createdAt"))`)
	// A hypertable of the same name in ANOTHER schema must not stand in for
	// the one this connection's MemoryNodes resolves to.
	execScratch(t, db, `CREATE SCHEMA elsewhere`)
	execScratch(t, db, `CREATE TABLE elsewhere."MemoryNodes" (id TEXT NOT NULL, "createdAt" TIMESTAMPTZ NOT NULL, concept TEXT NOT NULL, PRIMARY KEY (id, "createdAt"))`)
	execScratch(t, db, `SELECT create_hypertable('elsewhere."MemoryNodes"'::regclass, 'createdAt')`)
	if got := ensure(); got != database.CompressionNotAHypertable {
		t.Fatalf("with MemoryNodes a plain table: %q, want %q", got, database.CompressionNotAHypertable)
	}
	if plain := readCompression(t, db, "MemoryNodes"); plain.hypertable {
		t.Fatalf("the plain table became %+v", plain)
	}
	var elsewhere bool
	if err := db.QueryRowContext(ctx, `SELECT compression_enabled FROM timescaledb_information.hypertables
		 WHERE hypertable_schema = 'elsewhere' AND hypertable_name = 'MemoryNodes'`).Scan(&elsewhere); err != nil || elsewhere {
		t.Fatalf("the other schema's hypertable was given compression (%v, %v)", elsewhere, err)
	}

	execScratch(t, db, `SELECT create_hypertable('"MemoryNodes"'::regclass, 'createdAt')`)
	execScratch(t, db, `CREATE FUNCTION capture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$`)
	// INSERT and UPDATE transition triggers do not stand in the way; DELETE does.
	execScratch(t, db, `CREATE TRIGGER capture_insert AFTER INSERT ON "MemoryNodes" REFERENCING NEW TABLE AS n FOR EACH STATEMENT EXECUTE FUNCTION capture()`)
	execScratch(t, db, `CREATE TRIGGER capture_delete AFTER DELETE ON "MemoryNodes" REFERENCING OLD TABLE AS o FOR EACH STATEMENT EXECUTE FUNCTION capture()`)

	// An ordinary read held open on another connection: what a busy node
	// looks like to a booting one.
	openRead := func() bun.Tx {
		t.Helper()
		reader, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ExecContext(ctx, `SELECT count(*) FROM "MemoryNodes"`); err != nil {
			t.Fatal(err)
		}
		return reader
	}

	// Blocked is answered from the catalog, by name, even behind an open read.
	// TimescaleDB would refuse the ALTER too, but as a raw error that reads
	// like a failure the next boot might get past.
	reader := openRead()
	if got := ensure(); got != database.CompressionBlockedPrefix+"capture_delete" {
		t.Fatalf("with a DELETE transition trigger behind an open read: %q, want %q", got, database.CompressionBlockedPrefix+"capture_delete")
	}
	// Blocked is the schema's standing state on every boot of every node:
	// said at INFO, never WARN, since nothing an operator does changes it.
	if got := logs.levels(); len(got) != 1 || got[0] != slog.LevelInfo {
		t.Errorf("the blocked answer logged at %v, want exactly one INFO", got)
	}
	if err := reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	if held := readCompression(t, db, "MemoryNodes"); held.enabled {
		t.Fatalf("a blocked table was given compression: %+v", held)
	}

	// With nothing in the way but an open read, the enabling boot waits for
	// the lock only as long as its lock_timeout, then gives up and says so --
	// rather than queueing every later query on the table behind its DDL.
	// "canceling statement due to lock timeout" is the server's own words for
	// exactly that, so a return at all, with them, is the proof.
	execScratch(t, db, `DROP TRIGGER capture_delete ON "MemoryNodes"`)
	reader = openRead()
	if got := ensure(); !strings.HasPrefix(got, "failed: ") || !strings.Contains(got, "lock timeout") {
		t.Fatalf("behind an open read: %q, want a failure naming the lock timeout", got)
	}
	// A real failure stays a WARN.
	if got := logs.levels(); len(got) != 1 || got[0] != slog.LevelWarn {
		t.Errorf("the failure logged at %v, want exactly one WARN", got)
	}
	if err := reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	if held := readCompression(t, db, "MemoryNodes"); held.enabled {
		t.Fatalf("a boot that could not get the lock changed the table: %+v", held)
	}

	// The next boot finds the table free.
	if got := ensure(); got != database.CompressionEnabledThisBoot {
		t.Fatalf("with nothing in the way: %q, want %q", got, database.CompressionEnabledThisBoot)
	}
	if on := readCompression(t, db, "MemoryNodes"); !on.enabled || on.settings != intendedSettings || len(on.policies) != 1 {
		t.Fatalf("MemoryNodes after enabling = %+v", on)
	}
	if got := ensure(); got != database.CompressionAlreadyEnabled {
		t.Fatalf("a second call: %q, want %q", got, database.CompressionAlreadyEnabled)
	}
}

// levelLog is a slog handler that keeps the level of every record, so a case
// can say how loudly something was said.
type levelLog struct {
	mu   sync.Mutex
	seen []slog.Level
}

func (l *levelLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *levelLog) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, r.Level)
	return nil
}
func (l *levelLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *levelLog) WithGroup(string) slog.Handler      { return l }
func (l *levelLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = nil
}
func (l *levelLog) levels() []slog.Level {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]slog.Level(nil), l.seen...)
}

// TestTheMigrationPoolLiftsTheDecompressionCap: the migration runner's
// connections carry an unlimited DML decompression cap, and request-serving
// connections keep TimescaleDB's default -- set on connect, so it holds across
// reconnects.
func TestTheMigrationPoolLiftsTheDecompressionCap(t *testing.T) {
	dsn := dbtest.DSN()
	show := func(params func() map[string]any) string {
		t.Helper()
		db, err := database.PgSQLOpener(params)("pg", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var got string
		if err := db.QueryRowContext(context.Background(), "SHOW "+database.MigrationDecompressionCapParam).Scan(&got); err != nil {
			dbtest.Unreachable(t, "the migration pool's decompression cap", dsn, err)
		}
		return got
	}
	if got := show(database.MigrationConnParams); got != "0" {
		t.Errorf("the migration pool's %s = %q, want 0 (unlimited)", database.MigrationDecompressionCapParam, got)
	}
	if got := show(database.SessionConnParams); got == "0" {
		t.Errorf("a request-serving connection's %s = 0; the cap guards request traffic and must stay", database.MigrationDecompressionCapParam)
	}
}
