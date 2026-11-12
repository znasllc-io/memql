package storestats

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// walk_test.go -- the walk in windows against a scripted engine: one that
// answers the walk's reads from versions it holds, the way the engine's
// latest-version scan does (the newest version of each row as of the read's
// instant, kept when it was written at or after since, oldest first, ties by
// id, a keyset cursor), and that can be told to time out, to hand a cursor
// back unchanged, or to fail. Not a database test: walk_db_test.go holds the
// same walk to the real engine.

// version is one version of one row, as the scripted engine holds it.
type version struct {
	id    string
	at    time.Time
	email string // a buyer's email, so a test can see an error or a report does not carry it
}

type scriptedCall struct {
	query, cursor              string
	since, until               time.Time // until is zero for a read to the clock
	internal, fresh, rootsOnly bool
}

type scriptedEngine struct {
	t        *testing.T
	clock    time.Time
	pageSize int
	versions []version

	// storeFirstWritten answers the store row read: a row for an instant at
	// or after it, none before. Zero answers none ever.
	storeFirstWritten time.Time

	timeout    func(call scriptedCall, read int) bool // answer the statement timeout
	fail       func(call scriptedCall, read int) error
	sameCursor bool           // answer every page with more rows and the one cursor "stuck"
	afterPage  func(read int) // runs after each walk read is answered

	calls []scriptedCall
}

var (
	walkAsked    = regexp.MustCompile(`^(?:asOf\()?paginate\(query (\w+)\(storeId: "([^"]*)", since: "([^"]+)"\), (\d+)\)(?:, "([^"]+)"\))?$`)
	storeAtAsked = regexp.MustCompile(`^query storeStatisticsStoreAt\(storeId: "([^"]*)", at: "([^"]+)"\)$`)
)

func (e *scriptedEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	call := scriptedCall{
		query:     query,
		cursor:    memql.CursorFromContext(ctx),
		internal:  auth.OriginFromContext(ctx).IsInternal(),
		fresh:     memql.FreshReadFromContext(ctx),
		rootsOnly: memql.RootsOnlyReadFromContext(ctx),
	}
	if m := storeAtAsked.FindStringSubmatch(query); m != nil {
		at := mustInstant(e.t, m[2])
		e.calls = append(e.calls, call)
		if e.storeFirstWritten.IsZero() || at.Before(e.storeFirstWritten) {
			return memql.NewResultWithOutput([]any{}), nil
		}
		return memql.NewResultWithOutput([]any{map[string]any{"id": "v1:shopify:store:" + m[1]}}), nil
	}
	m := walkAsked.FindStringSubmatch(query)
	if m == nil {
		e.t.Fatalf("the walk asked %q, which is no read of the walk", query)
	}
	if m[4] != fmt.Sprint(walkPageSize) {
		e.t.Errorf("the walk asked for pages of %s, want %d", m[4], walkPageSize)
	}
	call.since = mustInstant(e.t, m[3])
	if m[5] != "" {
		call.until = mustInstant(e.t, m[5])
	}
	e.calls = append(e.calls, call)
	read := len(e.calls)
	if e.fail != nil {
		if err := e.fail(call, read); err != nil {
			return nil, err
		}
	}
	if e.timeout != nil && e.timeout(call, read) {
		return nil, errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE=57014)")
	}
	if e.sameCursor {
		res := memql.NewResultWithOutput([]any{map[string]any{"id": "v1:shopify:order:x", "createdAt": call.since}})
		res.Meta = &memql.ResultMeta{HasMore: true, Cursor: "stuck"}
		return res, nil
	}
	until := call.until
	if until.IsZero() {
		until = e.clock.Add(time.Hour) // every version there is
	}
	latest := map[string]version{}
	for _, v := range e.versions {
		if v.at.After(until) {
			continue
		}
		if cur, ok := latest[v.id]; !ok || v.at.After(cur.at) {
			latest[v.id] = v
		}
	}
	var rows []version
	for _, v := range latest {
		if !v.at.Before(call.since) {
			rows = append(rows, v)
		}
	}
	sort.Slice(rows, func(a, b int) bool {
		if !rows[a].at.Equal(rows[b].at) {
			return rows[a].at.Before(rows[b].at)
		}
		return rows[a].id < rows[b].id
	})
	if call.cursor != "" {
		parts := strings.SplitN(call.cursor, "|", 2)
		at, id := mustInstant(e.t, parts[0]), parts[1]
		for len(rows) > 0 && (rows[0].at.Before(at) || rows[0].at.Equal(at) && rows[0].id <= id) {
			rows = rows[1:]
		}
	}
	size := e.pageSize
	if size == 0 {
		size = walkPageSize
	}
	more := len(rows) > size
	if more {
		rows = rows[:size]
	}
	out := make([]any, len(rows))
	for n, v := range rows {
		out[n] = map[string]any{"id": v.id, "createdAt": v.at, "email": v.email}
	}
	res := memql.NewResultWithOutput(out)
	if more {
		last := rows[len(rows)-1]
		res.Meta = &memql.ResultMeta{HasMore: true, Cursor: last.at.UTC().Format(time.RFC3339Nano) + "|" + last.id}
	}
	if e.afterPage != nil {
		e.afterPage(read)
	}
	return res, nil
}

