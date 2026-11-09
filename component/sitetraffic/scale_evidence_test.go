package sitetraffic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/core/statsperiod"
)

// scale_evidence_test.go -- opt-in evidence of what the website statistics
// cost at the sizes the 2026-11-09 scale review named, in the way of
// component/emailstats/statistics_scale_test.go:
//
//   - TestRollupAtScaleEvidence writes one UTC day of ten million requests
//     over fifty sites -- three in ten page views, in visits of three, the
//     rest assets, a twentieth of those bots -- and times the rollup's runs
//     through that day and the next morning: the first, which folds the day
//     so far, then the five-minutely runs, the one that closes the day, and
//     the 24-hour read over it.
//   - TestStaffReadsAtScaleEvidence writes 400 days of daily rows for two
//     hundred sites, with a hundred values a day in each of the six top lists
//     and an (other) row (page and source values each site's own), marks the
//     days final, folds the months, and times a staff read of every period
//     under the request pool's 8 s statement timeout.
//
// Both write millions of rows and delete them afterwards, in a band of dates
// (2039) no other test writes, and run the rollup, whose retention deletes
// every site's rows older than its cutoff across the shared tables: they
// belong on a disposable database, sized like the cluster the figures are for
// (the 2026-11-09 measurements: the CI TimescaleDB image with --cpus=2
// --memory=8g, shared_buffers 2GB, effective_cache_size 6GB, work_mem 8MB,
// the mid preset):
//
//	MEMQL_SITETRAFFIC_SCALE_EVIDENCE_FILE=/tmp/site-scale.json \
//	  MEMQL_REQUIRE_DB=1 MEMQL_DATABASE_DSN=... \
//	  go test -count=1 -p=1 -timeout 90m github.com/znasllc-io/memql/component/sitetraffic/ \
//	  -run 'TestRollupAtScaleEvidence|TestStaffReadsAtScaleEvidence'
//
// MEMQL_SITETRAFFIC_SCALE_REQUESTS and MEMQL_SITETRAFFIC_SCALE_SITES change
// the rollup's day and the reads' number of sites.

var scaleDay = time.Date(2039, 3, 10, 0, 0, 0, 0, time.UTC)

// evidenceWriter is a handle for the evidence's own writes, whose statements
// run for minutes: pgdriver's default 10 s read deadline would end them at
// the client while the server carried on, committing rows after the test had
// cleared its band.
func evidenceWriter(t *testing.T) *bun.DB {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithReadTimeout(time.Hour), pgdriver.WithWriteTimeout(time.Hour))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		dbtest.Unreachable(t, "the website statistics evidence", dbtest.DSN(), err)
		return nil
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func scaleEvidence(t *testing.T) string {
	t.Helper()
	output := os.Getenv("MEMQL_SITETRAFFIC_SCALE_EVIDENCE_FILE")
	if output == "" {
		t.Skip("opt-in: set MEMQL_SITETRAFFIC_SCALE_EVIDENCE_FILE against a disposable database")
	}
	return output
}

func scaleInt(name string, fallback int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return fallback
}

// writeEvidence adds what one test measured to the evidence file, under its
// name.
func writeEvidence(t *testing.T, output, name string, measured any) {
	t.Helper()
	all := map[string]any{}
	if raw, err := os.ReadFile(output); err == nil {
		_ = json.Unmarshal(raw, &all)
	}
	all[name] = measured
	raw, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", raw)
}

