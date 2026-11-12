package storestats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// walk.go -- how a store's changed orders, lines and refunds are read: from
// the server-only reads, in bounded windows, from a floor found for each
// store. It reads and hands each page on; what is made of a page is the
// caller's (the fold).
//
// # Where a store's history begins
//
// No date is compiled in. No mirror row of a store is older than its store
// row, so the walk begins at an instant before the store row was first
// written: its floor. Nothing on a row says when that was -- row.createdAt is
// the write time of its newest version, and a store row is rewritten at least
// daily -- so floorOf asks for the store row as of the clock less 1, 2, 4, 8
// ... days, floorProbes times at most, and answers the first instant at which
// there was no row; when one answers at every probe, the last instant asked.
//
// THE CLOCK IS ASKED FIRST. An unknown or mistyped id, a caller the read does
// not admit and a row that is gone all answer no row, as a store not written
// yet does; taken for "not written yet", that would give a floor of a day
// ago, kept for good, and every older row hidden. So no store row at the clock
// is the source's error, nothing is kept, and the next run asks again: only a
// store that exists now gets a floor.
// That is at most twice the store's age too early, which costs a few reads of
// empty windows and loses nothing. ensureFloor does it once and keeps it in
// history_floor, which nothing overwrites, a rebuild included.
//
// THE FLOOR RELIES ON EVERY VERSION OF THE STORE ROW BEING KEPT. With old
// versions pruned the probe would find the oldest one kept, the floor would
// move forward, and the rows written before it would never be walked.
// v1:shopify:store declares no retention, and
// TestTheStoreConceptKeepsEveryVersion fails when it does. It relies, too, on a
// store's row not being deleted while its mirror rows remain (an uninstall
// marks the store disconnected and keeps the row).
//
// # Windows
//
// A read costs every version of the concept written since `since`, on the
// whole cluster (component/memql, latest_scan_keys.go), and a first fill, a
// rebuild and the check all start at the floor. Without a bound a large
// mirror's first page outlasts the statement timeout on every run, as the email
// walk's did before it was bounded. So a source is read in windows. Where the
// walk stands is Through (the floor when it has not been walked) and a Width;
// a window reads from `since`, Through less watermarkOverlap, to `until`,
// Through plus Width:
//
//   - When until is within the overlap of the clock, the read is to the clock.
//     Its pages move Through to the newest createdAt read, never back and never
//     past the clock, and its last page is caught up at the clock that page
//     was asked by. That page also moves Through to at least that clock less
//     the overlap, and Width back to defaultWindow: every row written before
//     then that was visible was read, and one that was not yet visible was
//     written within the overlap of it (the bound below). So a quiet store's
//     next run reads twice the overlap and the time since, not the weeks since
//     its newest row. The walk ends there.
//   - Otherwise the window is past, and read as of its end (asOf, the form the
//     email walk's walkQuery writes), so the engine collapses only the versions
//     written up to it. Its last page sets Through to until and doubles Width
//     up to maxWindow, and the walk goes on to the next window. A window that
//     held no row is handed on too, as a page with no rows, so the caller
//     keeps where the walk stands.
//
// until is measured from Through, not from since: since plus Width would stop
// moving once Width is at or below the overlap, and minWindow is below it.
//
// # What the reads guarantee, and what the walk does about it
//
// A page is ordered by write time, ties broken by id; within one read of
// unchanged rows nothing is skipped or repeated. A row rewritten while the
// walk reads comes back on a later page: the walk hands it on again, and the
// fold dedupes by id and version. `since` is inclusive, so a run that starts
// at the newest row it read reads that row again, and rows written at the same
// instant on the far side of a page boundary are read rather than lost. A
// version's write time is set before its transaction commits, so a row can
// become visible behind rows already read: every window starts the overlap
// behind where the walk stands, and that overlap is the room a late commit
// has.
//
// # What the overlap rests on
//
// Nothing in the engine bounds how late a version becomes visible after its
// write time is taken, so watermarkOverlap is an assumption, stated here:
//
//   - row.createdAt is the writer's clock, taken in Concept.Create
//     (component/database/memory-nodes/concept.go) before the INSERT is sent;
//   - the statement then runs under the request pool's statement timeout, 8 s
//     by default (component/database/database.go, defaultStatementTimeoutMs,
//     MEMQL_DB_STATEMENT_TIMEOUT_MS);
//   - on the transactional write path (component/memql/outbox_append.go,
//     runInWriteTx) a session can sit idle between statements up to the
//     idle-in-transaction bound, 60 s by default (database.go,
//     defaultIdleInTxTimeoutMs; 15 s when fenced);
//   - and, with no bound the engine sets, the wait for a pool connection under
//     the caller's context, a pause of the writer's process, and the skew
//     between the writer's clock and the reader's.
//
// The overlap is five minutes because the bounded parts sum to 68 s, under
// half of it: TestTheOverlapHoldsTheEnginesOwnBoundsOnALateCommit reads both
// defaults from the engine's code and fails when they reach half the overlap.
// An installation that switches the statement timeout off (0) configures a
// lateness the walk cannot bound, as do the unbounded parts. What a walk
// misses by more than the overlap is found by the periodic check (a later task
// of the store statistics design), which re-reads a store's rows from its
// floor.
//
// # Bounded work
//
// Every read counts against maxReadsPerRun; a page counts against the caller's
// budget when it holds a row written after where the walk stood when the call
// began, so a page of rows the walk has read before -- the overlap, re-read --
// is not paid for twice and a run always gets past it. Both are checked before
// every page, never only between windows. A run that reaches either stops
// behind, and next says where: the next run carries on from there. A cursor the
// engine hands back unchanged is an error, never a loop.
//
// A page that outlasts a read halves Width, down to minWindow, and ends the
// source for the run with no error: the halved width is in next and the caller
// keeps it. At minWindow it is the source's error. A read to the clock that
// times out sets Width to half the span it tried, or half Width when that is
// narrower, so the next run reads a past window that far; one that times out
// at minWindow is the source's error too. So consecutive runs that time out
// end in an error within clockTimeoutRuns, never in silence.
//
// MORE VERSIONS INSIDE ONE OVERLAP THAN A RUN MAY READ. A run that reads
// maxReadsPerRun pages without moving past where it began has read nothing but
// rows at or behind it: more than maxReadsPerRun x walkPageSize versions of
// the store within the overlap. Every run would read the same pages and stop
// at the same place, so it is errOverlapTooDense, which says so and the
// limit, and is not a transient failure.
//
// # Errors
//
// An error stops the source for the run, with next where the last complete
// page left it, and says which store and which read. A page's rows are never
// in it: two of the shapes the walk reads carry a buyer's email.
//
// Every read is made under walkContext (rollup.go): internal origin, fresh and
// roots-only. The statements are written out by name in rollup.go, beside the
// stamp, so test/dslconformance sees each call and its stamp in one file. Never as a journaled automation step, for the same reason as the
// errors: the work journal keeps what a step answers, rows included.

