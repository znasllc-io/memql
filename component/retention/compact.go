package retention

// compact.go -- returning the space retention leaves inside CLOSED chunks.
//
// # Why this exists
//
// "MemoryNodes" and "SecretMemoryNodes" are append-only hypertables in 7-day
// chunks on "createdAt". Inserts only ever reach the newest chunk, so the space a
// DELETE frees in a closed chunk -- every retention whose window is longer than a
// chunk (declared @retention TTLs, the work journal's 30 days, pipeline runs) --
// and the space an UPDATE that shrank rows left behind (the October 14 schema
// conversion) is reused by nothing and never returned. Production measured five
// closed chunks holding about 7.1 GB free, one of them 7.7 GB with 3.3 GB live.
// Retention deleted rows; the disk did not get them back.
//
// # What a pass does
//
// One pass rewrites at most ONE closed chunk, with TimescaleDB's reorder_chunk,
// which rewrites the chunk's heap, TOAST and indexes as CLUSTER does while reads
// carry on, and then VACUUM (ANALYZE)s it, because a rewrite does not set the
// visibility map index-only reads need. In order:
//
//  1. Open the maintenance session and take the cluster-wide advisory lock, so
//     two replicas -- the scheduled pass and an owner's manual one -- never run
//     at once. A contender answers already_running.
//  2. List the closed chunks: range ended at least a day ago, not compressed.
//  3. Read the room (judgeRoom): the database-health report, or the configured
//     volume size, against the live database sizes. A real pass that cannot
//     read it stops here, before scanning anything it could not act on.
//  4. Measure the chunks whose measurement is missing or stale (compact_decide.go,
//     staleness), at most MeasureChunks of them and for at most measureBudget, by
//     one heap scan each: the summed pg_column_size of every column, plus tuple
//     overhead, read without following TOAST pointers. Measurements are kept in
//     chunk_compaction_progress and reused while the chunk's size and its
//     insert/update/delete counter still read what they read then; a retention
//     delete moves the counter, so the next pass measures again.
//  5. Choose the most reclaimable chunk over both thresholds that fits the room
//     (choose): twice its live data and indexes on the data volume.
//  6. Refuse unless the session's role owns the chunk (reorder_chunk requires
//     the owner) and the WAL archive is keeping up (judgeWAL): a rewrite writes
//     WAL roughly the size of what it rewrites.
//  7. reorder_chunk under lock_timeout 1s. The copy lets reads through; the swap
//     at the end needs an AccessExclusive lock, and while it waits it queues
//     every later reader, so a reader in the way makes it fail within a second
//     instead, with the chunk intact and the copy discarded. Measured on a local
//     TimescaleDB 2.29.1 before this was built: a new reader waited under a
//     second, and a quiet retry succeeded; compact_db_test.go holds the clean
//     failure and the retry. A lock timeout is retried up to maxAttempts, the
//     WAL guard re-read before each; anything else ends the pass.
//
// # What it never does
//
// Change a row. reorder_chunk swaps a rewritten copy in or discards it; a lock
// timeout, a statement timeout, a lost connection or any error leaves the chunk as
// it was. A pass that rewrites nothing says why (CompactionReport.Reason), and a
// failure is logged at Warn naming the chunk. No row's content is read or
// returned: the measurement aggregates column sizes, which is why no row
// admission applies, and the handler's cluster-owner gate is the authorization.
//
// # What it costs and where it runs
//
// One extra connection while a pass runs, cluster-wide (the advisory lock), opened
// on the direct endpoint (component/database/maintenance_session.go). The nightly
// reclaimClosedChunkSpace automation runs it at 04:20 UTC on the cron leader,
// after the 03:20 log and 03:40 work-journal retention. A rewrite holds an
// ExclusiveLock on the chunk while it copies, so a DELETE into that chunk waits for
// it: the hourly declared-retention sweep at :40 can see one batch end at its own
// statement timeout during a long rewrite, and retries next hour.
//
// Decisions and their measurements: docs/public/operate/database-platform.md
// (Closed-chunk compaction). Tests: compact_decide_test.go (every decision),
// compact_db_test.go (against TimescaleDB).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/metrics"
)

