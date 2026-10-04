package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

// Collapse v1:platform:moduleReadiness to one version per row (epic memql#5316
// ruling D4, issue memql#5325), in bounded batches that survive an
// interruption (memql#5604).
//
// ===========================================================================
// WHAT THIS REMOVES
// ===========================================================================
// Every version of every readiness row except the newest. The newest is each
// node's current verdict on one module and is exactly what the fold reads; the
// ones behind it are restatements of a verdict that had not changed, appended
// by a writer that had no unchanged-skip.
//
// HOW MANY. 772k versions for a few dozen live ids on a production instance
// (2026-09-13), which is what motivated the write-on-change rule now in
// component/memql/readiness_write.go (readinessRewriteFloor). Every
// registration heartbeat flush -- one per connected machine every 15 s -- made
// every node that heard it re-evaluate and append all seven of its rows, at
// roughly 3.7 versions a second cluster-wide. Write-on-change stopped
// PRODUCING them in the release that shipped it; nothing has ever removed the
// ones already written, because v1:platform:moduleReadiness is append-only and
// had no retention of any kind.
//
// WHY COLLAPSE AND NOT DELETE EVERYTHING. D4 said "delete every
// v1:platform:moduleReadiness row once. The next boot writes the new shape",
// and the reason was D3's plan to change that shape to one ai--cluster row. D3
// was replaced by S1 (docs/superpowers/specs/2026-09-14-readiness-convergence-design.md)
// and per-node rows stayed, so the shape does not change and there is nothing
// for a rewrite to correct. Keeping the newest version therefore costs nothing
// and buys the absence of a window: delete the current verdicts too and every
// module reads `unreported` -- "Not reported" on the desk, an open core gate --
// from the moment this migration commits until each node's next pass. Honest,
// but a flicker on every installation for no reason that still holds.
//
// WHY A MIGRATION AND NOT A SWEEP. The rows are indistinguishable from each
// other: there is no field, no provenance and no age band that separates the
// ones a heartbeat produced from the ones a change produced, so a recurring
// sweep would have nothing to key on beyond "not the newest" -- which is this
// migration, and it only ever needs to run once per installation. The ongoing
// mechanism is the two things that now exist: write-on-change, which stops the
// churn at the source, and component/node/readiness_row_purge.go, which removes
// a stopped node's rows outright.
//
// SCOPED BY CONCEPT, NEVER BY KEY NAME. Nothing here reads a payload key at
// all, which is one better than the rule memql#5199 earned: `state`, `module`
// and `nodeId` are all names a product bundle mounted at MEMQL_DSL_PATH may
// declare on a concept this repository has never seen. A row of another
// concept under the same id is never read and never deleted, and the newest
// version is the newest among this concept's rows only.
//
// NOTHING READS READINESS HISTORY. The one query over the concept is
// moduleReadinessAll (dsl/platform/queries.memql), `asOf latest`; the OS folds
// the same latest rows. An audit walk over the concept CAN return older
// versions, and after this it returns one per node per module -- which is the
// whole of what the concept ever had to say, with the restatements removed.
//
// ===========================================================================
// WHY GO, AND WHY IT KEPT ITS VERSION
// ===========================================================================
// It shipped as one SQL DELETE joined to a per-id MAX("createdAt"), and on the
// instance whose 772k versions motivated it that statement outlived pgdriver's
// 10 s read deadline (memql#5604): the client gave up and sent no cancel, the
// backend kept deleting, and every 30 s retry on both migrating replicas
// started another full copy -- 33 concurrent backends, sixteen minutes before
// one committed. PR #5640 made the runner keep its lock across an uncertain
// outcome, so retries no longer overlap, but that does not make the statement
// fit: an attempt still fails at the deadline and still leaves an orphan an
// operator has to see stop before the lock may be cleared.
//
// SQL cannot be bounded here. bun runs a .sql migration as the statements the
// file contains, so a loop has to be one statement -- a DO block -- and a DO
// block is one read on the client however many COMMITs it makes inside, so it
// meets the same deadline the DELETE did.
//
// The SAME 14-digit version, as 20260913000000 did when it became Go: bun
// matches applied migrations on the version alone, so an installation that
// recorded the SQL form keeps it applied and this never runs there, while one
// that has not -- a fresh install, or an upgrade from v0.22.8 or earlier --
// runs this instead. Both leave the same table.
//
// ===========================================================================
// HOW EVERY STATEMENT STAYS BOUNDED
// ===========================================================================
// One id at a time, ids in order, and within an id newest first. A batch
// deletes the range from the cursor down to its floor, the
// readinessCollapseBatch-th version below the cursor, so it removes at most
// that many versions: (id, "createdAt") is the primary key, so no two versions
// of an id share a "createdAt" and the range holds exactly the versions the
// floor counted. Every read is a probe of the latest-row index (concept, id,
// "createdAt" DESC), which 20260913000000 and 20260914000000 ensure before this
// runs:
//
//   - an id's newest version, ORDER BY "createdAt" DESC LIMIT 1, which a
//     hypertable answers from the chunk holding it (ChunkAppend, newest chunk
//     first);
//   - the range below the cursor, which starts AT the cursor rather than behind
//     the versions earlier batches deleted, so no batch walks the dead tuples
//     its predecessors left;
//   - the next id, `id > cursor ORDER BY id LIMIT 1`, which the index descends
//     past the finished id rather than through it.
//
// Measured on TimescaleDB 2.29.0 over 791,770 versions of 140 ids in 11 chunks,
// on a shared machine: a 1000-version batch took 17 ms at the median (5000: 72
// ms; it grows with the batch), and deleting the range rather than joining each
// doomed row back by (id, "createdAt") took half the time and a quarter of the
// buffers, because the join probed every chunk's primary key for every row. The
// whole walk took 14 to 25 s against 3.9 s for the single DELETE on the same
// seed. That is the trade, made on purpose: the total is spread across
// statements and, if need be, across attempts, and no one statement carries it.
//
// Each statement runs in its own transaction under SET LOCAL
// statement_timeout, below the read deadline. One that runs long anyway -- a
// lock wait behind the stopped-node purge, an I/O stall, a plan nobody
// measured -- is then cancelled by the SERVER, with an error the client
// receives: the runner sees an acknowledged failure, releases its lock and
// retries, rather than the client timing out over a backend that is still
// running (migrationMayStillBeRunning, database.go). The batch it cancelled
// rolls back whole and the next attempt redoes it.
//
// And each transaction commits ASYNCHRONOUSLY (SET LOCAL synchronous_commit =
// off). A production-sized walk is some 1100 commits, and a synchronous commit
// waits for its WAL flush -- the one wait statement_timeout cannot end.
// Measured on the same seed: commits were 29 of the walk's 45 s, and one waited
// 7.9 s; asynchronous, they were half a second in all. Nothing is lost by it
// that matters. A crash can lose the last few batches, but only whole, with the
// cursor moves inside them, so the next attempt redoes exactly those. And it
// cannot leave the migration recorded over a lost batch: bun's MarkApplied
// commits synchronously after the last of them, and flushing a commit record
// flushes every WAL record before it, on the primary and on any standby the
// WAL streams to.
//
// ===========================================================================
// HOW IT RESUMES
// ===========================================================================
// Progress is one row of readinessCollapseProgressTable: the id being
// collapsed and the cursor below which its versions remain. A batch moves the
// cursor in the SAME statement that deletes, so a batch and its progress commit
// or roll back together: an interruption anywhere loses at most the batch in
// flight, and the next attempt starts where the last committed batch ended.
//
// Without it a retry could only find its place by walking the dead tuples the
// interrupted attempt left -- the first probe of a resumed walk has to step
// through every finished id in every chunk -- and that is the statement shape
// this migration exists to remove. Measured on the same seed with 75% of the
// ids collapsed and not yet vacuumed: that one probe walked 566k dead index
// entries with a heap fetch each, 101 ms and 76k buffers with every page
// cached. A cold production cache multiplies the cost of each buffer, not the
// number of them. With the cursor, a walk interrupted at 418,367 versions
// resumed at the id it stopped on and deleted the other 373,263, which together
// are every version that was not the newest.
//
// The table is dropped when the walk finishes, so a completed run leaves
// nothing behind, and a second run finds every id already holding one version.
// A cursor carried across attempts does mean an id it has passed is not
// revisited: a version written to that id after it was collapsed stays, as a
// version written after the single DELETE's snapshot did.
//
// ===========================================================================
// THE MIGRATION DEADLINE
// ===========================================================================
// The runner gives a whole attempt MIGRATION_TIMEOUT_MS (30 s by default), and
// at production volume the collapse can need more than one attempt. So before
// each statement it checks the attempt's deadline and stops while there is
// still room for the longest statement it might send plus the runner's own
// bookkeeping, returning errReadinessCollapseDeferred. That error is
// deliberately NOT a context error: the runner reads a context or transport
// error as "the statement may still be running" and keeps its lock for an
// operator, which is right for a statement cut off in flight and wrong for a
// run that stopped between statements with nothing outstanding. The next
// attempt -- the node's monitor tick, or the migrate Job's retry -- resumes
// from the cursor.
//
// bun names a Go migration after the FILE its Register call sits in, so the
// call must stay in this file.

