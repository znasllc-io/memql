package sitetraffic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/schema"
)

// The statistics rollup: folds the edge's request log into a deployable's
// hourly and daily website statistics (docs/superpowers/specs/
// 2026-11-08-site-and-email-statistics-design.md, section 5, D6 and D17).
// builtin siteStatisticsRollup runs it every five minutes on the cron leader,
// under its own maintenance principal (integration.go, handleRollup).
//
// # Why a scheduled job and not a continuous aggregate (D6)
//
// The two figures that matter most cannot be kept by a continuous aggregate.
// Visitors are COUNT(DISTINCT visitor), which a TimescaleDB continuous
// aggregate cannot maintain, and a visit is a window over each visitor's page
// views. So the rollup keeps its own state of each open day, per visitor, and
// folds the request log into it (fold.go).
//
// # Incremental, with a two-hour tail
//
// Until 2026-11-09 every run recomputed today, and yesterday until 02:00, from
// the whole request log. Measured at ten million requests a day over fifty
// sites, on a database sized like the mid preset (2 CPUs, 8 GiB, 2 GB shared
// buffers, 8 MB work_mem): each of a day's three statements took 47 to 52 s,
// against the 60 s bound below, and a run spent two and a half minutes of
// database time every five -- twice that before 02:00 (the measurement:
// TestRollupAtScaleEvidence, rollup_scale_test.go).
//
// Now a request is read into its day's figures in two ways:
//
//   - SETTLED, once. A run folds every request of an open day served more than
//     finalAfter (two hours) before its clock into the day's settled state
//     (settle, fold.go) and moves the day's progress past it
//     (edge_stats_progress); no later run reads it again. The state is the
//     day's figures per hour and per value, and one row per visitor
//     (migrations 20261109090100).
//   - THE TAIL, every run. The requests after the day's progress -- the last
//     two hours -- are folded against the settled state again on every run
//     (publish.go), and what the run publishes is the settled state with the
//     tail added. Nothing of the tail is kept.
//
// So a run reads the last two hours of requests, whatever the day's size, and
// what a run writes is the whole of the day as it stands: running twice
// changes nothing, and a run that dies part way loses nothing but its own
// work. The two hours are the late-request horizon the recompute had: a
// request written within two hours of being served still counts, as it did
// until its day's 02:00, and one written later than that does not
// (TestARequestIsFoldedOnceAndOnlyTheLastTwoHoursAreReadAgain). The edge's
// sink writes a request within seconds or drops it (sink.go), so the horizon
// is a margin, not a feature: what it must outlast is a replica whose clock
// lags the cron leader's.
//
// The settled fold reads its range in windows of at most foldRows requests
// (foldEnd), each its own transaction with its own progress, so a day of any
// size is folded in statements of a bounded size, and a day that does not
// finish in one run's budget carries on from its progress in the next.
//
// # When a day is final
//
// A day is FINAL when its settled fold reaches its end: at or after 02:00 UTC
// the day after, when the horizon has passed it. The window that reaches its
// end publishes it from the settled state alone, records it in
// edge_stats_final and deletes its state, in one transaction (finishDay);
// no run touches the day again. Today is never final.
//
// So which days a run reads is read off the record, not off the clock: today,
// and every day of the request log's retention window (the configured
// MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS, capped at 400; see planRollup)
// through yesterday that has no record. That one rule does three jobs.
// Yesterday is read until the first run at or after 02:00 finalizes it. On the
// first run nothing is final, so every day of the window is folded: the
// backfill. And an outage of any length -- the cron leader down, every run
// failing -- leaves its days open, so the next run that succeeds folds them,
// with every request that arrived meanwhile. The bound is the request log's
// retention: a day whose requests are gone cannot be folded and keeps the
// figures last written for it
// (TestAnOutageAcrossTwoAMHealsOnTheNextRun, TestAFinalDayIsNotRecomputed).
//
// A record lives as long as its daily row, 400 days, and so a final day stays
// final while its figures are kept. That matters once the request log has
// dropped the day: the day comes back into the window if the retention is
// raised again, and a request dated on it can still turn up -- an edge replica
// whose clock lags, rows restored to the log. A day without a record would
// then be reopened and folded from what is left of it
// (TestAStrayRawRowCannotReopenAFinalDay). A day that never became final
// before its requests aged out has no record to protect it; its figures were
// short of the day already.
//
// ONE EXCEPTION, for the 24-hour read: a final day of the last two that was
// published before the daily rows carried each visitor's first and last hour
// -- by the recompute this replaced -- is folded once more from its requests,
// so the 24-hour window that reaches into it can count its visitors
// (planRollup). Only while the request log holds the day: folded without its
// requests, a day would be published empty.
//
// # Why every expression names UTC
//
// date_trunc on a timestamptz truncates in the SESSION time zone, and a cast to
// date does the same. Under a +05:30 session the same request would land in an
// hour that starts at :30 and, late in the evening, on the next day. Days are
// UTC (D5), so every bucket is computed `AT TIME ZONE 'UTC'`, and every bound
// is passed in as an instant or a 'YYYY-MM-DD' date: nothing depends on the
// connection's setting (TestDaysAndHoursAreUTCWhateverTheSessionTimeZone).
//
// # One statement per step, over every site
//
// Each statement covers all sites at once; there is no per-site loop. The bound
// is the statement timeout, which the server enforces (below): a step whose
// statement does not finish within it fails, is logged by the caller, and is
// tried again on the next run, and a run as a whole stops at rollupRunBudget.
//
// # Where the statements run: one maintenance session per run
//
// Not on the request pool. Its connections carry a server statement_timeout (8 s
// by default) and pgdriver's 10 s CLIENT read deadline, past which the client
// reports an i/o timeout and sends no cancel, so the backend carries on unheard
// (component/database, maintenance_session.go). A day's statement folds a whole
// UTC day of edge_request for every site, and on a busy cluster it can take
// longer than 10 s: on the pool such a day failed at the client on every run,
// never written and never final, while its orphan kept working up to the
// server's limit with the day's locks still held.
//
// So every statement of a run goes to the dedicated maintenance session
// (memql.PluginContext.OpenMaintenanceSession), which has no statement_timeout
// and no read deadline of its own, and the run supplies both, per statement
// (rollupTx.prepare): the server's statement_timeout is rollupStatementTimeout,
// or less when the run's budget has less left, and the client waits
// rollupStatementGrace past it. An overrun is then ended by the SERVER, which
// rolls the statement back and says so, never by the client cutting the socket
// (TestADayStatementOutlivesTheRequestPoolsReadDeadline).
//
// The session is opened when a run starts and closed when it ends, so an idle
// process holds no connection for it; a day that fails closes it, and the next
// day opens another (rollupSession.inTx says why). There is no fallback to the
// request pool: without a maintenance session the rollup refuses to run
// (errNoMaintenanceSession). docs/public/operate/db-connection-budget.md counts
// the connection.

