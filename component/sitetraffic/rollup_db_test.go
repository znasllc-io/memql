package sitetraffic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// rollup_db_test.go -- the statistics rollup against the real tables.
//
// Nothing here is provable in process: the hours and days are SQL's, the
// distinct visitors and the visit windows are SQL's, and which days a run
// recomputes is a decision read off what the tables hold.
//
// THE TESTS OWN A BAND OF DATES. The rollup recomputes every site in the days
// it covers, and chooses those days from the request log and the finalization
// record inside its 400-day window, so a test sharing that window with anybody
// else's rows would be asserting on theirs. Every `now` below is in 2036, whose windows reach back no further
// than November 2034; no other test writes there, and the band is cleared
// before and after each test. The dates are in the future for the reason the
// reader tests' are: the hypertable's retention and compression jobs act on
// chunks older than the real clock, and must not act on a fixture mid-test.
//
// Postgres-gated: CI's db-tests lane runs this package with MEMQL_REQUIRE_DB=1.

var (
	rollupBandStart = time.Date(2034, 11, 1, 0, 0, 0, 0, time.UTC)
	rollupBandEnd   = time.Date(2037, 1, 1, 0, 0, 0, 0, time.UTC)
)

// rollupTestDB opens the shared test database with the band cleared.
func rollupTestDB(t *testing.T) *bun.DB {
	t.Helper()
	db := edgeTestDB(t)
	clearRollupBand(t, db)
	t.Cleanup(func() { clearRollupBand(t, db) })
	return db
}

func clearRollupBand(t *testing.T, db *bun.DB) {
	t.Helper()
	ctx := context.Background()
	dropDailyRefusal(t, db)
	from, to := rollupBandStart.Format(time.DateOnly), rollupBandEnd.Format(time.DateOnly)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"DELETE FROM edge_request WHERE served_at >= ? AND served_at < ?", []any{rollupBandStart, rollupBandEnd}},
		{"DELETE FROM edge_stats_hourly WHERE hour >= ? AND hour < ?", []any{rollupBandStart, rollupBandEnd}},
		{"DELETE FROM edge_stats_daily WHERE day >= ?::date AND day < ?::date", []any{from, to}},
		{"DELETE FROM edge_stats_daily_top WHERE day >= ?::date AND day < ?::date", []any{from, to}},
		{"DELETE FROM edge_stats_final WHERE day >= ?::date AND day < ?::date", []any{from, to}},
		// The rollup's state and what it publishes beside the three tables:
		// a day's progress is the whole day's, every site's, so one left
		// behind would have the next test's fold start past its requests.
		{"DELETE FROM edge_stats_progress WHERE day >= ?::date AND day < ?::date", []any{from, to}},
		{"DELETE FROM edge_stats_settled_hour WHERE day >= ?::date AND day < ?::date", []any{from, to}},
		{"DELETE FROM edge_stats_visitor WHERE day >= ?::date AND day < ?::date", []any{from, to}},
		{"DELETE FROM edge_stats_value_hour WHERE day >= ?::date AND day < ?::date", []any{from, to}},
		{"DELETE FROM edge_stats_window_top WHERE window_end >= ? AND window_end <= ?", []any{rollupBandStart, rollupBandEnd}},
		{"DELETE FROM edge_stats_monthly_top WHERE month >= ?::date AND month < ?::date", []any{from, to}},
		{"DELETE FROM edge_stats_month_day WHERE day >= ?::date AND day < ?::date", []any{from, to}},
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("clear the rollup tests' band (%s): %v", q.sql, err)
		}
	}
}

// finalDays reads the days in [from, to] the rollup has recorded final, keyed
// "YYYY-MM-DD" with their finalized_at. The record is per day, not per site.
func finalDays(t *testing.T, db *bun.DB, from, to time.Time) map[string]time.Time {
	t.Helper()
	var rows []struct {
		Day         string    `bun:"day"`
		FinalizedAt time.Time `bun:"finalized_at"`
	}
	if err := db.NewRaw(`SELECT day::text AS day, finalized_at FROM edge_stats_final WHERE day >= ?::date AND day <= ?::date`,
		from.Format(time.DateOnly), to.Format(time.DateOnly)).Scan(context.Background(), &rows); err != nil {
		t.Fatalf("read the finalization record: %v", err)
	}
	out := map[string]time.Time{}
	for _, r := range rows {
		out[r.Day] = r.FinalizedAt.UTC()
	}
	return out
}