const (
	sourceOrder  = "order"
	sourceLine   = "line"
	sourceRefund = "refund"

	watermarkOverlap = 5 * time.Minute
	walkPageSize     = 250
	maxPagesPerRun   = 40
	maxReadsPerRun   = 400
	defaultWindow    = 24 * time.Hour
	maxWindow        = 32 * 24 * time.Hour
	minWindow        = time.Minute
	floorProbes      = 14 // the clock less 1, 2, 4, ... 8192 days, after the clock itself

	// clockTimeoutRuns bounds the consecutive runs in which a store's reads
	// time out before the walk answers an error: the halvings from maxWindow
	// to minWindow (32 days is 46,080 minutes, under 2^16), and the run at
	// minWindow that is the error.
	clockTimeoutRuns = 17
)

// floorForTests is zero outside tests. When a test sets it, it is every store's floor and no store row is asked for.
var floorForTests time.Time

type walkWindow struct {
	Through time.Time // rows written up to here are read; zero is not walked yet
	Width   time.Duration
}

type walkPage struct {
	Rows     []map[string]any // empty for a window that held no row
	Through  time.Time        // where the walk stands once this page is written
	Width    time.Duration    // the width to keep with it
	CaughtUp time.Time        // not zero on the last page of a read to the clock: the clock that page was asked by
}

