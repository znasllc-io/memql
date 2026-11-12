package sitetraffic

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/accountexport"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// account_handover_db_test.go -- the website statistics hand-over against
// the real tables.
//
// Every table the hand-over could read is shadowed by a TEMP table on one
// connection, so the fixture is the whole world: nothing another test or
// session keeps in the shared tables can reach the output, and nothing here
// is left behind. The rollup and the reader are not run; the rows are written
// as the rollup would have written them.
//
// Postgres-gated: CI's db-tests lane runs this package with MEMQL_REQUIRE_DB=1.

// statisticsTables are every table of the statistics, the published ones and
// the ones a file must never carry, so a read of the wrong table finds rows.
var statisticsTables = []string{
	"edge_request", "edge_stats_visitor", "edge_visitor_secret", "edge_stats_progress",
	"edge_stats_settled_hour", "edge_stats_value_hour", "edge_stats_final", "edge_stats_month_day",
	"edge_stats_hourly", "edge_stats_daily", "edge_stats_daily_top", "edge_stats_window_top", "edge_stats_monthly_top",
}

func handOverDB(t *testing.T) *bun.DB {
	t.Helper()
	ctx := context.Background()
	reachable, err := dbtest.EnsureSchema(ctx)
	if err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if !reachable {
		dbtest.Unreachable(t, "the website statistics hand-over", dbtest.DSN(), sql.ErrConnDone)
		return nil
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, table := range statisticsTables {
		if _, err := db.ExecContext(ctx, `CREATE TEMP TABLE `+table+` (LIKE public.`+table+` INCLUDING ALL)`); err != nil {
			t.Fatalf("shadow %s: %v", table, err)
		}
	}
	return db
}

func seedStatistics(t *testing.T, db *bun.DB, site string) {
	t.Helper()
	ctx := context.Background()
	hist := "'{" + strings.TrimSuffix(strings.Repeat("0,", 64), ",") + "}'::bigint[]"
	for _, q := range []string{
		`INSERT INTO edge_stats_hourly VALUES ('` + site + `', '2040-01-02T05:00:00Z', 3, 4, 1, 9, 12, 0, 1, 2048, 2, ` + hist + `)`,
		`INSERT INTO edge_stats_hourly VALUES ('` + site + `', '2040-01-02T04:00:00Z', NULL, NULL, NULL, NULL, 5, 1, 0, 100, 0, ` + hist + `)`,
		`INSERT INTO edge_stats_daily (site_id, day, visitors, visits, bounces, page_views, requests, errors, not_found, bytes, bot_requests, duration_hist)
		   VALUES ('` + site + `', '2040-01-02', 3, 4, 1, 9, 17, 1, 1, 2148, 2, ` + hist + `)`,
		`INSERT INTO edge_stats_daily_top VALUES ('` + site + `', '2040-01-02', 'page', '/about', 2, 2, 3)`,
		`INSERT INTO edge_stats_daily_top VALUES ('` + site + `', '2040-01-02', 'page', '/', 3, 4, 6)`,
		`INSERT INTO edge_stats_window_top VALUES ('` + site + `', '2040-01-03T00:00:00Z', 'country', 'NL', 1, 1, 1)`,
		`INSERT INTO edge_stats_monthly_top VALUES ('` + site + `', '2039-12-01', 'source', 'example.org', 7, 8, 9)`,
		`INSERT INTO edge_stats_settled_hour VALUES ('` + site + `', '2040-01-03', 1, true, 1, 1, 1, 1, 0, 1, 1, 0, 0, 10, 0, ` + hist + `)`,
		`INSERT INTO edge_stats_value_hour VALUES ('` + site + `', '2040-01-03', 'page', '/', 1, 1, 1, 1, 1)`,
		`INSERT INTO edge_stats_visitor VALUES ('` + site + `', '2040-01-03', 'visitor-hash', 2, now(), now(), 1, false, '{}', '{}')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed %s: %v\n%s", site, err, q)
		}
	}
}

// fakeSink records what a hand-over writes.
type fakeSink struct {
	dirs  []string
	parts map[string][]map[string]any
	lines map[string][]map[string]any
}

func newFakeSink() *fakeSink {
	return &fakeSink{parts: map[string][]map[string]any{}, lines: map[string][]map[string]any{}}
}

type fakePart struct {
	s   *fakeSink
	dir string
}

func (p fakePart) Write(v any) error {
	m, err := asMap(v)
	if err != nil {
		return err
	}
	p.s.parts[p.dir] = append(p.s.parts[p.dir], m)
	return nil
}

func (fakePart) Close() error { return nil }

func (s *fakeSink) Parted(dir string) accountexport.LineSink {
	s.dirs = append(s.dirs, dir)
	return fakePart{s, dir}
}

func (s *fakeSink) Line(member string, v any) error {
	m, err := asMap(v)
	if err != nil {
		return err
	}
	s.lines[member] = append(s.lines[member], m)
	return nil
}

func asMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}

func runSiteStatistics(t *testing.T, db *bun.DB, sites []string) *fakeSink {
	t.Helper()
	ctx := context.Background()
	tx, err := db.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	out := newFakeSink()
	in := accountexport.HandOverInput{Account: "acc-one", RowIDs: func(concept string) []string {
		if concept == "v1:platform:site" {
			return sites
		}
		return nil
	}}
	if err := accountexport.RunHandOver(ctx, "site-statistics", tx, in, out); err != nil {
		t.Fatalf("site-statistics: %v", err)
	}
	return out
}

func TestSiteStatisticsHandOverCarriesTheAccountsSitesOnly(t *testing.T) {
	db := handOverDB(t)
	if db == nil {
		return
	}
	seedStatistics(t, db, "site-one")
	seedStatistics(t, db, "site-two")
	seedStatistics(t, db, "site-other")

	out := runSiteStatistics(t, db, []string{"site-two", "site-one"})

	wantDirs := []string{
		"site-statistics/edge_stats_daily",
		"site-statistics/edge_stats_daily_top",
		"site-statistics/edge_stats_hourly",
		"site-statistics/edge_stats_monthly_top",
		"site-statistics/edge_stats_window_top",
	}
	if strings.Join(out.dirs, ",") != strings.Join(wantDirs, ",") {
		t.Fatalf("folders = %v, want %v", out.dirs, wantDirs)
	}
	for dir, lines := range out.parts {
		for _, l := range lines {
			if l["site_id"] != "site-one" && l["site_id"] != "site-two" {
				t.Fatalf("%s carries %v, a site of another account", dir, l["site_id"])
			}
		}
	}
	counts := map[string]int{}
	for dir, lines := range out.parts {
		counts[dir] = len(lines)
	}
	want := map[string]int{
		"site-statistics/edge_stats_hourly": 4, "site-statistics/edge_stats_daily": 2,
		"site-statistics/edge_stats_daily_top": 4, "site-statistics/edge_stats_window_top": 2,
		"site-statistics/edge_stats_monthly_top": 2,
	}
	for dir, n := range want {
		if counts[dir] != n {
			t.Fatalf("%s has %d lines, want %d (all: %v)", dir, counts[dir], n, counts)
		}
	}

	// The lines are the table's own columns, ordered by its key.
	hourly := out.parts["site-statistics/edge_stats_hourly"]
	var order []string
	for _, l := range hourly {
		order = append(order, l["site_id"].(string)+" "+l["hour"].(string))
	}
	if !sort.StringsAreSorted(order) || order[0] != "site-one 2040-01-02T04:00:00.000000Z" {
		t.Fatalf("hourly lines out of key order: %v", order)
	}
	first := hourly[0]
	if first["visitors"] != nil || first["requests"] != float64(5) {
		t.Fatalf("an unmeasured hour must keep NULL visitors and its counters: %v", first)
	}
	if hist, ok := first["duration_hist"].([]any); !ok || len(hist) != 64 {
		t.Fatalf("duration_hist = %v", first["duration_hist"])
	}
	top := out.parts["site-statistics/edge_stats_daily_top"]
	if top[0]["value"] != "/" || top[1]["value"] != "/about" || top[0]["dimension"] != "page" {
		t.Fatalf("daily top lines: %v", top)
	}

	// Two runs give the same output.
	again := runSiteStatistics(t, db, []string{"site-one", "site-two"})
	a, _ := json.Marshal(out.parts)
	b, _ := json.Marshal(again.parts)
	if string(a) != string(b) {
		t.Fatal("two runs over one snapshot wrote different lines")
	}
}

func TestSiteStatisticsHandOverWithNoSitesWritesNothing(t *testing.T) {
	db := handOverDB(t)
	if db == nil {
		return
	}
	seedStatistics(t, db, "site-other")
	out := runSiteStatistics(t, db, nil)
	if len(out.dirs) != 0 || len(out.lines) != 0 {
		t.Fatalf("an account with no sites wrote %v %v", out.dirs, out.lines)
	}
}
