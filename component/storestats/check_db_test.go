package storestats

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// check_db_test.go -- the periodic check (check.go): a pass over a store's
// mirrored rows that compares each fact with what its row gives now and
// repairs what differs, against the real engine and database. Postgres-gated,
// through the harness.

// checkAhead is how far ahead of the clock a test's check runs, so the rows
// the test wrote a moment ago are before the pass's cut and are compared.
const checkAhead = 6 * time.Minute

// checkCtx is the context the check's sweep runs its builtin under: the
// check's maintenance principal.
func checkCtx(t *testing.T) context.Context {
	t.Helper()
	return principalCtx(t, "storeStatisticsCheckSweep")
}

// settle runs the rollup until the store's first fill is over: its walk at
// the clock and its rebuild ended.
func settle(t *testing.T, i *Integration, id string) {
	t.Helper()
	for range 2 {
		if _, err := run(t, i); err != nil {
			t.Fatal(err)
		}
	}
	if k := keptRow(t, id); k.rebuildingSince.Valid || !k.walkedThrough.Valid {
		t.Fatalf("the store did not settle: rebuilding %v, walked through %v", k.rebuildingSince, k.walkedThrough)
	}
}

// checkRowOf is the store's store_sales_check row, and whether it has one.
func checkRowOf(t *testing.T, id string) (checkRow, bool) {
	t.Helper()
	_, db := statsEngine(t)
	var row checkRow
	if err := db.NewSelect().Model(&row).Where("store_id = ?", id).Scan(context.Background()); err != nil {
		return checkRow{}, false
	}
	return row, true
}

// passRead reports whether the row holds a pass that has read every source.
func passRead(r checkRow) bool {
	return r.PassStartedAt.Valid && r.OrderCursor == cursorDone && r.LineCursor == cursorDone && r.RefundCursor == cursorDone
}

// passCounts are the counts of the pass the row speaks for: the one under
// way, else the last that ended.
func passCounts(r checkRow) (compared, mismatched, repaired int) {
	if r.PassStartedAt.Valid {
		return r.Compared, r.Mismatched, r.Repaired
	}
	return r.LastCompared, r.LastMismatched, r.LastRepaired
}

// checkThrough carries the store's pass on at the clock plus checkAhead, a
// call at a time, until it ends or has read every source, and answers the
// store's check row.
func checkThrough(t *testing.T, i *Integration, id string, wholesale wholesaleState) checkRow {
	t.Helper()
	return checkThroughAt(t, i, id, wholesale, time.Now().Add(checkAhead))
}

func checkThroughAt(t *testing.T, i *Integration, id string, wholesale wholesaleState, now time.Time) checkRow {
	t.Helper()
	for range 30 {
		finished, err := checkStore(checkCtx(t), i, id, now, checkPagesPerRun, wholesale, nil)
		if err != nil {
			t.Fatalf("check the store: %v", err)
		}
		row, found := checkRowOf(t, id)
		if finished || (found && passRead(row)) {
			return row
		}
	}
	t.Error("the pass did not end in 30 calls")
	row, _ := checkRowOf(t, id)
	return row
}

func wantCounts(t *testing.T, what string, r checkRow, mismatched, repaired int) {
	t.Helper()
	if _, m, rp := passCounts(r); m != mismatched || rp != repaired {
		t.Errorf("%s: mismatched %d and repaired %d, want %d and %d", what, m, rp, mismatched, repaired)
	}
}

// ---- the pass and its repairs ----

// A FACT DELETED BY HAND IS RESTORED from its row, in each of the four
// tables, and the pass counts it once as mismatched and once as repaired.
func TestAFactDeletedByHandIsRestored(t *testing.T) {
	for _, c := range []struct{ table, column, gid string }{
		{"store_sales_order_fact", "order_gid", theOrder},
		{"store_sales_line_fact", "line_gid", lineOne},
		{"store_sales_refund_fact", "refund_gid", theRefund},
		{"store_sales_refund_line_fact", "refund_gid", theRefund},
	} {
		t.Run(c.table, func(t *testing.T) {
			i, id := runStore(t)
			mirrorThreeLines(t, id)
			settle(t, i, id)
			before := snapshot(t, id)
			exec(t, "DELETE FROM "+c.table+" WHERE store_id = ? AND "+c.column+" = ?", id, c.gid)
			row := checkThrough(t, i, id, wholesaleState{})
			if after := snapshot(t, id); after != before {
				t.Errorf("the facts after the pass differ from before the delete:\nbefore %s\nafter  %s", before, after)
			}
			wantCounts(t, c.table, row, 1, 1)
		})
	}
}

// A FACT WHOSE VERSION WAS SET BACK is older than its row: it is written
// again from the row, its version and its content.
func TestAFactWhoseVersionWasSetBackIsRecomputed(t *testing.T) {
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	settle(t, i, id)
	before := snapshot(t, id)
	exec(t, `UPDATE store_sales_order_fact SET order_version = order_version - interval '1 hour', shipping = 0
		WHERE store_id = ? AND order_gid = ?`, id, theOrder)
	row := checkThrough(t, i, id, wholesaleState{})
	if after := snapshot(t, id); after != before {
		t.Errorf("the order set back was not recomputed:\nbefore %s\nafter  %s", before, after)
	}
	wantCounts(t, "the order set back", row, 1, 1)
}

// A ROW CHANGED IN PLACE keeps its write time, so its fact is at the row's
// own version and only its content tells: a line's title rewritten.
func TestAFactWhoseRowWasChangedInPlaceIsRecomputed(t *testing.T) {
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	settle(t, i, id)
	rewriteInPlace(t, "v1:shopify:orderLineItem", id, lineTwo, map[string]any{"title": "Bicycle bell"})
	row := checkThrough(t, i, id, wholesaleState{})
	line := lineFactsOf(t, id)[lineTwo]
	want(t, "line two", line, "title", "Bicycle bell")
	wantVersion(t, "line two", line, "line_version", writtenAt(t, "v1:shopify:orderLineItem", id, lineTwo))
	wantCounts(t, "a title changed in place", row, 1, 1)
}

// AN ERASED ORDER IS RETAIL after one pass: its email, rewritten in place to
// the erasure's marker, is no granted buyer's.
func TestAnOrderWhoseEmailWasScrubbedInPlaceIsRetailAfterOnePass(t *testing.T) {
	i, id := runStore(t)
	granted := wholesaleState{Enabled: true, Granted: map[string]bool{"buyer-1@example.test": true}}
	i.wholesaleFor = func(context.Context, []string) (map[string]wholesaleState, error) {
		return map[string]wholesaleState{id: granted}, nil
	}
	mirrorOrder(t, id, theOrder, "email", "buyer-1@example.test")
	settle(t, i, id)
	want(t, "the granted buyer's order", orderFactsOf(t, id)[theOrder], "wholesale", "true")
	rewriteInPlace(t, orderConcept, id, theOrder, map[string]any{"email": shopify.RedactionMarker})
	row := checkThrough(t, i, id, granted)
	want(t, "the erased order", orderFactsOf(t, id)[theOrder], "wholesale", "false")
	wantCounts(t, "an erased order", row, 1, 1)
}