// RollupReport says what one run of the rollup did.
type RollupReport struct {
	// Hours is how many hour periods were published: for every published
	// day, its hours from the first one the hourly table still keeps through
	// the one under way.
	Hours int `json:"hours"`
	// Days is how many UTC days were published, today among them.
	Days int `json:"days"`
	// Backfilled is true when the run read a day before yesterday: every
	// logged day on the first run, or the days an outage or a failure left
	// without a finalization record.
	Backfilled bool `json:"backfilled"`
	// Deleted is how many statistics rows retention removed -- hourly, daily
	// and top rows. The finalization records and the state it removes are
	// not counted.
	Deleted int64 `json:"deleted"`
}

const (
	// rollupStatementTimeout bounds every statement of a run at the server,
	// which cancels an overrun and rolls it back: the rollup's bound on the work
	// of one step (see "One statement per step" above).
	rollupStatementTimeout = 60 * time.Second

	// rollupStatementGrace is how long past the server's bound the client keeps
	// waiting for a statement, so the server always ends an overrun first. The
	// client's own deadline is for a connection that has gone silent.
	rollupStatementGrace = 15 * time.Second

	// rollupRunBudget bounds a whole run. Long enough that today, which every
	// run reads first -- after an open yesterday, at most a day's folding --
	// always has room; short enough that a long backfill does not hold today's
	// figures back for hours behind the per-process single flight
	// (handleRollup). A run that reaches it stops between statements, and the
	// days it left open are the next run's.
	rollupRunBudget = 15 * time.Minute

	// hourlyRetention is how long edge_stats_hourly keeps a row: the request
	// log's own default, so the last thirty days' hours can always be folded.
	hourlyRetention = 30 * 24 * time.Hour

	// dailyRetentionDays is how long edge_stats_daily and edge_stats_daily_top
	// keep a row: thirteen months of daily totals (D4).
	dailyRetentionDays = 400

	// finalAfter is the late-request horizon: a request is folded into its
	// day's settled state once it is this old, and a day is final once the
	// horizon has passed its end, at 02:00 UTC the day after.
	finalAfter = 2 * time.Hour

	// topValuesKept is how many values a day's top list keeps per dimension
	// before the rest fold into topOther.
	topValuesKept = 100

	topOther   = "(other)"
	topUnknown = "(unknown)"
)

// foldRows is the most requests one settled window folds: what bounds the
// statements of a backfill or a catch-up whatever a day holds. A variable so
// a test can fold a few requests across many windows.
var foldRows = 400_000

