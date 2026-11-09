package sitetraffic

import (
	"context"
	"database/sql"
	"strings"
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

// A CLOCK FINER THAN THE DATABASE'S. A run's clock carries nanoseconds and the
// database keeps microseconds, rounding to the nearest. The settled fold that
// reaches the horizon stores the horizon as the day's progress, and when the
// nanoseconds rounded down the stored progress stayed a fraction of a
// microsecond short of it: every pass read the day as not caught up, folded the
// same empty sliver again, and the run spun until its budget ran out without
// publishing the day (0.31.28 in production: today never published, the
// progress row rewritten thirty-five times a second). Here each run's clock is
// 234 ns past a microsecond, which rounds down, and each must come back
// promptly having published: yesterday, still open before 02:00 with its tail,
// today, and the 24-hour window.
func TestARunWhoseClockIsFinerThanTheDatabasesStillPublishes(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	yesterday := time.Date(2036, 10, 26, 0, 0, 0, 0, time.UTC)
	today := yesterday.AddDate(0, 0, 1)
	seedRequests(t, db, site,
		pageViewRow(dayAt(yesterday, 21, 0), "na", "/"),
		pageViewRow(dayAt(yesterday, 23, 30), "nb", "/late"),
		pageViewRow(dayAt(today, 0, 20), "nc", "/"),
		pageViewRow(dayAt(today, 9, 0), "nd", "/"),
	)
	for _, now := range []time.Time{
		dayAt(today, 1, 0).Add(234 * time.Nanosecond),  // yesterday open, with its tail
		dayAt(today, 14, 0).Add(234 * time.Nanosecond), // yesterday final, today with its tail
	} {
		at := now.Format(time.RFC3339Nano)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start := time.Now()
		_, err := Rollup(ctx, testSession, now)
		took := time.Since(start)
		cancel()
		if err != nil {
			t.Fatalf("the run at %s failed after %s: %v", at, took.Round(time.Millisecond), err)
		}
		if took > 15*time.Second {
			t.Fatalf("the run at %s took %s for four requests", at, took.Round(time.Millisecond))
		}
		daily := dailyOf(t, db, site)
		// A published day counts its requests through its midnight, the tail's
		// end, so today's 09:00 request counts at 01:00 as well.
		wantFigures(t, "yesterday after the run at "+at, daily, dateArg(yesterday), measuredFigures(2, 2, 2, 2))
		wantFigures(t, "today after the run at "+at, daily, dateArg(today), measuredFigures(2, 2, 2, 2))
		var windows int
		if err := db.NewRaw(`SELECT count(*) FROM edge_stats_window_top WHERE site_id = ? AND window_end = ?`,
			site, statsperiod.For(statsperiod.Last24Hours, now).End).Scan(context.Background(), &windows); err != nil {
			t.Fatal(err)
		}
		if windows == 0 {
			t.Errorf("the run at %s published no 24-hour top lists for the window", at)
		}
	}
}

// A FOLD WHOSE PROGRESS DOES NOT LAND STOPS THE DAY, AT ONCE. Whatever keeps a
// day's stored progress from reading back as the instant the fold moved it to
// -- a clock finer than the database's was one -- the run must not fold again
// from where it was, which spins and, over a range with requests, counts them
// twice. Each settled pass reads the progress back in its own transaction and,
// when it differs, rolls the pass back and fails the day by name, so the run
// moves on promptly and nothing is folded. Here a trigger moves the stored
// progress a microsecond back.
func TestAFoldWhoseProgressDoesNotLandFailsTheDayPromptly(t *testing.T) {
	db := rollupTestDB(t)
	site := "site-rollup-" + suffix(t)
	day := time.Date(2036, 10, 29, 0, 0, 0, 0, time.UTC)
	seedRequests(t, db, site, pageViewRow(dayAt(day, 9, 0), "pa", "/"))
	ctx := context.Background()
	const shift = "rollup_test_shift_progress"
	drop := func() {
		_, _ = db.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+shift+" ON edge_stats_progress")
		_, _ = db.ExecContext(ctx, "DROP FUNCTION IF EXISTS "+shift+"()")
	}
	drop()
	t.Cleanup(drop)
	for _, stmt := range []string{
		"CREATE FUNCTION " + shift + "() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN " +
			"IF NEW.day = '" + dateArg(day) + "' THEN NEW.settled_through := NEW.settled_through - interval '1 microsecond'; END IF; " +
			"RETURN NEW; END $$",
		"CREATE TRIGGER " + shift + " BEFORE INSERT OR UPDATE ON edge_stats_progress FOR EACH ROW EXECUTE FUNCTION " + shift + "()",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	run, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := Rollup(run, testSession, dayAt(day, 14, 0))
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the run took %s: it spun on the day instead of failing it", took.Round(time.Millisecond))
	}
	if err == nil || !strings.Contains(err.Error(), dateArg(day)) || !strings.Contains(err.Error(), "progress") {
		t.Fatalf("the run answered %v, want the day failed by name for its progress", err)
	}
	var settled int
	if err := db.NewRaw(`SELECT count(*) FROM edge_stats_settled_hour WHERE day = ?::date`, dateArg(day)).Scan(ctx, &settled); err != nil {
		t.Fatal(err)
	}
	if settled != 0 {
		t.Errorf("%d settled hours were kept for a pass whose progress did not land, want the pass rolled back", settled)
	}
}