const (
	// readinessCollapseMigrationName is this file's name, which is the version
	// and comment bun derives for the migration.
	readinessCollapseMigrationName = "20260921000000_module_readiness_history_collapse"

	// readinessCollapseConcept is the one concept this migration reads or
	// deletes.
	readinessCollapseConcept = "v1:platform:moduleReadiness"

	// readinessCollapseProgressTable holds the walk's cursor between attempts
	// and is dropped when the walk finishes. Unqualified, like "MemoryNodes":
	// it lands in the same schema the table it describes does.
	readinessCollapseProgressTable = "module_readiness_collapse_progress"

	// readinessCollapseBatch is the most versions one delete removes. At the
	// 17 ms median measured in the header, it leaves the statement bound two
	// orders of magnitude of room for a slower or busier database; a larger
	// batch would buy fewer round trips, and the total is spent per version
	// either way.
	readinessCollapseBatch = 1000

	// readinessCollapseStatementTimeout is the server-side bound on every
	// statement, kept well under pgdriver's 10 s read deadline so the server
	// cancels a runaway and answers before the client stops reading.
	readinessCollapseStatementTimeout = 5 * time.Second

	// readinessCollapseReserve is how much of the attempt's deadline must be
	// left before another statement is sent: the longest one may run (the
	// statement bound) plus the runner's MarkApplied and unlock.
	readinessCollapseReserve = readinessCollapseStatementTimeout + 2*time.Second
)