// AN ERASED CUSTOM ITEM'S FACT holds the marker for its title and its product
// key after one pass, and nothing of what the person typed.
func TestACustomItemsScrubbedTitleLeavesTheFactAfterOnePass(t *testing.T) {
	i, id := runStore(t)
	mirrorOrder(t, id, theOrder, "subtotalLineItemsQuantity", float64(1))
	mirrorLine(t, id, lineOne, theOrder, "productGid", nil, "title", "Engraving: Ada", "quantity", float64(1), "currentQuantity", float64(1),
		"originalUnitPriceSet", set("15.00", "EUR"), "discountAllocations", []any{})
	settle(t, i, id)
	want(t, "the custom item", lineFactsOf(t, id)[lineOne], "title", "Engraving: Ada", "product_key", "title:Engraving: Ada")
	rewriteInPlace(t, "v1:shopify:orderLineItem", id, lineOne, map[string]any{"title": shopify.RedactionMarker})
	checkThrough(t, i, id, wholesaleState{})
	line := lineFactsOf(t, id)[lineOne]
	for _, col := range []string{"title", "product_key"} {
		if strings.Contains(cell(line, col), "Ada") {
			t.Errorf("the erased custom item's %s still holds what was typed: %s", col, cell(line, col))
		}
	}
	want(t, "the erased custom item", line, "title", shopify.RedactionMarker, "product_key", "title:"+shopify.RedactionMarker)
}

// AN OLDER VERSION A PAST WINDOW READS AGAIN is no mismatch: the pass's first
// window hands a row's older version, which is older than its fact. The pass
// is begun with ten-minute order windows, so its first is in the past.
func TestAnOlderVersionReadByAnEarlierWindowIsNotAMismatch(t *testing.T) {
	now := time.Now().UTC()
	floorAt(t, now.Add(-time.Hour))
	i, id := runStore(t)
	mirrorOrder(t, id, theOrder)
	backdate(t, orderConcept, id, theOrder, now.Add(-55*time.Minute))
	mirrorOrder(t, id, theOrder, "test", true)
	settle(t, i, id)
	at := now.Add(checkAhead)
	exec(t, `INSERT INTO store_sales_check (store_id, pass_started_at, order_window_seconds) VALUES (?, ?, 600)
		ON CONFLICT (store_id) DO UPDATE SET pass_started_at = EXCLUDED.pass_started_at, order_cursor = '', line_cursor = '',
			refund_cursor = '', order_window_seconds = 600, compared = 0, mismatched = 0, repaired = 0`, id, at)
	var asked []string
	i.engine = &hookEngine{inner: i.engine, before: func(q string) error { asked = append(asked, q); return nil }}
	row := checkThroughAt(t, i, id, wholesaleState{}, at)
	if len(asked) == 0 || !strings.HasPrefix(asked[0], "asOf(") {
		t.Fatalf("the pass's first read was not a past window: %v", asked)
	}
	compared, mismatched, _ := passCounts(row)
	if mismatched != 0 || compared == 0 {
		t.Errorf("a row rewritten later: compared %d and mismatched %d, want the newer version compared and nothing mismatched", compared, mismatched)
	}
	want(t, "the order", orderFactsOf(t, id)[theOrder], "test", "true")
}

// A DERIVED COLUMN CHANGED BY HAND is derived again: every order page
// derives its orders.
func TestADerivedColumnChangedByHandIsDerivedAgain(t *testing.T) {
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	settle(t, i, id)
	exec(t, `UPDATE store_sales_order_fact SET gross = 1.00 WHERE store_id = ? AND order_gid = ?`, id, theOrder)
	checkThrough(t, i, id, wholesaleState{})
	want(t, "the order", orderFactsOf(t, id)[theOrder], "gross", "99.99")
}

// A STORE WHOSE WALK IS BEHIND IS NOT CHECKED, nor one that is rebuilding,
// purging or in a wholesale pass: no pass begins and nothing is repaired. The
// same store with its walk caught up is checked.
func TestAStoreWhoseWalkIsBehindIsNotChecked(t *testing.T) {
	for _, c := range []struct{ name, set, unset string }{
		{"behind", `walked_through = now() - interval '1 hour'`, `walked_through = now()`},
		{"rebuilding", `rebuilding_since = now()`, `rebuilding_since = NULL`},
		{"purging", `purging = true`, `purging = false`},
		{"in a wholesale pass", `wholesale_cursor = 'under way'`, `wholesale_cursor = ''`},
	} {
		t.Run(c.name, func(t *testing.T) {
			i, id := runStore(t)
			mirrorThreeLines(t, id)
			settle(t, i, id)
			before, _ := checkRowOf(t, id)
			exec(t, `DELETE FROM store_sales_order_fact WHERE store_id = ? AND order_gid = ?`, id, theOrder)
			exec(t, `UPDATE store_sales_store SET `+c.set+` WHERE store_id = ?`, id)
			finished, err := checkStore(checkCtx(t), i, id, time.Now().Add(checkAhead), checkPagesPerRun, wholesaleState{}, nil)
			if err != nil || finished {
				t.Fatalf("a store %s: finished %v, error %v; want it left", c.name, finished, err)
			}
			after, _ := checkRowOf(t, id)
			if after.PassStartedAt != before.PassStartedAt || after.LastStartedAt != before.LastStartedAt {
				t.Errorf("a store %s had a pass begun: before %+v, after %+v", c.name, before, after)
			}
			if orderFactsOf(t, id)[theOrder] != nil {
				t.Errorf("a store %s had its order repaired", c.name)
			}
			exec(t, `UPDATE store_sales_store SET `+c.unset+` WHERE store_id = ?`, id)
			checkThrough(t, i, id, wholesaleState{})
			if orderFactsOf(t, id)[theOrder] == nil {
				t.Errorf("the store caught up again was not checked: its order is still missing")
			}
		})
	}
}