// call is the n-th call the engine was asked, or fails the test.
func (e *scriptedEngine) call(n int) scriptedCall {
	e.t.Helper()
	if n < 0 {
		n += len(e.calls)
	}
	if n < 0 || n >= len(e.calls) {
		e.t.Fatalf("the engine was asked %d times; there is no call %d", len(e.calls), n)
	}
	return e.calls[n]
}

// walkReads is how many reads of the walk the engine answered or refused.
func (e *scriptedEngine) walkReads() int {
	n := 0
	for _, c := range e.calls {
		if !c.since.IsZero() {
			n++
		}
	}
	return n
}

// pages records every page the walk hands on.
type pages struct{ got []walkPage }

func (p *pages) visit(page walkPage) error {
	p.got = append(p.got, page)
	return nil
}

func (p *pages) rows() []map[string]any {
	var out []map[string]any
	for _, page := range p.got {
		out = append(out, page.Rows...)
	}
	return out
}

const testQuery = "storeStatisticsOrdersChanged"

var testClock = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

func newScripted(t *testing.T) *scriptedEngine { return &scriptedEngine{t: t, clock: testClock} }

func (e *scriptedEngine) walk(floor time.Time, w walkWindow, budget int, p *pages) (walkWindow, time.Time, error) {
	return walkSource(context.Background(), e, testQuery, "store-a", floor, w, fixedClock(e.clock), budget, p.visit)
}

// A PAGE THAT OUTLASTS A READ HALVES THE WINDOW. The statement timeout on the
// first page of a past window: the walk stands where it stood, with half the
// width, and no error, so the next run reads a narrower window. On a later
// page of a window, where the last complete page left it. At the narrowest
// window it is the source's error, with the store and the read named.
func TestAPageThatOutlastsAReadHalvesTheWindow(t *testing.T) {
	floor := testClock.AddDate(0, 0, -30)

	e := newScripted(t)
	e.timeout = func(scriptedCall, int) bool { return true }
	var p pages
	next, caughtUp, err := e.walk(floor, walkWindow{Width: defaultWindow}, maxPagesPerRun, &p)
	if err != nil {
		t.Fatalf("a page that outlasted a read failed the walk: %v; want the window halved and no error", err)
	}
	if next.Width != defaultWindow/2 || !next.Through.IsZero() || !caughtUp.IsZero() {
		t.Errorf("after the timeout the walk stands at %+v (caught up %v), want the width halved to %v and not walked yet", next, caughtUp, defaultWindow/2)
	}
	if e.walkReads() != 1 || e.call(0).until.IsZero() {
		t.Errorf("the walk read %d times, first %q; want one read of a past window, as of its end", e.walkReads(), e.call(0).query)
	}
	if len(p.got) != 0 {
		t.Errorf("the walk handed on %d pages from a read that timed out", len(p.got))
	}

	// A later page: where the last complete page left it.
	e = newScripted(t)
	e.pageSize = 2
	for n := 0; n < 5; n++ {
		e.versions = append(e.versions, version{id: fmt.Sprintf("o%d", n), at: floor.Add(time.Duration(n+1) * time.Hour)})
	}
	e.timeout = func(_ scriptedCall, read int) bool { return read == 2 }
	p = pages{}
	next, _, err = e.walk(floor, walkWindow{Width: defaultWindow}, maxPagesPerRun, &p)
	if err != nil {
		t.Fatal(err)
	}
	if want := floor.Add(2 * time.Hour); !next.Through.Equal(want) || next.Width != defaultWindow/2 {
		t.Errorf("a timeout on the second page left the walk at %+v, want through the first page's newest row %v at half the width", next, want)
	}

	// The narrowest window: the source's error.
	e = newScripted(t)
	e.timeout = func(scriptedCall, int) bool { return true }
	_, _, err = e.walk(floor, walkWindow{Width: minWindow}, maxPagesPerRun, &pages{})
	if err == nil {
		t.Fatal("a page of the narrowest window that outlasted a read answered no error")
	}
	if !strings.Contains(err.Error(), "store-a") || !strings.Contains(err.Error(), testQuery) {
		t.Errorf("the error %q does not name the store and the read", err)
	}
}

