package dbtest

// ensure_schema_lock_db_test.go -- EnsureSchema WAITS OUT bun's migration lock
// instead of failing the package on it.
//
// The flake this pins (db-tests default-1, CI run 36379714840): a package's
// TestMain died in 0.1s with "migration lock: migrate: migrations table is
// already locked (duplicate key ... bun_migration_locks_table_name_key)", no
// test having run. EnsureSchema's advisory lock serializes EnsureSchema callers
// and nothing else, while bun's lock row is taken by EVERY Database.Start --
// including the db-gated tests that boot their own memory-nodes database
// mid-run (component/grpc's openWireTestDB, the pack live-e2e suites). bun's
// Lock is a try-lock, so a TestMain that landed inside one of those windows
// read the refusal as a migration failure.
//
// The foreign holder here is a bare bun migrator on the same lock row, which is
// exactly what a sibling's Database.Start is for the moment it holds it. Every
// case runs on a scratch database of its own: holding the SHARED database's
// lock row would stall every other package in the lane.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"

	"github.com/znasllc-io/memql/component/database"
)

// lockRaceScratchDatabase creates an empty database from template0 beside the
// one DSN() names, and drops it when the test ends. template0 carries no
// extensions: the migrations create what they need, as on a fresh install. It
// needs CREATEDB, which the db-tests lane's service user has.
func lockRaceScratchDatabase(t *testing.T) string {
	t.Helper()
	adminDSN := DSN()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(adminDSN))), pgdialect.New())
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.PingContext(ping); err != nil {
		_ = admin.Close()
		Unreachable(t, "EnsureSchema under a held migration lock", adminDSN, err)
		return ""
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "memql_dbtest_lock_" + hex.EncodeToString(suffix)
	if _, err := admin.ExecContext(context.Background(), `CREATE DATABASE `+name+` TEMPLATE template0`); err != nil {
		_ = admin.Close()
		t.Fatalf("create a scratch database: %v", err)
	}
	t.Cleanup(func() {
		// Housekeeping, not an assertion: a drop that does not land is named
		// so it can be done by hand.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Logf("could not drop the scratch database %s (drop it by hand): %v", name, err)
		}
		_ = admin.Close()
	})
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse %s: %v", SafeDSN(adminDSN), err)
	}
	u.Path = "/" + name
	return u.String()
}

// holdMigrationLock takes bun's migration lock row on dsn the way a sibling
// process's Database.Start does, and returns the migrator that holds it.
func holdMigrationLock(t *testing.T, dsn string) (*bun.DB, *migrate.Migrator) {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	holder := migrate.NewMigrator(db, migrate.NewMigrations())
	ctx := context.Background()
	if err := holder.Init(ctx); err != nil {
		t.Fatalf("create bun's migration tables: %v", err)
	}
	if err := holder.Lock(ctx); err != nil {
		t.Fatalf("take bun's migration lock row: %v", err)
	}
	return db, holder
}

