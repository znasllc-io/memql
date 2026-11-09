package sitetraffic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/statsperiod"
)

// statistics_db_test.go -- the siteStatistics read against the real tables,
// narrowed by a real engine.
//
// Nothing here is provable in process: the sums, the distinct visitors and the
// visit windows are SQL's, and which deployables a caller reaches is the
// site's own row-authz tier, asked of the engine under the caller's actor.
//
// THE TESTS OWN A BAND OF DATES, 2037 and 2038, and write the statistics
// tables DIRECTLY inside it, as the rollup would have. They never run the
// rollup: its retention pass deletes every site's rows older than its cutoff,
// across the shared tables, so a run here with a 2037 or 2038 `now` would
// delete the rollup tests' band (rollup_db_test.go, 2034-2036) and every older
// row anybody keeps. The rollup tests' own `now`s are in 2036, so their
// retention never reaches this band. The band is
// cleared before and after each test, for every site in it, because a caller
// who reaches every deployable reads every deployable's rows in it: rows left
// behind by an interrupted run would land in the next run's totals. No other
// test writes there. The dates are in the future for the reader tests'
// reason: the hypertable's compression and retention jobs act on chunks older
// than the real clock, and must not act on a fixture mid-test.
//
// Postgres-gated: CI's db-tests lane runs this package with MEMQL_REQUIRE_DB=1.

var (
	statsBandStart = time.Date(2037, 1, 1, 0, 0, 0, 0, time.UTC)
	statsBandEnd   = time.Date(2039, 1, 1, 0, 0, 0, 0, time.UTC)
)

// statisticsTest boots the shared engine and clears the band around the test.
func statisticsTest(t *testing.T) (*memql.MemQLEngine, *bun.DB) {
	t.Helper()
	eng, db, _ := trafficEngine(t)
	clearStatisticsBand(t, db)
	t.Cleanup(func() { clearStatisticsBand(t, db) })
	return eng, db
}

func clearStatisticsBand(t *testing.T, db *bun.DB) {
	t.Helper()
	ctx := context.Background()
	from, to := dateArg(statsBandStart), dateArg(statsBandEnd)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"DELETE FROM edge_request WHERE served_at >= ? AND served_at < ?", []any{statsBandStart, statsBandEnd}},
		{"DELETE FROM edge_stats_hourly WHERE hour >= ? AND hour < ?", []any{statsBandStart, statsBandEnd}},
		{"DELETE FROM edge_stats_daily WHERE day >= ?::date AND day < ?::date", []any{from, to}},
		{"DELETE FROM edge_stats_daily_top WHERE day >= ?::date AND day < ?::date", []any{from, to}},
		{"DELETE FROM edge_stats_window_top WHERE window_end >= ? AND window_end <= ?", []any{statsBandStart, statsBandEnd}},
		{"DELETE FROM edge_stats_monthly_top WHERE month >= ?::date AND month < ?::date", []any{from, to}},
		{"DELETE FROM edge_stats_month_day WHERE day >= ?::date AND day < ?::date", []any{from, to}},
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("clear the statistics tests' band (%s): %v", q.sql, err)
		}
	}
}

// inStatsBand refuses a fixture outside the band, so a mistyped year cannot
// write where the clearing never reaches.
func inStatsBand(t *testing.T, when time.Time) {
	t.Helper()
	if when.Before(statsBandStart) || !when.Before(statsBandEnd) {
		t.Fatalf("fixture at %s is outside the statistics tests' band [%s, %s)", when, statsBandStart, statsBandEnd)
	}
}

// figures is one hourly or daily row as the rollup would have written it.
// firstHours and lastHours are a daily row's visitors by the hour of their
// first and of their last page view, written only when set.
type figures struct {
	visitors, visits, bounces, pageViews    *int64
	requests, errors, notFound, bytes, bots int64
	hist                                    []int64
	firstHours, lastHours                   []int64
}

// seenIn is f with its visitors seen, by first and last hour, as given: one
// {first, last} pair per visitor.
func (f figures) seenIn(visitors ...[2]int) figures {
	f.firstHours, f.lastHours = make([]int64, 24), make([]int64, 24)
	for _, v := range visitors {
		f.firstHours[v[0]]++
		f.lastHours[v[1]]++
	}
	return f
}

// hoursOrNull is a daily row's hour counts as a statement argument, NULL when
// unset.
func hoursOrNull(h []int64) any {
	if h == nil {
		return nil
	}
	return pgdialect.Array(h)
}

// measured is a row the edge classified, so its visit figures are numbers.
// Each request took 3 ms and served 100 bytes.
func measured(visitors, visits, bounces, pageViews, requests int64) figures {
	return figures{
		visitors: ptr64(visitors), visits: ptr64(visits), bounces: ptr64(bounces), pageViews: ptr64(pageViews),
		requests: requests, bytes: 100 * requests, hist: durations(requests, 3_000_000),
	}
}

// unmeasured is a row from before the edge classified requests: the health
// counters every request has always fed, and NULL visit figures.
func unmeasured(requests int64) figures {
	return figures{requests: requests, bytes: 100 * requests, hist: durations(requests, 3_000_000)}
}

func (f figures) with(errors, notFound, bots int64) figures {
	f.errors, f.notFound, f.bots = errors, notFound, bots
	return f
}

// durations is the histogram of n requests that each took ns.
func durations(n, ns int64) []int64 {
	h := make([]int64, histogramBuckets)
	h[histogramBucket(ns)] = n
	return h
}

func seedDailyStats(t *testing.T, db *bun.DB, site string, day time.Time, f figures) {
	t.Helper()
	inStatsBand(t, day)
	if _, err := db.ExecContext(context.Background(), `INSERT INTO edge_stats_daily
		(site_id, day, visitors, visits, bounces, page_views, requests, errors, not_found, bytes, bot_requests, duration_hist,
		 first_hour_visitors, last_hour_visitors)
		VALUES (?, ?::date, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		site, dateArg(day), f.visitors, f.visits, f.bounces, f.pageViews,
		f.requests, f.errors, f.notFound, f.bytes, f.bots, pgdialect.Array(f.hist),
		hoursOrNull(f.firstHours), hoursOrNull(f.lastHours)); err != nil {
		t.Fatalf("seed the daily row of %s on %s: %v", site, dateArg(day), err)
	}
}

func seedHourlyStats(t *testing.T, db *bun.DB, site string, hour time.Time, f figures) {
	t.Helper()
	inStatsBand(t, hour)
	if _, err := db.ExecContext(context.Background(), `INSERT INTO edge_stats_hourly
		(site_id, hour, visitors, visits, bounces, page_views, requests, errors, not_found, bytes, bot_requests, duration_hist)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		site, hour.UTC(), f.visitors, f.visits, f.bounces, f.pageViews,
		f.requests, f.errors, f.notFound, f.bytes, f.bots, pgdialect.Array(f.hist)); err != nil {
		t.Fatalf("seed the hourly row of %s at %s: %v", site, hour.Format(time.RFC3339), err)
	}
}

func seedTopStats(t *testing.T, db *bun.DB, site string, day time.Time, dimension, value string, visitors, visits, pageViews int64) {
	t.Helper()
	inStatsBand(t, day)
	if _, err := db.ExecContext(context.Background(), `INSERT INTO edge_stats_daily_top
		(site_id, day, dimension, value, visitors, visits, page_views) VALUES (?, ?::date, ?, ?, ?, ?, ?)`,
		site, dateArg(day), dimension, value, visitors, visits, pageViews); err != nil {
		t.Fatalf("seed the %s %q top row of %s on %s: %v", dimension, value, site, dateArg(day), err)
	}
}