// The run's time limits. The server ends an overrunning statement (its
// statement_timeout is set below the client's deadline), so a timeout is a clean
// rollback, not a severed connection.
const (
	// runBudget bounds a whole pass: 04:20 to 05:15 at the latest.
	runBudget = 55 * time.Minute
	// measureBudget: no new measurement starts after this much of the pass.
	measureBudget = 15 * time.Minute
	// measureStatementTimeout bounds one chunk's heap scan.
	measureStatementTimeout = 10 * time.Minute
	// rewriteStatementTimeout bounds one reorder_chunk. A chunk that cannot be
	// rewritten within it fails as statement_timeout, intact, and backs off.
	// Measured on PostgreSQL 16 / TimescaleDB 2.27 (local disk, warm cache): a
	// 251 MB chunk with half its rows deleted rewrote, indexes included, in
	// 0.9 s. Production's 7.7 GB chunk at even a fiftieth of that rate is about
	// 25 minutes, inside the bound.
	rewriteStatementTimeout = 40 * time.Minute
	// minRewriteBudget: a rewrite does not start with less of the pass left.
	minRewriteBudget = 5 * time.Minute
	// vacuumStatementTimeout bounds the VACUUM (ANALYZE) after a rewrite.
	vacuumStatementTimeout = 10 * time.Minute
	// catalogStatementTimeout bounds every catalog and progress statement.
	catalogStatementTimeout = 30 * time.Second
	// statementGrace keeps the client's deadline this far past the server's
	// statement_timeout, so the server ends the statement first.
	statementGrace = 30 * time.Second
	// maxAttempts and defaultRetryPause: a lock timeout is retried after a
	// pause, at most this many attempts in one pass.
	maxAttempts       = 3
	defaultRetryPause = 30 * time.Second
)

// compactionLockKey is the advisory lock that admits one pass at a time
// cluster-wide. A fixed key derived from a name, as scoped cron leases derive
// theirs (component/automations/cron_leader.go), in a keyspace nothing else uses.
var compactionLockKey = func() int64 {
	digest := sha256.Sum256([]byte("memql:retention:closed-chunk-compaction"))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}()

// Compactor runs a closed-chunk compaction pass. See the header.
type Compactor struct {
	// Session opens the dedicated maintenance session every statement of a pass
	// runs on (PluginContext.OpenMaintenanceSession). Required.
	Session func(context.Context) (*sql.Conn, func(), error)
	// Storage reads the database-health report; nil or an error means none.
	Storage func(context.Context) (StorageReading, error)
	// VolumeBytes is MEMQL_CHUNK_COMPACTION_VOLUME_BYTES: the data volume's size,
	// used only when there is no fresh report. 0 means unset.
	VolumeBytes int64
	// Tables are the hypertables a pass considers, resolved through the
	// session's search_path.
	Tables []string
	Logger *slog.Logger
	// Now is the pass's clock for every decision and every stored time; nil is
	// time.Now. The closure cutoff is passed to the database from it too.
	Now func() time.Time
	// Attempts and RetryPause override maxAttempts and defaultRetryPause (tests).
	Attempts   int
	RetryPause time.Duration
	// Disabled is the operator's stop switch (MEMQL_CHUNK_COMPACTION_ENABLED,
	// plugin.go), with the reason it reads as disabled. A disabled pass that is
	// not a dry run answers skipped/disabled before it opens a session; a dry run
	// still measures and reports, so the effect of turning it back on can be
	// seen first.
	Disabled       bool
	DisabledReason string
}

// CompactionReport is what a pass did, and why when it did nothing.
type CompactionReport struct {
	DryRun bool `json:"dryRun"`
	// Outcome is compacted, planned (a dry run that would rewrite Chunk),
	// skipped (nothing rewritten; Reason says why) or failed (a rewrite was
	// attempted and did not finish; the chunk is as it was).
	Outcome string `json:"outcome"`
	// Reason, for skipped and failed: disabled, already_running,
	// reorder_unavailable, no_candidate, room_unknown, no_room, not_owner,
	// wal_backlog, wal_unknown, no_reorder_index, out_of_time; lock_timeout,
	// statement_timeout, cancelled, error.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Chunk is the chunk chosen (and, for compacted, what became of it).
	Chunk *ChunkReport `json:"chunk,omitempty"`
	// Closed is how many closed, uncompressed chunks the pass considered,
	// Measured those it (re)measured, MeasureFailures those it could not, and
	// Eligible those over both thresholds.
	Closed          int           `json:"closed"`
	Measured        []ChunkReport `json:"measured,omitempty"`
	MeasureFailures []string      `json:"measureFailures,omitempty"`
	Eligible        int           `json:"eligible"`
	Room            *Room         `json:"room,omitempty"`
	WAL             *WAL          `json:"wal,omitempty"`
	DurationMs      int64         `json:"durationMs"`
}

// ChunkReport is one chunk, as measured and, after a rewrite, as it became.
type ChunkReport struct {
	Chunk            string    `json:"chunk"`
	Hypertable       string    `json:"hypertable"`
	RangeStart       time.Time `json:"rangeStart"`
	RangeEnd         time.Time `json:"rangeEnd"`
	TotalBytes       int64     `json:"totalBytes"`
	IndexBytes       int64     `json:"indexBytes"`
	LiveBytes        int64     `json:"liveBytes"`
	LiveRows         int64     `json:"liveRows"`
	ReclaimableBytes int64     `json:"reclaimableBytes"`
	AfterBytes       int64     `json:"afterBytes,omitempty"`
	ReclaimedBytes   int64     `json:"reclaimedBytes,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`
	DurationMs       int64     `json:"durationMs,omitempty"`
	// Vacuum is "done", or why the VACUUM after a rewrite did not run
	// (autovacuum then sets the visibility map in its own time).
	Vacuum string `json:"vacuum,omitempty"`
}

