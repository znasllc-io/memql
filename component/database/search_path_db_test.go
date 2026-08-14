package database_test

// search_path_db_test.go -- the isolation premise of epic #3748, against a real
// database because it is the entire justification for the design (memql#3765).
//
// The claim is that the environment boundary is THE CONNECTION. If the search
// path were ever ignored -- a driver that dropped it, a wrapper that forgot it
// on reconnect, a value that silently degraded to one schema name -- staging
// would read and write production data while every test that mocks the database
// kept passing. So these do not mock it.
//
// They drive the SHIPPED path: MEMQL_DB_SEARCH_PATH through NewDatabase, the
// same call app bootstrap makes. Calling the connector wrapper directly would
// test a constructor; this tests the wiring, which is where a boundary actually
// gets lost.
//
// Per the house rule these run against a throwaway TimescaleDB container, never
// a bootstrapped cluster database.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/core/common"
)

const (
	envSearchPath = "MEMQL_DB_SEARCH_PATH" // the operator-facing name; spelled out because this tests that contract
	probeTable    = "search_path_probe"
)

// openOn boots a Database the way app bootstrap does, with the environment's
// search path configured, and returns its bun handle.
//
// Migrations are OFF: what is under test is the boundary, and a full migration
// per schema would make each case minutes long. The migration path has its own
// case below.
func openOn(t *testing.T, searchPath string) *bun.DB {
	t.Helper()
	t.Setenv(envSearchPath, searchPath)

	db, err := database.NewDatabase(common.ComponentName("searchPathTest"),
		database.WithDSN(dbtest.DSN()),
		(&database.Database{}).WithMigrateOnStart(false),
	)
	if err != nil {
		t.Fatalf("NewDatabase: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db.Start(ctx)

	waitCtx, waitCancel := context.WithTimeout(ctx, 15*time.Second)
	defer waitCancel()
	select {
	case <-db.Ready():
	case <-waitCtx.Done():
		dbtest.Unreachable(t, "DB-gated test for the environment search-path boundary", dbtest.SafeDSN(dbtest.DSN()), waitCtx.Err())
	}

	bunDB := db.BunDB()
	if bunDB == nil {
		dbtest.Unreachable(t, "DB-gated test for the environment search-path boundary", dbtest.SafeDSN(dbtest.DSN()), fmt.Errorf("no bun handle after Ready"))
	}
	if err := bunDB.PingContext(waitCtx); err != nil {
		dbtest.Unreachable(t, "DB-gated test for the environment search-path boundary", dbtest.SafeDSN(dbtest.DSN()), err)
	}
	return bunDB
}

// seedTwoEnvironments creates both schemas and puts a table of the SAME name in
// each, carrying a different row. Same name on purpose: what is under test is
// that the connection alone decides which one a bare name resolves to.
func seedTwoEnvironments(t *testing.T, staging, prod string) {
	t.Helper()
	admin := openOn(t, "") // no search path -- every statement below is qualified
	ctx := context.Background()

	for schema, marker := range map[string]string{staging: "staging-row", prod: "prod-row"} {
		for _, stmt := range []string{
			fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, schema),
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s (id text PRIMARY KEY)`, schema, probeTable),
			fmt.Sprintf(`DELETE FROM %s.%s`, schema, probeTable),
			fmt.Sprintf(`INSERT INTO %s.%s (id) VALUES ('%s')`, schema, probeTable, marker),
		} {
			if _, err := admin.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("seed %q: %v", stmt, err)
			}
		}
	}
	t.Cleanup(func() {
		for _, schema := range []string{staging, prod} {
			_, _ = admin.ExecContext(context.Background(), fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schema))
		}
	})
}

// TestSearchPathIsolatesEnvironments is the premise. A row written under one
// environment's search path is INVISIBLE under the other's, in both directions,
// with no filter anywhere in the query -- which is the whole point, since memQL
// has no tenancy dimension to filter on.
func TestSearchPathIsolatesEnvironments(t *testing.T) {
	const staging, prod = "memql_sp_staging", "memql_sp_prod"
	seedTwoEnvironments(t, staging, prod)

	read := func(searchPath string) string {
		db := openOn(t, searchPath)
		var id string
		if err := db.QueryRowContext(context.Background(), `SELECT id FROM `+probeTable).Scan(&id); err != nil {
			t.Fatalf("read on %q: %v", searchPath, err)
		}
		return id
	}

	if got := read(staging + ", public"); got != "staging-row" {
		t.Errorf("a connection on the staging path read %q; the boundary is not holding", got)
	}
	if got := read(prod + ", public"); got != "prod-row" {
		t.Errorf("a connection on the prod path read %q; the boundary is not holding", got)
	}
}

// TestSearchPathAppliesToEveryConnection is the property that makes this a
// boundary rather than a convention. The pool opens many backends and recycles
// them; every one must land on the configured path, or an environment leaks on
// whichever connection happened to be new.
func TestSearchPathAppliesToEveryConnection(t *testing.T) {
	const staging, prod = "memql_sp_every_staging", "memql_sp_every_prod"
	seedTwoEnvironments(t, staging, prod)

	db := openOn(t, staging+", public")
	db.SetMaxIdleConns(0) // force a fresh backend per query

	for i := 0; i < 8; i++ {
		var id string
		if err := db.QueryRowContext(context.Background(), `SELECT id FROM `+probeTable).Scan(&id); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
		if id != "staging-row" {
			t.Fatalf("query %d landed on %q -- a connection came up without the search path", i, id)
		}
	}
}

// TestUnsetSearchPathReadsNeitherEnvironment is the safety margin, and the
// reason production gets a NAME instead of living in public. With no search
// path the fallback is `"$user", public`, and public holds no application
// tables -- so the read FAILS rather than quietly resolving to an environment.
func TestUnsetSearchPathReadsNeitherEnvironment(t *testing.T) {
	const staging, prod = "memql_sp_unset_staging", "memql_sp_unset_prod"
	seedTwoEnvironments(t, staging, prod)

	db := openOn(t, "")
	var id string
	err := db.QueryRowContext(context.Background(), `SELECT id FROM `+probeTable).Scan(&id)
	if err == nil {
		t.Fatalf("an unset search path resolved %q -- the mistyped-configuration case now silently means an environment", id)
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("expected a loud missing-relation failure, got %v", err)
	}
}

// TestMigrateRefusesAPathThatCannotReachTheExtensions is the check that turns
// the worst failure in this design into a stated one.
//
// A path of `memql_<env>` alone cannot resolve create_hypertable, because the
// TimescaleDB extension's functions live in public. Without this the boot gets
// as far as whichever migration first calls one and fails with `function
// create_hypertable(regclass, unknown) does not exist` -- naming neither the
// search path nor the fix, with the schema half-built.
func TestMigrateRefusesAPathThatCannotReachTheExtensions(t *testing.T) {
	const env = "memql_sp_noext"

	admin := openOn(t, "")
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+env+` CASCADE`)
	})

	// No `public`: the extension becomes unreachable, which is the operator
	// error this is here to name.
	t.Setenv(envSearchPath, env)
	db, err := database.NewTimescaleDBDatabase(common.ComponentName("searchPathNoExtTest"),
		database.WithDSN(dbtest.DSN()),
	)
	if err != nil {
		t.Fatalf("NewTimescaleDBDatabase: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db.Start(ctx)

	waitCtx, waitCancel := context.WithTimeout(ctx, 60*time.Second)
	defer waitCancel()
	select {
	case <-db.Ready():
	case <-waitCtx.Done():
		t.Fatalf("database did not become ready: %v", waitCtx.Err())
	}

	migErr := db.MigrationError()
	if migErr == nil {
		t.Fatal("a search path that cannot reach the extensions was accepted; the migrations would fail later, obscurely")
	}
	if !strings.Contains(migErr.Error(), envSearchPath) {
		t.Errorf("the refusal must name the search path so an operator knows what to change: %v", migErr)
	}
}

// TestMigrationsLandInTheEnvironmentSchema covers the migrate-job half: the
// schema is created before anything migrates into it, the migrations land
// THERE, and the other environment is left untouched. Hypertables included --
// TimescaleDB resolves the table through the search path, and the extension's
// functions through `public`, which is why public stays on it.
//
// This is the slow case (a full migration), so it runs once against one schema
// rather than per assertion.
func TestMigrationsLandInTheEnvironmentSchema(t *testing.T) {
	const env, other = "memql_sp_migrate", "memql_sp_migrate_other"

	admin := openOn(t, "")
	ctx := context.Background()
	for _, schema := range []string{env, other} {
		if _, err := admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Fatalf("clean %s: %v", schema, err)
		}
	}
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+other); err != nil {
		t.Fatalf("create %s: %v", other, err)
	}
	t.Cleanup(func() {
		for _, schema := range []string{env, other} {
			_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		}
	})

	// Boot WITH migrations, on a search path naming a schema that does not yet
	// exist -- the fresh-environment case the migrate job faces.
	t.Setenv(envSearchPath, env+", public")
	db, err := database.NewTimescaleDBDatabase(common.ComponentName("searchPathMigrateTest"),
		database.WithDSN(dbtest.DSN()),
	)
	if err != nil {
		t.Fatalf("NewTimescaleDBDatabase: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db.Start(runCtx)

	waitCtx, waitCancel := context.WithTimeout(runCtx, 120*time.Second)
	defer waitCancel()
	select {
	case <-db.Ready():
	case <-waitCtx.Done():
		t.Fatalf("database did not become ready: %v", waitCtx.Err())
	}
	if err := db.MigrationError(); err != nil {
		t.Fatalf("migrations failed on a named schema: %v", err)
	}

	var landed bool
	if err := admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = ? AND table_name = 'MemoryNodes')`,
		env).Scan(&landed); err != nil {
		t.Fatalf("checking %s: %v", env, err)
	}
	if !landed {
		t.Errorf("migrations did not land in %s; the search path is not reaching the migrate path", env)
	}

	var leaked bool
	if err := admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = ? AND table_name = 'MemoryNodes')`,
		other).Scan(&leaked); err != nil {
		t.Fatalf("checking %s: %v", other, err)
	}
	if leaked {
		t.Errorf("migrating one environment created tables in %s as well", other)
	}

	// The hypertable is the case the design singled out, and the one that fails
	// if `public` is dropped from the path -- create_hypertable resolves the
	// TABLE through the search path but is itself a function that has to be
	// reachable on it.
	//
	// It is asserted for the ENV schema specifically. A schema-blind query
	// would find the shared public one this lane's TestMain migrated and pass
	// while proving nothing.
	var hyperHere bool
	if err := admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM timescaledb_information.hypertables
		                WHERE hypertable_name = 'MemoryNodes' AND hypertable_schema = ?)`,
		env).Scan(&hyperHere); err != nil {
		t.Fatalf("checking the hypertable in %s: %v", env, err)
	}
	if !hyperHere {
		t.Errorf("MemoryNodes is not a hypertable in %s. create_hypertable resolves the TABLE through the search "+
			"path and is itself a function that has to be reachable ON it, so this is what fails first if public "+
			"is dropped from the path", env)
	}
}
