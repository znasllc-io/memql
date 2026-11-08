package sitetraffic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// The statistics rollup: folds the edge's request log into a deployable's
// hourly and daily website statistics (docs/superpowers/specs/
// 2026-11-08-site-and-email-statistics-design.md, section 5, D6 and D17).
// builtin siteStatisticsRollup runs it every five minutes on the cron leader,
// under its own maintenance principal (integration.go, handleRollup).
//
// # Why a scheduled recompute and not a continuous aggregate (D6)
//
// The two figures that matter most cannot be folded incrementally. Visitors
// are COUNT(DISTINCT visitor), which a TimescaleDB continuous aggregate cannot
// maintain, and a visit is a window over each visitor's page views. So each run
// recomputes a whole period from edge_request and replaces that period's rows:
// a recompute is the entire computation for its period, which is what makes
// running it twice change nothing, and what lets a run that died part way be
// repaired by the next one.
//
// # Why every hour of today, not only the latest
//
// A visit is segmented over the whole day's page views and counted in the hour
// it started (D17), so a page view arriving now can change an EARLIER hour: the
// one-page visit that began at 09:50 stops being a bounce when its second page
// arrives at 10:05, and 09:00's bounces fall. Recomputing only the current hour
// would freeze 09:00's bounce at its first answer. Yesterday is recomputed too
// until 02:00 UTC, so a request served at 23:59 and written after midnight
// still lands in yesterday.
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
// # One statement per period, over every site
//
// Each statement covers all sites at once; there is no per-site loop. The bound
// is the statement timeout: a period whose statement does not finish within it
// fails, is logged by the caller, and is recomputed on the next run. A very
// large cluster's day therefore degrades to "retried every five minutes"
// rather than to a run that never ends.
//
// # What a run does not revisit (a known limitation)
//
// After 02:00 UTC yesterday is final, and a backfill only reaches below the
// oldest day already written. So if no run succeeds between a day's last
// recompute and 02:00 the next morning -- the cron leader down, every run
// failing -- that day keeps the figures of its last successful run, and a day
// on which no run succeeded at all has no rows, until somebody deletes the
// rows from that day onward so the next run backfills them. Finding such gaps
// would mean comparing every day's figures with the request log on every run.

// RollupReport says what one run of the rollup did.
type RollupReport struct {
	// Hours is how many hour periods were recomputed: for every recomputed
	// day, its hours from the first one the hourly table still keeps through
	// the one under way.
	Hours int `json:"hours"`
	// Days is how many UTC days were recomputed, today among them.
	Days int `json:"days"`
	// Backfilled is true when the run reached past today and yesterday for
	// days of the request log no run had covered: the whole log on the first
	// run, or the older end of a backfill a failure cut short.
	Backfilled bool `json:"backfilled"`
	// Deleted is how many rollup rows retention removed.
	Deleted int64 `json:"deleted"`
}

const (
	// rollupStatementTimeout bounds every statement the rollup runs. It is the
	// rollup's only bound on work: see the package comment above.
	rollupStatementTimeout = "60s"

	// hourlyRetention is how long edge_stats_hourly keeps a row: the request
	// log's own default, so the last thirty days can always be recomputed.
	hourlyRetention = 30 * 24 * time.Hour

	// dailyRetentionDays is how long edge_stats_daily and edge_stats_daily_top
	// keep a row: thirteen months of daily totals (D4).
	dailyRetentionDays = 400

	// yesterdayGrace is how long into a UTC day yesterday is still recomputed.
	yesterdayGrace = 2 * time.Hour

	// topValuesKept is how many values a day's top list keeps per dimension
	// before the rest fold into topOther.
	topValuesKept = 100

	topOther   = "(other)"
	topUnknown = "(unknown)"
)

