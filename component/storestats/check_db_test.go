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

	"github.com/znasllc-io/memql/component/auth"
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
	actor := &auth.AccessContext{UserId: checkPrincipal, Role: auth.RoleOwner, Unranked: true, Synthetic: true}
	claims := map[string]any{"sub": actor.UserId, "email": actor.UserId, "role": string(actor.Role)}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, actor)
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

// factsWithoutCheck is every fact of the store, as text, less checked_at.
func factsWithoutCheck(t *testing.T, id string) string {
	t.Helper()
	all := map[string]map[string]map[string]any{
		"order":       orderFactsOf(t, id),
		"line":        lineFactsOf(t, id),
		"refund":      refundFactsOf(t, id),
		"refund_line": refundLineFactsOf(t, id),
	}
	for _, table := range all {
		for _, row := range table {
			delete(row, "checked_at")
		}
	}
	raw, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
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
			before := factsWithoutCheck(t, id)
			exec(t, "DELETE FROM "+c.table+" WHERE store_id = ? AND "+c.column+" = ?", id, c.gid)
			row := checkThrough(t, i, id, wholesaleState{})
			if after := factsWithoutCheck(t, id); after != before {
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
	before := factsWithoutCheck(t, id)
	exec(t, `UPDATE store_sales_order_fact SET order_version = order_version - interval '1 hour', shipping = 0
		WHERE store_id = ? AND order_gid = ?`, id, theOrder)
	row := checkThrough(t, i, id, wholesaleState{})
	if after := factsWithoutCheck(t, id); after != before {
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
	before := factsWithoutCheck(t, id)
	orderFact(t, id, "gid://shopify/Order/9001", time.Now().Add(-time.Hour), false)
	lineFact(t, id, "gid://shopify/LineItem/9001", theOrder)
	exec(t, `INSERT INTO store_sales_refund_fact (store_id, refund_gid, order_gid, refund_version, recorded_at, refunded_at,
		money_refunded, detail_fetched, lines_complete) VALUES (?, 'gid://shopify/Refund/9001', ?, now(), now(), now(), 19.99, true, true)`, id, theOrder)
	exec(t, `INSERT INTO store_sales_refund_line_fact (store_id, refund_gid, position, order_gid, line_gid, quantity, subtotal, tax)
		VALUES (?, 'gid://shopify/Refund/9001', 0, ?, ?, 1, 19.99, 0)`, id, theOrder, lineTwo)
	exec(t, `UPDATE store_sales_line_fact SET refunded_quantity = 1 WHERE store_id = ? AND line_gid = ?`, id, lineTwo)
	row := checkThrough(t, i, id, wholesaleState{})
	if after := factsWithoutCheck(t, id); after != before {
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