// pastRead reads a past window as of its end: the form the email walk's
// walkQuery writes. The statements it wraps are in rollup.go (changedReads).
const pastRead = "asOf(%s, %s)"

// quoteInstant is an instant as a query argument.
func quoteInstant(t time.Time) string {
	return langparser.QuoteString(t.UTC().Format(time.RFC3339Nano))
}

// errNoStore refuses a blank store id before anything is asked: a blank
// member also matches rows with no store id.
var errNoStore = errors.New("no store id: a blank one would also match rows with no store")

// errOverlapTooDense: see "More versions inside one overlap than a run may
// read".
var errOverlapTooDense = errors.New("more versions were written inside one overlap than a run may read")

// floorOf answers an instant at which the store's row did not exist yet.
func floorOf(ctx context.Context, engine Engine, storeID string, now time.Time) (time.Time, error) {
	if strings.TrimSpace(storeID) == "" {
		return time.Time{}, fmt.Errorf("find a store's history floor: %w", errNoStore)
	}
	if engine == nil {
		return time.Time{}, errors.New("find a store's history floor: this node has no engine handle")
	}
	ctx = walkContext(ctx)
	exists := func(at time.Time) (bool, error) {
		res, err := engine.Execute(ctx, fmt.Sprintf(storeAtRead, langparser.QuoteString(storeID), quoteInstant(at)))
		if err != nil {
			return false, fmt.Errorf("store %s, storeStatisticsStoreAt: %w", storeID, err)
		}
		return len(memql.MaterializeRows(res)) > 0, nil
	}
	// The clock first: see "The clock is asked first".
	if ok, err := exists(now); err != nil {
		return time.Time{}, err
	} else if !ok {
		return time.Time{}, fmt.Errorf("store %s, storeStatisticsStoreAt: no store row at the clock -- an unknown id, a caller "+
			"the read does not admit, or a row that is gone; no floor is kept, and the next run asks again", storeID)
	}
	var at time.Time
	for probe := 0; probe < floorProbes; probe++ {
		at = now.Add(-time.Duration(1<<probe) * 24 * time.Hour)
		ok, err := exists(at)
		if err != nil {
			return time.Time{}, err
		}
		if !ok {
			return at, nil
		}
	}
	return at, nil
}

// ensureFloor answers the store's kept history_floor, or finds it with floorOf and keeps it.
func ensureFloor(ctx context.Context, i *Integration, storeID string, now time.Time) (time.Time, error) {
	if !floorForTests.IsZero() {
		return floorForTests, nil
	}
	if strings.TrimSpace(storeID) == "" {
		return time.Time{}, fmt.Errorf("find a store's history floor: %w", errNoStore)
	}
	var db *bun.DB
	if i.bunDB != nil {
		db = i.bunDB()
	}
	if db == nil {
		return time.Time{}, errors.New("this node has no database handle, so a store's history floor cannot be kept")
	}
	var kept sql.NullTime
	err := db.NewSelect().Table("store_sales_store").Column("history_floor").
		Where("store_id = ?", storeID).Scan(ctx, &kept)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("read the history floor of store %s: %w", storeID, err)
	}
	if err == nil && kept.Valid {
		return kept.Time.UTC(), nil
	}
	floor, err := floorOf(ctx, i.engine, storeID, now)
	if err != nil {
		return time.Time{}, err
	}
	var stored time.Time
	if err := writeTx(ctx, db, storeID, anyGeneration, func(ctx context.Context, tx bun.Tx) error {
		var err error
		stored, err = keepFloor(ctx, tx, storeID, floor)
		return err
	}); err != nil {
		return time.Time{}, err
	}
	return stored, nil
}