// A READ TO THE CLOCK THAT TIMES OUT HALVES THE SPAN IT TRIED: the next run
// reads a past window half as far as this one tried to read.
func TestAReadToTheClockThatTimesOutHalvesTheSpanItTried(t *testing.T) {
	e := newScripted(t)
	e.timeout = func(scriptedCall, int) bool { return true }
	through := testClock.Add(-time.Hour)
	next, caughtUp, err := e.walk(testClock.AddDate(0, 0, -30), walkWindow{Through: through, Width: defaultWindow}, maxPagesPerRun, &pages{})
	if err != nil {
		t.Fatalf("a read to the clock that timed out failed the walk: %v", err)
	}
	if e.walkReads() != 1 || !e.call(0).until.IsZero() {
		t.Fatalf("the walk asked %v, want one read to the clock", e.calls)
	}
	span := testClock.Sub(through.Add(-watermarkOverlap))
	if next.Width != span/2 || !next.Through.Equal(through) || !caughtUp.IsZero() {
		t.Errorf("after the timeout the walk stands at %+v (caught up %v), want through %v with half the %v it tried", next, caughtUp, through, span)
	}
}

// A CURSOR THE ENGINE HANDS BACK UNCHANGED IS AN ERROR, never a loop.
func TestACursorTheEngineHandsBackUnchangedIsAnError(t *testing.T) {
	e := newScripted(t)
	e.sameCursor = true
	_, _, err := e.walk(testClock.Add(-time.Hour), walkWindow{}, maxPagesPerRun, &pages{})
	if err == nil {
		t.Fatal("a cursor handed back unchanged answered no error")
	}
	if e.walkReads() != 2 {
		t.Errorf("the walk read %d pages, want two: the first, and the one that handed its cursor back", e.walkReads())
	}
	if !strings.Contains(err.Error(), "store-a") || !strings.Contains(err.Error(), testQuery) {
		t.Errorf("the error %q does not name the store and the read", err)
	}
}