// Rollup brings the statistics tables up to date with edge_request as of now.
//
// It reads today -- after yesterday, while yesterday is open -- then every day
// from the oldest one still in the request log through yesterday that is not
// yet final, newest first. Each day's settled
// fold is carried to the horizon, window by window; a day whose fold reaches
// its end is published final, and a day still open to late requests is
// published with its tail. Today's publication also writes the 24-hour
// window's top lists. Then it deletes the rows past retention, and folds each
// month whose final days changed into its monthly top lists.
//
// Every transaction is its own: a day that fails costs only that day -- it is
// not recorded final, so the next run carries it on from its progress -- and
// this run carries on with every other open day. Each failed day is a
// *dayFailure in the returned error, joined with any other failure, so the
// caller can log each by name.
//
// Every statement runs on a session that open opens, and on nothing else (see
// "Where the statements run" above); with no open at all, the run refuses.
func Rollup(ctx context.Context, open MaintenanceSession, now time.Time) (RollupReport, error) {
	var rep RollupReport
	if open == nil {
		return rep, errNoMaintenanceSession
	}
	// THE RUN'S CLOCK IS THE DATABASE'S PRECISION. A Go clock carries
	// nanoseconds and a timestamptz keeps microseconds, rounding to the
	// nearest, and the settled fold stores the horizon it reaches (now less
	// finalAfter) as the day's progress and reads it back to compare with the
	// same horizon. Kept to the nanosecond, a horizon whose nanoseconds rounded
	// down came back a fraction of a microsecond short of itself: every pass
	// read the day as not caught up, folded the same empty sliver again and
	// never published, until the run's budget ran out (0.31.28 in production,
	// TestARunWhoseClockIsFinerThanTheDatabasesStillPublishes). Truncated
	// once, here, every instant the run derives from its clock is one the
	// database stores exactly.
	now = now.UTC().Truncate(time.Microsecond)
	ctx, cancel := context.WithTimeout(ctx, rollupRunBudget)
	defer cancel()

	// Opened before anything is planned: a run that cannot open its session
	// can do nothing, and says so once rather than once for every day.
	s := &rollupSession{open: open}
	defer s.release()
	if err := s.connect(ctx); err != nil {
		return rep, fmt.Errorf("site statistics rollup: %w", err)
	}

	// A plan whose reads fail still has today to do: planRollup answers it
	// whatever its reads said.
	var failures []error
	days, err := planRollup(ctx, s, now)
	if err != nil {
		failures = append(failures, fmt.Errorf("site statistics plan: %w", err))
	}
	for _, d := range days {
		// A run that is cancelled, or has too little of its budget left to
		// start a statement, stops: every day after this one would fail the
		// same way, and none of them is lost, since none is recorded final.
		if _, err := statementBound(ctx); err != nil {
			failures = append(failures, err)
			break
		}
		r, err := rollupDayAt(ctx, s, d, now)
		if r.read && d.backfill {
			rep.Backfilled = true
		}
		if r.published {
			rep.Days++
			rep.Hours += r.hours
		}
		if err != nil {
			// Carry on with the other days. They are independent of each
			// other, so one that fails -- a statement timeout, a row the
			// database refuses -- must not hold back the rest, older ones
			// included. The run's length stays bounded by the statement
			// timeout per statement and by rollupRunBudget, and the
			// per-process single flight in handleRollup keeps a slow run from
			// overlapping the next.
			failures = append(failures, &dayFailure{Day: d.day, Err: err})
		}
	}

	deleted, err := deletePastRetention(ctx, s, now)
	if err != nil {
		failures = append(failures, fmt.Errorf("site statistics retention: %w", err))
	}
	rep.Deleted = deleted
	if err := foldMonths(ctx, s, now); err != nil {
		failures = append(failures, fmt.Errorf("site statistics months: %w", err))
	}
	return rep, errors.Join(failures...)
}

// dayFailure is one day a run could not write. Rollup returns one per failed
// day, joined into its error, so a caller can name each day it logs.
type dayFailure struct {
	// Day is the UTC day, at midnight.
	Day time.Time
	Err error
}

func (f *dayFailure) Error() string {
	return fmt.Sprintf("site statistics for %s: %v", f.Day.Format(time.DateOnly), f.Err)
}

func (f *dayFailure) Unwrap() error { return f.Err }

// rollupDay is one UTC day a run reads.
type rollupDay struct {
	day time.Time
	// backfill marks a day before yesterday: history on a first run, or a day
	// an outage or a failure left open.
	backfill bool
	// refold marks a final day the run folds again: see "ONE EXCEPTION" above.
	refold bool
}

// planRollup lists the days to read: today, then every day of the request
// log's retention window through yesterday that has no finalization record,
// newest first -- an open yesterday before today (below).
//
// THE WINDOW IS THE CONFIGURED RETENTION (openFrom, RetentionDays), not the
// oldest row the log holds. The oldest rows sit in a compressed chunk -- the
// log compresses every chunk older than a day -- so asking for the first of them
// would decompress it on every run, every five minutes. The configuration says
// the same thing for free: the log keeps exactly that many days. A day in the
// window with no request folds to nothing, in an empty range, and is recorded
// final like any other, so it is asked once.
//
// RetentionDays reads MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS on whichever node
// runs the rollup, while the edge applies the same variable to the log's
// retention policy; the window and the policy agree only if both nodes see
// the same value. A window wider than the policy reaches days the log has
// dropped; it folds them to nothing only if they are not final, which a day
// is the morning after it ends. A narrower one leaves the older days of an
// outage open beyond its reach.
//
// It also lists, as refold, the final days of the last two whose measured
// daily rows carry no first- and last-hour visitors: see "ONE EXCEPTION"
// above. Today is answered even when the plan's reads fail, with the error
// beside it.
func planRollup(ctx context.Context, s *rollupSession, now time.Time) ([]rollupDay, error) {
	today := utcMidnight(now)
	days := []rollupDay{{day: today}}
	from := openFrom(today, RetentionDays())
	final := map[string]bool{} // by date: time.Time's == also compares location and monotonic reading
	refold := map[string]bool{}
	err := s.inTx(ctx, "BEGIN", func(tx *rollupTx) error {
		recorded, err := tx.column(ctx, `SELECT day::text FROM edge_stats_final WHERE day >= ?::date AND day < ?::date`,
			dateArg(from), dateArg(today))
		if err != nil {
			return fmt.Errorf("read the finalization record: %w", err)
		}
		for _, d := range recorded {
			final[d] = true
		}
		// Only a day whose requests the log still holds: folded again
		// without them, a day would be published empty, its figures wiped
		// (TestAClosedDayIsFoldedAgainOnlyFromItsRequests).
		stale, err := tx.column(ctx, `SELECT DISTINCT d.day::text FROM edge_stats_daily d
			WHERE d.day >= ?::date AND d.day < ?::date AND d.visitors IS NOT NULL AND d.first_hour_visitors IS NULL
			  AND EXISTS (SELECT 1 FROM edge_request r
			               WHERE r.served_at >= d.day::timestamp AT TIME ZONE 'UTC'
			                 AND r.served_at < (d.day + 1)::timestamp AT TIME ZONE 'UTC')`,
			dateArg(today.AddDate(0, 0, -2)), dateArg(today))
		if err != nil {
			return fmt.Errorf("read the days published without their hours' visitors: %w", err)
		}
		for _, d := range stale {
			refold[d] = true
		}
		return nil
	})
	if err != nil {
		return days, err
	}
	yesterday := today.AddDate(0, 0, -1)
	for d := yesterday; !d.Before(from); d = d.AddDate(0, 0, -1) {
		date := dateArg(d)
		if final[date] && !refold[date] {
			continue
		}
		days = append(days, rollupDay{day: d, backfill: d.Before(yesterday), refold: final[date]})
	}
	// An open yesterday is read BEFORE today: today's publication writes the
	// 24-hour window's top lists, whose first part is yesterday's, and a
	// yesterday not yet folded -- on a first run, after an outage -- would
	// leave it out until the next run. Open, yesterday is two hours of
	// folding at most, until 02:00; past an outage, one day's.
	if len(days) > 1 && days[1].day.Equal(yesterday) {
		days[0], days[1] = days[1], days[0]
	}
	return days, nil
}