// clearScaleBand removes everything the evidence wrote.
func clearScaleBand(t *testing.T, db *bun.DB) {
	t.Helper()
	ctx := context.Background()
	from, to := scaleDay.AddDate(-2, 0, 0), scaleDay.AddDate(0, 0, 3)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"DELETE FROM edge_request WHERE served_at >= ? AND served_at < ?", []any{from, to}},
		{"DELETE FROM edge_stats_hourly WHERE hour >= ? AND hour < ?", []any{from, to}},
		{"DELETE FROM edge_stats_window_top WHERE window_end >= ? AND window_end < ?", []any{from, to}},
	} {
		if _, err := db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("clear the evidence (%s): %v", q.sql, err)
		}
	}
	for _, table := range []string{"edge_stats_daily", "edge_stats_daily_top", "edge_stats_final", "edge_stats_progress",
		"edge_stats_settled_hour", "edge_stats_visitor", "edge_stats_value_hour", "edge_stats_month_day"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+table+" WHERE day >= ?::date AND day < ?::date", dateArg(from), dateArg(to)); err != nil {
			t.Fatalf("clear the evidence from %s: %v", table, err)
		}
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM edge_stats_monthly_top WHERE month >= ?::date AND month < ?::date", dateArg(from), dateArg(to)); err != nil {
		t.Fatal(err)
	}
}

// scaleRequestsSQL writes n requests of one UTC day over sites sites: three in
// ten page views, numbered in visits of three page views 45 s apart, each
// visit on one site; the rest assets spread evenly over the day, a twentieth
// of them bots.
const scaleRequestsSQL = `
INSERT INTO edge_request (served_at, id, site_id, node, status, path_class, bytes, duration_ns,
                          measured, page_view, bot, path, source, country, device, browser, os, visitor)
SELECT CASE WHEN is_pv THEN ?::timestamptz + make_interval(secs => ((v * 86399.0) / visits_n) + (pv % 3) * 45)
            ELSE ?::timestamptz + make_interval(secs => (i * 86399.0) / ?) END,
       'scale-r' || i, 'scale-site-' || site, 'edge-0',
       CASE WHEN h % 1000 < 970 THEN 200 WHEN h % 1000 < 990 THEN 404 WHEN h % 1000 < 995 THEN 304 ELSE 500 END,
       CASE WHEN is_pv THEN 'document' ELSE 'asset' END,
       (h % 200000)::bigint, (500000 + (h % 150000000))::bigint, true, is_pv, (NOT is_pv AND h % 100 < 5),
       CASE WHEN is_pv THEN '/p/' || ((h2 % 2000) * (h2 % 2000) / 2000) ELSE '' END,
       CASE WHEN is_pv AND pv % 3 = 0 AND h % 10 < 4 THEN 'src' || (h2 % 300) ELSE '' END,
       CASE WHEN NOT is_pv AND h % 100 < 5 THEN '' ELSE 'C' || (v % 120) END,
       CASE WHEN NOT is_pv AND h % 100 < 5 THEN '' ELSE (ARRAY['desktop','mobile','tablet'])[1 + v % 3] END,
       CASE WHEN NOT is_pv AND h % 100 < 5 THEN '' ELSE (ARRAY['Chrome','Safari','Firefox','Edge','Opera','Samsung Internet','Other'])[1 + v % 7] END,
       CASE WHEN NOT is_pv AND h % 100 < 5 THEN '' ELSE (ARRAY['Windows','macOS','iOS','iPadOS','Android','Linux','ChromeOS','Other'])[1 + v % 8] END,
       CASE WHEN is_pv THEN md5('visitor' || ((v * 7919) % (visits_n * 10 / 13))) ELSE '' END
  FROM (SELECT i, is_pv, pv, pv / 3 AS v, (?::bigint * 3 / 10) / 3 AS visits_n,
               ((i * 2654435761) % 4294967296) AS h,
               ((i * 40503 + 12345) % 65536) AS h2,
               floor(? * power((((CASE WHEN is_pv THEN i / 10 ELSE i END * 2246822519) % 4294967296) / 4294967296.0), 2))::int AS site
          FROM (SELECT i, (i % 10 < 3) AS is_pv, (i / 10) * 3 + (i % 10) AS pv
                  FROM generate_series(1::bigint, ?::bigint) AS i) g) s`

type runTiming struct {
	At     string       `json:"at"`
	Took   string       `json:"took"`
	Report RollupReport `json:"report"`
}

