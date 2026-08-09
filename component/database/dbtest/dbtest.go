// Package dbtest provides shared setup for the Postgres-gated
// ("*_db_test.go") suites that CI runs together in the db-tests lane
// (.github/workflows/ci.yml) against ONE shared database.
//
// Those suites compile into separate per-package test binaries that `go
// test` runs as parallel processes, all pointed at the same
// MEMQL_DATABASE_DSN. Only one of them (examples/referencepack) ever migrated
// the blank service DB; the others (component/memql, component/automations/
// steps) opened a connection and read/wrote the MemoryNodes table assuming
// the schema already existed. With no coordination the two classes raced:
// when a reader reached its first DB op before referencepack's migration
// finished, it failed with `relation "MemoryNodes" does not exist`, or -- if
// the initial CREATE had landed but a later migration had not -- a
// half-applied `column ... does not exist`. That intermittently red the
// required db-tests lane and poisoned the merge queue (memql#2551).
//
// EnsureSchema closes the gap: every db-gated package calls it from TestMain,
// so the schema is fully migrated before any test in any package touches it.
// A blocking, cross-process advisory lock serializes the migration so the
// first process migrates while the rest wait, then observe the applied
// migrations and return -- the non-idempotent (column-rename) migrations can
// never run concurrently.
package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// dsnEnv is the environment variable every db-gated suite reads to find the
// shared test database. Named rather than repeated as a literal so the read,
// the (post-ping) write, and the tests that assert on it cannot drift.
const dsnEnv = "MEMQL_DATABASE_DSN"

// defaultDSN mirrors the fallback each *_db_test.go uses when
// MEMQL_DATABASE_DSN is unset, so the ensure step and the tests it guards
// target the same database.
const defaultDSN = "postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable"

// schemaLockKey is a dedicated, fixed 64-bit advisory-lock id that serializes
// schema migration across the parallel db-gated test processes. Distinct from
// the cron-leader / reconciler lock ids so the leases are independent
// (memql#2551).
const schemaLockKey int64 = 7756010113207025510

// EnsureSchema migrates the shared test database to the current schema,
// serialized across processes so the parallel db-gated package test binaries
// never race on a half-created schema (memql#2551). It is idempotent: the
// first process to acquire the advisory lock migrates; every other process
// blocks on the lock, then observes the applied migrations and returns.
//
// It reports reachable=false (err=nil) when no Postgres is reachable, so the
// caller can proceed to m.Run() and let the individual tests self-skip --
// preserving the green-by-skip behaviour on a DB-less CI lane.
//
// It never publishes a DSN it has not connected to. See ensureSchema.
func EnsureSchema(ctx context.Context) (reachable bool, err error) {
	return ensureSchema(ctx, defaultDSN)
}