func reportOf(c *chunk) *ChunkReport {
	r := &ChunkReport{Chunk: c.Name, Hypertable: c.Hypertable, RangeStart: c.RangeStart, RangeEnd: c.RangeEnd, TotalBytes: c.TotalBytes, IndexBytes: c.IndexBytes, ReclaimableBytes: c.reclaimable()}
	if c.m != nil {
		r.LiveBytes, r.LiveRows = c.m.LiveBytes, c.m.LiveRows
	}
	return r
}

func (c *Compactor) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *Compactor) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// Run executes one pass. It returns an error only when the pass could not start
// or could not read the catalog -- nothing was decided and nothing changed. Every
// outcome after that, a failed rewrite included, is the report's.
func (c *Compactor) Run(ctx context.Context, opt CompactOptions) (CompactionReport, error) {
	started := time.Now()
	report := CompactionReport{DryRun: opt.DryRun}
	if c.Disabled && !opt.DryRun {
		return c.finish(&report, started, "skipped", "disabled", c.DisabledReason), nil
	}
	if c.Session == nil {
		return report, errors.New("chunk compaction has no maintenance session")
	}
	ctx, cancel := context.WithTimeout(ctx, runBudget)
	defer cancel()

	conn, closeSession, err := c.Session(ctx)
	if err != nil {
		return report, fmt.Errorf("chunk compaction: %w", err)
	}
	defer closeSession()
	s := &compactSession{conn: conn}

	if err := s.setup(ctx); err != nil {
		return report, fmt.Errorf("chunk compaction: session setup: %w", err)
	}
	locked, err := s.tryLock(ctx)
	if err != nil {
		return report, fmt.Errorf("chunk compaction: %w", err)
	}
	if !locked {
		return c.finish(&report, started, "skipped", "already_running", "another compaction pass holds the cluster-wide lock"), nil
	}
	defer s.unlock()

	tsSchema, license, err := s.timescale(ctx)
	if err != nil {
		return report, fmt.Errorf("chunk compaction: %w", err)
	}
	if tsSchema == "" || license == "apache" {
		return c.finish(&report, started, "skipped", "reorder_unavailable", fmt.Sprintf("reorder_chunk needs TimescaleDB under its Community licence (extension schema %q, licence %q)", tsSchema, license)), nil
	}

	now := c.now()
	chunks, err := s.closedChunks(ctx, c.Tables, now.Add(-closedFor))
	if err != nil {
		return report, fmt.Errorf("chunk compaction: list closed chunks: %w", err)
	}
	if err := s.loadProgress(ctx, chunks); err != nil {
		return report, fmt.Errorf("chunk compaction: read progress: %w", err)
	}
	if !opt.DryRun {
		if err := s.pruneProgress(ctx); err != nil {
			c.logger().Warn("chunk compaction: could not prune progress rows of dropped chunks", "error", err)
		}
	}
	report.Closed = len(chunks)

	// The room is read before measuring. A real pass that cannot see the volume
	// could rewrite nothing whatever it measured, so it stops here rather than
	// scan chunks for nothing every night; a dry run measures anyway, to show
	// what a pass would find once the reading is back.
	room, roomReason, roomDetail, issues := c.room(ctx, s)
	if roomReason != "" && room.Source != "" {
		report.Room = &room
	}
	if roomReason != "" && !opt.DryRun {
		return c.finish(&report, started, "skipped", roomReason, roomDetail), nil
	}

	c.measure(ctx, s, chunks, opt, started, &report)

	candidates := eligible(chunks, opt, c.now())
	report.Eligible = len(candidates)
	if len(candidates) == 0 {
		if roomReason == "" {
			report.Room = &room
		}
		return c.finish(&report, started, "skipped", "no_candidate", fmt.Sprintf(
			"no measured closed chunk would give back at least %d bytes and %d%% of its size", opt.MinReclaimBytes, opt.MinReclaimPercent)), nil
	}
	if roomReason != "" {
		return c.finish(&report, started, "skipped", roomReason, roomDetail), nil
	}
	report.Room = &room
	pick, smallest := choose(candidates, room)
	if pick == nil {
		room.NeedBytes = 2 * smallest.need()
		report.Room = &room
		return c.finish(&report, started, "skipped", "no_room", fmt.Sprintf(
			"the smallest eligible chunk, %s, needs %d bytes (its live data and indexes, twice over); the volume has %d usable bytes free", smallest.Name, room.NeedBytes, room.UsableFreeBytes)), nil
	}
	room.NeedBytes = 2 * pick.need()
	report.Room = &room
	report.Chunk = reportOf(pick)

	if !pick.Owned {
		current, _ := s.currentUser(ctx)
		return c.finish(&report, started, "skipped", "not_owner", fmt.Sprintf(
			"%s is owned by role %q and this session runs as %q; reorder_chunk requires the chunk's owner", pick.Name, pick.Owner, current)), nil
	}
	wal, reason, detail := c.wal(ctx, s, issues)
	report.WAL = &wal
	if reason != "" {
		return c.finish(&report, started, "skipped", reason, detail), nil
	}
	index, err := s.reorderIndex(ctx, pick.Hypertable)
	if err != nil {
		return report, fmt.Errorf("chunk compaction: find the reorder index of %s: %w", pick.Hypertable, err)
	}
	if index == "" {
		return c.finish(&report, started, "skipped", "no_reorder_index", pick.Hypertable+` has no btree index on (id, "createdAt") to order the rewrite by`), nil
	}
	if opt.DryRun {
		return c.finish(&report, started, "planned", "", fmt.Sprintf("would rewrite %s ordered by %s", pick.Name, index)), nil
	}
	return c.rewrite(ctx, s, pick, tsSchema, index, issues, started, &report), nil
}

