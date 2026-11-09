package sitetraffic

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/core/statsperiod"
)

// rollup_reference_db_test.go -- the fold and the 24-hour read held to the
// recompute and the request-log read they replaced (rollup_reference_test.go),
// over the same generated traffic.

// randomDay is a day of generated traffic for sites: visitors who come back
// within thirty minutes, at exactly thirty and after more; page views with no
// visitor id; a source on some first pages and on some later ones; empty
// dimensions; requests that are not page views, bots among them, of every
// status; and, in the day's first hours, requests the edge did not classify.
// The values stay under a hundred per dimension, so no top list folds and a
// fold's cut cannot differ from a recompute's by where it falls.
func randomDay(seed int64, day time.Time, sites []string) []struct {
	site string
	row  Row
} {
	rng := rand.New(rand.NewSource(seed))
	pick := func(values ...string) string { return values[rng.Intn(len(values))] }
	var out []struct {
		site string
		row  Row
	}
	add := func(site string, r Row) {
		out = append(out, struct {
			site string
			row  Row
		}{site, r})
	}
	for _, site := range sites {
		for v := 0; v < 25; v++ {
			visitor := fmt.Sprintf("rv-%d-%d", seed, v)
			at := day.Add(time.Duration(rng.Intn(20*60)) * time.Minute)
			for views := 1 + rng.Intn(6); views > 0 && at.Before(day.AddDate(0, 0, 1)); views-- {
				r := pageViewRow(at, visitor, pick("/", "/a", "/b", "/c", "/shop", "/shop/1", "/shop/2", "/about"))
				r.Country, r.Device = pick("NL", "DE", "", "US"), pick("desktop", "mobile", "", "tablet")
				r.Browser, r.OS = pick("Firefox", "Chrome", "Safari", ""), pick("Linux", "macOS", "iOS", "")
				if rng.Intn(4) == 0 {
					r.Source = pick("google", "news.example", "direct", "x.com")
				}
				r.DurationNs = int64(500_000 + rng.Intn(200_000_000))
				r.Bytes = int64(rng.Intn(100_000))
				add(site, r)
				switch rng.Intn(5) {
				case 0:
					at = at.Add(30 * time.Minute) // exactly thirty: the same visit
				case 1:
					at = at.Add(time.Duration(31+rng.Intn(200)) * time.Minute) // a new visit
				default:
					at = at.Add(time.Duration(1+rng.Intn(29)) * time.Minute)
				}
			}
		}
		for i := 0; i < 60; i++ {
			at := day.Add(time.Duration(rng.Intn(24*60*60)) * time.Second)
			r := requestRow(at, []int{200, 200, 200, 304, 404, 500}[rng.Intn(6)], int64(rng.Intn(50_000)), int64(rng.Intn(90_000_000)))
			r.Bot = rng.Intn(5) == 0
			add(site, r)
		}
		for i := 0; i < 8; i++ {
			// A page view with no visitor id: a page view, nobody's visit.
			r := pageViewRow(day.Add(time.Duration(rng.Intn(24*60))*time.Minute), "", pick("/", "/a"))
			add(site, r)
		}
		for i := 0; i < 6; i++ {
			at := day.Add(time.Duration(rng.Intn(3*60)) * time.Minute)
			add(site, Row{ServedAt: at, Status: 200, PathClass: PathClassDocument, Bytes: 300, DurationNs: 4_000_000})
		}
	}
	return out
}

// seedRandomDay writes a generated day through the sink's own insert.
func seedRandomDay(t *testing.T, db *bun.DB, rows []struct {
	site string
	row  Row
}) {
	t.Helper()
	bySite := map[string][]Row{}
	for _, r := range rows {
		bySite[r.site] = append(bySite[r.site], r.row)
	}
	for site, rs := range bySite {
		seedRequests(t, db, site, rs...)
	}
}

