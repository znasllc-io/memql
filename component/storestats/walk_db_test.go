package storestats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// walk_db_test.go -- the walk in windows against the real engine and
// database (harness_db_test.go): the engine's own paging, its asOf and its
// write times, which the scripted engine of walk_test.go only stands in for.

const orderConcept = "v1:shopify:order"

// orderPayload is a mirrored order the fact rules read whole (facts_test.go's
// orderRow, less what the engine writes itself).
func orderPayload() map[string]any {
	row := orderRow()
	for _, k := range []string{"id", "createdAt", "cancelledAt", "storeId", "gid"} {
		delete(row, k)
	}
	return row
}

// pagingEngine answers from the real engine and notes each read and the
// cursor it was asked with.
type pagingEngine struct {
	inner Engine
	mu    sync.Mutex
	reads []pagedRead
}

type pagedRead struct{ query, cursor string }

func (e *pagingEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	e.mu.Lock()
	e.reads = append(e.reads, pagedRead{query: query, cursor: memql.CursorFromContext(ctx)})
	e.mu.Unlock()
	return e.inner.Execute(ctx, query)
}

func (e *pagingEngine) asked() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.reads)
}

func newPagingEngine(t *testing.T) *pagingEngine {
	t.Helper()
	eng, _ := statsEngine(t)
	if eng == nil {
		t.FailNow()
	}
	return &pagingEngine{inner: eng}
}

// walkCtx is the context the run hands the walk: the rollup's maintenance
// principal. walkSource stamps the rest itself (walkContext).
func walkCtx(t *testing.T) context.Context {
	return principalCtx(t, "storeStatisticsRollupSweep")
}

