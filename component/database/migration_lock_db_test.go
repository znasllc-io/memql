package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"
)

func migrationLockDB(t *testing.T, readTimeout time.Duration) (*bun.DB, *bun.DB, string) {
	t.Helper()
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("MEMQL_DATABASE_DSN is required")
		}
		t.Skip("requires Postgres")
	}
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	t.Cleanup(func() { admin.Close() })
	schema := fmt.Sprintf("migration_lock_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE SCHEMA ?", bun.Ident(schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec("SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE application_name = ? AND pid <> pg_backend_pid()", schema)
		admin.Exec("DROP SCHEMA ? CASCADE", bun.Ident(schema))
	})
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithConnParams(map[string]any{"search_path": schema}), pgdriver.WithApplicationName(schema), pgdriver.WithReadTimeout(readTimeout))), pgdialect.New())
	t.Cleanup(func() { db.Close() })
	return db, admin, schema
}

func lockedMigrationRunner(t *testing.T, migrations *migrate.Migrations) *Database {
	t.Helper()
	cfg := &Database{}
	d := newMigrationTestDB(t, cfg.WithMigrateOnStart(true))
	d.config.migrations = migrations
	d.config.migrationOpener = nil
	return d
}

func TestMigrationLockBlocksAnotherReplicaAndAllowsRetry(t *testing.T) {
	db, _, _ := migrationLockDB(t, time.Second)
	ctx := context.Background()
	calls := 0
	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "20260925000000", Up: func(context.Context, *migrate.Migrator, *migrate.Migration) error { calls++; return nil }})
	owner := migrate.NewMigrator(db, migrations)
	if err := owner.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := owner.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	runner := lockedMigrationRunner(t, migrations)
	runner.runMigrations(ctx, db)
	if calls != 0 || runner.MigrationError() == nil || !runner.migrationsPending() {
		t.Fatalf("ran without the other replica's lock: calls=%d err=%v", calls, runner.MigrationError())
	}
	// "Held" is the refusal a caller may wait out (dbtest.EnsureSchema does),
	// so it must be recognisable without reading the message.
	if !errors.Is(runner.MigrationError(), ErrMigrationLockHeld) {
		t.Fatalf("a lock held by another migrator is not ErrMigrationLockHeld: %v", runner.MigrationError())
	}
	if err := owner.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	runner.runMigrations(ctx, db)
	if calls != 1 || runner.MigrationError() != nil || runner.migrationsPending() {
		t.Fatalf("retry failed: calls=%d err=%v", calls, runner.MigrationError())
	}
}

