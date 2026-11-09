package sitetraffic

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/core/statsperiod"
)

// The siteStatistics read: a period of website statistics for the deployables
// a caller may read (docs/superpowers/specs/
// 2026-11-08-site-and-email-statistics-design.md, section 7), from the tables
// the rollup keeps (rollup.go). A deployable's Statistics page reads one
// deployable; the Deployables Overview's Websites section reads every one the
// person may see, added together.
//
// # Narrowed under the caller, never by what they name (D11)
//
// The statistics tables are dedicated tables, outside the parser and the
// row-authz filter path, so the gate is the site's own, asked of the engine
// under the caller's actor, as for the traffic read (reader.go). Both paths
// walk every page of `sitesAll` under that actor (walkReadableSites). Named
// deployables are kept to the ones on it, plus a few looked up by id
// (readableNamedSites, for an archived one). A read that names none covers
// every deployable on it, kept to the organizations they name
// (readableSitesForAccounts). Either way an id or an organization the caller
// cannot reach is dropped, so a client's member who names another client's
// deployable or organization gets exactly the answer a deployable with no
// traffic gets -- no refusal, no figure, not even the day counting started.
// A named list whose ids are all blank names nothing, and answers the same
// (statisticsSites). The cluster's own system-owned surfaces are never in an
// answer (D16).
//
// # Absent is never zero
//
// A figure nothing measured is NULL. Every period before the edge classified
// requests has NULL visitors, visits, bounces and page views (the rollup
// writes them so), and a period with no row at all is null throughout. In a
// period that has rows, a bucket with none served nothing and reads 0 -- but
// only from the first day these deployables have any statistics; before it
// nothing was recorded and the bucket is null, so a chart of twelve months
// drawn the week this shipped does not show eleven months of zero requests.
// The visit figures read 0 only from MeasuredSince, the first day with visitor
// figures, for the same reason. A rate with nothing to divide by is null.
//
// # The 24-hour period
//
// Its series is the hourly table's, and so are its visits, bounces and page
// views: a visit is counted in the hour it started, so the hours add up. Its
// VISITORS cannot be added up from the hours -- one person seen in two hours
// is one visitor. Its window is hour-aligned and 24 hours long, so it is the
// end of one UTC day and the start of the next (windowParts), and the visitor
// hash changes at midnight (D1), so nobody is in both parts: the window's
// visitors are the first day's visitors LAST seen in its part plus the second
// day's FIRST seen in its part, each read off the day's row, which carries its
// visitors by the hour of their first and of their last page view
// (readWindowVisitors). Its top lists are the rollup's for the window, per
// deployable (edge_stats_window_top, publish.go), summed over the
// deployables. So the period reads a few rows per deployable and never the
// request log: until 2026-11-09 it counted both from edge_request itself, and
// at ten million requests a day over fifty sites its first statement outlasted
// the request pool's 8 s limit, so the period could not be read at all.
//
// What it reads is what the last run published, five minutes old at most;
// a deployable's first requests count from the run after them. The other
// periods read the daily tables, whose visitors are each day's, summed: a
// person who came on three days counts three times in a week, which the page
// says once, and one seen either side of midnight is two even in 24 hours.
//
// # Whole months
//
// A period's top lists sum the daily top rows of its days, at about six
// hundred rows per deployable and day; a staff read of twelve months over two
// hundred deployables summed 44 million of them and took 7.6 s of the 8 s
// limit. So each month a period covers whole is read from its monthly top
// lists instead (edge_stats_monthly_top, folded by the rollup from the
// month's final days), and only the days no monthly row covers -- today, the
// days of a month the period starts or ends inside -- from their daily rows
// (dailyTopSQL). A month keeps a hundred values per dimension, as a day does,
// and the read keeps fifty, so a value can drop out of a list at the month's
// fold as it can at a day's.
//
// # Twelve months have no previous period (D4)
//
// See core/statsperiod: the thirteen months before them are past the daily
// tables' 400 days. Previous and PreviousSeries are empty, and the payload
// says null.
//
// # One snapshot
//
// Every statement of a read runs in one read-only REPEATABLE READ transaction,
// so the totals, the series, the top lists and the per-site rows agree with
// each other even while a rollup replaces a day underneath. A rollup's
// transaction replaces a day's rows at once, and a month's top lists with the
// days they cover, so the snapshot sees all of a publication or none of it.