// ensureSchema is EnsureSchema with the fallback DSN injected, so the
// unreachable-default behaviour is testable without a database and without
// mutating a package-level const (memql#3096, #3148).
func ensureSchema(ctx context.Context, fallbackDSN string) (reachable bool, err error) {
	dsn := strings.TrimSpace(os.Getenv(dsnEnv))
	// usedDefault distinguishes "the operator named a DSN" from "this helper
	// guessed one". The two deserve different answers on failure, and every
	// decision below turns on it.
	usedDefault := dsn == ""
	if usedDefault {
		dsn = fallbackDSN
	}

	lockDB := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	defer func() { _ = lockDB.Close() }()

	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPing()
	if perr := lockDB.PingContext(pingCtx); perr != nil {
		// No Postgres reachable: leave migration to nobody and let the
		// individual tests skip (they each ping + Skipf). Under
		// MEMQL_REQUIRE_DB the run WANTED a database, so an unreachable
		// one -- including a wrong password, which pings the same way --
		// is an error rather than a silent degrade to green-by-skip
		// (#2680).
		//
		// THE MEMQL_REQUIRE_DB DECISION (memql#3148). The hard error is gated
		// on !usedDefault, and that gate is the whole point of this branch:
		//
		//   - Operator NAMED a DSN and it does not answer -> error. The run
		//     asked for that database; there is nothing else to try and
		//     nothing to be gained by continuing. This is #2680's guarantee
		//     and it is preserved EXACTLY. The db-tests lane always takes
		//     this path -- ci.yml sets MEMQL_DATABASE_DSN at job level -- so
		//     CI's behaviour is unchanged by the gate.
		//
		//   - Operator named nothing and this helper's own fallback does not
		//     answer -> return (false, nil) and let the caller continue.
		//     Every main_dbschema_test.go exits only on err != nil, so the
		//     package reaches m.Run() and each test applies its own DSN
		//     resolution and its own dbtest.Unreachable gate -- which is
		//     itself MEMQL_REQUIRE_DB-aware and still fails loudly, naming
		//     the specific assertion that needed a database. Nothing goes
		//     quiet; the failure just stops being reported by the wrong
		//     component. EnsureSchema's job is to migrate a schema, not to
		//     decide that a run is over on the strength of a fallback string
		//     compiled into this file.
		//
		// The alternative -- erroring on both paths -- was rejected because
		// it is precisely what turned a stale credential in this file's own
		// defaultDSN into "0 tests ran, package FAIL" for every db-gated
		// package (memql#3030, #3096), while each package's own fallback was
		// correct the entire time.
		//
		// Honest limit: once every suite resolves through DSN() (memql#3149),
		// "the package's own fallback" IS this fallback, so falling through
		// yields one Fatalf per db-gated test rather than one os.Exit(1) per
		// package. Both are red. The fall-through names which assertions
		// wanted a database; the exit names only the package. That is the
		// trade, and it is deliberate.
		if RequireDB() && !usedDefault {
			return false, fmt.Errorf("dbtest: %s=1 but Postgres is UNREACHABLE at %s: %w", RequireDBEnv, SafeDSN(dsn), perr)
		}
		if RequireDB() {
			fmt.Fprintf(os.Stderr, "dbtest: %s=1 and no %s was set; the built-in fallback %s is "+
				"UNREACHABLE (%v). Continuing to m.Run() so each test reports its own requirement "+
				"-- %s still turns every one of those into a failure.\n",
				RequireDBEnv, dsnEnv, SafeDSN(dsn), perr, RequireDBEnv)
		}
		return false, nil
	}

	// Only now -- with the DSN proven to answer -- align
	// NewMemoryNodesDatabase (which reads the env) with the DSN the tests fall
	// back to, so both target the same database. Publishing before the ping
	// meant an unreachable fallback overwrote an empty env for the whole
	// process, and every downstream test that reads the env first inherited
	// this function's failure instead of applying its own fallback
	// (memql#3096).
	if usedDefault {
		_ = os.Setenv(dsnEnv, dsn)
	}

	// pg_advisory_lock is session-scoped, so the lock and its release must run
	// on the same pinned backend. Take a dedicated connection, matching the
	// cron-leader / reconciler advisory-lock idiom.
	conn, err := lockDB.DB.Conn(ctx)
	if err != nil {
		return true, fmt.Errorf("dbtest: acquire lock connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// BLOCKING lock: a sibling process that reaches here while another is
	// migrating waits rather than proceeding on an incomplete schema.
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", schemaLockKey); err != nil {
		return true, fmt.Errorf("dbtest: pg_advisory_lock: %w", err)
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = conn.ExecContext(uctx, "SELECT pg_advisory_unlock($1)", schemaLockKey)
		cancel()
	}()

	// Migrate via the production lifecycle. Under the advisory lock this is the
	// ONLY migration touching the DB, so the non-idempotent (rename)
	// migrations never run concurrently; a sibling that already applied them
	// leaves bun_migrations recording the fact, and this call is a no-op.
	mnd, err := memoryNodes.NewMemoryNodesDatabase()
	if err != nil {
		return true, fmt.Errorf("dbtest: NewMemoryNodesDatabase: %w", err)
	}
	mnd.Start(ctx)
	select {
	case <-mnd.Ready():
	case <-time.After(90 * time.Second):
		mnd.Stop(context.Background())
		return true, fmt.Errorf("dbtest: shared test database did not become ready within 90s")
	}
	migErr := mnd.MigrationError()
	mnd.Stop(context.Background())
	if migErr != nil {
		return true, fmt.Errorf("dbtest: migrate shared test schema: %w", migErr)
	}

	// Blast-radius guard (memql#3415). Everything above this point is about
	// making the schema correct; this is about making sure it is the right
	// DATABASE. See refuseBootstrappedDatabase.
	stamp, serr := readBootstrapStamp(ctx, lockDB)
	if serr != nil {
		return true, fmt.Errorf("dbtest: read cluster bootstrap state: %w", serr)
	}
	if rerr := refuseBootstrappedDatabase(stamp, dsn); rerr != nil {
		return true, rerr
	}
	return true, nil
}

// AllowBootstrappedDBEnv opts a run back in to a database that belongs to a
// bootstrapped cluster. Deliberately explicit and deliberately separate from
// MEMQL_REQUIRE_DB: "I want a database" and "I accept writing into a live
// cluster's data" are different statements.
const AllowBootstrappedDBEnv = "MEMQL_ALLOW_BOOTSTRAPPED_DB"

// clusterSettingsSingletonId is the one row that decides whether a cluster
// counts as bootstrapped. Its presence WITH a stamp is the sharpest available
// "this database belongs to a running cluster" signal: nothing but a completed
// claim writes it, and no test writes it (the one that used to is fixed).
const clusterSettingsSingletonId = "v1:identity:clusterSettings:cluster"

// refuseBootstrappedDatabase turns "the target database belongs to a
// bootstrapped cluster" into a hard stop for the db-gated suites.
//
// Why this exists (memql#3415). These suites accept whatever
// MEMQL_DATABASE_DSN names, and the fallback compiled into this file is the
// LOCAL k3d CLUSTER'S DATABASE -- so `go test ./component/memql/...` with no
// env set writes production-shaped rows straight into a running cluster's
// identity data. It did: a read-merge test targeting the singleton
// clusterSettings row appended blank-bootstrappedAt versions of it, which
// disabled login for every user on that cluster and made /setup -- the wizard
// that mints the cluster owner -- reachable unauthenticated. The specific write
// is fixed at three layers, but "a db-gated suite may write identity rows into
// a live cluster" is the general shape, and only this check addresses that.
//
// A CI lane's blank service database has no clusterSettings singleton and is
// unaffected. A developer who genuinely means to point a suite at a
// bootstrapped cluster sets AllowBootstrappedDBEnv.
func refuseBootstrappedDatabase(bootstrappedAt, dsn string) error {
	if strings.TrimSpace(bootstrappedAt) == "" {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(AllowBootstrappedDBEnv))) {
	case "", "0", "false", "no":
	default:
		return nil
	}
	return fmt.Errorf(
		"dbtest: REFUSING to run the db-gated suites against %s -- that database belongs to a "+
			"BOOTSTRAPPED cluster (%s was stamped at %s). These suites write real identity, cluster "+
			"and user rows; doing so against a live cluster has already disabled login for every "+
			"user and re-opened the unauthenticated /setup ownership wizard (memql#3415). Point %s "+
			"at a throwaway Postgres, or set %s=1 if you truly mean this database",
		SafeDSN(dsn), clusterSettingsSingletonId, bootstrappedAt, dsnEnv, AllowBootstrappedDBEnv)
}