// referenceSnapshot renders what the recompute writes for the sites' day: its
// hours, its daily rows and its top lists, into temporary tables that leave
// with the transaction.
func referenceSnapshot(t *testing.T, db *bun.DB, sites []string, day time.Time) string {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	dayEnd := day.AddDate(0, 0, 1)
	for _, q := range []string{
		`CREATE TEMP TABLE ref_hourly (LIKE edge_stats_hourly) ON COMMIT DROP`,
		`CREATE TEMP TABLE ref_daily (LIKE edge_stats_daily) ON COMMIT DROP`,
		`CREATE TEMP TABLE ref_top (LIKE edge_stats_daily_top) ON COMMIT DROP`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []func() (string, []any){
		func() (string, []any) { return referenceBucketInsert("ref_hourly", "hour", hourOf, day, dayEnd) },
		func() (string, []any) { return referenceBucketInsert("ref_daily", "day", dayOf, day, dayEnd) },
		func() (string, []any) { return referenceTopInsert("ref_top", day, dayEnd) },
	} {
		q, args := s()
		if _, err := tx.NewRaw(q, args...).Exec(ctx); err != nil {
			t.Fatalf("run the recompute: %v", err)
		}
	}
	return snapshotTables(t, tx, sites, "ref_hourly", "ref_daily", "ref_top")
}

// publishedSnapshot renders what the rollup published for the sites' day.
func publishedSnapshot(t *testing.T, db *bun.DB, sites []string, day time.Time) string {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	return snapshotTables(t, tx, sites, "edge_stats_hourly", "edge_stats_daily", "edge_stats_daily_top")
}

// snapshotTables renders the sites' rows of an hourly, a daily and a top
// table, one line each, in order.
func snapshotTables(t *testing.T, tx bun.Tx, sites []string, hourly, daily, top string) string {
	t.Helper()
	ctx := context.Background()
	var lines []string
	var buckets []struct {
		Site   string  `bun:"site_id"`
		Bucket string  `bun:"bucket"`
		V      *int64  `bun:"visitors"`
		Vi     *int64  `bun:"visits"`
		B      *int64  `bun:"bounces"`
		P      *int64  `bun:"page_views"`
		R      int64   `bun:"requests"`
		E      int64   `bun:"errors"`
		N      int64   `bun:"not_found"`
		By     int64   `bun:"bytes"`
		Bo     int64   `bun:"bot_requests"`
		Hist   []int64 `bun:"duration_hist,array"`
	}
	for _, src := range []struct{ table, key string }{
		{hourly, `to_char(hour AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24')`},
		{daily, `day::text`},
	} {
		buckets = buckets[:0]
		q := `SELECT site_id, ` + src.key + ` AS bucket, visitors, visits, bounces, page_views, requests, errors, not_found, bytes, bot_requests, duration_hist
		        FROM ` + src.table + ` WHERE site_id IN (?)`
		if err := tx.NewRaw(q, bun.In(sites)).Scan(ctx, &buckets); err != nil {
			t.Fatalf("read %s: %v", src.table, err)
		}
		for _, b := range buckets {
			lines = append(lines, fmt.Sprintf("%s %s %s v=%s vi=%s b=%s p=%s r=%d e=%d n=%d by=%d bo=%d h=%v",
				src.table[len(src.table)-5:], b.Site, b.Bucket, num(b.V), num(b.Vi), num(b.B), num(b.P), b.R, b.E, b.N, b.By, b.Bo, b.Hist))
		}
	}
	var tops []struct {
		Site      string `bun:"site_id"`
		Day       string `bun:"day"`
		Dimension string `bun:"dimension"`
		Value     string `bun:"value"`
		Visitors  int64  `bun:"visitors"`
		Visits    int64  `bun:"visits"`
		PageViews int64  `bun:"page_views"`
	}
	if err := tx.NewRaw(`SELECT site_id, day::text AS day, dimension, value, visitors, visits, page_views FROM `+top+` WHERE site_id IN (?)`,
		bun.In(sites)).Scan(ctx, &tops); err != nil {
		t.Fatalf("read %s: %v", top, err)
	}
	for _, r := range tops {
		lines = append(lines, fmt.Sprintf("top %s %s %s|%s %d/%d/%d", r.Site, r.Day, r.Dimension, r.Value, r.Visitors, r.Visits, r.PageViews))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// diffLines names the lines of two snapshots that differ.
func diffLines(want, got string) string {
	w, g := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(want, "\n") {
		w[l] = true
	}
	for _, l := range strings.Split(got, "\n") {
		g[l] = true
	}
	var out []string
	for l := range w {
		if !g[l] {
			out = append(out, "- "+l)
		}
	}
	for l := range g {
		if !w[l] {
			out = append(out, "+ "+l)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// THE FOLD IS THE RECOMPUTE, WHEREVER IT IS CUT. The same day of generated
// traffic, folded in one window or in windows of seven requests, published by
// runs at many times of the day -- with a tail that holds most of the day,
// some of it, or none once the day is final -- always publishes exactly the
// hours, the day and the top lists the recompute writes from the whole day
// at once: visits carried across windows, bounces taken back, a source found
// after a visit's first page, visitors new to an hour, a value or the day.
func TestTheFoldAgreesWithARecomputeWhereverItIsCut(t *testing.T) {
	day := time.Date(2036, 11, 4, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		rows   int
		seed   int64
		everyN time.Duration
	}{
		{"one window, published once a day is final", 1 << 30, 1, 0},
		{"windows of seven, published through the day", 7, 2, 47 * time.Minute},
		{"windows of three, published every quarter hour", 3, 3, 15 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := rollupTestDB(t)
			saved := foldRows
			foldRows = c.rows
			t.Cleanup(func() { foldRows = saved })
			sites := []string{"site-rollup-ref-a-" + suffix(t), "site-rollup-ref-b-" + suffix(t), "site-rollup-ref-c-" + suffix(t)}
			seedRandomDay(t, db, randomDay(c.seed, day, sites))
			want := referenceSnapshot(t, db, sites, day)
			if !strings.Contains(want, "top ") || !strings.Contains(want, "daily") {
				t.Fatalf("the recompute wrote nothing worth comparing:\n%s", want)
			}

			var runs []time.Time
			if c.everyN > 0 {
				for at := day.Add(time.Hour); at.Before(day.AddDate(0, 0, 1).Add(2 * time.Hour)); at = at.Add(c.everyN) {
					runs = append(runs, at)
				}
			}
			runs = append(runs, day.AddDate(0, 0, 1).Add(3*time.Hour))
			for _, at := range runs {
				runRollup(t, testSession, at)
				if got := publishedSnapshot(t, db, sites, day); got != want {
					t.Fatalf("after the run at %s the published rows differ from the recompute's:\n%s", at.Format(time.RFC3339), diffLines(want, got))
				}
			}
			if _, ok := finalDays(t, db, day, day)[dateArg(day)]; !ok {
				t.Error("the day was not recorded final by the run after 02:00")
			}
		})
	}
}

// THE 24-HOUR READ IS THE REQUEST LOG'S, from the rollup's rows. Over two
// days of generated traffic, at many times of the second day, its visitors
// -- for the period, the one before it and each deployable -- and its top
// lists are what counting them from the request log, as the read did until
// 2026-11-09, answers. The days are folded in windows of five requests, so a
// visitor's last view of a page, and of the day, moves on from one fold to
// the next, across the hour a window starts at.
func TestTheTwentyFourHourReadAgreesWithTheRequestLog(t *testing.T) {
	db := rollupTestDB(t)
	saved := foldRows
	foldRows = 5
	t.Cleanup(func() { foldRows = saved })
	first := time.Date(2036, 11, 7, 0, 0, 0, 0, time.UTC)
	second := first.AddDate(0, 0, 1)
	sites := []string{"site-rollup-win-a-" + suffix(t), "site-rollup-win-b-" + suffix(t)}
	seedRandomDay(t, db, randomDay(11, first.AddDate(0, 0, -1), sites))
	seedRandomDay(t, db, randomDay(12, first, sites))
	seedRandomDay(t, db, randomDay(13, second, sites))

	var reads []time.Time
	for _, hm := range [][2]int{{0, 40}, {1, 55}, {3, 5}, {5, 20}, {7, 45}, {9, 30}, {11, 15}, {13, 50}, {15, 5}, {17, 40}, {19, 25}, {21, 10}, {23, 10}} {
		reads = append(reads, dayAt(second, hm[0], hm[1]))
	}
	for _, now := range reads {
		runRollup(t, testSession, now)
		got := readSites(t, db, sites, statsperiod.Last24Hours, now)
		b := statsperiod.For(statsperiod.Last24Hours, now)

		ctx := context.Background()
		var wantTotal, wantPrevious requestVisitors
		var wantTop []topRow
		if err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			if wantTotal, err = referenceRequestVisitors(ctx, tx, sites, b.Start, b.End); err != nil {
				return err
			}
			if wantPrevious, err = referenceRequestVisitors(ctx, tx, sites, b.PrevStart, b.PrevEnd); err != nil {
				return err
			}
			q, args := referenceRequestTopSQL(sites, b.Start, b.End)
			return tx.NewRaw(q, args...).Scan(ctx, &wantTop)
		}); err != nil {
			t.Fatal(err)
		}
		at := now.Format("15:04")
		if g, w := num(got.Totals.Visitors), num(wantTotal.total()); g != w {
			t.Errorf("at %s the window's visitors = %s, the request log's %s", at, g, w)
		}
		if g, w := num(got.Previous.Visitors), num(wantPrevious.total()); g != w {
			t.Errorf("at %s the previous window's visitors = %s, the request log's %s", at, g, w)
		}
		for _, s := range got.Sites {
			if g, w := num(s.Visitors), num(wantTotal[s.SiteId]); g != w {
				t.Errorf("at %s %s's visitors = %s, the request log's %s", at, s.SiteId, g, w)
			}
		}
		wantLists := topListsOf(wantTop)
		for _, d := range topDimensions {
			if g, w := topLine(got.Top[d.name]), topLine(wantLists[d.name]); g != w {
				t.Errorf("at %s the window's %s list:\n  got  %s\n  want %s", at, d.name, g, w)
			}
		}
	}
}