// Rollup recomputes the statistics tables from edge_request as of now.
//
// It recomputes every hour of today and today itself, and yesterday's while
// now is before 02:00 UTC. It backfills the logged days no run has covered --
// on the first run, every day the request log still holds -- newest first,
// writing hourly rows only for the hours the hourly table keeps. Then it
// deletes the rows past retention: hourly rows older than now-30d, daily and
// top rows older than today-400d.
//
// Every day is its own transaction, so one day that fails costs only that
// day: the days before it in the run are still written, and the failure is
// returned, joined with any other, for the caller to log. A failed day is
// recomputed by a later run: today, and yesterday until 02:00, because every
// run recomputes them; a backfill day because the run stops at the first
// failed day before today, so every older day stays unwritten and the next
// run's backfill, which starts below the oldest daily row, reaches it again.
// On a run that is backfilling, that stop covers yesterday past 02:00 too; in
// steady operation yesterday is final at 02:00 (see the package comment).
func Rollup(ctx context.Context, db *bun.DB, now time.Time) (RollupReport, error) {
	var rep RollupReport
	if db == nil {
		return rep, errors.New("site statistics rollup: no database handle")
	}
	now = now.UTC()

	// A backfill that cannot be planned still leaves today and yesterday to
	// do: planRollup answers them whatever its reads said.
	var failures []error
	days, err := planRollup(ctx, db, now)
	if err != nil {
		failures = append(failures, fmt.Errorf("site statistics backfill: %w", err))
	}
	today := utcMidnight(now)
	for _, d := range days {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if d.backfill {
			rep.Backfilled = true
		}
		hours, err := recomputeDay(ctx, db, d.day, now)
		if err != nil {
			failures = append(failures, fmt.Errorf("site statistics for %s: %w", d.day.Format(time.DateOnly), err))
			if d.day.Before(today) {
				// Stop at the first failed day before today rather than
				// skip ahead. The next run backfills below the oldest daily
				// row, so a failed day is found again only while every day
				// older than it is still missing: on a run that is
				// backfilling, writing an older day would hide this one for
				// good -- yesterday included, once 02:00 has passed. In
				// steady operation nothing older is scheduled, so the stop
				// costs nothing. Today is the exception: every run
				// recomputes it, so a today that keeps failing does not also
				// hold the backfill back.
				break
			}
			continue
		}
		rep.Days++
		rep.Hours += hours
	}

	deleted, err := deletePastRetention(ctx, db, now)
	if err != nil {
		failures = append(failures, fmt.Errorf("site statistics retention: %w", err))
	}
	rep.Deleted = deleted
	return rep, errors.Join(failures...)
}

// rollupDay is one UTC day a run recomputes.
type rollupDay struct {
	day time.Time
	// backfill marks a day outside the regular set (today, and yesterday
	// before 02:00), found below the oldest day already rolled up.
	backfill bool
}

// planRollup lists the days to recompute: today and yesterday first, then the
// backfill from the newest missing day to the oldest.
//
// THE BACKFILL IS EVERY LOGGED DAY OLDER THAN THE OLDEST DAILY ROW, inside the
// daily table's 400 days. With the table empty that is every day the request
// log holds, which is the first run's backfill. Afterwards it is empty --
// every logged day has a daily row once written, and the log only loses old
// days -- unless a backfill stopped part way, in which case it is exactly the
// days it did not reach. Both bounds are read inside the window: a day the
// daily table could not keep is never backfilled, so retention and the
// backfill cannot undo each other every run, and a row outside the window
// (none, in production; a test's, elsewhere) does not move the decision.
//
// It does not find a gap between written days, such as a day the cron leader
// never ran at all; see "What a run does not revisit" above.
//
// The regular days are answered even when the backfill's reads fail, with the
// error beside them.
func planRollup(ctx context.Context, db *bun.DB, now time.Time) ([]rollupDay, error) {
	today := utcMidnight(now)
	days := []rollupDay{{day: today}}
	// Keyed by date rather than by time.Time, whose == also compares the
	// location and any monotonic reading.
	regular := map[string]bool{dateArg(today): true}
	if now.Before(today.Add(yesterdayGrace)) {
		yesterday := today.AddDate(0, 0, -1)
		days = append(days, rollupDay{day: yesterday})
		regular[dateArg(yesterday)] = true
	}

	oldest, newest, ok, err := backfillRange(ctx, db, today)
	if err != nil || !ok {
		return days, err
	}
	for d := newest; !d.Before(oldest); d = d.AddDate(0, 0, -1) {
		if !regular[dateArg(d)] {
			days = append(days, rollupDay{day: d, backfill: true})
		}
	}
	return days, nil
}