// errReadinessCollapseDeferred marks an attempt that stopped between
// statements, with every batch it ran committed and nothing in flight. Not a
// context error, on purpose; see the header.
var errReadinessCollapseDeferred = errors.New("module readiness history collapse deferred to the next migration attempt")

// readinessCollapseReport is what one run of the collapse did.
type readinessCollapseReport struct {
	// Collapsed is how many versions this run deleted.
	Collapsed int64
	// Batches is how many delete statements this run sent, and LargestBatch
	// how many versions the largest of them deleted.
	Batches      int
	LargestBatch int64
	// Ids is how many ids this run finished.
	Ids int
	// Resumed says the run started from a cursor an interrupted run left.
	Resumed bool
	// Done says the walk finished and the progress table is gone.
	Done bool
}

// registerModuleReadinessHistoryCollapse adds the migration to the set the SQL
// files are discovered into. It MUST be called from this file; see above.
func registerModuleReadinessHistoryCollapse(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil || goMigrationRegistered(m, readinessCollapseMigrationName) {
		return
	}
	m.MustRegister(
		func(ctx context.Context, db *bun.DB) error {
			_, err := collapseModuleReadinessHistory(ctx, db, logger, readinessCollapseBatch)
			return err
		},
		revertModuleReadinessHistoryCollapse,
	)
}

// revertModuleReadinessHistoryCollapse is the down, and it is DELIBERATELY
// empty. The up removed restatements of verdicts that had not changed. Nothing
// in this database can reconstruct one, and a down that invented versions to
// fill the gap would fabricate a history of evaluations that never happened --
// a record of a cluster checking itself at moments it did not, which is worse
// than the absence.
//
// There is also nothing to roll back TO. The up changes no shape and drops no
// field: every node's current verdict is still there, still the newest version
// of its own row, and every reader of the concept (`asOf latest`) sees exactly
// what it saw before. Rolling back the engine version that ran this needs no
// data change at all. A function that does nothing rather than an absent one,
// so the set records the decision.
func revertModuleReadinessHistoryCollapse(context.Context, *bun.DB) error { return nil }