// measure (re)measures the stale chunks, within the pass's limits. A
// measurement that fails is reported and, outside a dry run, recorded so the
// chunk waits its turn behind the others for a week.
func (c *Compactor) measure(ctx context.Context, s *compactSession, chunks []*chunk, opt CompactOptions, started time.Time, report *CompactionReport) {
	queue := measureQueue(chunks, c.now())
	for n, ch := range queue {
		if n >= opt.MeasureChunks || time.Since(started) > measureBudget || ctx.Err() != nil {
			return
		}
		m, err := s.measure(ctx, ch, c.now())
		if err != nil {
			report.MeasureFailures = append(report.MeasureFailures, ch.Name+": "+err.Error())
			c.logger().Warn("chunk compaction: measuring a closed chunk failed; it is measured again later", "chunk", ch.Name, "kind", failureKind(ctx, err), "error", err)
			if !opt.DryRun {
				if err := s.recordMeasureFailure(ctx, ch, c.now(), err.Error()); err != nil {
					c.logger().Warn("chunk compaction: could not record a failed measurement", "chunk", ch.Name, "error", err)
				}
			}
			continue
		}
		ch.m = m
		if !opt.DryRun {
			if err := s.saveMeasurement(ctx, ch); err != nil {
				c.logger().Warn("chunk compaction: could not record a measurement; the next pass measures again", "chunk", ch.Name, "error", err)
			}
		}
		report.Measured = append(report.Measured, *reportOf(ch))
	}
}

// room reads what judgeRoom needs: the live database sizes, the monitor's
// report and the configured capacity. It answers the report's issues too, for
// the WAL guard.
func (c *Compactor) room(ctx context.Context, s *compactSession) (Room, string, string, []string) {
	in := roomInputs{configured: c.VolumeBytes, now: c.now()}
	used, unreadable, err := s.databaseBytes(ctx)
	if err != nil {
		return Room{}, "room_unknown", "could not read the database sizes: " + err.Error(), nil
	}
	in.used, in.unreadable = used, unreadable
	if c.Storage == nil {
		in.reportErr = "no database-health reader on this node"
	} else if reading, err := c.Storage(ctx); err != nil {
		in.reportErr = err.Error()
	} else {
		in.report = &reading
	}
	room, reason, detail := judgeRoom(in)
	var issues []string
	if fresh(in.report, in.now) {
		issues = in.report.Issues
	}
	return room, reason, detail, issues
}

// wal reads the archive and judges it.
func (c *Compactor) wal(ctx context.Context, s *compactSession, issues []string) (WAL, string, string) {
	state, err := s.walState(ctx)
	if err != nil {
		return WAL{}, "wal_unknown", "could not read the archiver: " + err.Error()
	}
	return judgeWAL(state, issues)
}