// backfillRange answers the logged days below the oldest daily row, oldest
// and newest, inside the daily table's window. Both reads are cheap: the
// oldest daily row is an index lookup, and the request log is asked only for
// its first row below that day, which in the steady state lies in no chunk at
// all.
func backfillRange(ctx context.Context, db *bun.DB, today time.Time) (oldest, newest time.Time, ok bool, err error) {
	cutoff := dailyCutoff(today)
	err = boundedTx(ctx, db, nil, func(ctx context.Context, tx bun.Tx) error {
		var frontier sql.NullString
		if err := tx.NewRaw(`SELECT min(day)::text FROM edge_stats_daily WHERE day >= ?::date AND day <= ?::date`,
			dateArg(cutoff), dateArg(today)).Scan(ctx, &frontier); err != nil {
			return fmt.Errorf("read the oldest daily row: %w", err)
		}
		newest = today
		if frontier.Valid {
			f, err := time.Parse(time.DateOnly, frontier.String)
			if err != nil {
				return fmt.Errorf("read the oldest daily row: %w", err)
			}
			newest = f.AddDate(0, 0, -1)
		}
		if newest.Before(cutoff) {
			return nil
		}
		var first sql.NullTime
		if err := tx.NewRaw(`SELECT min(served_at) FROM edge_request WHERE served_at >= ? AND served_at < ?`,
			cutoff, newest.AddDate(0, 0, 1)).Scan(ctx, &first); err != nil {
			return fmt.Errorf("read the oldest logged request: %w", err)
		}
		if first.Valid {
			oldest, ok = utcMidnight(first.Time), true
		}
		return nil
	})
	return oldest, newest, ok, err
}

// recomputeDay replaces one UTC day's rows -- its hours still inside the
// hourly table's thirty days, its daily row and its top rows -- with what
// edge_request holds, in one transaction. It returns the hour periods covered.
//
// ONE SNAPSHOT. The transaction is REPEATABLE READ, so the hours, the day and
// the top lists are computed from the same rows and always agree with each
// other, whatever the edge writes meanwhile.
//
// ONE WRITER. Two runs can overlap -- a leader change mid-run, a run slower
// than the schedule on another replica -- and two recomputes of one day would
// each delete, then collide inserting. The table lock serializes them, and it
// is taken BEFORE the first query on purpose: a repeatable-read snapshot is
// fixed by the first query, not by BEGIN or a lock, so the run that waited
// sees the rows the other committed and replaces them, rather than failing on
// them. SHARE ROW EXCLUSIVE conflicts with itself and with writers, never with
// a reader, so the Statistics page is not held up by a run.
func recomputeDay(ctx context.Context, db *bun.DB, day, now time.Time) (int, error) {
	dayEnd := day.AddDate(0, 0, 1)
	// The hours the hourly table keeps: retention removes every hour before
	// hourlyFloor, so writing one would be work thrown away the same run.
	hourStart := hourlyFloor(now)
	if hourStart.Before(day) {
		hourStart = day
	}
	withHours := hourStart.Before(dayEnd)
	date := dateArg(day)

	err := boundedTx(ctx, db, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, "LOCK TABLE edge_stats_daily IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return fmt.Errorf("take the rollup lock: %w", err)
		}
		if withHours {
			if _, err := tx.ExecContext(ctx, `DELETE FROM edge_stats_hourly WHERE hour >= ? AND hour < ?`, hourStart, dayEnd); err != nil {
				return fmt.Errorf("clear the hours: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM edge_stats_daily WHERE day = ?::date`, date); err != nil {
			return fmt.Errorf("clear the day: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM edge_stats_daily_top WHERE day = ?::date`, date); err != nil {
			return fmt.Errorf("clear the top lists: %w", err)
		}
		if withHours {
			q, args := bucketStatsInsert("edge_stats_hourly", "hour", hourOf, hourStart, dayEnd)
			if _, err := tx.ExecContext(ctx, q, args...); err != nil {
				return fmt.Errorf("write the hours: %w", err)
			}
		}
		q, args := bucketStatsInsert("edge_stats_daily", "day", dayOf, day, dayEnd)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("write the day: %w", err)
		}
		q, args = topInsert(day, dayEnd)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("write the top lists: %w", err)
		}
		return nil
	})
	if err != nil || !withHours {
		return 0, err
	}
	// Counted through the hour under way; the hours after it are cleared and
	// empty, not recomputed.
	through := now.Truncate(time.Hour).Add(time.Hour)
	if through.After(dayEnd) {
		through = dayEnd
	}
	if !through.After(hourStart) {
		return 0, nil
	}
	return int(through.Sub(hourStart) / time.Hour), nil
}