// dayResult is what reading one day did.
type dayResult struct {
	// read is true when the run folded some of the day or published it.
	read bool
	// published is true when the day's rows were written; hours is how many
	// hour periods.
	published bool
	hours     int
}

// rollupDayAt reads one day: it carries the day's settled fold to the
// horizon, one window per transaction, publishing the day final in the window
// that reaches its end; and a day the horizon has not passed -- today, or
// yesterday before 02:00 -- it then publishes with its tail. A day whose fold
// is not carried to the horizon in this run, its budget spent, is not
// published: the next run carries it on.
//
// ONE WRITER. Two runs can overlap -- a leader change mid-run, a run slower
// than the schedule on another replica -- and both would fold the same
// requests. Every transaction takes the rollup's table lock before it reads
// anything, and reads the day's progress and record under it, so the run
// that waited folds from where the other left off, and finds a day the other
// closed closed (TestTwoRollupsAtOnceBothFinish). SHARE ROW EXCLUSIVE
// conflicts with itself and with writers, never with a reader, so the
// Statistics page is not held up by a run.
func rollupDayAt(ctx context.Context, s *rollupSession, d rollupDay, now time.Time) (dayResult, error) {
	var res dayResult
	day := d.day
	dayEnd := day.AddDate(0, 0, 1)
	settleTo := now.Add(-finalAfter)
	if settleTo.After(dayEnd) {
		settleTo = dayEnd
	}
	for {
		if _, err := statementBound(ctx); err != nil {
			return res, err
		}
		var done bool
		err := s.inTx(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ", func(tx *rollupTx) error {
			st, err := openDay(ctx, tx, d)
			if err != nil || st.closed {
				done = true
				return err
			}
			if !st.progress.Before(settleTo) {
				done = true
				return nil
			}
			end, err := foldEnd(ctx, tx, st.progress, settleTo)
			if err != nil {
				return err
			}
			ft := foldTablesFor("s")
			if err := fold(ctx, tx, ft, day, st.progress, end); err != nil {
				return err
			}
			if err := settle(ctx, tx, ft, day, end); err != nil {
				return err
			}
			// THE PROGRESS MUST LAND WHERE THE PASS MOVED IT. The next pass
			// starts from what the day's progress reads back as; one that
			// read back short of end -- a clock finer than the database's
			// did, see Rollup -- would fold from there again, spinning to the
			// budget over an empty sliver and counting a range twice over a
			// full one. So the pass is rolled back and the day fails by name,
			// and the run moves on
			// (TestAFoldWhoseProgressDoesNotLandFailsTheDayPromptly).
			landed, err := dayProgress(ctx, tx, day)
			if err != nil {
				return err
			}
			if !landed.progress.Equal(end) {
				return fmt.Errorf("the day's progress reads back as %s after the fold moved it to %s, so the fold is not kept: folding again from where it reads would repeat it",
					landed.progress.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
			}
			res.read = true
			if !end.Before(dayEnd) {
				// The window that reaches the day's end closes it.
				hours, err := finishDay(ctx, tx, day, now)
				if err != nil {
					return err
				}
				res.published, res.hours, done = true, hours, true
			}
			return nil
		})
		if err != nil {
			return res, err
		}
		if done {
			break
		}
	}
	if res.published || !settleTo.Before(dayEnd) {
		return res, nil
	}
	err := s.inTx(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ", func(tx *rollupTx) error {
		st, err := openDay(ctx, tx, d)
		if err != nil || st.closed || st.progress.Before(settleTo) {
			return err
		}
		tails := map[string]*foldTables{}
		ft := foldTablesFor("t")
		if err := fold(ctx, tx, ft, day, st.progress, dayEnd); err != nil {
			return err
		}
		tails[dateArg(day)] = &ft
		hours, err := publishDay(ctx, tx, day, now, &ft)
		if err != nil {
			return err
		}
		res.read, res.published, res.hours = true, true, hours
		if !day.Equal(utcMidnight(now)) {
			return nil
		}
		// Today's publication writes the 24-hour window's top lists too,
		// from today with its tail and from yesterday, which until 02:00 has
		// a tail of its own: the requests after its progress, two hours and
		// a run's worth. A yesterday whose fold is further behind than that
		// -- not yet folded at all, on the first run after this rollup came
		// to be -- is left at its settled part for this run, rather than
		// folding a day inside today's transaction; its own reading, later
		// in the run, catches it up for the next.
		yesterday := day.AddDate(0, 0, -1)
		if y, err := dayProgress(ctx, tx, yesterday); err != nil {
			return err
		} else if y.open && day.Sub(y.progress) <= finalAfter+time.Hour {
			yt := foldTablesFor("y")
			if err := fold(ctx, tx, yt, yesterday, y.progress, day); err != nil {
				return err
			}
			tails[dateArg(yesterday)] = &yt
		}
		return publishWindow(ctx, tx, now, tails)
	})
	return res, err
}