func TestFailedMigrationDefersHooksAndAnotherReplicaCanFinish(t *testing.T) {
	db, _, _ := migrationLockDB(t, time.Second)
	ctx := context.Background()
	wantErr := errors.New("schema repair is not ready")
	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "20260925000000", Up: func(ctx context.Context, m *migrate.Migrator, _ *migrate.Migration) error {
		// Leave a partially installed schema, as a failed SQL migration can.
		_, err := m.DB().ExecContext(ctx, `CREATE TABLE IF NOT EXISTS partial_schema (id int)`)
		if err != nil {
			return err
		}
		return wantErr
	}})
	calls := 0
	hook := func(context.Context, *bun.DB, *slog.Logger) error { calls++; return nil }
	first := lockedMigrationRunner(t, migrations)
	first.config.postMigrationHooks = []PostMigrationHook{hook}
	first.runMigrations(ctx, db)
	if calls != 0 || !errors.Is(first.MigrationError(), wantErr) || !first.migrationsPending() {
		t.Fatalf("failed schema ran a hook or lost its refusal: hooks=%d error=%v", calls, first.MigrationError())
	}

	// A different replica has no first-node state. Its repaired migration
	// must acquire the released lock, finish, and then run its post-hook.
	repaired := migrate.NewMigrations()
	repaired.Add(migrate.Migration{Name: "20260925000000", Up: func(ctx context.Context, m *migrate.Migrator, _ *migrate.Migration) error {
		_, err := m.DB().ExecContext(ctx, `ALTER TABLE partial_schema ADD COLUMN ready bool`)
		return err
	}})
	second := lockedMigrationRunner(t, repaired)
	second.config.postMigrationHooks = []PostMigrationHook{hook}
	second.runMigrations(ctx, db)
	if calls != 1 || second.MigrationError() != nil || second.migrationsPending() {
		t.Fatalf("second replica did not finish: hooks=%d error=%v", calls, second.MigrationError())
	}
	var readyColumns int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'partial_schema' AND column_name = 'ready'`).Scan(&readyColumns); err != nil || readyColumns != 1 {
		t.Fatalf("repair missing: ready columns=%d error=%v", readyColumns, err)
	}
}

func TestMigrationReadTimeoutRetainsLockWhileBackendRuns(t *testing.T) {
	// Initialization and lock acquisition must finish before the injected
	// timeout. A connection-wide 100ms timeout can fail CREATE TABLE on a
	// loaded runner, never reaching the orphaned migration this test covers.
	db, admin, schema := migrationLockDB(t, 5*time.Second)
	migrations := migrate.NewMigrations()
	calls := 0
	migrations.Add(migrate.Migration{Name: "20260925000000", Up: func(ctx context.Context, m *migrate.Migrator, _ *migrate.Migration) error {
		calls++
		ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		_, err := m.DB().ExecContext(ctx, "SELECT pg_sleep(3)")
		return err
	}})
	runner := lockedMigrationRunner(t, migrations)
	runner.runMigrations(context.Background(), db)
	if runner.MigrationError() == nil {
		t.Fatal("expected client timeout")
	}
	if calls != 1 {
		t.Fatalf("did not reach the timed-out migration: calls=%d err=%v", calls, runner.MigrationError())
	}
	var active int
	if err := admin.NewRaw("SELECT count(*) FROM pg_stat_activity WHERE application_name = ? AND state = 'active' AND query LIKE 'SELECT pg_sleep%'", schema).Scan(context.Background(), &active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("expected abandoned backend still executing, got %d", active)
	}
	sibling := lockedMigrationRunner(t, migrations)
	sibling.runMigrations(context.Background(), db)
	if calls != 1 || sibling.MigrationError() == nil || !strings.Contains(sibling.MigrationError().Error(), "migration lock") {
		t.Fatalf("second replica overlapped orphan: calls=%d err=%v", calls, sibling.MigrationError())
	}
}

// migrationLockRows counts the rows of bun's lock table in this test's schema.
func migrationLockRows(t *testing.T, db *bun.DB) int {
	t.Helper()
	var held int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM bun_migration_locks`).Scan(&held); err != nil {
		t.Fatalf("read bun_migration_locks: %v", err)
	}
	return held
}