// seedWindowTop writes one row of the 24-hour window's top lists that ends at
// end, as the rollup writes them.
func seedWindowTop(t *testing.T, db *bun.DB, site string, end time.Time, dimension, value string, visitors, visits, pageViews int64) {
	t.Helper()
	inStatsBand(t, end)
	if _, err := db.ExecContext(context.Background(), `INSERT INTO edge_stats_window_top
		(site_id, window_end, dimension, value, visitors, visits, page_views) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		site, end.UTC(), dimension, value, visitors, visits, pageViews); err != nil {
		t.Fatalf("seed the %s %q window row of %s: %v", dimension, value, site, err)
	}
}

// seedBandRequests writes raw request rows, refusing any outside the band.
func seedBandRequests(t *testing.T, db *bun.DB, site string, rows ...Row) {
	t.Helper()
	for _, r := range rows {
		inStatsBand(t, r.ServedAt)
	}
	seedRequests(t, db, site, rows...)
}

func readStatistics(t *testing.T, ctx context.Context, eng *memql.MemQLEngine, db *bun.DB, q StatisticsQuery) Statistics {
	t.Helper()
	s, err := NewReader(db, eng).Statistics(ctx, q)
	if err != nil {
		t.Fatalf("Statistics(%+v): %v", q, err)
	}
	return s
}

// developerCtx is staff below the cluster owner: developer rank reaches every
// organization's deployables as a rule, and is not the cluster owner.
func developerCtx(userId string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userId, Role: auth.RoleDeveloper})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

// systemActorCtx is the seed materializer's shape: the only actor that creates
// a system-owned site.
func systemActorCtx() context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "system:statistics-test", Role: auth.RoleOwner, Synthetic: true, Unranked: true,
	})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "system:statistics-test"})
}

// accountOf is the organization seedDeployable made for a site, bare.
func accountOf(siteId string) string { return "account-" + siteId }

// addMember puts userId in the organization seedDeployable made for siteId,
// as a Member: one of the client's people, who did not create the deployable
// and reaches it only through the organization's group.
func addMember(t *testing.T, eng *memql.MemQLEngine, siteId, userId string) {
	t.Helper()
	group := "group-" + siteId
	q := fmt.Sprintf(`mutation writeGroupMembership(membershipId: %s, groupId: %s, userId: %s, accountId: %s, origin: "added", status: "active")`,
		langparser.QuoteString(group+"-"+userId), langparser.QuoteString(group), langparser.QuoteString(userId), langparser.QuoteString(accountOf(siteId)))
	if _, err := eng.Execute(auth.ContextWithInternalOrigin(operatorCtx("traffic-seeder")), q); err != nil {
		t.Fatalf("add %s to the organization of %s: %v", userId, siteId, err)
	}
}

// seedSystemOwnedSite creates a site the way the seed materializer creates the
// cluster's own surfaces, and returns its bare id. The site is deleted again
// when the test ends -- by the system actor, the one caller the systemOwned
// delete guard admits (component/memql/platform_site_delete_guard.go) -- so a
// cluster owner's list on the shared database does not gain a system-owned
// fixture every run.
func seedSystemOwnedSite(t *testing.T, eng *memql.MemQLEngine, slug string) string {
	t.Helper()
	id := "site-traffic-" + slug
	q := fmt.Sprintf(`mutation createSite(siteId: %s, hostname: %s, bundleRef: %s, status: "live", systemOwned: true)`,
		langparser.QuoteString(id), langparser.QuoteString(shortHost(slug)+"."+trafficTestDomain), langparser.QuoteString("blob://sites/"+id+"/v1/"))
	if _, err := eng.Execute(systemActorCtx(), q); err != nil {
		t.Fatalf("seed the system-owned site: %v", err)
	}
	t.Cleanup(func() {
		if _, err := eng.Execute(systemActorCtx(), fmt.Sprintf(`mutation deleteSite(siteId: %s)`, langparser.QuoteString(id))); err != nil {
			t.Errorf("delete the system-owned fixture %s: %v", id, err)
		}
	})
	return id
}

// ---------------------------------------------------------------------------
// Rendering: every figure in one line, nil as "null", so a failure shows the
// whole row and a null can never pass for a zero.
// ---------------------------------------------------------------------------

func num(p *int64) string {
	if p == nil {
		return "null"
	}
	return strconv.FormatInt(*p, 10)
}

func totalsLine(x Totals) string {
	return fmt.Sprintf("visitors=%s visits=%s bounces=%s pageViews=%s requests=%s errors=%s notFound=%s bytes=%s botRequests=%s",
		num(x.Visitors), num(x.Visits), num(x.Bounces), num(x.PageViews),
		num(x.Requests), num(x.Errors), num(x.NotFound), num(x.Bytes), num(x.BotRequests))
}

func pointLine(p Point) string {
	return fmt.Sprintf("%s visitors=%s visits=%s bounces=%s pageViews=%s requests=%s errors=%s notFound=%s",
		p.At.UTC().Format(time.RFC3339), num(p.Visitors), num(p.Visits), num(p.Bounces), num(p.PageViews),
		num(p.Requests), num(p.Errors), num(p.NotFound))
}

// zeroLine and nullLine are a bucket that served nothing, and one nothing
// measured.
func zeroLine(at time.Time) string {
	return at.UTC().Format(time.RFC3339) + " visitors=0 visits=0 bounces=0 pageViews=0 requests=0 errors=0 notFound=0"
}

func nullLine(at time.Time) string {
	return at.UTC().Format(time.RFC3339) + " visitors=null visits=null bounces=null pageViews=null requests=null errors=null notFound=null"
}

func topLine(entries []TopEntry) string {
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = fmt.Sprintf("%s %d/%d/%d", e.Value, e.Visitors, e.Visits, e.PageViews)
	}
	return strings.Join(parts, "; ")
}

func sitesLine(sites []SiteTotals) string {
	parts := make([]string, len(sites))
	for i, s := range sites {
		parts[i] = fmt.Sprintf("%s %s/%s/%s", s.SiteId, num(s.Visitors), num(s.Visits), num(s.PageViews))
	}
	return strings.Join(parts, "; ")
}

func wantSeries(t *testing.T, what string, got []Point, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d points, want %d", what, len(got), len(want))
	}
	for i := 0; i < len(got) && i < len(want); i++ {
		if line := pointLine(got[i]); line != want[i] {
			t.Errorf("%s[%d]:\n  got  %s\n  want %s", what, i, line, want[i])
		}
	}
}

func wantNear(t *testing.T, what string, got *float64, want, tolerance float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = null, want %v", what, want)
		return
	}
	if math.Abs(*got-want) > tolerance {
		t.Errorf("%s = %v, want %v (within %v)", what, *got, want, tolerance)
	}
}

func isNullPoint(p Point) bool {
	return p.Visitors == nil && p.Visits == nil && p.Bounces == nil && p.PageViews == nil &&
		p.Requests == nil && p.Errors == nil && p.NotFound == nil && p.P95Ms == nil
}

// wantNothing is the answer a caller gets for deployables they may not read:
// the same as for deployables with no traffic, so it confirms nothing.
func wantNothing(t *testing.T, who string, s Statistics) {
	t.Helper()
	if len(s.Sites) != 0 {
		t.Errorf("%s: sites %s, want none", who, sitesLine(s.Sites))
	}
	none := totalsLine(Totals{})
	if line := totalsLine(s.Totals); line != none {
		t.Errorf("%s: totals %s, want every figure null", who, line)
	}
	if line := totalsLine(s.Previous); line != none {
		t.Errorf("%s: previous %s, want every figure null", who, line)
	}
	for _, f := range []*float64{s.Totals.ViewsPerVisit, s.Totals.BounceRate, s.Totals.MedianMs, s.Totals.P95Ms} {
		if f != nil {
			t.Errorf("%s: a rate or a percentile is %v, want null", who, *f)
		}
	}
	if s.MeasuredSince != nil {
		t.Errorf("%s: measuredSince %s, want none -- even the day somebody else's counting started is theirs", who, s.MeasuredSince.Format(time.DateOnly))
	}
	for i, p := range s.Series {
		if !isNullPoint(p) {
			t.Errorf("%s: series[%d] = %s, want null", who, i, pointLine(p))
		}
	}
	for i, p := range s.PreviousSeries {
		if !isNullPoint(p) {
			t.Errorf("%s: previousSeries[%d] = %s, want null", who, i, pointLine(p))
		}
	}
	for dim, entries := range s.Top {
		if len(entries) != 0 {
			t.Errorf("%s: top %s = %s, want none", who, dim, topLine(entries))
		}
	}
}

// payloadOf is the answer as the client receives it.
func payloadOf(t *testing.T, s Statistics) (map[string]any, string) {
	t.Helper()
	raw, err := json.Marshal(s.Payload())
	if err != nil {
		t.Fatalf("encode the payload: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode the payload: %v", err)
	}
	return out, string(raw)
}

// ---------------------------------------------------------------------------
// The figures
// ---------------------------------------------------------------------------

// The headline: a week of one deployable, summed from its days, against the
// week before it, with its top lists and its own row.
func TestStatisticsForOneSiteOverSevenDays(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	owner := "user-stats-" + sfx
	site := seedDeployable(t, eng, owner, sfx)
	now := time.Date(2037, 6, 15, 10, 17, 0, 0, time.UTC)

	seedDailyStats(t, db, site, utcDay(2037, 5, 20), measured(9, 9, 9, 9, 9))                // before both periods: only where counting starts
	seedDailyStats(t, db, site, utcDay(2037, 6, 4), measured(4, 5, 2, 10, 30).with(2, 1, 1)) // the previous week
	seedDailyStats(t, db, site, utcDay(2037, 6, 9), measured(2, 2, 1, 5, 12).with(0, 1, 0))
	seedDailyStats(t, db, site, utcDay(2037, 6, 12), measured(3, 4, 2, 7, 20).with(1, 0, 2))
	seedDailyStats(t, db, site, utcDay(2037, 6, 15), measured(1, 1, 0, 2, 4))      // today, so far
	seedDailyStats(t, db, site, utcDay(2037, 6, 16), measured(50, 50, 50, 50, 50)) // past End: the range is half-open
	for _, r := range []struct {
		day                        time.Time
		dimension, value           string
		visitors, visits, pageView int64
	}{
		{utcDay(2037, 6, 9), "page", "/", 2, 2, 3},
		{utcDay(2037, 6, 9), "page", "/pricing", 1, 0, 2},
		{utcDay(2037, 6, 12), "page", "/", 2, 3, 4},
		{utcDay(2037, 6, 12), "page", "/docs", 2, 1, 3},
		{utcDay(2037, 6, 9), "source", "direct", 1, 1, 1},
		{utcDay(2037, 6, 9), "source", "news.example", 1, 1, 1},
		{utcDay(2037, 6, 12), "source", "direct", 3, 4, 4},
		{utcDay(2037, 6, 9), "country", "NL", 2, 2, 5},
		{utcDay(2037, 6, 12), "country", "DE", 2, 3, 5},
		{utcDay(2037, 6, 12), "country", topUnknown, 1, 1, 2},
		{utcDay(2037, 6, 4), "page", "/last-week", 4, 5, 10}, // the previous week has no top lists
		{utcDay(2037, 6, 16), "page", "/tomorrow", 50, 50, 50},
	} {
		seedTopStats(t, db, site, r.day, r.dimension, r.value, r.visitors, r.visits, r.pageView)
	}

	got := readStatistics(t, userCtx(owner), eng, db, StatisticsQuery{SiteIds: []string{site}, Period: statsperiod.Last7Days, Now: now})

	if got.Period != statsperiod.Last7Days || got.Bucket != statsperiod.BucketDay || !got.HasPrevious {
		t.Errorf("period %q bucket %q hasPrevious %v, want 7d, day, true", got.Period, got.Bucket, got.HasPrevious)
	}
	if !got.Start.Equal(utcDay(2037, 6, 9)) || !got.End.Equal(utcDay(2037, 6, 16)) ||
		!got.PrevStart.Equal(utcDay(2037, 6, 2)) || !got.PrevEnd.Equal(utcDay(2037, 6, 9)) {
		t.Errorf("bounds [%s, %s) previous [%s, %s), want [06-09, 06-16) and [06-02, 06-09)", got.Start, got.End, got.PrevStart, got.PrevEnd)
	}
	if got.MeasuredSince == nil || !got.MeasuredSince.Equal(utcDay(2037, 5, 20)) {
		t.Errorf("measuredSince = %v, want 2037-05-20, the first day with visitors", got.MeasuredSince)
	}
	if line, want := totalsLine(got.Totals), "visitors=6 visits=7 bounces=3 pageViews=14 requests=36 errors=1 notFound=1 bytes=3600 botRequests=2"; line != want {
		t.Errorf("totals:\n  got  %s\n  want %s", line, want)
	}
	wantNear(t, "viewsPerVisit", got.Totals.ViewsPerVisit, 2, 1e-9)
	wantNear(t, "bounceRate", got.Totals.BounceRate, 3.0/7, 1e-9)
	// Every request took 3 ms, and the percentiles are the histogram's: inside
	// the bucket holding 3 ms, interpolated (histogram.go).
	week := durations(36, 3_000_000)
	median, _ := percentileMs(week, 0.5)
	p95, _ := percentileMs(week, 0.95)
	wantNear(t, "medianMs", got.Totals.MedianMs, median, 1e-9)
	wantNear(t, "p95Ms", got.Totals.P95Ms, p95, 1e-9)
	if line, want := totalsLine(got.Previous), "visitors=4 visits=5 bounces=2 pageViews=10 requests=30 errors=2 notFound=1 bytes=3000 botRequests=1"; line != want {
		t.Errorf("previous:\n  got  %s\n  want %s", line, want)
	}

	wantSeries(t, "series", got.Series, []string{
		"2037-06-09T00:00:00Z visitors=2 visits=2 bounces=1 pageViews=5 requests=12 errors=0 notFound=1",
		zeroLine(utcDay(2037, 6, 10)),
		zeroLine(utcDay(2037, 6, 11)),
		"2037-06-12T00:00:00Z visitors=3 visits=4 bounces=2 pageViews=7 requests=20 errors=1 notFound=0",
		zeroLine(utcDay(2037, 6, 13)),
		zeroLine(utcDay(2037, 6, 14)),
		"2037-06-15T00:00:00Z visitors=1 visits=1 bounces=0 pageViews=2 requests=4 errors=0 notFound=0",
	})
	wantSeries(t, "previousSeries", got.PreviousSeries, []string{
		zeroLine(utcDay(2037, 6, 2)),
		zeroLine(utcDay(2037, 6, 3)),
		"2037-06-04T00:00:00Z visitors=4 visits=5 bounces=2 pageViews=10 requests=30 errors=2 notFound=1",
		zeroLine(utcDay(2037, 6, 5)),
		zeroLine(utcDay(2037, 6, 6)),
		zeroLine(utcDay(2037, 6, 7)),
		zeroLine(utcDay(2037, 6, 8)),
	})
	if len(got.Series) == 7 {
		wantNear(t, "series[0].p95Ms", got.Series[0].P95Ms, p95, 1e-9)
		if got.Series[1].P95Ms != nil {
			t.Errorf("series[1].p95Ms = %v, want null: a day that served nothing has no response time", *got.Series[1].P95Ms)
		}
	}

	for dim, want := range map[string]string{
		"page":    "/ 4/5/7; /docs 2/1/3; /pricing 1/0/2",
		"source":  "direct 4/5/5; news.example 1/1/1",
		"country": "DE 2/3/5; NL 2/2/5; (unknown) 1/1/2", // a tie on visitors and views falls to the value, in byte order
		"device":  "",
		"browser": "",
		"os":      "",
	} {
		entries, ok := got.Top[dim]
		if !ok {
			t.Errorf("top has no %s list, want one (empty when nothing was seen)", dim)
		}
		if line := topLine(entries); line != want {
			t.Errorf("top %s:\n  got  %s\n  want %s", dim, line, want)
		}
	}
	if line, want := sitesLine(got.Sites), site+" 6/7/14"; line != want {
		t.Errorf("sites = %s, want %s", line, want)
	}

	// The payload is what the OS parses (Task 15): lowerCamel keys, RFC3339
	// instants, a date for measuredSince, and a zero that stays a zero.
	p, _ := payloadOf(t, got)
	for key, want := range map[string]any{
		"period": "7d", "bucket": "day", "hasPrevious": true,
		"start": "2037-06-09T00:00:00Z", "end": "2037-06-16T00:00:00Z",
		"prevStart": "2037-06-02T00:00:00Z", "prevEnd": "2037-06-09T00:00:00Z",
		"measuredSince": "2037-05-20",
	} {
		if p[key] != want {
			t.Errorf("payload %s = %v, want %v", key, p[key], want)
		}
	}
	totals, _ := p["totals"].(map[string]any)
	for key, want := range map[string]any{"pageViews": 14.0, "notFound": 1.0, "botRequests": 2.0, "viewsPerVisit": 2.0} {
		if totals[key] != want {
			t.Errorf("payload totals.%s = %v, want %v", key, totals[key], want)
		}
	}
	series, _ := p["series"].([]any)
	if len(series) != 7 {
		t.Fatalf("payload series has %d points, want 7", len(series))
	}
	if second, _ := series[1].(map[string]any); second["visitors"] != 0.0 || second["at"] != "2037-06-10T00:00:00Z" || second["p95Ms"] != nil {
		t.Errorf("payload series[1] = %v, want visitors 0 (not null) at 2037-06-10T00:00:00Z and a null p95Ms", second)
	}
	if prev, _ := p["previousSeries"].([]any); len(prev) != 7 {
		t.Errorf("payload previousSeries has %d points, want 7", len(prev))
	}
	top, _ := p["top"].(map[string]any)
	pages, _ := top["page"].([]any)
	if len(pages) == 0 {
		t.Fatal("payload top.page is empty")
	}
	if first, _ := pages[0].(map[string]any); first["value"] != "/" || first["pageViews"] != 7.0 {
		t.Errorf("payload top.page[0] = %v, want / with 7 pageViews", first)
	}
	sites, _ := p["sites"].([]any)
	if len(sites) != 1 {
		t.Fatalf("payload sites = %v, want one", sites)
	}
	if one, _ := sites[0].(map[string]any); one["siteId"] != site || one["visits"] != 7.0 {
		t.Errorf("payload sites[0] = %v, want %s with 7 visits", one, site)
	}
}

// 24 HOURS COUNTS VISITORS DISTINCT OVER THE WINDOW. A visitor seen in two
// hours is one visitor in the period, which adding up the hourly rows cannot
// say: the window is the end of one day and the start of the next, and its
// visitors are the first day's last seen in its part and the second day's
// first seen in its part, off the days' rows. The visits, bounces and page
// views add up from the hours, and the top lists are the rollup's for the
// window (the rollup's side: rollup_incremental_db_test.go).
func TestStatisticsForTwentyFourHoursCountDistinctVisitors(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	owner := "user-stats-" + sfx
	site := seedDeployable(t, eng, owner, sfx)
	now := time.Date(2037, 7, 20, 10, 17, 0, 0, time.UTC) // the period is [07-19 11:00, 07-20 11:00)
	d18, d19, d20 := utcDay(2037, 7, 18), utcDay(2037, 7, 19), utcDay(2037, 7, 20)

	// The requests these rows stand for, as the rollup reads them:
	//   07-18 15:00 c /p                    (the previous 24 hours)
	//   07-19 10:50 d /d1, 11:10 d /d2      (d's visit starts before the period and continues into it)
	//   07-19 12:00 a /a direct, 12:01 an asset, 13:30 a /b (ninety minutes on: a's second visit)
	//   07-20 09:00 b /a news.example
	//   07-20 11:30 x /late                 (past End)
	// The hours as the rollup writes them: each visit counted in the hour it
	// started, each hour's visitors distinct within the hour.
	for _, h := range []struct {
		at time.Time
		f  figures
	}{
		{dayAt(d19, 10, 0), measured(1, 1, 0, 1, 1)}, // the previous 24 hours: d's visit starts here
		{dayAt(d19, 11, 0), measured(1, 0, 0, 1, 1)}, // d: no visit starts here
		{dayAt(d19, 12, 0), measured(1, 1, 1, 1, 2)},
		{dayAt(d19, 13, 0), measured(1, 1, 1, 1, 1)},
		{dayAt(d20, 9, 0), measured(1, 1, 1, 1, 1)},
		{dayAt(d18, 15, 0), measured(1, 1, 1, 1, 1)},
		{dayAt(d20, 11, 0), measured(1, 1, 1, 1, 1)}, // past End
	} {
		seedHourlyStats(t, db, site, h.at, h.f)
	}
	// The days, with each visitor's first and last hour: c 15-15; d 10-11
	// and a 12-13; b 09-09 and x 11-11.
	seedDailyStats(t, db, site, d18, measured(1, 1, 1, 1, 1).seenIn([2]int{15, 15}))
	seedDailyStats(t, db, site, d19, measured(2, 3, 2, 4, 5).seenIn([2]int{10, 11}, [2]int{12, 13}))
	seedDailyStats(t, db, site, d20, measured(2, 2, 2, 2, 2).seenIn([2]int{9, 9}, [2]int{11, 11}))
	// The window's top lists, as the rollup writes them: d's visit began
	// before the period, so /d2 is viewed here and entered nowhere; a entered
	// on /a and then on /b, b on /a.
	end := dayAt(d20, 11, 0)
	for _, r := range []struct {
		dimension, value         string
		visitors, visits, viewed int64
	}{
		{"page", "/a", 2, 2, 2}, {"page", "/b", 1, 1, 1}, {"page", "/d2", 1, 0, 1},
		{"source", "direct", 1, 1, 1}, {"source", "news.example", 1, 1, 1},
		{"country", "NL", 3, 3, 4}, {"device", "desktop", 3, 3, 4}, {"browser", "Firefox", 3, 3, 4}, {"os", "Linux", 3, 3, 4},
	} {
		seedWindowTop(t, db, site, end, r.dimension, r.value, r.visitors, r.visits, r.viewed)
	}
	// An older window's rows, which a read of this one never sums.
	seedWindowTop(t, db, site, end.Add(-time.Hour), "page", "/stale", 9, 9, 9)

	got := readStatistics(t, userCtx(owner), eng, db, StatisticsQuery{SiteIds: []string{site}, Period: statsperiod.Last24Hours, Now: now})

	if got.Bucket != statsperiod.BucketHour || !got.Start.Equal(dayAt(d19, 11, 0)) || !got.End.Equal(dayAt(d20, 11, 0)) {
		t.Errorf("bucket %q [%s, %s), want hour [07-19 11:00, 07-20 11:00)", got.Bucket, got.Start, got.End)
	}
	// Three visitors -- d and a last seen on 07-19 from 11:00, b first seen
	// on 07-20 before it -- where the four hours they were seen in add up to
	// four.
	if line, want := totalsLine(got.Totals), "visitors=3 visits=3 bounces=3 pageViews=4 requests=5 errors=0 notFound=0 bytes=500 botRequests=0"; line != want {
		t.Errorf("totals:\n  got  %s\n  want %s", line, want)
	}
	// The previous 24 hours hold c, last seen on 07-18 at 15:00, and d, first
	// seen on 07-19 at 10:00: two visitors.
	if line, want := totalsLine(got.Previous), "visitors=2 visits=2 bounces=1 pageViews=2 requests=2 errors=0 notFound=0 bytes=200 botRequests=0"; line != want {
		t.Errorf("previous:\n  got  %s\n  want %s", line, want)
	}

	want := make([]string, 24)
	for i := range want {
		want[i] = zeroLine(got.Start.Add(time.Duration(i) * time.Hour))
	}
	want[0] = "2037-07-19T11:00:00Z visitors=1 visits=0 bounces=0 pageViews=1 requests=1 errors=0 notFound=0"
	want[1] = "2037-07-19T12:00:00Z visitors=1 visits=1 bounces=1 pageViews=1 requests=2 errors=0 notFound=0"
	want[2] = "2037-07-19T13:00:00Z visitors=1 visits=1 bounces=1 pageViews=1 requests=1 errors=0 notFound=0"
	want[22] = "2037-07-20T09:00:00Z visitors=1 visits=1 bounces=1 pageViews=1 requests=1 errors=0 notFound=0"
	wantSeries(t, "series", got.Series, want)
	prev := make([]string, 24)
	for i := range prev {
		prev[i] = zeroLine(got.PrevStart.Add(time.Duration(i) * time.Hour))
	}
	prev[4] = "2037-07-18T15:00:00Z visitors=1 visits=1 bounces=1 pageViews=1 requests=1 errors=0 notFound=0"
	prev[23] = "2037-07-19T10:00:00Z visitors=1 visits=1 bounces=0 pageViews=1 requests=1 errors=0 notFound=0"
	wantSeries(t, "previousSeries", got.PreviousSeries, prev)

	for dim, want := range map[string]string{
		"page":    "/a 2/2/2; /b 1/1/1; /d2 1/0/1",
		"source":  "direct 1/1/1; news.example 1/1/1",
		"country": "NL 3/3/4",
		"device":  "desktop 3/3/4",
		"browser": "Firefox 3/3/4",
		"os":      "Linux 3/3/4",
	} {
		if line := topLine(got.Top[dim]); line != want {
			t.Errorf("top %s:\n  got  %s\n  want %s", dim, line, want)
		}
	}
	if line, want := sitesLine(got.Sites), site+" 3/3/4"; line != want {
		t.Errorf("sites = %s, want %s: a deployable's own visitors are distinct too", line, want)
	}
}

// A 24-HOUR LIST IS THE WINDOW'S, OR THE ONE JUST BEFORE IT. In the first
// seconds of an hour the rollup has not yet published the window that ends at
// the next one, and the read takes the window an hour earlier; a list older
// than that -- the rollup has not run for hours -- is not the last 24 hours,
// and the read answers no list rather than present it as them.
func TestATwentyFourHourListOlderThanAnHourIsNotRead(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	owner := "user-stats-" + sfx
	site := seedDeployable(t, eng, owner, sfx)
	now := time.Date(2038, 2, 3, 10, 0, 5, 0, time.UTC) // the window ends at 11:00
	seedWindowTop(t, db, site, dayAt(utcDay(2038, 2, 3), 10, 0), "page", "/an-hour-old", 1, 1, 1)
	got := readStatistics(t, userCtx(owner), eng, db, StatisticsQuery{SiteIds: []string{site}, Period: statsperiod.Last24Hours, Now: now})
	if line := topLine(got.Top["page"]); line != "/an-hour-old 1/1/1" {
		t.Errorf("in the first seconds of the hour the pages read %q, want the window an hour earlier's", line)
	}
	got = readStatistics(t, userCtx(owner), eng, db, StatisticsQuery{SiteIds: []string{site}, Period: statsperiod.Last24Hours, Now: now.Add(2 * time.Hour)})
	if line := topLine(got.Top["page"]); line != "" {
		t.Errorf("three hours on, the pages read %q, want none: that list is not the last 24 hours", line)
	}
}

// ABSENT IS NOT ZERO. Days before the edge classified requests have health
// figures and NULL visitors; a day after that with no row served nothing; a
// day before anything was recorded is null throughout. And a period with no
// row at all is null, not a page of zeroes.
func TestAPeriodBeforeVisitorCountingIsNullNotZero(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	owner := "user-stats-" + sfx
	site := seedDeployable(t, eng, owner, sfx)
	for d, requests := range map[int]int64{1: 10, 2: 11, 3: 12} {
		seedDailyStats(t, db, site, utcDay(2037, 8, d), unmeasured(requests))
	}
	seedDailyStats(t, db, site, utcDay(2037, 8, 5), measured(2, 2, 1, 3, 5))
	now := time.Date(2037, 8, 6, 12, 0, 0, 0, time.UTC) // the period is [07-31, 08-07)

	got := readStatistics(t, userCtx(owner), eng, db, StatisticsQuery{SiteIds: []string{site}, Period: statsperiod.Last7Days, Now: now})

	if got.MeasuredSince == nil || !got.MeasuredSince.Equal(utcDay(2037, 8, 5)) {
		t.Errorf("measuredSince = %v, want 2037-08-05", got.MeasuredSince)
	}
	if line, want := totalsLine(got.Totals), "visitors=2 visits=2 bounces=1 pageViews=3 requests=38 errors=0 notFound=0 bytes=3800 botRequests=0"; line != want {
		t.Errorf("totals:\n  got  %s\n  want %s", line, want)
	}
	wantSeries(t, "series", got.Series, []string{
		nullLine(utcDay(2037, 7, 31)), // before anything was recorded for this deployable
		"2037-08-01T00:00:00Z visitors=null visits=null bounces=null pageViews=null requests=10 errors=0 notFound=0",
		"2037-08-02T00:00:00Z visitors=null visits=null bounces=null pageViews=null requests=11 errors=0 notFound=0",
		"2037-08-03T00:00:00Z visitors=null visits=null bounces=null pageViews=null requests=12 errors=0 notFound=0",
		"2037-08-04T00:00:00Z visitors=null visits=null bounces=null pageViews=null requests=0 errors=0 notFound=0", // served nothing, before counting began
		"2037-08-05T00:00:00Z visitors=2 visits=2 bounces=1 pageViews=3 requests=5 errors=0 notFound=0",
		zeroLine(utcDay(2037, 8, 6)),
	})
	// The week before has no row at all: null, not zero.
	if line, none := totalsLine(got.Previous), totalsLine(Totals{}); line != none {
		t.Errorf("previous = %s, want every figure null", line)
	}
	for i, p := range got.PreviousSeries {
		if !isNullPoint(p) {
			t.Errorf("previousSeries[%d] = %s, want null", i, pointLine(p))
		}
	}
	p, _ := payloadOf(t, got)
	series, _ := p["series"].([]any)
	if len(series) == 7 {
		gap, _ := series[4].(map[string]any)
		if v, present := gap["visitors"]; !present || v != nil || gap["requests"] != 0.0 {
			t.Errorf("payload series[4] = %v, want visitors present and null beside requests 0", gap)
		}
	}
	if prev, _ := p["previous"].(map[string]any); prev == nil || prev["visitors"] != nil {
		t.Errorf("payload previous = %v, want an object of nulls", p["previous"])
	}

	// A deployable whose rows all predate visitor counting: visitors are not
	// measured anywhere, and its requests still are.
	early := seedDeployable(t, eng, owner+"-early", "early-"+sfx)
	for d := 1; d <= 3; d++ {
		seedDailyStats(t, db, early, utcDay(2037, 8, d), unmeasured(4))
	}
	got = readStatistics(t, userCtx(owner+"-early"), eng, db, StatisticsQuery{SiteIds: []string{early}, Period: statsperiod.Last7Days, Now: now})
	if got.MeasuredSince != nil {
		t.Errorf("measuredSince = %s, want none: no row of this deployable has visitors", got.MeasuredSince.Format(time.DateOnly))
	}
	if line, want := totalsLine(got.Totals), "visitors=null visits=null bounces=null pageViews=null requests=12 errors=0 notFound=0 bytes=1200 botRequests=0"; line != want {
		t.Errorf("totals:\n  got  %s\n  want %s", line, want)
	}
	if got.Totals.ViewsPerVisit != nil || got.Totals.BounceRate != nil {
		t.Errorf("viewsPerVisit %v bounceRate %v, want both null with visits unmeasured", got.Totals.ViewsPerVisit, got.Totals.BounceRate)
	}
	if line := totalsLine(got.Totals); strings.Contains(line, "visitors=0") {
		t.Errorf("an unmeasured period reads as nobody: %s", line)
	}
	wantSeries(t, "series of the early deployable", got.Series, []string{
		nullLine(utcDay(2037, 7, 31)),
		"2037-08-01T00:00:00Z visitors=null visits=null bounces=null pageViews=null requests=4 errors=0 notFound=0",
		"2037-08-02T00:00:00Z visitors=null visits=null bounces=null pageViews=null requests=4 errors=0 notFound=0",
		"2037-08-03T00:00:00Z visitors=null visits=null bounces=null pageViews=null requests=4 errors=0 notFound=0",
		"2037-08-04T00:00:00Z visitors=null visits=null bounces=null pageViews=null requests=0 errors=0 notFound=0",
		"2037-08-05T00:00:00Z visitors=null visits=null bounces=null pageViews=null requests=0 errors=0 notFound=0",
		"2037-08-06T00:00:00Z visitors=null visits=null bounces=null pageViews=null requests=0 errors=0 notFound=0",
	})
}

// Twelve months are thirteen monthly buckets -- the current month and the
// twelve before it -- each the sum of its days, and no comparison (D4).
func TestTwelveMonthsSumsDaysIntoMonths(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	owner := "user-stats-" + sfx
	site := seedDeployable(t, eng, owner, sfx)
	now := time.Date(2038, 3, 10, 9, 0, 0, 0, time.UTC)                                 // the period is [2037-03-01, 2038-04-01)
	seedDailyStats(t, db, site, utcDay(2037, 2, 28), measured(100, 100, 100, 100, 100)) // before Start
	seedDailyStats(t, db, site, utcDay(2037, 3, 5), measured(1, 1, 1, 2, 3))
	seedDailyStats(t, db, site, utcDay(2037, 3, 20), measured(2, 3, 1, 4, 6))
	seedDailyStats(t, db, site, utcDay(2037, 12, 31), measured(5, 5, 5, 5, 5))
	seedDailyStats(t, db, site, utcDay(2038, 1, 1), measured(7, 8, 1, 9, 10))
	seedDailyStats(t, db, site, utcDay(2038, 3, 9), measured(1, 1, 0, 1, 2))
	seedDailyStats(t, db, site, utcDay(2038, 4, 1), measured(100, 100, 100, 100, 100)) // past End
	seedTopStats(t, db, site, utcDay(2037, 3, 5), "page", "/", 1, 1, 2)
	seedTopStats(t, db, site, utcDay(2038, 1, 1), "page", "/", 7, 8, 9)
	seedTopStats(t, db, site, utcDay(2038, 1, 1), "page", "/new", 1, 1, 1)
	seedTopStats(t, db, site, utcDay(2037, 2, 28), "page", "/old", 100, 100, 100)

	got := readStatistics(t, userCtx(owner), eng, db, StatisticsQuery{SiteIds: []string{site}, Period: statsperiod.Last12Months, Now: now})

	if got.Bucket != statsperiod.BucketMonth || !got.Start.Equal(utcDay(2037, 3, 1)) || !got.End.Equal(utcDay(2038, 4, 1)) {
		t.Errorf("bucket %q [%s, %s), want month [2037-03-01, 2038-04-01)", got.Bucket, got.Start, got.End)
	}
	want := []string{"2037-03-01T00:00:00Z visitors=3 visits=4 bounces=2 pageViews=6 requests=9 errors=0 notFound=0"}
	for m := time.April; m <= time.November; m++ {
		want = append(want, zeroLine(utcDay(2037, m, 1)))
	}
	want = append(want,
		"2037-12-01T00:00:00Z visitors=5 visits=5 bounces=5 pageViews=5 requests=5 errors=0 notFound=0",
		"2038-01-01T00:00:00Z visitors=7 visits=8 bounces=1 pageViews=9 requests=10 errors=0 notFound=0",
		zeroLine(utcDay(2038, 2, 1)),
		"2038-03-01T00:00:00Z visitors=1 visits=1 bounces=0 pageViews=1 requests=2 errors=0 notFound=0",
	)
	wantSeries(t, "series", got.Series, want)
	if line, want := totalsLine(got.Totals), "visitors=16 visits=18 bounces=8 pageViews=21 requests=26 errors=0 notFound=0 bytes=2600 botRequests=0"; line != want {
		t.Errorf("totals:\n  got  %s\n  want %s", line, want)
	}
	if line := topLine(got.Top["page"]); line != "/ 8/9/11; /new 1/1/1" {
		t.Errorf("top page = %s, want / 8/9/11; /new 1/1/1", line)
	}

	// No previous period: nothing to compare, and nothing pretending to be.
	if got.HasPrevious || !got.PrevStart.IsZero() || !got.PrevEnd.IsZero() || got.PreviousSeries != nil {
		t.Errorf("hasPrevious %v previous [%s, %s) with %d points, want none", got.HasPrevious, got.PrevStart, got.PrevEnd, len(got.PreviousSeries))
	}
	if line, none := totalsLine(got.Previous), totalsLine(Totals{}); line != none {
		t.Errorf("previous = %s, want nothing", line)
	}
	p, _ := payloadOf(t, got)
	for _, key := range []string{"previous", "previousSeries", "prevStart", "prevEnd"} {
		if v, present := p[key]; !present || v != nil {
			t.Errorf("payload %s = %v (present %v), want present and null", key, v, present)
		}
	}
	if p["hasPrevious"] != false || p["bucket"] != "month" {
		t.Errorf("payload hasPrevious %v bucket %v, want false and month", p["hasPrevious"], p["bucket"])
	}
	if series, _ := p["series"].([]any); len(series) != 13 {
		t.Errorf("payload series has %d points, want 13", len(series))
	}
}

// Views per visit and bounce rate are worked out from the SUMMED page views,
// visits and bounces, never averaged from each day's rate, and both are null
// when there was no visit to divide by -- while the visits stay a zero.
func TestVisitsBouncesAndTheirRates(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	owner := "user-stats-" + sfx
	site := seedDeployable(t, eng, owner, sfx)
	now := time.Date(2037, 9, 10, 12, 0, 0, 0, time.UTC) // [08-12, 09-11), previous [07-13, 08-12)
	seedDailyStats(t, db, site, utcDay(2037, 9, 1), measured(3, 4, 1, 10, 10))
	seedDailyStats(t, db, site, utcDay(2037, 9, 5), measured(2, 2, 2, 2, 2))
	seedDailyStats(t, db, site, utcDay(2037, 9, 9), measured(4, 4, 1, 13, 13))
	seedDailyStats(t, db, site, utcDay(2037, 8, 1), measured(0, 0, 0, 0, 7)) // requests, and nobody visiting

	got := readStatistics(t, userCtx(owner), eng, db, StatisticsQuery{SiteIds: []string{site}, Period: statsperiod.Last30Days, Now: now})

	// 25 views over 10 visits and 4 bounces in 10 visits. The days' own rates
	// average to 2.25 and 0.5.
	wantNear(t, "viewsPerVisit", got.Totals.ViewsPerVisit, 2.5, 1e-9)
	wantNear(t, "bounceRate", got.Totals.BounceRate, 0.4, 1e-9)
	if num(got.Totals.Visits) != "10" || num(got.Totals.Bounces) != "4" || num(got.Totals.PageViews) != "25" {
		t.Errorf("visits %s bounces %s pageViews %s, want 10, 4 and 25", num(got.Totals.Visits), num(got.Totals.Bounces), num(got.Totals.PageViews))
	}
	if len(got.Series) == 30 {
		if line, want := pointLine(got.Series[20]), "2037-09-01T00:00:00Z visitors=3 visits=4 bounces=1 pageViews=10 requests=10 errors=0 notFound=0"; line != want {
			t.Errorf("series[20]:\n  got  %s\n  want %s", line, want)
		}
	} else {
		t.Errorf("%d points, want 30", len(got.Series))
	}

	if num(got.Previous.Visits) != "0" || num(got.Previous.PageViews) != "0" {
		t.Errorf("previous visits %s pageViews %s, want 0 and 0: measured, and nobody came", num(got.Previous.Visits), num(got.Previous.PageViews))
	}
	if got.Previous.ViewsPerVisit != nil || got.Previous.BounceRate != nil {
		t.Errorf("previous viewsPerVisit %v bounceRate %v, want null with no visit to divide by", got.Previous.ViewsPerVisit, got.Previous.BounceRate)
	}
}

// A median of medians is not a median: the period's percentiles come from the
// days' histograms added together.
func TestHealthPercentilesComeFromMergedHistograms(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	owner := "user-stats-" + sfx
	site := seedDeployable(t, eng, owner, sfx)
	now := time.Date(2037, 10, 10, 12, 0, 0, 0, time.UTC) // [10-04, 10-11)
	fast, slow := measured(1, 1, 1, 1, 900), measured(1, 1, 1, 1, 100)
	fast.hist = durations(900, 10_000_000)
	slow.hist = durations(100, 500_000_000)
	seedDailyStats(t, db, site, utcDay(2037, 10, 5), fast)
	seedDailyStats(t, db, site, utcDay(2037, 10, 6), slow)

	got := readStatistics(t, userCtx(owner), eng, db, StatisticsQuery{SiteIds: []string{site}, Period: statsperiod.Last7Days, Now: now})

	merged := mergeHistograms(fast.hist, slow.hist)
	median, _ := percentileMs(merged, 0.5)
	p95, _ := percentileMs(merged, 0.95)
	wantNear(t, "medianMs", got.Totals.MedianMs, median, 1e-9)
	wantNear(t, "p95Ms", got.Totals.P95Ms, p95, 1e-9)
	// And within the histogram's ten per cent of the truth: the 950th of 1000
	// requests is one of the slow hundred, at 500 ms.
	wantNear(t, "medianMs", got.Totals.MedianMs, 10, 1)
	wantNear(t, "p95Ms", got.Totals.P95Ms, 500, 50)
	if len(got.Series) != 7 {
		t.Fatalf("%d points, want 7", len(got.Series))
	}
	// Each day's own, from its own histogram -- and their mean, the answer a
	// median of medians would give, is nowhere near the period's.
	fastP95, _ := percentileMs(fast.hist, 0.95)
	slowP95, _ := percentileMs(slow.hist, 0.95)
	wantNear(t, "series[1].p95Ms (10-05)", got.Series[1].P95Ms, fastP95, 1e-9)
	wantNear(t, "series[2].p95Ms (10-06)", got.Series[2].P95Ms, slowP95, 1e-9)
	if mean := (fastP95 + slowP95) / 2; got.Totals.P95Ms != nil && math.Abs(*got.Totals.P95Ms-mean) < 100 {
		t.Errorf("p95Ms = %v is the mean of the days' own (%v), want the merged histogram's", *got.Totals.P95Ms, mean)
	}
	if got.Series[0].P95Ms != nil {
		t.Errorf("series[0].p95Ms = %v, want null for a day that served nothing", *got.Series[0].P95Ms)
	}
}

// A top list answers fifty values and an (other) remainder, last. A stored
// (other) -- a day's own fold -- is a remainder, never a ranked value, however
// large it is.
func TestTopListsKeepFiftyValuesAndFoldTheRest(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	owner := "user-stats-" + sfx
	site := seedDeployable(t, eng, owner, sfx)
	day := utcDay(2038, 5, 10)
	for i := 0; i < 53; i++ {
		seedTopStats(t, db, site, day, "page", fmt.Sprintf("/p%02d", i), int64(100-i), 1, 1)
	}
	seedTopStats(t, db, site, day, "page", topOther, 1000, 7, 7)

	got := readStatistics(t, userCtx(owner), eng, db, StatisticsQuery{SiteIds: []string{site}, Period: statsperiod.Last7Days, Now: dayAt(day, 12, 0)})

	pages := got.Top["page"]
	if len(pages) != topEntriesKept+1 {
		t.Fatalf("%d entries, want %d values and the remainder: %s", len(pages), topEntriesKept, topLine(pages))
	}
	for i := 0; i < topEntriesKept; i++ {
		if want := fmt.Sprintf("/p%02d", i); pages[i].Value != want {
			t.Errorf("entry %d is %s, want %s", i, pages[i].Value, want)
		}
	}
	// /p50, /p51 and /p52 fold, with the day's own remainder.
	if last := pages[topEntriesKept]; last.Value != topOther || last.Visitors != 50+49+48+1000 || last.Visits != 3+7 || last.PageViews != 3+7 {
		t.Errorf("the remainder is %s %d/%d/%d, want (other) 1147/10/10", last.Value, last.Visitors, last.Visits, last.PageViews)
	}
}

// 24 hours never adds hourly visitors up, for the period or for a deployable:
// with no first and last hours on its days there is nobody to count distinct,
// and the figure is null rather than the hours' overcount.
func TestTwentyFourHoursNeverAddsUpHourlyVisitors(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	owner := "user-stats-" + sfx
	site := seedDeployable(t, eng, owner, sfx)
	day := utcDay(2038, 6, 1)
	seedDailyStats(t, db, site, day, measured(1, 2, 2, 2, 2))
	seedHourlyStats(t, db, site, dayAt(day, 8, 0), measured(1, 1, 1, 1, 1))
	seedHourlyStats(t, db, site, dayAt(day, 9, 0), measured(1, 1, 1, 1, 1))

	got := readStatistics(t, userCtx(owner), eng, db, StatisticsQuery{SiteIds: []string{site}, Period: statsperiod.Last24Hours, Now: dayAt(day, 10, 0)})

	if got.Totals.Visitors != nil {
		t.Errorf("totals visitors = %s, want null: the day carries no visitors by hour", num(got.Totals.Visitors))
	}
	if line, want := sitesLine(got.Sites), site+" null/2/2"; line != want {
		t.Errorf("sites = %s, want %s -- its visits and page views from the hours, and no visitors", line, want)
	}
}

// ---------------------------------------------------------------------------
// Who reaches what
// ---------------------------------------------------------------------------

// THE PROPERTY THE OWNER CARES ABOUT MOST. A client's member who names
// another client's deployable, or that client's organization, gets the answer
// a deployable with no traffic gets: no refusal to confirm it exists, and none
// of its figures -- not even the day its counting began.
func TestAClientMemberGetsNothingForAnotherClientsSiteEvenById(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	siteA := seedDeployable(t, eng, "user-a-"+sfx, "a-"+sfx)
	siteB := seedDeployable(t, eng, "user-b-"+sfx, "b-"+sfx)
	member := "user-member-a-" + sfx
	addMember(t, eng, siteA, member)
	day := utcDay(2037, 11, 10)
	seedDailyStats(t, db, siteA, day, measured(3, 3, 1, 6, 10))
	seedDailyStats(t, db, siteB, day, measured(40, 41, 7, 90, 120))
	seedTopStats(t, db, siteB, day, "page", "/b-only-page", 40, 41, 90)
	now := time.Date(2037, 11, 12, 9, 0, 0, 0, time.UTC)
	ctx := userCtx(member)

	for name, q := range map[string]StatisticsQuery{
		"B's deployable by id":                {SiteIds: []string{siteB}},
		"B's organization":                    {AccountIds: []string{accountOf(siteB)}},
		"B's deployable and B's organization": {SiteIds: []string{siteB}, AccountIds: []string{accountOf(siteB)}},
	} {
		q.Period, q.Now = statsperiod.Last7Days, now
		got, err := NewReader(db, eng).Statistics(ctx, q)
		if err != nil {
			t.Fatalf("%s: a member's read must answer, not refuse -- a refusal would confirm the id: %v", name, err)
		}
		wantNothing(t, name, got)
		_, raw := payloadOf(t, got)
		for _, leak := range []string{siteB, accountOf(siteB), "/b-only-page"} {
			if strings.Contains(raw, leak) {
				t.Errorf("%s: the answer carries %q", name, leak)
			}
		}
	}

	// Their own organization answers, so the empty answers above are the
	// narrowing and not a read that answers nobody.
	for name, q := range map[string]StatisticsQuery{
		"both deployables by id":  {SiteIds: []string{siteA, siteB}},
		"both organizations":      {AccountIds: []string{accountOf(siteA), accountOf(siteB)}},
		"every deployable at all": {},
	} {
		q.Period, q.Now = statsperiod.Last7Days, now
		got := readStatistics(t, ctx, eng, db, q)
		if line, want := sitesLine(got.Sites), siteA+" 3/3/6"; line != want {
			t.Errorf("%s: sites = %s, want only their own %s", name, line, want)
		}
		if num(got.Totals.Visitors) != "3" || num(got.Totals.Requests) != "10" {
			t.Errorf("%s: totals %s, want their own deployable's alone", name, totalsLine(got.Totals))
		}
		if len(got.Top["page"]) != 0 {
			t.Errorf("%s: top page = %s, want none of B's pages", name, topLine(got.Top["page"]))
		}
	}
}

// Staff reach every organization, so a read that names nothing is every
// deployable in the cluster, added together.
func TestStaffGetEverySiteWhenNoneAreNamed(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	siteA := seedDeployable(t, eng, "user-a-"+sfx, "a-"+sfx)
	siteB := seedDeployable(t, eng, "user-b-"+sfx, "b-"+sfx)
	day := utcDay(2037, 12, 2)
	seedDailyStats(t, db, siteA, day, measured(5, 6, 2, 9, 20))
	seedDailyStats(t, db, siteB, day, measured(3, 3, 1, 4, 8))
	seedTopStats(t, db, siteA, day, "source", "direct", 5, 6, 9)
	seedTopStats(t, db, siteB, day, "source", "direct", 3, 3, 4)
	seedTopStats(t, db, siteB, day, "country", "SE", 3, 3, 4)
	now := time.Date(2037, 12, 3, 9, 0, 0, 0, time.UTC)

	for name, ctx := range map[string]context.Context{
		"a cluster owner": operatorCtx("user-owner-" + sfx),
		"a developer":     developerCtx("user-developer-" + sfx),
	} {
		got := readStatistics(t, ctx, eng, db, StatisticsQuery{Period: statsperiod.Last7Days, Now: now})
		if line, want := sitesLine(got.Sites), siteA+" 5/6/9; "+siteB+" 3/3/4"; line != want {
			t.Errorf("%s: sites = %s, want both organizations' deployables, most visited first: %s", name, line, want)
		}
		if line, want := totalsLine(got.Totals), "visitors=8 visits=9 bounces=3 pageViews=13 requests=28 errors=0 notFound=0 bytes=2800 botRequests=0"; line != want {
			t.Errorf("%s: totals:\n  got  %s\n  want %s", name, line, want)
		}
		if line := topLine(got.Top["source"]); line != "direct 8/9/13" {
			t.Errorf("%s: top source = %s, want the two deployables' direct arrivals together", name, line)
		}
		if line := topLine(got.Top["country"]); line != "SE 3/3/4" {
			t.Errorf("%s: top country = %s, want SE 3/3/4", name, line)
		}
	}

	// A client's member, asking the same way, reaches their own.
	member := "user-member-b-" + sfx
	addMember(t, eng, siteB, member)
	got := readStatistics(t, userCtx(member), eng, db, StatisticsQuery{Period: statsperiod.Last7Days, Now: now})
	if line, want := sitesLine(got.Sites), siteB+" 3/3/4"; line != want {
		t.Errorf("a member: sites = %s, want only %s", line, want)
	}
}

// Named organizations keep an every-deployable read to those clients -- and
// only ever narrow what the caller reaches, never widen it.
func TestAccountIdsKeepTheReadToThoseClients(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	siteA := seedDeployable(t, eng, "user-a-"+sfx, "a-"+sfx)
	siteB := seedDeployable(t, eng, "user-b-"+sfx, "b-"+sfx)
	siteC := seedDeployable(t, eng, "user-c-"+sfx, "c-"+sfx)
	day := utcDay(2038, 1, 10)
	seedDailyStats(t, db, siteA, day, measured(1, 1, 1, 1, 1))
	seedDailyStats(t, db, siteB, day, measured(2, 2, 2, 2, 2))
	seedDailyStats(t, db, siteC, day, measured(4, 4, 4, 4, 4))
	now := time.Date(2038, 1, 12, 9, 0, 0, 0, time.UTC)
	staff := operatorCtx("user-owner-" + sfx)

	for _, c := range []struct {
		name     string
		accounts []string
		want     string
	}{
		{"one client", []string{accountOf(siteB)}, siteB + " 2/2/2"},
		{"two clients", []string{accountOf(siteA), accountOf(siteC)}, siteC + " 4/4/4; " + siteA + " 1/1/1"},
		{"an organization named canonically", []string{"v1:accounts:account:" + accountOf(siteA)}, siteA + " 1/1/1"},
		{"an organization with no deployables", []string{"account-nobody-" + sfx}, ""},
	} {
		got := readStatistics(t, staff, eng, db, StatisticsQuery{AccountIds: c.accounts, Period: statsperiod.Last7Days, Now: now})
		if line := sitesLine(got.Sites); line != c.want {
			t.Errorf("%s: sites = %q, want %q", c.name, line, c.want)
		}
	}

	member := "user-member-a-" + sfx
	addMember(t, eng, siteA, member)
	got := readStatistics(t, userCtx(member), eng, db, StatisticsQuery{AccountIds: []string{accountOf(siteA), accountOf(siteB)}, Period: statsperiod.Last7Days, Now: now})
	if line, want := sitesLine(got.Sites), siteA+" 1/1/1"; line != want {
		t.Errorf("a member naming two organizations: sites = %s, want their own %s", line, want)
	}
}

// D16: the cluster's own surfaces are never recorded and never in an answer --
// not in an every-deployable read, and not when named.
func TestSystemOwnedSitesAreNeverInTheAnswer(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	normal := seedDeployable(t, eng, "user-n-"+sfx, "n-"+sfx)
	system := seedSystemOwnedSite(t, eng, "sys-"+sfx)
	day := utcDay(2038, 2, 10)
	seedDailyStats(t, db, normal, day, measured(2, 2, 1, 3, 5))
	// As though the edge had recorded the console anyway.
	seedDailyStats(t, db, system, day, measured(70, 70, 70, 70, 70))
	seedTopStats(t, db, system, day, "page", "/console", 70, 70, 70)
	now := time.Date(2038, 2, 12, 9, 0, 0, 0, time.UTC)
	owner := operatorCtx("user-owner-" + sfx)

	// The cluster owner can read the system-owned row, so leaving it out is
	// this read's doing and not the row gate's. Asked BY ID: sitesAll is
	// sorted by hostname, and on a shared database holding more deployables
	// than its first page the row need not be on it.
	res, err := eng.Execute(owner, fmt.Sprintf("query siteById(siteId: %s)", langparser.QuoteString(system)))
	if err != nil {
		t.Fatalf("read the system-owned site: %v", err)
	}
	if rows := memql.MaterializeRows(res); len(rows) != 1 || rows[0]["systemOwned"] != true {
		t.Fatalf("precondition: the cluster owner does not read the system-owned site %s; got %v", system, rows)
	}

	for name, q := range map[string]StatisticsQuery{
		"every deployable":        {},
		"both named":              {SiteIds: []string{system, normal}},
		"an organization is none": {AccountIds: []string{accountOf(normal)}},
	} {
		q.Period, q.Now = statsperiod.Last7Days, now
		got := readStatistics(t, owner, eng, db, q)
		if line, want := sitesLine(got.Sites), normal+" 2/2/3"; line != want {
			t.Errorf("%s: sites = %s, want only %s", name, line, want)
		}
		if num(got.Totals.Visitors) != "2" {
			t.Errorf("%s: totals %s, want the system-owned site's figures left out", name, totalsLine(got.Totals))
		}
		if line := topLine(got.Top["page"]); strings.Contains(line, "/console") {
			t.Errorf("%s: top page = %s, want no page of the console", name, line)
		}
	}
	got := readStatistics(t, owner, eng, db, StatisticsQuery{SiteIds: []string{system}, Period: statsperiod.Last7Days, Now: now})
	wantNothing(t, "the system-owned site named alone", got)
}

// The every-deployable read walks sitesAll to its end. Read here a page of
// one at a time, so a caller who belongs to three organizations crosses two
// page boundaries whatever else the shared database holds.
func TestAnEveryDeployableReadWalksEveryPageOfTheList(t *testing.T) {
	eng, db := statisticsTest(t)
	defer func(size int) { sitesPageSize = size }(sitesPageSize)
	sitesPageSize = 1
	sfx := suffix(t)
	member := "user-member-" + sfx
	var want []string
	for i, v := range []int64{3, 2, 1} {
		site := seedDeployable(t, eng, fmt.Sprintf("user-%d-%s", i, sfx), fmt.Sprintf("p%d-%s", i, sfx))
		addMember(t, eng, site, member)
		seedDailyStats(t, db, site, utcDay(2038, 3, 3), measured(v, v, v, v, v))
		want = append(want, fmt.Sprintf("%s %d/%d/%d", site, v, v, v))
	}
	got := readStatistics(t, userCtx(member), eng, db, StatisticsQuery{Period: statsperiod.Last7Days, Now: time.Date(2038, 3, 4, 9, 0, 0, 0, time.UTC)})
	if line := sitesLine(got.Sites); line != strings.Join(want, "; ") {
		t.Errorf("sites = %s, want all three organizations' deployables: %s", line, strings.Join(want, "; "))
	}
}

// A BLANK OR MALFORMED ID NAMES NOTHING; IT NEVER WIDENS THE READ. A list the
// caller sent is a list they named, so one whose every id is blank or not a
// string gets what an unreadable id gets -- nothing -- and not what naming
// none gets, which is every deployable they may read. Asked of the handler,
// because the wire's raw argument is where "named" is decided, and asked by
// staff, who reach every deployable, so a widened read cannot hide.
func TestABlankOrMalformedIdNamesNothingAndNeverWidensTheRead(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	siteA := seedDeployable(t, eng, "user-a-"+sfx, "a-"+sfx)
	siteB := seedDeployable(t, eng, "user-b-"+sfx, "b-"+sfx)
	day := utcDay(2038, 7, 1)
	seedDailyStats(t, db, siteA, day, measured(2, 2, 1, 3, 4))
	seedDailyStats(t, db, siteB, day, measured(5, 5, 1, 6, 7))
	i := NewIntegration(eng, func() *bun.DB { return db }, nil, quietLogger())
	i.now = func() time.Time { return dayAt(day, 12, 0) }
	staff := operatorCtx("user-owner-" + sfx)

	// read answers the site ids and the visitors total of one call.
	read := func(name string, args map[string]any) ([]string, any) {
		t.Helper()
		args["period"] = "7d"
		nodes, err := i.handleStatistics(staff, args, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(nodes) != 1 {
			t.Fatalf("%s: %d nodes, want 1", name, len(nodes))
		}
		var p map[string]any
		if err := json.Unmarshal(nodes[0].Payload, &p); err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		var ids []string
		sites, _ := p["sites"].([]any)
		for _, s := range sites {
			site, _ := s.(map[string]any)
			id, _ := site["siteId"].(string)
			ids = append(ids, id)
		}
		totals, _ := p["totals"].(map[string]any)
		return ids, totals["visitors"]
	}

	for name, args := range map[string]map[string]any{
		"a blank site id":           {"siteIds": []any{""}},
		"a non-string site id":      {"siteIds": []any{42}},
		"blank, null and a number":  {"siteIds": []any{" ", nil, 3.5}},
		"one blank site id":         {"siteIds": "  "},
		"a blank organization":      {"accountIds": []any{""}},
		"a non-string organization": {"accountIds": []any{7}},
	} {
		if ids, visitors := read(name, args); len(ids) != 0 || visitors != nil {
			t.Errorf("%s: answered sites %v and %v visitors, want nothing: the caller named something, and it names no deployable", name, ids, visitors)
		}
	}

	// And what still widens, and what does not: naming none reads every
	// deployable, and a usable id beside a blank one is read.
	for name, c := range map[string]struct {
		args map[string]any
		want string
	}{
		"no siteIds at all":              {map[string]any{}, siteB + "," + siteA},
		"an empty list":                  {map[string]any{"siteIds": []any{}}, siteB + "," + siteA},
		"a usable id beside a blank one": {map[string]any{"siteIds": []any{"", siteA}}, siteA},
		"an organization beside a blank": {map[string]any{"accountIds": []any{"", accountOf(siteB)}}, siteB},
	} {
		if ids, _ := read(name, c.args); strings.Join(ids, ",") != c.want {
			t.Errorf("%s: sites %v, want %s", name, ids, c.want)
		}
	}
}

// A NAMED DEPLOYABLE PAST THE FIRST PAGE IS READ. Named ids resolve against
// every page of sitesAll -- the walk a read naming none makes -- and only the
// ids the walk did not return (an archived deployable's) fall back to the
// bounded per-id lookup, so a list longer than one page, and than the
// fallback's sixteen lookups, is answered whole rather than short. Pages of
// one, so eighteen organizations cross seventeen boundaries whatever else
// the shared database holds.
func TestANamedDeployablePastTheFirstPageIsRead(t *testing.T) {
	eng, db := statisticsTest(t)
	defer func(size int) { sitesPageSize = size }(sitesPageSize)
	sitesPageSize = 1
	sfx := suffix(t)
	member := "user-member-" + sfx
	n := maxIndividualLookups + 2
	var named []string
	for k := 0; k < n; k++ {
		site := seedDeployable(t, eng, fmt.Sprintf("user-%02d-%s", k, sfx), fmt.Sprintf("n%02d-%s", k, sfx))
		addMember(t, eng, site, member)
		seedDailyStats(t, db, site, utcDay(2038, 8, 3), measured(1, 1, 1, 1, 1))
		named = append(named, site)
	}

	got := readStatistics(t, userCtx(member), eng, db, StatisticsQuery{SiteIds: named, Period: statsperiod.Last7Days, Now: time.Date(2038, 8, 4, 9, 0, 0, 0, time.UTC)})

	if len(got.Sites) != n || num(got.Totals.Visitors) != strconv.Itoa(n) {
		t.Errorf("%d deployables and %s visitors, want all %d of them: a named deployable past the first page was left out", len(got.Sites), num(got.Totals.Visitors), n)
	}
}

// The builtin answers ONE node, which is what the OS reads its row from.
func TestTheStatisticsBuiltinAnswersOneNode(t *testing.T) {
	eng, db := statisticsTest(t)
	sfx := suffix(t)
	owner := "user-stats-" + sfx
	site := seedDeployable(t, eng, owner, sfx)
	now := time.Date(2038, 4, 2, 9, 0, 0, 0, time.UTC)
	seedDailyStats(t, db, site, utcDay(2038, 4, 1), measured(2, 2, 1, 3, 4))
	i := NewIntegration(eng, func() *bun.DB { return db }, nil, quietLogger())
	i.now = func() time.Time { return now }

	nodes, err := i.handleStatistics(userCtx(owner), map[string]any{"siteIds": []any{site}, "period": "7d"}, 0)
	if err != nil {
		t.Fatalf("handleStatistics: %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != "statistics" || nodes[0].Concept != statisticsConcept {
		t.Fatalf("got %+v, want one node %q of %s", nodes, "statistics", statisticsConcept)
	}
	var p map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &p); err != nil {
		t.Fatalf("decode the payload: %v", err)
	}
	totals, _ := p["totals"].(map[string]any)
	if p["period"] != "7d" || p["start"] != "2038-03-27T00:00:00Z" || totals["visitors"] != 2.0 {
		t.Errorf("payload period %v start %v visitors %v, want 7d from 2038-03-27 with 2 visitors", p["period"], p["start"], totals["visitors"])
	}

	if _, err := i.handleStatistics(userCtx(owner), map[string]any{"siteIds": []any{site}, "period": "1y"}, 0); err == nil ||
		!strings.Contains(err.Error(), "12m") {
		t.Errorf("a period nobody offers answered %v, want a refusal naming the periods", err)
	}
}

// THE READ RUNS INSIDE THE REQUEST POOL'S STATEMENT LIMIT, NEVER ABOVE IT.
//
// The pool's limit (MEMQL_DB_STATEMENT_TIMEOUT_MS, 8 s by default) sits below
// pgdriver's 10 s client read deadline, so a slow statement is cancelled by the
// SERVER and the caller is told why (component/database, database.go). A read
// that raised it -- this one used to SET LOCAL statement_timeout = '30s' --
// bought nothing, since the client still gives up at 10 s, and left a slow scan
// running unheard for up to 20 s more, holding its snapshot, on every refresh of
// every Statistics page. Asked under the shipped limit and under an operator's
// lower one: the read follows the pool's limit, whatever it is, and is never
// unbounded.
func TestTheStatisticsReadStaysInsideThePoolsStatementLimit(t *testing.T) {
	ctx := context.Background()
	for _, poolMs := range []int64{8000, 3000} {
		db := servingPool(t, poolMs)
		var pool int64
		if err := db.NewRaw(statementTimeoutSetting).Scan(ctx, &pool); err != nil {
			t.Fatalf("read the pool's statement_timeout: %v", err)
		}
		if pool != poolMs {
			t.Fatalf("the pool's sessions run under a statement_timeout of %d ms, want %d: the test would prove nothing", pool, poolMs)
		}
		var inside int64
		if err := statisticsTx(ctx, db, func(ctx context.Context, tx bun.Tx) error {
			return tx.NewRaw(statementTimeoutSetting).Scan(ctx, &inside)
		}); err != nil {
			t.Fatalf("the read's transaction: %v", err)
		}
		if inside <= 0 || inside > pool {
			t.Errorf("under a pool limit of %d ms the read runs its statements under %d ms, want the pool's limit or less, and some limit",
				pool, inside)
		}
	}
}

// statementTimeoutSetting answers the session's statement_timeout in
// milliseconds, as a SET LOCAL inside the transaction leaves it.
const statementTimeoutSetting = `SELECT setting::bigint FROM pg_settings WHERE name = 'statement_timeout'`

// servingPool opens the test database with the request pool's statement limit:
// every backend SETs statement_timeout as component/database's sessionConnParams
// does, under pgdriver's default 10 s read deadline.
func servingPool(t *testing.T, statementTimeoutMs int64) *bun.DB {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"statement_timeout": strconv.FormatInt(statementTimeoutMs, 10)}),
	)), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		dbtest.Unreachable(t, "the statistics read's statement limit", dbtest.DSN(), err)
		return nil
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
