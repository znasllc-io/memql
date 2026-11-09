package sitetraffic

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// fold.go -- what a range of the request log adds to a day's figures, read
// against the day's settled state (rollup.go, "Incremental, with a two-hour
// tail"). The rollup folds each request into its day's settled state once,
// when it is two hours old (settle), and reads the last two hours again on
// every run without keeping them (the tail, publish.go). Both are this fold.
//
// # The definition of a visit (spec D17), incrementally
//
// A visit is one visitor's page views with no gap over thirty minutes, read
// per site, per UTC day and per visitor from the measured page views that
// have a visitor id, in (served_at, id) order. The day partitions everything
// (a day's state is its own), so a visit never crosses midnight, which the
// visitor hash makes true anyway (D1).
//
//   - A page view starts a visit when it is its visitor's first of the day or
//     comes more than thirty minutes after their previous one -- exactly
//     thirty is the same visit. The previous one is the one before it in the
//     range, or for the range's first, the last page view the state holds
//     (edge_stats_visitor.last_at), so a visit carries on across folds.
//   - A visit is counted in the hour it started, so hourly visits add up to
//     the day's.
//   - Its entry page, country, device, browser and OS are its first page
//     view's, and its entry source is the first of its page views with a
//     source at all, which a later fold can find: a visit the state holds
//     open with no source yet takes the first source a fold finds in it
//     (visit_sourced).
//   - It is a bounce while it holds exactly one page view: a fold that
//     continues a one-page visit takes the bounce back from the hour it
//     started.
//
// A visitor is counted once per day, and once per hour (the state's hours
// bitmask). Each visitor is also counted in the hour of their first and of
// their last page view of the day so far, which is what lets the 24-hour
// read add the end of one day to the start of the next. A value -- a page, a
// source, a country, a device, a browser, an operating system -- counts its
// distinct visitors the same way, through a 64-bit hash of (dimension, value)
// per visitor (edge_stats_visitor.seen) and the last hour of each; two values
// whose hashes meet for one visitor would count that visitor once, a chance of
// about one in 10^17 for a visitor with ten values. Page views count every
// page view, with a visitor id or without, as the request log's own count
// does.
//
// TestTheFoldAgreesWithARecomputeWhereverItIsCut holds this to the
// recompute it replaced (rollup_reference_test.go), over the same requests
// folded in one range and in many.

// foldTables are the temporary tables one fold writes, ON COMMIT DROP, named
// by a tag so two folds can share a transaction: today's and yesterday's
// tails, which the 24-hour top lists add up.
type foldTables struct {
	tag string
	// pv is the range's measured page views with a visitor id, numbered into
	// visits: visit 0 continues the visit the state holds open.
	pv string
	// visit is one row per visit in pv.
	visit string
	// visitor is one row per visitor in pv: their state before and after.
	visitor string
	// pair is one row per visitor and (dimension, value) they viewed.
	pair string
	// hour is the range's deltas per site and hour of the day.
	hour string
	// value is the range's deltas per site, dimension, value and hour.
	value string
	// seen is each visitor's seen hashes after the range, for settle.
	seen string
}

func foldTablesFor(tag string) foldTables {
	t := func(name string) string { return "fold_" + tag + "_" + name }
	return foldTables{tag: tag, pv: t("pv"), visit: t("visit"), visitor: t("visitor"),
		pair: t("pair"), hour: t("hour"), value: t("value"), seen: t("seen")}
}

