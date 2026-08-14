package database

// search_path.go -- the environment boundary (memql#3765, epic #3748 §3.1).
//
// memQL has no tenancy dimension in the actor envelope. That is a recorded
// finding rather than an oversight (memql#3321), and the partition dimension
// that once did this job was deliberately retired (#56), so every
// account-scoped filter compares against a caller-supplied argument. An
// environment boundary therefore cannot be a filter: there is nothing in
// `actor.*` to filter on, and one missed conjunct leaks production data.
//
// It also cannot be "shared data, separate artifacts". Exercising a staging SPA
// performs MUTATIONS -- automations fire, agents write -- and those land
// wherever the connection points.
//
// So the boundary is THE CONNECTION, which is the one place no application code
// path can forget. Each namespace's secret carries the same DSN with a
// different MEMQL_DB_SEARCH_PATH, and every backend the driver opens gets it.
//
// # `public` is left empty, and that is load-bearing
//
// An unset or mistyped search path falls back to `"$user", public`. If
// production lived in `public`, that slip would silently mean PRODUCTION. With
// both environments in named schemas the same slip resolves to a schema holding
// no application tables and the first statement fails loudly. The safe failure
// is bought by giving production a name too -- which is why parseSearchPath
// REFUSES `public` as the first element rather than trusting the deployment to
// remember.
//
// # Why this is not pgdriver.WithConnParams
//
// The property that mechanism has -- applied to every backend the driver opens,
// reconnects included -- is exactly what is required here, and it is why the
// design named it. But it cannot carry the VALUE. pgdriver applies each param
// as a PARAMETERIZED statement (`SET %s TO $1`, driver.go:151), and a bound
// parameter is one string, so Postgres stores one schema name. Measured against
// the pinned pgdriver v1.2.18:
//
//	WithConnParams{"search_path": "memql_staging"}
//	  -> effective `memql_staging`                    a bare table name resolves
//	WithConnParams{"search_path": "memql_staging, public"}
//	  -> effective `"memql_staging, public"`          nothing resolves at all
//
// The DSN route is the same code path -- pgdriver turns every unrecognised
// query parameter into a ConnParam (config.go:412) -- so `?options=-c
// search_path=...` fails identically.
//
// This file keeps the property and drops the call: a connector wrapper issues a
// literal `SET search_path = <list>` on each new connection, at the same seam
// the retrying connector already wraps.
//
// # `public` STAYS on the path, and the migrate job checks that it worked
//
// The TimescaleDB extension's functions live in `public`, so a path of
// `memql_<env>` alone cannot resolve them:
//
//	SET search_path = memql_staging;
//	SELECT create_hypertable('ts_probe'::regclass, 'created_at');
//	ERROR:  function create_hypertable(regclass, unknown) does not exist
//
//	SET search_path = memql_staging, public;
//	SELECT create_hypertable('ts_probe'::regclass, 'created_at');
//	 (1,memql_staging,ts_probe,t)          <- lands in the NAMED schema
//
// So the operator's value is `memql_<env>, public`, and the safety margin above
// is untouched: `public` holds the extension and no application tables.
//
// The requirement is not enforced by hard-coding `public` -- where an extension
// lives is a property of the database, not of this code. It is enforced by
// CHECKING, in ensureSearchPathSchema, that the extension is actually reachable
// on the configured path, so a wrong path fails at the migrate job with a
// message naming the path instead of as `function create_hypertable does not
// exist` forty migrations later.
//
// # One caveat, inherited rather than introduced
//
// This is SESSION state, so a transaction-mode connection pooler in front of
// Postgres would not preserve it. That is equally true of WithConnParams -- the
// tree already routes the session-scoped advisory-lock holder through
// DIRECT_DSN for exactly this reason (memql#1930) -- so it is a property of the
// deployment rather than of this file.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/uptrace/bun"
)

// envDBSearchPath names the per-environment schema search path. Unset means no
// search path is applied at all -- the single-environment case, which is every
// local cluster and every deployment predating epic #3748.
const envDBSearchPath = "MEMQL_DB_SEARCH_PATH"

// schemaIdentifier is the shape a schema name must have to be interpolated into
// SQL. `SET` takes no bound parameter for a list value (see the header), so the
// value reaches the statement as text and every element is checked rather than
// trusted. Deliberately narrow -- the names this design mints are `memql_prod`,
// `memql_staging` and `public`.
var schemaIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// searchPathFromEnv returns the configured search path, or "" when unset.
func searchPathFromEnv() string {
	return strings.TrimSpace(os.Getenv(envDBSearchPath))
}