// ONLY PAGES THAT HELD ROWS COUNT AGAINST THE BUDGET. Ten empty windows and a
// page of rows are read on a budget of one; the page after it is not, though
// it is in the same window. Every read counts against maxReadsPerRun, which
// bounds a history of nothing but empty windows.
func TestOnlyPagesThatHeldRowsCountAgainstTheBudget(t *testing.T) {
	floor := testClock.AddDate(0, 0, -400)
	// Ten empty windows: 1, 2, 4, 8 and 16 days, then five of 32.
	end := floor.Add((1 + 2 + 4 + 8 + 16 + 5*32) * 24 * time.Hour)
	e := newScripted(t)
	e.pageSize = 2
	for n := 0; n < 5; n++ {
		e.versions = append(e.versions, version{id: fmt.Sprintf("o%d", n), at: end.Add(24*time.Hour + time.Duration(n)*time.Minute)})
	}
	var p pages
	next, caughtUp, err := e.walk(floor, walkWindow{}, 1, &p)
	if err != nil {
		t.Fatal(err)
	}
	if e.walkReads() != 11 {
		t.Errorf("on a budget of one the walk read %d times, want 11: ten empty windows and one page of rows", e.walkReads())
	}
	if len(p.got) != 11 || len(p.got[10].Rows) != 2 {
		t.Fatalf("the walk handed on %d pages, want ten empty and one of two rows", len(p.got))
	}
	for n, page := range p.got[:10] {
		if len(page.Rows) != 0 {
			t.Errorf("window %d handed on %d rows, want none", n, len(page.Rows))
		}
	}
	if want := e.versions[1].at; !next.Through.Equal(want) || !caughtUp.IsZero() {
		t.Errorf("the walk stopped at %+v (caught up %v), want through the page's newest row %v and behind", next, caughtUp, want)
	}

	// Nothing but empty windows, on any budget: maxReadsPerRun.
	e = newScripted(t)
	p = pages{}
	next, caughtUp, err = e.walk(testClock.AddDate(-50, 0, 0), walkWindow{}, 1, &p)
	if err != nil {
		t.Fatal(err)
	}
	if e.walkReads() != maxReadsPerRun || len(p.got) != maxReadsPerRun {
		t.Errorf("fifty years of empty windows took %d reads and %d pages, want maxReadsPerRun (%d)", e.walkReads(), len(p.got), maxReadsPerRun)
	}
	if next.Through.IsZero() || !caughtUp.IsZero() || next.Width != maxWindow {
		t.Errorf("the run stopped at %+v (caught up %v), want where its last window ended, at the widest window, behind", next, caughtUp)
	}
}

// A WINDOW THAT HELD NO ROW IS STILL HANDED ON, with the window's end as where
// the walk stands and the doubled width, so the caller keeps both.
func TestAWindowThatHeldNoRowIsStillHandedOn(t *testing.T) {
	floor := testClock.AddDate(0, 0, -10)
	e := newScripted(t)
	var p pages
	if _, _, err := e.walk(floor, walkWindow{Width: defaultWindow}, maxPagesPerRun, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.got) == 0 {
		t.Fatal("the walk handed on nothing")
	}
	first := p.got[0]
	if len(first.Rows) != 0 || !first.Through.Equal(floor.Add(defaultWindow)) || first.Width != 2*defaultWindow || !first.CaughtUp.IsZero() {
		t.Errorf("the first window was handed on as %+v, want no rows, through %v, width %v", first, floor.Add(defaultWindow), 2*defaultWindow)
	}
	if !e.call(0).since.Equal(floor.Add(-watermarkOverlap)) || !e.call(0).until.Equal(floor.Add(defaultWindow)) {
		t.Errorf("the first window read from %v as of %v, want from the floor less the overlap as of the floor plus the width", e.call(0).since, e.call(0).until)
	}
	// The last: a read to the clock, caught up at the clock it was asked by.
	last := p.got[len(p.got)-1]
	if !last.CaughtUp.Equal(testClock) || !e.call(-1).until.IsZero() {
		t.Errorf("the last page was %+v from %q, want a read to the clock caught up at %v", last, e.call(-1).query, testClock)
	}
}