// fold computes into ft's temporary tables what the requests served in
// [from, to) of the UTC day add to the day's figures, read against the day's
// settled state as it stands. It writes nothing else; settle applies the
// result to the state, and publish adds it to the state for a tail.
func fold(ctx context.Context, tx *rollupTx, ft foldTables, day, from, to time.Time) error {
	date := dateArg(day)
	steps := []struct {
		what  string
		query string
		args  []any
	}{
		{"number the page views into visits", fmt.Sprintf(`
CREATE TEMP TABLE %s ON COMMIT DROP AS
SELECT p.site_id, p.visitor, p.served_at, p.id, p.hour, p.path, p.source, p.country, p.device, p.browser, p.os, p.known,
       sum(CASE WHEN p.previous IS NULL OR p.served_at - p.previous > interval '30 minutes' THEN 1 ELSE 0 END)
           OVER (PARTITION BY p.site_id, p.visitor ORDER BY p.served_at, p.id ROWS UNBOUNDED PRECEDING) AS visit
  FROM (SELECT r.site_id, r.visitor, r.served_at, r.id, %s AS hour,
               r.path, r.source, r.country, r.device, r.browser, r.os,
               s.visitor IS NOT NULL AS known,
               coalesce(lag(r.served_at) OVER (PARTITION BY r.site_id, r.visitor ORDER BY r.served_at, r.id), s.last_at) AS previous
          FROM edge_request r
          LEFT JOIN edge_stats_visitor s ON s.site_id = r.site_id AND s.day = ?::date AND s.visitor = r.visitor
         WHERE r.served_at >= ? AND r.served_at < ? AND r.page_view AND r.visitor <> '') p`,
			ft.pv, hourIndexOf("r.served_at")), []any{date, from.UTC(), to.UTC()}},

		{"gather the visits", fmt.Sprintf(`
CREATE TEMP TABLE %s ON COMMIT DROP AS
SELECT site_id, visitor, visit, min(served_at) AS started_at, count(*) AS views,
       (array_agg(hour ORDER BY served_at, id))[1] AS start_hour,
       (array_agg(path ORDER BY served_at, id))[1] AS entry_page,
       (array_agg(source ORDER BY served_at, id) FILTER (WHERE source <> ''))[1] AS entry_source,
       (array_agg(country ORDER BY served_at, id))[1] AS country,
       (array_agg(device ORDER BY served_at, id))[1] AS device,
       (array_agg(browser ORDER BY served_at, id))[1] AS browser,
       (array_agg(os ORDER BY served_at, id))[1] AS os
  FROM %s GROUP BY site_id, visitor, visit`, ft.visit, ft.pv), nil},

		// A visitor's open visit after the range is their last one in it:
		// the one they had open carried on (visit 0), or the last they began.
		{"gather the visitors", fmt.Sprintf(`
CREATE TEMP TABLE %s ON COMMIT DROP AS
SELECT w.site_id, w.visitor,
       s.visitor IS NOT NULL AS known,
       coalesce(s.hours, 0) AS old_hours,
       coalesce(s.hours, 0) | w.hours AS hours,
       w.last_at, w.first_hour, w.last_hour,
       %s AS old_last_hour,
       CASE WHEN lv.visit = 0 THEN s.visit_start ELSE lv.started_at END AS visit_start,
       CASE WHEN lv.visit = 0 THEN s.visit_views + lv.views ELSE lv.views END AS visit_views,
       CASE WHEN lv.visit = 0 THEN s.visit_sourced OR lv.entry_source IS NOT NULL ELSE lv.entry_source IS NOT NULL END AS visit_sourced,
       s.visit_start AS old_visit_start, s.visit_views AS old_visit_views, s.visit_sourced AS old_visit_sourced,
       c.views AS cont_views, c.entry_source AS cont_source,
       s.seen AS old_seen, s.seen_last AS old_seen_last
  FROM (SELECT site_id, visitor, bit_or(1 << hour) AS hours, max(served_at) AS last_at,
               min(hour) AS first_hour, max(hour) AS last_hour
          FROM %s GROUP BY site_id, visitor) w
  JOIN (SELECT DISTINCT ON (site_id, visitor) site_id, visitor, visit, started_at, views, entry_source
          FROM %s ORDER BY site_id, visitor, visit DESC) lv ON lv.site_id = w.site_id AND lv.visitor = w.visitor
  LEFT JOIN %s c ON c.site_id = w.site_id AND c.visitor = w.visitor AND c.visit = 0
  LEFT JOIN edge_stats_visitor s ON s.site_id = w.site_id AND s.day = ?::date AND s.visitor = w.visitor`,
			ft.visitor, hourIndexOf("s.last_at"), ft.pv, ft.visit, ft.visit), []any{date}},

		{"gather what each visitor viewed", fmt.Sprintf(`
CREATE TEMP TABLE %s ON COMMIT DROP AS
SELECT pr.site_id, pr.visitor, pr.dimension, pr.value, pr.hash, pr.first_hour, pr.last_hour,
       array_position(v.old_seen, pr.hash) AS at,
       v.old_seen_last[array_position(v.old_seen, pr.hash)] AS old_last
  FROM (SELECT site_id, visitor, dimension, value, %s AS hash, first_hour, last_hour
          FROM (SELECT p.site_id, p.visitor, d.dimension, d.value, min(p.hour) AS first_hour, max(p.hour) AS last_hour
                  FROM %s p CROSS JOIN LATERAL (VALUES %s) AS d(dimension, value)
                 WHERE d.value IS NOT NULL
                 GROUP BY 1, 2, 3, 4) g) pr
  JOIN %s v ON v.site_id = pr.site_id AND v.visitor = pr.visitor`,
			ft.pair, seenHashSQL("dimension", "value"), ft.pv,
			dimensionValues(func(_, viewed, _ string) string { return viewed }), ft.visitor), nil},

		{"count the hours", hourDeltasSQL(ft), []any{from.UTC(), to.UTC()}},

		{"count the values", valueDeltasSQL(ft), []any{from.UTC(), to.UTC()}},
	}
	for _, s := range steps {
		if _, err := tx.exec(ctx, s.query, s.args...); err != nil {
			return fmt.Errorf("%s: %w", s.what, err)
		}
	}
	return nil
}

