package sitetraffic

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
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
// # Why the 24-hour period reads the request log
//
// Its series is the hourly table's, and so are its visits, bounces and page
// views: a visit is counted in the hour it started, so the hours add up. Its
// VISITORS cannot be added up from the hours -- one person seen in two hours
// is one visitor -- so they are counted distinct from edge_request itself, and
// so are its top lists, for which there is no hourly table; their visits come
// from the rollup's own visit segmentation (visitSegmentation), never a second
// copy. The other periods read the daily tables, whose visitors are each
// day's, summed: the visitor hash changes at midnight (D1), so a person who
// came on three days counts three times in a week, which the page says once,
// and one seen either side of midnight is two even in 24 hours.
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
// transaction replaces a day's rows at once, so the snapshot sees all of a
// recompute or none of it.

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
	// The 24-hour period reads the request log; see statisticsTx.
	err = statisticsTx(ctx, r.db, b.Bucket == statsperiod.BucketHour, func(ctx context.Context, tx bun.Tx) error {
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
// snapshot (and, for 24 hours, the request log's lock below), on every refresh
// of every Statistics page -- the orphan the pool's limit exists to prevent. A
// local limit below the pool's was the other choice, and is not taken: it
// would second-guess the operator's setting, lowering a raised one and raising
// a lowered one, where following the pool keeps one serving limit for the whole
// cluster. A read slower than the limit answers the server's statement-timeout
// error, which is the honest refusal
// (TestTheStatisticsReadStaysInsideThePoolsStatementLimit).
//
// HOLDING THE REQUEST LOG, for a read of it, is the rollup's lesson
// (recomputeDay): TimescaleDB compresses a chunk by moving its rows and
// truncating it, which is not MVCC-safe, so a snapshot fixed before that
// commit can read the chunk as empty afterwards -- here, the previous 24 hours
// would read as nobody. ACCESS SHARE on the hypertable, taken before the first
// query fixes the snapshot, puts the snapshot after any compression that
// already holds a chunk; it does not conflict with the edge's inserts, and a
// compression reaching its exclusive step waits for this read to end. A read
// of the daily tables alone never touches the log and takes no lock.
func statisticsTx(ctx context.Context, db *bun.DB, holdRequestLog bool, fn func(context.Context, bun.Tx) error) error {
	opts := &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	return db.RunInTx(ctx, opts, func(ctx context.Context, tx bun.Tx) error {
		if holdRequestLog {
			if _, err := tx.ExecContext(ctx, "LOCK TABLE edge_request IN ACCESS SHARE MODE"); err != nil {
				return fmt.Errorf("hold the request log: %w", err)
			}
		}
		return fn(ctx, tx)
	})
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
		visitors, err := readRequestVisitors(ctx, tx, sites, b.Start, b.End)
		if err != nil {
			return err
		}
		out.Totals.Visitors = visitors.total()
		// Each deployable's visitors are the log's too, never its hours' sum:
		// one whose requests the log no longer holds has none to count, and
		// reads null rather than an overcount. And one whose requests are not
		// yet rolled up -- its first minutes -- is listed with its visitors.
		for site, entry := range perSite {
			entry.Visitors = visitors[site]
		}
		for site, n := range visitors {
			if _, ok := perSite[site]; !ok {
				perSite[site] = &SiteTotals{SiteId: site, Visitors: n}
			}
		}
		if b.HasPrevious {
			previous, err := readRequestVisitors(ctx, tx, sites, b.PrevStart, b.PrevEnd)
			if err != nil {
				return err
			}
			out.Previous.Visitors = previous.total()
		}
		q, args := requestTopSQL(sites, b.Start, b.End)
		if err := tx.NewRaw(q, args...).Scan(ctx, &top); err != nil {
			return fmt.Errorf("read the top lists from the request log: %w", err)
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
// period's table. For 24 hours the visitors are replaced from the request log
// by the caller.
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
// for a deployable none of whose requests in it the edge classified.
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

// readRequestVisitors counts each deployable's visitors over [from, to) from
// edge_request: distinct over the page views with a visitor id, NULL when no
// request in the range was measured.
func readRequestVisitors(ctx context.Context, tx bun.Tx, sites []string, from, to time.Time) (requestVisitors, error) {
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

// dailyTopSQL sums the daily top lists over [from, to) and the deployables.
// The visitors of a value are summed over its days, as the period's are.
func dailyTopSQL(sites []string, from, to time.Time) (string, []any) {
	q := `
WITH summed AS (
    SELECT dimension, value,
           sum(visitors)::bigint AS visitors, sum(visits)::bigint AS visits, sum(page_views)::bigint AS page_views
      FROM edge_stats_daily_top
     WHERE site_id IN (?) AND day >= ?::date AND day < ?::date
     GROUP BY 1, 2
)` + topFoldSQL()
	return q, []any{bun.In(sites), dateArg(from), dateArg(to)}
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
func requestTopSQL(sites []string, from, to time.Time) (string, []any) {
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
