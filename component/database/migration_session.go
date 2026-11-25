package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"
)

// THE MIGRATION LOCK IS THE MIGRATION SESSION (2026-11-25).
//
// ===========================================================================
// WHY NOT A ROW
// ===========================================================================
// bun's own lock is a row: Lock INSERTs into bun_migration_locks and Unlock
// DELETEs it. A row lasts until something deletes it, and nothing does when
// the process that wrote it ends without running its Unlock: an engine whose
// init deadline called os.Exit, a SIGKILL, an OOM kill, a node lost with its
// pods. The runner also kept the row on purpose while a statement might still
// be running, because a row cannot say whether its holder has stopped.
// Production's migrations stopped that way on 2026-09-26, 2026-10-30 and
// 2026-11-25, each time until an operator verified the holder had stopped and
// deleted the row by hand.
//
// ===========================================================================
// THE LOCK LASTS EXACTLY AS LONG AS THE WORK
// ===========================================================================
// An attempt runs EVERY statement on one database session and holds the
// migration lock, a session advisory lock keyed by the schema bun's tables
// live in, on that same session. The session's pool can never open a second
// connection (migrationSessionConnector), so once its one connection is lost
// the attempt cannot touch the database at all. The backend that holds the
// lock is therefore the backend that ran the attempt's work, and Postgres
// releases a session lock only when that backend unlocks or exits:
//
//   - The process ends while the session is idle: the server sees the socket
//     close and the backend exits at once. A node lost without closing its
//     sockets leaves an idle backend that idle_session_timeout ends within
//     five minutes (migrationConnParams).
//   - The process ends, or the attempt's deadline cuts its connection, while a
//     statement runs: the backend carries on with the statement, as on
//     2026-11-25, and keeps the lock until the statement ends and the backend
//     finds its client gone. Nothing overlaps the orphaned statement, because
//     nothing can take the lock while it runs, and nobody has to release
//     anything, because the lock goes when the statement ends.
//   - The attempt ends normally: it releases the lock and closes the session.
//
// So the runner no longer keeps anything "until an operator verifies the holder
// has stopped": the holder stopping IS the release. A lock taken through a pool
// that can replace its connections would be lost silently with the connection
// holding it, while the next statement ran on a backend holding nothing. Hence
// one connection, and never a second.
//
// The session reads with no driver read deadline of its own (ReadTimeout 0 in
// the default connector). Every statement it sends carries a context deadline
// instead: the attempt's, the ledger's (migrationLedgerContext) or a cleanup's.
// pgdriver's ten-second default would otherwise cut every index build longer
// than that, and with no second connection a cut ends the attempt.
//
// ===========================================================================
// THE FENCE, FOR ENGINES THAT LOCK BY ROW
// ===========================================================================
// An engine older than this one knows only the row. While both run, during a
// rollout or a rollback, an attempt holding the session lock also writes the
// row, so the older engine's Lock fails on the row's unique key and stays out.
// This row's id is NEGATIVE, which no row an older engine writes can be (bun
// takes those from the table's sequence), and that sign marks it as written
// under the session lock. The fence outlives its session like any row, so an
// attempt that holds the session lock and finds a negative-id row knows that
// the row's writer, and every statement it ran, has finished, and clears the row
// itself. A positive-id row belongs to an engine that locks by row. Nothing
// here can tell whether its holder has stopped, so today's rules stand for it:
// refuse, report it once it has outlasted any attempt, and leave its release
// to the runbook.
//
// ===========================================================================
// AN ATTEMPT FINDING ITS OWN NODE'S STATEMENT
// ===========================================================================
// When an attempt's deadline cuts a build, the attempt after it on the same
// node finds the lock held by the backend still running that build. That is no
// other migrator, so it waits for it within its own attempt and carries on once
// it ends (lock). Any other holder refuses the attempt at once, as bun's
// try-lock did, so a starting replica never spends a whole attempt waiting for
// a sibling before it serves.
//
// Regression evidence: TestAHolderKilledMidBuildIsRecoveredOnceItsStatementEnds
// (migration_lock_killed_holder_db_test.go) and migration_lock_db_test.go. The
// decision record is docs/internal/ops/harness-decisions.md, "The migration
// lock is the migration session".