// backdate moves the newest version of a mirror row to at: a version whose
// write time is earlier than the instant it became visible, as one that
// commits late is.
func backdate(t *testing.T, concept, storeID, gid string, at time.Time) {
	t.Helper()
	_, db := statsEngine(t)
	id := concept + ":" + shopify.MirrorRowID(storeID, gid)
	res, err := db.ExecContext(context.Background(), `UPDATE "MemoryNodes" SET "createdAt" = ?
		WHERE concept = ? AND id = ? AND "createdAt" = (SELECT max("createdAt") FROM "MemoryNodes" WHERE concept = ? AND id = ?)`,
		at.UTC(), concept, id, concept, id)
	if err != nil {
		t.Fatalf("backdate %s: %v", gid, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("backdate %s moved %d versions, want one", gid, n)
	}
}

// writtenAt is when the newest version of a mirror row was written.
func writtenAt(t *testing.T, concept, storeID, gid string) time.Time {
	t.Helper()
	_, db := statsEngine(t)
	var at time.Time
	if err := db.NewRaw(`SELECT max("createdAt") FROM "MemoryNodes" WHERE concept = ? AND id = ?`,
		concept, concept+":"+shopify.MirrorRowID(storeID, gid)).Scan(context.Background(), &at); err != nil {
		t.Fatal(err)
	}
	return at.UTC()
}

func gidsOf(rows []map[string]any) []string {
	out := make([]string, len(rows))
	for n, r := range rows {
		out[n], _ = r["gid"].(string)
	}
	return out
}

// A PAST WINDOW IS READ AS OF ITS END, AND THE NEXT STARTS THERE. An order
// written three hours ago and rewritten an hour ago: the window that ends two
// hours ago hands its older version, and the read to the clock after it the
// newer.
func TestAPastWindowIsReadAsOfItsEndAndTheNextStartsThere(t *testing.T) {
	e := newPagingEngine(t)
	storeID := newStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	gid := fmt.Sprintf("gid://shopify/Order/%d", now.UnixNano())
	mirror(t, orderConcept, storeID, gid, orderPayload())
	backdate(t, orderConcept, storeID, gid, now.Add(-3*time.Hour))
	mirror(t, orderConcept, storeID, gid, orderPayload())
	backdate(t, orderConcept, storeID, gid, now.Add(-time.Hour))

	var p pages
	next, caughtUp, err := walkSource(walkCtx(t), e, "storeStatisticsOrdersChanged", storeID, now.Add(-4*time.Hour),
		walkWindow{Width: 2 * time.Hour}, time.Now, maxPagesPerRun, p.visit)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 2 || len(e.reads) != 2 {
		t.Fatalf("the walk handed on %d pages from %d reads, want two of each", len(p.got), len(e.reads))
	}
	if !strings.HasPrefix(e.reads[0].query, "asOf(") || strings.HasPrefix(e.reads[1].query, "asOf(") {
		t.Errorf("the walk asked %q then %q, want the past window as of its end and then a read to the clock", e.reads[0].query, e.reads[1].query)
	}
	first, second := p.got[0], p.got[1]
	if len(first.Rows) != 1 || !first.Through.Equal(now.Add(-2*time.Hour)) {
		t.Fatalf("the past window handed on %v through %v, want the one order through the window's end %v", first.Rows, first.Through, now.Add(-2*time.Hour))
	}
	if at, _ := instantOf(first.Rows[0]["createdAt"]); !at.Equal(now.Add(-3 * time.Hour)) {
		t.Errorf("the past window handed on the version written at %v, want the one written three hours ago", at)
	}
	if len(second.Rows) != 1 || caughtUp.IsZero() || !second.CaughtUp.Equal(caughtUp) {
		t.Fatalf("the read to the clock handed on %v, caught up %v", second.Rows, caughtUp)
	}
	if at, _ := instantOf(second.Rows[0]["createdAt"]); !at.Equal(now.Add(-time.Hour)) || !next.Through.Equal(at) {
		t.Errorf("the read to the clock handed on the version written at %v and stands at %v, want the rewrite an hour ago", at, next.Through)
	}
}

// AN EMPTY STRETCH OF HISTORY IS CROSSED IN WIDENED WINDOWS: a floor a year
// back and one order today. Found in one call, within maxReadsPerRun, the
// width doubling on every empty window up to maxWindow.
func TestAnEmptyStretchOfHistoryIsCrossedInWidenedWindows(t *testing.T) {
	e := newPagingEngine(t)
	storeID := newStore(t)
	now := time.Now().UTC()
	floorForTests = now.AddDate(-1, 0, 0)
	t.Cleanup(func() { floorForTests = time.Time{} })
	i := testIntegration(t)
	i.engine = e
	floor, err := ensureFloor(walkCtx(t), i, storeID, now)
	if err != nil || !floor.Equal(floorForTests) || e.asked() != 0 {
		t.Fatalf("with floorForTests set the floor is %v (%v) after %d reads, want floorForTests and no read", floor, err, e.asked())
	}
	gid := fmt.Sprintf("gid://shopify/Order/%d", now.UnixNano())
	mirror(t, orderConcept, storeID, gid, orderPayload())

	var p pages
	_, caughtUp, err := walkSource(walkCtx(t), e, "storeStatisticsOrdersChanged", storeID, floor, walkWindow{}, time.Now, maxPagesPerRun, p.visit)
	if err != nil {
		t.Fatal(err)
	}
	if got := gidsOf(p.rows()); len(got) != 1 || got[0] != gid || caughtUp.IsZero() {
		t.Fatalf("one call over a year handed on %v, caught up %v; want today's order, caught up", got, caughtUp)
	}
	if e.asked() > 20 {
		t.Errorf("a year of nothing took %d reads; widened windows cross it in fewer than 20", e.asked())
	}
	width := defaultWindow
	for n, page := range p.got[:len(p.got)-1] {
		if width *= 2; width > maxWindow {
			width = maxWindow
		}
		if len(page.Rows) != 0 || page.Width != width {
			t.Errorf("window %d handed on %d rows at width %v, want none at %v", n, len(page.Rows), page.Width, width)
		}
	}
}

// A ROW COMMITTED BEHIND THE WATERMARK IS READ BY THE NEXT RUN. A version's
// write time is set before its transaction commits, so it can become visible
// with a write time earlier than a row the walk has already read. The next run
// starts the overlap behind where the walk stands, and reads it.
func TestARowCommittedBehindTheWatermarkIsReadByTheNextRun(t *testing.T) {
	e := newPagingEngine(t)
	storeID := newStore(t)
	suffix := time.Now().UnixNano()
	first := fmt.Sprintf("gid://shopify/Order/%d-1", suffix)
	mirror(t, orderConcept, storeID, first, orderPayload())

	floor := time.Now().UTC().Add(-time.Hour)
	var p pages
	w, _, err := walkSource(walkCtx(t), e, "storeStatisticsOrdersChanged", storeID, floor, walkWindow{}, time.Now, maxPagesPerRun, p.visit)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Through.Equal(writtenAt(t, orderConcept, storeID, first)) {
		t.Fatalf("the first run stands at %v, want the order's write time", w.Through)
	}

	late := fmt.Sprintf("gid://shopify/Order/%d-2", suffix)
	mirror(t, orderConcept, storeID, late, orderPayload())
	backdate(t, orderConcept, storeID, late, w.Through.Add(-2*time.Minute))

	p = pages{}
	if _, _, err := walkSource(walkCtx(t), e, "storeStatisticsOrdersChanged", storeID, floor, w, time.Now, maxPagesPerRun, p.visit); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range gidsOf(p.rows()) {
		found = found || g == late
	}
	if !found {
		t.Errorf("the order written two minutes behind where the walk stood was not read by the next run; it read %v", gidsOf(p.rows()))
	}
}

// A WALK PAGES EVERY MIRRORED ROW ONCE, IN CREATION ORDER. 600 orders, twenty
// of them written at one instant across the first page boundary: three pages,
// 600 rows, each order once, write times never decreasing, no cursor asked
// twice.
func TestAWalkPagesEveryMirroredRowOnceInCreationOrder(t *testing.T) {
	e := newPagingEngine(t)
	storeID := newStore(t)
	suffix := time.Now().UnixNano()
	gids := make([]string, 600)
	for n := range gids {
		gids[n] = fmt.Sprintf("gid://shopify/Order/%d-%03d", suffix, n)
		mirror(t, orderConcept, storeID, gids[n], orderPayload())
	}
	tie := writtenAt(t, orderConcept, storeID, gids[240])
	for _, gid := range gids[241:260] {
		backdate(t, orderConcept, storeID, gid, tie)
	}

	var p pages
	_, caughtUp, err := walkSource(walkCtx(t), e, "storeStatisticsOrdersChanged", storeID, time.Now().UTC().Add(-time.Hour),
		walkWindow{}, time.Now, maxPagesPerRun, p.visit)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 3 || caughtUp.IsZero() {
		t.Errorf("the walk handed on %d pages, caught up %v; want three, caught up", len(p.got), caughtUp)
	}
	rows := p.rows()
	seen := map[string]int{}
	var last time.Time
	for n, r := range rows {
		seen[r["gid"].(string)]++
		at, ok := instantOf(r["createdAt"])
		if !ok || at.Before(last) {
			t.Errorf("row %d was written at %v (%v), before the row ahead of it at %v", n, at, ok, last)
		}
		last = at
	}
	if len(rows) != 600 || len(seen) != 600 {
		t.Errorf("the walk handed on %d rows of %d orders, want 600 of 600", len(rows), len(seen))
	}
	for gid, n := range seen {
		if n != 1 {
			t.Errorf("%s was handed on %d times", gid, n)
		}
	}
	cursors := map[string]bool{}
	for _, r := range e.reads {
		if cursors[r.cursor] {
			t.Errorf("the cursor %q was asked twice", r.cursor)
		}
		cursors[r.cursor] = true
	}
}

// A ROW REWRITTEN BETWEEN TWO PAGES IS HANDED ON AGAIN, on a later page, and
// the walk ends: the engine orders by write time, so the rewrite comes after
// every row the walk had still to read.
func TestARowRewrittenBetweenTwoPagesOfTheEngineIsHandedOnAgain(t *testing.T) {
	e := newPagingEngine(t)
	storeID := newStore(t)
	suffix := time.Now().UnixNano()
	gids := make([]string, walkPageSize+10)
	for n := range gids {
		gids[n] = fmt.Sprintf("gid://shopify/Order/%d-%03d", suffix, n)
		mirror(t, orderConcept, storeID, gids[n], orderPayload())
	}
	var p pages
	visit := func(page walkPage) error {
		if len(p.got) == 0 {
			mirror(t, orderConcept, storeID, gids[0], orderPayload())
		}
		return p.visit(page)
	}
	_, caughtUp, err := walkSource(walkCtx(t), e, "storeStatisticsOrdersChanged", storeID, time.Now().UTC().Add(-time.Hour),
		walkWindow{}, time.Now, maxPagesPerRun, visit)
	if err != nil {
		t.Fatal(err)
	}
	if caughtUp.IsZero() {
		t.Error("the walk did not catch up")
	}
	var versions []time.Time
	for _, r := range p.rows() {
		if r["gid"] == gids[0] {
			at, _ := instantOf(r["createdAt"])
			versions = append(versions, at)
		}
	}
	if len(versions) != 2 || !versions[1].After(versions[0]) || len(p.rows()) != len(gids)+1 {
		t.Errorf("the order rewritten after the first page was handed on at %v among %d rows; want twice, the rewrite last, among %d", versions, len(p.rows()), len(gids)+1)
	}
}

// A WALKED ORDER IS READ BY THE FACT RULES: whatever the engine hands back for
// createdAt and the other dates, the walk passes the row on in a form
// orderFactOf reads.
func TestAWalkedOrderIsReadByTheFactRules(t *testing.T) {
	e := newPagingEngine(t)
	storeID := newStore(t)
	gid := fmt.Sprintf("gid://shopify/Order/%d", time.Now().UnixNano())
	mirror(t, orderConcept, storeID, gid, orderPayload())
	var p pages
	if _, _, err := walkSource(walkCtx(t), e, "storeStatisticsOrdersChanged", storeID, time.Now().UTC().Add(-time.Hour),
		walkWindow{}, time.Now, maxPagesPerRun, p.visit); err != nil {
		t.Fatal(err)
	}
	rows := p.rows()
	if len(rows) != 1 {
		t.Fatalf("the walk handed on %d rows, want the order", len(rows))
	}
	fact, kind, err := orderFactOf(rows[0])
	if err != nil || kind != rowLive {
		t.Fatalf("the walked order is %v to the fact rules: %v", kind, err)
	}
	if !fact.Version.Equal(writtenAt(t, orderConcept, storeID, gid)) || fact.OrderGID != gid || fact.StoreID != storeID || fact.OrderedAt.IsZero() {
		t.Errorf("the walked order's fact is %+v; want its write time, gid, store and business date", fact)
	}
}

// THE FLOOR IS FOUND ONCE AND KEPT. ensureFloor on a real store finds an
// instant before the store row was first written and keeps it in
// history_floor; asked again, it answers the kept floor and asks nothing.
func TestTheFloorIsFoundOnceAndKept(t *testing.T) {
	_, db := statsEngine(t)
	e := newPagingEngine(t)
	i := testIntegration(t)
	i.engine = e
	storeID := newStore(t)
	now := time.Now().UTC()

	floor, err := ensureFloor(walkCtx(t), i, storeID, now)
	if err != nil {
		t.Fatal(err)
	}
	asked := e.asked()
	if asked == 0 {
		t.Fatal("the floor was found without asking for the store row")
	}
	var firstWrite time.Time
	if err := db.NewRaw(`SELECT min("createdAt") FROM "MemoryNodes" WHERE concept = 'v1:shopify:store' AND id = ?`, "v1:shopify:store:"+storeID).
		Scan(context.Background(), &firstWrite); err != nil {
		t.Fatal(err)
	}
	if !floor.Before(firstWrite) {
		t.Errorf("the floor %v is not before the store row's first write %v", floor, firstWrite)
	}
	row := storeRow(t, storeID)
	if kept, ok := row["history_floor"].(time.Time); !ok || !kept.Equal(floor) {
		t.Errorf("history_floor is %v, want the floor %v", row["history_floor"], floor)
	}

	again, err := ensureFloor(walkCtx(t), i, storeID, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Equal(floor) || e.asked() != asked {
		t.Errorf("asked again, the floor is %v after %d more reads, want the kept %v and none", again, e.asked()-asked, floor)
	}
}

// A STORE IS WALKED FROM ITS FLOOR, NOT FROM A COMPILED DATE. With
// floorForTests unset, the floor comes from the store row and the order
// written after it is handed on; and no file of the package that is not a test
// holds a date.
func TestAStoreIsWalkedFromItsFloorNotFromACompiledDate(t *testing.T) {
	if !floorForTests.IsZero() {
		t.Fatalf("floorForTests is %v outside a test that sets it", floorForTests)
	}
	e := newPagingEngine(t)
	i := testIntegration(t)
	i.engine = e
	storeID := newStore(t)
	gid := fmt.Sprintf("gid://shopify/Order/%d", time.Now().UnixNano())
	mirror(t, orderConcept, storeID, gid, orderPayload())

	now := time.Now().UTC().Truncate(time.Microsecond)
	floor, err := ensureFloor(walkCtx(t), i, storeID, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(-24 * time.Hour); !floor.Equal(want) {
		t.Errorf("the floor of a store written a moment ago is %v, want the clock less a day, %v", floor, want)
	}
	var p pages
	if _, _, err := walkSource(walkCtx(t), e, "storeStatisticsOrdersChanged", storeID, floor, walkWindow{}, time.Now, maxPagesPerRun, p.visit); err != nil {
		t.Fatal(err)
	}
	if got := gidsOf(p.rows()); len(got) != 1 || got[0] != gid {
		t.Errorf("the walk from the store's floor handed on %v, want its order", got)
	}

	date := regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					if s, err := strconv.Unquote(x.Value); err == nil && date.MatchString(s) {
						t.Errorf("%s: a date literal %q; where a store's history begins is found, never compiled in", fset.Position(x.Pos()), s)
					}
				}
			case *ast.SelectorExpr:
				if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "time" && x.Sel.Name == "Date" {
					t.Errorf("%s: time.%s builds a date; where a store's history begins is found, never compiled in", fset.Position(x.Pos()), x.Sel.Name)
				}
			}
			return true
		})
	}
	if checked < 4 {
		t.Fatalf("checked only %d files of the package", checked)
	}
}

