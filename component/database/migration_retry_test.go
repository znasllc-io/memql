package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/core/common"
)

// migration_retry_test.go -- znasllc-io/memql#3879.
//
// WHAT HAPPENED. A fresh local cluster came up with every pod Running and
// Ready, and every write failing:
//
//	relation "MemoryNodes" does not exist (SQLSTATE=42P01)
//
// The database had ZERO application tables. Sign-in failed at
// `/register` with a 500 ("could not persist client"), node bootstrap
// lookups failed, and the topology reconciler logged the same error on a
// three-second loop -- while the cluster reported itself healthy.
//
// The node's own startup log has the whole story:
//
//	05:55:36  identity starts
//	05:55:36  ERROR "failed to initialize migrations"  (connection refused)
//	05:55:40  "startup ping failed; monitoring will retry"
//	05:55:40  "started"                                 <- comes up anyway
//	          restarts=0                                <- never tried again
//
// Postgres was still Pending on its PVC (the local-path helper pod spent ~98s
// pulling busybox on a slow link -- the same condition behind memql#3877 and
// memql#3873). The nodes came up around it and migrated nothing.
//
// WHY IT NEVER RECOVERED, which is the part worth keeping. Migrations ARE
// re-run after a reconnect -- tryPing's reconnect branch calls runMigrations,
// and runMigrations' own comment says it is written to be re-run. But a pool
// that starts working is not a reconnect. pgdriver heals itself, so the next
// tick PINGS SUCCESSFULLY and returns early, above the reconnect branch. The
// connection is healthy from then on; the schema was never created; nothing
// ever tries again.
//
// So the retry belonged on connectivity RETURNING rather than on the narrower
// event of a connection being re-established, and "no recorded error" had to
// stop meaning "migrated" -- a first attempt that dies at connect time records
// nothing at all.

// countingMigrator records how many times it ran and can be told to start
// failing or succeeding, standing in for a database that is down and then up.
type countingMigrator struct {
	calls atomic.Int32
	err   atomic.Pointer[error]
}

func (m *countingMigrator) Migrate(context.Context, *bun.DB) error {
	m.calls.Add(1)
	if p := m.err.Load(); p != nil {
		return *p
	}
	return nil
}

func (m *countingMigrator) fail(err error) { m.err.Store(&err) }
func (m *countingMigrator) succeed()       { m.err.Store(nil) }

func newRetryTestDB(t *testing.T, m Migrator) *Database {
	t.Helper()
	cfg := &Database{}
	d, err := NewDatabase(common.ComponentName("test-migrate-retry"),
		WithDSN("postgres://localhost:5432/test?sslmode=disable"),
		cfg.WithMigrateOnStart(true),
		cfg.WithMigrator(m),
	)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	// Same carve-out as migration_error_test.go: the default empty bun
	// migration set would need a live connection for Init().
	d.config.migrations = nil
	return d
}

// THE REGRESSION, stated as the property that was false.
//
// After an attempt that FAILED because the database was unreachable,
// migrations must still be pending -- so that the next time connectivity is
// confirmed, they run. Before the fix, nothing consulted this: the only
// re-run lived behind a reconnect that a self-healing pool never performs.
func TestMigrationsStayPendingAfterAFailedAttempt(t *testing.T) {
	m := &countingMigrator{}
	m.fail(errors.New("dial tcp 10.43.68.86:5432: connect: connection refused"))

	d := newRetryTestDB(t, m)
	d.runMigrations(context.Background(), newStubBun())

	if d.MigrationError() == nil {
		t.Fatal("a failed migration recorded no error")
	}
	if !d.migrationsPending() {
		t.Fatal("migrations are not pending after a failed attempt -- nothing will ever retry them, " +
			"which is how a database stays empty forever behind a healthy-looking node (memql#3879)")
	}
}

// The other half, and the one that makes the retry safe to put on a hot path:
// once an attempt completes cleanly, migrations must STOP being pending, so
// tryPing does not re-run them on every tick forever.
func TestMigrationsStopBeingPendingOnceTheySucceed(t *testing.T) {
	m := &countingMigrator{}
	d := newRetryTestDB(t, m)

	d.runMigrations(context.Background(), newStubBun())

	if err := d.MigrationError(); err != nil {
		t.Fatalf("clean migration recorded an error: %v", err)
	}
	if d.migrationsPending() {
		t.Error("migrations still pending after a successful run -- the retry would fire on every " +
			"ping for the lifetime of the process")
	}
	if got := m.calls.Load(); got != 1 {
		t.Errorf("migrator ran %d times, want 1", got)
	}
}