// hourIndexOf is the UTC hour of a timestamptz expression, 0 to 23.
func hourIndexOf(column string) string {
	return "extract(hour FROM " + column + " AT TIME ZONE 'UTC')::int"
}

// seenHashSQL is the 64-bit hash a visitor's state keeps of a (dimension,
// value) they were counted under. chr(31), the unit separator, keeps
// ("page", "x") and ("pag", "ex") apart.
func seenHashSQL(dimension, value string) string {
	return "hashtextextended(" + dimension + " || chr(31) || " + value + ", 0)"
}

// hourDeltasSQL counts, per site and hour of the day, what the range adds:
// the health counters of every request in it, and the visitor and visit
// counters of its page views against the state. Its two arguments are the
// range.
func hourDeltasSQL(ft foldTables) string {
	zeroVisits := "0 AS visitors, 0 AS first_visitors, 0 AS last_visitors, 0 AS visits, 0 AS bounces"
	noHealth := "0 AS page_views, 0 AS requests, 0 AS errors, 0 AS not_found, 0 AS bytes, 0 AS bot_requests, NULL::bigint[] AS hist"
	parts := []string{
		// The health counters, from every request of the range: grouped by
		// the response-time bucket first, so each hour's histogram is built
		// from at most 64 rows rather than by 64 filters over every request
		// (measured on a two-hour tail of 900,000 requests: 0.32 s against
		// 1.85 s).
		fmt.Sprintf(`SELECT site_id, hour, bool_or(measured) AS measured, %s,
       sum(page_views) AS page_views, sum(requests) AS requests, sum(errors) AS errors,
       sum(not_found) AS not_found, sum(bytes) AS bytes, sum(bot_requests) AS bot_requests,
       %s AS hist
  FROM (SELECT site_id, %s AS hour, %s AS duration_bucket, bool_or(measured) AS measured,
               count(*) FILTER (WHERE page_view) AS page_views, count(*) AS requests,
               count(*) FILTER (WHERE status >= 500) AS errors,
               count(*) FILTER (WHERE status >= 400 AND status < 500) AS not_found,
               coalesce(sum(bytes), 0) AS bytes, count(*) FILTER (WHERE bot) AS bot_requests
          FROM edge_request WHERE served_at >= ? AND served_at < ?
         GROUP BY 1, 2, 3) r
 GROUP BY 1, 2`, zeroVisits, histogramFromCountsSQL("duration_bucket", "requests"), hourIndexOf("served_at"), histogramBucketSQL("duration_ns")),
		// A visitor new to an hour.
		fmt.Sprintf(`SELECT v.site_id, h, false, count(*), 0, 0, 0, 0, %s
  FROM %s v CROSS JOIN generate_series(0, 23) h
 WHERE (v.hours >> h) & 1 = 1 AND (v.old_hours >> h) & 1 = 0
 GROUP BY 1, 2`, noHealth, ft.visitor),
		// A visitor new to the day, in the hour of their first page view.
		fmt.Sprintf(`SELECT site_id, first_hour, false, 0, count(*), 0, 0, 0, %s FROM %s WHERE NOT known GROUP BY 1, 2`, noHealth, ft.visitor),
		// The hour of each visitor's last page view: counted where it is now,
		// and no longer where it was.
		fmt.Sprintf(`SELECT site_id, last_hour, false, 0, 0, count(*), 0, 0, %s FROM %s
 WHERE NOT known OR last_hour > old_last_hour GROUP BY 1, 2`, noHealth, ft.visitor),
		fmt.Sprintf(`SELECT site_id, old_last_hour, false, 0, 0, -count(*), 0, 0, %s FROM %s
 WHERE known AND last_hour > old_last_hour GROUP BY 1, 2`, noHealth, ft.visitor),
		// A visit begun in the range, in the hour it began; a one-page
		// visit is a bounce.
		fmt.Sprintf(`SELECT site_id, start_hour, false, 0, 0, 0, count(*), count(*) FILTER (WHERE views = 1), %s
  FROM %s WHERE visit >= 1 GROUP BY 1, 2`, noHealth, ft.visit),
		// A one-page visit the range carries on is a bounce no longer, in the
		// hour it began.
		fmt.Sprintf(`SELECT site_id, %s, false, 0, 0, 0, 0, -count(*), %s FROM %s
 WHERE cont_views IS NOT NULL AND old_visit_views = 1 GROUP BY 1, 2`, hourIndexOf("old_visit_start"), noHealth, ft.visitor),
	}
	return fmt.Sprintf(`
CREATE TEMP TABLE %s ON COMMIT DROP AS
SELECT site_id, hour::smallint AS hour, bool_or(measured) AS measured,
       sum(visitors)::bigint AS visitors, sum(first_visitors)::bigint AS first_visitors,
       sum(last_visitors)::bigint AS last_visitors, sum(visits)::bigint AS visits, sum(bounces)::bigint AS bounces,
       sum(page_views)::bigint AS page_views, sum(requests)::bigint AS requests, sum(errors)::bigint AS errors,
       sum(not_found)::bigint AS not_found, sum(bytes)::bigint AS bytes, sum(bot_requests)::bigint AS bot_requests,
       %s AS duration_hist
  FROM (%s) x
 GROUP BY site_id, hour`, ft.hour, histogramSumSQL("hist"), strings.Join(parts, "\nUNION ALL\n"))
}

