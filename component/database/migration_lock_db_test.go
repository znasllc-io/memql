package database

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
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

// lockedMigrationRunner is a replica migrating one scratch schema: its
// migration session is the production one, dialled on the test database with
// the schema as its search_path and its application_name, so
// migrationLockDB's cleanup reaches the session's backends too.
func lockedMigrationRunner(t *testing.T, schema string, migrations *migrate.Migrations) *Database {
	t.Helper()
	return sessionMigrationRunner(t, schema, schema, migrations)
}

// sessionMigrationRunner is lockedMigrationRunner with the session's
// application_name chosen, for a replica the test must tell apart.
func sessionMigrationRunner(t *testing.T, schema, app string, migrations *migrate.Migrations) *Database {
	t.Helper()
	cfg := &Database{}
	d := newMigrationTestDB(t, cfg.WithMigrateOnStart(true))
	d.config.migrations = migrations
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	d.config.dsn = &dsn
	d.config.migrationConnector = func(dsn string) (driver.Connector, error) {
		params := migrationConnParams()
		params["search_path"] = schema
		return pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithConnParams(params),
			pgdriver.WithApplicationName(app), pgdriver.WithReadTimeout(0)), nil
	}
	return d
}

func TestMigrationLockBlocksAnotherReplicaAndAllowsRetry(t *testing.T) {
	db, _, schema := migrationLockDB(t, time.Second)
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
	runner := lockedMigrationRunner(t, schema, migrations)
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
	db, _, schema := migrationLockDB(t, time.Second)
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
	first := lockedMigrationRunner(t, schema, migrations)
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
	second := lockedMigrationRunner(t, schema, repaired)
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

// A STATEMENT CUT OFF INSIDE A MIGRATION HOLDS THE LOCK WHILE ITS BACKEND RUNS
// IT, AND ONLY THAT LONG. Nothing knows what an arbitrary migration left
// running, which is why the runner used to keep bun's row for an operator. The
// statement ran on the attempt's session, and that session's backend holds the
// migration lock, so no other replica overlaps it, and once it ends the next
// attempt migrates without anyone clearing anything.
func TestMigrationReadTimeoutRetainsLockWhileBackendRuns(t *testing.T) {
	// The lock and bun's bookkeeping must finish before the Up's own 100 ms
	// timeout, so the timeout lands on the statement this test is about.
	db, admin, schema := migrationLockDB(t, 5*time.Second)
	migrations := migrate.NewMigrations()
	calls := 0
	migrations.Add(migrate.Migration{Name: "20260925000000", Up: func(ctx context.Context, m *migrate.Migrator, _ *migrate.Migration) error {
		calls++
		if calls > 1 {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		_, err := m.DB().ExecContext(ctx, "SELECT pg_sleep(3)")
		return err
	}})
	runner := lockedMigrationRunner(t, schema, migrations)
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
	sibling := lockedMigrationRunner(t, schema, migrations)
	sibling.runMigrations(context.Background(), db)
	if calls != 1 || !errors.Is(sibling.MigrationError(), ErrMigrationLockHeld) || !strings.Contains(sibling.MigrationError().Error(), "pg_sleep") {
		t.Fatalf("second replica overlapped orphan, or was refused without naming it: calls=%d err=%v", calls, sibling.MigrationError())
	}

	// The statement ends, its backend with it, and the lock with the backend.
	waitUntil(t, 15*time.Second, func() bool {
		var running int
		if err := admin.NewRaw("SELECT count(*) FROM pg_stat_activity WHERE application_name = ? AND query LIKE 'SELECT pg_sleep%'", schema).Scan(context.Background(), &running); err != nil {
			t.Fatal(err)
		}
		return running == 0
	}, func() string { return "the cut-off statement's backend never ended" })
	sibling.runMigrations(context.Background(), db)
	if calls != 2 || sibling.MigrationError() != nil {
		t.Fatalf("the attempt after the cut-off statement ended did not migrate: calls=%d err=%v", calls, sibling.MigrationError())
	}
	if held := migrationLockRows(t, db); held != 0 {
		t.Fatalf("the finishing attempt left %d lock rows", held)
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
	return lookupIndexMigrations(table, index)
}

// lookupIndexMigrations is lookupIndexMigration's migration alone, for a
// process migrating a table another one created.
func lookupIndexMigrations(table, index string) *migrate.Migrations {
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

// A BUILD THAT OUTLIVES ITS ATTEMPT HOLDS THE LOCK FOR AS LONG AS IT RUNS, AND
// NO LONGER. Production's migrations stopped on this for days: a CREATE INDEX
// over a large table outlived the attempt's deadline, the runner kept bun's row
// "until an operator verifies the backend has stopped", no operator knew to,
// and every later migration sat behind a build that had long since finished.
// Now the build's backend IS the lock: the attempt's one session ran the build,
// so the migration lock goes when the build does. Until then nothing overlaps
// it: another replica is refused, an engine that locks by row finds the fence,
// and this node's own next attempt waits for its build instead of failing.
func TestAnIndexBuildThatOutlivesItsAttemptHoldsTheLockUntilItEnds(t *testing.T) {
	db, admin, schema := migrationLockDB(t, time.Second)
	ctx := context.Background()
	table, index := schema+"_nodes", "lock_"+schema[len("migration_lock_"):]+"_idx"
	migrations := lookupIndexMigration(t, db, table, index)
	hooks := 0
	hook := func(context.Context, *bun.DB, *slog.Logger) error { hooks++; return nil }
	replica := func() *Database {
		runner := lockedMigrationRunner(t, schema, migrations)
		runner.config.postMigrationHooks = []PostMigrationHook{hook}
		return runner
	}
	attempt := func(runner *Database, timeoutMs int) *Database {
		runner.config.migrationTimeoutMs = timeoutMs
		runner.runMigrations(ctx, db)
		return runner
	}

	// An open writer holds the build on its ShareLock past every deadline below,
	// the way a long scan would.
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	if _, err := writer.ExecContext(ctx, `LOCK TABLE `+quoteIdentifier(table)+` IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	node := replica()
	if err := attempt(node, 2500).MigrationError(); !errors.Is(err, ErrMigrationDeferred) {
		t.Fatalf("an index build outliving its attempt did not defer: %v", err)
	}
	if active := activeIndexBuilds(t, admin, schema); active != 1 {
		t.Fatalf("expected the cut-off build still running, got %d", active)
	}

	// The fence stays while the build runs, so an engine that locks by row
	// keeps out: its Lock is bun's own.
	var fence int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM bun_migration_locks`).Scan(&fence); err != nil || fence >= 0 {
		t.Fatalf("expected the session's fence (a negative id) while the build runs: id=%d err=%v", fence, err)
	}
	older := migrate.NewMigrator(db, migrations)
	if err := older.Lock(ctx); err == nil {
		_ = older.Unlock(ctx)
		t.Fatal("an engine that locks by row took the lock while the build ran")
	}

	// Another replica is refused while the build runs, and is told what by.
	other := attempt(replica(), 2500)
	if err := other.MigrationError(); !errors.Is(err, ErrMigrationLockHeld) || !strings.Contains(err.Error(), "CREATE INDEX") {
		t.Fatalf("another replica beside the running build was not refused naming it: %v", err)
	}
	if active := activeIndexBuilds(t, admin, schema); active != 1 {
		t.Fatalf("an attempt overlapped the cut-off build: %d builds running", active)
	}

	// This node's next attempt waits for its own build and, when its attempt
	// runs out first, defers rather than refusing itself.
	err = attempt(node, 1500).MigrationError()
	if !errors.Is(err, ErrMigrationDeferred) || !strings.Contains(err.Error(), "earlier migration statement") {
		t.Fatalf("this node's attempt did not wait for its own running build: %v", err)
	}
	if active := activeIndexBuilds(t, admin, schema); active != 1 {
		t.Fatalf("this node's attempt overlapped its own build: %d builds running", active)
	}
	if ledger := migrationLedgerNames(t, db); ledger != "" {
		t.Fatalf("recorded a migration whose index has not finished: %q", ledger)
	}
	if hooks != 0 {
		t.Fatalf("hooks ran over an unfinished schema: %d", hooks)
	}

	// The build ends. Nobody releases anything.
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	last := attempt(node, 30000)
	if err := last.MigrationError(); err != nil {
		t.Fatalf("the attempt once the build could finish: %v", err)
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

	runner := lockedMigrationRunner(t, schema, migrations)
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
			db, _, schema := migrationLockDB(t, 5*time.Second)
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
			runner := lockedMigrationRunner(t, schema, migrations)
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
	db, _, schema := migrationLockDB(t, 5*time.Second)
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

	runner := lockedMigrationRunner(t, schema, migrations)
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

// A SESSION HOLDING THE LOCK WHILE RUNNING NOTHING IS REPORTED; ONE RUNNING A
// STATEMENT IS NOT. A session releases the lock when its process ends, so one
// that has sat idle in the way for longer than any attempt is a process that
// stopped migrating and kept its session: migrations are stopped until it ends,
// which is what the ERROR and memql_database_migration_lock_held_seconds (and
// the release gates reading it) mean. A session running a statement ends the
// lock itself when the statement ends, as the dead replica's build did eight
// minutes after 2026-11-25's stranding began, so it starts the clock again.
func TestASessionHolderIsReportedStuckOnlyWhileItRunsNothing(t *testing.T) {
	db, admin, schema := migrationLockDB(t, 5*time.Second)
	ctx := context.Background()
	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{Name: "20260925000000", Up: func(context.Context, *migrate.Migrator, *migrate.Migration) error { return nil }})

	key := migrationLockKey(schema)
	holder, err := admin.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	var holderPid int64
	if err := holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		t.Fatal(err)
	}

	runner := lockedMigrationRunner(t, schema, migrations)
	logger, logs := migrationLogs()
	runner.Logger = logger
	runner.runMigrations(ctx, db)
	if !errors.Is(runner.MigrationError(), ErrMigrationLockHeld) || strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("a session just found holding the lock is contention: err=%v logs=%s", runner.MigrationError(), logs)
	}

	// The same idle session, seen for longer than any attempt can hold it.
	runner.heldLock.since = time.Now().Add(-migrationLockStuckAfter(runner.config.migrationTimeoutMs) - time.Minute)
	logs.Reset()
	runner.runMigrations(ctx, db)
	if reported := logs.String(); !strings.Contains(reported, "level=ERROR") || !strings.Contains(reported, "running no statement") ||
		!strings.Contains(reported, "env-vars.md") || !strings.Contains(reported, strconv.FormatInt(holderPid, 10)) {
		t.Fatalf("an idle session holding the lock past any attempt was not reported naming it: %s", reported)
	}
	if heldFor := runner.MigrationLockHeldFor(); heldFor < time.Minute {
		t.Fatalf("the node reports %s for an idle holder past its limit", heldFor)
	}

	// The holder runs a statement: it will end the lock itself.
	slept := make(chan error, 1)
	go func() { _, err := holder.ExecContext(ctx, `SELECT pg_sleep(2)`); slept <- err }()
	waitUntil(t, 5*time.Second, func() bool {
		var state string
		_ = admin.NewRaw("SELECT COALESCE(state, '') FROM pg_stat_activity WHERE pid = ?", holderPid).Scan(ctx, &state)
		return state == "active"
	}, func() string { return "the holder never started its statement" })
	logs.Reset()
	runner.runMigrations(ctx, db)
	if !errors.Is(runner.MigrationError(), ErrMigrationLockHeld) || strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("a holder running a statement was reported as stuck: err=%v logs=%s", runner.MigrationError(), logs)
	}
	if heldFor := runner.MigrationLockHeldFor(); heldFor != 0 {
		t.Fatalf("the node reports %s for a holder running a statement", heldFor)
	}
	if err := <-slept; err != nil {
		t.Fatal(err)
	}

	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
		t.Fatal(err)
	}
	runner.runMigrations(ctx, db)
	if err := runner.MigrationError(); err != nil {
		t.Fatalf("once the session let go: %v", err)
	}
}

func TestMigrationSQLFailureReleasesLockForRetry(t *testing.T) {
	db, _, schema := migrationLockDB(t, time.Second)
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
	runner := lockedMigrationRunner(t, schema, migrations)
	runner.runMigrations(context.Background(), db)
	if runner.MigrationError() == nil {
		t.Fatal("expected SQL error")
	}
	// A migration that FAILED is not a lock someone else holds: a caller
	// that waits out ErrMigrationLockHeld would otherwise spin on it.
	if errors.Is(runner.MigrationError(), ErrMigrationLockHeld) {
		t.Fatalf("a failed migration was classified as a held lock: %v", runner.MigrationError())
	}
	sibling := lockedMigrationRunner(t, schema, migrations)
	sibling.runMigrations(context.Background(), db)
	if calls != 2 || sibling.MigrationError() != nil {
		t.Fatalf("acknowledged SQL failure stranded lock: calls=%d err=%v", calls, sibling.MigrationError())
	}
}
