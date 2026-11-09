package sitetraffic

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/statsperiod"
)

// publish.go -- the rows a read reads, written from a day's settled state and,
// for a day still open to late requests, its tail (rollup.go, "Incremental,
// with a two-hour tail"): the hourly, daily and daily top rows of a day; the
// current 24-hour window's top lists; and each month's top lists.
//
// A day's rows are REPLACED, never added to: every publication is the whole
// of what the state and the tail say, so publishing twice changes nothing,
// and a run that dies part way leaves the rows of the last publication that
// committed.

// dayHours is the relation of one day's hours: its settled hours with, when
// tail is set, the tail's hour deltas added, one row per site and hour that
// served a request. Its one argument is the day.
func dayHours(tail *foldTables) string {
	columns := "site_id, hour, measured, visitors, first_visitors, last_visitors, visits, bounces, page_views, requests, errors, not_found, bytes, bot_requests, duration_hist"
	from := "SELECT " + columns + " FROM edge_stats_settled_hour WHERE day = ?::date"
	if tail != nil {
		from += "\n UNION ALL SELECT " + columns + " FROM " + tail.hour
	}
	return fmt.Sprintf(`(SELECT site_id, hour, bool_or(measured) AS measured,
        sum(visitors)::bigint AS visitors, sum(first_visitors)::bigint AS first_visitors,
        sum(last_visitors)::bigint AS last_visitors, sum(visits)::bigint AS visits, sum(bounces)::bigint AS bounces,
        sum(page_views)::bigint AS page_views, sum(requests)::bigint AS requests, sum(errors)::bigint AS errors,
        sum(not_found)::bigint AS not_found, sum(bytes)::bigint AS bytes, sum(bot_requests)::bigint AS bot_requests,
        %s AS duration_hist
   FROM (%s) x
  GROUP BY site_id, hour
 HAVING sum(requests) > 0)`, histogramSumSQL("duration_hist"), from)
}

// dayValues is the relation of one day's value hours: its settled ones with,
// when tail is set, the tail's value deltas, unsummed. Its one argument is
// the day.
func dayValues(tail *foldTables) string {
	columns := "site_id, dimension, value, hour, first_visitors, last_visitors, visits, page_views"
	from := "SELECT " + columns + " FROM edge_stats_value_hour WHERE day = ?::date"
	if tail != nil {
		from += "\n UNION ALL SELECT " + columns + " FROM " + tail.value
	}
	return "(" + from + ")"
}

// hourArraySQL is a day's 24 per-hour sums of a column, in hour order, zero
// for an hour with none.
func hourArraySQL(column string) string {
	parts := make([]string, 24)
	for h := range parts {
		parts[h] = fmt.Sprintf("coalesce(sum(%s) FILTER (WHERE hour = %d), 0)", column, h)
	}
	return "ARRAY[" + strings.Join(parts, ", ") + "]::bigint[]"
}

// foldTopSQL ranks a `summed` relation of (site_id, dimension, value,
// visitors, visits, page_views) per site and dimension and folds every value
// past keep into (other). Its one argument is the key written between the
// site and the dimension -- the day, the month or the window's end -- of type
// keyType.
//
// The rank is the one every top list uses: most visitors, then most page
// views, then the value in byte order (COLLATE "C"), so the cut is the same on
// every database. The fold groups by its label, so a real value that reads
// "(other)" -- a utm_source is the client's to write -- joins the fold instead
// of colliding with it on the primary key and failing the whole publication
// for every site (TestACraftedOtherSourceJoinsTheFoldInsteadOfFailingTheDay).
// A day's and a window's values are the requests' own, so such a value is
// ranked like any other, as the recompute ranked it. A month's are days' top
// rows, whose (other) rows are remainders: with remainders set, a value that
// reads (other) is never ranked, and joins the month's remainder.
func foldTopSQL(keep int, keyType string, remainders bool) string {
	partition := "site_id, dimension"
	kept := fmt.Sprintf("rank <= %d", keep)
	if remainders {
		partition += ", value = '" + topOther + "'"
		kept = "value <> '" + topOther + "' AND " + kept
	}
	return fmt.Sprintf(`,
ranked AS (
    SELECT site_id, dimension, value, visitors, visits, page_views,
           row_number() OVER (PARTITION BY %[2]s
                              ORDER BY visitors DESC, page_views DESC, value COLLATE "C" ASC) AS rank
      FROM summed
)
SELECT site_id, ?::%[4]s, dimension,
       CASE WHEN %[3]s THEN value ELSE '%[1]s' END,
       sum(visitors)::bigint, sum(visits)::bigint, sum(page_views)::bigint
  FROM ranked
 GROUP BY 1, 2, 3, 4`, topOther, partition, kept, keyType)
}