// collapseModuleReadinessHistory runs the collapse, resuming any walk an
// earlier run left, until it finishes or the attempt's deadline says stop.
// batch is the most versions one statement deletes. db is the migration pool
// at boot; a test passes its own transaction or connection, since every
// statement goes through db and nothing here assumes a second connection.
func collapseModuleReadinessHistory(ctx context.Context, db bun.IDB, logger *slog.Logger, batch int) (readinessCollapseReport, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if batch < 1 {
		return readinessCollapseReport{}, fmt.Errorf("module readiness history collapse: batch size %d, want at least 1", batch)
	}
	c := &readinessCollapse{db: db, logger: logger, batch: batch, started: time.Now()}
	return c.run(ctx)
}

// readinessCollapse is one run of the walk.
type readinessCollapse struct {
	db      bun.IDB
	logger  *slog.Logger
	batch   int
	started time.Time

	// cursor is the id being collapsed. olderThan bounds the versions of it
	// still to delete; invalid means the walk has not started on it, so the
	// bound is its newest version.
	cursor    string
	olderThan sql.NullTime
	// collapsedBefore is the running total earlier runs recorded.
	collapsedBefore int64

	report readinessCollapseReport
}

func (c *readinessCollapse) run(ctx context.Context) (readinessCollapseReport, error) {
	if err := c.open(ctx); err != nil {
		return c.report, err
	}
	if c.report.Resumed {
		c.logger.Info("module readiness history collapse: resuming an interrupted walk",
			"migration", readinessCollapseMigrationName, "cursor", c.cursor,
			"collapsedByEarlierAttempts", c.collapsedBefore)
	}
	for {
		if !c.olderThan.Valid {
			newest, found, err := c.newest(ctx)
			if err != nil {
				return c.report, err
			}
			if found {
				c.olderThan = sql.NullTime{Time: newest, Valid: true}
			}
		}
		if c.olderThan.Valid {
			deleted, floor, err := c.deleteBatch(ctx)
			if err != nil {
				return c.report, err
			}
			if deleted == int64(c.batch) {
				// A full batch: there may be more below its floor.
				c.olderThan = sql.NullTime{Time: floor, Valid: true}
				continue
			}
		}
		next, found, err := c.advance(ctx)
		if err != nil {
			return c.report, err
		}
		if c.cursor != "" {
			c.report.Ids++
		}
		if !found {
			return c.report, c.finish(ctx)
		}
		c.cursor, c.olderThan = next, sql.NullTime{}
	}
}

// step runs one statement's work in its own transaction under the statement
// bound, after making sure the attempt has room for it.
func (c *readinessCollapse) step(ctx context.Context, fn func(ctx context.Context, tx bun.Tx) error) error {
	if why, stop := c.mustStop(ctx); stop {
		return c.deferred(why)
	}
	return c.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", readinessCollapseStatementTimeout.Milliseconds())); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "SET LOCAL synchronous_commit = off"); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

// mustStop says whether the next statement must not be sent, and why. An
// attempt whose context has already ended stops HERE, between statements, so
// what the runner receives is errReadinessCollapseDeferred rather than the
// context error it would read as a statement still running.
func (c *readinessCollapse) mustStop(ctx context.Context) (string, bool) {
	if ctx.Err() != nil {
		return "the migration attempt ended", true
	}
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left < readinessCollapseReserve {
			return fmt.Sprintf("%s was left before the migration deadline, less than the %s one more statement and the runner's bookkeeping may need",
				left.Round(time.Millisecond), readinessCollapseReserve), true
		}
	}
	return "", false
}