// StatisticsQuery is one statistics read.
type StatisticsQuery struct {
	// SiteIds names the deployables, as bare ids, at most maxSitesPerRead.
	// Empty means every deployable the caller may read. A list that is not
	// empty is named even when none of its ids is usable: blank ids name no
	// deployable, and the answer is empty.
	SiteIds []string
	// AccountIds keeps a read that names no deployables to these
	// organizations, as bare ids. It narrows what the caller may read and
	// never widens it, and it is not consulted when SiteIds names the
	// deployables: the names are already narrower than any organization. As
	// with SiteIds, a list of blank ids names no organization and keeps none.
	AccountIds []string
	// Period is one of statsperiod's.
	Period statsperiod.Period
	// Now is the instant the period is counted back from; zero is time.Now.
	Now time.Time
}

// Totals are a period's figures; nil is "not measured".
type Totals struct {
	Visitors, Visits, Bounces, PageViews, Requests, Errors, NotFound, Bytes, BotRequests *int64
	// ViewsPerVisit is PageViews / Visits and BounceRate is Bounces / Visits,
	// worked out from the period's sums rather than averaged from its
	// buckets; both nil without a visit.
	ViewsPerVisit, BounceRate *float64
	// MedianMs and P95Ms are merged from the period's response-time
	// histograms (histogram.go); nil when nothing was served.
	MedianMs, P95Ms *float64
}

// Point is one bucket of a series, starting At.
type Point struct {
	At                                                               time.Time
	Visitors, Visits, Bounces, PageViews, Requests, Errors, NotFound *int64
	P95Ms                                                            *float64
}

// TopEntry is one value of a top list, summed over the period.
type TopEntry struct {
	Value                       string
	Visitors, Visits, PageViews int64
}

// SiteTotals is one deployable's own figures over the period.
type SiteTotals struct {
	SiteId                      string
	Visitors, Visits, PageViews *int64
}

// Statistics is one read's answer.
type Statistics struct {
	Period statsperiod.Period
	// Bucket is statsperiod's: hour, day or month.
	Bucket string
	// Start and End are the period; PrevStart and PrevEnd the one before it,
	// zero when HasPrevious is false.
	Start, End, PrevStart, PrevEnd time.Time
	HasPrevious                    bool
	// MeasuredSince is the first day any of the deployables has visitor
	// figures, nil when none has: where the page says visitor counting began.
	MeasuredSince *time.Time
	Totals        Totals
	// Previous is the previous period's figures, every one nil without one.
	Previous Totals
	// Series has one point per bucket, oldest first, and PreviousSeries the
	// previous period's, aligned by offset; nil without a previous period.
	Series, PreviousSeries []Point
	// Top holds a list per dimension (topDimensions), each the
	// topEntriesKept leading values and an (other) remainder last.
	Top map[string][]TopEntry
	// Sites holds each deployable with any row in the period, most visitors
	// first.
	Sites []SiteTotals
}

// statisticsConcept is the concept of the one node the read answers with. Not
// a DSL concept: nothing stores it, and the client reads the node's payload.
const statisticsConcept = "integration:siteTraffic:statistics"

// topEntriesKept is how many values each top list answers before the rest
// fold into (other). The rollup keeps a hundred per day (topValuesKept), so a
// value can reach these fifty on any day it was seen.
const topEntriesKept = 50

// Statistics answers the query for the deployables the caller may read.
func (r *Reader) Statistics(ctx context.Context, q StatisticsQuery) (Statistics, error) {
	if len(q.SiteIds) > maxSitesPerRead {
		return Statistics{}, fmt.Errorf("a statistics read covers at most %d deployables at once; %d were asked for. Ask in pages, or name none to read every deployable you may see", maxSitesPerRead, len(q.SiteIds))
	}
	period, err := statsperiod.Parse(string(q.Period))
	if err != nil {
		return Statistics{}, err
	}
	if r == nil || r.db == nil {
		return Statistics{}, fmt.Errorf("this node has no database handle, so it cannot read website statistics")
	}
	if r.engine == nil {
		return Statistics{}, fmt.Errorf("this node has no engine handle, so it cannot check which deployables you may read")
	}
	now := q.Now
	if now.IsZero() {
		now = time.Now()
	}
	b := statsperiod.For(period, now)
	out := emptyStatistics(period, b)

	sites, err := r.statisticsSites(ctx, q)
	if err != nil {
		return Statistics{}, err
	}
	if len(sites) == 0 {
		// Nothing readable was named, or the caller reaches nothing: the
		// answer a deployable with no traffic gets, and nothing is read.
		return out, nil
	}
	err = statisticsTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		return fillStatistics(ctx, tx, sites, b, &out)
	})
	if err != nil {
		return Statistics{}, fmt.Errorf("read the website statistics: %w", err)
	}
	return out, nil
}