// dayAt is an instant on a UTC day.
func dayAt(day time.Time, hour, minute int) time.Time {
	return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

// pageViewRow is a measured page view by one visitor, with the dimensions a
// desktop Firefox on Linux in the Netherlands would carry.
func pageViewRow(when time.Time, visitor, path string) Row {
	return Row{
		ServedAt: when, Status: 200, PathClass: PathClassDocument, Bytes: 2048, DurationNs: 3_000_000,
		Measured: true, PageView: true, Path: path, Visitor: visitor,
		Country: "NL", Device: "desktop", Browser: "Firefox", OS: "Linux",
	}
}

// requestRow is a measured request that is not a page view.
func requestRow(when time.Time, status int, bytes, durationNs int64) Row {
	return Row{ServedAt: when, Status: status, PathClass: PathClassAsset, Bytes: bytes, DurationNs: durationNs, Measured: true}
}

// seedRequests writes rows for one site through the sink's own insert.
func seedRequests(t *testing.T, db *bun.DB, site string, rows ...Row) {
	t.Helper()
	for i := range rows {
		rows[i].Id = newRowId()
		rows[i].SiteId = site
		rows[i].Node = "edge-rollup-test"
		if rows[i].PathClass == "" {
			rows[i].PathClass = PathClassDocument
		}
		if rows[i].Status == 0 {
			rows[i].Status = 200
		}
	}
	if err := NewSink(db, SinkOptions{Logger: quietLogger()}).insertBatch(context.Background(), rows); err != nil {
		t.Fatalf("seed %d request rows: %v", len(rows), err)
	}
}

func runRollup(t *testing.T, open MaintenanceSession, now time.Time) RollupReport {
	t.Helper()
	rep, err := Rollup(context.Background(), open, now)
	if err != nil {
		t.Fatalf("Rollup at %s: %v", now.Format(time.RFC3339), err)
	}
	return rep
}

// testSession opens the rollup's session on the test database as production
// opens it (database.OpenMaintenanceSessionDSN: no statement_timeout, no read
// deadline beyond the context), never on a test pool, so every test here runs
// the statements on the kind of connection a deployed run uses.
func testSession(ctx context.Context) (*sql.Conn, func(), error) {
	return database.OpenMaintenanceSessionDSN(ctx, dbtest.DSN())
}

// rollupRow is one hourly or daily row, its bucket rendered in UTC.
type rollupRow struct {
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

const rollupColumns = "visitors, visits, bounces, page_views, requests, errors, not_found, bytes, bot_requests, duration_hist"

// hourlyOf reads a site's hourly rows keyed "YYYY-MM-DDTHH" in UTC. The key
// is rendered by the database AT TIME ZONE 'UTC', so it reads the same under
// any session time zone.
func hourlyOf(t *testing.T, db *bun.DB, site string) map[string]rollupRow {
	t.Helper()
	var rows []rollupRow
	q := `SELECT to_char(hour AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24') AS bucket, ` + rollupColumns +
		` FROM edge_stats_hourly WHERE site_id = ? ORDER BY hour`
	if err := db.NewRaw(q, site).Scan(context.Background(), &rows); err != nil {
		t.Fatalf("read the hourly rows of %s: %v", site, err)
	}
	out := map[string]rollupRow{}
	for _, r := range rows {
		out[r.Bucket] = r
	}
	return out
}

// dailyOf reads a site's daily rows keyed "YYYY-MM-DD".
func dailyOf(t *testing.T, db *bun.DB, site string) map[string]rollupRow {
	t.Helper()
	var rows []rollupRow
	q := `SELECT day::text AS bucket, ` + rollupColumns + ` FROM edge_stats_daily WHERE site_id = ? ORDER BY day`
	if err := db.NewRaw(q, site).Scan(context.Background(), &rows); err != nil {
		t.Fatalf("read the daily rows of %s: %v", site, err)
	}
	out := map[string]rollupRow{}
	for _, r := range rows {
		out[r.Bucket] = r
	}
	return out
}

type rollupTopRow struct {
	Day       string `bun:"day"`
	Dimension string `bun:"dimension"`
	Value     string `bun:"value"`
	Visitors  int64  `bun:"visitors"`
	Visits    int64  `bun:"visits"`
	PageViews int64  `bun:"page_views"`
}

// topOf reads a site's top rows for one day, keyed "dimension|value".
func topOf(t *testing.T, db *bun.DB, site string, day time.Time) map[string]rollupTopRow {
	t.Helper()
	var rows []rollupTopRow
	q := `SELECT day::text AS day, dimension, value, visitors, visits, page_views
	        FROM edge_stats_daily_top WHERE site_id = ? AND day = ?::date`
	if err := db.NewRaw(q, site, day.Format(time.DateOnly)).Scan(context.Background(), &rows); err != nil {
		t.Fatalf("read the top rows of %s: %v", site, err)
	}
	out := map[string]rollupTopRow{}
	for _, r := range rows {
		out[r.Dimension+"|"+r.Value] = r
	}
	return out
}

// visitFigures is the visit half of a row, comparable in one step; nil is NULL.
type visitFigures struct{ Visitors, Visits, Bounces, PageViews *int64 }

func figuresOf(r rollupRow) visitFigures {
	return visitFigures{r.Visitors, r.Visits, r.Bounces, r.PageViews}
}

func (f visitFigures) String() string {
	show := func(p *int64) string {
		if p == nil {
			return "NULL"
		}
		return strconv.FormatInt(*p, 10)
	}
	return fmt.Sprintf("visitors=%s visits=%s bounces=%s pageViews=%s", show(f.Visitors), show(f.Visits), show(f.Bounces), show(f.PageViews))
}

func ptr64(v int64) *int64 { return &v }

// measuredFigures are the figures of a bucket that was measured.
func measuredFigures(visitors, visits, bounces, pageViews int64) visitFigures {
	return visitFigures{ptr64(visitors), ptr64(visits), ptr64(bounces), ptr64(pageViews)}
}

func wantFigures(t *testing.T, where string, rows map[string]rollupRow, key string, want visitFigures) {
	t.Helper()
	got, ok := rows[key]
	if !ok {
		t.Errorf("%s %s: no row, want %s", where, key, want)
		return
	}
	if figuresOf(got).String() != want.String() {
		t.Errorf("%s %s: %s, want %s", where, key, figuresOf(got), want)
	}
}

// histogramOf is the histogram the given durations should fold into.
func histogramOf(durations ...int64) []int64 {
	h := make([]int64, histogramBuckets)
	for _, d := range durations {
		h[histogramBucket(d)]++
	}
	return h
}

func sameHistogram(a, b []int64) bool {
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// The headline: page views, distinct visitors, visits and the health counters
// land in the hour and the day they were served, per site.
func TestRollupCountsVisitorsPageViewsAndHealthPerHourAndDay(t *testing.T) {
	db := rollupTestDB(t)
	site, other := "site-rollup-"+suffix(t), "site-rollup-other-"+suffix(t)
	day := time.Date(2036, 3, 10, 0, 0, 0, 0, time.UTC)

	bot := Row{ServedAt: dayAt(day, 10, 35), Status: 200, PathClass: PathClassDocument, Bytes: 4000, DurationNs: 10_000_000, Measured: true, Bot: true}
	seedRequests(t, db, site,
		pageViewRow(dayAt(day, 10, 0), "va", "/"),
		pageViewRow(dayAt(day, 10, 10), "va", "/pricing"),
		pageViewRow(dayAt(day, 10, 15), "vb", "/"),
		requestRow(dayAt(day, 10, 20), 200, 500, 1_000_000),
		requestRow(dayAt(day, 10, 25), 503, 100, 40_000_000),
		requestRow(dayAt(day, 10, 30), 404, 50, 700_000),
		bot,
		pageViewRow(dayAt(day, 11, 5), "vc", "/about"),
		// va again, seventy minutes after their last page: a second visit.
		pageViewRow(dayAt(day, 11, 20), "va", "/"),
	)
	seedRequests(t, db, other, pageViewRow(dayAt(day, 10, 0), "vz", "/"))

	runRollup(t, testSession, dayAt(day, 12, 30))

	hourly := hourlyOf(t, db, site)
	if len(hourly) != 2 {
		t.Fatalf("got hourly rows %v, want hours 10 and 11 only -- an hour with no request has no row", statsKeys(hourly))
	}
	h10, h11 := hourly["2036-03-10T10"], hourly["2036-03-10T11"]
	wantFigures(t, "hour", hourly, "2036-03-10T10", measuredFigures(2, 2, 1, 3))
	wantFigures(t, "hour", hourly, "2036-03-10T11", measuredFigures(2, 2, 2, 2))
	if h10.Requests != 7 || h10.Errors != 1 || h10.NotFound != 1 || h10.BotRequests != 1 || h10.Bytes != 3*2048+500+100+50+4000 {
		t.Errorf("hour 10 health = requests %d errors %d notFound %d bots %d bytes %d, want 7, 1, 1, 1, %d",
			h10.Requests, h10.Errors, h10.NotFound, h10.BotRequests, h10.Bytes, 3*2048+500+100+50+4000)
	}
	if h11.Requests != 2 || h11.Errors != 0 || h11.NotFound != 0 || h11.BotRequests != 0 || h11.Bytes != 2*2048 {
		t.Errorf("hour 11 health = requests %d errors %d notFound %d bots %d bytes %d, want 2, 0, 0, 0, %d",
			h11.Requests, h11.Errors, h11.NotFound, h11.BotRequests, h11.Bytes, 2*2048)
	}
	if want := histogramOf(3_000_000, 3_000_000, 3_000_000, 1_000_000, 40_000_000, 700_000, 10_000_000); !sameHistogram(h10.Hist, want) {
		t.Errorf("hour 10 histogram = %v, want %v", h10.Hist, want)
	}

	daily := dailyOf(t, db, site)
	// Three people, not four: va was counted in both hours, and a day's
	// visitors are distinct over the day, not the sum of its hours.
	wantFigures(t, "day", daily, "2036-03-10", measuredFigures(3, 4, 3, 5))
	d := daily["2036-03-10"]
	if d.Requests != 9 || d.Errors != 1 || d.NotFound != 1 || d.BotRequests != 1 || d.Bytes != h10.Bytes+h11.Bytes {
		t.Errorf("day health = requests %d errors %d notFound %d bots %d bytes %d, want 9, 1, 1, 1, %d",
			d.Requests, d.Errors, d.NotFound, d.BotRequests, d.Bytes, h10.Bytes+h11.Bytes)
	}
	if len(d.Hist) != histogramBuckets || !sameHistogram(d.Hist, mergeHistograms(h10.Hist, h11.Hist)) {
		t.Errorf("day histogram = %v, want the sum of its hours", d.Hist)
	}

	// The other site is rolled up on its own rows, and only its own.
	wantFigures(t, "other site's day", dailyOf(t, db, other), "2036-03-10", measuredFigures(1, 1, 1, 1))
}

// A recompute is the whole computation for its period, so a second run over
// the same rows writes the same rows.
func TestRerunningTheRollupChangesNothing(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	day := time.Date(2036, 3, 12, 0, 0, 0, 0, time.UTC)
	seedRequests(t, db, site,
		Row{ServedAt: dayAt(day, 8, 0), Status: 200, Measured: true, PageView: true, Path: "/", Source: "google", Visitor: "r1", Country: "DE", Device: "mobile", Browser: "Safari", OS: "iOS", DurationNs: 2_000_000},
		pageViewRow(dayAt(day, 8, 10), "r1", "/shop"),
		pageViewRow(dayAt(day, 9, 0), "r2", "/"),
		requestRow(dayAt(day, 9, 5), 500, 10, 90_000_000),
	)

	now := dayAt(day, 15, 0)
	runRollup(t, testSession, now)
	first := rollupSnapshot(t, db, site, day)
	if !strings.Contains(first, "2036-03-12T08") || !strings.Contains(first, "page|/shop") {
		t.Fatalf("the first run wrote nothing worth comparing:\n%s", first)
	}

	rep := runRollup(t, testSession, now)
	if rep.Backfilled {
		t.Errorf("a second run backfilled again: %+v", rep)
	}
	if again := rollupSnapshot(t, db, site, day); again != first {
		t.Errorf("a re-run changed the rows.\nfirst:\n%s\nre-run:\n%s", first, again)
	}
	runRollup(t, testSession, now.Add(5*time.Minute))
	if later := rollupSnapshot(t, db, site, day); later != first {
		t.Errorf("the next scheduled run, with no new requests, changed the rows.\nfirst:\n%s\nlater:\n%s", first, later)
	}
}

// rollupSnapshot renders every rollup row of one site and day, in order.
func rollupSnapshot(t *testing.T, db *bun.DB, site string, day time.Time) string {
	t.Helper()
	var lines []string
	for k, r := range hourlyOf(t, db, site) {
		lines = append(lines, fmt.Sprintf("hourly %s %s %d %d %d %d %d %v", k, figuresOf(r), r.Requests, r.Errors, r.NotFound, r.Bytes, r.BotRequests, r.Hist))
	}
	for k, r := range dailyOf(t, db, site) {
		lines = append(lines, fmt.Sprintf("daily %s %s %d %d %d %d %d %v", k, figuresOf(r), r.Requests, r.Errors, r.NotFound, r.Bytes, r.BotRequests, r.Hist))
	}
	for k, r := range topOf(t, db, site, day) {
		lines = append(lines, fmt.Sprintf("top %s %d %d %d", k, r.Visitors, r.Visits, r.PageViews))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// ABSENT IS NOT ZERO. Requests recorded before the edge classified them --
// every request before this shipped -- count for health and leave visitors,
// visits, bounces and page views NULL, which reads "not measured". A measured
// hour with no page view says 0, which reads "nobody".
func TestRequestsWithoutVisitFieldsRollUpToNullVisitors(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	before := time.Date(2036, 3, 13, 0, 0, 0, 0, time.UTC)
	day := before.AddDate(0, 0, 1)

	unmeasured := func(when time.Time, status int) Row {
		return Row{ServedAt: when, Status: status, PathClass: PathClassDocument, Bytes: 300, DurationNs: 5_000_000}
	}
	seedRequests(t, db, site,
		// A whole day before counting began.
		unmeasured(dayAt(before, 14, 0), 200),
		unmeasured(dayAt(before, 15, 0), 200),
		// Hour 9: still unmeasured.
		unmeasured(dayAt(day, 9, 0), 200),
		unmeasured(dayAt(day, 9, 10), 500),
		unmeasured(dayAt(day, 9, 20), 404),
		// Hour 10: measured, and nobody viewed a page.
		requestRow(dayAt(day, 10, 0), 200, 100, 1_000_000),
		requestRow(dayAt(day, 10, 5), 200, 100, 1_000_000),
		// Hour 11: one unmeasured request beside one measured page view.
		unmeasured(dayAt(day, 11, 0), 200),
		pageViewRow(dayAt(day, 11, 30), "n1", "/"),
	)

	runRollup(t, testSession, dayAt(day, 18, 0))

	hourly := hourlyOf(t, db, site)
	wantFigures(t, "unmeasured hour", hourly, "2036-03-14T09", visitFigures{})
	if h := hourly["2036-03-14T09"]; h.Requests != 3 || h.Errors != 1 || h.NotFound != 1 || h.Bytes != 900 {
		t.Errorf("an unmeasured hour still counts for health: requests %d errors %d notFound %d bytes %d, want 3, 1, 1, 900", h.Requests, h.Errors, h.NotFound, h.Bytes)
	}
	wantFigures(t, "measured hour with no page view", hourly, "2036-03-14T10", measuredFigures(0, 0, 0, 0))
	wantFigures(t, "mixed hour", hourly, "2036-03-14T11", measuredFigures(1, 1, 1, 1))

	daily := dailyOf(t, db, site)
	wantFigures(t, "unmeasured day", daily, "2036-03-13", visitFigures{})
	if d := daily["2036-03-13"]; d.Requests != 2 {
		t.Errorf("the unmeasured day counts %d requests, want 2", d.Requests)
	}
	wantFigures(t, "partly measured day", daily, "2036-03-14", measuredFigures(1, 1, 1, 1))
	if top := topOf(t, db, site, before); len(top) != 0 {
		t.Errorf("an unmeasured day has top rows %v, want none -- top lists come from page views only", top)
	}
}

// A day's top list keeps the hundred values with the most visitors, ties
// broken by page views and then by value, and folds the rest into (other).
func TestTopListsKeepAHundredAndFoldTheRest(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	day := time.Date(2036, 4, 2, 0, 0, 0, 0, time.UTC)

	var rows []Row
	// /zzz has three visitors, so it ranks first despite sorting last.
	for i := 0; i < 3; i++ {
		r := pageViewRow(dayAt(day, 9, i), fmt.Sprintf("v-z%d", i), "/zzz")
		r.Source = "newsletter"
		rows = append(rows, r)
	}
	// /p140 has one visitor and two page views, so it ranks above every
	// other one-visitor path.
	rows = append(rows, pageViewRow(dayAt(day, 10, 0), "v-140", "/p140"), pageViewRow(dayAt(day, 10, 5), "v-140", "/p140"))
	for i := 0; i < 149; i++ {
		if i == 140 {
			continue
		}
		rows = append(rows, pageViewRow(dayAt(day, 12, 0).Add(time.Duration(i)*time.Second), fmt.Sprintf("v-%03d", i), fmt.Sprintf("/p%03d", i)))
	}
	seedRequests(t, db, site, rows...)

	runRollup(t, testSession, dayAt(day, 20, 0))

	top := topOf(t, db, site, day)
	var pages []string
	for k := range top {
		if strings.HasPrefix(k, "page|") {
			pages = append(pages, k)
		}
	}
	if len(pages) != 101 {
		t.Fatalf("150 pages give %d page rows, want 100 plus (other)", len(pages))
	}
	for key, want := range map[string]rollupTopRow{
		"page|/zzz":    {Visitors: 3, Visits: 3, PageViews: 3},
		"page|/p140":   {Visitors: 1, Visits: 1, PageViews: 2},
		"page|/p000":   {Visitors: 1, Visits: 1, PageViews: 1},
		"page|/p097":   {Visitors: 1, Visits: 1, PageViews: 1},
		"page|(other)": {Visitors: 50, Visits: 50, PageViews: 50},
		// Every visitor's first page view was in the Netherlands.
		"country|NL":        {Visitors: 152, Visits: 152, PageViews: 153},
		"source|newsletter": {Visitors: 3, Visits: 3, PageViews: 3},
	} {
		got, ok := top[key]
		if !ok {
			t.Errorf("no %s row", key)
			continue
		}
		if got.Visitors != want.Visitors || got.Visits != want.Visits || got.PageViews != want.PageViews {
			t.Errorf("%s = %d visitors, %d visits, %d page views; want %d, %d, %d", key, got.Visitors, got.Visits, got.PageViews, want.Visitors, want.Visits, want.PageViews)
		}
	}
	if _, kept := top["page|/p098"]; kept {
		t.Error("/p098 is kept, want it folded: it ties /p000../p097 on visitors and page views and sorts after them")
	}
	if _, ok := top["source|"]; ok {
		t.Error("an empty source has a row, want it left out: it is a click inside the site, not an arrival")
	}
}

// THE FIRST RUN BACKFILLS. No day has a finalization record yet, so every day
// of the request log's window -- its configured retention, sixty days here --
// is recomputed: each gets its daily row, and every hour of the last thirty
// days its hourly row; a day older than thirty days gets no hours, because the
// hourly table keeps thirty days. A day in the window with no request is
// recomputed to nothing and recorded final all the same. A row older than the
// window is not read at all: the log's retention says it is not there. Every
// day before today is past 02:00 of the day after it, so each is recorded
// final, and the next run recomputes today alone.
func TestTheFirstRunBackfillsEveryDayStillLogged(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	t.Setenv("MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS", "60")
	now := time.Date(2036, 6, 15, 12, 0, 0, 0, time.UTC)
	seedRequests(t, db, site,
		pageViewRow(time.Date(2036, 4, 1, 10, 0, 0, 0, time.UTC), "b0", "/"),
		pageViewRow(time.Date(2036, 5, 1, 10, 0, 0, 0, time.UTC), "b1", "/"),
		pageViewRow(time.Date(2036, 6, 1, 8, 0, 0, 0, time.UTC), "b2", "/"),
		Row{ServedAt: time.Date(2036, 6, 1, 8, 30, 0, 0, time.UTC), Status: 200},
		pageViewRow(time.Date(2036, 6, 14, 22, 0, 0, 0, time.UTC), "b3", "/"),
		pageViewRow(time.Date(2036, 6, 15, 9, 0, 0, 0, time.UTC), "b4", "/"),
	)

	rep := runRollup(t, testSession, now)
	// The window is the sixty days 2036-04-16 through 2036-06-14, and today.
	// The hours run from the first one the hourly table keeps, 2036-05-16
	// 12:00, through the current one: thirty days of them and the hour under
	// way.
	if !rep.Backfilled || rep.Days != 61 || rep.Hours != 30*24+1 {
		t.Errorf("the first run reported %+v, want a backfill of 61 days and %d hours", rep, 30*24+1)
	}
	daily := dailyOf(t, db, site)
	for _, d := range []string{"2036-05-01", "2036-06-01", "2036-06-14", "2036-06-15"} {
		wantFigures(t, "backfilled day", daily, d, measuredFigures(1, 1, 1, 1))
	}
	if _, ok := daily["2036-04-01"]; ok {
		t.Error("a day older than the request log's retention was recomputed")
	}
	if r := daily["2036-06-01"]; r.Requests != 2 {
		t.Errorf("2036-06-01 counts %d requests, want 2", r.Requests)
	}
	hourly := hourlyOf(t, db, site)
	for _, h := range []string{"2036-06-01T08", "2036-06-14T22", "2036-06-15T09"} {
		if _, ok := hourly[h]; !ok {
			t.Errorf("no hourly row for %s, which is inside the last thirty days", h)
		}
	}
	if _, ok := hourly["2036-05-01T10"]; ok {
		t.Error("an hour 45 days old was written, past the hourly table's thirty days")
	}
	final := finalDays(t, db, time.Date(2036, 3, 1, 0, 0, 0, 0, time.UTC), now)
	if len(final) != 60 {
		t.Errorf("%d days were recorded final, want the 60 of the window, empty ones included", len(final))
	}
	if _, ok := final["2036-06-15"]; ok {
		t.Error("today was recorded final")
	}
	if _, ok := final["2036-04-01"]; ok {
		t.Error("a day older than the window was recorded final")
	}

	again := runRollup(t, testSession, now)
	if again.Backfilled || again.Days != 1 || again.Hours != 13 {
		t.Errorf("the next run reported %+v, want today alone: 1 day, its 13 hours so far, no backfill", again)
	}
}

// A run that is cut off part way is finished by the next one. A run carries on
// past a day it could not write; only a cancelled context stops it -- a node
// shutting down mid-run, say -- and the days it had not reached are left
// without a finalization record, which the next run recomputes.
func TestAnInterruptedBackfillResumesOnTheNextRun(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	now := time.Date(2036, 6, 20, 12, 0, 0, 0, time.UTC)
	for _, d := range []int{5, 10, 15, 20} {
		seedRequests(t, db, site, pageViewRow(time.Date(2036, 6, d, 9, 0, 0, 0, time.UTC), "i"+strconv.Itoa(d), "/"))
	}
	runRollup(t, testSession, now)

	// What a run cancelled after 2036-06-12 leaves behind: the days before
	// it neither written nor recorded final.
	for _, q := range []string{
		"DELETE FROM edge_stats_daily WHERE site_id = ? AND day < '2036-06-12'",
		"DELETE FROM edge_stats_hourly WHERE site_id = ? AND hour < '2036-06-12T00:00:00Z'",
	} {
		if _, err := db.ExecContext(context.Background(), q, site); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(context.Background(), "DELETE FROM edge_stats_final WHERE day >= '2036-06-01' AND day < '2036-06-12'"); err != nil {
		t.Fatal(err)
	}

	rep := runRollup(t, testSession, now)
	if !rep.Backfilled {
		t.Errorf("the run after an interrupted backfill reported %+v, want it to finish the backfill", rep)
	}
	daily := dailyOf(t, db, site)
	for _, d := range []string{"2036-06-05", "2036-06-10", "2036-06-15", "2036-06-20"} {
		wantFigures(t, "day", daily, d, measuredFigures(1, 1, 1, 1))
	}
	if _, ok := hourlyOf(t, db, site)["2036-06-05T09"]; !ok {
		t.Error("the resumed backfill wrote no hour for 2036-06-05")
	}
}

// refuseDailyRowsFor makes every insert of a daily row for the given days fail,
// until the returned func or the test's end removes it. A trigger on the real
// table rather than a seam in the code, so the failure takes the path a
// statement timeout would: the day's transaction rolls back and Rollup reports
// the day. It names dates in the tests' band, so nothing else is refused, and
// clearRollupBand removes it too, so a test binary killed mid-test cannot
// leave it refusing those dates for the next run.
func refuseDailyRowsFor(t *testing.T, db *bun.DB, days ...time.Time) func() {
	t.Helper()
	return refuseDailyRowsWith(t, db, "raise_exception", days...)
}

// refuseDailyRowsWith is refuseDailyRowsFor raising the given condition: the
// default raise_exception (SQLSTATE P0001) leaves the connection open, while
// query_canceled (57014), the code a statement timeout raises, is one pgdriver
// closes the connection under.
func refuseDailyRowsWith(t *testing.T, db *bun.DB, condition string, days ...time.Time) func() {
	t.Helper()
	ctx := context.Background()
	dates := make([]string, len(days))
	for i, d := range days {
		dates[i] = "'" + d.Format(time.DateOnly) + "'"
	}
	remove := func() { dropDailyRefusal(t, db) }
	remove()
	for _, stmt := range []string{
		"CREATE FUNCTION " + dailyRefusal + "() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN " +
			"IF NEW.day::text = ANY (TG_ARGV) THEN RAISE EXCEPTION 'injected failure for %', NEW.day USING ERRCODE = '" + condition + "'; END IF; " +
			"RETURN NEW; END $$",
		"CREATE TRIGGER " + dailyRefusal + " BEFORE INSERT ON edge_stats_daily FOR EACH ROW EXECUTE FUNCTION " +
			dailyRefusal + "(" + strings.Join(dates, ", ") + ")",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("install the refusal for %s: %v", strings.Join(dates, ", "), err)
		}
	}
	t.Cleanup(remove)
	return remove
}

// dailyRefusal names both the trigger and its function.
const dailyRefusal = "rollup_test_refuse_day"

func dropDailyRefusal(t *testing.T, db *bun.DB) {
	t.Helper()
	for _, stmt := range []string{
		"DROP TRIGGER IF EXISTS " + dailyRefusal + " ON edge_stats_daily",
		"DROP FUNCTION IF EXISTS " + dailyRefusal + "()",
	} {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("remove the daily-row refusal: %v", err)
		}
	}
}

// A day that fails is only that day: it is neither written nor recorded final,
// so the next run recomputes it, and the run carries on with every other open
// day -- the newer ones, and the older ones too.
func TestAFailedDayDoesNotHoldBackOtherDays(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	now := time.Date(2036, 6, 20, 12, 0, 0, 0, time.UTC)
	for _, d := range []int{5, 10, 15, 20} {
		seedRequests(t, db, site, pageViewRow(time.Date(2036, 6, d, 9, 0, 0, 0, time.UTC), "f"+strconv.Itoa(d), "/"))
	}
	restore := refuseDailyRowsFor(t, db, time.Date(2036, 6, 10, 0, 0, 0, 0, time.UTC))

	rep, err := Rollup(context.Background(), testSession, now)
	if err == nil || !strings.Contains(err.Error(), "2036-06-10") {
		t.Fatalf("the run reported %v, want the failure of 2036-06-10", err)
	}
	// Today and the thirty open days of the window, 2036-05-21..06-19, less
	// the one that failed.
	if !rep.Backfilled || rep.Days != 30 {
		t.Errorf("the failing run reported %+v, want every day written but the one that failed: 30", rep)
	}
	daily := dailyOf(t, db, site)
	for d, want := range map[string]bool{"2036-06-20": true, "2036-06-15": true, "2036-06-10": false, "2036-06-05": true} {
		if _, got := daily[d]; got != want {
			t.Errorf("after the failing run, a daily row for %s exists = %v, want %v", d, got, want)
		}
	}
	final := finalDays(t, db, time.Date(2036, 6, 1, 0, 0, 0, 0, time.UTC), now)
	for d, want := range map[string]bool{"2036-06-19": true, "2036-06-11": true, "2036-06-10": false, "2036-06-09": true, "2036-06-05": true} {
		if _, got := final[d]; got != want {
			t.Errorf("after the failing run, %s recorded final = %v, want %v", d, got, want)
		}
	}

	restore()
	rep = runRollup(t, testSession, now)
	if rep.Days != 2 || !rep.Backfilled {
		t.Errorf("the next run reported %+v, want today and the day that failed", rep)
	}
	wantFigures(t, "retried day", dailyOf(t, db, site), "2036-06-10", measuredFigures(1, 1, 1, 1))
	if _, ok := finalDays(t, db, now.AddDate(0, 0, -10), now)["2036-06-10"]; !ok {
		t.Error("the retried day was not recorded final")
	}
}

// A DAY THE SERVER CANCELS COSTS THAT DAY ALONE, ON ONE SESSION. A statement
// the server ends at its timeout fails with SQLSTATE 57014, and pgdriver closes
// the connection under that code; a run's days share one session, so had the
// cancelled day kept it, every day after it would fail as well, on a closed
// connection (rollupSession.inTx). The trigger raises the same SQLSTATE, since
// a real timeout would take rollupStatementTimeout to reach, and pgdriver reads
// nothing but the code.
func TestADayTheServerCancelsCostsThatDayAlone(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	now := time.Date(2036, 7, 20, 12, 0, 0, 0, time.UTC)
	for _, d := range []int{5, 10, 15} {
		seedRequests(t, db, site, pageViewRow(time.Date(2036, 7, d, 9, 0, 0, 0, time.UTC), "x"+strconv.Itoa(d), "/"))
	}
	refuseDailyRowsWith(t, db, "query_canceled", time.Date(2036, 7, 15, 0, 0, 0, 0, time.UTC))

	_, err := Rollup(context.Background(), testSession, now)
	var failed []string
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, f := range joined.Unwrap() {
			failed = append(failed, f.Error())
		}
	} else if err != nil {
		failed = append(failed, err.Error())
	}
	if len(failed) != 1 || !strings.Contains(failed[0], "2036-07-15") || !strings.Contains(failed[0], "57014") {
		t.Fatalf("the run failed %d times: %v; want the cancelled day alone, by its SQLSTATE", len(failed), failed)
	}
	daily := dailyOf(t, db, site)
	for d, want := range map[string]bool{"2036-07-15": false, "2036-07-10": true, "2036-07-05": true} {
		if _, got := daily[d]; got != want {
			t.Errorf("a daily row for %s exists = %v, want %v", d, got, want)
		}
	}
}

// Every day a run could not write is logged once, by name, at Warn, and the
// run still writes the rest. The run's error is returned too, so the
// automation's run reads as failed.
func TestEachFailedDayIsLoggedOnceByName(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	now := time.Date(2036, 6, 28, 12, 0, 0, 0, time.UTC)
	for _, d := range []int{22, 24, 26} {
		seedRequests(t, db, site, pageViewRow(time.Date(2036, 6, d, 9, 0, 0, 0, time.UTC), "w"+strconv.Itoa(d), "/"))
	}
	refuseDailyRowsFor(t, db, time.Date(2036, 6, 22, 0, 0, 0, 0, time.UTC), time.Date(2036, 6, 26, 0, 0, 0, 0, time.UTC))

	logs := &recordedLogs{}
	i := NewIntegration(nil, func() *bun.DB { return db }, testSession, slog.New(logs))
	i.now = func() time.Time { return now }
	ctx := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor(auth.AutomationOriginEngine, "siteStatisticsRollupSweep"))
	if _, err := i.handleRollup(ctx, nil, 0); err == nil {
		t.Fatal("a run with two failed days answered no error")
	}
	var named []string
	for _, r := range logs.warnings() {
		day, ok := r["day"]
		if !ok {
			t.Errorf("a Warn names no day: %v", r)
			continue
		}
		named = append(named, day)
	}
	sort.Strings(named)
	if strings.Join(named, ",") != "2036-06-22,2036-06-26" {
		t.Errorf("the Warns name the days %v, want 2036-06-22 and 2036-06-26, once each", named)
	}
	if _, ok := dailyOf(t, db, site)["2036-06-24"]; !ok {
		t.Error("the day between the two failed days was not written")
	}
}

// recordedLogs is a slog.Handler that keeps what it is given, so a test can ask
// what was logged and at which level.
type recordedLogs struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordedLogs) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordedLogs) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recordedLogs) WithGroup(string) slog.Handler            { return h }
func (h *recordedLogs) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

