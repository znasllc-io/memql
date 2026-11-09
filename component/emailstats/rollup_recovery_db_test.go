package emailstats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/memql"
)

// rollup_recovery_db_test.go -- the two ways a walk could stop for good, and
// how each now carries on (rollup.go, "A burst the overlap cannot hold" and
// "A walk whose pages outgrow a read").
//
// Postgres-gated, as the rest of the package: the facts, the watermark and
// the two recovery columns are the real tables.

// walkedDelivery is a delivery as the walk reads it.
func walkedDelivery(suffix string, n int, at time.Time) map[string]any {
	return map[string]any{"id": fmt.Sprintf("v1:campaigns:delivery:%s-%d", suffix, n), "accountId": "org-x", "status": "sent", "sentAt": stamp(at), "createdAt": at}
}

// pagedEngine answers the delivery walk with its rows written at or after the
// walk's since, oldest first, walkPageSize to a page, each page's cursor the
// index of the row after it; every other read answers nothing. It records the
// since each walk began at.
type pagedEngine struct {
	t      *testing.T
	rows   []map[string]any
	starts []time.Time
}

func (e *pagedEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if m := queryName.FindStringSubmatch(query); m == nil || m[1] != "deliveriesWrittenSince" {
		return memql.NewResultWithOutput([]any{}), nil
	}
	since := sinceOf(e.t, query)
	from := 0
	if cursor := memql.CursorFromContext(ctx); cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil {
			return nil, err
		}
		from = n
	} else {
		e.starts = append(e.starts, since)
		for from < len(e.rows) && e.rows[from]["createdAt"].(time.Time).Before(since) {
			from++
		}
	}
	end := min(from+walkPageSize, len(e.rows))
	out := make([]any, 0, end-from)
	for _, r := range e.rows[from:end] {
		out = append(out, r)
	}
	res := memql.NewResultWithOutput(out)
	if end < len(e.rows) {
		res.SetMeta(&memql.ResultMeta{Cursor: strconv.Itoa(end), HasMore: true})
	}
	return res, nil
}

// A BURST THE OVERLAP CANNOT HOLD IS READ IN PARTS. Every walk starts five
// minutes before its watermark and reads those rows again, and a run reads at
// most maxPagesReadPerRun pages. A burst of more rows than that inside five
// minutes used to fail every run at the same bound: the run read the burst
// from its start, stopped, wrote no watermark past it, and the next run began
// it again, so nothing written after the burst was ever counted. Now the run
// that stops there records how far it got, says it is behind, and the next run
// carries on from there, past the burst to the rows after it.
func TestABurstTheOverlapCannotHoldIsReadInPartsAcrossRuns(t *testing.T) {
	_, db := statsEngine(t)
	suffix := fmt.Sprintf("burst-%d", time.Now().UnixNano())
	pageSize := walkPageSize
	walkPageSize = 2
	t.Cleanup(func() {
		walkPageSize = pageSize
		_, _ = db.NewRaw(`DELETE FROM email_stat_fact WHERE source_id LIKE ?`, suffix+"%").Exec(context.Background())
		walkFrom(t, db, time.Now())
	})
	watermark := time.Now().UTC().Truncate(time.Millisecond).Add(-3 * time.Hour)
	walkFrom(t, db, watermark)

	// The burst: more rows inside the overlap than a run reads pages of, all
	// written before the watermark, in microsecond steps. Then three rows
	// after it, which only a walk past the burst reaches.
	burst := (maxPagesReadPerRun + 50) * walkPageSize
	engine := &pagedEngine{t: t}
	for i := 0; i < burst; i++ {
		engine.rows = append(engine.rows, walkedDelivery(suffix, i, watermark.Add(-4*time.Minute+time.Duration(i)*time.Microsecond)))
	}
	var after []string
	for i := 1; i <= 3; i++ {
		engine.rows = append(engine.rows, walkedDelivery(suffix, burst+i, watermark.Add(time.Duration(i)*time.Second)))
		after = append(after, fmt.Sprintf("%s-%d", suffix, burst+i))
	}
	now := watermark.Add(time.Hour)

	first, err := runRollup(principalCtx(t), engine, db, now)
	if err != nil {
		t.Fatalf("the run that met the burst failed: %v; want it to stop behind and carry on next time", err)
	}
	if !behind(first, sourceDelivery) {
		t.Errorf("the run that stopped inside the burst did not say the deliveries are behind: %+v", first)
	}
	if _, err := runRollup(principalCtx(t), engine, db, now); err != nil {
		t.Fatalf("the run after it failed: %v", err)
	}
	if len(engine.starts) != 2 || !engine.starts[1].After(engine.starts[0]) {
		t.Fatalf("the walks began at %v; want the second to carry on from where the first stopped, not from the start of the overlap again", engine.starts)
	}
	facts := storedFacts(t, db, after...)
	for _, id := range after {
		wantFact(t, facts, kindSent, id)
	}
	if through, _ := watermarkOf(t, db, sourceDelivery); !through.Equal(watermark.Add(3 * time.Second)) {
		t.Errorf("after the burst the watermark is %v, want the last row's %v", through, watermark.Add(3*time.Second))
	}
}