// dayState is where one day stands, read under the rollup's lock.
type dayState struct {
	// closed is true when the day is final and the run has nothing to do.
	closed bool
	// progress is how far the day's settled fold has read: its midnight
	// when it has not begun.
	progress time.Time
}

// openDay takes the rollup's lock and reads where d stands. The two locks are
// taken BEFORE the first query on purpose: a repeatable-read snapshot is
// fixed by the first query, not by BEGIN or a lock.
//
//   - The rollup's own lock serializes the rollup's writers (rollupDayAt,
//     "ONE WRITER"), so the run that waited sees what the other committed.
//   - Hold the request log still while this day is read. TimescaleDB
//     compresses a chunk under an ACCESS EXCLUSIVE lock, moving its rows and
//     truncating the chunk, and a truncate is not MVCC-safe: what a snapshot
//     fixed before that commit reads afterwards depends on how the moved rows
//     were written. On the CI image (TimescaleDB 2.29) a first compression's
//     rows stay visible to it; nothing promises that of a recompression or
//     another version, and a range read as empty would be folded empty and
//     recorded settled. ACCESS SHARE on the hypertable locks every chunk too,
//     and it is taken here, before the first query fixes the snapshot, so the
//     snapshot always comes after any compression or retention drop that
//     already holds a chunk. It does not conflict with the edge's inserts (ROW
//     EXCLUSIVE). The cost runs the other way: a compression or a drop that
//     reaches its exclusive step while this transaction holds the lock waits
//     for it to end, which the statement bound limits to the statements of one
//     window, each ended by the server at rollupStatementTimeout
//     (TestTheSnapshotIsTakenAfterALockOnTheDaysChunk).
//
// A day is closed when it is final, unless the run folds it again (refold)
// and it still lacks what the fold adds (refoldStale).
func openDay(ctx context.Context, tx *rollupTx, d rollupDay) (dayState, error) {
	if _, err := tx.exec(ctx, "LOCK TABLE edge_stats_daily IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return dayState{}, fmt.Errorf("take the rollup lock: %w", err)
	}
	if _, err := tx.exec(ctx, "LOCK TABLE edge_request IN ACCESS SHARE MODE"); err != nil {
		return dayState{}, fmt.Errorf("hold the request log: %w", err)
	}
	p, err := dayProgress(ctx, tx, d.day)
	if err != nil {
		return dayState{}, err
	}
	if !p.open {
		if !d.refold {
			return dayState{closed: true}, nil
		}
		stale, err := tx.column(ctx, `SELECT 1 FROM edge_stats_daily
			WHERE day = ?::date AND visitors IS NOT NULL AND first_hour_visitors IS NULL LIMIT 1`, dateArg(d.day))
		if err != nil {
			return dayState{}, fmt.Errorf("read whether the day still lacks its hours' visitors: %w", err)
		}
		if len(stale) == 0 && p.progress.Equal(d.day) {
			return dayState{closed: true}, nil
		}
	}
	return dayState{progress: p.progress}, nil
}

// progress is one day's record: open is false once it is final, and progress
// is how far its settled fold has read, its midnight when it has not begun.
type progress struct {
	open     bool
	progress time.Time
}

// dayProgress reads one day's record and progress, rendered in UTC whatever
// the session's time zone.
func dayProgress(ctx context.Context, tx *rollupTx, day time.Time) (progress, error) {
	rows, err := tx.column(ctx, `SELECT coalesce((SELECT 'final' FROM edge_stats_final WHERE day = ?::date), 'open') || ' ' ||
		coalesce((SELECT to_char(settled_through AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
		            FROM edge_stats_progress WHERE day = ?::date), '')`, dateArg(day), dateArg(day))
	if err != nil {
		return progress{}, fmt.Errorf("read the day's progress: %w", err)
	}
	if len(rows) != 1 {
		return progress{}, fmt.Errorf("read the day's progress: %d rows", len(rows))
	}
	state, at, _ := strings.Cut(rows[0], " ")
	p := progress{open: state == "open", progress: day}
	if at != "" {
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return progress{}, fmt.Errorf("read the day's progress %q: %w", at, err)
		}
		p.progress = t.UTC()
	}
	return p, nil
}

// foldEnd is where a settled window that starts at from ends: at the
// foldRows-th request after from, or at to when fewer are left. The probe
// walks the request log's (served_at, id) index, so it costs the window it
// measures.
func foldEnd(ctx context.Context, tx *rollupTx, from, to time.Time) (time.Time, error) {
	rows, err := tx.column(ctx, `SELECT to_char(served_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
		FROM edge_request WHERE served_at >= ? AND served_at < ? ORDER BY served_at LIMIT 1 OFFSET ?`,
		from.UTC(), to.UTC(), foldRows)
	if err != nil {
		return time.Time{}, fmt.Errorf("measure the next window: %w", err)
	}
	if len(rows) == 0 {
		return to, nil
	}
	end, err := time.Parse(time.RFC3339Nano, rows[0])
	if err != nil {
		return time.Time{}, fmt.Errorf("measure the next window %q: %w", rows[0], err)
	}
	// More requests than a window holds share from's microsecond: they are
	// folded together, rather than not at all.
	if !end.After(from) {
		end = from.Add(time.Microsecond)
	}
	return end, nil
}