// readBootstrapStamp returns the latest clusterSettings singleton's
// bootstrappedAt, or "" when the row (or the table) is absent. A missing table
// is not an error here: EnsureSchema is also the path that CREATES it, and an
// older schema is still a database worth running against.
func readBootstrapStamp(ctx context.Context, db *bun.DB) (string, error) {
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT to_regclass('public."MemoryNodes"') IS NOT NULL`).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", nil
	}
	var stamp sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT payload->>'bootstrappedAt'
		   FROM "MemoryNodes"
		  WHERE id = ?
		  ORDER BY "createdAt" DESC
		  LIMIT 1`, clusterSettingsSingletonId).Scan(&stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return stamp.String, nil
}

// --- Reachability reporting (#2680) -----------------------------------------
//
// Every db-gated suite self-skips when Postgres is unreachable, which keeps a
// DB-less lane green. The failure mode that motivated these helpers: a WRONG
// DSN (bad password, wrong port) is also "unreachable", so a run intended as
// db-gated verification silently degrades to green-by-skip and reports `ok`.
// Local verification of #2599/#2600 ran that way for a whole session --
// 17 conformance tests skipped while the run looked green.
//
// Two guards: the skip message names the DSN it actually tried (password
// redacted), so "wrong credentials" is never mistaken for "no database"; and
// MEMQL_REQUIRE_DB=1 converts every such skip into a failure, so a run meant
// to exercise the DB cannot quietly not.

// RequireDBEnv is the opt-in that turns db-gated skips into failures.
const RequireDBEnv = "MEMQL_REQUIRE_DB"

// DSN returns the database DSN the db-gated suites target: the
// MEMQL_DATABASE_DSN environment value, or the shared default.
func DSN() string {
	if dsn := strings.TrimSpace(os.Getenv(dsnEnv)); dsn != "" {
		return dsn
	}
	return defaultDSN
}

// RequireDB reports whether MEMQL_REQUIRE_DB demands a reachable database.
func RequireDB() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(RequireDBEnv))) {
	case "", "0", "false", "no":
		return false
	}
	return true
}

// SafeDSN redacts the password in a DSN so it can appear in test output.
// Returns the input unchanged when it carries no credentials.
func SafeDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	scheme := strings.Index(dsn, "://")
	if scheme < 0 || scheme+3 > at {
		return dsn
	}
	creds := dsn[scheme+3 : at]
	colon := strings.Index(creds, ":")
	if colon < 0 {
		return dsn // user only, no password to hide
	}
	return dsn[:scheme+3] + creds[:colon] + ":***" + dsn[at:]
}

// Unreachable is the single decision point for a db-gated test whose ping
// failed: it FAILS the test when MEMQL_REQUIRE_DB is set, and otherwise skips
// with a message naming the redacted DSN and the underlying error -- so an
// authentication failure reads as one instead of looking like an absent
// database. purpose describes the suite ("accountDeletionSweep acceptance").
func Unreachable(t testing.TB, purpose, dsn string, err error) {
	t.Helper()
	if RequireDB() {
		// Explicit return rather than relying on Fatalf's implicit
		// Goexit: the control flow must be correct for any testing.TB,
		// not just the stdlib one (#2680's own test caught this).
		t.Fatalf("%s: %s=1 but Postgres is UNREACHABLE at %s: %v", purpose, RequireDBEnv, SafeDSN(dsn), err)
		return
	}
	t.Skipf("%s: no Postgres reachable at %s (%v) -- set %s=1 to make this a failure", purpose, SafeDSN(dsn), err, RequireDBEnv)
}
