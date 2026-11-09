package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun/driver/pgdriver"
)

// maintenance_session.go -- ONE dedicated session for maintenance statements
// that legitimately run for minutes. Its first user is the closed-chunk
// compaction (component/retention/compact.go): a TimescaleDB reorder_chunk of
// a multi-gigabyte chunk, and the heap scan that measures what a chunk would
// give back. The second is the website statistics rollup
// (component/sitetraffic/rollup.go), whose recompute of a busy UTC day can
// outlast both limits below.
//
// # Why not a pool the process already has
//
// Every pool here carries a limit that ends such a statement part-way:
//
//   - the request pool's server statement_timeout (8 s by default,
//     sessionConnParams). Maintenance must not inherit it: on 2026-10-10 and
//     2026-10-11 the log sweep's DELETE was cancelled at 8.3 s and 8.9 s, the
//     day it was deleting stayed, and the instance filled its disk
//     (component/logstore/sweep.go);
//   - pgdriver's CLIENT read deadline, 10 s on every connection of every pool
//     here, the migration and gate pools included (latest_row_index.go measured
//     it: pg_sleep(12) fails at 10.0 s with "i/o timeout", and the backend
//     carries on as an orphan the client no longer hears from).
//
// So this session has neither. Its connection parameters set no
// statement_timeout, and the caller SETs one per statement below its own
// deadline, so an overrun is ended by the SERVER, which rolls the statement
// back and says so, rather than by the client cutting the socket. Its read
// deadline is the caller's context and nothing shorter (ReadTimeout 0:
// pgdriver then uses the context's deadline alone). The idle limits stay: a
// session whose process died is still reaped by the server.
//
// # Why the direct endpoint, and why one session opened per use
//
// The caller's statements are session-shaped: SET lock_timeout and
// statement_timeout must hold across statements, a session advisory lock keeps
// two replicas from compacting at once, and reorder_chunk refuses to run inside
// a transaction block. A transaction-mode pooler recycles the backend between
// statements, so the session is opened on DIRECT_DSN when one is configured
// (the endpoint DirectBunDB serves for the same reason) and on the main DSN
// otherwise, which is the direct endpoint on an install without a pooler.
//
// It is opened for one run and closed after it, so an idle process holds no
// connection for it. The connection budget counts these sessions per
// INSTALLATION while a run is in progress, not per process, because each
// caller admits one run at a time cluster-wide: the compaction by an advisory
// lock (a contender opens its session only long enough to be refused it), the
// statistics rollup by running on the cron leader (a second session only while
// a change of leader overlaps two runs). A new caller states its own bound
// there (docs/public/operate/db-connection-budget.md).

// ErrMaintenanceSessionUnavailable answers OpenMaintenanceSession on a
// Database with no DSN, or one built over a custom opener (tests, sqlmock),
// which has no way to open a session without the request pool's limits.
var ErrMaintenanceSessionUnavailable = errors.New("maintenance session unavailable on this database")

// maintenanceConnParams is sessionConnParams without the statement timeout.
// The idle limits stay, so the server still reaps a session whose process
// died; the caller bounds every statement itself.
func maintenanceConnParams() map[string]any {
	params := sessionConnParams()
	delete(params, "statement_timeout")
	return params
}

// maintenanceSQLOpener is the default opener of the maintenance session: the
// request pool's opener with maintenanceConnParams and no client read deadline
// beyond the caller's context.
func maintenanceSQLOpener() SQLOpener {
	return pgSQLOpenerWith(maintenanceConnParams, pgdriver.WithReadTimeout(0))
}

// OpenMaintenanceSession opens one dedicated maintenance session (see the
// header): no server statement_timeout, no client read deadline beyond ctx's,
// on the direct endpoint when one is configured. The caller owns it and must
// call the returned func, which closes the session and the single-connection
// pool behind it. Every statement on it should run under a context with a
// deadline and a statement_timeout the caller sets.
func (d *Database) OpenMaintenanceSession(ctx context.Context) (*sql.Conn, func(), error) {
	dsn := ""
	switch {
	case d.config.directDSN != nil && *d.config.directDSN != "":
		dsn = *d.config.directDSN
	case d.config.dsn != nil:
		dsn = *d.config.dsn
	}
	if d.config.maintenanceOpener == nil || dsn == "" {
		return nil, nil, ErrMaintenanceSessionUnavailable
	}
	return openMaintenanceSession(ctx, d.config.maintenanceOpener, d.config.driverName, dsn)
}

// OpenMaintenanceSessionDSN opens a maintenance session on dsn with the default
// opener. For the tests of the packages that run maintenance, which must run it
// on a session shaped like the deployed one rather than on whatever pool they
// have (NewGatePool's reason).
func OpenMaintenanceSessionDSN(ctx context.Context, dsn string) (*sql.Conn, func(), error) {
	return openMaintenanceSession(ctx, maintenanceSQLOpener(), "bun:pg", dsn)
}

func openMaintenanceSession(ctx context.Context, opener SQLOpener, driverName, dsn string) (*sql.Conn, func(), error) {
	db, err := opener(driverName, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open maintenance session: %w", err)
	}
	// One connection, none kept idle: the session is the run's and closes with it.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("open maintenance session: %w", err)
	}
	return conn, func() {
		_ = conn.Close()
		_ = db.Close()
	}, nil
}