// TestEnsureSchema_ConcurrentCallersWaitOutAForeignMigrationLock is the lane in
// miniature: several TestMains reach EnsureSchema together while a migrator
// outside the advisory lock holds bun's row. The callers are goroutines, but
// each opens its own sessions, so the advisory lock serializes them exactly as
// it serializes processes. Every caller must return cleanly, none before the
// row is released, and the schema must be migrated exactly once -- the first
// caller migrates, the rest observe.
func TestEnsureSchema_ConcurrentCallersWaitOutAForeignMigrationLock(t *testing.T) {
	// Before the sandbox, so an unreachable server honours the ambient
	// MEMQL_REQUIRE_DB.
	scratch := lockRaceScratchDatabase(t)
	dsnEnvSandbox(t)
	os.Setenv(dsnEnv, scratch)

	db, holder := holdMigrationLock(t, scratch)

	const callers = 4
	const holdFor = 2 * time.Second
	type release struct {
		// startedAt is taken BEFORE the DELETE, so no caller can have
		// migrated legitimately before it.
		startedAt time.Time
		err       error
	}
	released := make(chan release, 1)
	go func() {
		time.Sleep(holdFor)
		at := time.Now()
		released <- release{startedAt: at, err: holder.Unlock(context.Background())}
	}()

	type result struct {
		reachable bool
		err       error
		returned  time.Time
	}
	results := make([]result, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// The fallback is unused: the env names the scratch database.
			reachable, err := ensureSchema(context.Background(), defaultDSN)
			results[i] = result{reachable: reachable, err: err, returned: time.Now()}
		}()
	}
	close(start)
	wg.Wait()
	rel := <-released
	if rel.err != nil {
		t.Fatalf("release the foreign migration lock: %v", rel.err)
	}

	for i, r := range results {
		if r.err != nil {
			t.Errorf("caller %d: EnsureSchema failed on a migration lock another process held "+
				"briefly -- it must wait the lock out, not fail the package: %v", i, r.err)
			continue
		}
		if !r.reachable {
			t.Errorf("caller %d: reachable=false against a database that answered", i)
		}
		if r.returned.Before(rel.startedAt) {
			t.Errorf("caller %d returned %v BEFORE the foreign lock was released -- it migrated "+
				"past a lock row it did not own", i, rel.startedAt.Sub(r.returned))
		}
	}
	if t.Failed() {
		return
	}

	ctx := context.Background()
	var rows, names, groups int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*), count(DISTINCT name), count(DISTINCT group_id) FROM bun_migrations`).
		Scan(&rows, &names, &groups); err != nil {
		t.Fatalf("read bun_migrations: %v", err)
	}
	if rows == 0 {
		t.Fatal("bun_migrations is empty: no caller migrated the scratch database")
	}
	if rows != names {
		t.Errorf("bun_migrations holds %d rows for %d migrations: a migration was applied more "+
			"than once, so two callers migrated concurrently", rows, names)
	}
	if groups != 1 {
		t.Errorf("bun_migrations spans %d migration groups, want 1: the first caller must apply "+
			"every migration and the others must find nothing left to apply", groups)
	}
	var locks int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM bun_migration_locks`).Scan(&locks); err != nil {
		t.Fatalf("read bun_migration_locks: %v", err)
	}
	if locks != 0 {
		t.Errorf("%d migration lock row(s) left behind: a caller stranded the lock", locks)
	}
}

// TestEnsureSchema_StrandedMigrationLockIsABoundedError pins the other half of
// waiting: a lock row nobody releases -- one component/database keeps on
// purpose after a migration's connection died mid-flight -- must end in an
// error that says so, within the bound, rather than a TestMain that hangs until
// the lane's timeout names nothing.
func TestEnsureSchema_StrandedMigrationLockIsABoundedError(t *testing.T) {
	scratch := lockRaceScratchDatabase(t)
	dsnEnvSandbox(t)
	os.Setenv(dsnEnv, scratch)

	holdMigrationLock(t, scratch) // never released; the database is dropped at cleanup

	const bound = time.Second
	began := time.Now()
	reachable, err := ensureSchemaWith(context.Background(), defaultDSN, bound)
	took := time.Since(began)

	if err == nil {
		t.Fatal("EnsureSchema returned nil past a migration lock row that was never released")
	}
	if !reachable {
		t.Errorf("reachable=false against a database that answered")
	}
	if !errors.Is(err, database.ErrMigrationLockHeld) {
		t.Errorf("the error must still identify the held lock (errors.Is ErrMigrationLockHeld): %v", err)
	}
	if !strings.Contains(err.Error(), "DELETE FROM bun_migration_locks") {
		t.Errorf("the error must name the remedy for a stranded row: %v", err)
	}
	// Generous against a slow machine: one migrateOnce is a full Database
	// start. What matters is that the wait ended near the bound, not at
	// migrationLockWait or the lane's timeout.
	if took > bound+30*time.Second {
		t.Errorf("gave up after %v with a %v bound -- the wait is not bounded by lockWait", took, bound)
	}
}