// IS PAGE TIMEOUT READS THE STATEMENT TIMEOUT AND NOTHING ELSE: the email
// walk's cases (component/emailstats,
// TestAPageTooLongForItsDeadlineIsToldApartFromAnyOtherFailure), on the
// driver's own errors, so a database test. The server ending the statement at
// its statement timeout and pgdriver's read deadline are a page too long; a
// statement cancelled on request, a refused or reset connection and an ended
// context are not.
func TestIsPageTimeoutReadsTheStatementTimeoutAndNothingElse(t *testing.T) {
	_, db := statsEngine(t)
	timeout := driverStatementTimeout(t, db)
	deadline := driverReadDeadline(t)
	var ne net.Error
	if !errors.As(deadline, &ne) || !ne.Timeout() {
		t.Fatalf("pgdriver's read deadline answered %T (%v), want a net.Error whose Timeout() is true", deadline, deadline)
	}
	for name, err := range map[string]error{
		"the statement timeout":                timeout,
		"the statement timeout, wrapped":       fmt.Errorf("read a page: %w", timeout),
		"the statement timeout, as text":       errors.New(timeout.Error()),
		"the statement timeout, flattened":     fmt.Errorf("execute: %v", timeout),
		"the driver's read deadline":           deadline,
		"the driver's read deadline, wrapped":  fmt.Errorf("read a page: %w", deadline),
		"the driver's read deadline, as text":  errors.New(deadline.Error()),
		"the driver's read deadline, flatten.": fmt.Errorf("execute: %v", deadline),
	} {
		if !isPageTimeout(err) {
			t.Errorf("%s (%v) is not read as a page too long for its deadline", name, err)
		}
	}
	cancelled := driverCancelOnRequest(t, db)
	refused := driverConnectionRefused(t)
	for name, err := range map[string]error{
		"a statement cancelled on request":            cancelled,
		"a statement cancelled on request, wrapped":   fmt.Errorf("read a page: %w", cancelled),
		"a statement cancelled on request, as text":   errors.New(cancelled.Error()),
		"a refused connection":                        refused,
		"a refused connection, as text":               errors.New(refused.Error()),
		"a reset connection":                          errors.New("read tcp 10.0.0.5:5432: connection reset by peer"),
		"a dial that timed out":                       &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded},
		"a dial that timed out, as text":              errors.New("dial tcp 10.0.0.5:5432: i/o timeout"),
		"a write that timed out":                      &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded},
		"the run's context ending":                    fmt.Errorf("read a page: %w", context.Canceled),
		"the run's context reaching its own deadline": fmt.Errorf("read a page: %w", context.DeadlineExceeded),
	} {
		if isPageTimeout(err) {
			t.Errorf("%s (%v) is read as a page too long for its deadline", name, err)
		}
	}
}