// deletePastRetention removes hourly rows older than now-30d and daily and top
// rows older than today-400d, and answers how many went; and with them what
// the rollup keeps beside them that has aged out. It takes the rollup lock
// like a day's transaction, so it never deletes under a day being written.
//
// A day's finalization record goes WITH ITS DAILY ROW, after the same 400 days,
// and not when the request log loses the day. Kept that long, a final day stays
// final for as long as its figures are kept: a request dated on it that turns
// up later -- an edge replica whose clock lags past the log's retention, or
// rows restored to the log -- cannot reopen the day and rewrite its figures
// from the few rows there (TestAStrayRawRowCannotReopenAFinalDay). The cost is
// at most one small row per day the daily table keeps. The records are not
// counted in the answer.
//
// THE STATE: a final day's value hours go once the 24-hour window cannot reach
// the day, the day before yesterday; and an open day's state goes once the day
// has left the window a run reads (a retention lowered under an outage), since
// no run will fold it again.
//
// THE MONTHS: a month wholly before the cutoff goes. The month the cutoff
// falls in loses days, so its record of the days it covers goes whole when
// one of them is past the cutoff, and foldMonths folds the month again from
// the days it has left.
func deletePastRetention(ctx context.Context, s *rollupSession, now time.Time) (int64, error) {
	var deleted int64
	today := utcMidnight(now)
	cutoff := dateArg(dailyCutoff(today))
	open := dateArg(openFrom(today, RetentionDays()))
	err := s.inTx(ctx, "BEGIN", func(tx *rollupTx) error {
		deleted = 0
		if _, err := tx.exec(ctx, "LOCK TABLE edge_stats_daily IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return fmt.Errorf("take the rollup lock: %w", err)
		}
		for _, d := range []struct {
			query string
			arg   any
		}{
			{`DELETE FROM edge_stats_hourly WHERE hour < ?`, now.Add(-hourlyRetention)},
			{`DELETE FROM edge_stats_daily WHERE day < ?::date`, cutoff},
			{`DELETE FROM edge_stats_daily_top WHERE day < ?::date`, cutoff},
		} {
			res, err := tx.exec(ctx, d.query, d.arg)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			deleted += n
		}
		for _, d := range []struct {
			what  string
			query string
			args  []any
		}{
			{"the finalization records", `DELETE FROM edge_stats_final WHERE day < ?::date`, []any{cutoff}},
			{"the value hours the 24-hour window no longer reaches", `DELETE FROM edge_stats_value_hour v
				WHERE v.day < ?::date AND EXISTS (SELECT 1 FROM edge_stats_final f WHERE f.day = v.day)`, []any{dateArg(today.AddDate(0, 0, -1))}},
			{"the state of days no run folds", `DELETE FROM edge_stats_value_hour WHERE day < ?::date`, []any{open}},
			{"the visitors of days no run folds", `DELETE FROM edge_stats_visitor WHERE day < ?::date`, []any{open}},
			{"the hours of days no run folds", `DELETE FROM edge_stats_settled_hour WHERE day < ?::date`, []any{open}},
			{"the progress of days no run folds", `DELETE FROM edge_stats_progress WHERE day < ?::date`, []any{open}},
			{"the months past retention", `DELETE FROM edge_stats_monthly_top WHERE month < date_trunc('month', ?::date)::date`, []any{cutoff}},
			{"the record of the month the cutoff falls in", `DELETE FROM edge_stats_month_day
				WHERE date_trunc('month', day) = date_trunc('month', ?::date)
				  AND EXISTS (SELECT 1 FROM edge_stats_month_day WHERE day < ?::date)`, []any{cutoff, cutoff}},
			{"the month days past retention", `DELETE FROM edge_stats_month_day WHERE day < ?::date`, []any{cutoff}},
		} {
			if _, err := tx.exec(ctx, d.query, d.args...); err != nil {
				return fmt.Errorf("delete %s past retention: %w", d.what, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// foldMonths folds every month whose final days changed into its monthly top
// lists (publish.go, staleMonths and buildMonth), newest first, a month to a
// transaction: once a day after a day becomes final, and every kept month on
// the first run after the monthly top lists came to be. A month that fails is
// folded again by the next run, and until then a read takes its days from the
// daily rows (statistics.go, "Whole months").
func foldMonths(ctx context.Context, s *rollupSession, now time.Time) error {
	cutoff := dailyCutoff(utcMidnight(now))
	var months []time.Time
	err := s.inTx(ctx, "BEGIN", func(tx *rollupTx) error {
		var err error
		months, err = staleMonths(ctx, tx, cutoff)
		return err
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, m := range months {
		if _, err := statementBound(ctx); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := s.inTx(ctx, "BEGIN", func(tx *rollupTx) error {
			if _, err := tx.exec(ctx, "LOCK TABLE edge_stats_daily IN SHARE ROW EXCLUSIVE MODE"); err != nil {
				return fmt.Errorf("take the rollup lock: %w", err)
			}
			return buildMonth(ctx, tx, m, cutoff)
		}); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// MaintenanceSession opens the dedicated session a run's statements run on:
// memql.PluginContext.OpenMaintenanceSession, which component/database's
// maintenance_session.go provides. The run closes it with the func it answers.
type MaintenanceSession func(ctx context.Context) (*sql.Conn, func(), error)

// errNoMaintenanceSession refuses a run that has no session to run on. The
// plug-in contract (memql.PluginContext.OpenMaintenanceSession) has a plug-in
// that needs the session refuse rather than fall back to the request pool, and
// the pool is exactly what the session replaces here.
var errNoMaintenanceSession = errors.New("site statistics rollup: this node offers no maintenance session, and the rollup does not run on the request pool instead, whose statement timeout and 10 s read deadline end a day's fold part-way")

// queryGen formats a statement's ? placeholders as bun formats them for the
// request pool, whose bun.DB is built over pgdialect.New() (component/database).
// The session is a bare *sql.Conn with no bun.DB of its own, and the SQL
// builders below are shared with the statistics read, which runs through bun,
// so a statement reads the same on either.
var queryGen = schema.NewQueryGen(pgdialect.New())

// rollupSession is a run's maintenance session ("Where the statements run"
// above): connected when the run starts, released when it ends or when one of
// its transactions fails.
type rollupSession struct {
	open  MaintenanceSession
	conn  *sql.Conn
	close func()
}

// connect opens the session, unless the run holds one.
//
// client_connection_check_interval is component/retention's reason
// (compact.go): the server does not look at a client's socket while a
// statement runs, so a backend whose engine process died would run its
// statement on for nobody until the server's limit, here a minute; with it,
// the server looks every ten seconds and stops. PostgreSQL 14 and later; a
// server that does not know it answers an error that changes nothing, so the
// error is not the run's.
func (s *rollupSession) connect(ctx context.Context) error {
	if s.conn != nil {
		return nil
	}
	conn, closeConn, err := s.open(ctx)
	if err != nil {
		return fmt.Errorf("open the maintenance session: %w", err)
	}
	s.conn, s.close = conn, closeConn
	if sctx, _, cancel, err := statementContext(ctx); err == nil {
		_, _ = conn.ExecContext(sctx, "SET client_connection_check_interval = '10s'")
		cancel()
	}
	return nil
}

// release closes the session; a later transaction of the run opens another.
func (s *rollupSession) release() {
	if s.close != nil {
		s.close()
	}
	s.conn, s.close = nil, nil
}

// inTx runs fn in one transaction on the session, begun with begin (BEGIN, or
// BEGIN with an isolation level), and commits it.
//
// A TRANSACTION THAT FAILS RELEASES THE SESSION, and the next one opens a fresh
// session, rather than rolling back and carrying on. Whether the connection
// outlives a failure depends on the failure: pgdriver closes it under a
// statement the server cancelled (SQLSTATE 57014, which a statement timeout
// raises) and under any error it cannot read as the server's -- a deadline, a
// broken socket -- and keeps it under the rest. Releasing is right for every
// one of them: the server rolls the transaction back when its connection goes,
// and a failed day costs that day alone, never the days after it in the run.
// Kept instead, one cancelled day failed every later day of the run on "sql:
// connection is already closed" (TestADayTheServerCancelsCostsThatDayAlone).
//
// BEGIN AND COMMIT ARE STATEMENTS SENT LIKE ANY OTHER, not database/sql's Tx:
// pgdriver commits and rolls a Tx back under context.Background(), which on
// this session, with no read deadline of its own, is a wait with no end. A
// COMMIT sent here is bounded as every statement is.
func (s *rollupSession) inTx(ctx context.Context, begin string, fn func(*rollupTx) error) error {
	// No time left to start a statement is no reason to open a session.
	if _, err := statementBound(ctx); err != nil {
		return err
	}
	if err := s.connect(ctx); err != nil {
		return err
	}
	tx := &rollupTx{conn: s.conn}
	err := tx.begin(ctx, begin)
	if err == nil {
		err = fn(tx)
	}
	if err == nil {
		_, err = tx.exec(ctx, "COMMIT")
	}
	if err != nil {
		s.release()
	}
	return err
}

// rollupTx is one transaction of a run, on the run's session. Every statement
// of it is bounded: BEGIN by begin, the rest, COMMIT among them, by prepare.
type rollupTx struct {
	conn *sql.Conn
	// limit is the statement_timeout the transaction last SET LOCAL, so a
	// statement under the same limit as the one before it needs no SET.
	limit time.Duration
}

// begin opens the transaction. A SET LOCAL before BEGIN would mean nothing, so
// BEGIN runs under the session's own limit, none, and the client's deadline
// alone; it waits on nothing in the server.
func (t *rollupTx) begin(ctx context.Context, statement string) error {
	sctx, _, cancel, err := statementContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = t.conn.ExecContext(sctx, statement)
	return err
}

// exec runs one statement of the transaction, bounded by prepare, its ?
// placeholders formatted by queryGen.
func (t *rollupTx) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	sctx, cancel, err := t.prepare(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return t.conn.ExecContext(sctx, queryGen.FormatQuery(query, args...))
}

// column runs one query of the transaction as exec does, and answers its one
// text column, row by row.
func (t *rollupTx) column(ctx context.Context, query string, args ...any) ([]string, error) {
	sctx, cancel, err := t.prepare(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	rows, err := t.conn.QueryContext(sctx, queryGen.FormatQuery(query, args...))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// prepare bounds the statement about to run: the server's statement_timeout is
// SET LOCAL to statementContext's limit, unless the transaction's last SET
// already says it, and the client waits under statementContext's deadline. SET
// takes no snapshot, so it may come before the lock a REPEATABLE READ
// day's transaction takes ahead of its first query (openDay).
func (t *rollupTx) prepare(ctx context.Context) (context.Context, context.CancelFunc, error) {
	sctx, limit, cancel, err := statementContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	if limit != t.limit {
		if _, err := t.conn.ExecContext(sctx, "SET LOCAL statement_timeout = "+strconv.FormatInt(limit.Milliseconds(), 10)); err != nil {
			cancel()
			return nil, nil, fmt.Errorf("bound the statement: %w", err)
		}
		t.limit = limit
	}
	return sctx, cancel, nil
}

// statementContext answers, for a statement that starts now, the context the
// client waits for it under and the server limit it runs under: the limit is
// statementBound, and the client waits rollupStatementGrace past it, so the
// client is never the one to stop first. component/retention's statementContext
// (compact.go), but with no time left it refuses rather than rounding up.
func statementContext(ctx context.Context) (context.Context, time.Duration, context.CancelFunc, error) {
	limit, err := statementBound(ctx)
	if err != nil {
		return nil, 0, nil, err
	}
	sctx, cancel := context.WithTimeout(ctx, limit+rollupStatementGrace)
	return sctx, limit, cancel, nil
}

// statementBound is the server's limit for a statement that starts now:
// rollupStatementTimeout, or what ctx has left less rollupStatementGrace when
// that is shorter. Less than a second is no time to start a statement in, and
// answers an error wrapping context.DeadlineExceeded, which stops the run:
// rounding up, as a compaction pass does, could hand the server a limit past
// the client's deadline, the one order this file exists to rule out.
func statementBound(ctx context.Context) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	limit := rollupStatementTimeout
	if d, ok := ctx.Deadline(); ok {
		limit = min(limit, time.Until(d)-rollupStatementGrace)
	}
	if limit < time.Second {
		return 0, fmt.Errorf("too little of the run's time is left to start a statement: %w", context.DeadlineExceeded)
	}
	return limit, nil
}

// histogramCountsSQL is the 64-element count array of a bucket column, as one
// aggregate expression: a single pass over the rows, where building the array
// from a second GROUP BY on the bucket would scan them twice.
func histogramCountsSQL(bucketColumn string) string {
	var b strings.Builder
	b.WriteString("ARRAY[")
	for i := 0; i < histogramBuckets; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "count(*) FILTER (WHERE %s = %d)", bucketColumn, i)
	}
	b.WriteString("]::bigint[]")
	return b.String()
}

// topDimensions are the top lists of a day: what a page view counts under,
// and what a visit counts under (its entry page, its entry source, its first
// row's country, device, browser and OS). An empty source is a click inside
// the site, not an arrival, so it has no row; an empty country, device,
// browser or OS is something the edge could not tell, and is kept as
// (unknown) so the list still adds up to the day.
var topDimensions = []struct{ name, viewed, entered string }{
	{"page", "path", "entry_page"},
	{"source", "NULLIF(source, '')", "entry_source"},
	{"country", unknownIfEmpty("country"), unknownIfEmpty("country")},
	{"device", unknownIfEmpty("device"), unknownIfEmpty("device")},
	{"browser", unknownIfEmpty("browser"), unknownIfEmpty("browser")},
	{"os", unknownIfEmpty("os"), unknownIfEmpty("os")},
}

func unknownIfEmpty(column string) string {
	return "CASE WHEN " + column + " = '' THEN '" + topUnknown + "' ELSE " + column + " END"
}

// dimensionValues is the LATERAL VALUES list that turns one row into one
// (dimension, value) pair per dimension, so every list is counted in one pass.
func dimensionValues(pick func(name, viewed, entered string) string) string {
	parts := make([]string, len(topDimensions))
	for i, d := range topDimensions {
		parts[i] = fmt.Sprintf("('%s', %s)", d.name, pick(d.name, d.viewed, d.entered))
	}
	return strings.Join(parts, ", ")
}

// hourlyFloor is the first hour the hourly table keeps at now: the hour
// boundary at or after now-30d, since retention deletes every hour before it.
func hourlyFloor(now time.Time) time.Time {
	cut := now.UTC().Add(-hourlyRetention)
	h := cut.Truncate(time.Hour)
	if h.Before(cut) {
		h = h.Add(time.Hour)
	}
	return h
}

// openFrom is the first day a run looks for open days on: the request log's
// retention counted back from today, capped at the daily table's 400 days.
// RetentionDays is clamped to 365 today, so the cap guards a future wider
// clamp rather than a value anyone can set now.
//
// The oldest day of that window is whole. The log drops a chunk only once all
// of it is older than the retention, so on day D the chunk for D-retention is
// still there, and the one before it is the first to go.
func openFrom(today time.Time, retentionDays int) time.Time {
	from := today.AddDate(0, 0, -retentionDays)
	if cutoff := dailyCutoff(today); from.Before(cutoff) {
		from = cutoff
	}
	return from
}

// dailyCutoff is the oldest day the daily and top tables keep on today.
func dailyCutoff(today time.Time) time.Time {
	return today.AddDate(0, 0, -dailyRetentionDays)
}

// dateArg renders a UTC day as the 'YYYY-MM-DD' a ?::date placeholder takes:
// a date, not an instant, so no session time zone can move it.
func dateArg(day time.Time) string {
	return day.UTC().Format(time.DateOnly)
}
