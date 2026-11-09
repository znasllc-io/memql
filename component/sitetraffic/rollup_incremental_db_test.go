package sitetraffic

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	"github.com/znasllc-io/memql/core/statsperiod"
)

// rollup_incremental_db_test.go -- what makes the rollup incremental, and the
// reads that lean on it (rollup.go, "Incremental, with a two-hour tail";
// statistics.go, "The 24-hour period" and "Whole months").
//
// In the rollup tests' band (rollup_db_test.go), and the reads here call
// fillStatistics on the band's own sites directly: the statistics tests'
// band never runs the rollup (statistics_db_test.go says why), and which
// deployables a caller may read is theirs to test.

// readSites is one period of statistics for the given deployables, read as
// the siteStatistics read reads them, with no narrowing.
func readSites(t *testing.T, db *bun.DB, sites []string, p statsperiod.Period, now time.Time) Statistics {
	t.Helper()
	b := statsperiod.For(p, now)
	out := emptyStatistics(p, b)
	opts := &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	if err := db.RunInTx(context.Background(), opts, func(ctx context.Context, tx bun.Tx) error {
		return fillStatistics(ctx, tx, sites, b, &out)
	}); err != nil {
		t.Fatalf("read %s of %v: %v", p, sites, err)
	}
	return out
}

func deleteRequests(t *testing.T, db *bun.DB, site string, before time.Time) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), "DELETE FROM edge_request WHERE site_id = ? AND served_at < ?", site, before); err != nil {
		t.Fatal(err)
	}
}

// A REQUEST IS FOLDED ONCE. Once it is two hours old, a request's part of
// every figure is in its day's settled state, and no later run reads it
// again: here the requests the first run settled are deleted from the request
// log, and the next run's figures still count them. The last two hours are
// read again by every run, so a request written late, inside them, still
// counts.
func TestARequestIsFoldedOnceAndOnlyTheLastTwoHoursAreReadAgain(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	day := time.Date(2036, 10, 3, 0, 0, 0, 0, time.UTC)
	seedRequests(t, db, site,
		pageViewRow(dayAt(day, 8, 0), "fa", "/"),
		pageViewRow(dayAt(day, 8, 10), "fa", "/pricing"),
		pageViewRow(dayAt(day, 9, 0), "fb", "/"),
		pageViewRow(dayAt(day, 13, 30), "fc", "/about"),
	)
	runRollup(t, testSession, dayAt(day, 14, 0))
	wantFigures(t, "day", dailyOf(t, db, site), "2036-10-03", measuredFigures(3, 3, 2, 4))

	// The settled requests leave the log; a late one arrives inside the tail.
	deleteRequests(t, db, site, dayAt(day, 12, 0))
	seedRequests(t, db, site, pageViewRow(dayAt(day, 12, 30), "fd", "/"))
	runRollup(t, testSession, dayAt(day, 14, 5))

	daily := dailyOf(t, db, site)
	wantFigures(t, "day after the settled requests left the log", daily, "2036-10-03", measuredFigures(4, 4, 3, 5))
	hourly := hourlyOf(t, db, site)
	wantFigures(t, "a settled hour", hourly, "2036-10-03T08", measuredFigures(1, 1, 0, 2))
	wantFigures(t, "the late request's hour", hourly, "2036-10-03T12", measuredFigures(1, 1, 1, 1))
	top := topOf(t, db, site, day)
	if got := top["page|/"]; got.Visitors != 3 || got.PageViews != 3 {
		t.Errorf("page / = %+v, want the two settled visitors and the late one, 3 visitors and 3 page views", got)
	}
}

// THE 24-HOUR PERIOD READS THE ROLLUP, NOT THE REQUEST LOG. Its visitors are
// distinct over the window, the ones last seen in the first day's part of it
// and the ones first seen in the second's, and its top lists are the
// window's; both come from what the rollup writes, so they are the same once
// the request log no longer holds the window, and a read costs a few rows per
// deployable instead of a day of page views.
func TestTheTwentyFourHourReadDoesNotReadTheRequestLog(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	yesterday := time.Date(2036, 10, 6, 0, 0, 0, 0, time.UTC)
	today := yesterday.AddDate(0, 0, 1)
	seedRequests(t, db, site,
		// Before the window: the previous period's.
		pageViewRow(dayAt(yesterday, 9, 0), "wa", "/a"),
		pageViewRow(dayAt(yesterday, 20, 0), "wb", "/a"),
		pageViewRow(dayAt(yesterday, 20, 10), "wb", "/b"),
		pageViewRow(dayAt(yesterday, 23, 50), "wc", "/a"),
		pageViewRow(dayAt(today, 0, 20), "wc", "/c"),
		pageViewRow(dayAt(today, 8, 0), "wd", "/a"),
		pageViewRow(dayAt(today, 10, 15), "we", "/b"),
	)
	now := dayAt(today, 10, 30)
	runRollup(t, testSession, now)
	deleteRequests(t, db, site, today.AddDate(0, 0, 1))

	got := readSites(t, db, []string{site}, statsperiod.Last24Hours, now)
	// The window is yesterday 11:00 to today 11:00: wb and wc yesterday, wc
	// again today (a new day, a new visitor hash), wd and we.
	if num(got.Totals.Visitors) != "5" || num(got.Previous.Visitors) != "1" {
		t.Errorf("visitors = %s, previous %s; want 5 in the window and 1 before it", num(got.Totals.Visitors), num(got.Previous.Visitors))
	}
	if line := topLine(got.Top["page"]); line != "/a 3/3/3; /b 2/1/2; /c 1/1/1" {
		t.Errorf("the window's pages = %q, want /a 3/3/3; /b 2/1/2; /c 1/1/1", line)
	}
	if len(got.Sites) != 1 || num(got.Sites[0].Visitors) != "5" {
		t.Errorf("the deployable's own row = %s, want its 5 visitors", sitesLine(got.Sites))
	}
}