// publishDay replaces one day's published rows -- its hours still inside the
// hourly table's thirty days, its daily row and its top lists -- with what its
// settled state says, with tail's deltas added when tail is set. It answers
// the hour periods it wrote, through the hour under way.
func publishDay(ctx context.Context, tx *rollupTx, day, now time.Time, tail *foldTables) (int, error) {
	dayEnd := day.AddDate(0, 0, 1)
	date := dateArg(day)
	// The hours the hourly table keeps: retention removes every hour before
	// hourlyFloor, so writing one would be work thrown away the same run.
	hourStart := hourlyFloor(now)
	if hourStart.Before(day) {
		hourStart = day
	}
	withHours := hourStart.Before(dayEnd)
	if withHours {
		if _, err := tx.exec(ctx, `DELETE FROM edge_stats_hourly WHERE hour >= ? AND hour < ?`, hourStart, dayEnd); err != nil {
			return 0, fmt.Errorf("clear the hours: %w", err)
		}
		q := fmt.Sprintf(`
INSERT INTO edge_stats_hourly (site_id, hour, visitors, visits, bounces, page_views,
                               requests, errors, not_found, bytes, bot_requests, duration_hist)
SELECT site_id, (?::date + make_interval(hours => hour))::timestamp AT TIME ZONE 'UTC',
       CASE WHEN measured THEN visitors END, CASE WHEN measured THEN visits END,
       CASE WHEN measured THEN bounces END, CASE WHEN measured THEN page_views END,
       requests, errors, not_found, bytes, bot_requests, duration_hist
  FROM %s h
 WHERE hour >= ?`, dayHours(tail))
		if _, err := tx.exec(ctx, q, date, date, int(hourStart.Sub(day)/time.Hour)); err != nil {
			return 0, fmt.Errorf("write the hours: %w", err)
		}
	}
	if _, err := tx.exec(ctx, `DELETE FROM edge_stats_daily WHERE day = ?::date`, date); err != nil {
		return 0, fmt.Errorf("clear the day: %w", err)
	}
	// ABSENT IS NOT ZERO. visitors, visits, bounces and page_views are NULL for
	// a day none of whose requests the edge classified -- every day recorded
	// before it did -- so a reader says "not measured" instead of "nobody". A
	// day's visitors are distinct over the day: each visitor is counted in the
	// hour of their first page view once, so the first-hour counts add up to
	// them.
	q := fmt.Sprintf(`
INSERT INTO edge_stats_daily (site_id, day, visitors, visits, bounces, page_views,
                              requests, errors, not_found, bytes, bot_requests, duration_hist,
                              first_hour_visitors, last_hour_visitors)
SELECT site_id, ?::date,
       CASE WHEN bool_or(measured) THEN sum(first_visitors)::bigint END,
       CASE WHEN bool_or(measured) THEN sum(visits)::bigint END,
       CASE WHEN bool_or(measured) THEN sum(bounces)::bigint END,
       CASE WHEN bool_or(measured) THEN sum(page_views)::bigint END,
       sum(requests)::bigint, sum(errors)::bigint, sum(not_found)::bigint, sum(bytes)::bigint,
       sum(bot_requests)::bigint, %s,
       CASE WHEN bool_or(measured) THEN %s END,
       CASE WHEN bool_or(measured) THEN %s END
  FROM %s h
 GROUP BY site_id`, histogramSumSQL("duration_hist"), hourArraySQL("first_visitors"), hourArraySQL("last_visitors"), dayHours(tail))
	if _, err := tx.exec(ctx, q, date, date); err != nil {
		return 0, fmt.Errorf("write the day: %w", err)
	}
	if _, err := tx.exec(ctx, `DELETE FROM edge_stats_daily_top WHERE day = ?::date`, date); err != nil {
		return 0, fmt.Errorf("clear the top lists: %w", err)
	}
	// A value's visitors are distinct over the day: each is counted in the
	// hour they first viewed it, once. A value nobody viewed that day has no
	// row, as a recompute's top lists had none.
	q = fmt.Sprintf(`
INSERT INTO edge_stats_daily_top (site_id, day, dimension, value, visitors, visits, page_views)
WITH summed AS (
    SELECT site_id, dimension, value, sum(first_visitors)::bigint AS visitors,
           sum(visits)::bigint AS visits, sum(page_views)::bigint AS page_views
      FROM %s v
     GROUP BY 1, 2, 3
    HAVING sum(page_views) > 0
)%s`, dayValues(tail), foldTopSQL(topValuesKept, "date", false))
	if _, err := tx.exec(ctx, q, date, date); err != nil {
		return 0, fmt.Errorf("write the top lists: %w", err)
	}
	if !withHours {
		return 0, nil
	}
	// Counted through the hour under way; the hours after it are cleared and
	// empty, not published.
	through := now.Truncate(time.Hour).Add(time.Hour)
	if through.After(dayEnd) {
		through = dayEnd
	}
	if !through.After(hourStart) {
		return 0, nil
	}
	return int(through.Sub(hourStart) / time.Hour), nil
}