// EVERY READ IS THE WALK'S: internal, fresh and roots-only, whatever context
// the caller hands in (walkContext); and a blank store is refused before
// anything is asked, since a blank member also matches rows with no store.
func TestEveryReadOfTheWalkIsMadeUnderWalkContext(t *testing.T) {
	e := newScripted(t)
	e.storeFirstWritten = testClock.AddDate(0, 0, -3)
	if _, err := floorOf(context.Background(), e, "store-a", testClock); err != nil {
		t.Fatal(err)
	}
	e.versions = []version{{id: "o1", at: testClock.Add(-time.Minute)}}
	if _, _, err := e.walk(testClock.AddDate(0, 0, -3), walkWindow{}, maxPagesPerRun, &pages{}); err != nil {
		t.Fatal(err)
	}
	if len(e.calls) < 3 {
		t.Fatalf("the engine was asked %d times", len(e.calls))
	}
	for _, c := range e.calls {
		if !c.internal || !c.fresh || !c.rootsOnly {
			t.Errorf("%q was asked internal %v, fresh %v, roots-only %v; want all three", c.query, c.internal, c.fresh, c.rootsOnly)
		}
	}

	e = newScripted(t)
	if _, err := floorOf(context.Background(), e, " ", testClock); err == nil {
		t.Error("the floor of a blank store answered no error")
	}
	if _, _, err := walkSource(context.Background(), e, testQuery, "", testClock.Add(-time.Hour), walkWindow{}, fixedClock(testClock), 1, (&pages{}).visit); err == nil {
		t.Error("a walk of a blank store answered no error")
	}
	if len(e.calls) != 0 {
		t.Errorf("a blank store was asked about: %v", e.calls)
	}
}

// A READ THAT FAILS STOPS THE SOURCE FOR THIS RUN where the last complete page
// left it, and says which store and which read, and nothing of a row.
func TestAReadThatFailsLeavesTheWalkWhereItsLastPageLeftIt(t *testing.T) {
	floor := testClock.Add(-time.Hour)
	e := newScripted(t)
	e.pageSize = 2
	for n := 0; n < 5; n++ {
		e.versions = append(e.versions, version{id: fmt.Sprintf("o%d", n), at: floor.Add(time.Duration(n+1) * time.Minute), email: "buyer@example.com"})
	}
	e.fail = func(_ scriptedCall, read int) error {
		if read == 2 {
			return errors.New("the database went away")
		}
		return nil
	}
	var p pages
	next, _, err := e.walk(floor, walkWindow{}, maxPagesPerRun, &p)
	if err == nil {
		t.Fatal("a read that failed answered no error")
	}
	if !strings.Contains(err.Error(), "store-a") || !strings.Contains(err.Error(), testQuery) || strings.Contains(err.Error(), "example.com") {
		t.Errorf("the error %q does not name the store and the read, or names a row", err)
	}
	if want := floor.Add(2 * time.Minute); !next.Through.Equal(want) || len(p.got) != 1 {
		t.Errorf("after the failure the walk stands at %+v with %d pages handed on, want through the first page's newest row %v", next, len(p.got), want)
	}
}

// ROWS WRITTEN AT ONE INSTANT, ACROSS A PAGE BOUNDARY, ARE NOT LOST, and the
// walk still ends. `since` is inclusive and the walk stands at the newest row
// it read, never past it: a run that stops between two pages of the same
// instant starts the next at or before that instant, and reads the rest.
func TestRowsWrittenAtOneInstantAcrossAPageBoundaryAreAllRead(t *testing.T) {
	floor := testClock.Add(-time.Hour)
	instant := testClock.Add(-30 * time.Minute)
	e := newScripted(t)
	e.pageSize = 2
	var want []string
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		e.versions = append(e.versions, version{id: id, at: instant})
		want = append(want, id)
	}
	e.versions = append(e.versions, version{id: "f", at: instant.Add(time.Second)})
	want = append(want, "f")

	seen := map[string]bool{}
	w := walkWindow{}
	runs := 0
	for ; runs < 10; runs++ {
		var p pages
		next, caughtUp, err := e.walk(floor, w, 1, &p)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range p.rows() {
			seen[r["id"].(string)] = true
		}
		if runs == 0 && !next.Through.Equal(instant) {
			t.Errorf("the first run, stopped inside the instant, stands at %v, want the instant %v itself", next.Through, instant)
		}
		w = next
		if !caughtUp.IsZero() {
			break
		}
	}
	if runs == 10 {
		t.Fatal("ten runs on a budget of one did not catch up with six rows")
	}
	for _, id := range want {
		if !seen[id] {
			t.Errorf("the row %s, written at the instant a page ended, was never read", id)
		}
	}
	if !w.Through.Equal(instant.Add(time.Second)) {
		t.Errorf("the walk ended at %v, want the newest row's %v", w.Through, instant.Add(time.Second))
	}
}