// WHOLE MONTHS ARE READ FROM THEIR MONTHLY TOP LISTS. A day becomes part of its
// month's top lists once it is final, and a read of a period that covers the
// month whole sums the month's rows, not each of its days': here the final
// days' own top rows are gone, and the twelve months still count them.
func TestWholeMonthsAreReadFromTheirMonthlyTopLists(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	seedRequests(t, db, site,
		pageViewRow(time.Date(2036, 8, 3, 10, 0, 0, 0, time.UTC), "ma", "/x"),
		pageViewRow(time.Date(2036, 8, 4, 10, 0, 0, 0, time.UTC), "mb", "/y"),
		pageViewRow(time.Date(2036, 8, 20, 9, 0, 0, 0, time.UTC), "mc", "/x"),
	)
	now := time.Date(2036, 8, 20, 12, 0, 0, 0, time.UTC)
	runRollup(t, testSession, now)
	if _, err := db.ExecContext(context.Background(), "DELETE FROM edge_stats_daily_top WHERE site_id = ? AND day < '2036-08-20'", site); err != nil {
		t.Fatal(err)
	}

	got := readSites(t, db, []string{site}, statsperiod.Last12Months, now)
	if line := topLine(got.Top["page"]); line != "/x 2/2/2; /y 1/1/1" {
		t.Errorf("twelve months of pages = %q, want /x 2/2/2; /y 1/1/1: the final days from their month, today from its day", line)
	}
}

// A DAY THE RECOMPUTE CLOSED IS FOLDED AGAIN ONLY FROM ITS REQUESTS. A final
// day of the last two whose daily row carries no visitors by hour -- one the
// recompute this rollup replaced wrote -- is folded once more, so the 24-hour
// window can count its visitors; but only while the request log holds the
// day. A day whose requests are gone would be folded to nothing and its
// figures wiped, so it keeps the figures it has.
func TestAClosedDayIsFoldedAgainOnlyFromItsRequests(t *testing.T) {
	db := rollupTestDB(t)
	site, logged := "site-rollup-"+suffix(t), "site-rollup-logged-"+suffix(t)
	today := time.Date(2036, 10, 20, 0, 0, 0, 0, time.UTC)
	gone, kept := today.AddDate(0, 0, -2), today.AddDate(0, 0, -1)
	ctx := context.Background()
	// Both days closed by the recompute: final, with daily rows and no hours.
	for _, d := range []time.Time{gone, kept} {
		if _, err := db.ExecContext(ctx, `INSERT INTO edge_stats_final (day, finalized_at) VALUES (?::date, ?)`, dateArg(d), d.AddDate(0, 0, 1).Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO edge_stats_daily
			(site_id, day, visitors, visits, bounces, page_views, requests, errors, not_found, bytes, bot_requests, duration_hist)
			VALUES (?, ?::date, 7, 8, 3, 20, 40, 0, 0, 4000, 0, ?)`, site, dateArg(d), pgdialect.Array(make([]int64, histogramBuckets))); err != nil {
			t.Fatal(err)
		}
	}
	// Only yesterday's requests are still in the log.
	seedRequests(t, db, logged, pageViewRow(dayAt(kept, 9, 0), "ra", "/"), pageViewRow(dayAt(kept, 15, 0), "rb", "/"))

	runRollup(t, testSession, dayAt(today, 6, 0))

	daily := dailyOf(t, db, site)
	wantFigures(t, "the day whose requests are gone", daily, dateArg(gone), measuredFigures(7, 8, 3, 20))
	if _, ok := daily[dateArg(kept)]; ok {
		t.Errorf("yesterday was folded again from its requests, and still holds the recompute's row for %s, which has none", site)
	}
	wantFigures(t, "yesterday, folded again", dailyOf(t, db, logged), dateArg(kept), measuredFigures(2, 2, 2, 2))
}