// valueDeltasSQL counts, per site, dimension, value and hour of the day, what
// the range adds: page views of every page view in it, and visitors and
// visits against the state. Its two arguments are the range.
func valueDeltasSQL(ft foldTables) string {
	viewed := dimensionValues(func(_, viewed, _ string) string { return viewed })
	entered := dimensionValues(func(_, _, entered string) string { return entered })
	parts := []string{
		// Page views, every one, with a visitor id or without.
		fmt.Sprintf(`SELECT r.site_id, d.dimension, d.value, %s AS hour, 0 AS first_visitors, 0 AS last_visitors, 0 AS visits, count(*) AS page_views
  FROM edge_request r CROSS JOIN LATERAL (VALUES %s) AS d(dimension, value)
 WHERE r.served_at >= ? AND r.served_at < ? AND r.page_view AND d.value IS NOT NULL
 GROUP BY 1, 2, 3, 4`, hourIndexOf("r.served_at"), viewed),
		// A value new to its visitor, first and last seen in the range.
		fmt.Sprintf(`SELECT site_id, dimension, value, first_hour, 1, 0, 0, 0 FROM %s WHERE at IS NULL`, ft.pair),
		fmt.Sprintf(`SELECT site_id, dimension, value, last_hour, 0, 1, 0, 0 FROM %s WHERE at IS NULL`, ft.pair),
		// A value the visitor had: the hour they last viewed it moves on.
		fmt.Sprintf(`SELECT site_id, dimension, value, old_last, 0, -1, 0, 0 FROM %s WHERE at IS NOT NULL AND last_hour > old_last`, ft.pair),
		fmt.Sprintf(`SELECT site_id, dimension, value, last_hour, 0, 1, 0, 0 FROM %s WHERE at IS NOT NULL AND last_hour > old_last`, ft.pair),
		// A visit begun in the range, under its entry values, in the hour it
		// began.
		fmt.Sprintf(`SELECT v.site_id, d.dimension, d.value, v.start_hour, 0, 0, 1, 0
  FROM %s v CROSS JOIN LATERAL (VALUES %s) AS d(dimension, value)
 WHERE v.visit >= 1 AND d.value IS NOT NULL`, ft.visit, entered),
		// A visit the state held open with no source, whose first source is
		// in the range: counted under it, in the hour the visit began.
		fmt.Sprintf(`SELECT site_id, 'source', cont_source, %s, 0, 0, 1, 0 FROM %s
 WHERE cont_source IS NOT NULL AND NOT old_visit_sourced`, hourIndexOf("old_visit_start"), ft.visitor),
	}
	return fmt.Sprintf(`
CREATE TEMP TABLE %s ON COMMIT DROP AS
SELECT site_id, dimension, value, hour::smallint AS hour,
       sum(first_visitors)::bigint AS first_visitors, sum(last_visitors)::bigint AS last_visitors,
       sum(visits)::bigint AS visits, sum(page_views)::bigint AS page_views
  FROM (%s) x
 GROUP BY 1, 2, 3, 4`, ft.value, strings.Join(parts, "\nUNION ALL\n"))
}