// warnings answers the attributes of each Warn record, as strings.
func (h *recordedLogs) warnings() []map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]string
	for _, r := range h.records {
		if r.Level != slog.LevelWarn {
			continue
		}
		attrs := map[string]string{"msg": r.Message}
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.String()
			return true
		})
		out = append(out, attrs)
	}
	return out
}

// A yesterday that fails is only that day: the run writes today and the days
// older than it, and yesterday, which has no finalization record, is
// recomputed -- and recorded final -- by the first run after 02:00 that
// succeeds.
func TestAYesterdayThatFailsDuringABackfillIsWrittenAfterTwo(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	yesterday := time.Date(2036, 6, 24, 0, 0, 0, 0, time.UTC)
	today := yesterday.AddDate(0, 0, 1)
	seedRequests(t, db, site,
		pageViewRow(time.Date(2036, 6, 20, 9, 0, 0, 0, time.UTC), "y-old", "/"),
		pageViewRow(dayAt(yesterday, 22, 0), "y-yesterday", "/"),
		pageViewRow(dayAt(today, 0, 30), "y-today", "/"),
	)
	restore := refuseDailyRowsFor(t, db, yesterday)

	if _, err := Rollup(context.Background(), testSession, dayAt(today, 1, 0)); err == nil || !strings.Contains(err.Error(), "2036-06-24") {
		t.Fatalf("the first run reported %v, want yesterday's failure", err)
	}
	daily := dailyOf(t, db, site)
	if _, ok := daily["2036-06-25"]; !ok {
		t.Error("today was not written by the run that failed on yesterday")
	}
	wantFigures(t, "a day older than the failed yesterday", daily, "2036-06-20", measuredFigures(1, 1, 1, 1))

	restore()
	rep := runRollup(t, testSession, dayAt(today, 3, 0))
	if rep.Days != 2 || rep.Backfilled {
		t.Errorf("the run after 02:00 reported %+v, want yesterday and today alone: the older days were written already", rep)
	}
	wantFigures(t, "yesterday", dailyOf(t, db, site), "2036-06-24", measuredFigures(1, 1, 1, 1))
	if _, ok := finalDays(t, db, yesterday, yesterday)["2036-06-24"]; !ok {
		t.Error("yesterday, recomputed after 02:00, was not recorded final")
	}
}