// A PASS KEEPS THE WIDTH IT LEARNED FOR EACH SOURCE: a page that outlasts a
// read halves order_window_seconds, the next call reads orders at that
// width, and the lines' width is not touched.
func TestAPassKeepsTheWindowWidthItLearnedForEachSource(t *testing.T) {
	now := time.Now().UTC()
	floor := now.Add(-72 * time.Hour)
	floorAt(t, floor)
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	settle(t, i, id)
	var mu sync.Mutex
	var asked []string
	timedOut := false
	i.engine = &hookEngine{inner: i.engine, before: func(q string) error {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, q)
		if strings.Contains(q, "storeStatisticsOrdersChanged") && !timedOut {
			timedOut = true
			return errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")
		}
		return nil
	}}
	at := time.Now().Add(checkAhead)
	if _, err := checkStore(checkCtx(t), i, id, at, checkPagesPerRun, wholesaleState{}, nil); err != nil {
		t.Fatal(err)
	}
	row, _ := checkRowOf(t, id)
	if row.OrderWindowSeconds != 43200 || row.OrderCursor != "" {
		t.Errorf("after a page that outlasted a read: order window %d s at cursor %q, want 43200 s not begun", row.OrderWindowSeconds, row.OrderCursor)
	}
	if row.LineWindowSeconds != 86400 {
		t.Errorf("the lines' window is %d s, want 86400 s untouched", row.LineWindowSeconds)
	}
	mu.Lock()
	asked = nil
	mu.Unlock()
	if _, err := checkStore(checkCtx(t), i, id, at, checkPagesPerRun, wholesaleState{}, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var first string
	for _, q := range asked {
		if strings.Contains(q, "storeStatisticsOrdersChanged") {
			first = q
			break
		}
	}
	if !strings.Contains(first, quoteInstant(floor.Add(12*time.Hour))) {
		t.Errorf("the next call's first order read is %q, want a window to the floor plus twelve hours", first)
	}
}

// A RECOMPUTED ORDER KEEPS ITS WHOLESALE SIDE: a granted buyer's order fact
// deleted by hand comes back wholesale, judged with the store's granted set.
func TestARecomputedOrderKeepsItsWholesaleSide(t *testing.T) {
	i, id := runStore(t)
	granted := wholesaleState{Enabled: true, Granted: map[string]bool{"buyer-1@example.test": true}}
	i.wholesaleFor = func(context.Context, []string) (map[string]wholesaleState, error) {
		return map[string]wholesaleState{id: granted}, nil
	}
	mirrorOrder(t, id, theOrder, "email", "Buyer-1@Example.test")
	settle(t, i, id)
	exec(t, `DELETE FROM store_sales_order_fact WHERE store_id = ? AND order_gid = ?`, id, theOrder)
	checkThrough(t, i, id, granted)
	want(t, "the recomputed order", orderFactsOf(t, id)[theOrder], "wholesale", "true")
}

// ---- the end of a pass ----

// A FACT NO MIRRORED ROW ANSWERS FOR IS DELETED at the end of a pass: an
// order fact, a line fact and a refund fact with its lines, each counted as
// mismatched and repaired, and the order whose line and refund went is
// derived again.
func TestAFactWithNoMirroredRowIsDeleted(t *testing.T) {
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	settle(t, i, id)
	before := snapshot(t, id)
	orderFact(t, id, "gid://shopify/Order/9001", time.Now().Add(-time.Hour), false)
	lineFact(t, id, "gid://shopify/LineItem/9001", theOrder)
	exec(t, `INSERT INTO store_sales_refund_fact (store_id, refund_gid, order_gid, refund_version, recorded_at, refunded_at,
		money_refunded, detail_fetched, lines_complete) VALUES (?, 'gid://shopify/Refund/9001', ?, now(), now(), now(), 19.99, true, true)`, id, theOrder)
	exec(t, `INSERT INTO store_sales_refund_line_fact (store_id, refund_gid, position, order_gid, line_gid, quantity, subtotal, tax)
		VALUES (?, 'gid://shopify/Refund/9001', 0, ?, ?, 1, 19.99, 0)`, id, theOrder, lineTwo)
	exec(t, `UPDATE store_sales_line_fact SET refunded_quantity = 1 WHERE store_id = ? AND line_gid = ?`, id, lineTwo)
	row := checkThrough(t, i, id, wholesaleState{})
	if after := snapshot(t, id); after != before {
		t.Errorf("the facts no row answers for were not all deleted, or their order not derived again:\nbefore %s\nafter  %s", before, after)
	}
	wantCounts(t, "three facts no row answers for", row, 3, 3)
}

// THE DELETE RUNS IN BATCHES: five order facts no row answers for and a
// batch of two are three batches that delete, and the pass ends only after a
// fourth that deletes nothing.
func TestTheDeleteOfFactsNoRowAnswersForRunsInBatches(t *testing.T) {
	lower(t, &deleteBatch, 2)
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	settle(t, i, id)
	for n := range 5 {
		orderFact(t, id, "gid://shopify/Order/"+strconv.Itoa(9001+n), time.Now().Add(-time.Hour), false)
	}
	queries := storeSalesQueries(t)
	row := checkThrough(t, i, id, wholesaleState{})
	batches, ended := 0, -1
	for _, q := range queries() {
		if strings.HasPrefix(q, "DELETE FROM store_sales_order_fact") && strings.Contains(q, "checked_at IS NULL") {
			batches++
		}
		if strings.HasPrefix(q, "UPDATE store_sales_check") && strings.Contains(q, "last_started_at") {
			if ended >= 0 {
				t.Errorf("the pass ended twice")
			}
			ended = batches
		}
	}
	if batches != 4 || ended != 4 {
		t.Errorf("the delete took %d batches and the pass ended after %d, want four and after the fourth", batches, ended)
	}
	if n := len(orderFactsOf(t, id)); n != 1 {
		t.Errorf("the store keeps %d order facts, want its one mirrored order", n)
	}
	wantCounts(t, "five facts no row answers for", row, 5, 5)
}

