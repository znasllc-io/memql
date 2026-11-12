package storestats

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
)

// budget_db_test.go -- a store's turn has a time budget, and a transaction a
// time bound, so a run's hard deadline is minutes (rollup.go, "A turn has a
// budget"). The budgets read clockForTests, which these tests drive: nothing
// here sleeps for a budget to pass.

// budgetClock is a clock the budgets read, moved by the test: by step on every
// reading, and by advance when the test says.
type budgetClock struct {
	mu   sync.Mutex
	at   time.Time
	step time.Duration
}

func (c *budgetClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.at
	c.at = c.at.Add(c.step)
	return t
}

func (c *budgetClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// useClock makes c the budgets' clock for the rest of the test.
func useClock(t *testing.T, c *budgetClock) {
	t.Helper()
	was := clockForTests
	clockForTests = c.now
	t.Cleanup(func() { clockForTests = was })
}

var sinceInRead = regexp.MustCompile(`since: "([^"]+)"`)

// A TURN PAST ITS BUDGET ASKS NO FURTHER PAGE. Each read of a changed-rows
// read moves the budgets' clock past the turn's budget: the turn asks that one
// page and no other of any source, keeps where the walk stands, is not a
// failure, and the store is taken up again by the next run, which reads on
// from there.
func TestATurnPastItsBudgetAsksNoFurtherPage(t *testing.T) {
	i, id := runStore(t)
	floorAt(t, time.Now().Add(-10*24*time.Hour))
	mirror(t, orderConcept, id, fmt.Sprintf("gid://shopify/Order/%d", time.Now().UnixNano()), orderPayload())
	clock := &budgetClock{at: time.Now()}
	useClock(t, clock)
	var mu sync.Mutex
	var reads []string
	i.engine = &hookEngine{inner: i.engine, after: func(query string) {
		if strings.Contains(query, "Changed(") {
			mu.Lock()
			reads = append(reads, query)
			mu.Unlock()
			clock.advance(turnBudget + time.Second)
		}
	}}

	if _, err := run(t, i); err != nil {
		t.Fatalf("a turn past its budget failed the run: %v", err)
	}
	mu.Lock()
	first := append([]string{}, reads...)
	reads = nil
	mu.Unlock()
	if len(first) != 1 {
		t.Fatalf("a turn past its budget after its first page asked %d pages, want that one: %v", len(first), first)
	}
	w := watermarksOf(t, id)
	if w[sourceOrder] == nil {
		t.Fatalf("the turn did not keep where the order walk stands: %v", w)
	}

	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	second := append([]string{}, reads...)
	mu.Unlock()
	if len(second) != 1 {
		t.Fatalf("the next turn asked %d pages, want one", len(second))
	}
	s1, s2 := sinceInRead.FindStringSubmatch(first[0]), sinceInRead.FindStringSubmatch(second[0])
	if s1 == nil || s2 == nil {
		t.Fatalf("no since in %q or %q", first[0], second[0])
	}
	a, _ := time.Parse(time.RFC3339Nano, s1[1])
	b, _ := time.Parse(time.RFC3339Nano, s2[1])
	if !b.After(a) {
		t.Errorf("the next turn read from %s, the first from %s: it did not carry on from where the walk stood", b, a)
	}
}

// A PURGE PAST ITS BUDGET STOPS BETWEEN BATCHES AND RESUMES. Five facts in
// batches of two: each reading of the budgets' clock moves it on, so a turn
// purges one batch and stops; the store is not a failure and its purge goes
// on next run, to the end. A store under its budget purges in one turn.
func TestAPurgePastItsBudgetStopsBetweenBatchesAndResumes(t *testing.T) {
	for _, c := range []struct {
		name string
		step time.Duration
		left []int // facts after each run
	}{
		{"past its budget", turnBudget/2 + time.Second, []int{3, 1, 0}},
		{"under its budget", 0, []int{0}},
	} {
		t.Run(c.name, func(t *testing.T) {
			i := testIntegration(t)
			// Nobody is wholesale: the clock below moves sixteen seconds at
			// every reading, which the wholesale reads' own budget of ten
			// (wholesale.go) would read as spent before their first page.
			i.wholesaleFor = nil
			answerVerdict(i, verdictReply("recording", time.Now()))
			lower(t, &deleteBatch, 2)
			id := keptStore(t)
			keep(t, i, storeOf(id, nil), time.Now()) // the first fill
			for n := 1; n <= 5; n++ {
				orderFact(t, id, fmt.Sprintf("o-%d", n), at("2025-10-01T10:00:00Z"), false)
			}
			markRebuild(t, id, time.Now())
			useClock(t, &budgetClock{at: time.Now(), step: c.step})
			for run, want := range c.left {
				if _, err := runRollup(principalCtx(t, "storeStatisticsRollupSweep"), i, time.Now()); err != nil {
					t.Fatalf("run %d: %v", run+1, err)
				}
				if got := rowsOf(t, id, "store_sales_order_fact")["store_sales_order_fact"]; got != want {
					t.Errorf("after run %d the store has %d facts, want %d", run+1, got, want)
				}
			}
			if keptRow(t, id).purging {
				t.Error("the purge did not end")
			}
		})
	}
}

// A TRANSACTION HAS A TIME BOUND. One that outlasts txTimeout is rolled
// back: its write is not kept, and the caller is told.
func TestATransactionPastItsBoundIsRolledBack(t *testing.T) {
	_, db := statsEngine(t)
	storeID := newStore(t)
	ensureRow(t, db, storeID)
	was := txTimeout
	txTimeout = 300 * time.Millisecond
	t.Cleanup(func() { txTimeout = was })
	err := writeTx(context.Background(), db, storeID, anyGeneration, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE store_sales_store SET store_name = 'past its bound' WHERE store_id = ?`, storeID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `SELECT pg_sleep(2)`)
		return err
	})
	if err == nil {
		t.Fatal("a transaction past its bound answered no error")
	}
	if got := storeRow(t, storeID)["store_name"]; got == "past its bound" {
		t.Error("a transaction past its bound was kept")
	}
	rerr := readTx(context.Background(), db, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.ExecContext(ctx, `SELECT pg_sleep(2)`)
		return err
	})
	if rerr == nil {
		t.Error("a read past its bound answered no error")
	}
}

// THE HARD DEADLINE IS MINUTES. It sits at or above what a run can take once
// its budgets are spent (runDeadlineBound) and at or below fifteen minutes, so
// work added to a store's turn asks the turn's budget between its units
// rather than adding to a sum. Not a database test.
func TestTheRunDeadlineIsMinutesAboveWhatARunCanTake(t *testing.T) {
	b := runDeadlineBound()
	if b <= runBudget+turnBudget || runDeadline < b || runDeadline > 15*time.Minute {
		t.Errorf("the run deadline is %s and what a run can take %s; want the deadline at or above it and at most fifteen minutes", runDeadline, b)
	}
	// The comment's sum, line by line (integration.go): the wholesale reads
	// count as their time budget and the one read under way when it ran out,
	// and the turn's tail is the floor's.
	if got := turnTail(); got != 210*time.Second {
		t.Errorf("a turn's tail is %s; the longest turnTail's comment writes out is the floor's, 210 s", got)
	}
	if b != 510*time.Second {
		t.Errorf("what a run can take is %s; the sum runDeadlineBound's comment writes out is 510 s", b)
	}
}

// A TURN SPENT BEFORE ITS SECOND SOURCE ASKS NOTHING OF IT. The first page of
// the order walk moves the budgets' clock past the turn's budget: the turn
// asks no page of the line and refund sources and does not read where they
// stand, ends normally with all three behind, and the next run walks them.
func TestATurnSpentBeforeASourceAsksNothingOfIt(t *testing.T) {
	i, id := runStore(t)
	floorAt(t, time.Now().Add(-10*24*time.Hour))
	mirror(t, orderConcept, id, fmt.Sprintf("gid://shopify/Order/%d", time.Now().UnixNano()), orderPayload())
	clock := &budgetClock{at: time.Now()}
	useClock(t, clock)
	var mu sync.Mutex
	var reads []string
	spend := true
	i.engine = &hookEngine{inner: i.engine, after: func(query string) {
		if strings.Contains(query, "Changed(") {
			mu.Lock()
			reads = append(reads, query)
			if spend {
				clock.advance(turnBudget + time.Second)
			}
			mu.Unlock()
		}
	}}
	queries := storeSalesQueries(t)

	rep, err := run(t, i)
	if err != nil {
		t.Fatalf("a turn spent before a source failed the run: %v", err)
	}
	mu.Lock()
	first := append([]string{}, reads...)
	reads, spend = nil, false
	mu.Unlock()
	for _, q := range first {
		if !strings.Contains(q, "storeStatisticsOrdersChanged(") {
			t.Errorf("a turn spent on its order walk asked a page of another source: %s", q)
		}
	}
	for _, q := range queries() {
		if strings.Contains(q, "store_sales_watermark") && (strings.Contains(q, "source = 'line'") || strings.Contains(q, "source = 'refund'")) {
			t.Errorf("a turn spent before its line and refund sources read where they stand: %s", q)
		}
	}
	for _, s := range []string{sourceOrder, sourceLine, sourceRefund} {
		if !slices.Contains(rep.Behind, id+"/"+s) {
			t.Errorf("the report does not say the %s source is behind: %v", s, rep.Behind)
		}
	}

	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	second := append([]string{}, reads...)
	mu.Unlock()
	if !slices.ContainsFunc(second, func(q string) bool { return strings.Contains(q, "storeStatisticsLinesChanged(") }) {
		t.Errorf("the next run asked no page of the line source: %v", second)
	}
}

// A TURN SPENT BEFORE THE FLOOR'S PROBES MAKES NONE. A store with no floor
// kept: its shop read moves the budgets' clock past the turn's budget, so the
// turn asks no probe and no page, keeps no floor, ends normally with all
// three sources behind, and the next run finds and keeps the floor.
func TestATurnSpentBeforeTheFloorMakesNoProbe(t *testing.T) {
	i, id := runStore(t)
	// The store's first keeping, whose rebuild's purge would take the turn.
	if outcome := keep(t, i, storeOf(id, nil), time.Now()); outcome != outcomeRebuilt {
		t.Fatalf("the store's first keeping answered %q, want its rebuild done", outcome)
	}
	clock := &budgetClock{at: time.Now()}
	useClock(t, clock)
	var mu sync.Mutex
	probes, pages := 0, 0
	spend := true
	i.engine = &hookEngine{inner: i.engine, after: func(query string) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(query, "storeStatisticsShop("):
			if spend {
				clock.advance(turnBudget + time.Second)
			}
		case strings.Contains(query, "storeStatisticsStoreAt("):
			probes++
		case strings.Contains(query, "Changed("):
			pages++
		}
	}}

	rep, err := run(t, i)
	if err != nil {
		t.Fatalf("a turn spent before its floor failed the run: %v", err)
	}
	mu.Lock()
	gotProbes, gotPages := probes, pages
	probes, pages, spend = 0, 0, false
	mu.Unlock()
	if gotProbes != 0 || gotPages != 0 {
		t.Errorf("a turn spent before its floor made %d probes and asked %d pages, want none", gotProbes, gotPages)
	}
	if floor := storeRow(t, id)["history_floor"]; floor != nil {
		t.Errorf("a turn spent before its floor kept the floor %v", floor)
	}
	for _, s := range []string{sourceOrder, sourceLine, sourceRefund} {
		if !slices.Contains(rep.Behind, id+"/"+s) {
			t.Errorf("the report does not say the %s source is behind: %v", s, rep.Behind)
		}
	}

	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotProbes = probes
	mu.Unlock()
	if gotProbes == 0 || storeRow(t, id)["history_floor"] == nil {
		t.Errorf("the next run made %d probes and kept the floor %v; want it found and kept", gotProbes, storeRow(t, id)["history_floor"])
	}
}