// A FINAL DAY STAYS FINAL. Once the request log's retention has dropped a
// day's requests, the day can come back into a run's window -- the retention
// raised again -- and a stray request for it can turn up -- an edge replica
// whose clock lags, rows restored to the log. Neither may reopen the day and
// rewrite its figures from what little is there. The day's record lives as
// long as its daily row, so the run leaves it alone.
func TestAStrayRawRowCannotReopenAFinalDay(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	day := time.Date(2036, 8, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2036, 8, 10, 12, 0, 0, 0, time.UTC)
	seedRequests(t, db, site,
		pageViewRow(dayAt(day, 9, 0), "s1", "/"),
		pageViewRow(dayAt(day, 10, 0), "s2", "/"),
		pageViewRow(dayAt(day, 11, 0), "s3", "/"),
		pageViewRow(time.Date(2036, 8, 5, 9, 0, 0, 0, time.UTC), "n1", "/"),
	)
	runRollup(t, testSession, now)
	wantFigures(t, "the day", dailyOf(t, db, site), "2036-08-01", measuredFigures(3, 3, 3, 3))

	// The request log is cut to five days, which drops the day's requests, and
	// a run passes with the day outside its window.
	t.Setenv("MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS", "5")
	if _, err := db.ExecContext(context.Background(), "DELETE FROM edge_request WHERE served_at >= ? AND served_at < ?", day, day.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	runRollup(t, testSession, now.Add(5*time.Minute))

	// Then the retention goes back to thirty days, bringing the day back into
	// the window, and a replica with a lagging clock writes a request on it.
	t.Setenv("MEMQL_EDGE_REQUEST_LOG_RETENTION_DAYS", "30")
	seedRequests(t, db, site, pageViewRow(dayAt(day, 12, 0), "stray", "/stray"))
	rep := runRollup(t, testSession, now.Add(10*time.Minute))
	if rep.Days != 1 {
		t.Errorf("the run recomputed %d days, want today alone", rep.Days)
	}
	wantFigures(t, "the final day after a stray request", dailyOf(t, db, site), "2036-08-01", measuredFigures(3, 3, 3, 3))
	if got, ok := finalDays(t, db, day, day)["2036-08-01"]; !ok || !got.Equal(now) {
		t.Errorf("the day's record = %v (%v), want it kept as the first run wrote it", got, ok)
	}
}

// THE RULING THIS MECHANISM EXISTS FOR. A request served at 23:30 is written,
// and then no run happens from 22:55 until 03:00 the next morning -- an
// outage across 02:00. The run at 03:00 finds yesterday without a
// finalization record, recomputes it with the 23:30 request in it, and only
// then records it final.
func TestAnOutageAcrossTwoAMHealsOnTheNextRun(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	yesterday := time.Date(2036, 7, 19, 0, 0, 0, 0, time.UTC)
	today := yesterday.AddDate(0, 0, 1)
	seedRequests(t, db, site, pageViewRow(dayAt(yesterday, 20, 0), "o1", "/"), pageViewRow(dayAt(yesterday, 22, 0), "o2", "/"))
	runRollup(t, testSession, dayAt(yesterday, 22, 55))

	seedRequests(t, db, site, pageViewRow(dayAt(yesterday, 23, 30), "o3", "/late"))
	// No run between 22:55 and 03:00.
	rep := runRollup(t, testSession, dayAt(today, 3, 0))
	if rep.Days != 2 {
		t.Errorf("the run at 03:00 recomputed %d days, want today and yesterday", rep.Days)
	}
	wantFigures(t, "yesterday after the outage", dailyOf(t, db, site), "2036-07-19", measuredFigures(3, 3, 3, 3))
	wantFigures(t, "yesterday's 23:00 after the outage", hourlyOf(t, db, site), "2036-07-19T23", measuredFigures(1, 1, 1, 1))
	if got, ok := finalDays(t, db, yesterday, yesterday)["2036-07-19"]; !ok || !got.Equal(dayAt(today, 3, 0)) {
		t.Errorf("yesterday's finalization record = %v (%v), want the 03:00 run's", got, ok)
	}
}

// A day recorded final is left as it is: a request for it written later --
// past the last recompute -- changes nothing, and the record keeps the run that
// made it.
func TestAFinalDayIsNotRecomputed(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	day := time.Date(2036, 7, 22, 0, 0, 0, 0, time.UTC)
	next := day.AddDate(0, 0, 1)
	seedRequests(t, db, site, pageViewRow(dayAt(day, 10, 0), "l1", "/"))
	runRollup(t, testSession, dayAt(next, 3, 0))
	if _, ok := finalDays(t, db, day, day)["2036-07-22"]; !ok {
		t.Fatal("the day was not recorded final by a run after 02:00 the next day, so this test would prove nothing")
	}

	seedRequests(t, db, site, pageViewRow(dayAt(day, 11, 0), "l2", "/late"))
	rep := runRollup(t, testSession, dayAt(next, 4, 0))
	if rep.Days != 1 {
		t.Errorf("the run recomputed %d days, want today alone", rep.Days)
	}
	wantFigures(t, "final day", dailyOf(t, db, site), "2036-07-22", measuredFigures(1, 1, 1, 1))
	if _, ok := hourlyOf(t, db, site)["2036-07-22T11"]; ok {
		t.Error("the late request's hour was written for a final day")
	}
	if got := finalDays(t, db, day, day)["2036-07-22"]; !got.Equal(dayAt(next, 3, 0)) {
		t.Errorf("the finalization record now says %v, want the first run's 03:00", got)
	}
}

// Today is never final, however late the run: it can still be served. A day
// becomes final at 02:00 the day after it, and not a moment before.
func TestTodayIsNeverFinal(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	day := time.Date(2036, 7, 25, 0, 0, 0, 0, time.UTC)
	next := day.AddDate(0, 0, 1)
	seedRequests(t, db, site, pageViewRow(dayAt(day, 10, 0), "t1", "/"))

	for _, when := range []time.Time{dayAt(day, 10, 5), dayAt(day, 23, 59).Add(59 * time.Second), dayAt(next, 1, 59).Add(59 * time.Second)} {
		runRollup(t, testSession, when)
		if final := finalDays(t, db, day, next); len(final) != 0 {
			t.Errorf("after the run at %s the record holds %v, want nothing final yet", when.Format(time.RFC3339), final)
		}
	}
	runRollup(t, testSession, dayAt(next, 2, 0))
	final := finalDays(t, db, day, next)
	if _, ok := final["2036-07-25"]; !ok {
		t.Error("the day was not recorded final by the run at 02:00 the next day")
	}
	if _, ok := final["2036-07-26"]; ok {
		t.Error("today was recorded final")
	}
}

// Yesterday is recomputed until 02:00 UTC, so a request served at 23:59 and
// written a little later still lands in yesterday's figures. The first run at
// or after 02:00 recomputes it one last time and records it final; after that,
// yesterday is left as it is.
func TestYesterdayIsRecomputedUntilTwoAfterMidnight(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	yesterday := time.Date(2036, 7, 9, 0, 0, 0, 0, time.UTC)
	today := yesterday.AddDate(0, 0, 1)
	pageViewsOn := func(when time.Time) int64 {
		got := dailyOf(t, db, site)["2036-07-09"]
		if got.PageViews == nil {
			t.Fatalf("at %s yesterday's page views are not measured", when.Format("15:04"))
		}
		return *got.PageViews
	}

	seedRequests(t, db, site, pageViewRow(dayAt(yesterday, 20, 0), "y1", "/"))
	runRollup(t, testSession, dayAt(yesterday, 21, 0))

	seedRequests(t, db, site, pageViewRow(dayAt(yesterday, 23, 59), "y2", "/"))
	at0030 := dayAt(today, 0, 30)
	if rep := runRollup(t, testSession, at0030); rep.Days != 2 {
		t.Errorf("at 00:30 the run recomputed %d days, want yesterday and today", rep.Days)
	}
	if got := pageViewsOn(at0030); got != 2 {
		t.Fatalf("at 00:30 yesterday reads %d page views, want the late page view counted", got)
	}
	if _, ok := finalDays(t, db, yesterday, yesterday)["2036-07-09"]; ok {
		t.Error("yesterday was recorded final before 02:00")
	}

	// Written before 02:00: the 02:00 run is yesterday's last recompute.
	seedRequests(t, db, site, pageViewRow(dayAt(yesterday, 23, 59).Add(30*time.Second), "y3", "/"))
	at0200 := dayAt(today, 2, 0)
	if rep := runRollup(t, testSession, at0200); rep.Days != 2 {
		t.Errorf("at 02:00 the run recomputed %d days, want yesterday one last time, and today", rep.Days)
	}
	if got := pageViewsOn(at0200); got != 3 {
		t.Errorf("at 02:00 yesterday reads %d page views, want the 23:59:30 view counted", got)
	}

	// Written after it: a late row for 23:59 does not count at 02:30.
	seedRequests(t, db, site, pageViewRow(dayAt(yesterday, 23, 59).Add(45*time.Second), "y4", "/"))
	at0230 := dayAt(today, 2, 30)
	if rep := runRollup(t, testSession, at0230); rep.Days != 1 {
		t.Errorf("at 02:30 the run recomputed %d days, want today alone", rep.Days)
	}
	if got := pageViewsOn(at0230); got != 3 {
		t.Errorf("at 02:30 yesterday reads %d page views, want it left as the 02:00 run wrote it", got)
	}
	if got := hourlyOf(t, db, site)["2036-07-09T23"]; got.PageViews == nil || *got.PageViews != 2 {
		t.Errorf("at 02:30 yesterday's 23:00 reads %s, want it left as the 02:00 run wrote it", figuresOf(got))
	}
}

// Retention: hourly rows go after thirty days, daily and top rows after 400,
// and a day's finalization record with its daily row -- not when the request
// log loses the day, which is what keeps a final day final.
func TestRowsPastRetentionAreDeleted(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	ctx := context.Background()
	now := time.Date(2036, 8, 20, 12, 0, 0, 0, time.UTC)
	hourCutoff := now.AddDate(0, 0, -30)
	dayCutoff := time.Date(2036, 8, 20, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -400)
	zeros := make([]int64, histogramBuckets)

	for _, h := range []time.Time{hourCutoff.Add(-time.Hour), hourCutoff, time.Date(2036, 8, 1, 10, 0, 0, 0, time.UTC)} {
		if _, err := db.NewRaw(`INSERT INTO edge_stats_hourly (site_id, hour, requests, errors, not_found, bytes, bot_requests, duration_hist)
			VALUES (?, ?, 1, 0, 0, 0, 0, ?)`, site, h, pgdialect.Array(zeros)).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []time.Time{dayCutoff.AddDate(0, 0, -1), dayCutoff, time.Date(2036, 8, 1, 0, 0, 0, 0, time.UTC)} {
		if _, err := db.NewRaw(`INSERT INTO edge_stats_daily (site_id, day, requests, errors, not_found, bytes, bot_requests, duration_hist)
			VALUES (?, ?::date, 1, 0, 0, 0, 0, ?)`, site, d.Format(time.DateOnly), pgdialect.Array(zeros)).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := db.NewRaw(`INSERT INTO edge_stats_daily_top (site_id, day, dimension, value, visitors, visits, page_views)
			VALUES (?, ?::date, 'page', '/', 1, 1, 1)`, site, d.Format(time.DateOnly)).Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Records for days inside and outside the run's window: 2036-07-21 and
	// 2036-08-01 are final, as every past day is in practice, so the run leaves
	// the rows above for retention to judge; the day cutoff and the day before
	// it are outside the window. A record goes with its daily row, after 400
	// days, and with nothing else. 2036-08-10 has a request and no record, so
	// the run recomputes it and records it final.
	seedRequests(t, db, site, pageViewRow(time.Date(2036, 8, 10, 9, 0, 0, 0, time.UTC), "r-open", "/"))
	recordedAt := time.Date(2036, 8, 13, 3, 0, 0, 0, time.UTC)
	for _, d := range []string{dayCutoff.AddDate(0, 0, -1).Format(time.DateOnly), dayCutoff.Format(time.DateOnly), "2036-07-21", "2036-08-01", "2036-08-05", "2036-08-12"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO edge_stats_final (day, finalized_at) VALUES (?::date, ?)`, d, recordedAt); err != nil {
			t.Fatal(err)
		}
	}
	// Every row past retention is deleted, whoever wrote it, so the expected
	// count is read rather than assumed: the table is shared with the lane.
	var pastRetention int64
	if err := db.NewRaw(`SELECT (SELECT count(*) FROM edge_stats_hourly WHERE hour < ?)
		+ (SELECT count(*) FROM edge_stats_daily WHERE day < ?::date)
		+ (SELECT count(*) FROM edge_stats_daily_top WHERE day < ?::date)`,
		hourCutoff, dayCutoff.Format(time.DateOnly), dayCutoff.Format(time.DateOnly)).Scan(ctx, &pastRetention); err != nil {
		t.Fatal(err)
	}

	rep := runRollup(t, testSession, now)
	if rep.Deleted != pastRetention || pastRetention < 3 {
		t.Errorf("retention deleted %d rows, want the %d past retention, this test's three among them", rep.Deleted, pastRetention)
	}
	hourly := hourlyOf(t, db, site)
	for _, k := range []string{hourCutoff.Format("2006-01-02T15"), "2036-08-01T10"} {
		if _, ok := hourly[k]; !ok {
			t.Errorf("hourly row %s was deleted, want it kept", k)
		}
	}
	if _, ok := hourly[hourCutoff.Add(-time.Hour).Format("2006-01-02T15")]; ok {
		t.Error("an hourly row older than thirty days was kept")
	}
	daily := dailyOf(t, db, site)
	for _, k := range []string{dayCutoff.Format(time.DateOnly), "2036-08-01"} {
		if _, ok := daily[k]; !ok {
			t.Errorf("daily row %s was deleted, want it kept", k)
		}
		if len(topOf(t, db, site, mustDay(t, k))) != 1 {
			t.Errorf("the top row for %s was deleted, want it kept", k)
		}
	}
	old := dayCutoff.AddDate(0, 0, -1)
	if _, ok := daily[old.Format(time.DateOnly)]; ok {
		t.Error("a daily row older than 400 days was kept")
	}
	if len(topOf(t, db, site, old)) != 0 {
		t.Error("a top row older than 400 days was kept")
	}
	final := finalDays(t, db, old, now)
	if _, ok := final[old.Format(time.DateOnly)]; ok {
		t.Error("a finalization record older than 400 days was kept")
	}
	for _, d := range []string{dayCutoff.Format(time.DateOnly), "2036-07-21", "2036-08-01", "2036-08-05", "2036-08-12"} {
		if got, ok := final[d]; !ok || !got.Equal(recordedAt) {
			t.Errorf("the record for %s = %v (%v), want it kept as it was", d, got, ok)
		}
	}
	if _, ok := final["2036-08-10"]; !ok {
		t.Error("an open day in the window was not recorded final by the run")
	}
}

func mustDay(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The builtin is the scheduled automation's and nobody else's: the same id
// with any other role, any other automation's principal, a cluster owner by
// hand and an ordinary user are all refused before the database is touched.
func TestTheBuiltinRefusesAnyoneButItsMaintenancePrincipal(t *testing.T) {
	principal := auth.MaintenanceActor(auth.AutomationOriginEngine, "siteStatisticsRollupSweep")
	if principal == nil {
		t.Fatal("siteStatisticsRollupSweep is not on the maintenance list, so its automation runs as a reader and every run is refused")
	}

	touched := false
	refusing := NewIntegration(nil, func() *bun.DB { touched = true; return nil }, touchingSession(&touched), quietLogger())
	for name, ac := range map[string]*auth.AccessContext{
		"an ordinary user":              {UserId: "user-ordinary", Role: auth.RoleWriter},
		"a cluster owner by hand":       {UserId: "user-owner", Role: auth.RoleOwner},
		"another automation's":          auth.MaintenanceActor(auth.AutomationOriginEngine, "checkDeployableHealth"),
		"the principal's id as reader":  {UserId: principal.UserId, Role: auth.RoleReader},
		"the ordinary automation actor": {UserId: "system:automation:siteStatisticsRollupSweep", Role: auth.RoleReader},
	} {
		nodes, err := refusing.handleRollup(auth.ContextWithAccess(context.Background(), ac), nil, 0)
		if err == nil || nodes != nil {
			t.Errorf("%s: got %d nodes and error %v, want a refusal", name, len(nodes), err)
		}
	}
	if nodes, err := refusing.handleRollup(context.Background(), nil, 0); err == nil || nodes != nil {
		t.Errorf("no caller at all: got %d nodes and error %v, want a refusal", len(nodes), err)
	}
	if touched {
		t.Error("a refused call reached for the database")
	}

	// And the principal is admitted, and answered one node with the report.
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	now := time.Date(2036, 9, 1, 12, 0, 0, 0, time.UTC)
	seedRequests(t, db, site, pageViewRow(dayAt(now.Truncate(24*time.Hour), 9, 0), "p1", "/"))
	admitting := NewIntegration(nil, func() *bun.DB { return db }, testSession, quietLogger())
	admitting.now = func() time.Time { return now }
	nodes, err := admitting.handleRollup(auth.ContextWithAccess(context.Background(), principal), nil, 0)
	if err != nil {
		t.Fatalf("the maintenance principal was refused: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Concept != "integration:siteTraffic:rollup" {
		t.Fatalf("got %+v, want one integration:siteTraffic:rollup node", nodes)
	}
	var rep RollupReport
	if err := json.Unmarshal(nodes[0].Payload, &rep); err != nil {
		t.Fatalf("decode the report: %v", err)
	}
	if rep.Days < 1 || rep.Hours < 1 {
		t.Errorf("the report says %+v, want today recomputed", rep)
	}
	wantFigures(t, "day", dailyOf(t, db, site), "2036-09-01", measuredFigures(1, 1, 1, 1))
}

// A second rollup while one runs in this process answers nothing and does
// nothing: a run slower than the schedule must not pile up behind itself.
func TestASecondRollupInTheSameProcessIsSkipped(t *testing.T) {
	touched := false
	i := NewIntegration(nil, func() *bun.DB { touched = true; return nil }, touchingSession(&touched), quietLogger())
	i.rollupRunning.Store(true)
	ctx := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor(auth.AutomationOriginEngine, "siteStatisticsRollupSweep"))
	nodes, err := i.handleRollup(ctx, nil, 0)
	if err != nil || nodes != nil || touched {
		t.Errorf("got %d nodes, error %v, database touched %v; want a quiet skip", len(nodes), err, touched)
	}
}

// touchingSession is a session opener that only records that it was asked, for
// a test that wants the database left alone.
func touchingSession(touched *bool) MaintenanceSession {
	return func(context.Context) (*sql.Conn, func(), error) {
		*touched = true
		return nil, nil, fmt.Errorf("this test opens no maintenance session")
	}
}

// D17: a visitor's page views more than thirty minutes apart are two visits,
// each counted in the hour it started.
//
// Views at 10:00, 10:20, 10:55 and 11:30 are THREE visits, not two: 10:20 to
// 10:55 is 35 minutes and so is 10:55 to 11:30, both over thirty, so the third
// starts at 11:30, in hour 11. Two of them start in hour 10. The gap is
// measured from the previous page view, never from the visit's start.
func TestAGapOverThirtyMinutesStartsANewVisit(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	day := time.Date(2036, 5, 5, 0, 0, 0, 0, time.UTC)
	seedRequests(t, db, site,
		pageViewRow(dayAt(day, 10, 0), "g1", "/a"),
		pageViewRow(dayAt(day, 10, 20), "g1", "/b"),
		pageViewRow(dayAt(day, 10, 55), "g1", "/c"),
		pageViewRow(dayAt(day, 11, 30), "g1", "/d"),
		// Exactly thirty minutes is not over thirty: one visit.
		pageViewRow(dayAt(day, 14, 0), "g2", "/a"),
		pageViewRow(dayAt(day, 14, 30), "g2", "/b"),
		// Thirty minutes and a second is: two.
		pageViewRow(dayAt(day, 15, 0), "g3", "/a"),
		pageViewRow(dayAt(day, 15, 30).Add(time.Second), "g3", "/b"),
		// An hour of reading, twenty minutes a page: one visit, because no
		// gap is over thirty, though the last page is an hour after the first.
		pageViewRow(dayAt(day, 16, 0), "g4", "/a"),
		pageViewRow(dayAt(day, 16, 20), "g4", "/b"),
		pageViewRow(dayAt(day, 16, 40), "g4", "/c"),
		pageViewRow(dayAt(day, 17, 0), "g4", "/d"),
	)
	runRollup(t, testSession, dayAt(day, 20, 0))

	hourly := hourlyOf(t, db, site)
	wantFigures(t, "hour", hourly, "2036-05-05T10", measuredFigures(1, 2, 1, 3))
	wantFigures(t, "hour", hourly, "2036-05-05T11", measuredFigures(1, 1, 1, 1))
	wantFigures(t, "hour", hourly, "2036-05-05T14", measuredFigures(1, 1, 0, 2))
	wantFigures(t, "hour", hourly, "2036-05-05T15", measuredFigures(1, 2, 2, 2))
	wantFigures(t, "hour", hourly, "2036-05-05T16", measuredFigures(1, 1, 0, 3))
	wantFigures(t, "hour", hourly, "2036-05-05T17", measuredFigures(1, 0, 0, 1))
	wantFigures(t, "day", dailyOf(t, db, site), "2036-05-05", measuredFigures(4, 7, 4, 12))
}

// D17: a bounce is a visit of one page view.
func TestAOnePageVisitIsABounce(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	day := time.Date(2036, 5, 6, 0, 0, 0, 0, time.UTC)
	seedRequests(t, db, site,
		pageViewRow(dayAt(day, 10, 5), "b1", "/"),
		pageViewRow(dayAt(day, 10, 10), "b2", "/"),
		pageViewRow(dayAt(day, 10, 15), "b2", "/next"),
		// An hour apart: two visits of one page each, so two bounces.
		pageViewRow(dayAt(day, 10, 40), "b3", "/"),
		pageViewRow(dayAt(day, 11, 40), "b3", "/"),
	)
	runRollup(t, testSession, dayAt(day, 20, 0))

	hourly := hourlyOf(t, db, site)
	wantFigures(t, "hour", hourly, "2036-05-06T10", measuredFigures(3, 3, 2, 4))
	wantFigures(t, "hour", hourly, "2036-05-06T11", measuredFigures(1, 1, 1, 1))
	wantFigures(t, "day", dailyOf(t, db, site), "2036-05-06", measuredFigures(3, 4, 3, 5))
}

// D17: a visit is counted once, in the hour it started, so hourly visits add
// up to the day's without counting a visit twice.
func TestAVisitCountsInTheHourItStarted(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	day := time.Date(2036, 5, 7, 0, 0, 0, 0, time.UTC)
	seedRequests(t, db, site, pageViewRow(dayAt(day, 10, 50), "h1", "/"), pageViewRow(dayAt(day, 11, 10), "h1", "/next"))
	runRollup(t, testSession, dayAt(day, 20, 0))

	hourly := hourlyOf(t, db, site)
	wantFigures(t, "hour", hourly, "2036-05-07T10", measuredFigures(1, 1, 0, 1))
	// Measured, with a page view and no visit starting in it: 0, not NULL.
	wantFigures(t, "hour", hourly, "2036-05-07T11", measuredFigures(1, 0, 0, 1))
	wantFigures(t, "day", dailyOf(t, db, site), "2036-05-07", measuredFigures(1, 1, 0, 2))
}

// D17: a visit that crosses midnight UTC is two visits. The edge's visitor hash
// changes at midnight, so a real visitor arrives as two ids anyway; this test
// writes the SAME id on both sides, so the split is shown to be the rollup's
// own rule and not only a side effect of the hash.
func TestAVisitAcrossMidnightSplitsInTwo(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	first := time.Date(2036, 5, 8, 0, 0, 0, 0, time.UTC)
	second := first.AddDate(0, 0, 1)
	seedRequests(t, db, site, pageViewRow(dayAt(first, 23, 50), "m1", "/"), pageViewRow(dayAt(second, 0, 10), "m1", "/next"))
	runRollup(t, testSession, dayAt(second, 0, 30))

	hourly := hourlyOf(t, db, site)
	wantFigures(t, "hour", hourly, "2036-05-08T23", measuredFigures(1, 1, 1, 1))
	wantFigures(t, "hour", hourly, "2036-05-09T00", measuredFigures(1, 1, 1, 1))
	daily := dailyOf(t, db, site)
	wantFigures(t, "day", daily, "2036-05-08", measuredFigures(1, 1, 1, 1))
	wantFigures(t, "day", daily, "2036-05-09", measuredFigures(1, 1, 1, 1))
}

// A visit counts once under the page it entered on and the source it arrived
// from, and under its first page view's country -- not under every page it
// went on to see.
func TestTopRowsCountAVisitUnderItsEntryPageAndSource(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	day := time.Date(2036, 5, 10, 0, 0, 0, 0, time.UTC)
	with := func(r Row, source, country string) Row {
		r.Source, r.Country = source, country
		return r
	}
	seedRequests(t, db, site,
		// e1 arrives on /a from google and clicks through to /b and /c.
		with(pageViewRow(dayAt(day, 9, 0), "e1", "/a"), "google", "DE"),
		with(pageViewRow(dayAt(day, 9, 5), "e1", "/b"), "", "DE"),
		with(pageViewRow(dayAt(day, 9, 10), "e1", "/c"), "", "DE"),
		// e2 arrives on /b with no referrer, from an address no country
		// database knows, and goes on to /a.
		with(pageViewRow(dayAt(day, 9, 30), "e2", "/b"), "direct", ""),
		with(pageViewRow(dayAt(day, 9, 35), "e2", "/a"), "", ""),
		// e3 arrives on /a from a newsletter; an hour later it is back on
		// /b, a page inside the site, and then arrives again from mail. The
		// second visit's source is its first page view that has one.
		with(pageViewRow(dayAt(day, 12, 0), "e3", "/a"), "news", "FR"),
		with(pageViewRow(dayAt(day, 13, 0), "e3", "/b"), "", "FR"),
		with(pageViewRow(dayAt(day, 13, 5), "e3", "/c"), "mail", "FR"),
	)
	runRollup(t, testSession, dayAt(day, 20, 0))

	top := topOf(t, db, site, day)
	for key, want := range map[string]rollupTopRow{
		"page|/a":           {Visitors: 3, Visits: 2, PageViews: 3},
		"page|/b":           {Visitors: 3, Visits: 2, PageViews: 3},
		"page|/c":           {Visitors: 2, Visits: 0, PageViews: 2},
		"source|google":     {Visitors: 1, Visits: 1, PageViews: 1},
		"source|direct":     {Visitors: 1, Visits: 1, PageViews: 1},
		"source|news":       {Visitors: 1, Visits: 1, PageViews: 1},
		"source|mail":       {Visitors: 1, Visits: 1, PageViews: 1},
		"country|DE":        {Visitors: 1, Visits: 1, PageViews: 3},
		"country|FR":        {Visitors: 1, Visits: 2, PageViews: 3},
		"country|(unknown)": {Visitors: 1, Visits: 1, PageViews: 2},
	} {
		got, ok := top[key]
		if !ok {
			t.Errorf("no %s row", key)
			continue
		}
		if got.Visitors != want.Visitors || got.Visits != want.Visits || got.PageViews != want.PageViews {
			t.Errorf("%s = %d visitors, %d visits, %d page views; want %d, %d, %d", key, got.Visitors, got.Visits, got.PageViews, want.Visitors, want.Visits, want.PageViews)
		}
	}
	if len(top) != 10+3 {
		t.Errorf("got %d top rows %v, want the ten above and one each for desktop, Firefox and Linux", len(top), topKeys(top))
	}
}

// D5: days and hours are UTC. date_trunc on a timestamptz truncates in the
// SESSION time zone, so under a +05:30 session a naive rollup files 23:40 UTC
// in the next day and in an hour that starts at :30. This runs the rollup over
// such a connection.
func TestDaysAndHoursAreUTCWhateverTheSessionTimeZone(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	first := time.Date(2036, 5, 12, 0, 0, 0, 0, time.UTC)
	second := first.AddDate(0, 0, 1)
	seedRequests(t, db, site, pageViewRow(dayAt(first, 23, 40), "k1", "/"), pageViewRow(dayAt(second, 0, 20), "k2", "/"))

	// Every session the run opens is put in +05:30 before the run sees it.
	kolkata := func(ctx context.Context) (*sql.Conn, func(), error) {
		conn, closeSession, err := testSession(ctx)
		if err != nil {
			return nil, nil, err
		}
		if _, err := conn.ExecContext(ctx, "SET TIME ZONE 'Asia/Kolkata'"); err != nil {
			closeSession()
			return nil, nil, err
		}
		return conn, closeSession, nil
	}
	conn, closeSession, err := kolkata(context.Background())
	if err != nil {
		t.Fatalf("open a +05:30 session: %v", err)
	}
	var zone string
	err = conn.QueryRowContext(context.Background(), "SHOW TIME ZONE").Scan(&zone)
	closeSession()
	if err != nil || zone != "Asia/Kolkata" {
		t.Fatalf("the session time zone is %q (%v), want Asia/Kolkata -- the test would prove nothing", zone, err)
	}

	runRollup(t, kolkata, dayAt(second, 1, 0))

	hourly := hourlyOf(t, db, site)
	if len(hourly) != 2 {
		t.Errorf("hourly rows %v, want 2036-05-12T23 and 2036-05-13T00", statsKeys(hourly))
	}
	wantFigures(t, "UTC hour", hourly, "2036-05-12T23", measuredFigures(1, 1, 1, 1))
	wantFigures(t, "UTC hour", hourly, "2036-05-13T00", measuredFigures(1, 1, 1, 1))
	var aligned bool
	if err := db.NewRaw("SELECT bool_and(date_trunc('hour', hour AT TIME ZONE 'UTC') = hour AT TIME ZONE 'UTC') FROM edge_stats_hourly WHERE site_id = ?", site).Scan(context.Background(), &aligned); err != nil || !aligned {
		t.Errorf("an hourly row does not start on a UTC hour (%v)", err)
	}
	daily := dailyOf(t, db, site)
	wantFigures(t, "UTC day", daily, "2036-05-12", measuredFigures(1, 1, 1, 1))
	wantFigures(t, "UTC day", daily, "2036-05-13", measuredFigures(1, 1, 1, 1))
}

// ONE SNAPSHOT, FIXED AFTER WHATEVER HOLDS THE DAY'S CHUNK. TimescaleDB
// compresses a chunk under an exclusive lock and moves its rows as it goes, and
// a recompute whose snapshot was fixed before such a change committed would
// read the day as that change left it for an older snapshot -- empty, if the
// move truncates. The recompute takes ACCESS SHARE on edge_request before its
// first query, so it waits for the holder and fixes its snapshot only after the
// holder has committed. Here the holder locks today's chunk as compression does
// and writes a page view under the lock; the recompute must count it.
//
// The holder is simulated, not a real compress_chunk: on the CI image a
// snapshot taken before a first compression still reads the compressed rows,
// so a real compression would not tell the two orders apart. A committed write
// under the same lock does, and it is the ordering the lock exists to give.
func TestTheSnapshotIsTakenAfterALockOnTheDaysChunk(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	ctx := context.Background()
	day := time.Date(2036, 9, 5, 0, 0, 0, 0, time.UTC)
	seedRequests(t, db, site, pageViewRow(dayAt(day, 9, 0), "h1", "/"))

	// The day's chunk where TimescaleDB keeps one, else the table itself.
	target := "edge_request"
	var timescale bool
	if err := db.NewRaw("SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')").Scan(ctx, &timescale); err != nil {
		t.Fatal(err)
	}
	if timescale {
		if err := db.NewRaw(`SELECT c::text FROM show_chunks('edge_request', newer_than => ?::timestamptz, older_than => ?::timestamptz) c LIMIT 1`,
			day, day.AddDate(0, 0, 1)).Scan(ctx, &target); err != nil {
			t.Fatalf("find the day's chunk: %v", err)
		}
	}

	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback() })
	if _, err := holder.ExecContext(ctx, "LOCK TABLE "+target+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock %s: %v", target, err)
	}
	held := []Row{pageViewRow(dayAt(day, 10, 0), "h2", "/held")}
	held[0].Id, held[0].SiteId, held[0].Node = newRowId(), site, "edge-rollup-test"
	if _, err := holder.NewInsert().Model(&held).Exec(ctx); err != nil {
		t.Fatalf("write under the lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := Rollup(ctx, testSession, dayAt(day, 12, 0))
		done <- err
	}()
	// Commit only once the recompute is waiting on the lock.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := db.NewRaw(`SELECT count(*) FROM pg_locks WHERE locktype = 'relation' AND relation = ?::regclass AND NOT granted`,
			target).Scan(ctx, &waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the rollup never waited on %s, so this test would prove nothing", target)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Rollup: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the rollup did not finish after the lock was released")
	}
	wantFigures(t, "the day", dailyOf(t, db, site), "2036-09-05", measuredFigures(2, 2, 2, 2))
}

// Two runs at once -- a leader change mid-run, say -- both finish, and the rows
// are what one run writes.
func TestTwoRollupsAtOnceBothFinish(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	day := time.Date(2036, 5, 14, 0, 0, 0, 0, time.UTC)
	seedRequests(t, db, site, pageViewRow(dayAt(day, 9, 0), "c1", "/"), pageViewRow(dayAt(day, 9, 5), "c1", "/next"), pageViewRow(dayAt(day, 10, 0), "c2", "/"))
	now := dayAt(day, 12, 0)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = Rollup(context.Background(), testSession, now)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("run %d of 4 at once failed: %v", i, err)
		}
	}
	wantFigures(t, "day", dailyOf(t, db, site), "2036-05-14", measuredFigures(2, 2, 1, 3))
}

// A DAY'S STATEMENT MAY RUN PAST TEN SECONDS. pgdriver gives every connection
// of the request pool a 10 s client read deadline, after which the client
// reports an i/o timeout and the backend carries on unheard
// (component/database, maintenance_session.go). A day's recompute on a busy
// cluster can take longer than that, and on the pool it failed at the client
// on every run -- the day never written, never final -- while its orphan kept
// working. Here the day's first statement, the rollup's own lock, waits 10.5 s
// on a holder, as it waits on another replica's run, and the day must still be
// written.
//
// It takes about eleven seconds, which is the property itself: a wait past the
// read deadline cannot be shown in less. component/database's own
// maintenance-session test pays the same, and like it this runs in every lane;
// the package has no -short convention, and CI's database lane passes none.
func TestADayStatementOutlivesTheRequestPoolsReadDeadline(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	ctx := context.Background()
	day := time.Date(2036, 9, 8, 0, 0, 0, 0, time.UTC)
	seedRequests(t, db, site, pageViewRow(dayAt(day, 9, 0), "slow-1", "/"))

	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback() })
	// EXCLUSIVE conflicts with the rollup's SHARE ROW EXCLUSIVE, and with no
	// reader, so nothing but a rollup or a writer waits on it.
	if _, err := holder.ExecContext(ctx, "LOCK TABLE edge_stats_daily IN EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock edge_stats_daily: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := Rollup(ctx, testSession, dayAt(day, 12, 0))
		done <- err
	}()
	waitForLockWaiter(t, db, "edge_stats_daily")
	time.Sleep(10*time.Second + 500*time.Millisecond)
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a run whose day waited 10.5 s on its lock failed: %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the rollup did not finish after the lock was released")
	}
	wantFigures(t, "the day", dailyOf(t, db, site), "2036-09-08", measuredFigures(1, 1, 1, 1))
}

// waitForLockWaiter returns once some session waits for a lock on relation,
// and fails the test if none does within ten seconds.
func waitForLockWaiter(t *testing.T, db *bun.DB, relation string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := db.NewRaw(`SELECT count(*) FROM pg_locks WHERE locktype = 'relation' AND relation = ?::regclass AND NOT granted`,
			relation).Scan(context.Background(), &waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing ever waited on %s, so this test would prove nothing", relation)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// THE REFERENCE SEGMENTATION'S CONTRACT over a window that is not one UTC day,
// which is how the request-log read the 24-hour period replaced called it
// (rollup_reference_test.go). TestTheTwentyFourHourReadAgreesWithTheRequestLog
// holds today's read to that reference, so the reference has to be right where
// a window cuts a day: none of the three properties below is visible through a
// whole day.
//
//   - It answers the visits that STARTED in the window: one begun before it
//     and running into it is not counted again where the window cuts it.
//   - A visit never crosses midnight UTC, even under one visitor id, and even
//     when the gap across midnight is thirty minutes, which is one visit.
//   - A visit that started in the window is whole: its page views after the
//     window's end count, so it is not mistaken for a bounce.
func TestTheVisitSegmentationAnswersTheVisitsThatStartedInAWindow(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	first := time.Date(2036, 5, 15, 0, 0, 0, 0, time.UTC)
	second := first.AddDate(0, 0, 1)
	seedRequests(t, db, site,
		// A: begins before the window, continues into it.
		pageViewRow(dayAt(first, 22, 50), "w1", "/a"),
		pageViewRow(dayAt(first, 23, 10), "w1", "/b"),
		pageViewRow(dayAt(first, 23, 35), "w1", "/c"),
		// B: thirty minutes after A's last page, but past midnight.
		pageViewRow(dayAt(second, 0, 5), "w1", "/d"),
		// C: starts in the window and ends after it.
		pageViewRow(dayAt(second, 0, 50), "w1", "/e"),
		pageViewRow(dayAt(second, 1, 10), "w1", "/f"),
	)

	segment, args := visitSegmentation(dayAt(first, 23, 0), dayAt(second, 1, 0))
	type visit struct {
		StartedAt time.Time `bun:"started_at"`
		Views     int64     `bun:"views"`
		EntryPage string    `bun:"entry_page"`
		Day       string    `bun:"day"`
	}
	var visits []visit
	q := `SELECT started_at, views, entry_page, day::text AS day FROM (` + segment + `) v WHERE site_id = ? ORDER BY started_at`
	if err := db.NewRaw(q, append(args, site)...).Scan(context.Background(), &visits); err != nil {
		t.Fatalf("run the segmentation: %v", err)
	}
	var got []string
	for _, v := range visits {
		got = append(got, fmt.Sprintf("%s %s %s views=%d", v.Day, v.StartedAt.UTC().Format("15:04"), v.EntryPage, v.Views))
	}
	want := []string{"2036-05-16 00:05 /d views=1", "2036-05-16 00:50 /e views=2"}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("visits started in [23:00, 01:00) = %v, want %v", got, want)
	}
}

// A utm_source is the client's to write, and "(other)" is a source somebody
// can write. Kept in the top hundred it would share the fold's primary key
// and fail the whole day's insert -- every site's -- on every run. The fold
// groups by its label instead, so the crafted value joins it.
func TestACraftedOtherSourceJoinsTheFoldInsteadOfFailingTheDay(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	day := time.Date(2036, 5, 17, 0, 0, 0, 0, time.UTC)
	var rows []Row
	for i := 0; i < 3; i++ {
		r := pageViewRow(dayAt(day, 8, i), fmt.Sprintf("o-crafted-%d", i), "/")
		r.Source = topOther
		rows = append(rows, r)
	}
	for i := 0; i < 101; i++ {
		r := pageViewRow(dayAt(day, 9, 0).Add(time.Duration(i)*time.Second), fmt.Sprintf("o-%03d", i), "/")
		r.Source = fmt.Sprintf("s%03d", i)
		rows = append(rows, r)
	}
	seedRequests(t, db, site, rows...)

	runRollup(t, testSession, dayAt(day, 20, 0))

	top := topOf(t, db, site, day)
	// "(other)" ranks first on its three visitors; s000..s098 fill the
	// hundred; s099 and s100 fold, and the fold and the crafted value are one
	// row.
	if got := top["source|"+topOther]; got.Visitors != 5 || got.Visits != 5 || got.PageViews != 5 {
		t.Errorf("the (other) source row = %+v, want the crafted value's 3 and the fold's 2 together", got)
	}
	if _, ok := top["source|s098"]; !ok {
		t.Error("s098 is missing, want it kept in the hundred")
	}
	if _, ok := top["source|s099"]; ok {
		t.Error("s099 is kept, want it folded")
	}
}

func statsKeys(m map[string]rollupRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func topKeys(m map[string]rollupTopRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