// settle applies a fold of [from, to) to the day's settled state and moves the
// day's progress to to, in the fold's transaction: the hour and value
// counters add the fold's deltas, and each visitor's row becomes what the fold
// says it is after the range.
func settle(ctx context.Context, tx *rollupTx, ft foldTables, day, to time.Time) error {
	date := dateArg(day)
	steps := []struct {
		what  string
		query string
		args  []any
	}{
		{"merge what each visitor has been counted under", fmt.Sprintf(`
CREATE TEMP TABLE %s ON COMMIT DROP AS
SELECT site_id, visitor, array_agg(hash ORDER BY hash) AS seen, array_agg(seen_hour ORDER BY hash) AS seen_last
  FROM (SELECT site_id, visitor, hash, max(seen_hour)::smallint AS seen_hour
          FROM (SELECT v.site_id, v.visitor, u.hash, u.seen_hour
                  FROM %s v CROSS JOIN LATERAL unnest(v.old_seen, v.old_seen_last) AS u(hash, seen_hour)
                 WHERE v.known
                UNION ALL
                SELECT site_id, visitor, hash, last_hour FROM %s) x
         GROUP BY 1, 2, 3) y
 GROUP BY 1, 2`, ft.seen, ft.visitor, ft.pair), nil},

		{"add the hours", fmt.Sprintf(`
INSERT INTO edge_stats_settled_hour AS s (site_id, day, hour, measured, visitors, first_visitors, last_visitors,
        visits, bounces, page_views, requests, errors, not_found, bytes, bot_requests, duration_hist)
SELECT site_id, ?::date, hour, measured, visitors, first_visitors, last_visitors,
       visits, bounces, page_views, requests, errors, not_found, bytes, bot_requests, duration_hist
  FROM %s
ON CONFLICT (site_id, day, hour) DO UPDATE SET
       measured = s.measured OR EXCLUDED.measured,
       visitors = s.visitors + EXCLUDED.visitors,
       first_visitors = s.first_visitors + EXCLUDED.first_visitors,
       last_visitors = s.last_visitors + EXCLUDED.last_visitors,
       visits = s.visits + EXCLUDED.visits,
       bounces = s.bounces + EXCLUDED.bounces,
       page_views = s.page_views + EXCLUDED.page_views,
       requests = s.requests + EXCLUDED.requests,
       errors = s.errors + EXCLUDED.errors,
       not_found = s.not_found + EXCLUDED.not_found,
       bytes = s.bytes + EXCLUDED.bytes,
       bot_requests = s.bot_requests + EXCLUDED.bot_requests,
       duration_hist = %s`, ft.hour, addArraysSQL("s.duration_hist", "EXCLUDED.duration_hist")), []any{date}},

		{"add the values", fmt.Sprintf(`
INSERT INTO edge_stats_value_hour AS s (site_id, day, dimension, value, hour, first_visitors, last_visitors, visits, page_views)
SELECT site_id, ?::date, dimension, value, hour, first_visitors, last_visitors, visits, page_views FROM %s
ON CONFLICT (site_id, day, dimension, value, hour) DO UPDATE SET
       first_visitors = s.first_visitors + EXCLUDED.first_visitors,
       last_visitors = s.last_visitors + EXCLUDED.last_visitors,
       visits = s.visits + EXCLUDED.visits,
       page_views = s.page_views + EXCLUDED.page_views`, ft.value), []any{date}},

		{"write the visitors", fmt.Sprintf(`
INSERT INTO edge_stats_visitor (site_id, day, visitor, hours, last_at, visit_start, visit_views, visit_sourced, seen, seen_last)
SELECT v.site_id, ?::date, v.visitor, v.hours, v.last_at, v.visit_start, v.visit_views, v.visit_sourced, m.seen, m.seen_last
  FROM %s v JOIN %s m ON m.site_id = v.site_id AND m.visitor = v.visitor
ON CONFLICT (site_id, day, visitor) DO UPDATE SET
       hours = EXCLUDED.hours, last_at = EXCLUDED.last_at, visit_start = EXCLUDED.visit_start,
       visit_views = EXCLUDED.visit_views, visit_sourced = EXCLUDED.visit_sourced,
       seen = EXCLUDED.seen, seen_last = EXCLUDED.seen_last`, ft.visitor, ft.seen), []any{date}},

		{"move the day's progress", `
INSERT INTO edge_stats_progress (day, settled_through) VALUES (?::date, ?)
ON CONFLICT (day) DO UPDATE SET settled_through = EXCLUDED.settled_through`, []any{date, to.UTC()}},
	}
	for _, s := range steps {
		if _, err := tx.exec(ctx, s.query, s.args...); err != nil {
			return fmt.Errorf("%s: %w", s.what, err)
		}
	}
	return nil
}

// histogramFromCountsSQL is the 64-element histogram of a group whose rows
// carry a bucket and a count of requests in it.
func histogramFromCountsSQL(bucketColumn, countColumn string) string {
	parts := make([]string, histogramBuckets)
	for i := range parts {
		parts[i] = fmt.Sprintf("coalesce(sum(%s) FILTER (WHERE %s = %d), 0)", countColumn, bucketColumn, i)
	}
	return "ARRAY[" + strings.Join(parts, ", ") + "]::bigint[]"
}

// addArraysSQL adds two arrays of the same length element by element.
func addArraysSQL(a, b string) string {
	return "ARRAY(SELECT coalesce(x, 0) + coalesce(y, 0) FROM unnest(" + a + ", " + b + ") WITH ORDINALITY AS u(x, y, i) ORDER BY i)"
}