// A DELETED ORDER COSTS A PASS NOTHING (Review Focus): an order folded live
// and then retired. Each of two passes, whose first window reads the live
// version again, ends with nothing mismatched and clean, and the fact is
// still deleted.
func TestACheckOverAStoreWithADeletedOrderEndsCleanWithNothingMismatched(t *testing.T) {
	now := time.Now().UTC()
	floorAt(t, now.Add(-time.Hour))
	i, id := runStore(t)
	mirrorOrder(t, id, theOrder)
	backdate(t, orderConcept, id, theOrder, now.Add(-55*time.Minute))
	settle(t, i, id)
	want(t, "the order folded live", orderFactsOf(t, id)[theOrder], "deleted", "false")
	mirror(t, orderConcept, id, theOrder, map[string]any{"deleted": true})
	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
	want(t, "the retired order", orderFactsOf(t, id)[theOrder], "deleted", "true")
	for pass, at := range []time.Time{now.Add(checkAhead), now.Add(checkAhead + time.Minute)} {
		exec(t, `INSERT INTO store_sales_check (store_id, pass_started_at, order_window_seconds) VALUES (?, ?, 600)
			ON CONFLICT (store_id) DO UPDATE SET pass_started_at = EXCLUDED.pass_started_at, order_cursor = '', line_cursor = '',
				refund_cursor = '', order_window_seconds = 600, compared = 0, mismatched = 0, repaired = 0`, id, at)
		row := checkThroughAt(t, i, id, wholesaleState{}, at)
		if row.PassStartedAt.Valid || row.LastMismatched != 0 || !row.CleanAt.Valid || !row.CleanAt.Time.Equal(micro(at)) {
			t.Errorf("pass %d: under way %v, mismatched %d, clean at %v; want it ended clean at %v", pass+1, row.PassStartedAt.Valid,
				row.LastMismatched, row.CleanAt, at)
		}
		want(t, "the retired order", orderFactsOf(t, id)[theOrder], "deleted", "true")
	}
}

// A DELETED FACT IS NEVER DELETED AS ONE NO ROW ANSWERS FOR: its mirror row
// removed by hand, it is still there, deleted, after a pass.
func TestADeletedFactIsNeverDeletedAsAnOrphan(t *testing.T) {
	i, id := runStore(t)
	mirrorOrder(t, id, theOrder)
	settle(t, i, id)
	mirror(t, orderConcept, id, theOrder, map[string]any{"deleted": true})
	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
	exec(t, `DELETE FROM "MemoryNodes" WHERE concept = ? AND id = ?`, orderConcept, orderConcept+":"+shopify.MirrorRowID(id, theOrder))
	row := checkThrough(t, i, id, wholesaleState{})
	want(t, "the deleted fact", orderFactsOf(t, id)[theOrder], "deleted", "true")
	wantCounts(t, "a deleted fact with no row", row, 0, 0)
}

// A ROW WRITTEN AFTER THE PASS BEGAN is the walk's to fold, not a mismatch:
// an order written a second before the pass, with no fact yet.
func TestARowWrittenAfterThePassBeganIsNotAMismatch(t *testing.T) {
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	settle(t, i, id)
	const late = "gid://shopify/Order/1002"
	mirrorOrder(t, id, late)
	time.Sleep(time.Second)
	row := checkThroughAt(t, i, id, wholesaleState{}, time.Now())
	if row.PassStartedAt.Valid || row.LastMismatched != 0 || !row.CleanAt.Valid {
		t.Errorf("a row written after the pass began: under way %v, mismatched %d, clean at %v; want it ended clean",
			row.PassStartedAt.Valid, row.LastMismatched, row.CleanAt)
	}
	if orderFactsOf(t, id)[late] != nil {
		t.Errorf("the check folded a row written after its pass began")
	}
}