// spanEngine stands in for the engine's latest-version scan, whose page costs
// what it collapses: every version written between the walk's since and the
// instant it reads as of -- the clock, for a walk that names none. A page over
// more than limit times out as the pool's statement timeout ends it; a page
// within it answers the rows written in its span, oldest first, in one page.
type spanEngine struct {
	t     *testing.T
	clock time.Time
	limit time.Duration
	rows  []map[string]any
	reads []string
}

var asOfArg = regexp.MustCompile(`^asOf\(.*, "([^"]+)"\)$`)

func (e *spanEngine) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	if m := queryName.FindStringSubmatch(query); m == nil || m[1] != "deliveriesWrittenSince" {
		return memql.NewResultWithOutput([]any{}), nil
	}
	e.reads = append(e.reads, query)
	since, until := sinceOf(e.t, query), e.clock
	if m := asOfArg.FindStringSubmatch(query); m != nil {
		at, err := time.Parse(time.RFC3339Nano, m[1])
		if err != nil {
			e.t.Fatal(err)
		}
		until = at
	}
	if until.Sub(since) > e.limit {
		return nil, errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE=57014)")
	}
	out := []any{}
	for _, r := range e.rows {
		if at := r["createdAt"].(time.Time); !at.Before(since) && !at.After(until) {
			out = append(out, r)
		}
	}
	return memql.NewResultWithOutput(out), nil
}

// A WALK WHOSE PAGES OUTGROW A READ CARRIES ON IN BOUNDED WINDOWS. A walk
// that has a watermark pays, for every page, for every version written since
// it; behind by enough, each page outlasts the statement timeout, and the
// walk used to fail every run from the same watermark, its figures null for
// good -- only a first walk's first page moved its start. Now a page that
// times out bounds the next walk to half way from its start to the clock it
// asked by, read as of that instant, halving again on each further timeout;
// the walk that reads its last page clears the bound. So the walk moves on,
// window by window, until it reads to the clock again.
func TestAWalkWhosePagesOutgrowAReadCarriesOnInBoundedWindows(t *testing.T) {
	_, db := statsEngine(t)
	suffix := fmt.Sprintf("window-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.NewRaw(`DELETE FROM email_stat_fact WHERE source_id LIKE ?`, suffix+"%").Exec(context.Background())
		walkFrom(t, db, time.Now())
	})
	now := time.Now().UTC().Truncate(time.Millisecond)
	watermark := now.Add(-5 * 24 * time.Hour)
	walkFrom(t, db, watermark)
	engine := &spanEngine{t: t, clock: now, limit: 2 * 24 * time.Hour}
	var ids []string
	for i := 1; i <= 19; i++ {
		at := watermark.Add(time.Duration(i) * 6 * time.Hour)
		engine.rows = append(engine.rows, walkedDelivery(suffix, i, at))
		ids = append(ids, fmt.Sprintf("%s-%d", suffix, i))
	}

	runs := 0
	for ; runs < 12 && len(storedFacts(t, db, ids...)) < len(ids); runs++ {
		_, _ = runRollup(principalCtx(t), engine, db, now)
	}
	if got := len(storedFacts(t, db, ids...)); got != len(ids) {
		t.Fatalf("after %d runs %d of the %d deliveries are counted; the walk did not carry on past the timeout. Its reads:\n%s",
			runs, got, len(ids), strings.Join(engine.reads, "\n"))
	}
	if until, ok := readUntilOf(t, db, sourceDelivery); ok {
		t.Errorf("the walk that read to the clock again left its bound at %v", until)
	}
	if walked, _ := walkedOf(t, db, sourceDelivery); walked.Before(engine.rows[len(engine.rows)-1]["createdAt"].(time.Time)) {
		t.Errorf("the deliveries are walked through %v, before the last row the walk read", walked)
	}
}

// AN EMPTY WINDOW STILL MOVES THE WALK ON. A backlog whose first stretch holds
// no row gives a bounded walk nothing to resume from: without more, its
// watermark would stay, the next walk would read the same span to the clock,
// time out, and be bounded to the same empty window, for good. A bounded walk
// that reads its last page has read everything as of its bound, so it resumes
// from the bound, when the bound is more than the overlap behind the clock.
func TestAnEmptyBoundedWindowStillMovesTheWalkOn(t *testing.T) {
	_, db := statsEngine(t)
	suffix := fmt.Sprintf("emptywindow-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.NewRaw(`DELETE FROM email_stat_fact WHERE source_id LIKE ?`, suffix+"%").Exec(context.Background())
		walkFrom(t, db, time.Now())
	})
	now := time.Now().UTC().Truncate(time.Millisecond)
	watermark := now.Add(-5 * 24 * time.Hour)
	walkFrom(t, db, watermark)
	engine := &spanEngine{t: t, clock: now, limit: 2 * 24 * time.Hour}
	var ids []string
	for i := 1; i <= 10; i++ {
		at := watermark.Add(84*time.Hour + time.Duration(i)*time.Hour)
		engine.rows = append(engine.rows, walkedDelivery(suffix, i, at))
		ids = append(ids, fmt.Sprintf("%s-%d", suffix, i))
	}

	runs := 0
	for ; runs < 12 && len(storedFacts(t, db, ids...)) < len(ids); runs++ {
		_, _ = runRollup(principalCtx(t), engine, db, now)
	}
	if got := len(storedFacts(t, db, ids...)); got != len(ids) {
		t.Fatalf("after %d runs %d of the %d deliveries are counted; the walk stayed on an empty window. Its reads:\n%s",
			runs, got, len(ids), strings.Join(engine.reads, "\n"))
	}
}