// migrationLedgerNames lists what this test's schema's ledger records as applied.
func migrationLedgerNames(t *testing.T, db *bun.DB) string {
	t.Helper()
	var names []string
	rows, err := db.QueryContext(context.Background(), `SELECT name FROM bun_migrations ORDER BY name`)
	if err != nil {
		t.Fatalf("read bun_migrations: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(names, ",")
}

// lookupIndexMigration is what every index migration is: one Up that ensures
// a payload lookup index, on a scratch table of this test's schema.
func lookupIndexMigration(t *testing.T, db *bun.DB, table, index string) *migrate.Migrations {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE `+quoteIdentifier(table)+
		` (id text NOT NULL, concept text NOT NULL, "createdAt" timestamptz NOT NULL, payload jsonb)`); err != nil {
		t.Fatal(err)
	}
	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "20260925000000", Up: func(ctx context.Context, m *migrate.Migrator, _ *migrate.Migration) error {
		return ensurePayloadLookupIndex(ctx, m.DB(), nil, table, index, "field", []string{"v1:test:lock"})
	}})
	return migrations
}

// activeIndexBuilds counts the CREATE INDEX statements this test's client
// backends are running, whichever client is still waiting on them. A parallel
// build's workers show the same query, so they are left out.
func activeIndexBuilds(t *testing.T, admin *bun.DB, schema string) int {
	t.Helper()
	var active int
	if err := admin.NewRaw("SELECT count(*) FROM pg_stat_activity WHERE application_name = ? AND backend_type = 'client backend' AND state = 'active' AND query LIKE 'CREATE INDEX%'", schema).Scan(context.Background(), &active); err != nil {
		t.Fatal(err)
	}
	return active
}

// A BUILD THAT OUTLIVES ITS ATTEMPT MUST NOT STRAND THE LOCK. Production's
// migrations stopped on this for days: a CREATE INDEX over a large table
// outlived the attempt's deadline, the runner kept bun's lock "until an operator
// verifies the backend has stopped", no operator knew to, and every later
// migration sat behind a build that had long since finished. The runner's rule
// exists so that nothing overlaps an orphan -- and for an index build something
// already prevents that: the orphan holds the build lock until its backend
// exits, and the next attempt takes that lock before it does anything. So the
// attempt defers, and the attempts after it wait their turn on the build lock
// and judge the result from the catalog.
func TestAnIndexBuildThatOutlivesItsAttemptReleasesTheLock(t *testing.T) {
	db, admin, schema := migrationLockDB(t, time.Second)
	ctx := context.Background()
	table, index := schema+"_nodes", "lock_"+schema[len("migration_lock_"):]+"_idx"
	migrations := lookupIndexMigration(t, db, table, index)
	hooks := 0
	hook := func(context.Context, *bun.DB, *slog.Logger) error { hooks++; return nil }
	attempt := func(timeoutMs int) *Database {
		runner := lockedMigrationRunner(t, migrations)
		runner.config.migrationTimeoutMs = timeoutMs
		runner.config.postMigrationHooks = []PostMigrationHook{hook}
		runner.runMigrations(ctx, db)
		return runner
	}

	// An open writer holds the build on its ShareLock past both deadlines, the
	// way a long scan would.
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	if _, err := writer.ExecContext(ctx, `LOCK TABLE `+quoteIdentifier(table)+` IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	first := attempt(2500)
	if !errors.Is(first.MigrationError(), ErrMigrationDeferred) {
		t.Fatalf("an index build outliving its attempt did not defer: %v", first.MigrationError())
	}
	if migrationMayStillBeRunning(first.MigrationError()) {
		t.Fatalf("the deferral still reads as an uncertain outcome, so the runner would keep its lock: %v", first.MigrationError())
	}
	if held := migrationLockRows(t, db); held != 0 {
		t.Fatalf("the attempt stranded the lock: %d rows", held)
	}
	if active := activeIndexBuilds(t, admin, schema); active != 1 {
		t.Fatalf("expected the orphaned build still running, got %d", active)
	}

	// The next attempt takes the released lock while the orphan runs. It must
	// wait on the build lock, not start a second build, and leave the same way.
	second := attempt(2500)
	if !errors.Is(second.MigrationError(), ErrMigrationDeferred) {
		t.Fatalf("an attempt finding the orphan did not defer: %v", second.MigrationError())
	}
	if !strings.Contains(second.MigrationError().Error(), "build lock") {
		t.Fatalf("the deferral does not say what it waited for: %v", second.MigrationError())
	}
	if held := migrationLockRows(t, db); held != 0 {
		t.Fatalf("the second attempt stranded the lock: %d rows", held)
	}
	if active := activeIndexBuilds(t, admin, schema); active != 1 {
		t.Fatalf("an attempt overlapped the orphan: %d builds running", active)
	}
	if ledger := migrationLedgerNames(t, db); ledger != "" {
		t.Fatalf("recorded a migration whose index has not finished: %q", ledger)
	}
	if hooks != 0 {
		t.Fatalf("hooks ran over an unfinished schema: %d", hooks)
	}

	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for activeIndexBuilds(t, admin, schema) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the orphaned build never finished")
		}
		time.Sleep(50 * time.Millisecond)
	}

	last := attempt(30000)
	if err := last.MigrationError(); err != nil {
		t.Fatalf("the attempt after the orphan finished: %v", err)
	}
	if last.migrationsPending() {
		t.Fatal("migrations still pending once the index was verified")
	}
	if ledger := migrationLedgerNames(t, db); ledger != "20260925000000" {
		t.Fatalf("ledger = %q", ledger)
	}
	if held := migrationLockRows(t, db); held != 0 {
		t.Fatalf("the finishing attempt left the lock: %d rows", held)
	}
	if hooks != 1 {
		t.Fatalf("hooks ran %d times, want once", hooks)
	}
	var usable bool
	if err := db.QueryRowContext(ctx, `SELECT i.indisvalid AND i.indisready FROM pg_index i WHERE i.indexrelid = to_regclass(?)`, index).Scan(&usable); err != nil || !usable {
		t.Fatalf("the index is not usable: usable=%v err=%v", usable, err)
	}
}