// parseSearchPath splits a configured search path into its schema names.
//
// It REFUSES rather than sanitising. A search path is the environment boundary;
// a value this cannot parse means the operator meant something the engine
// cannot deliver, and quietly dropping the unparseable element is how a node
// comes up pointed at the wrong schema while looking configured.
func parseSearchPath(raw string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if !schemaIdentifier.MatchString(name) {
			return nil, fmt.Errorf("%s: %q is not a bare schema identifier (lower-case letters, digits and underscores)", envDBSearchPath, name)
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is set but names no schema", envDBSearchPath)
	}
	// The whole safety margin is that an unset or mistyped path resolves to a
	// schema holding nothing. A deployment whose FIRST element is `public`
	// writes its tables exactly where the fallback points, so the mistyped case
	// stops failing and starts silently meaning this environment.
	if out[0] == "public" {
		return nil, fmt.Errorf("%s: %q may not be the first schema -- an unset or mistyped search path falls back to \"$user\", public, so an environment living there is what a configuration slip silently resolves to. Give it a name (memql_prod, memql_staging) and leave public empty", envDBSearchPath, "public")
	}
	return out, nil
}

// targetSchema returns the schema an unqualified CREATE lands in: the first on
// the path. It is what the migrate job creates before migrating, and where
// every write goes.
func targetSchema(schemas []string) string { return schemas[0] }

// ensureSearchPathSchema creates the environment's schema and verifies the
// configured path can actually resolve what the migrations call.
//
// Both halves run before the first migration, because both failures are
// otherwise discovered mid-migration: `CREATE TABLE IF NOT EXISTS` cannot land
// in a schema that does not exist, and a path missing the extension's home
// fails on whichever migration first calls a TimescaleDB function -- by which
// point the schema is half-built.
func ensureSearchPathSchema(ctx context.Context, db *bun.DB, raw string) error {
	schemas, err := parseSearchPath(raw)
	if err != nil {
		return err
	}
	target := targetSchema(schemas)
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+target); err != nil {
		return fmt.Errorf("creating schema %q: %w", target, err)
	}
	return verifyExtensionsReachable(ctx, db, schemas)
}

// verifyExtensionsReachable checks that every installed extension's schema is
// on the configured path.
//
// Derived from pg_extension rather than assumed: where an extension lives is a
// property of the database. The check exists because the failure it replaces is
// so unhelpful -- a path of `memql_staging` alone yields `function
// create_hypertable(regclass, unknown) does not exist`, which names neither the
// search path nor the fix.
func verifyExtensionsReachable(ctx context.Context, db *bun.DB, schemas []string) error {
	onPath := make(map[string]bool, len(schemas)+1)
	for _, s := range schemas {
		onPath[s] = true
	}
	// pg_catalog is implicitly on every search path, so an extension living
	// there (plpgsql) is always reachable and is not a finding.
	onPath["pg_catalog"] = true

	var missing []string
	rows, err := db.QueryContext(ctx, `
		SELECT e.extname, n.nspname
		FROM pg_extension e
		JOIN pg_namespace n ON n.oid = e.extnamespace
	`)
	if err != nil {
		return fmt.Errorf("reading installed extensions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var extName, schema string
		if err := rows.Scan(&extName, &schema); err != nil {
			return fmt.Errorf("reading installed extensions: %w", err)
		}
		if !onPath[schema] {
			missing = append(missing, fmt.Sprintf("%s (in %s)", extName, schema))
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading installed extensions: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"%s=%q does not reach %s; the migrations call those extensions' functions by bare name, so add each schema to the path (the usual value is \"<environment schema>, public\")",
			envDBSearchPath, strings.Join(schemas, ", "), strings.Join(missing, ", "))
	}
	return nil
}

// searchPathConnector applies the configured search path to every connection
// the wrapped connector opens.
type searchPathConnector struct {
	base driver.Connector
	stmt string
}

// newSearchPathConnector wraps base so each new connection is put on `schemas`.
// A nil base, or an empty schema list, returns base untouched so the
// single-environment case composes exactly as before.
func newSearchPathConnector(base driver.Connector, schemas []string) driver.Connector {
	if base == nil || len(schemas) == 0 {
		return base
	}
	return &searchPathConnector{
		base: base,
		stmt: "SET search_path = " + strings.Join(schemas, ", "),
	}
}

func (c *searchPathConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	execer, ok := conn.(driver.ExecerContext)
	if !ok {
		// REFUSE the connection rather than hand back one on the default path.
		// A connection whose search path was never set is the exact failure
		// this file exists to prevent, and it would present as production data
		// appearing in staging rather than as an error.
		_ = conn.Close()
		return nil, fmt.Errorf("database: driver connection does not support ExecerContext, so %s cannot be applied", envDBSearchPath)
	}
	if _, err := execer.ExecContext(ctx, c.stmt, nil); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("database: applying %s: %w", envDBSearchPath, err)
	}
	return conn, nil
}

func (c *searchPathConnector) Driver() driver.Driver { return c.base.Driver() }