// A ROW REWRITTEN BETWEEN TWO PAGES COMES BACK, and the walk hands it on again
// and ends: it never assumes it sees a row once (the fold dedupes by id and
// version).
func TestARowRewrittenBetweenTwoPagesIsHandedOnAgain(t *testing.T) {
	floor := testClock.Add(-time.Hour)
	e := newScripted(t)
	e.pageSize = 2
	for n := 0; n < 4; n++ {
		e.versions = append(e.versions, version{id: fmt.Sprintf("o%d", n), at: floor.Add(time.Duration(n+1) * time.Minute)})
	}
	rewritten := testClock.Add(-time.Second)
	e.afterPage = func(read int) {
		if read == 1 {
			e.versions = append(e.versions, version{id: "o0", at: rewritten})
		}
	}
	var p pages
	_, caughtUp, err := e.walk(floor, walkWindow{}, maxPagesPerRun, &p)
	if err != nil {
		t.Fatal(err)
	}
	if caughtUp.IsZero() {
		t.Error("the walk did not catch up")
	}
	var versions []time.Time
	for _, r := range p.rows() {
		if r["id"] == "o0" {
			versions = append(versions, r["createdAt"].(time.Time))
		}
	}
	if len(versions) != 2 || !versions[1].Equal(rewritten) {
		t.Errorf("the rewritten row was handed on at %v, want its first version and then its rewrite at %v", versions, rewritten)
	}
	if len(p.rows()) != 5 {
		t.Errorf("the walk handed on %d rows, want the four and the rewrite", len(p.rows()))
	}
}

// THE WALK STANDS AT THE NEWEST ROW IT READ, NEVER PAST THE CLOCK: a row dated
// past the clock moves it only as far as the clock.
func TestTheWalkNeverStandsPastTheClock(t *testing.T) {
	e := newScripted(t)
	e.versions = []version{{id: "o1", at: testClock.Add(time.Minute)}}
	next, caughtUp, err := e.walk(testClock.Add(-time.Hour), walkWindow{}, maxPagesPerRun, &pages{})
	if err != nil {
		t.Fatal(err)
	}
	if !next.Through.Equal(testClock) || !caughtUp.Equal(testClock) {
		t.Errorf("a row dated past the clock left the walk at %v (caught up %v), want the clock %v", next.Through, caughtUp, testClock)
	}
}

// A BOUNDED RUN CARRIES ON WHERE IT STOPPED. A long history on a budget of one
// page a run: each run stops at its budget with the window it reached kept,
// the next carries on from there, and the runs together read what one
// unbounded run reads and end where it ends.
func TestABoundedRunCarriesOnWhereItStopped(t *testing.T) {
	floor := testClock.AddDate(0, 0, -100)
	script := func() *scriptedEngine {
		e := newScripted(t)
		e.pageSize = 2
		for n := 0; n < 40; n++ {
			// Bursts of four rows a few days apart, two of each burst at one instant.
			at := floor.Add(time.Duration(n/4)*9*24*time.Hour + time.Duration(n%4/2)*time.Second)
			e.versions = append(e.versions, version{id: fmt.Sprintf("o%02d", n), at: at})
		}
		// And the newest row an hour ago, so where a caught-up walk stands is
		// that row whatever windows it came through: a read to the clock
		// moves Through only to the newest row it read.
		e.versions = append(e.versions, version{id: "o40", at: testClock.Add(-time.Hour)})
		return e
	}
	type key struct {
		id string
		at time.Time
	}
	read := func(p *pages, into map[key]bool) {
		for _, r := range p.rows() {
			into[key{r["id"].(string), r["createdAt"].(time.Time)}] = true
		}
	}

	whole := script()
	var all pages
	end, caughtUp, err := whole.walk(floor, walkWindow{}, 1000, &all)
	if err != nil || caughtUp.IsZero() {
		t.Fatalf("one unbounded run answered %v and caught up %v", err, caughtUp)
	}
	want := map[key]bool{}
	read(&all, want)

	e := script()
	got := map[key]bool{}
	w := walkWindow{}
	runs := 0
	for ; runs < 100; runs++ {
		var p pages
		next, caughtUp, err := e.walk(floor, w, 1, &p)
		if err != nil {
			t.Fatal(err)
		}
		read(&p, got)
		if caughtUp.IsZero() && !next.Through.After(w.Through) {
			t.Fatalf("run %d stopped behind at %v without moving from %v", runs, next.Through, w.Through)
		}
		w = next
		if !caughtUp.IsZero() {
			break
		}
	}
	if runs < 2 {
		t.Errorf("a budget of one caught up in %d runs; the test does not bound anything", runs+1)
	}
	if len(got) != len(want) || len(want) != 41 {
		t.Errorf("the bounded runs read %d versions, one unbounded run %d, of 41", len(got), len(want))
	}
	for k := range want {
		if !got[k] {
			t.Errorf("the bounded runs never read %v", k)
		}
	}
	if !w.Through.Equal(end.Through) || !end.Through.Equal(testClock.Add(-time.Hour)) || w.Width != end.Width {
		t.Errorf("the bounded runs ended at %+v, one unbounded run at %+v", w, end)
	}
}