// ConnectorOpener opens the driver connector a pool dials through. The
// migration session takes one so it can control how many connections that
// connector may ever make.
type ConnectorOpener func(dsn string) (driver.Connector, error)

// bunMigrationsTable is the table_name bun's Lock writes into
// bun_migration_locks: the migrations table's name as migrate.NewMigrator
// defaults it. This package never overrides it.
const bunMigrationsTable = "bun_migrations"

// errNoMigrationSession says why an attempt ran no migration: there was no DSN
// or connector to open its session on. Migrating over a shared pool instead
// would take the lock on one connection and run statements on others.
var errNoMigrationSession = errors.New("migrations run on a database session of their own, and this database has no DSN or migration connector to open one on")

// errMigrationSessionEnded is what every statement an attempt sends after its
// session's connection was lost gets back: the attempt never opens a second
// session. The lost connection's backend may still be running the statement
// that was cut off, holding the migration lock until it ends.
var errMigrationSessionEnded = errors.New("the migration session ended, and an attempt never opens a second one")

// migrationSessionConnector hands out its first connection and refuses every
// later one, so the pool over it can never replace the session that holds the
// migration lock with one that does not.
type migrationSessionConnector struct {
	inner  driver.Connector
	mu     sync.Mutex
	opened bool
}

func (c *migrationSessionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.opened {
		return nil, errMigrationSessionEnded
	}
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	c.opened = true
	return conn, nil
}

func (c *migrationSessionConnector) Driver() driver.Driver { return c.inner.Driver() }

// migrationBackend names one server backend: its pid, and when it started,
// because a pid alone is reused.
type migrationBackend struct {
	pid     int64
	started time.Time
}

// migrationLockHolder is the backend holding the migration lock, as
// pg_stat_activity shows it.
type migrationLockHolder struct {
	backend    migrationBackend
	app        string
	state      string
	waiting    string
	runningFor time.Duration
	query      string
}

// running reports whether the holder is executing a statement. One that is
// will release the lock on its own when the statement ends.
func (h migrationLockHolder) running() bool { return h.state == "active" }

// watchKey is what the stuck watch tells one holder from the next by.
func (h migrationLockHolder) watchKey() string {
	return "session:" + strconv.FormatInt(h.backend.pid, 10) + "@" + strconv.FormatInt(h.backend.started.UnixNano(), 10)
}

func (h migrationLockHolder) String() string {
	state := h.state
	if state == "" {
		state = "state not visible"
	}
	if h.waiting != "" {
		state += ", waiting on " + h.waiting
	}
	return fmt.Sprintf("backend %d (application %q, %s, its statement running for %s: %s)",
		h.backend.pid, h.app, state, h.runningFor, h.query)
}

// migrationSession is one attempt's database session. Every statement the
// attempt sends runs on it, and it holds the migration lock and the fence.
type migrationSession struct {
	db      *bun.DB
	logger  *slog.Logger
	schema  string
	key     int64
	backend migrationBackend
	locked  bool
	fenceID int64
}

// migrationLockKey is the session advisory-lock key for migrating one schema's
// bun tables: FNV-1a over a fixed prefix, the schema and the table, so every
// node migrating the same schema contends on one key and a scratch schema
// (a test's) has its own. Advisory locks are per database already.
func migrationLockKey(schema string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("memql:migrations:" + schema + "." + bunMigrationsTable))
	return int64(h.Sum64())
}

// migrationSessionDSN is the endpoint the session dials: the direct
// (non-pooled) one when configured, because a transaction pooler hands a
// session's statements to whichever backend is free and would drop the lock.
func (d *Database) migrationSessionDSN() string {
	if d.config.directDSN != nil && *d.config.directDSN != "" {
		return *d.config.directDSN
	}
	if d.config.dsn != nil {
		return *d.config.dsn
	}
	return ""
}

// migrationSessionQuery identifies the session's backend and the schema bun's
// unqualified tables resolve to on it.
const migrationSessionQuery = `SELECT pg_backend_pid(), a.backend_start, COALESCE(current_schema(), '')
  FROM pg_stat_activity a
 WHERE a.pid = pg_backend_pid()`