// keepFloor keeps floor as the store's history_floor, making its row when it
// has none, and answers the floor kept: one kept before -- by another node,
// say -- stays.
func keepFloor(ctx context.Context, tx bun.Tx, storeID string, floor time.Time) (time.Time, error) {
	row := map[string]any{"store_id": storeID, "history_floor": floor.UTC()}
	var kept time.Time
	if err := tx.NewInsert().Model(&row).TableExpr("store_sales_store").
		On("CONFLICT (store_id) DO UPDATE").
		Set("history_floor = COALESCE(store_sales_store.history_floor, EXCLUDED.history_floor)").
		Returning("history_floor").
		Scan(ctx, &kept); err != nil {
		return time.Time{}, fmt.Errorf("keep the history floor of store %s: %w", storeID, err)
	}
	return kept.UTC(), nil
}

// windowWidth is a width the walk reads with: zero is defaultWindow, and any
// other is kept between minWindow and maxWindow.
func windowWidth(d time.Duration) time.Duration {
	switch {
	case d == 0:
		return defaultWindow
	case d < minWindow:
		return minWindow
	case d > maxWindow:
		return maxWindow
	}
	return d
}

// walkSource reads one source of one store in creation order from w, or from floor when w.Through is zero, in bounded
// windows, at most budget pages that held rows. visit is handed every page, and every window that held no row, so the
// caller can keep where the walk stands. next is where it stands when it returns; caughtUp is the clock the last page
// of a read to the clock was asked by, and zero when the read stopped behind.
func walkSource(ctx context.Context, engine Engine, query, storeID string, floor time.Time, w walkWindow, clock func() time.Time, budget int,
	visit func(page walkPage) error) (next walkWindow, caughtUp time.Time, err error) {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("store %s, %s: "+format, append([]any{storeID, query}, args...)...)
	}
	switch {
	case strings.TrimSpace(storeID) == "":
		return w, time.Time{}, fmt.Errorf("walk a store's rows: %w", errNoStore)
	case changedReads[query] == "":
		return w, time.Time{}, fmt.Errorf("walk a store's rows: %q is not one of the walk's reads", query)
	case engine == nil:
		return w, time.Time{}, fail("this node has no engine handle")
	case w.Through.IsZero() && floor.IsZero():
		return w, time.Time{}, fail("the store has no floor to walk from")
	case budget < 1:
		return w, time.Time{}, fail("a walk needs a budget of at least one page, not %d", budget)
	}
	ctx = walkContext(ctx)
	w.Width = windowWidth(w.Width)
	began := w.Through
	reads, counted := 0, 0

	// Each window: bounded by maxReadsPerRun, since every page of every
	// window is a read, and a window moves Through on at its last page.
	for {
		start := w.Through
		if start.IsZero() {
			start = floor
		}
		since := start.Add(-watermarkOverlap)
		until := start.Add(w.Width)
		toClock := !until.Before(clock().Add(-watermarkOverlap))
		call := fmt.Sprintf(changedReads[query], langparser.QuoteString(storeID), quoteInstant(since), walkPageSize)
		if !toClock {
			call = fmt.Sprintf(pastRead, call, quoteInstant(until))
		}

		// Each page of the window, the budget and the reads checked before
		// every one.
		cursor := ""
		for {
			if counted >= budget {
				return w, time.Time{}, nil
			}
			if reads >= maxReadsPerRun {
				if !w.Through.After(began) {
					return w, time.Time{}, fail("%w: more than %d versions (%d pages of %d) were written at or behind where the walk stands, "+
						"within the %v overlap. Every run reads the same pages and stops here, so this does not pass on its own",
						errOverlapTooDense, maxReadsPerRun*walkPageSize, maxReadsPerRun, walkPageSize, watermarkOverlap)
				}
				return w, time.Time{}, nil
			}
			askedAt := clock()
			res, err := engine.Execute(memql.ContextWithCursor(ctx, cursor), call)
			reads++
			if err != nil {
				if ctx.Err() == nil && isPageTimeout(err) {
					if w.Width <= minWindow {
						return w, time.Time{}, fail("a page of a window of %v outlasted a read, and no window is narrower: %w", minWindow, err)
					}
					if toClock {
						w.Width = windowWidth(min(askedAt.Sub(since), w.Width) / 2)
						return w, time.Time{}, nil
					}
					w.Width = windowWidth(w.Width / 2)
					return w, time.Time{}, nil
				}
				return w, time.Time{}, fail("read a page: %w", err)
			}
			// A page that says there are more rows and offers no way to them
			// is refused, never read as the end of the window.
			more, err := memql.ResultPageCursor(res)
			if err != nil {
				return w, time.Time{}, fail("%w", err)
			}
			if more != "" && more == cursor {
				return w, time.Time{}, fail("the walk did not advance past a page: the engine answered the same cursor it was asked with")
			}

			page := walkPage{Rows: memql.MaterializeRows(res), Through: w.Through, Width: w.Width}
			var newest time.Time
			for _, row := range page.Rows {
				if at, ok := instantOf(row["createdAt"]); ok && at.After(newest) {
					newest = at
				}
			}
			// The newest row read, never past the clock and never back; and,
			// once read, at least where the read started.
			stand := newest
			if now := clock(); stand.After(now) {
				stand = now
			}
			if stand.After(page.Through) {
				page.Through = stand
			}
			if page.Through.Before(start) {
				page.Through = start
			}
			last := more == ""
			if last && toClock {
				page.CaughtUp = askedAt
				if caught := askedAt.Add(-watermarkOverlap); caught.After(page.Through) {
					page.Through = caught
				}
				page.Width = defaultWindow
			}
			if last && !toClock {
				if until.After(page.Through) {
					page.Through = until
				}
				page.Width = windowWidth(2 * w.Width)
			}
			if len(page.Rows) > 0 && (began.IsZero() || newest.After(began)) {
				counted++
			}
			if err := visit(page); err != nil {
				return w, time.Time{}, fail("hand on a page: %w", err)
			}
			w = walkWindow{Through: page.Through, Width: page.Width}
			if last && toClock {
				return w, page.CaughtUp, nil
			}
			if last {
				break
			}
			cursor = more
		}
	}
}