// readUntilOf is how far a source's next walk reads (email_stat_watermark.
// read_until), and false when it reads to the clock.
func readUntilOf(t *testing.T, db *bun.DB, source string) (time.Time, bool) {
	t.Helper()
	var at sql.NullTime
	err := db.NewRaw(`SELECT read_until FROM email_stat_watermark WHERE source = ?`, source).Scan(context.Background(), &at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false
	}
	if err != nil {
		t.Fatalf("read how far the %s walk reads: %v", source, err)
	}
	return at.Time, at.Valid
}

// scanCapture records, for a context that carries it, every statement the
// engine sends: what proves which versions a read collapses.
type scanCapture struct {
	mu      sync.Mutex
	queries []string
}

type scanCaptureKey struct{}

func (c *scanCapture) BeforeQuery(ctx context.Context, e *bun.QueryEvent) context.Context {
	if ctx.Value(scanCaptureKey{}) != nil {
		c.mu.Lock()
		c.queries = append(c.queries, e.Query)
		c.mu.Unlock()
	}
	return ctx
}

func (c *scanCapture) AfterQuery(context.Context, *bun.QueryEvent) {}

var captureOnce sync.Once
var capture = &scanCapture{}

// A BOUNDED WALK READS THE GRAPH AS OF ITS END, through the real engine: the
// deliveries written by then are counted, at their versions then, and a
// later one waits for the next walk; the latest-version scan is bounded on
// both sides, so its page costs the versions in between rather than every one
// since the walk's start; and the walk that reads its last page clears the
// bound and is walked through it, not through the clock.
func TestABoundedWalkReadsTheGraphAsOfItsEnd(t *testing.T) {
	f := newFixture(t)
	captureOnce.Do(func() { f.db.AddQueryHook(capture) })
	recipient := f.recipient(f.audience)
	base := time.Now().UTC().Truncate(time.Millisecond).Add(-3 * time.Hour)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	send := func(n, minutes int) string {
		return f.deliver(recipient, map[string]any{"emailRuleId": f.rule, "requestId": fmt.Sprintf("%s-bounded-%d", f.suffix, n),
			"status": "sent", "sentAt": stamp(at(minutes)), "versionTime": stamp(at(minutes))})
	}
	early, middle, late := send(1, 0), send(2, 60), send(3, 120)
	t.Cleanup(func() { walkFrom(t, f.db, time.Now()) })
	walkFrom(t, f.db, at(-1))
	until := at(90)
	if _, err := f.db.NewRaw(`UPDATE email_stat_watermark SET read_until = ? WHERE source = ?`, until, sourceDelivery).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}

	capture.mu.Lock()
	capture.queries = nil
	capture.mu.Unlock()
	ctx := context.WithValue(principalCtx(t), scanCaptureKey{}, true)
	if _, err := runRollup(ctx, f.eng, f.db, time.Now()); err != nil {
		t.Fatal(err)
	}
	facts := storedFacts(t, f.db, early, middle, late)
	wantFact(t, facts, kindSent, early)
	wantFact(t, facts, kindSent, middle)
	wantNoFact(t, facts, kindSent, late)
	if got, ok := readUntilOf(t, f.db, sourceDelivery); ok {
		t.Errorf("the bounded walk read its last page and left its bound at %v", got)
	}
	if walked, _ := walkedOf(t, f.db, sourceDelivery); !walked.Equal(until) {
		t.Errorf("the bounded walk is walked through %v, want its bound, %v: the rows after it were not read", walked, until)
	}
	// The bound as bun writes an instant into a statement: UTC, its fraction
	// as short as it can be (".39", not ".390").
	bound := `"createdAt" <= '` + until.UTC().Format("2006-01-02 15:04:05.999999")
	bounded := false
	capture.mu.Lock()
	for _, q := range capture.queries {
		if strings.Contains(q, "latest_keys") && strings.Contains(q, bound) {
			bounded = true
		}
	}
	captured := strings.Join(capture.queries, "\n")
	capture.mu.Unlock()
	if !bounded {
		t.Errorf("no latest-version scan was bounded at %v, so a page still collapses every version since the walk's start:\n%s", until, captured)
	}

	if _, err := runRollup(principalCtx(t), f.eng, f.db, time.Now()); err != nil {
		t.Fatal(err)
	}
	wantFact(t, storedFacts(t, f.db, late), kindSent, late)
}