// deletePastRetention removes hourly rows older than now-30d and daily and top
// rows older than today-400d, and answers how many went. It takes the rollup
// lock like a recompute, so it never deletes under a day being written.
func deletePastRetention(ctx context.Context, db *bun.DB, now time.Time) (int64, error) {
	var deleted int64
	cutoff := dateArg(dailyCutoff(utcMidnight(now)))
	err := boundedTx(ctx, db, nil, func(ctx context.Context, tx bun.Tx) error {
		deleted = 0
		if _, err := tx.ExecContext(ctx, "LOCK TABLE edge_stats_daily IN SHARE ROW EXCLUSIVE MODE"); err != nil {
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
			res, err := tx.ExecContext(ctx, d.query, d.arg)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			deleted += n
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// boundedTx runs fn in a transaction whose every statement is bounded by the
// rollup's statement timeout. SET LOCAL ends with the transaction, so the
// pooled connection goes back with its own setting.
func boundedTx(ctx context.Context, db *bun.DB, opts *sql.TxOptions, fn func(context.Context, bun.Tx) error) error {
	return db.RunInTx(ctx, opts, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '"+rollupStatementTimeout+"'"); err != nil {
			return fmt.Errorf("bound the statements: %w", err)
		}
		return fn(ctx, tx)
	})
}

// The visit segmentation (spec D17): THE definition of a visit in SQL, built
// once and used by every figure that counts visits -- the rollup's hours, days
// and top lists here, and the 24-hour period's top lists the siteStatistics
// read computes from raw rows.
//
// A visit is one visitor's page views with no gap over thirty minutes. Read
// per site, per UTC day and per visitor from the measured page views that have
// a visitor id, in (served_at, id) order:
//
//   - a row starts a visit when it is the visitor's first that day, or comes
//     more than thirty minutes after their previous one -- exactly thirty is
//     the same visit;
//   - a running sum of those starts numbers the visits;
//   - a visit's start is its first row's served_at, and it is counted in the
//     hour and the day of that start, so hourly visits add up to the day's;
//   - its entry page, country, device, browser and OS are its first row's, and
//     its entry source is the first of its rows with a source at all -- a
//     visit that resumes after a pause on an inner page has no source on its
//     first row;
//   - it is a bounce when it holds exactly one row.
//
// The UTC day partitions every window and groups every visit, so a visit never
// crosses midnight. The visitor hash changes at midnight in any case (D1); the
// day key makes the split a property of the definition rather than of the
// hash, which matters to a window that spans midnight, such as the 24-hour
// read's (TestTheVisitSegmentationAnswersTheVisitsThatStartedInAWindow).
//
// The SELECT answers one row per visit with the columns site_id, day, visitor,
// started_at, views, entry_page, entry_source (NULL when none of its rows has
// a source), country, device, browser and os, in the placeholder order
// visitSegmentation returns. It covers every site. A read narrowed to some
// sites filters the result on site_id: site_id partitions every window and
// groups the result, so the planner pushes such a predicate down to the scan
// of edge_request (checked with EXPLAIN on the CI TimescaleDB image, 2.29: an
// outer `site_id IN (...)` becomes the index condition on the chunk scan).
const visitSegmentationSQL = `
SELECT site_id, day, visitor,
       min(served_at) AS started_at,
       count(*) AS views,
       (array_agg(path ORDER BY served_at, id))[1] AS entry_page,
       (array_agg(source ORDER BY served_at, id) FILTER (WHERE source <> ''))[1] AS entry_source,
       (array_agg(country ORDER BY served_at, id))[1] AS country,
       (array_agg(device ORDER BY served_at, id))[1] AS device,
       (array_agg(browser ORDER BY served_at, id))[1] AS browser,
       (array_agg(os ORDER BY served_at, id))[1] AS os
  FROM (
        SELECT pv.*,
               sum(CASE WHEN previous IS NULL OR served_at - previous > interval '30 minutes' THEN 1 ELSE 0 END)
                   OVER (PARTITION BY site_id, day, visitor ORDER BY served_at, id ROWS UNBOUNDED PRECEDING) AS visit
          FROM (
                SELECT site_id, (served_at AT TIME ZONE 'UTC')::date AS day, visitor, served_at, id,
                       path, source, country, device, browser, os,
                       lag(served_at) OVER (PARTITION BY site_id, (served_at AT TIME ZONE 'UTC')::date, visitor
                                            ORDER BY served_at, id) AS previous
                  FROM edge_request
                 WHERE served_at >= ? AND served_at < ? AND page_view AND visitor <> ''
               ) pv
       ) numbered
 GROUP BY site_id, day, visitor, visit
HAVING min(served_at) >= ? AND min(served_at) < ?`

// visitSegmentation answers the visits that STARTED in [from, to), and the
// arguments for visitSegmentationSQL's placeholders, in order. A caller embeds
// the query as a CTE and appends its own arguments after these.
//
// It reads page views from the UTC midnight at or before from, so a row just
// after from is known to continue a visit begun earlier that day rather than
// to start one, and up to the UTC midnight at or after to, so every visit
// counted is complete: its page views, and so whether it bounced, include the
// rows after to, since no visit crosses midnight.
func visitSegmentation(from, to time.Time) (string, []any) {
	readFrom := utcMidnight(from)
	readTo := utcMidnight(to)
	if readTo.Before(to) {
		readTo = readTo.AddDate(0, 0, 1)
	}
	return visitSegmentationSQL, []any{readFrom, readTo, from.UTC(), to.UTC()}
}

// hourOf and dayOf are the UTC hour and day of a timestamptz expression.
func hourOf(column string) string {
	return "date_trunc('hour', " + column + " AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'"
}

func dayOf(column string) string {
	return "(" + column + " AT TIME ZONE 'UTC')::date"
}

// bucketStatsInsert is the INSERT of one table's buckets -- hours or days --
// over [from, to), with its arguments.
//
// ABSENT IS NOT ZERO. visitors, visits, bounces and page_views are NULL for a
// bucket none of whose rows the edge classified -- every bucket recorded
// before it did -- so a reader says "not measured" instead of "nobody". A
// bucket with a measured row and no page view says 0. A visit begins on a
// measured page view, so every bucket a visit starts in has a measured row and
// the join loses none.
func bucketStatsInsert(table, column string, bucketOf func(string) string, from, to time.Time) (string, []any) {
	visits, args := visitSegmentation(from, to)
	q := fmt.Sprintf(`
WITH visits AS (%s),
started AS (
    SELECT site_id, %s AS bucket,
           count(*) AS visits,
           count(*) FILTER (WHERE views = 1) AS bounces
      FROM visits
     GROUP BY 1, 2
),
served AS (
    SELECT site_id, %s AS bucket,
           bool_or(measured) AS measured,
           count(DISTINCT visitor) FILTER (WHERE page_view AND visitor <> '') AS visitors,
           count(*) FILTER (WHERE page_view) AS page_views,
           count(*) AS requests,
           count(*) FILTER (WHERE status >= 500) AS errors,
           count(*) FILTER (WHERE status >= 400 AND status < 500) AS not_found,
           coalesce(sum(bytes), 0)::bigint AS bytes,
           count(*) FILTER (WHERE bot) AS bot_requests,
           %s AS duration_hist
      FROM (SELECT site_id, served_at, status, bytes, measured, page_view, bot, visitor,
                   %s AS duration_bucket
              FROM edge_request
             WHERE served_at >= ? AND served_at < ?) r
     GROUP BY 1, 2
)
INSERT INTO %s (site_id, %s, visitors, visits, bounces, page_views,
                requests, errors, not_found, bytes, bot_requests, duration_hist)
SELECT s.site_id, s.bucket,
       CASE WHEN s.measured THEN s.visitors END,
       CASE WHEN s.measured THEN coalesce(v.visits, 0) END,
       CASE WHEN s.measured THEN coalesce(v.bounces, 0) END,
       CASE WHEN s.measured THEN s.page_views END,
       s.requests, s.errors, s.not_found, s.bytes, s.bot_requests, s.duration_hist
  FROM served s
  LEFT JOIN started v ON v.site_id = s.site_id AND v.bucket = s.bucket`,
		visits, bucketOf("started_at"), bucketOf("served_at"),
		histogramCountsSQL("duration_bucket"), histogramBucketSQL("duration_ns"),
		table, column)
	return q, append(args, from.UTC(), to.UTC())
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

// topInsert is the INSERT of one day's top lists, with its arguments.
//
// Each list keeps the hundred values with the most visitors, ties broken by
// page views and then by value in byte order (COLLATE "C", so the cut is the
// same on every database), and folds the rest into (other), summing their
// visitors, visits and page views. The fold groups by the label, so a real
// value that happens to read "(other)" -- a utm_source is the client's to
// write -- joins the fold instead of colliding with it on the primary key and
// failing the whole day for every site.
func topInsert(from, to time.Time) (string, []any) {
	visits, args := visitSegmentation(from, to)
	q := fmt.Sprintf(`
WITH visits AS (%s),
views AS (
    SELECT site_id, (served_at AT TIME ZONE 'UTC')::date AS day,
           visitor, path, source, country, device, browser, os
      FROM edge_request
     WHERE served_at >= ? AND served_at < ? AND page_view
),
viewed AS (
    SELECT site_id, day, d.dimension, d.value,
           count(DISTINCT visitor) FILTER (WHERE visitor <> '') AS visitors,
           count(*) AS page_views
      FROM views
     CROSS JOIN LATERAL (VALUES %s) AS d(dimension, value)
     WHERE d.value IS NOT NULL
     GROUP BY 1, 2, 3, 4
),
entered AS (
    SELECT site_id, day, d.dimension, d.value, count(*) AS visits
      FROM visits
     CROSS JOIN LATERAL (VALUES %s) AS d(dimension, value)
     WHERE d.value IS NOT NULL
     GROUP BY 1, 2, 3, 4
),
ranked AS (
    SELECT v.site_id, v.day, v.dimension, v.value, v.visitors, coalesce(e.visits, 0) AS visits, v.page_views,
           row_number() OVER (PARTITION BY v.site_id, v.day, v.dimension
                              ORDER BY v.visitors DESC, v.page_views DESC, v.value COLLATE "C" ASC) AS rank
      FROM viewed v
      LEFT JOIN entered e
        ON e.site_id = v.site_id AND e.day = v.day AND e.dimension = v.dimension AND e.value = v.value
)
INSERT INTO edge_stats_daily_top (site_id, day, dimension, value, visitors, visits, page_views)
SELECT site_id, day, dimension,
       CASE WHEN rank <= %d THEN value ELSE '%s' END,
       sum(visitors)::bigint, sum(visits)::bigint, sum(page_views)::bigint
  FROM ranked
 GROUP BY 1, 2, 3, 4`,
		visits,
		dimensionValues(func(_, viewed, _ string) string { return viewed }),
		dimensionValues(func(_, _, entered string) string { return entered }),
		topValuesKept, topOther)
	return q, append(args, from.UTC(), to.UTC())
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

// dailyCutoff is the oldest day the daily and top tables keep on today.
func dailyCutoff(today time.Time) time.Time {
	return today.AddDate(0, 0, -dailyRetentionDays)
}

// dateArg renders a UTC day as the 'YYYY-MM-DD' a ?::date placeholder takes:
// a date, not an instant, so no session time zone can move it.
func dateArg(day time.Time) string {
	return day.UTC().Format(time.DateOnly)
}