// statisticsSites narrows the query to bare site ids under the caller.
//
// WHETHER DEPLOYABLES WERE NAMED is the length of the list the caller sent,
// blanks included, and never the number of usable ids in it: a list of blank
// ids names no deployable and answers empty, where reading it as "none named"
// would answer every deployable the caller may read -- for staff, the whole
// cluster -- under the label of the one they asked about. The handler keeps
// an unusable element as "" for exactly this (namedIdsArg).
func (r *Reader) statisticsSites(ctx context.Context, q StatisticsQuery) ([]string, error) {
	if len(q.SiteIds) == 0 {
		return r.readableSitesForAccounts(ctx, q.AccountIds)
	}
	rows, err := r.readableNamedSites(ctx, q.SiteIds)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, s := range rows {
		// A system-owned surface is never measured (D16), named or not.
		if !s.systemOwned {
			out = append(out, s.id)
		}
	}
	return out, nil
}

// emptyStatistics is the answer before anything is read: the bounds, a null
// point for every bucket, and empty lists.
func emptyStatistics(p statsperiod.Period, b statsperiod.Bounds) Statistics {
	s := Statistics{
		Period: p, Bucket: b.Bucket,
		Start: b.Start, End: b.End, PrevStart: b.PrevStart, PrevEnd: b.PrevEnd, HasPrevious: b.HasPrevious,
		Series: nullPoints(b.Starts()),
		Top:    map[string][]TopEntry{},
		Sites:  []SiteTotals{},
	}
	if b.HasPrevious {
		s.PreviousSeries = nullPoints(b.PreviousStarts())
	}
	for _, d := range topDimensions {
		s.Top[d.name] = []TopEntry{}
	}
	return s
}

func nullPoints(starts []time.Time) []Point {
	out := make([]Point, len(starts))
	for i, at := range starts {
		out[i] = Point{At: at}
	}
	return out
}

// statisticsTx runs one read in a read-only REPEATABLE READ transaction, on the
// request pool and under the pool's own statement limit.
//
// THE POOL'S LIMIT IS THE READ'S, raised by nothing here. The database layer
// gives every serving backend a statement_timeout (MEMQL_DB_STATEMENT_TIMEOUT_MS,
// 8 s by default) set below pgdriver's 10 s client read deadline, so a slow
// statement is cancelled by the server, which rolls it back and says so
// (component/database, database.go). Raising it, as this read once did with a
// 30 s SET LOCAL for staff summing every deployable of a large cluster over
// ninety days, buys nothing: the client still stops listening at 10 s with an
// i/o timeout, and the scan runs on unheard for up to 20 s more, holding its
// snapshot, on every refresh of every Statistics page -- the orphan the pool's
// limit exists to prevent. A local limit below the pool's was the other
// choice, and is not taken: it would second-guess the operator's setting,
// lowering a raised one and raising a lowered one, where following the pool
// keeps one serving limit for the whole cluster. A read slower than the limit
// answers the server's statement-timeout error, which is the honest refusal
// (TestTheStatisticsReadStaysInsideThePoolsStatementLimit). What keeps a read
// inside it is what it reads: the rollup's tables, whole months from their
// monthly rows, and never the request log (TestStaffReadsAtScaleEvidence,
// statistics_scale_test.go).
func statisticsTx(ctx context.Context, db *bun.DB, fn func(context.Context, bun.Tx) error) error {
	opts := &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	return db.RunInTx(ctx, opts, fn)
}

// fillStatistics reads the tables into out, for sites over b.
func fillStatistics(ctx context.Context, tx bun.Tx, sites []string, b statsperiod.Bounds, out *Statistics) error {
	first, err := readFirstDays(ctx, tx, sites)
	if err != nil {
		return err
	}
	out.MeasuredSince = first.measured

	current, err := readBuckets(ctx, tx, sites, b.Bucket, b.Start, b.End)
	if err != nil {
		return err
	}
	out.Totals = totalsOf(current)
	out.Series = seriesOf(b.Starts(), current, first)
	if b.HasPrevious {
		previous, err := readBuckets(ctx, tx, sites, b.Bucket, b.PrevStart, b.PrevEnd)
		if err != nil {
			return err
		}
		out.Previous = totalsOf(previous)
		out.PreviousSeries = seriesOf(b.PreviousStarts(), previous, first)
	}

	perSite, err := readSiteTotals(ctx, tx, sites, b)
	if err != nil {
		return err
	}
	var top []topRow
	if b.Bucket == statsperiod.BucketHour {
		visitors, err := readWindowVisitors(ctx, tx, sites, b.Start, b.End)
		if err != nil {
			return err
		}
		out.Totals.Visitors = visitors.total()
		// Each deployable's visitors are the window's too, never its hours'
		// sum, which counts a person seen in two hours twice.
		for site, entry := range perSite {
			entry.Visitors = visitors[site]
		}
		for site, n := range visitors {
			if _, ok := perSite[site]; !ok {
				perSite[site] = &SiteTotals{SiteId: site, Visitors: n}
			}
		}
		if b.HasPrevious {
			previous, err := readWindowVisitors(ctx, tx, sites, b.PrevStart, b.PrevEnd)
			if err != nil {
				return err
			}
			out.Previous.Visitors = previous.total()
		}
		q, args := windowTopSQL(sites, b.End)
		if err := tx.NewRaw(q, args...).Scan(ctx, &top); err != nil {
			return fmt.Errorf("read the window's top lists: %w", err)
		}
	} else {
		q, args := dailyTopSQL(sites, b.Start, b.End)
		if err := tx.NewRaw(q, args...).Scan(ctx, &top); err != nil {
			return fmt.Errorf("read the top lists: %w", err)
		}
	}
	out.Top = topListsOf(top)
	out.Sites = sitesOf(perSite)
	return nil
}