// openMigrationSession opens this attempt's session and identifies it. It
// takes no lock.
func (d *Database) openMigrationSession(ctx context.Context) (*migrationSession, error) {
	dsn := d.migrationSessionDSN()
	if d.config.migrationConnector == nil || dsn == "" {
		return nil, errNoMigrationSession
	}
	connector, err := d.config.migrationConnector(dsn)
	if err != nil {
		return nil, err
	}
	sqlDB := sql.OpenDB(&migrationSessionConnector{inner: connector})
	sqlDB.SetMaxOpenConns(migrationMaxOpenConns)
	sqlDB.SetMaxIdleConns(migrationMaxOpenConns)
	sessionDB, err := d.openBun(sqlDB)
	if err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	s := &migrationSession{db: sessionDB, logger: d.Logger}
	var started sql.NullTime
	if err := sessionDB.QueryRowContext(ctx, migrationSessionQuery).Scan(&s.backend.pid, &started, &s.schema); err != nil {
		_ = sessionDB.Close()
		return nil, err
	}
	s.backend.started = started.Time
	s.key = migrationLockKey(s.schema)
	return s, nil
}

// migrationLockHolderQuery reads the backend holding a bigint advisory key in
// this database. The key is stored split across classid (high half) and objid
// (low half) with objsubid = 1, and pg_locks lists every database's locks.
const migrationLockHolderQuery = `SELECT a.pid, a.backend_start, COALESCE(a.application_name, ''), COALESCE(a.state, ''),
       COALESCE(a.wait_event_type || ':' || a.wait_event, ''),
       COALESCE(round(extract(epoch FROM now() - a.query_start))::int8, 0),
       left(COALESCE(a.query, ''), 160)
  FROM pg_locks l
  JOIN pg_stat_activity a ON a.pid = l.pid
 WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1
   AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
   AND l.classid::int8 = ? AND l.objid::int8 = ?
 LIMIT 1`

// holder reads who holds the migration lock; found is false when nobody does
// any more.
func (s *migrationSession) holder(ctx context.Context) (migrationLockHolder, bool, error) {
	var (
		h       migrationLockHolder
		started sql.NullTime
		seconds int64
	)
	hi, lo := int64(uint64(s.key)>>32), int64(uint64(s.key)&0xffffffff)
	err := s.db.QueryRowContext(ctx, migrationLockHolderQuery, hi, lo).
		Scan(&h.backend.pid, &started, &h.app, &h.state, &h.waiting, &seconds, &h.query)
	if errors.Is(err, sql.ErrNoRows) {
		return h, false, nil
	}
	if err != nil {
		return h, false, err
	}
	h.backend.started = started.Time
	h.runningFor = time.Duration(seconds) * time.Second
	return h, true, nil
}