// A PASS THAT STOPPED AT ITS BUDGET CARRIES ON AND FINISHES: a budget of one
// page and 600 orders take several calls, and nothing no row answers for is
// deleted before the last page has been read.
func TestAPassThatStoppedAtItsBudgetCarriesOnAndFinishes(t *testing.T) {
	i, id := runStore(t)
	for n := range 600 {
		mirror(t, orderConcept, id, "gid://shopify/Order/"+strconv.Itoa(20000+n), orderPayload())
	}
	settle(t, i, id)
	const orphan = "gid://shopify/Order/9001"
	orderFact(t, id, orphan, time.Now().Add(-time.Hour), false)
	at := time.Now().Add(checkAhead)
	calls := 0
	for {
		calls++
		if calls > 20 {
			t.Fatal("the pass did not end in 20 calls")
		}
		finished, err := checkStore(checkCtx(t), i, id, at, 1, wholesaleState{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if finished {
			break
		}
		if orderFactsOf(t, id)[orphan] == nil {
			t.Fatalf("call %d deleted a fact no row answers for before the pass had read every order", calls)
		}
	}
	if calls < 3 {
		t.Errorf("the pass ended in %d calls; a page a call over 600 orders takes three or more", calls)
	}
	row, _ := checkRowOf(t, id)
	if orderFactsOf(t, id)[orphan] != nil || row.LastCompared < 600 || row.LastMismatched != 1 {
		t.Errorf("the ended pass: orphan kept %v, compared %d, mismatched %d; want it deleted, 600 or more compared, one mismatched",
			orderFactsOf(t, id)[orphan] != nil, row.LastCompared, row.LastMismatched)
	}
}

// A CLEAN PASS SAYS SO AND A DIRTY ONE DOES NOT: clean_at is set only when
// nothing was mismatched and is kept from an earlier clean pass, and the last
// pass's start, end and counts are the ended pass's.
func TestACleanPassSaysSoAndADirtyOneDoesNot(t *testing.T) {
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	settle(t, i, id)
	first := time.Now().Add(checkAhead).UTC()
	clean := checkThroughAt(t, i, id, wholesaleState{}, first)
	if !clean.CleanAt.Valid || !clean.CleanAt.Time.Equal(micro(first)) || clean.LastMismatched != 0 {
		t.Fatalf("a clean pass: clean at %v, mismatched %d; want clean at %v", clean.CleanAt, clean.LastMismatched, first)
	}
	exec(t, `DELETE FROM store_sales_line_fact WHERE store_id = ? AND line_gid = ?`, id, lineTwo)
	second := first.Add(time.Minute)
	dirty := checkThroughAt(t, i, id, wholesaleState{}, second)
	if !dirty.CleanAt.Time.Equal(clean.CleanAt.Time) {
		t.Errorf("a dirty pass moved clean_at to %v, want it kept at %v", dirty.CleanAt.Time, clean.CleanAt.Time)
	}
	if !dirty.LastStartedAt.Time.Equal(micro(second)) || !dirty.LastFinishedAt.Time.Equal(micro(second)) {
		t.Errorf("the dirty pass: started %v, finished %v; want both %v", dirty.LastStartedAt.Time, dirty.LastFinishedAt.Time, second)
	}
	if dirty.LastMismatched != 1 || dirty.LastRepaired != 1 || dirty.LastCompared < 5 || dirty.LastDisagreeing != 1 {
		t.Errorf("the dirty pass: compared %d, mismatched %d, repaired %d, disagreeing %d; want 5 or more, 1, 1 and 1",
			dirty.LastCompared, dirty.LastMismatched, dirty.LastRepaired, dirty.LastDisagreeing)
	}
	if dirty.PassStartedAt.Valid || dirty.Compared != 0 || dirty.OrderCursor != "" {
		t.Errorf("the ended pass left its own columns: %+v", dirty)
	}
}

// AN ORDER WHOSE REFUNDS DO NOT ADD UP TO SHOPIFY'S TOTAL DISAGREES (D14): an
// order whose totals the facts add up to, and then a refund its total does not
// count.
func TestAnOrderWhoseRefundsDoNotAddUpToShopifysTotalDisagrees(t *testing.T) {
	i, id := runStore(t)
	mirrorOrder(t, id, theOrder, "subtotalLineItemsQuantity", float64(1), "currentSubtotalPriceSet", set("40.00", "EUR"),
		"totalDiscountsSet", set("0.00", "EUR"), "totalRefundedSet", set("0.0", "EUR"))
	mirrorLine(t, id, lineOne, theOrder, "quantity", float64(1), "currentQuantity", float64(1),
		"originalUnitPriceSet", set("40.00", "EUR"), "discountAllocations", []any{})
	settle(t, i, id)
	if row := checkThrough(t, i, id, wholesaleState{}); row.LastDisagreeing != 0 {
		t.Fatalf("an order whose totals add up: disagreeing %d, want 0", row.LastDisagreeing)
	}
	mirrorRefund(t, id, theRefund, theOrder, "totalRefundedSet", set("10.00", "EUR"), "refundLineItems", []any{})
	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
	if row := checkThroughAt(t, i, id, wholesaleState{}, time.Now().Add(checkAhead+time.Minute)); row.LastDisagreeing != 1 {
		t.Errorf("an order whose refunds do not add up to its total: disagreeing %d, want 1", row.LastDisagreeing)
	}
}

// TWO CHECKS AT ONCE LEAVE ONE ANSWER: two calls carry the same store's pass
// on together; one ends it, the other leaves the store, and the pass counts
// the one fact deleted by hand once.
func TestTwoChecksAtOnceLeaveOneAnswer(t *testing.T) {
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	settle(t, i, id)
	exec(t, `DELETE FROM store_sales_line_fact WHERE store_id = ? AND line_gid = ?`, id, lineOne)
	at := time.Now().Add(checkAhead)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ended := 0
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			finished, err := checkStore(checkCtx(t), i, id, at, checkPagesPerRun, wholesaleState{}, nil)
			if err != nil {
				t.Error(err)
			}
			if finished {
				mu.Lock()
				ended++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	row, _ := checkRowOf(t, id)
	if ended != 1 || row.PassStartedAt.Valid || row.LastMismatched != 1 || row.LastRepaired != 1 {
		t.Errorf("two checks at once: %d ended the pass, under way %v, mismatched %d, repaired %d; want one, ended, 1 and 1",
			ended, row.PassStartedAt.Valid, row.LastMismatched, row.LastRepaired)
	}
	if lineFactsOf(t, id)[lineOne] == nil {
		t.Error("the line deleted by hand was not restored")
	}
}

// A RECORD OF A ROW THAT COULD NOT BE READ whose mirror row was removed by
// hand has none after a pass; one whose row is still unreadable keeps it.
func TestAnUnreadableRowsRecordGoesWithItsRowAndStaysWhileItIsUnreadable(t *testing.T) {
	const unreadable = "gid://shopify/Order/1003"
	for _, removed := range []bool{true, false} {
		i, id := runStore(t)
		mirrorThreeLines(t, id)
		mirrorOrder(t, id, unreadable, "currencyCode", "")
		settle(t, i, id)
		if recordsOf(t, id)[sourceOrder+"/"+unreadable] == nil {
			t.Fatalf("the unreadable order has no record: %v", recordsOf(t, id))
		}
		if removed {
			exec(t, `DELETE FROM "MemoryNodes" WHERE concept = ? AND id = ?`, orderConcept, orderConcept+":"+shopify.MirrorRowID(id, unreadable))
		}
		checkThrough(t, i, id, wholesaleState{})
		kept := recordsOf(t, id)[sourceOrder+"/"+unreadable] != nil
		if kept == removed {
			t.Errorf("row removed %v: record kept %v after a pass", removed, kept)
		}
	}
}

// ---- who may call what: the check, the rebuild and the check's state ----

// payloadOfNode is the one node a handler answered, decoded.
func payloadOfNode(t *testing.T, nodes []memorynodes.MemoryNode, id, concept string) map[string]any {
	t.Helper()
	if len(nodes) != 1 || nodes[0].ID != id || nodes[0].Concept != concept {
		t.Fatalf("the handler answered %d nodes %+v, want one %s node %s", len(nodes), nodes, concept, id)
	}
	out := map[string]any{}
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// asActor is ac signed in, at client origin.
func asActor(ac *auth.AccessContext) context.Context {
	return auth.ContextWithAccess(context.Background(), ac)
}

var (
	aDeveloper = &auth.AccessContext{UserId: "dbtest-storestats-developer", Role: auth.RoleDeveloper}
	aWriter    = &auth.AccessContext{UserId: "dbtest-storestats-writer", Role: auth.RoleWriter}
)

// checksPrincipal is the check's maintenance principal, as its automation
// runs under it.
func checksPrincipal() *auth.AccessContext { return auth.MaintenanceActor("storeStatisticsCheckSweep") }

// checkPersonCtx is a person signed in with this role, as a client calls.
func checkPersonCtx(ac *auth.AccessContext) context.Context {
	claims := map[string]any{"sub": ac.UserId, "role": string(ac.Role)}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, ac)
}

// THE CHECK IS THE ENGINE'S. A cluster owner by hand, the automation's reader
// actor, the rollup's principal, the principal's id as a reader, the
// principal at client origin and nobody at all are each refused, before any
// read and before the database is reached. The principal at internal origin
// runs it and is answered with counts and the stores whose pass ended.
func TestOnlyTheChecksPrincipalRunsIt(t *testing.T) {
	internal := func(ac *auth.AccessContext) context.Context {
		return auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), ac))
	}
	engine := &contextEngine{}
	touched := false
	refusing := NewIntegration(engine, func() *bun.DB { touched = true; return nil }, quietLogger())
	for name, ctx := range map[string]context.Context{
		"a cluster owner by hand, at internal origin": internal(&auth.AccessContext{UserId: "user-owner", Role: auth.RoleOwner}),
		"the automation's reader actor":               internal(&auth.AccessContext{UserId: "system:automation:storeStatisticsCheckSweep", Role: auth.RoleReader}),
		"the rollup's principal":                      internal(auth.MaintenanceActor("storeStatisticsRollupSweep")),
		"the principal's id as a reader":              internal(&auth.AccessContext{UserId: checkPrincipal, Role: auth.RoleReader}),
		"the principal at client origin":              asActor(checksPrincipal()),
		"nobody at all, at internal origin":           auth.ContextWithInternalOrigin(context.Background()),
	} {
		nodes, err := refusing.handleCheck(ctx, nil, 0)
		if err == nil || nodes != nil || !strings.Contains(err.Error(), "store statistics check requires the engine maintenance principal") {
			t.Errorf("%s: got %d nodes and error %v, want the check's refusal", name, len(nodes), err)
		}
	}
	if engine.asked() != 0 || touched {
		t.Errorf("a refused call asked %d queries and reached for the database %v; want neither", engine.asked(), touched)
	}
	if checksPrincipal() == nil {
		t.Fatal("storeStatisticsCheckSweep is not on the maintenance list, so its automation runs as a reader and every run is refused")
	}

	// Through the engine, as the automation calls it: the builtin dispatches
	// here, and the principal at client origin is refused.
	eng, _ := statsEngine(t)
	if err := eng.AuditIntegrationExecutors(); err != nil {
		t.Fatalf("the engine's boot-time executor audit refuses this wiring: %v", err)
	}
	if _, err := eng.Execute(checkCtx(t), "builtin storeStatisticsCheck()"); err == nil ||
		!strings.Contains(err.Error(), "store statistics check requires the engine maintenance principal") {
		t.Errorf("the engine ran the check for the principal at client origin: %v", err)
	}
	if _, err := eng.Execute(auth.ContextWithInternalOrigin(checkCtx(t)), "builtin storeStatisticsCheck()"); err != nil {
		t.Errorf("the engine refused the check's principal: %v", err)
	}

	i, id := runStore(t)
	mirrorThreeLines(t, id)
	settle(t, i, id)
	ahead := time.Now().Add(checkAhead)
	i.now = func() time.Time { return ahead }
	nodes, err := i.handleCheck(auth.ContextWithInternalOrigin(checkCtx(t)), nil, 0)
	if err != nil {
		t.Fatalf("the check's principal was refused: %v", err)
	}
	rep := payloadOfNode(t, nodes, "storeStatisticsCheck", checkConcept)
	finished, _ := rep["finished"].([]any)
	if rep["stores"] == float64(0) || len(finished) != 1 || finished[0] != id || rep["compared"] == float64(0) {
		t.Errorf("the check's report is %v, want the store's pass ended and its rows compared", rep)
	}
}