// firstDays are the two thresholds the series is read against.
type firstDays struct {
	// recorded is the first day these deployables have any statistics row:
	// before it nothing was recorded.
	recorded *time.Time
	// measured is the first day with visitor figures: MeasuredSince.
	measured *time.Time
}

// readFirstDays reads both thresholds off the daily table, over everything it
// keeps for these deployables. The daily row of a day is written with its
// hours, in one transaction, so it answers for the hourly series as well.
func readFirstDays(ctx context.Context, tx bun.Tx, sites []string) (firstDays, error) {
	var row struct {
		Recorded *string `bun:"recorded"`
		Measured *string `bun:"measured"`
	}
	q := `SELECT to_char(min(day)::timestamp, 'YYYY-MM-DD') AS recorded,
	             to_char((min(day) FILTER (WHERE visitors IS NOT NULL))::timestamp, 'YYYY-MM-DD') AS measured
	        FROM edge_stats_daily
	       WHERE site_id IN (?)`
	if err := tx.NewRaw(q, bun.In(sites)).Scan(ctx, &row); err != nil {
		return firstDays{}, fmt.Errorf("read the first recorded day: %w", err)
	}
	var out firstDays
	var err error
	if out.recorded, err = parseDay(row.Recorded); err != nil {
		return firstDays{}, err
	}
	if out.measured, err = parseDay(row.Measured); err != nil {
		return firstDays{}, err
	}
	return out, nil
}

func parseDay(s *string) (*time.Time, error) {
	if s == nil {
		return nil, nil
	}
	d, err := time.Parse(time.DateOnly, *s)
	if err != nil {
		return nil, fmt.Errorf("read a day %q: %w", *s, err)
	}
	return &d, nil
}

// bucketRow is one bucket's figures summed over the deployables. The visit
// figures are NULL when no row of the bucket was measured: SUM over NULLs is
// NULL.
type bucketRow struct {
	Bucket      string  `bun:"bucket"`
	Visitors    *int64  `bun:"visitors"`
	Visits      *int64  `bun:"visits"`
	Bounces     *int64  `bun:"bounces"`
	PageViews   *int64  `bun:"page_views"`
	Requests    int64   `bun:"requests"`
	Errors      int64   `bun:"errors"`
	NotFound    int64   `bun:"not_found"`
	Bytes       int64   `bun:"bytes"`
	BotRequests int64   `bun:"bot_requests"`
	Hist        []int64 `bun:"duration_hist,array"`
}

// bucketKeyLayout is how a bucket's start is keyed on both sides: the SQL
// renders it in UTC and Go formats the bucket starts the same way.
const bucketKeyLayout = "2006-01-02T15:04:05Z"

func bucketKey(t time.Time) string { return t.UTC().Format(bucketKeyLayout) }

