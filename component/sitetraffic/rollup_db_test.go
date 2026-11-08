package sitetraffic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// rollup_db_test.go -- the statistics rollup against the real tables.
//
// Nothing here is provable in process: the hours and days are SQL's, the
// distinct visitors and the visit windows are SQL's, and the backfill is a
// decision read off what the tables hold.
//
// THE TESTS OWN A BAND OF DATES. The rollup recomputes every site in the days
// it covers and decides a backfill from what its 400-day window holds, so a
// test sharing that window with anybody else's rows would be asserting on
// theirs. Every `now` below is in 2036, whose windows reach back no further
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
	from, to := rollupBandStart.Format(time.DateOnly), rollupBandEnd.Format(time.DateOnly)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"DELETE FROM edge_request WHERE served_at >= ? AND served_at < ?", []any{rollupBandStart, rollupBandEnd}},
		{"DELETE FROM edge_stats_hourly WHERE hour >= ? AND hour < ?", []any{rollupBandStart, rollupBandEnd}},
		{"DELETE FROM edge_stats_daily WHERE day >= ?::date AND day < ?::date", []any{from, to}},
		{"DELETE FROM edge_stats_daily_top WHERE day >= ?::date AND day < ?::date", []any{from, to}},
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("clear the rollup tests' band (%s): %v", q.sql, err)
		}
	}
}