// ONLY A PERSON WHO IS A CLUSTER OWNER REBUILDS: a developer and a writer are
// refused and nothing is marked; a cluster owner's rebuild raises the store's
// generation and marks it rebuilding.
func TestOnlyAPersonWhoIsAClusterOwnerRebuilds(t *testing.T) {
	i := testIntegration(t)
	id := foldStore(t)
	before := keptRow(t, id)
	for name, ac := range map[string]*auth.AccessContext{"a developer": aDeveloper, "a writer": aWriter} {
		nodes, err := i.handleRebuild(asActor(ac), map[string]any{"storeId": id}, 0)
		if err == nil || nodes != nil || err.Error() != "store statistics rebuild is for a cluster owner" {
			t.Errorf("%s: got %d nodes and error %v, want the rebuild's refusal", name, len(nodes), err)
		}
	}
	if k := keptRow(t, id); k.generation != before.generation || k.rebuildingSince.Valid || k.purging {
		t.Errorf("a refused rebuild marked the store: generation %d (was %d), rebuilding %v, purging %v",
			k.generation, before.generation, k.rebuildingSince.Valid, k.purging)
	}
	// Called by name through the engine, which has no gate of its own for it:
	// the handler's.
	eng, _ := statsEngine(t)
	call := "builtin storeStatisticsRebuild(storeId: " + langparser.QuoteString(id) + ")"
	if _, err := eng.Execute(checkPersonCtx(aDeveloper), call); err == nil || !strings.Contains(err.Error(), "store statistics rebuild is for a cluster owner") {
		t.Errorf("a developer calling the builtin by name: %v, want the rebuild's refusal", err)
	}
	if k := keptRow(t, id); k.generation != before.generation {
		t.Errorf("a refused rebuild through the engine moved the generation")
	}
	nodes, err := i.handleRebuild(ownerCtx(), map[string]any{"storeId": id}, 0)
	if err != nil {
		t.Fatalf("a cluster owner was refused: %v", err)
	}
	answer := payloadOfNode(t, nodes, "storeStatisticsRebuild", rebuildConcept)
	k := keptRow(t, id)
	if answer["storeId"] != id || answer["rebuildingSince"] == nil || k.generation == before.generation || !k.rebuildingSince.Valid || !k.purging {
		t.Errorf("a cluster owner's rebuild answered %v; row generation %d (was %d), rebuilding %v, purging %v",
			answer, k.generation, before.generation, k.rebuildingSince.Valid, k.purging)
	}
}

// A MAINTENANCE PRINCIPAL CANNOT REBUILD, though each is a cluster owner: the
// rollup's and the check's are refused with the rebuild's sentence, at either
// origin, and the generation does not move.
func TestAMaintenancePrincipalCannotRebuild(t *testing.T) {
	i := testIntegration(t)
	id := foldStore(t)
	before := keptRow(t, id).generation
	for name, ac := range map[string]*auth.AccessContext{
		"the rollup's principal": auth.MaintenanceActor("storeStatisticsRollupSweep"),
		"the check's principal":  checksPrincipal(),
	} {
		if !ac.IsClusterOwner() {
			t.Fatalf("%s is not a cluster owner, so this proves nothing", name)
		}
		for _, ctx := range []context.Context{asActor(ac), auth.ContextWithInternalOrigin(asActor(ac))} {
			nodes, err := i.handleRebuild(ctx, map[string]any{"storeId": id}, 0)
			if err == nil || nodes != nil || err.Error() != "store statistics rebuild is for a cluster owner" {
				t.Errorf("%s: got %d nodes and error %v, want the rebuild's refusal", name, len(nodes), err)
			}
		}
	}
	if after := keptRow(t, id).generation; after != before {
		t.Errorf("a maintenance principal's rebuild moved the generation from %d to %d", before, after)
	}
}

// A REBUILD TAKES EITHER FORM OF THE STORE ID: "<id>" and
// "v1:shopify:store:<id>" mark the same row.
func TestARebuildTakesEitherFormOfTheStoreId(t *testing.T) {
	i := testIntegration(t)
	id := foldStore(t)
	generation := keptRow(t, id).generation
	for _, form := range []string{"v1:shopify:store:" + id, id} {
		nodes, err := i.handleRebuild(ownerCtx(), map[string]any{"storeId": form}, 0)
		if err != nil {
			t.Fatalf("a rebuild of %q: %v", form, err)
		}
		if answer := payloadOfNode(t, nodes, "storeStatisticsRebuild", rebuildConcept); answer["storeId"] != id {
			t.Errorf("a rebuild of %q answered store %v, want %s", form, answer["storeId"], id)
		}
		k := keptRow(t, id)
		if k.generation == generation {
			t.Errorf("a rebuild of %q did not mark the store's row", form)
		}
		generation = k.generation
	}
}

