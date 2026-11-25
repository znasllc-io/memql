package database

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"
)

// migrationHolderEnv hands a helper process the scratch schema, table, index
// and application_name of the replica it plays: one that is killed mid-build.
// main_dbschema_test.go's TestMain reads the same name, so the helper does not
// migrate the shared database first.
const migrationHolderEnv = "MEMQL_TEST_MIGRATION_HOLDER"

// A HOLDER THAT DIES MID-BUILD MUST NOT STRAND THE MIGRATION LOCK. Production's
// migrations stopped this way three times (2026-09-26, 2026-10-30, 2026-11-25).
// On 2026-11-25 an identity replica took the lock, started a calls lookup index,
// and its own engine-init deadline ended the process (os.Exit) while the CREATE
// INDEX was still running server-side. Nothing ran its unlock. The build finished
// eight minutes later, and every attempt after that, from both replicas, was
// refused until an operator deleted the lock row by hand.
//
// What must hold instead: nothing overlaps the dead holder's statement while it
// runs, and once it ends the next attempt migrates with no one touching the
// lock. The holder here is another process, killed with SIGKILL while its build
// waits on a writer's lock, so no deferred cleanup of its own can run.
func TestAHolderKilledMidBuildIsRecoveredOnceItsStatementEnds(t *testing.T) {
	if spec := os.Getenv(migrationHolderEnv); spec != "" {
		runKilledMigrationHolder(t, spec)
		return
	}

	db, admin, schema := migrationLockDB(t, time.Second)
	ctx := context.Background()
	table, index := schema+"_nodes", "lock_"+schema[len("migration_lock_"):]+"_idx"
	migrations := lookupIndexMigration(t, db, table, index)
	holderApp := schema + "_holder"
	t.Cleanup(func() {
		// Ahead of migrationLockDB's DROP SCHEMA, which would wait on a build the
		// test left running.
		admin.Exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = ?", holderApp)
	})

	hooks := 0
	hook := func(context.Context, *bun.DB, *slog.Logger) error { hooks++; return nil }
	attempt := func(timeoutMs int) *Database {
		runner := lockedMigrationRunner(t, schema, migrations)
		runner.config.migrationTimeoutMs = timeoutMs
		runner.config.postMigrationHooks = []PostMigrationHook{hook}
		runner.runMigrations(ctx, db)
		return runner
	}

	// An open writer holds the build on its ShareLock, the way the newest chunk's
	// writers held production's.
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	if _, err := writer.ExecContext(ctx, `LOCK TABLE `+quoteIdentifier(table)+` IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), migrationHolderEnv+"="+strings.Join([]string{schema, table, index, holderApp}, ","))
	var holderOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &holderOut, &holderOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	waitUntil(t, 60*time.Second, func() bool { return activeIndexBuildsOf(t, admin, holderApp) == 1 },
		func() string { return "the holder never started its build:\n" + holderOut.String() })

	// The process ends the way production's did, with nothing run on its way out.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	killed = true
	if running := activeIndexBuildsOf(t, admin, holderApp); running != 1 {
		t.Fatalf("expected the dead holder's build still running server-side, got %d", running)
	}

	// Another replica, while the dead holder's statement runs: it must not start
	// a second build beside it.
	other := attempt(2500)
	if !errors.Is(other.MigrationError(), ErrMigrationLockHeld) {
		t.Fatalf("an attempt beside the dead holder's running build was not refused as a held lock: %v", other.MigrationError())
	}
	if running := activeIndexBuildsOf(t, admin, holderApp) + activeIndexBuilds(t, admin, schema); running != 1 {
		t.Fatalf("an attempt overlapped the dead holder's build: %d builds running", running)
	}

	// The statement ends; its backend goes with it.
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 30*time.Second, func() bool { return backendsOf(t, admin, holderApp) == 0 },
		func() string { return "the dead holder's backend never exited" })

	// The next attempt migrates on its own: no operator, no row cleared by hand.
	last := attempt(30000)
	if err := last.MigrationError(); err != nil {
		t.Fatalf("the attempt after the dead holder's statement ended did not recover: %v", err)
	}
	if ledger := migrationLedgerNames(t, db); ledger != "20260925000000" {
		t.Fatalf("ledger = %q", ledger)
	}
	if held := migrationLockRows(t, db); held != 0 {
		t.Fatalf("the recovering attempt left %d lock rows", held)
	}
	if hooks != 1 {
		t.Fatalf("hooks ran %d times, want once", hooks)
	}
	var usable bool
	if err := db.QueryRowContext(ctx, `SELECT i.indisvalid AND i.indisready FROM pg_index i WHERE i.indexrelid = to_regclass(?)`, index).Scan(&usable); err != nil || !usable {
		t.Fatalf("the index is not usable: usable=%v err=%v", usable, err)
	}
}

// runKilledMigrationHolder is the helper process: a replica migrating the
// parent's scratch schema, whose build waits on the parent's writer until the
// parent kills it. Returning at all means the parent never did.
func runKilledMigrationHolder(t *testing.T, spec string) {
	parts := strings.Split(spec, ",")
	if len(parts) != 4 {
		t.Fatalf("%s = %q, want schema,table,index,application", migrationHolderEnv, spec)
	}
	schema, table, index, app := parts[0], parts[1], parts[2], parts[3]
	// What the post-migration hooks would run on; the migrations run on the
	// runner's own session, which is the backend the parent watches.
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn),
		pgdriver.WithConnParams(map[string]any{"search_path": schema}),
		pgdriver.WithApplicationName(app))), pgdialect.New())
	defer db.Close()
	runner := killedHolderRunner(t, schema, app, lookupIndexMigrations(table, index))
	runner.config.migrationTimeoutMs = 120000
	runner.runMigrations(context.Background(), db)
	t.Fatalf("the holder's attempt returned before the parent killed it: %v", runner.MigrationError())
}

// killedHolderRunner builds the helper's runner the way a replica builds its
// own, with a session the parent can find by its application_name.
func killedHolderRunner(t *testing.T, schema, app string, migrations *migrate.Migrations) *Database {
	t.Helper()
	return sessionMigrationRunner(t, schema, app, migrations)
}

// activeIndexBuildsOf counts the CREATE INDEX statements one application's
// client backends are running.
func activeIndexBuildsOf(t *testing.T, admin *bun.DB, app string) int {
	t.Helper()
	var active int
	if err := admin.NewRaw("SELECT count(*) FROM pg_stat_activity WHERE application_name = ? AND backend_type = 'client backend' AND state = 'active' AND query LIKE 'CREATE INDEX%'", app).Scan(context.Background(), &active); err != nil {
		t.Fatal(err)
	}
	return active
}

// backendsOf counts one application's backends, whatever they are doing.
func backendsOf(t *testing.T, admin *bun.DB, app string) int {
	t.Helper()
	var n int
	if err := admin.NewRaw("SELECT count(*) FROM pg_stat_activity WHERE application_name = ?", app).Scan(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// waitUntil polls cond until it holds or the bound passes.
func waitUntil(t *testing.T, bound time.Duration, cond func() bool, why func() string) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(why())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