// at is an instant on a UTC day.
func dayAt(day time.Time, hour, minute int) time.Time {
	return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

// view is a measured page view by one visitor, with the dimensions a desktop
// Firefox on Linux in the Netherlands would carry.
func pageViewRow(when time.Time, visitor, path string) Row {
	return Row{
		ServedAt: when, Status: 200, PathClass: PathClassDocument, Bytes: 2048, DurationNs: 3_000_000,
		Measured: true, PageView: true, Path: path, Visitor: visitor,
		Country: "NL", Device: "desktop", Browser: "Firefox", OS: "Linux",
	}
}

// request is a measured request that is not a page view.
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

func runRollup(t *testing.T, db *bun.DB, now time.Time) RollupReport {
	t.Helper()
	rep, err := Rollup(context.Background(), db, now)
	if err != nil {
		t.Fatalf("Rollup at %s: %v", now.Format(time.RFC3339), err)
	}
	return rep
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

// histOf is the histogram the given durations should fold into.
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

	runRollup(t, db, dayAt(day, 12, 30))

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
	runRollup(t, db, now)
	first := rollupSnapshot(t, db, site, day)
	if !strings.Contains(first, "2036-03-12T08") || !strings.Contains(first, "page|/shop") {
		t.Fatalf("the first run wrote nothing worth comparing:\n%s", first)
	}

	rep := runRollup(t, db, now)
	if rep.Backfilled {
		t.Errorf("a second run backfilled again: %+v", rep)
	}
	if again := rollupSnapshot(t, db, site, day); again != first {
		t.Errorf("a re-run changed the rows.\nfirst:\n%s\nre-run:\n%s", first, again)
	}
	runRollup(t, db, now.Add(5*time.Minute))
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

	runRollup(t, db, dayAt(day, 18, 0))

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

	runRollup(t, db, dayAt(day, 20, 0))

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

// THE FIRST RUN BACKFILLS. Every day still in the request log gets its daily
// row, and every hour of the last thirty days its hourly row; a day older than
// thirty days gets no hours, because the hourly table keeps thirty days.
func TestTheFirstRunBackfillsEveryDayStillLogged(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	now := time.Date(2036, 6, 15, 12, 0, 0, 0, time.UTC)
	seedRequests(t, db, site,
		pageViewRow(time.Date(2036, 5, 1, 10, 0, 0, 0, time.UTC), "b1", "/"),
		pageViewRow(time.Date(2036, 6, 1, 8, 0, 0, 0, time.UTC), "b2", "/"),
		Row{ServedAt: time.Date(2036, 6, 1, 8, 30, 0, 0, time.UTC), Status: 200},
		pageViewRow(time.Date(2036, 6, 14, 22, 0, 0, 0, time.UTC), "b3", "/"),
		pageViewRow(time.Date(2036, 6, 15, 9, 0, 0, 0, time.UTC), "b4", "/"),
	)

	rep := runRollup(t, db, now)
	// 2036-05-01 through 2036-06-15 is 46 days. The hours run from the first
	// one the hourly table keeps, 2036-05-16 12:00, through the current one:
	// thirty days of them and the hour under way.
	if !rep.Backfilled || rep.Days != 46 || rep.Hours != 30*24+1 {
		t.Errorf("the first run reported %+v, want a backfill of 46 days and %d hours", rep, 30*24+1)
	}
	daily := dailyOf(t, db, site)
	for _, d := range []string{"2036-05-01", "2036-06-01", "2036-06-14", "2036-06-15"} {
		wantFigures(t, "backfilled day", daily, d, measuredFigures(1, 1, 1, 1))
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

	again := runRollup(t, db, now)
	if again.Backfilled || again.Days != 1 || again.Hours != 13 {
		t.Errorf("the next run reported %+v, want today alone: 1 day, its 13 hours so far, no backfill", again)
	}
}

// A backfill that stops part way is finished by the next run. The backfill
// walks from the newest day to the oldest and stops at the first day it could
// not write, so what is missing is always the oldest days, and the next run
// finds them below the oldest daily row.
func TestAnInterruptedBackfillResumesOnTheNextRun(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	now := time.Date(2036, 6, 20, 12, 0, 0, 0, time.UTC)
	for _, d := range []int{5, 10, 15, 20} {
		seedRequests(t, db, site, pageViewRow(time.Date(2036, 6, d, 9, 0, 0, 0, time.UTC), "i"+strconv.Itoa(d), "/"))
	}
	runRollup(t, db, now)

	// What a run that stopped after 2036-06-12 leaves behind.
	if _, err := db.ExecContext(context.Background(), "DELETE FROM edge_stats_daily WHERE site_id = ? AND day < '2036-06-12'", site); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "DELETE FROM edge_stats_hourly WHERE site_id = ? AND hour < '2036-06-12T00:00:00Z'", site); err != nil {
		t.Fatal(err)
	}

	rep := runRollup(t, db, now)
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

// Yesterday is recomputed until 02:00 UTC, so a request served at 23:59 and
// written a little later still lands in yesterday's figures -- and after that,
// yesterday is left as it is.
func TestYesterdayIsRecomputedUntilTwoAfterMidnight(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	yesterday := time.Date(2036, 7, 9, 0, 0, 0, 0, time.UTC)
	today := yesterday.AddDate(0, 0, 1)

	seedRequests(t, db, site, pageViewRow(dayAt(yesterday, 20, 0), "y1", "/"))
	runRollup(t, db, dayAt(yesterday, 21, 0))

	seedRequests(t, db, site, pageViewRow(dayAt(yesterday, 23, 59), "y2", "/"))
	rep := runRollup(t, db, dayAt(today, 0, 30))
	if rep.Days != 2 {
		t.Errorf("at 00:30 the run recomputed %d days, want yesterday and today", rep.Days)
	}
	if got := dailyOf(t, db, site)["2036-07-09"]; got.PageViews == nil || *got.PageViews != 2 {
		t.Fatalf("at 00:30 yesterday reads %s, want the late page view counted", figuresOf(got))
	}

	seedRequests(t, db, site, pageViewRow(dayAt(yesterday, 23, 59).Add(30*time.Second), "y3", "/"))
	for _, when := range []time.Time{dayAt(today, 2, 0), dayAt(today, 2, 30)} {
		rep := runRollup(t, db, when)
		if rep.Days != 1 {
			t.Errorf("at %s the run recomputed %d days, want today alone", when.Format("15:04"), rep.Days)
		}
		if got := dailyOf(t, db, site)["2036-07-09"]; got.PageViews == nil || *got.PageViews != 2 {
			t.Errorf("at %s yesterday reads %s, want it left as it was at 00:30", when.Format("15:04"), figuresOf(got))
		}
		if got := hourlyOf(t, db, site)["2036-07-09T23"]; got.PageViews == nil || *got.PageViews != 1 {
			t.Errorf("at %s yesterday's 23:00 reads %s, want it left as it was at 00:30", when.Format("15:04"), figuresOf(got))
		}
	}
}

// Retention: hourly rows go after thirty days, daily and top rows after 400.
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
	// Every row past retention is deleted, whoever wrote it, so the expected
	// count is read rather than assumed: the table is shared with the lane.
	var pastRetention int64
	if err := db.NewRaw(`SELECT (SELECT count(*) FROM edge_stats_hourly WHERE hour < ?)
		+ (SELECT count(*) FROM edge_stats_daily WHERE day < ?::date)
		+ (SELECT count(*) FROM edge_stats_daily_top WHERE day < ?::date)`,
		hourCutoff, dayCutoff.Format(time.DateOnly), dayCutoff.Format(time.DateOnly)).Scan(ctx, &pastRetention); err != nil {
		t.Fatal(err)
	}

	rep := runRollup(t, db, now)
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
	principal := auth.MaintenanceActor("siteStatisticsRollupSweep")
	if principal == nil {
		t.Fatal("siteStatisticsRollupSweep is not on the maintenance list, so its automation runs as a reader and every run is refused")
	}

	touched := false
	refusing := NewIntegration(nil, func() *bun.DB { touched = true; return nil }, quietLogger())
	for name, ac := range map[string]*auth.AccessContext{
		"an ordinary user":              {UserId: "user-ordinary", Role: auth.RoleWriter},
		"a cluster owner by hand":       {UserId: "user-owner", Role: auth.RoleOwner},
		"another automation's":          auth.MaintenanceActor("checkDeployableHealth"),
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
	admitting := NewIntegration(nil, func() *bun.DB { return db }, quietLogger())
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
	i := NewIntegration(nil, func() *bun.DB { touched = true; return nil }, quietLogger())
	i.rollupRunning.Store(true)
	ctx := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("siteStatisticsRollupSweep"))
	nodes, err := i.handleRollup(ctx, nil, 0)
	if err != nil || nodes != nil || touched {
		t.Errorf("got %d nodes, error %v, database touched %v; want a quiet skip", len(nodes), err, touched)
	}
}

// D17: a visitor's page views more than thirty minutes apart are two visits,
// each counted in the hour it started.
//
// The brief's example, 10:00, 10:20, 10:55 and 11:30, says "two visits, both
// started in hour 10". By D17's own rule it is three: 10:20 to 10:55 is 35
// minutes and so is 10:55 to 11:30, both over thirty, so a third visit starts
// at 11:30 in hour 11. The rule is the owner's decision and wins over the
// example; the arithmetic is asserted here as the rule gives it.
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
	)
	runRollup(t, db, dayAt(day, 20, 0))

	hourly := hourlyOf(t, db, site)
	wantFigures(t, "hour", hourly, "2036-05-05T10", measuredFigures(1, 2, 1, 3))
	wantFigures(t, "hour", hourly, "2036-05-05T11", measuredFigures(1, 1, 1, 1))
	wantFigures(t, "hour", hourly, "2036-05-05T14", measuredFigures(1, 1, 0, 2))
	wantFigures(t, "hour", hourly, "2036-05-05T15", measuredFigures(1, 2, 2, 2))
	wantFigures(t, "day", dailyOf(t, db, site), "2036-05-05", measuredFigures(3, 6, 4, 8))
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
	runRollup(t, db, dayAt(day, 20, 0))

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
	runRollup(t, db, dayAt(day, 20, 0))

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
	runRollup(t, db, dayAt(second, 0, 30))

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
	runRollup(t, db, dayAt(day, 20, 0))

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

	kolkata := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"timezone": "Asia/Kolkata"}),
	)), pgdialect.New())
	t.Cleanup(func() { _ = kolkata.Close() })
	var zone string
	if err := kolkata.NewRaw("SHOW TIME ZONE").Scan(context.Background(), &zone); err != nil || zone != "Asia/Kolkata" {
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
			_, errs[i] = Rollup(context.Background(), db, now)
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

// THE SEGMENTATION'S CONTRACT over a window that is not one UTC day, which is
// how the 24-hour read calls it. The rollup's own windows are whole days, so
// none of the three properties below is visible through Rollup: a day window
// cannot cut a visit at midnight, start inside a visit, or end inside one.
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

	runRollup(t, db, dayAt(day, 20, 0))

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