// THE COLD-START RACE, end to end, as the field hit it: the first attempt
// fails because the database is not up, the database then comes up, and the
// node must migrate WITHOUT being restarted.
//
// Before the fix the second attempt never happened, because the only re-run
// was behind a reconnect and the pool had healed itself instead.
func TestMigrationsRunOnceTheDatabaseComesUp(t *testing.T) {
	m := &countingMigrator{}
	m.fail(errors.New("dial tcp: connect: connection refused"))

	d := newRetryTestDB(t, m)

	// Boot: the database is down.
	d.runMigrations(context.Background(), newStubBun())
	if d.MigrationError() == nil {
		t.Fatal("first attempt should have failed")
	}
	if !d.migrationsPending() {
		t.Fatal("migrations must still be pending while the database is unreachable")
	}

	// The database comes up. Something confirms connectivity -- in production
	// that is tryPing's successful ping, which now consults migrationsPending
	// instead of returning early.
	m.succeed()
	if d.migrationsPending() {
		d.runMigrations(context.Background(), newStubBun())
	}

	if err := d.MigrationError(); err != nil {
		t.Fatalf("retry after the database came up still failed: %v", err)
	}
	if d.migrationsPending() {
		t.Error("migrations still pending after a successful retry")
	}
	if got := m.calls.Load(); got != 2 {
		t.Errorf("migrator ran %d times, want 2 (the failed boot attempt, then the retry)", got)
	}
}

// A NEVER-ATTEMPTED migration must also count as pending. This is the case
// "no recorded error" quietly gets wrong: runMigrations with a nil bun logs
// "bun not initialized; skipping migrations" and returns having recorded
// nothing, so a check keyed only on MigrationError() would read that silence
// as success and never retry.
func TestMigrationsArePendingWhenNothingHasBeenAttempted(t *testing.T) {
	d := newRetryTestDB(t, &countingMigrator{})

	if d.MigrationError() != nil {
		t.Fatal("no attempt has been made; there should be no error")
	}
	if !d.migrationsPending() {
		t.Error("migrations are not pending before any attempt -- 'no error yet' is not 'migrated', " +
			"and conflating them is what leaves a node that never reached the database unmigrated forever")
	}
}

// And the switch still switches: with migrateOnStart off, nothing is pending
// and the retry must never fire.
func TestMigrationsAreNeverPendingWhenMigrateOnStartIsOff(t *testing.T) {
	cfg := &Database{}
	d, err := NewDatabase(common.ComponentName("test-migrate-off"),
		WithDSN("postgres://localhost:5432/test?sslmode=disable"),
		cfg.WithMigrateOnStart(false),
		cfg.WithMigrator(&countingMigrator{}),
	)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}
	d.config.migrations = nil

	if d.migrationsPending() {
		t.Error("migrations reported pending with migrateOnStart=false")
	}
}

// pingableConnector yields a connection whose Ping SUCCEEDS, which is the
// state the cold-start race leaves behind: pgdriver's pool has healed, the
// database is reachable, and nothing is wrong with connectivity at all.
type pingableConnector struct{}

func (pingableConnector) Connect(context.Context) (driver.Conn, error) { return &pingableConn{}, nil }
func (pingableConnector) Driver() driver.Driver                        { return pingableDriver{} }

type pingableDriver struct{}

func (pingableDriver) Open(string) (driver.Conn, error) { return &pingableConn{}, nil }

type pingableConn struct{}

func (*pingableConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*pingableConn) Close() error                        { return nil }
func (*pingableConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (*pingableConn) Ping(context.Context) error          { return nil }

// THE WIRING, asserted against the real function.
//
// The two tests above prove migrationsPending has the right semantics. This one
// proves tryPing CONSULTS it -- because a correct helper that nothing calls is
// the specific failure this repository keeps rediscovering (a check that was
// really a display, memql#3817), and it is exactly what the bug was: a re-run
// path that existed, was correct, and sat behind a branch the cold-start race
// never reaches.
//
// The ping succeeds here, so control returns at the top of tryPing -- above the
// reconnect branch. Before the fix that is an unconditional `return true` and
// the migrator is never called again.
func TestTryPingRunsPendingMigrationsWhenTheDatabaseIsReachable(t *testing.T) {
	m := &countingMigrator{}
	m.fail(errors.New("dial tcp: connect: connection refused"))

	d := newRetryTestDB(t, m)

	// Boot with the database down: the attempt fails and stays pending.
	d.runMigrations(context.Background(), newStubBun())
	if !d.migrationsPending() {
		t.Fatal("migrations should be pending after a failed boot attempt")
	}
	before := m.calls.Load()

	// The database is now up: a healthy pool whose ping succeeds.
	d.Lock()
	d.DB = sql.OpenDB(pingableConnector{})
	d.Bun = newStubBun()
	d.Unlock()
	m.succeed()

	if ok := d.tryPing(context.Background()); !ok {
		t.Fatal("tryPing reported failure against a connection that pings cleanly")
	}

	if got := m.calls.Load(); got <= before {
		t.Fatalf("tryPing did not re-run the pending migrations (migrator calls %d -> %d). "+
			"A successful ping returns above the reconnect branch, so the reconnect path's "+
			"re-run is unreachable in the cold-start race -- which is memql#3879.", before, got)
	}
	if d.migrationsPending() {
		t.Error("migrations still pending after tryPing ran them successfully")
	}
}
