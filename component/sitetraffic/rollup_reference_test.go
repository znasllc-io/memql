package sitetraffic

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// rollup_reference_test.go -- THE RECOMPUTE THE FOLD REPLACED, kept as the
// reference the fold is held to (fold.go; TestTheFoldAgreesWithARecomputeWhereverItIsCut),
// and the request-log read the 24-hour period replaced
// (TestTheTwentyFourHourReadAgreesWithTheRequestLog). Until 2026-11-09 these
// statements were the rollup and the read: every run recomputed a day from
// the whole request log, one statement per table, and the 24-hour period
// counted its visitors and top lists from edge_request itself. They answer
// the same figures from scratch, over the whole range at once, written
// independently of the fold's state, which is what makes them worth
// comparing against. Nothing outside the tests calls them.

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
func referenceBucketInsert(table, column string, bucketOf func(string) string, from, to time.Time) (string, []any) {
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

// topInsert is the INSERT of one day's top lists, with its arguments.
//
// Each list keeps the hundred values with the most visitors, ties broken by
// page views and then by value in byte order (COLLATE "C", so the cut is the
// same on every database), and folds the rest into (other), summing their
// visitors, visits and page views. The fold groups by the label, so a real
// value that happens to read "(other)" -- a utm_source is the client's to
// write -- joins the fold instead of colliding with it on the primary key and
// failing the whole day for every site.
func referenceTopInsert(table string, from, to time.Time) (string, []any) {
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
INSERT INTO %s (site_id, day, dimension, value, visitors, visits, page_views)
SELECT site_id, day, dimension,
       CASE WHEN rank <= %d THEN value ELSE '%s' END,
       sum(visitors)::bigint, sum(visits)::bigint, sum(page_views)::bigint
  FROM ranked
 GROUP BY 1, 2, 3, 4`,
		visits,
		dimensionValues(func(_, viewed, _ string) string { return viewed }),
		dimensionValues(func(_, _, entered string) string { return entered }),
		table, topValuesKept, topOther)
	return q, append(args, from.UTC(), to.UTC())
}

// readRequestVisitors counts each deployable's visitors over [from, to) from
// edge_request: distinct over the page views with a visitor id, NULL when no
// request in the range was measured.
func referenceRequestVisitors(ctx context.Context, tx bun.Tx, sites []string, from, to time.Time) (requestVisitors, error) {
	var rows []struct {
		SiteId   string `bun:"site_id"`
		Measured bool   `bun:"measured"`
		Visitors int64  `bun:"visitors"`
	}
	q := `
SELECT site_id, bool_or(measured) AS measured,
       count(DISTINCT visitor) FILTER (WHERE page_view AND visitor <> '') AS visitors
  FROM edge_request
 WHERE site_id IN (?) AND served_at >= ? AND served_at < ?
 GROUP BY site_id`
	if err := tx.NewRaw(q, bun.In(sites), from.UTC(), to.UTC()).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("count the visitors in the request log: %w", err)
	}
	out := make(requestVisitors, len(rows))
	for _, r := range rows {
		if r.Measured {
			out[r.SiteId] = count(r.Visitors)
		} else {
			out[r.SiteId] = nil
		}
	}
	return out, nil
}

// requestTopSQL counts the top lists over [from, to) from edge_request, for
// the 24-hour period: a value's visitors distinct over its page views, its
// page views counted, and its visits the visits that STARTED in the range
// under it, from the rollup's segmentation. The dimension expressions are the
// rollup's (topDimensions), so a value reads the same in both.
//
// A FULL join where the rollup's day has a LEFT one. Within a day every row
// of a visit is in the day, so every value a visit entered under was also
// viewed. A range that ends mid-day can cut a visit after its first row, and
// its entry source -- the first of its rows with a source -- may come after
// the range; that visit still started in the range and is counted.
func referenceRequestTopSQL(sites []string, from, to time.Time) (string, []any) {
	visits, args := visitSegmentation(from, to)
	q := fmt.Sprintf(`
WITH visits AS (%s),
views AS (
    SELECT site_id, visitor, path, source, country, device, browser, os
      FROM edge_request
     WHERE site_id IN (?) AND served_at >= ? AND served_at < ? AND page_view
),
viewed AS (
    SELECT d.dimension, d.value,
           count(DISTINCT (site_id, visitor)) FILTER (WHERE visitor <> '') AS visitors,
           count(*) AS page_views
      FROM views
     CROSS JOIN LATERAL (VALUES %s) AS d(dimension, value)
     WHERE d.value IS NOT NULL
     GROUP BY 1, 2
),
entered AS (
    SELECT d.dimension, d.value, count(*) AS visits
      FROM visits
     CROSS JOIN LATERAL (VALUES %s) AS d(dimension, value)
     WHERE visits.site_id IN (?) AND d.value IS NOT NULL
     GROUP BY 1, 2
),
summed AS (
    SELECT coalesce(v.dimension, e.dimension) AS dimension, coalesce(v.value, e.value) AS value,
           coalesce(v.visitors, 0) AS visitors, coalesce(e.visits, 0) AS visits,
           coalesce(v.page_views, 0) AS page_views
      FROM viewed v
      FULL JOIN entered e ON e.dimension = v.dimension AND e.value = v.value
)%s`,
		visits,
		dimensionValues(func(_, viewed, _ string) string { return viewed }),
		dimensionValues(func(_, _, entered string) string { return entered }),
		topFoldSQL())
	args = append(args, bun.In(sites), from.UTC(), to.UTC(), bun.In(sites))
	return q, args
}