// lock takes the migration lock on the session, or says who holds it. A holder
// that is one of this node's earlier sessions, still running a statement an
// earlier attempt's deadline cut off, is waited for within the attempt; when
// the attempt ends first, the wait defers (ErrMigrationDeferred) and the next
// attempt waits on. Any other holder refuses the attempt at once
// (ErrMigrationLockHeld).
func (s *migrationSession) lock(ctx context.Context, own func(migrationBackend) bool) error {
	wait := latestRowIndexPoll
	var waitedFor *migrationLockHolder
	for {
		var held bool
		if err := s.db.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(?)`, s.key).Scan(&held); err != nil {
			if waitedFor != nil && ctx.Err() != nil {
				return s.ownWaitEnded(*waitedFor)
			}
			return fmt.Errorf("taking the migration lock: %w", err)
		}
		if held {
			s.locked = true
			return nil
		}
		holder, found, err := s.holder(ctx)
		if err != nil {
			if waitedFor != nil && ctx.Err() != nil {
				return s.ownWaitEnded(*waitedFor)
			}
			return fmt.Errorf("reading who holds the migration lock: %w", err)
		}
		if found && !own(holder.backend) {
			return migrationLockHeldError{
				cause:    fmt.Errorf("the migration lock is held by %s", holder),
				holder:   holder.watchKey(),
				stalled:  !holder.running(),
				describe: holder.String(),
			}
		}
		if found && waitedFor == nil {
			s.logger.Warn("migration lock: this node's earlier migration statement is still running; waiting for it",
				"holder", holder.String())
			waitedFor = &holder
		}
		select {
		case <-ctx.Done():
			if waitedFor != nil {
				return s.ownWaitEnded(*waitedFor)
			}
			return fmt.Errorf("taking the migration lock: %w", ctx.Err())
		case <-time.After(wait):
		}
		if wait < latestRowIndexPollMax {
			wait *= 2
		}
	}
}

// ownWaitEnded is the deferral an attempt ends with when it ran out waiting
// for its own node's earlier statement. Nothing of its own is running: it never
// took the lock.
func (s *migrationSession) ownWaitEnded(holder migrationLockHolder) error {
	return fmt.Errorf("the attempt ended while this node's earlier migration statement was still running in %s; "+
		"its backend holds the migration lock until it ends: %w", holder, ErrMigrationDeferred)
}

// migrationFenceInsert writes the fence: bun's own row, under a negative id no
// older engine's row can carry.
const migrationFenceInsert = `INSERT INTO bun_migration_locks (id, table_name)
VALUES (-nextval(pg_get_serial_sequence('bun_migration_locks', 'id')), ?)
RETURNING id`

// fence writes the row an engine that locks by row is kept out by, after
// clearing one a session left when it ended. A row such an engine holds refuses
// the attempt: it is that engine's lock. Call it holding the migration lock.
func (s *migrationSession) fence(ctx context.Context) error {
	for range 3 {
		var id int64
		err := s.db.QueryRowContext(ctx, migrationFenceInsert, bunMigrationsTable).Scan(&id)
		if err == nil {
			s.fenceID = id
			return nil
		}
		if !isUniqueViolation(err) {
			return fmt.Errorf("writing the migration fence: %w", err)
		}
		var existing int64
		switch err := s.db.QueryRowContext(ctx, `SELECT id FROM bun_migration_locks WHERE table_name = ?`, bunMigrationsTable).Scan(&existing); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return fmt.Errorf("reading the migration lock row: %w", err)
		}
		if existing > 0 {
			return migrationLockHeldError{
				cause: fmt.Errorf("lock row %d was written by an engine that locks by row "+
					"(bun's migrations table is already locked)", existing),
				holder:   "row:" + strconv.FormatInt(existing, 10),
				stalled:  true,
				row:      existing,
				describe: "lock row " + strconv.FormatInt(existing, 10) + " in bun_migration_locks, written by an engine that locks by row",
			}
		}
		// A negative id is a fence written under this lock, which this session
		// now holds: its writer's session, and every statement it ran, is over.
		if _, err := s.db.ExecContext(ctx, `DELETE FROM bun_migration_locks WHERE id = ?`, existing); err != nil {
			return fmt.Errorf("clearing the fence a migration session left: %w", err)
		}
		s.logger.Info("migration lock: cleared the fence an earlier migration session left when it ended", "fenceId", existing)
	}
	return errors.New("writing the migration fence: the lock row kept changing under it")
}

// end releases what the attempt holds and closes the session. A session whose
// connection was lost can release nothing; its backend keeps the lock until the
// statement it was running ends, and the next attempt clears the fence then.
func (s *migrationSession) end() {
	defer func() { _ = s.db.Close() }()
	if !s.locked {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.fenceID != 0 {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM bun_migration_locks WHERE id = ?`, s.fenceID); err != nil {
			s.releaseFailed(err)
			return
		}
	}
	if _, err := s.db.ExecContext(ctx, `SELECT pg_advisory_unlock(?)`, s.key); err != nil {
		s.releaseFailed(err)
	}
}

func (s *migrationSession) releaseFailed(err error) {
	if errors.Is(err, errMigrationSessionEnded) {
		s.logger.Warn("migration lock: the attempt's session ended before it could release the lock; its backend keeps it "+
			"until the statement it was running ends, and the next attempt clears the fence then",
			"backend", s.backend.pid, "fenceId", s.fenceID)
		return
	}
	s.logger.Warn("migration lock: releasing it failed; the lock ends with the session, which closes now, and the next "+
		"attempt clears the fence", "backend", s.backend.pid, "fenceId", s.fenceID, "error", err)
}

// isUniqueViolation reports whether err is Postgres refusing a write on a
// unique key.
func isUniqueViolation(err error) bool {
	var pgErr pgdriver.Error
	return errors.As(err, &pgErr) && pgErr.Field('C') == pgUniqueViolation
}