// rewrite runs reorder_chunk on pick, then VACUUM (ANALYZE), and records what
// happened. Nothing it does can leave the chunk half-rewritten: reorder_chunk
// swaps a finished copy in or discards it.
func (c *Compactor) rewrite(ctx context.Context, s *compactSession, pick *chunk, tsSchema, index string, issues []string, started time.Time, report *CompactionReport) CompactionReport {
	attempts := c.Attempts
	if attempts <= 0 {
		attempts = maxAttempts
	}
	pause := c.RetryPause
	if pause <= 0 {
		pause = defaultRetryPause
	}
	rewriteStarted := time.Now()
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			if !sleepCtx(ctx, pause) {
				break
			}
			// A copy that reached the swap before it timed out wrote WAL; the
			// next copy waits for the archive like the first one did.
			wal, reason, detail := c.wal(ctx, s, issues)
			report.WAL = &wal
			if reason != "" {
				err = fmt.Errorf("%w; not retried: %s (%s)", err, reason, detail)
				break
			}
		}
		budget := time.Until(deadlineOf(ctx)) - statementGrace
		if budget < minRewriteBudget {
			if attempt == 1 {
				return c.finish(report, started, "skipped", "out_of_time", fmt.Sprintf("%s of the pass is left, less than the %s a rewrite is started with", budget.Round(time.Second), minRewriteBudget))
			}
			break
		}
		report.Chunk.Attempts = attempt
		err = s.reorder(ctx, tsSchema, pick.Name, index, min(rewriteStatementTimeout, budget))
		if err == nil || failureKind(ctx, err) != "lock_timeout" {
			break
		}
		c.logger().Info("chunk compaction: a lock wait ran past lock_timeout; the chunk is intact", "chunk", pick.Name, "attempt", attempt, "maxAttempts", attempts)
	}
	report.Chunk.DurationMs = time.Since(rewriteStarted).Milliseconds()
	if err != nil {
		kind := failureKind(ctx, err)
		c.logger().Warn("chunk compaction: rewrite did not finish; the chunk is as it was", "chunk", pick.Name, "kind", kind, "attempts", report.Chunk.Attempts, "error", err)
		if rerr := s.recordFailure(ctx, pick, c.now(), kind, err.Error()); rerr != nil {
			c.logger().Warn("chunk compaction: could not record the failed rewrite", "chunk", pick.Name, "error", rerr)
		}
		return c.finish(report, started, "failed", kind, err.Error())
	}

	report.Chunk.Vacuum = "done"
	if verr := s.vacuum(ctx, pick.Name); verr != nil {
		report.Chunk.Vacuum = "not run: " + verr.Error()
		c.logger().Warn("chunk compaction: VACUUM after the rewrite did not run; autovacuum will", "chunk", pick.Name, "error", verr)
	}
	total, indexes, changes, err := s.sizes(ctx, pick.Name)
	if err != nil {
		// The rewrite is done and committed; only its accounting is missing.
		c.logger().Warn("chunk compaction: rewrote the chunk but could not read its new size", "chunk", pick.Name, "error", err)
		return c.finish(report, started, "compacted", "", "rewritten; its new size could not be read: "+err.Error())
	}
	report.Chunk.AfterBytes = total
	report.Chunk.ReclaimedBytes = max(0, pick.TotalBytes-total)
	metrics.ChunkCompactionReclaimed(report.Chunk.ReclaimedBytes)
	if err := s.recordSuccess(ctx, pick, c.now(), total, indexes, changes); err != nil {
		c.logger().Warn("chunk compaction: could not record the rewrite; the next pass measures the chunk again", "chunk", pick.Name, "error", err)
	}
	c.logger().Info("chunk compaction: rewrote a closed chunk", "chunk", pick.Name, "hypertable", pick.Hypertable,
		"beforeBytes", pick.TotalBytes, "afterBytes", total, "reclaimedBytes", report.Chunk.ReclaimedBytes,
		"liveBytes", pick.m.LiveBytes, "attempts", report.Chunk.Attempts, "durationMs", report.Chunk.DurationMs)
	return c.finish(report, started, "compacted", "", "")
}

// finish closes a report: outcome, reason, duration, the metric and, for a pass
// that did nothing, the log line that says why.
func (c *Compactor) finish(report *CompactionReport, started time.Time, outcome, reason, detail string) CompactionReport {
	report.Outcome, report.Reason, report.Detail = outcome, reason, detail
	report.DurationMs = time.Since(started).Milliseconds()
	metrics.ChunkCompactionRun(outcome)
	if outcome == "skipped" || outcome == "planned" {
		c.logger().Info("chunk compaction: no chunk rewritten", "outcome", outcome, "reason", reason, "detail", detail, "dryRun", report.DryRun, "measured", len(report.Measured), "eligible", report.Eligible)
	}
	return *report
}

func deadlineOf(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(runBudget)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// compactSession is the pass's statements on its maintenance session.
type compactSession struct {
	conn *sql.Conn
}

// setup sets what holds for the whole session: lock_timeout, so a lock wait --
// the rewrite's swap above all -- ends within a second instead of queueing
// readers behind it; and client_connection_check_interval, so a backend whose
// engine process died stops its rewrite within seconds rather than finishing it
// for nobody (PostgreSQL 14 and later; ignored where unsupported).
func (s *compactSession) setup(ctx context.Context) error {
	if err := s.exec(ctx, catalogStatementTimeout, "SET lock_timeout = '1s'"); err != nil {
		return err
	}
	_ = s.exec(ctx, catalogStatementTimeout, "SET client_connection_check_interval = '10s'")
	return nil
}

// statementContext bounds one statement: the server's statement_timeout is the
// smaller of timeout and what the pass has left less statementGrace, and the
// client's deadline sits statementGrace past it.
func statementContext(ctx context.Context, timeout time.Duration) (context.Context, time.Duration, context.CancelFunc) {
	if d, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(d)-statementGrace)
	}
	if timeout < time.Second {
		timeout = time.Second
	}
	child, cancel := context.WithTimeout(ctx, timeout+statementGrace)
	return child, timeout, cancel
}