// A REBUILD OF A STORE WITH NO ROW IS REFUSED with a sentence, and no row is
// made.
func TestARebuildOfAStoreWithNoRowIsRefused(t *testing.T) {
	i := testIntegration(t)
	id := "dbtest-storestats-none-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	for _, form := range []string{id, "v1:shopify:store:" + id, ""} {
		nodes, err := i.handleRebuild(ownerCtx(), map[string]any{"storeId": form}, 0)
		if err == nil || nodes != nil || err.Error() != "Store statistics has no store with that id." {
			t.Errorf("a rebuild of %q: got %d nodes and error %v, want the sentence that there is no such store", form, len(nodes), err)
		}
	}
	if storeRow(t, id) != nil {
		t.Error("a refused rebuild made the store a row")
	}
}

// checkState reads the store's check state as a cluster owner.
func checkState(t *testing.T, i *Integration, storeID string) map[string]any {
	t.Helper()
	nodes, err := i.handleCheckState(ownerCtx(), map[string]any{"storeId": storeID}, 0)
	if err != nil {
		t.Fatalf("read the check state: %v", err)
	}
	return payloadOfNode(t, nodes, "checkState", checkStateConcept)
}

// THE CHECK STATE SAYS WHEN THE PASS BEGAN, ENDED AND ENDED CLEAN: before any
// pass every time is null; under way it names when; after a clean pass its
// start, end and cleanAt, nothing mismatched; after a dirty one cleanAt is the
// earlier pass's.
func TestTheCheckStateSaysWhenThePassBeganEndedAndEndedClean(t *testing.T) {
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	if _, err := run(t, i); err != nil { // the first fill: rebuilding, no pass yet
		t.Fatal(err)
	}
	s := checkState(t, i, id)
	for _, k := range []string{"passUnderWaySince", "lastStartedAt", "lastFinishedAt", "cleanAt"} {
		if s[k] != nil {
			t.Errorf("before any pass: %s = %v, want null", k, s[k])
		}
	}
	if s["storeId"] != id || s["compared"] != float64(0) || s["mismatched"] != float64(0) {
		t.Errorf("before any pass: %v", s)
	}
	settle(t, i, id)

	began := time.Now().Add(-time.Minute).UTC()
	exec(t, `INSERT INTO store_sales_check (store_id, pass_started_at) VALUES (?, ?)
		ON CONFLICT (store_id) DO UPDATE SET pass_started_at = EXCLUDED.pass_started_at`, id, began)
	if s := checkState(t, i, id); s["passUnderWaySince"] != stampOf(micro(began)) {
		t.Errorf("a pass under way: passUnderWaySince %v, want %s", s["passUnderWaySince"], stampOf(micro(began)))
	}
	exec(t, `UPDATE store_sales_check SET pass_started_at = NULL WHERE store_id = ?`, id)

	first := time.Now().Add(checkAhead).UTC()
	checkThroughAt(t, i, id, wholesaleState{}, first)
	s = checkState(t, i, id)
	if s["passUnderWaySince"] != nil || s["lastStartedAt"] != stampOf(micro(first)) || s["lastFinishedAt"] != stampOf(micro(first)) ||
		s["cleanAt"] != stampOf(micro(first)) || s["mismatched"] != float64(0) || s["compared"] == float64(0) {
		t.Errorf("after a clean pass at %s: %v", stampOf(first), s)
	}
	exec(t, `DELETE FROM store_sales_line_fact WHERE store_id = ? AND line_gid = ?`, id, lineTwo)
	second := first.Add(time.Minute)
	checkThroughAt(t, i, id, wholesaleState{}, second)
	s = checkState(t, i, id)
	if s["lastStartedAt"] != stampOf(micro(second)) || s["cleanAt"] != stampOf(micro(first)) || s["mismatched"] != float64(1) ||
		s["repaired"] != float64(1) || s["disagreeing"] != float64(1) {
		t.Errorf("after a dirty pass at %s: %v, want cleanAt kept at %s", stampOf(second), s, stampOf(first))
	}
}

// ONLY A PERSON WHO IS A CLUSTER OWNER READS THE CHECK STATE: a developer and
// a maintenance principal are refused; either form of the store id is taken;
// a store with no row is refused.
func TestOnlyAPersonWhoIsAClusterOwnerReadsTheCheckState(t *testing.T) {
	i := testIntegration(t)
	id := foldStore(t)
	for name, ctx := range map[string]context.Context{
		"a developer":            asActor(aDeveloper),
		"the check's principal":  auth.ContextWithInternalOrigin(asActor(checksPrincipal())),
		"the rollup's principal": auth.ContextWithInternalOrigin(asActor(auth.MaintenanceActor("storeStatisticsRollupSweep"))),
	} {
		nodes, err := i.handleCheckState(ctx, map[string]any{"storeId": id}, 0)
		if err == nil || nodes != nil || err.Error() != "store statistics check state is for a cluster owner" {
			t.Errorf("%s: got %d nodes and error %v, want the check state's refusal", name, len(nodes), err)
		}
	}
	eng, _ := statsEngine(t)
	call := "builtin storeStatisticsCheckState(storeId: " + langparser.QuoteString(id) + ")"
	if _, err := eng.Execute(checkPersonCtx(aDeveloper), call); err == nil || !strings.Contains(err.Error(), "store statistics check state is for a cluster owner") {
		t.Errorf("a developer calling the builtin by name: %v, want the check state's refusal", err)
	}
	for _, form := range []string{id, "v1:shopify:store:" + id} {
		if s := checkState(t, i, form); s["storeId"] != id {
			t.Errorf("the check state of %q names store %v, want %s", form, s["storeId"], id)
		}
	}
	none := "dbtest-storestats-none-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := i.handleCheckState(ownerCtx(), map[string]any{"storeId": none}, 0); err == nil || err.Error() != "Store statistics has no store with that id." {
		t.Errorf("the check state of a store with no row: %v, want the sentence that there is no such store", err)
	}
}