// The site-access rollout note tells an operator to build the indexes by hand
// on a large table, and a release can come up while that session is running.
// Waiting for it must not cost the lock either.
func TestAnAttemptWaitingOnAnOperatorsBuildReleasesTheLock(t *testing.T) {
	db, admin, schema := migrationLockDB(t, time.Second)
	ctx := context.Background()
	table, index := schema+"_nodes", "lock_"+schema[len("migration_lock_"):]+"_idx"
	migrations := lookupIndexMigration(t, db, table, index)
	if _, err := db.ExecContext(ctx, `INSERT INTO `+quoteIdentifier(table)+` (id, concept, "createdAt")
		SELECT 'row' || g, 'v1:test:lock', now() FROM generate_series(1, 40) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE FUNCTION slow_key(text) RETURNS text IMMUTABLE LANGUAGE plpgsql AS
		$$ BEGIN PERFORM pg_sleep(0.1); RETURN $1; END $$`); err != nil {
		t.Fatal(err)
	}

	built := make(chan error, 1)
	go func() {
		_, err := admin.ExecContext(ctx, `CREATE INDEX operator_build ON `+quoteIdentifier(schema)+`.`+quoteIdentifier(table)+` (`+quoteIdentifier(schema)+`.slow_key(id))`)
		built <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var running int
		if err := admin.NewRaw("SELECT count(*) FROM pg_stat_progress_create_index WHERE relid = to_regclass(?)", quoteIdentifier(schema)+"."+quoteIdentifier(table)).Scan(ctx, &running); err != nil {
			t.Fatal(err)
		}
		if running == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the operator's build never showed in pg_stat_progress_create_index")
		}
		time.Sleep(20 * time.Millisecond)
	}

	runner := lockedMigrationRunner(t, migrations)
	runner.config.migrationTimeoutMs = 1500
	runner.runMigrations(ctx, db)
	if !errors.Is(runner.MigrationError(), ErrMigrationDeferred) {
		t.Fatalf("waiting on an operator's build did not defer: %v", runner.MigrationError())
	}
	if held := migrationLockRows(t, db); held != 0 {
		t.Fatalf("waiting on an operator's build stranded the lock: %d rows", held)
	}
	if err := <-built; err != nil {
		t.Fatalf("the operator's build: %v", err)
	}
	runner.config.migrationTimeoutMs = 10000
	runner.runMigrations(ctx, db)
	if err := runner.MigrationError(); err != nil {
		t.Fatalf("the attempt after the operator finished: %v", err)
	}
}

// THE OTHER WAY AN ATTEMPT'S END STRANDED THE LOCK, with nothing running at
// all: on September 26 the sequence deadline expired between a migration
// finishing and bun recording it, and bun's statement refused on the expired
// context, which reads exactly like a statement that may still be running. An
// attempt ending between migrations has nothing outstanding, so what finished is
// recorded, what has not started does not, and the lock is released. A node
// stopping mid-attempt is the same case by cancellation.
func TestAnAttemptEndingBetweenMigrationsRecordsWhatFinishedAndReleasesTheLock(t *testing.T) {
	for name, cancelled := range map[string]bool{"deadline": false, "cancelled": true} {
		t.Run(name, func(t *testing.T) {
			db, _, _ := migrationLockDB(t, 5*time.Second)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			first, second := 0, 0
			migrations := migrate.NewMigrations()
			migrations.Add(migrate.Migration{Name: "20260925000000", Up: func(ctx context.Context, _ *migrate.Migrator, _ *migrate.Migration) error {
				first++
				if cancelled {
					cancel()
				}
				<-ctx.Done()
				return nil
			}})
			migrations.Add(migrate.Migration{Name: "20260925000001", Up: func(context.Context, *migrate.Migrator, *migrate.Migration) error {
				second++
				return nil
			}})
			runner := lockedMigrationRunner(t, migrations)
			runner.config.migrationTimeoutMs = 300
			runner.runMigrations(parent, db)
			if !errors.Is(runner.MigrationError(), ErrMigrationDeferred) {
				t.Fatalf("an attempt ending between migrations did not defer: %v", runner.MigrationError())
			}
			if first != 1 || second != 0 {
				t.Fatalf("first ran %d times, second %d; want 1 and 0", first, second)
			}
			if ledger := migrationLedgerNames(t, db); ledger != "20260925000000" {
				t.Fatalf("what finished was not recorded: ledger = %q", ledger)
			}
			if held := migrationLockRows(t, db); held != 0 {
				t.Fatalf("the attempt stranded the lock with nothing running: %d rows", held)
			}

			runner.runMigrations(context.Background(), db)
			if err := runner.MigrationError(); err != nil {
				t.Fatalf("the next attempt: %v", err)
			}
			if first != 1 || second != 1 {
				t.Fatalf("after the next attempt, first ran %d times and second %d; want 1 and 1", first, second)
			}
		})
	}
}