// TestRollupAtScaleEvidence: see the file comment.
func TestRollupAtScaleEvidence(t *testing.T) {
	output := scaleEvidence(t)
	db := evidenceWriter(t)
	ctx := context.Background()
	clearScaleBand(t, db)
	t.Cleanup(func() { clearScaleBand(t, db) })
	n, sites := scaleInt("MEMQL_SITETRAFFIC_SCALE_REQUESTS", 10_000_000), scaleInt("MEMQL_SITETRAFFIC_SCALE_SITES", 50)

	began := time.Now()
	if _, err := db.ExecContext(ctx, queryGen.FormatQuery(scaleRequestsSQL, scaleDay, scaleDay, n, n, sites, n)); err != nil {
		t.Fatalf("write the day's requests: %v", err)
	}
	if _, err := db.ExecContext(ctx, "ANALYZE edge_request"); err != nil {
		t.Fatal(err)
	}
	written := time.Since(began)

	var runs []runTiming
	for _, at := range []time.Time{
		dayAt(scaleDay, 23, 50), dayAt(scaleDay, 23, 55), // the first run, which folds the day so far; then one five minutes on
		dayAt(scaleDay, 23, 58), dayAt(scaleDay.AddDate(0, 0, 1), 0, 3), // the last of the day; the first of the next
		dayAt(scaleDay.AddDate(0, 0, 1), 1, 0), dayAt(scaleDay.AddDate(0, 0, 1), 2, 5), // with yesterday open; the one that closes it
		dayAt(scaleDay.AddDate(0, 0, 1), 2, 10), // and after
	} {
		start := time.Now()
		rep, err := Rollup(ctx, testSession, at)
		if err != nil {
			t.Fatalf("the run at %s: %v", at.Format(time.RFC3339), err)
		}
		runs = append(runs, runTiming{At: at.Format(time.RFC3339), Took: time.Since(start).String(), Report: rep})
	}
	if _, ok := finalDays(t, db, scaleDay, scaleDay)[dateArg(scaleDay)]; !ok {
		t.Fatal("the day was not closed by the run at 02:05")
	}

	var names []string
	for i := 0; i < sites; i++ {
		names = append(names, fmt.Sprintf("scale-site-%d", i))
	}
	pool := poolHandleForEvidence(t)
	reads := map[string]string{}
	for _, at := range []time.Time{dayAt(scaleDay, 23, 59), dayAt(scaleDay.AddDate(0, 0, 1), 2, 11)} {
		start := time.Now()
		got := readSites(t, pool, names, statsperiod.Last24Hours, at)
		reads[at.Format(time.RFC3339)] = time.Since(start).String()
		if got.Totals.Visitors == nil || *got.Totals.Visitors == 0 {
			t.Fatalf("the 24-hour read at %s counted no visitors", at.Format(time.RFC3339))
		}
	}
	writeEvidence(t, output, "rollup", map[string]any{
		"requests": n, "sites": sites, "day": dateArg(scaleDay), "writtenIn": written.String(),
		"runs": runs, "twentyFourHourReads": reads, "statementTimeout": "8s for the reads (the request pool's); 60s per rollup statement",
	})
}

// poolHandleForEvidence is a handle with the request pool's session limits:
// the server's 8 s statement timeout, under pgdriver's own 10 s read deadline.
func poolHandleForEvidence(t *testing.T) *bun.DB {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"statement_timeout": "8000"}),
	)), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestStaffReadsAtScaleEvidence: see the file comment.