func (s *compactSession) setTimeout(ctx context.Context, timeout time.Duration) error {
	_, err := s.conn.ExecContext(ctx, "SET statement_timeout = "+strconv.FormatInt(timeout.Milliseconds(), 10))
	return err
}

func (s *compactSession) exec(ctx context.Context, timeout time.Duration, query string, args ...any) error {
	sctx, server, cancel := statementContext(ctx, timeout)
	defer cancel()
	if err := s.setTimeout(sctx, server); err != nil {
		return err
	}
	_, err := s.conn.ExecContext(sctx, query, args...)
	return err
}

func (s *compactSession) scan(ctx context.Context, timeout time.Duration, query string, args []any, dest ...any) error {
	sctx, server, cancel := statementContext(ctx, timeout)
	defer cancel()
	if err := s.setTimeout(sctx, server); err != nil {
		return err
	}
	return s.conn.QueryRowContext(sctx, query, args...).Scan(dest...)
}

func (s *compactSession) rows(ctx context.Context, timeout time.Duration, query string, args []any, each func(*sql.Rows) error) error {
	sctx, server, cancel := statementContext(ctx, timeout)
	defer cancel()
	if err := s.setTimeout(sctx, server); err != nil {
		return err
	}
	rows, err := s.conn.QueryContext(sctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *compactSession) tryLock(ctx context.Context) (bool, error) {
	var locked bool
	err := s.scan(ctx, catalogStatementTimeout, "SELECT pg_try_advisory_lock($1)", []any{compactionLockKey}, &locked)
	return locked, err
}

// unlock releases the lock; closing the session would too, so a failure here
// costs nothing but the line.
func (s *compactSession) unlock() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", compactionLockKey)
}

func (s *compactSession) currentUser(ctx context.Context) (string, error) {
	var user string
	err := s.scan(ctx, catalogStatementTimeout, "SELECT current_user::text", nil, &user)
	return user, err
}

// timescale answers the extension's schema (quoted, "" when it is not
// installed) and its licence setting.
func (s *compactSession) timescale(ctx context.Context) (string, string, error) {
	var schema, license sql.NullString
	err := s.scan(ctx, catalogStatementTimeout, `SELECT
		(SELECT quote_ident(n.nspname) FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'timescaledb'),
		current_setting('timescaledb.license', true)`, nil, &schema, &license)
	return schema.String, license.String, err
}