// windowPart is one day's part of a 24-hour window: its hours from hour on
// (a suffix, whose visitors are counted by the hour of their last page view),
// or before hour (a prefix, counted by the hour of their first). The visitor
// hash changes at midnight (D1), so nobody is in both of a window's parts, and
// the two add up.
type windowPart struct {
	day    time.Time
	hour   int
	suffix bool
}

// windowParts are the parts of the hour-aligned 24-hour window [from, to): the
// rest of from's day, and to's day up to to's hour when to is not a midnight.
func windowParts(from, to time.Time) []windowPart {
	from, to = from.UTC(), to.UTC()
	parts := []windowPart{{day: utcMidnight(from), hour: from.Hour(), suffix: true}}
	if end := utcMidnight(to); end.After(utcMidnight(from)) && to.Hour() > 0 {
		parts = append(parts, windowPart{day: end, hour: to.Hour()})
	}
	return parts
}

// publishWindow replaces the current 24-hour window's top lists -- the window
// the 24-hour read reads, ending at the next hour -- with what the window's
// days hold: each part's value hours, settled and, for a day in tails, its
// tail's deltas. Every other window's rows go with it.
func publishWindow(ctx context.Context, tx *rollupTx, now time.Time, tails map[string]*foldTables) error {
	b := statsperiod.For(statsperiod.Last24Hours, now)
	var parts []string
	var args []any
	for _, p := range windowParts(b.Start, b.End) {
		visitors, hours := "first_visitors", "hour < ?"
		if p.suffix {
			visitors, hours = "last_visitors", "hour >= ?"
		}
		parts = append(parts, fmt.Sprintf(`SELECT site_id, dimension, value, %s AS visitors, visits, page_views FROM %s v WHERE %s`,
			visitors, dayValues(tails[dateArg(p.day)]), hours))
		args = append(args, dateArg(p.day), p.hour)
	}
	if _, err := tx.exec(ctx, `DELETE FROM edge_stats_window_top`); err != nil {
		return fmt.Errorf("clear the window's top lists: %w", err)
	}
	q := fmt.Sprintf(`
INSERT INTO edge_stats_window_top (site_id, window_end, dimension, value, visitors, visits, page_views)
WITH summed AS (
    SELECT site_id, dimension, value, sum(visitors)::bigint AS visitors,
           sum(visits)::bigint AS visits, sum(page_views)::bigint AS page_views
      FROM (%s) p
     GROUP BY 1, 2, 3
    HAVING sum(page_views) > 0
)%s`, strings.Join(parts, "\nUNION ALL\n"), foldTopSQL(topValuesKept, "timestamptz", false))
	if _, err := tx.exec(ctx, q, append(args, b.End.UTC())...); err != nil {
		return fmt.Errorf("write the window's top lists: %w", err)
	}
	return nil
}