// driverStatementTimeout is the error pgdriver answers for a statement the
// server ends at its statement timeout.
func driverStatementTimeout(t *testing.T, db *bun.DB) error {
	t.Helper()
	var timeout error
	_ = db.RunInTx(context.Background(), nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout = '50ms'`); err != nil {
			t.Fatal(err)
		}
		_, timeout = tx.ExecContext(ctx, `SELECT pg_sleep(2)`)
		return timeout
	})
	if timeout == nil {
		t.Fatal("a two-second statement under a fifty-millisecond timeout did not fail")
	}
	return timeout
}

// driverReadDeadline is the error pgdriver answers for a statement that
// outlasts its own client read deadline, on a handle of its own with a short
// one and no server statement timeout.
func driverReadDeadline(t *testing.T) error {
	t.Helper()
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithReadTimeout(200*time.Millisecond)))
	defer func() { _ = sqldb.Close() }()
	_, err := sqldb.ExecContext(context.Background(), `SELECT pg_sleep(2)`)
	if err == nil {
		t.Fatal("a two-second statement under a two-hundred-millisecond read deadline did not fail")
	}
	return err
}

// driverCancelOnRequest is the error pgdriver answers for a statement another
// session cancels, as an operator's pg_cancel_backend does: SQLSTATE 57014,
// like the statement timeout, with the caller's own context live throughout.
func driverCancelOnRequest(t *testing.T, db *bun.DB) error {
	t.Helper()
	ctx := context.Background()
	conn, err := db.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var pid int
	if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := conn.ExecContext(ctx, `SELECT pg_sleep(5)`)
		done <- err
	}()
	for waited := 0; ; waited++ {
		var cancelled bool
		if err := db.NewRaw(`SELECT pg_cancel_backend(pid) FROM pg_stat_activity WHERE pid = ? AND state = 'active' AND query LIKE 'SELECT pg_sleep(5)%'`, pid).Scan(ctx, &cancelled); err == nil && cancelled {
			break
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		if waited > 200 {
			t.Fatal("the statement to cancel never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	err = <-done
	if err == nil {
		t.Fatal("a statement cancelled on request did not fail")
	}
	return err
}

// driverConnectionRefused is the error pgdriver answers when no server takes
// the connection.
func driverConnectionRefused(t *testing.T) error {
	t.Helper()
	sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithAddr("127.0.0.1:1"), pgdriver.WithUser("memql"), pgdriver.WithInsecure(true), pgdriver.WithTimeout(2*time.Second)))
	defer func() { _ = sqldb.Close() }()
	_, err := sqldb.ExecContext(context.Background(), `SELECT 1`)
	if err == nil {
		t.Fatal("a connection to a port nothing listens on did not fail")
	}
	return err
}