// closedChunks lists every closed, uncompressed chunk of the named hypertables
// whose range ended at or before cutoff. A name that is not a hypertable (a plain
// table on an install without TimescaleDB's conversion) contributes nothing.
func (s *compactSession) closedChunks(ctx context.Context, tables []string, cutoff time.Time) ([]*chunk, error) {
	var out []*chunk
	for _, table := range tables {
		err := s.rows(ctx, catalogStatementTimeout, `
			WITH ht AS (
				SELECT n.nspname AS schema_name, c.relname AS table_name
				  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
				 WHERE c.oid = to_regclass(quote_ident($1))
			)
			SELECT format('%I.%I', ch.chunk_schema, ch.chunk_name),
			       format('%I.%I', ch.hypertable_schema, ch.hypertable_name),
			       ch.range_start, ch.range_end,
			       pg_total_relation_size(c.oid), pg_indexes_size(c.oid),
			       st.n_tup_ins + st.n_tup_upd + st.n_tup_del,
			       pg_has_role(c.relowner, 'USAGE'), pg_get_userbyid(c.relowner)::text
			  FROM ht
			  JOIN timescaledb_information.chunks ch
			    ON ch.hypertable_schema = ht.schema_name AND ch.hypertable_name = ht.table_name
			  JOIN pg_namespace cn ON cn.nspname = ch.chunk_schema
			  JOIN pg_class c ON c.relnamespace = cn.oid AND c.relname = ch.chunk_name
			  LEFT JOIN pg_stat_user_tables st ON st.relid = c.oid
			 WHERE NOT ch.is_compressed
			   AND ch.range_end IS NOT NULL
			   AND ch.range_end <= $2
			 ORDER BY ch.range_start`, []any{table, cutoff}, func(rows *sql.Rows) error {
			var c chunk
			if err := rows.Scan(&c.Name, &c.Hypertable, &c.RangeStart, &c.RangeEnd, &c.TotalBytes, &c.IndexBytes, &c.Changes, &c.Owned, &c.Owner); err != nil {
				return err
			}
			out = append(out, &c)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", table, err)
		}
	}
	return out, nil
}

// loadProgress attaches each chunk's stored measurement and last attempt.
func (s *compactSession) loadProgress(ctx context.Context, chunks []*chunk) error {
	byName := make(map[string]*chunk, len(chunks))
	for _, c := range chunks {
		byName[c.Name] = c
	}
	return s.rows(ctx, catalogStatementTimeout, `SELECT chunk, measured_at, measured_total_bytes, measured_index_bytes, measured_changes,
		live_bytes, live_rows, measure_failed_at, attempted_at, outcome, failures FROM chunk_compaction_progress`, nil, func(rows *sql.Rows) error {
		var name, outcome string
		var at, failedAt, attemptedAt sql.NullTime
		var total, indexes, changes, live, liveRows sql.NullInt64
		var failures int
		if err := rows.Scan(&name, &at, &total, &indexes, &changes, &live, &liveRows, &failedAt, &attemptedAt, &outcome, &failures); err != nil {
			return err
		}
		c := byName[name]
		if c == nil {
			return nil
		}
		if at.Valid && total.Valid && indexes.Valid && live.Valid && liveRows.Valid {
			c.m = &measurement{At: at.Time, TotalBytes: total.Int64, IndexBytes: indexes.Int64, Changes: changes, LiveBytes: live.Int64, LiveRows: liveRows.Int64}
		}
		c.MeasureFailedAt, c.AttemptedAt, c.Outcome, c.Failures = failedAt, attemptedAt, outcome, failures
		return nil
	})
}

// pruneProgress forgets chunks that no longer exist.
func (s *compactSession) pruneProgress(ctx context.Context) error {
	return s.exec(ctx, catalogStatementTimeout, "DELETE FROM chunk_compaction_progress WHERE to_regclass(chunk) IS NULL")
}

// measure scans one chunk's heap: per row, the summed pg_column_size of every
// column (a TOASTed value's stored size, read off its pointer, never fetched) and
// tupleOverheadBytes. The column list comes from the catalog, so a column added
// later is counted without a change here.
func (s *compactSession) measure(ctx context.Context, c *chunk, now time.Time) (*measurement, error) {
	var expr sql.NullString
	if err := s.scan(ctx, catalogStatementTimeout, `SELECT string_agg(format('coalesce(pg_column_size(%I), 0)', attname), ' + ' ORDER BY attnum)
		FROM pg_attribute WHERE attrelid = to_regclass($1) AND attnum > 0 AND NOT attisdropped`, []any{c.Name}, &expr); err != nil {
		return nil, err
	}
	if !expr.Valid || expr.String == "" {
		return nil, fmt.Errorf("%s has no columns to measure", c.Name)
	}
	// c.Name and expr are the catalog's own quoting (format %I), not input.
	query := "SELECT count(*), coalesce(sum(" + expr.String + " + " + strconv.Itoa(tupleOverheadBytes) + "), 0)::bigint FROM " + c.Name
	var rows, live int64
	if err := s.scan(ctx, measureStatementTimeout, query, nil, &rows, &live); err != nil {
		return nil, err
	}
	return &measurement{At: now, TotalBytes: c.TotalBytes, IndexBytes: c.IndexBytes, Changes: c.Changes, LiveBytes: live, LiveRows: rows}, nil
}

func (s *compactSession) saveMeasurement(ctx context.Context, c *chunk) error {
	return s.exec(ctx, catalogStatementTimeout, `INSERT INTO chunk_compaction_progress
		(chunk, hypertable, measured_at, measured_total_bytes, measured_index_bytes, measured_changes, live_bytes, live_rows, measure_failed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULL)
		ON CONFLICT (chunk) DO UPDATE SET hypertable = EXCLUDED.hypertable, measured_at = EXCLUDED.measured_at,
			measured_total_bytes = EXCLUDED.measured_total_bytes, measured_index_bytes = EXCLUDED.measured_index_bytes,
			measured_changes = EXCLUDED.measured_changes, live_bytes = EXCLUDED.live_bytes, live_rows = EXCLUDED.live_rows,
			measure_failed_at = NULL`,
		c.Name, c.Hypertable, c.m.At, c.m.TotalBytes, c.m.IndexBytes, c.m.Changes, c.m.LiveBytes, c.m.LiveRows)
}

func (s *compactSession) recordMeasureFailure(ctx context.Context, c *chunk, now time.Time, detail string) error {
	return s.exec(ctx, catalogStatementTimeout, `INSERT INTO chunk_compaction_progress (chunk, hypertable, measure_failed_at, detail)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (chunk) DO UPDATE SET measure_failed_at = EXCLUDED.measure_failed_at, detail = EXCLUDED.detail`,
		c.Name, c.Hypertable, now, truncate(detail))
}

func (s *compactSession) recordFailure(ctx context.Context, c *chunk, now time.Time, kind, detail string) error {
	return s.exec(ctx, catalogStatementTimeout, `UPDATE chunk_compaction_progress
		SET attempted_at = $2, outcome = $3, detail = $4, failures = failures + 1 WHERE chunk = $1`,
		c.Name, now, kind, truncate(detail))
}

// recordSuccess stores the rewritten chunk as its own new measurement: its data
// as it now stands IS what it would rewrite to, so it is not eligible again until
// something changes it.
func (s *compactSession) recordSuccess(ctx context.Context, c *chunk, now time.Time, total, indexes int64, changes sql.NullInt64) error {
	return s.exec(ctx, catalogStatementTimeout, `UPDATE chunk_compaction_progress
		SET attempted_at = $2, outcome = 'compacted', detail = '', failures = 0, compacted_at = $2,
		    measured_at = $2, measured_total_bytes = $3, measured_index_bytes = $4, measured_changes = $5, live_bytes = $6
		WHERE chunk = $1`,
		c.Name, now, total, indexes, changes, total-indexes)
}

// databaseBytes is the summed size of every database this role may connect to,
// the figure the monitor sums, and how many it may not.
func (s *compactSession) databaseBytes(ctx context.Context) (int64, int, error) {
	var used int64
	var unreadable int
	err := s.scan(ctx, catalogStatementTimeout, `SELECT
		coalesce(sum(CASE WHEN has_database_privilege(oid, 'CONNECT') THEN pg_database_size(oid) END), 0)::bigint,
		count(*) FILTER (WHERE NOT has_database_privilege(oid, 'CONNECT'))
		FROM pg_database WHERE datallowconn`, nil, &used, &unreadable)
	return used, unreadable, err
}

// walState reads the archiver and the WAL position: views and functions every
// role may read (pg_ls_archive_statusdir needs pg_monitor, which the engine's
// role is not given).
func (s *compactSession) walState(ctx context.Context) (walState, error) {
	var w walState
	err := s.scan(ctx, catalogStatementTimeout, `SELECT current_setting('archive_mode'),
		(SELECT setting::bigint FROM pg_settings WHERE name = 'wal_segment_size'),
		pg_walfile_name(pg_current_wal_lsn()),
		a.last_archived_wal, a.last_archived_time, a.last_failed_time, now()
		FROM pg_stat_archiver a`, nil, &w.ArchiveMode, &w.SegmentBytes, &w.CurrentWAL, &w.LastArchivedWAL, &w.LastArchivedAt, &w.LastFailedAt, &w.DBNow)
	return w, err
}

// reorderIndex names the hypertable's btree index on exactly (id, "createdAt"),
// preferring "createdAt" DESC and then the primary key: rewritten in that order,
// a record's versions sit together, and the PK covers an install where the
// DESC index is missing. reorder_chunk maps a hypertable index to the chunk's.
func (s *compactSession) reorderIndex(ctx context.Context, hypertable string) (string, error) {
	var index sql.NullString
	err := s.scan(ctx, catalogStatementTimeout, `SELECT format('%I.%I', n.nspname, ic.relname)
		  FROM pg_index i
		  JOIN pg_class ic ON ic.oid = i.indexrelid
		  JOIN pg_namespace n ON n.oid = ic.relnamespace
		  JOIN pg_am am ON am.oid = ic.relam
		 WHERE i.indrelid = to_regclass($1)
		   AND am.amname = 'btree'
		   AND i.indpred IS NULL AND i.indexprs IS NULL
		   AND i.indnkeyatts = 2
		   AND i.indkey[0] = (SELECT attnum FROM pg_attribute WHERE attrelid = i.indrelid AND attname = 'id')
		   AND i.indkey[1] = (SELECT attnum FROM pg_attribute WHERE attrelid = i.indrelid AND attname = 'createdAt')
		 ORDER BY (i.indoption[1] & 1) DESC, i.indisprimary DESC, ic.relname
		 LIMIT 1`, []any{hypertable}, &index)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return index.String, err
}

// reorder rewrites the chunk. The function is named in the extension's schema,
// so the session's search_path does not decide which one runs.
func (s *compactSession) reorder(ctx context.Context, tsSchema, chunkName, index string, timeout time.Duration) error {
	return s.exec(ctx, timeout, "SELECT "+tsSchema+".reorder_chunk($1::regclass, $2::regclass)", chunkName, index)
}

func (s *compactSession) vacuum(ctx context.Context, chunkName string) error {
	return s.exec(ctx, vacuumStatementTimeout, "VACUUM (ANALYZE) "+chunkName)
}

func (s *compactSession) sizes(ctx context.Context, chunkName string) (int64, int64, sql.NullInt64, error) {
	var total, indexes int64
	var changes sql.NullInt64
	err := s.scan(ctx, catalogStatementTimeout, `SELECT pg_total_relation_size(to_regclass($1)), pg_indexes_size(to_regclass($1)),
		(SELECT n_tup_ins + n_tup_upd + n_tup_del FROM pg_stat_user_tables WHERE relid = to_regclass($1))`,
		[]any{chunkName}, &total, &indexes, &changes)
	return total, indexes, changes, err
}

// truncate keeps a stored error detail to one line of reasonable length.
func truncate(detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	if len(detail) > 500 {
		return detail[:500]
	}
	return detail
}