// finishDay publishes a day whose settled fold has reached its end, records it
// final, and deletes its state, in the caller's transaction: a day is final
// only if the publication that closed it was written. Its value hours stay
// when the 24-hour window can still reach the day -- it is yesterday or today
// -- and go otherwise. ON CONFLICT DO NOTHING keeps the first record, should
// two runs ever close one day.
func finishDay(ctx context.Context, tx *rollupTx, day, now time.Time) (int, error) {
	hours, err := publishDay(ctx, tx, day, now, nil)
	if err != nil {
		return 0, err
	}
	date := dateArg(day)
	if _, err := tx.exec(ctx, `INSERT INTO edge_stats_final (day, finalized_at) VALUES (?::date, ?)
		ON CONFLICT (day) DO NOTHING`, date, now); err != nil {
		return 0, fmt.Errorf("record the day final: %w", err)
	}
	statements := []string{
		`DELETE FROM edge_stats_visitor WHERE day = ?::date`,
		`DELETE FROM edge_stats_settled_hour WHERE day = ?::date`,
		`DELETE FROM edge_stats_progress WHERE day = ?::date`,
	}
	if day.Before(utcMidnight(now).AddDate(0, 0, -1)) {
		statements = append(statements, `DELETE FROM edge_stats_value_hour WHERE day = ?::date`)
	}
	for _, q := range statements {
		if _, err := tx.exec(ctx, q, date); err != nil {
			return 0, fmt.Errorf("delete the final day's state: %w", err)
		}
	}
	return hours, nil
}

// staleMonths are the months whose top lists do not cover exactly their final
// days: a day finalized since the month was folded, a day retention removed,
// or every month before the monthly top lists existed. Counting is enough:
// a month's covered days are final days, and both leave only through
// retention, together.
func staleMonths(ctx context.Context, tx *rollupTx, cutoff time.Time) ([]time.Time, error) {
	rows, err := tx.column(ctx, `
SELECT to_char(coalesce(f.month, c.month), 'YYYY-MM-DD')
  FROM (SELECT date_trunc('month', day)::date AS month, count(*) AS days
          FROM edge_stats_final WHERE day >= ?::date GROUP BY 1) f
  FULL JOIN (SELECT date_trunc('month', day)::date AS month, count(*) AS days
               FROM edge_stats_month_day GROUP BY 1) c ON c.month = f.month
 WHERE f.days IS DISTINCT FROM c.days
 ORDER BY 1 DESC`, dateArg(cutoff))
	if err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, len(rows))
	for _, r := range rows {
		m, err := time.Parse(time.DateOnly, r)
		if err != nil {
			return nil, fmt.Errorf("read a month %q: %w", r, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// buildMonth replaces one month's top lists with the fold of its final days'
// top rows, and records the days it covers, in the caller's transaction:
// each value's visitors, visits and page views summed over the days, the
// hundred with the most visitors kept per site and dimension, the rest in
// (other). The days' own (other) rows join the month's.
func buildMonth(ctx context.Context, tx *rollupTx, month, cutoff time.Time) error {
	from, to := dateArg(month), dateArg(month.AddDate(0, 1, 0))
	steps := []struct {
		what  string
		query string
		args  []any
	}{
		{"clear the month", `DELETE FROM edge_stats_monthly_top WHERE month = ?::date`, []any{from}},
		{"clear the month's days", `DELETE FROM edge_stats_month_day WHERE day >= ?::date AND day < ?::date`, []any{from, to}},
		{"record the month's days", `INSERT INTO edge_stats_month_day (day)
SELECT day FROM edge_stats_final WHERE day >= ?::date AND day < ?::date AND day >= ?::date`, []any{from, to, dateArg(cutoff)}},
		{"fold the month", `
INSERT INTO edge_stats_monthly_top (site_id, month, dimension, value, visitors, visits, page_views)
WITH summed AS (
    SELECT site_id, dimension, value, sum(visitors)::bigint AS visitors,
           sum(visits)::bigint AS visits, sum(page_views)::bigint AS page_views
      FROM edge_stats_daily_top
     WHERE day IN (SELECT day FROM edge_stats_month_day WHERE day >= ?::date AND day < ?::date)
     GROUP BY 1, 2, 3
)` + foldTopSQL(topValuesKept, "date", true), []any{from, to, from}},
	}
	for _, s := range steps {
		if _, err := tx.exec(ctx, s.query, s.args...); err != nil {
			return fmt.Errorf("%s of %s: %w", s.what, month.Format("2006-01"), err)
		}
	}
	return nil
}