func (c *readinessCollapse) deferred(why string) error {
	c.logger.Info("module readiness history collapse: stopping between statements; the next migration attempt resumes from the cursor",
		"migration", readinessCollapseMigrationName, "why", why, "cursor", c.cursor,
		"collapsedThisAttempt", c.report.Collapsed, "collapsedInAll", c.collapsedBefore+c.report.Collapsed,
		"idsThisAttempt", c.report.Ids, "duration", time.Since(c.started).Round(time.Millisecond).String())
	return fmt.Errorf("%w: %s; %d versions collapsed by this attempt, %d in all, every batch committed and the cursor at %q",
		errReadinessCollapseDeferred, why, c.report.Collapsed, c.collapsedBefore+c.report.Collapsed, c.cursor)
}

// fail says what the step was doing and how far the attempt got. The deferral
// passes through as it is: it already says both, and it must stay recognisable
// as itself.
func (c *readinessCollapse) fail(err error, doing string) error {
	if errors.Is(err, errReadinessCollapseDeferred) {
		return err
	}
	return fmt.Errorf("module readiness history collapse: %s, after %d versions collapsed by this attempt (%d in all; "+
		"every committed batch stands and the next attempt resumes from the cursor): %w",
		doing, c.report.Collapsed, c.collapsedBefore+c.report.Collapsed, err)
}

// readinessCollapseProgressDDL creates the progress table: one row, the id
// being collapsed and the cursor below which its versions remain (NULL: start
// at its newest), plus the running total and when the walk began, for whoever
// looks while it is unfinished.
const readinessCollapseProgressDDL = `CREATE TABLE IF NOT EXISTS ` + readinessCollapseProgressTable + ` (
  singleton  boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  cursor_id  text NOT NULL DEFAULT '',
  older_than timestamptz,
  collapsed  bigint NOT NULL DEFAULT 0,
  started_at timestamptz NOT NULL DEFAULT now()
)`