// isPageTimeout reports whether a read ended because the page took longer
// than a read may. Copied from the email walk (component/emailstats,
// rollup.go), whose reading of it the tree keeps beside each walk it serves
// (component/retention has its own). Two deadlines can end a page, and both
// say that:
//
//   - the server's statement timeout: SQLSTATE 57014 whose message says
//     "statement timeout". 57014 is query_canceled, which a statement
//     cancelled on request -- an operator's pg_cancel_backend -- answers too,
//     with "user request"; that says nothing about the page, so the message
//     tells the two apart;
//   - pgdriver's own client read deadline, which ends a slower page first when
//     the statement timeout is off or long: a *net.OpError on a read whose
//     Timeout() is true. A dial or a write that timed out, or a connection
//     reset or refused, is the network, not the page.
//
// Each is read off the driver's error when the engine passes it on, and off
// the text the driver renders when the engine passes on only that.
//
// THE MESSAGE IS THE SERVER'S ENGLISH. A server whose lc_messages is another
// language words a statement timeout differently, so its timeouts read as
// ordinary failures here; the client read deadline still reads as a timeout.
func isPageTimeout(err error) bool {
	var pgerr pgdriver.Error
	if errors.As(err, &pgerr) {
		return pgerr.StatementTimeout() && strings.Contains(pgerr.Field('M'), "statement timeout")
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return op.Op == "read" && op.Timeout()
	}
	text := err.Error()
	return strings.Contains(text, "canceling statement due to statement timeout") || readDeadlineText.MatchString(text)
}

// readDeadlineText is how a *net.OpError renders a read that reached its
// deadline: "read tcp 10.0.0.5:51334->10.0.0.9:5432: i/o timeout".
var readDeadlineText = regexp.MustCompile(`\bread (tcp|tcp4|tcp6|unix) [^ ]+: i/o timeout`)