// migrationLogs is a logger to read back: what an operator would find.
func migrationLogs() (*slog.Logger, *bytes.Buffer) {
	var out bytes.Buffer
	return slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo})), &out
}

// A LOCK THAT STAYS HELD SAYS SO. Both replicas answered a stranded lock with
// the same WARN line a moment's contention earns, about 900 times each in eight
// hours, and nothing that read the node's state could tell. The row's id is the
// evidence: an attempt is bounded, and bun inserts a new row for each one, so
// one id seen for longer than an attempt can last is no attempt.
func TestALockThatOutlastsAnyAttemptIsReportedAsStuck(t *testing.T) {
	db, _, _ := migrationLockDB(t, 5*time.Second)
	ctx := context.Background()
	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "20260925000000", Up: func(context.Context, *migrate.Migrator, *migrate.Migration) error { return nil }})
	owner := migrate.NewMigrator(db, migrations)
	if err := owner.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := owner.Lock(ctx); err != nil {
		t.Fatal(err)
	}

	runner := lockedMigrationRunner(t, migrations)
	logger, logs := migrationLogs()
	runner.Logger = logger

	runner.runMigrations(ctx, db)
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("a lock just found held is contention, not a failure: %s", logs)
	}
	if !errors.Is(runner.MigrationError(), ErrMigrationLockHeld) {
		t.Fatalf("held lock: %v", runner.MigrationError())
	}

	// The same row, seen for longer than any attempt can hold it.
	runner.heldLock.since = time.Now().Add(-migrationLockStuckAfter(runner.config.migrationTimeoutMs) - time.Minute)
	logs.Reset()
	runner.runMigrations(ctx, db)
	reported := logs.String()
	if !strings.Contains(reported, "level=ERROR") {
		t.Fatalf("a lock outlasting any attempt was not reported as an error: %s", reported)
	}
	for _, want := range []string{"20260925000000", "env-vars.md"} {
		if !strings.Contains(reported, want) {
			t.Fatalf("the report does not name %q: %s", want, reported)
		}
	}
	if heldFor := runner.MigrationLockHeldFor(); heldFor < time.Minute {
		t.Fatalf("the node reports %s for a lock held past its limit", heldFor)
	}

	// A new row is a new attempt, however long the last one's row stayed.
	if err := owner.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := owner.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	runner.runMigrations(ctx, db)
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("a fresh lock row inherited the old one's age: %s", logs)
	}

	if err := owner.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	runner.runMigrations(ctx, db)
	if err := runner.MigrationError(); err != nil {
		t.Fatalf("once the lock came free: %v", err)
	}
	if heldFor := runner.MigrationLockHeldFor(); heldFor != 0 {
		t.Fatalf("the node reports %s with no lock in the way", heldFor)
	}
}

func TestMigrationSQLFailureReleasesLockForRetry(t *testing.T) {
	db, _, _ := migrationLockDB(t, time.Second)
	calls := 0
	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "20260925000000", Up: func(ctx context.Context, m *migrate.Migrator, _ *migrate.Migration) error {
		calls++
		if calls == 1 {
			_, err := m.DB().ExecContext(ctx, "SELECT 1/0")
			return err
		}
		return nil
	}})
	runner := lockedMigrationRunner(t, migrations)
	runner.runMigrations(context.Background(), db)
	if runner.MigrationError() == nil {
		t.Fatal("expected SQL error")
	}
	// A migration that FAILED is not a lock someone else holds: a caller
	// that waits out ErrMigrationLockHeld would otherwise spin on it.
	if errors.Is(runner.MigrationError(), ErrMigrationLockHeld) {
		t.Fatalf("a failed migration was classified as a held lock: %v", runner.MigrationError())
	}
	sibling := lockedMigrationRunner(t, migrations)
	sibling.runMigrations(context.Background(), db)
	if calls != 2 || sibling.MigrationError() != nil {
		t.Fatalf("acknowledged SQL failure stranded lock: calls=%d err=%v", calls, sibling.MigrationError())
	}
}