func TestStaffReadsAtScaleEvidence(t *testing.T) {
	output := scaleEvidence(t)
	db := evidenceWriter(t)
	ctx := context.Background()
	clearScaleBand(t, db)
	t.Cleanup(func() { clearScaleBand(t, db) })
	sites := scaleInt("MEMQL_SITETRAFFIC_SCALE_SITES", 200)
	const days = 400
	last := scaleDay.AddDate(0, 0, -1)

	began := time.Now()
	for _, q := range []string{
		`INSERT INTO edge_stats_daily (site_id, day, visitors, visits, bounces, page_views, requests, errors, not_found, bytes, bot_requests, duration_hist)
		 SELECT 'read-site-' || s, ?::date - d, 1000 + (s * 7 + d) % 500, 1300 + (s * 7 + d) % 600, 500 + (s + d) % 200, 3900 + (s * 3 + d) % 900,
		        13000 + (s * 11 + d) % 3000, (s + d) % 20, (s * 3 + d) % 60, 100000000 + s * d, 600 + (s + d) % 100,
		        array_fill(((s + d) % 50)::bigint, ARRAY[64])
		   FROM generate_series(0, ? - 1) s, generate_series(0, ? - 1) d`,
		`INSERT INTO edge_stats_daily_top (site_id, day, dimension, value, visitors, visits, page_views)
		 SELECT 'read-site-' || s, ?::date - d, dim,
		        CASE WHEN j = 100 THEN '(other)'
		             WHEN dim = 'page' THEN '/s' || s || '/p' || ((d * 37 + j * 7) % 1000)
		             WHEN dim = 'source' THEN 's' || s || '-src' || ((d * 11 + j * 3) % 300)
		             ELSE dim || '-' || ((d * 37 + j * 7 + s * 13) % 300) END,
		        1000 - j * 9, 900 - j * 8, 3000 - j * 25
		   FROM generate_series(0, ? - 1) s, generate_series(0, ? - 1) d,
		        unnest(ARRAY['page','source','country','device','browser','os']) dim, generate_series(0, 100) j
		 ON CONFLICT DO NOTHING`,
	} {
		if _, err := db.ExecContext(ctx, queryGen.FormatQuery(q, dateArg(last), sites, days)); err != nil {
			t.Fatalf("write the evidence's rows: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO edge_stats_final (day, finalized_at)
		SELECT d::date, ? FROM generate_series(?::date - 399, ?::date, interval '1 day') d ON CONFLICT DO NOTHING`,
		scaleDay, dateArg(last), dateArg(last)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "ANALYZE edge_stats_daily; ANALYZE edge_stats_daily_top"); err != nil {
		t.Fatal(err)
	}
	written := time.Since(began)
	began = time.Now()
	s := &rollupSession{open: testSession}
	defer s.release()
	if err := foldMonths(ctx, s, dayAt(scaleDay, 18, 0)); err != nil {
		t.Fatalf("fold the months: %v", err)
	}
	folded := time.Since(began)

	var names []string
	for i := 0; i < sites; i++ {
		names = append(names, fmt.Sprintf("read-site-%d", i))
	}
	pool := poolHandleForEvidence(t)
	type timing struct {
		Period   string    `json:"period"`
		RunsMs   []float64 `json:"runsMs"`
		MedianMs float64   `json:"medianMs"`
	}
	var timings []timing
	for _, p := range []statsperiod.Period{statsperiod.Last7Days, statsperiod.Last30Days, statsperiod.Last90Days, statsperiod.Last12Months} {
		tm := timing{Period: string(p)}
		for run := 0; run < 3; run++ {
			start := time.Now()
			got := readSites(t, pool, names, p, dayAt(last, 18, 0))
			tm.RunsMs = append(tm.RunsMs, float64(time.Since(start).Microseconds())/1000)
			if len(got.Top["page"]) == 0 {
				t.Fatalf("%s read no pages", p)
			}
		}
		sorted := append([]float64{}, tm.RunsMs...)
		sort.Float64s(sorted)
		tm.MedianMs = sorted[len(sorted)/2]
		timings = append(timings, tm)
	}
	writeEvidence(t, output, "reads", map[string]any{
		"sites": sites, "days": days, "topRowsPerSiteAndDay": 606, "writtenIn": written.String(), "monthsFoldedIn": folded.String(),
		"statementTimeout": "8s (the request pool's)", "staffReads": timings,
	})
}