// THE FLOOR IS THE FIRST INSTANT WITH NO STORE ROW, asked at the clock less 1,
// 2, 4 ... days. A store first written 100 days ago and rewritten since has a
// row as of 1 to 64 days back and none as of 128: its floor is the clock less
// 128 days. One written an hour ago: the clock less a day.
func TestTheFloorIsTheFirstInstantWithNoStoreRow(t *testing.T) {
	for _, c := range []struct {
		name         string
		firstWritten time.Time
		floor        time.Time
		probes       int
	}{
		{"a hundred days old", testClock.AddDate(0, 0, -100), testClock.Add(-128 * 24 * time.Hour), 8},
		{"an hour old", testClock.Add(-time.Hour), testClock.Add(-24 * time.Hour), 1},
	} {
		e := newScripted(t)
		e.storeFirstWritten = c.firstWritten
		floor, err := floorOf(context.Background(), e, "store-a", testClock)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !floor.Equal(c.floor) || len(e.calls) != c.probes {
			t.Errorf("%s: the floor is %v after %d probes, want %v after %d", c.name, floor, len(e.calls), c.floor, c.probes)
		}
	}
}

// A STORE OLDER THAN EVERY PROBE has the last instant asked as its floor: the
// clock less 8192 days, after floorProbes probes.
func TestTheFloorOfAStoreOlderThanEveryProbeIsTheLastInstantAsked(t *testing.T) {
	e := newScripted(t)
	e.storeFirstWritten = testClock.AddDate(-40, 0, 0)
	floor, err := floorOf(context.Background(), e, "store-a", testClock)
	if err != nil {
		t.Fatal(err)
	}
	if want := testClock.Add(-8192 * 24 * time.Hour); !floor.Equal(want) || len(e.calls) != floorProbes {
		t.Errorf("the floor is %v after %d probes, want %v after %d", floor, len(e.calls), want, floorProbes)
	}
}

// THE STORE CONCEPT KEEPS EVERY VERSION. The floor is found by asking for the
// store row as of earlier and earlier instants; if old versions of it were
// ever pruned, the probe would find the oldest kept, the floor would move
// forward, and every row written before it would never be walked.
func TestTheStoreConceptKeepsEveryVersion(t *testing.T) {
	concepts, _, err := memql.BuildUnifiedConcepts(nil, memqldsl.Tree())
	if err != nil {
		t.Fatal(err)
	}
	store := concepts["v1:shopify:store"]
	if store == nil {
		t.Fatal("no v1:shopify:store in the DSL tree")
	}
	if store.Retention != nil {
		t.Errorf("v1:shopify:store declares @retention %+v. The store statistics walk finds each store's history floor "+
			"by reading the store row as of earlier and earlier instants (component/storestats, floorOf), so it depends on "+
			"every version of the store row being kept: with versions pruned the floor moves forward and every row written "+
			"before it is never walked", *store.Retention)
	}
}