// open creates the progress row if no earlier run left one, and reads it.
func (c *readinessCollapse) open(ctx context.Context) error {
	err := c.step(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, readinessCollapseProgressDDL); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+readinessCollapseProgressTable+
			` (singleton) VALUES (true) ON CONFLICT (singleton) DO NOTHING`); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT cursor_id, older_than, collapsed FROM `+
			readinessCollapseProgressTable+` WHERE singleton`).
			Scan(&c.cursor, &c.olderThan, &c.collapsedBefore)
	})
	if err != nil {
		return c.fail(err, "opening the progress row")
	}
	c.report.Resumed = c.cursor != "" || c.collapsedBefore > 0
	return nil
}

// readinessCollapseNewestSQL reads one id's newest version of the concept.
//
// staged-data: MUST-NOT-GATE -- a MIGRATION must see every row, by definition
// (epic memql#3974, task memql#3984): gated, a staged id's newest version
// reads as absent, and the walk moves past an id whose history it never
// collapsed.
const readinessCollapseNewestSQL = `SELECT "createdAt"
  FROM "MemoryNodes"
 WHERE concept = ? AND id = ?
 ORDER BY "createdAt" DESC
 LIMIT 1`

// newest reads the cursor id's newest version, or found=false when the id has
// no version of this concept (the empty cursor a walk starts from, or an id the
// stopped-node purge removed between attempts).
func (c *readinessCollapse) newest(ctx context.Context) (time.Time, bool, error) {
	var newest time.Time
	found := true
	err := c.step(ctx, func(ctx context.Context, tx bun.Tx) error {
		err := tx.QueryRowContext(ctx, readinessCollapseNewestSQL, readinessCollapseConcept, c.cursor).Scan(&newest)
		if errors.Is(err, sql.ErrNoRows) {
			found = false
			return nil
		}
		return err
	})
	if err != nil {
		return time.Time{}, false, c.fail(err, fmt.Sprintf("reading the newest version of %q", c.cursor))
	}
	return newest, found, nil
}

// readinessCollapseBatchSQL deletes one batch below the cursor and moves the
// cursor to the oldest version it deleted, in one statement, so the two commit
// or roll back together. floor is the batch-th version below the cursor --
// OFFSET batch-1 is the statement's bound -- and when fewer remain it is absent
// and the range runs to the id's oldest version. A batch that deleted nothing
// leaves the progress row as it was.
//
// staged-data: MUST-NOT-GATE -- a MIGRATION must see every row, by definition
// (epic memql#3974, task memql#3984): gated, a staged id keeps every version
// the migration exists to remove.
const readinessCollapseBatchSQL = `WITH floor AS (
  SELECT "createdAt" AS at
    FROM "MemoryNodes"
   WHERE concept = ? AND id = ? AND "createdAt" < ?
   ORDER BY "createdAt" DESC
  OFFSET ? LIMIT 1
), gone AS (
  DELETE FROM "MemoryNodes"
   WHERE concept = ? AND id = ? AND "createdAt" < ?
     AND "createdAt" >= COALESCE((SELECT at FROM floor), '-infinity'::timestamptz)
  RETURNING "createdAt"
), moved AS (
  UPDATE ` + readinessCollapseProgressTable + `
     SET older_than = (SELECT min("createdAt") FROM gone),
         collapsed = collapsed + (SELECT count(*) FROM gone)
   WHERE singleton AND EXISTS (SELECT 1 FROM gone)
)
SELECT count(*), min("createdAt") FROM gone`

// deleteBatch deletes at most c.batch versions of the cursor id below
// c.olderThan and reports how many it deleted and the oldest of them.
func (c *readinessCollapse) deleteBatch(ctx context.Context) (int64, time.Time, error) {
	var (
		deleted int64
		floor   sql.NullTime
	)
	err := c.step(ctx, func(ctx context.Context, tx bun.Tx) error {
		return tx.QueryRowContext(ctx, readinessCollapseBatchSQL,
			readinessCollapseConcept, c.cursor, c.olderThan.Time, c.batch-1,
			readinessCollapseConcept, c.cursor, c.olderThan.Time).Scan(&deleted, &floor)
	})
	if err != nil {
		return 0, time.Time{}, c.fail(err, fmt.Sprintf("deleting a batch of %q older than %s",
			c.cursor, c.olderThan.Time.UTC().Format(time.RFC3339Nano)))
	}
	c.report.Batches++
	c.report.Collapsed += deleted
	c.report.LargestBatch = max(c.report.LargestBatch, deleted)
	return deleted, floor.Time, nil
}

// readinessCollapseAdvanceSQL moves the cursor to the concept's next id. No
// row comes back after the last one.
//
// staged-data: MUST-NOT-GATE -- a MIGRATION must see every row, by definition
// (epic memql#3974, task memql#3984): gated, the walk steps over a staged id
// and never collapses it.
const readinessCollapseAdvanceSQL = `UPDATE ` + readinessCollapseProgressTable + ` p
   SET cursor_id = n.id, older_than = NULL
  FROM (SELECT id
          FROM "MemoryNodes"
         WHERE concept = ? AND id > ?
         ORDER BY id
         LIMIT 1) n
 WHERE p.singleton
RETURNING p.cursor_id`

// advance moves the cursor to the next id of the concept and reports it, or
// found=false when the cursor id was the last.
func (c *readinessCollapse) advance(ctx context.Context) (string, bool, error) {
	var next string
	found := true
	err := c.step(ctx, func(ctx context.Context, tx bun.Tx) error {
		err := tx.QueryRowContext(ctx, readinessCollapseAdvanceSQL, readinessCollapseConcept, c.cursor).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			found = false
			return nil
		}
		return err
	})
	if err != nil {
		return "", false, c.fail(err, fmt.Sprintf("moving past %q", c.cursor))
	}
	return next, found, nil
}

// finish drops the progress table: the walk is over, and a completed run
// leaves nothing behind.
func (c *readinessCollapse) finish(ctx context.Context) error {
	if err := c.step(ctx, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+readinessCollapseProgressTable)
		return err
	}); err != nil {
		return c.fail(err, "dropping the finished progress table")
	}
	c.report.Done = true
	c.logger.Info("module readiness history collapse: done",
		"migration", readinessCollapseMigrationName,
		"collapsedThisAttempt", c.report.Collapsed, "collapsedInAll", c.collapsedBefore+c.report.Collapsed,
		"batches", c.report.Batches, "largestBatch", c.report.LargestBatch, "idsThisAttempt", c.report.Ids,
		"resumed", c.report.Resumed, "duration", time.Since(c.started).Round(time.Millisecond).String())
	return nil
}