// A REBUILD DELETES IN BATCHES AND ENDS WITH THE SAME FACTS AND A CLEAN
// CHECK: with a batch of two, the store's facts are deleted over several
// batches and refilled by the walk; every fact row is what it was but its
// checked_at, and the pass the walk began when the rebuild ended is clean and
// compared the store's rows.
func TestARebuildDeletesInBatchesAndEndsWithTheSameFactsAndACleanCheck(t *testing.T) {
	lower(t, &deleteBatch, 2)
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	ago := time.Now().Add(-10 * time.Minute)
	backdate(t, orderConcept, id, theOrder, ago)
	for _, l := range []string{lineOne, lineTwo, lineGift} {
		backdate(t, "v1:shopify:orderLineItem", id, l, ago)
	}
	backdate(t, "v1:shopify:refund", id, theRefund, ago)
	settle(t, i, id)
	before := snapshot(t, id)

	if _, err := i.handleRebuild(ownerCtx(), map[string]any{"storeId": id}, 0); err != nil {
		t.Fatal(err)
	}
	queries := storeSalesQueries(t)
	if _, err := run(t, i); err != nil { // the purge, in batches, and the walk that refills
		t.Fatal(err)
	}
	batches := 0
	for _, q := range queries() {
		if strings.HasPrefix(q, "DELETE FROM store_sales_line_fact") && strings.Contains(q, "ctid IN") {
			batches++
		}
	}
	if batches < 2 {
		t.Errorf("the rebuild deleted the store's three line facts in %d batches of two, want two or more", batches)
	}
	if row, found := checkRowOf(t, id); found {
		t.Errorf("a check began before the rebuild's walk had caught up: %+v", row)
	}
	if _, err := run(t, i); err != nil { // the walk at the clock: the rebuild ends, and its check begins
		t.Fatal(err)
	}
	if after := snapshot(t, id); after != before {
		t.Errorf("the facts after the rebuild differ:\nbefore %s\nafter  %s", before, after)
	}
	row, _ := checkRowOf(t, id)
	if row.PassStartedAt.Valid || !row.CleanAt.Valid || row.LastMismatched != 0 || row.LastCompared < 5 {
		t.Errorf("the rebuild's check: under way %v, clean at %v, mismatched %d, compared %d; want it ended clean over the five rows",
			row.PassStartedAt.Valid, row.CleanAt, row.LastMismatched, row.LastCompared)
	}
}

// A REBUILD DURING A PASS DROPS THE PASS: the generation raised between a
// check page's read and its write, nothing is written, and the store has no
// check row until its walk has caught up again.
func TestARebuildDuringAPassDropsThePass(t *testing.T) {
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	settle(t, i, id)
	exec(t, `DELETE FROM store_sales_line_fact WHERE store_id = ? AND line_gid = ?`, id, lineOne)
	rebuilt := false
	i.engine = &hookEngine{inner: i.engine, after: func(q string) {
		if !rebuilt && strings.Contains(q, "storeStatisticsOrdersChanged") {
			rebuilt = true
			markRebuild(t, id, time.Now())
		}
	}}
	finished, err := checkStore(checkCtx(t), i, id, time.Now().Add(checkAhead), checkPagesPerRun, wholesaleState{}, nil)
	if err != nil || finished || !rebuilt {
		t.Fatalf("a check under a rebuild: finished %v, error %v, rebuilt %v; want it left with no error", finished, err, rebuilt)
	}
	if row, found := checkRowOf(t, id); found {
		t.Errorf("the store has a check row after the rebuild: %+v", row)
	}
	if lineFactsOf(t, id)[lineOne] != nil {
		t.Error("the check wrote under the rebuild")
	}
	if finished, err := checkStore(checkCtx(t), i, id, time.Now().Add(checkAhead), checkPagesPerRun, wholesaleState{}, nil); err != nil || finished {
		t.Fatalf("a check of a store rebuilding: finished %v, error %v", finished, err)
	}
	if _, found := checkRowOf(t, id); found {
		t.Error("a store rebuilding got a check row")
	}
	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
	if _, found := checkRowOf(t, id); found {
		t.Error("a store whose rebuild's walk has not caught up got a check row")
	}
	if _, err := run(t, i); err != nil {
		t.Fatal(err)
	}
	if _, found := checkRowOf(t, id); !found {
		t.Error("the store has no check row once its rebuild's walk caught up")
	}
}

// A CHECK RUN'S DEADLINE sits at or above what a run can take once its
// budgets are spent, and below the schedule's hour. Not a database test.
func TestTheCheckRunDeadlineIsBelowTheHour(t *testing.T) {
	b := checkRunDeadlineBound()
	if b <= checkRunBudget || checkRunDeadline < b || checkRunDeadline >= time.Hour {
		t.Errorf("the check run's deadline is %s and what a run can take %s; want the deadline at or above it and under an hour", checkRunDeadline, b)
	}
}

// ==== fix round 1 ====

// A ROW WHOSE NEWEST VERSION CANNOT BE READ STILL ANSWERS FOR ITS FACT: an
// order, a line and a refund each folded readable and then rewritten to a
// version the rules refuse. The walk keeps the fact of the last readable
// version and records the row; a whole pass, whose one read hands only the
// unreadable version (the floor is an hour back), keeps both and counts
// nothing.
func TestAFactWhoseRowsNewestVersionIsUnreadableIsKeptByAPass(t *testing.T) {
	for _, c := range []struct {
		source, concept, gid string
		unread               []any
		facts                func(*testing.T, string) map[string]map[string]any
	}{
		{sourceOrder, orderConcept, theOrder, []any{"currencyCode", ""}, orderFactsOf},
		{sourceLine, "v1:shopify:orderLineItem", lineTwo, []any{"parentGid", ""}, lineFactsOf},
		{sourceRefund, "v1:shopify:refund", theRefund, []any{"parentGid", ""}, refundFactsOf},
	} {
		t.Run(c.source, func(t *testing.T) {
			floorAt(t, time.Now().Add(-time.Hour))
			i, id := runStore(t)
			mirrorThreeLines(t, id)
			settle(t, i, id)
			before := c.facts(t, id)[c.gid]
			if before == nil {
				t.Fatal("no fact after the first fill")
			}
			mirror(t, c.concept, id, c.gid, map[string]any{c.unread[0].(string): c.unread[1]})
			if _, err := run(t, i); err != nil {
				t.Fatal(err)
			}
			if recordsOf(t, id)[c.source+"/"+c.gid] == nil {
				t.Fatalf("the unreadable newest version has no record: %v", recordsOf(t, id))
			}
			row := checkThrough(t, i, id, wholesaleState{})
			if row.PassStartedAt.Valid {
				t.Fatal("the pass did not end")
			}
			if c.facts(t, id)[c.gid] == nil {
				t.Errorf("the pass deleted the fact of a %s whose row exists and is unreadable", c.source)
			}
			wantCounts(t, "an unreadable newest version", row, 0, 0)
			if recordsOf(t, id)[c.source+"/"+c.gid] == nil {
				t.Errorf("the pass removed the record of a row it met that is still unreadable")
			}
		})
	}
}