// statsSource is the table a period reads, the expression keying its bucket,
// its range predicate and that predicate's arguments. Hours are bounded by
// instants and days by 'YYYY-MM-DD' dates, so no session time zone moves a
// bucket (rollup.go, "Why every expression names UTC"); the month of a day is
// truncated as a timestamp WITHOUT time zone for the same reason.
func statsSource(bucket string, from, to time.Time) (table, key, where string, args []any) {
	switch bucket {
	case statsperiod.BucketHour:
		return "edge_stats_hourly",
			`to_char(hour AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
			"hour >= ? AND hour < ?", []any{from.UTC(), to.UTC()}
	case statsperiod.BucketMonth:
		return "edge_stats_daily",
			`to_char(date_trunc('month', day::timestamp), 'YYYY-MM-DD"T00:00:00Z"')`,
			"day >= ?::date AND day < ?::date", []any{dateArg(from), dateArg(to)}
	default:
		return "edge_stats_daily",
			`to_char(day::timestamp, 'YYYY-MM-DD"T00:00:00Z"')`,
			"day >= ?::date AND day < ?::date", []any{dateArg(from), dateArg(to)}
	}
}

// readBuckets reads [from, to) of one period's table, summed over the
// deployables per bucket and keyed by bucketKey.
func readBuckets(ctx context.Context, tx bun.Tx, sites []string, bucket string, from, to time.Time) (map[string]bucketRow, error) {
	table, key, where, args := statsSource(bucket, from, to)
	q := fmt.Sprintf(`
SELECT %s AS bucket,
       sum(visitors)::bigint AS visitors, sum(visits)::bigint AS visits,
       sum(bounces)::bigint AS bounces, sum(page_views)::bigint AS page_views,
       sum(requests)::bigint AS requests, sum(errors)::bigint AS errors,
       sum(not_found)::bigint AS not_found, sum(bytes)::bigint AS bytes,
       sum(bot_requests)::bigint AS bot_requests,
       %s AS duration_hist
  FROM %s
 WHERE site_id IN (?) AND %s
 GROUP BY 1`, key, histogramSumSQL("duration_hist"), table, where)
	var rows []bucketRow
	if err := tx.NewRaw(q, append([]any{bun.In(sites)}, args...)...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("read %s: %w", table, err)
	}
	out := make(map[string]bucketRow, len(rows))
	for _, row := range rows {
		out[row.Bucket] = row
	}
	return out, nil
}

// totalsOf sums a period's buckets. No bucket is no figure at all: every
// total nil, not zero.
func totalsOf(rows map[string]bucketRow) Totals {
	if len(rows) == 0 {
		return Totals{}
	}
	var t Totals
	var requests, errs, notFound, bytes, bots int64
	hists := make([][]int64, 0, len(rows))
	for _, r := range rows {
		t.Visitors = plus(t.Visitors, r.Visitors)
		t.Visits = plus(t.Visits, r.Visits)
		t.Bounces = plus(t.Bounces, r.Bounces)
		t.PageViews = plus(t.PageViews, r.PageViews)
		requests += r.Requests
		errs += r.Errors
		notFound += r.NotFound
		bytes += r.Bytes
		bots += r.BotRequests
		hists = append(hists, r.Hist)
	}
	t.Requests, t.Errors, t.NotFound, t.Bytes, t.BotRequests = &requests, &errs, &notFound, &bytes, &bots
	t.ViewsPerVisit = ratio(t.PageViews, t.Visits)
	t.BounceRate = ratio(t.Bounces, t.Visits)
	merged := mergeHistograms(hists...)
	t.MedianMs = percentile(merged, 0.5)
	t.P95Ms = percentile(merged, 0.95)
	return t
}

// seriesOf lays one period's buckets onto its bucket starts. See the package
// comment's "Absent is never zero" for the three answers a bucket with no row
// can have.
func seriesOf(starts []time.Time, rows map[string]bucketRow, first firstDays) []Point {
	out := nullPoints(starts)
	if len(rows) == 0 {
		// A period with no row at all: nothing was measured, null throughout.
		return out
	}
	for i, at := range starts {
		p := &out[i]
		if r, ok := rows[bucketKey(at)]; ok {
			p.Visitors, p.Visits, p.Bounces, p.PageViews = r.Visitors, r.Visits, r.Bounces, r.PageViews
			p.Requests, p.Errors, p.NotFound = count(r.Requests), count(r.Errors), count(r.NotFound)
			p.P95Ms = percentile(r.Hist, 0.95)
			continue
		}
		if !onOrAfter(first.recorded, at) {
			continue
		}
		p.Requests, p.Errors, p.NotFound = count(0), count(0), count(0)
		if onOrAfter(first.measured, at) {
			p.Visitors, p.Visits, p.Bounces, p.PageViews = count(0), count(0), count(0), count(0)
		}
	}
	return out
}

// onOrAfter reports whether a bucket starting at at begins on or after the
// day since; never, when there is no such day.
func onOrAfter(since *time.Time, at time.Time) bool {
	return since != nil && !at.Before(*since)
}

// plus adds v to sum. A nil v adds nothing and leaves an unmeasured sum
// unmeasured; one measured bucket makes the sum measured.
func plus(sum, v *int64) *int64 {
	if v == nil {
		return sum
	}
	n := *v
	if sum != nil {
		n += *sum
	}
	return &n
}

func count(v int64) *int64 { return &v }

// ratio is num / den, nil when either is unmeasured or there is nothing to
// divide by.
func ratio(num, den *int64) *float64 {
	if num == nil || den == nil || *den <= 0 {
		return nil
	}
	f := float64(*num) / float64(*den)
	return &f
}

func percentile(h []int64, q float64) *float64 {
	ms, ok := percentileMs(h, q)
	if !ok {
		return nil
	}
	return &ms
}

// siteRow is one deployable's visit figures over a period.
type siteRow struct {
	SiteId    string `bun:"site_id"`
	Visitors  *int64 `bun:"visitors"`
	Visits    *int64 `bun:"visits"`
	PageViews *int64 `bun:"page_views"`
}

// readSiteTotals reads each deployable's own figures over the period, from the
// period's table. For 24 hours the visitors are replaced from the window's
// days by the caller (readWindowVisitors).
func readSiteTotals(ctx context.Context, tx bun.Tx, sites []string, b statsperiod.Bounds) (map[string]*SiteTotals, error) {
	table, _, where, args := statsSource(b.Bucket, b.Start, b.End)
	q := fmt.Sprintf(`
SELECT site_id, sum(visitors)::bigint AS visitors, sum(visits)::bigint AS visits, sum(page_views)::bigint AS page_views
  FROM %s
 WHERE site_id IN (?) AND %s
 GROUP BY site_id`, table, where)
	var rows []siteRow
	if err := tx.NewRaw(q, append([]any{bun.In(sites)}, args...)...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("read each deployable's figures: %w", err)
	}
	out := make(map[string]*SiteTotals, len(rows))
	for _, r := range rows {
		out[r.SiteId] = &SiteTotals{SiteId: r.SiteId, Visitors: r.Visitors, Visits: r.Visits, PageViews: r.PageViews}
	}
	return out, nil
}

// sitesOf orders the deployables: most visitors first, then most page views,
// an unmeasured figure after every measured one, then by id.
func sitesOf(perSite map[string]*SiteTotals) []SiteTotals {
	out := make([]SiteTotals, 0, len(perSite))
	for _, s := range perSite {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if c := compareCounts(out[i].Visitors, out[j].Visitors); c != 0 {
			return c > 0
		}
		if c := compareCounts(out[i].PageViews, out[j].PageViews); c != 0 {
			return c > 0
		}
		return out[i].SiteId < out[j].SiteId
	})
	return out
}

// compareCounts orders two figures, a measured one above an unmeasured one.
func compareCounts(a, b *int64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	case *a > *b:
		return 1
	case *a < *b:
		return -1
	}
	return 0
}

// requestVisitors are each deployable's distinct visitors over a range, nil
// for a deployable none of whose days in it carries visitor figures.
type requestVisitors map[string]*int64

// total adds the deployables' visitors up. The visitor hash is keyed by the
// site (D1), so a person on two deployables is two visitors, and the sum of
// each deployable's distinct count is the distinct count of the whole.
func (v requestVisitors) total() *int64 {
	var sum *int64
	for _, n := range v {
		sum = plus(sum, n)
	}
	return sum
}

// readWindowVisitors counts each deployable's visitors over the hour-aligned
// 24-hour window [from, to) from the daily rows of its days: in the first
// day's part, the visitors whose last page view of that day is in it; in the
// second's, those whose first is (see "The 24-hour period"). A deployable is
// measured when one of its days in the window carries the hours' visitors.
func readWindowVisitors(ctx context.Context, tx bun.Tx, sites []string, from, to time.Time) (requestVisitors, error) {
	var parts []string
	var args []any
	for _, p := range windowParts(from, to) {
		column, slice := "first_hour_visitors", "[1:?]"
		bound := p.hour
		if p.suffix {
			column, slice, bound = "last_hour_visitors", "[?:24]", p.hour+1
		}
		parts = append(parts, fmt.Sprintf(`SELECT site_id, %[1]s IS NOT NULL AS measured,
       coalesce((SELECT sum(n) FROM unnest(%[1]s%[2]s) n), 0) AS visitors
  FROM edge_stats_daily WHERE site_id IN (?) AND day = ?::date`, column, slice))
		args = append(args, bound, bun.In(sites), dateArg(p.day))
	}
	var rows []struct {
		SiteId   string `bun:"site_id"`
		Measured bool   `bun:"measured"`
		Visitors int64  `bun:"visitors"`
	}
	q := `SELECT site_id, bool_or(measured) AS measured, sum(visitors) FILTER (WHERE measured)::bigint AS visitors
  FROM (` + strings.Join(parts, "\nUNION ALL\n") + `) p
 GROUP BY site_id`
	if err := tx.NewRaw(q, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("count the window's visitors: %w", err)
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

// topRow is one value of one dimension's list, after the fold.
type topRow struct {
	Dimension string `bun:"dimension"`
	Value     string `bun:"value"`
	Visitors  int64  `bun:"visitors"`
	Visits    int64  `bun:"visits"`
	PageViews int64  `bun:"page_views"`
}

// topFoldSQL ranks a `summed` relation of (dimension, value, visitors, visits,
// page_views) and folds every value past topEntriesKept into (other), summing
// what it folds.
//
// The rank is the rollup's (topInsert): most visitors, then most page views,
// then the value in byte order (COLLATE "C"), so the cut is the same on every
// database. A stored (other) -- each day's own fold, or a source somebody
// spelled that way -- is never ranked: it is a remainder, so it joins the
// remainder rather than taking one of the fifty places.
func topFoldSQL() string {
	return fmt.Sprintf(`,
ranked AS (
    SELECT dimension, value, visitors, visits, page_views,
           row_number() OVER (PARTITION BY dimension, value = '%[1]s'
                              ORDER BY visitors DESC, page_views DESC, value COLLATE "C" ASC) AS rank
      FROM summed
)
SELECT dimension,
       CASE WHEN value <> '%[1]s' AND rank <= %[2]d THEN value ELSE '%[1]s' END AS value,
       sum(visitors)::bigint AS visitors, sum(visits)::bigint AS visits, sum(page_views)::bigint AS page_views
  FROM ranked
 GROUP BY 1, 2`, topOther, topEntriesKept)
}

// dailyTopSQL sums the top lists over [from, to) and the deployables: each
// month the range covers whole from its monthly rows, and every day those do
// not cover from its daily rows (see "Whole months"). The visitors of a value
// are summed over its days, as the period's are.
//
// The days a month's rows cover are the ones edge_stats_month_day records
// with them, so a day is never read twice and never missed, whatever the
// rollup has folded so far. The uncovered days are listed first, so the
// daily rows are reached through the day's index for those days alone, not
// scanned for the range and thrown away.
func dailyTopSQL(sites []string, from, to time.Time) (string, []any) {
	q := `
WITH months AS (
    SELECT m::date AS month
      FROM generate_series(date_trunc('month', ?::date::timestamp), ?::date::timestamp, interval '1 month') m
     WHERE m::date >= ?::date AND (m + interval '1 month')::date <= ?::date
),
covered AS (
    SELECT c.day FROM edge_stats_month_day c
      JOIN months m ON c.day >= m.month AND c.day < (m.month + interval '1 month')::date
),
uncovered AS (
    SELECT d::date AS day
      FROM generate_series(?::date::timestamp, ?::date::timestamp - interval '1 day', interval '1 day') d
     WHERE NOT EXISTS (SELECT 1 FROM covered c WHERE c.day = d::date)
),
summed AS (
    SELECT dimension, value,
           sum(visitors)::bigint AS visitors, sum(visits)::bigint AS visits, sum(page_views)::bigint AS page_views
      FROM (SELECT dimension, value, visitors, visits, page_views FROM edge_stats_monthly_top
             WHERE site_id IN (?) AND month IN (SELECT month FROM months)
            UNION ALL
            SELECT dimension, value, visitors, visits, page_views FROM edge_stats_daily_top
             WHERE site_id IN (?) AND day IN (SELECT day FROM uncovered)) x
     GROUP BY 1, 2
)` + topFoldSQL()
	f, t := dateArg(from), dateArg(to)
	return q, []any{f, t, f, t, f, t, bun.In(sites), bun.In(sites)}
}

// windowTopSQL sums the 24-hour window's top lists over the deployables: the
// window ending at end, or when no run has published it yet -- the first
// seconds of an hour -- the one an hour before it. One older than that is not
// the last 24 hours: with the rollup stopped for hours, the read answers no
// list rather than an old one under the period's name
// (TestATwentyFourHourListOlderThanAnHourIsNotRead).
func windowTopSQL(sites []string, end time.Time) (string, []any) {
	q := `
WITH summed AS (
    SELECT dimension, value,
           sum(visitors)::bigint AS visitors, sum(visits)::bigint AS visits, sum(page_views)::bigint AS page_views
      FROM edge_stats_window_top
     WHERE site_id IN (?)
       AND window_end = (SELECT max(window_end) FROM edge_stats_window_top WHERE window_end <= ? AND window_end >= ?)
     GROUP BY 1, 2
)` + topFoldSQL()
	return q, []any{bun.In(sites), end.UTC(), end.Add(-time.Hour).UTC()}
}

// topListsOf groups the folded rows by dimension, every dimension present: the
// kept values in the rank's order, then the (other) remainder last.
func topListsOf(rows []topRow) map[string][]TopEntry {
	out := make(map[string][]TopEntry, len(topDimensions))
	for _, d := range topDimensions {
		out[d.name] = []TopEntry{}
	}
	for _, r := range rows {
		if _, ok := out[r.Dimension]; !ok {
			continue
		}
		out[r.Dimension] = append(out[r.Dimension], TopEntry{Value: r.Value, Visitors: r.Visitors, Visits: r.Visits, PageViews: r.PageViews})
	}
	for _, entries := range out {
		sort.Slice(entries, func(i, j int) bool {
			a, b := entries[i], entries[j]
			if (a.Value == topOther) != (b.Value == topOther) {
				return b.Value == topOther
			}
			if a.Visitors != b.Visitors {
				return a.Visitors > b.Visitors
			}
			if a.PageViews != b.PageViews {
				return a.PageViews > b.PageViews
			}
			// Byte order, as the SQL's COLLATE "C".
			return a.Value < b.Value
		})
	}
	return out
}

// Payload is the answer as the node carries it, which the OS parses
// (parseSiteStatistics): lowerCamel keys, instants in RFC3339, measuredSince
// as YYYY-MM-DD, and every figure nothing measured PRESENT AND NULL, so a
// client can tell "not measured" from a key it does not know. Without a
// previous period, previous, previousSeries, prevStart and prevEnd are null.
func (s Statistics) Payload() map[string]any {
	p := map[string]any{
		"period":         string(s.Period),
		"bucket":         s.Bucket,
		"start":          instant(s.Start),
		"end":            instant(s.End),
		"hasPrevious":    s.HasPrevious,
		"prevStart":      nil,
		"prevEnd":        nil,
		"measuredSince":  nil,
		"totals":         s.Totals.payload(),
		"previous":       nil,
		"series":         pointsPayload(s.Series),
		"previousSeries": nil,
		"top":            topPayload(s.Top),
		"sites":          sitesPayload(s.Sites),
	}
	if s.HasPrevious {
		p["prevStart"], p["prevEnd"] = instant(s.PrevStart), instant(s.PrevEnd)
		p["previous"] = s.Previous.payload()
		p["previousSeries"] = pointsPayload(s.PreviousSeries)
	}
	if s.MeasuredSince != nil {
		p["measuredSince"] = s.MeasuredSince.UTC().Format(time.DateOnly)
	}
	return p
}

func (t Totals) payload() map[string]any {
	return map[string]any{
		"visitors": countOrNull(t.Visitors), "visits": countOrNull(t.Visits),
		"bounces": countOrNull(t.Bounces), "pageViews": countOrNull(t.PageViews),
		"requests": countOrNull(t.Requests), "errors": countOrNull(t.Errors),
		"notFound": countOrNull(t.NotFound), "bytes": countOrNull(t.Bytes),
		"botRequests":   countOrNull(t.BotRequests),
		"viewsPerVisit": rateOrNull(t.ViewsPerVisit), "bounceRate": rateOrNull(t.BounceRate),
		"medianMs": rateOrNull(t.MedianMs), "p95Ms": rateOrNull(t.P95Ms),
	}
}

func pointsPayload(points []Point) []any {
	out := make([]any, len(points))
	for i, p := range points {
		out[i] = map[string]any{
			"at":       instant(p.At),
			"visitors": countOrNull(p.Visitors), "visits": countOrNull(p.Visits),
			"bounces": countOrNull(p.Bounces), "pageViews": countOrNull(p.PageViews),
			"requests": countOrNull(p.Requests), "errors": countOrNull(p.Errors),
			"notFound": countOrNull(p.NotFound), "p95Ms": rateOrNull(p.P95Ms),
		}
	}
	return out
}

// topPayload carries every dimension, an empty list where nothing was seen.
func topPayload(top map[string][]TopEntry) map[string]any {
	out := make(map[string]any, len(topDimensions))
	for _, d := range topDimensions {
		entries := top[d.name]
		list := make([]any, len(entries))
		for i, e := range entries {
			list[i] = map[string]any{"value": e.Value, "visitors": e.Visitors, "visits": e.Visits, "pageViews": e.PageViews}
		}
		out[d.name] = list
	}
	return out
}

func sitesPayload(sites []SiteTotals) []any {
	out := make([]any, len(sites))
	for i, s := range sites {
		out[i] = map[string]any{
			"siteId": s.SiteId, "visitors": countOrNull(s.Visitors),
			"visits": countOrNull(s.Visits), "pageViews": countOrNull(s.PageViews),
		}
	}
	return out
}

// countOrNull and rateOrNull spell an unmeasured figure as an untyped nil,
// which JSON writes as null.
func countOrNull(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func rateOrNull(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// instant renders a bound in RFC3339, UTC; a zero time is no bound at all.
func instant(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
