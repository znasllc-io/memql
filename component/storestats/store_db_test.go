package storestats

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// store_db_test.go -- what the walk keeps about each store (store.go):
// the store's row in store_sales_store, a rebuild, a purge in batches, and a
// store whose data was removed. Postgres-gated, through the harness.

// storeSalesTables is every table the store statistics keep, by name. A
// store whose data was removed has no row left in any of them. A table the
// migrations add joins this list: TestEveryStoreSalesTableIsOnThePurgeList
// fails until it does.
var storeSalesTables = []string{
	"store_sales_order_fact",
	"store_sales_line_fact",
	"store_sales_refund_fact",
	"store_sales_refund_line_fact",
	"store_sales_store",
	"store_sales_watermark",
	"store_sales_check",
}

func sweepCtx(t *testing.T) context.Context {
	t.Helper()
	return walkContext(principalCtx(t, "storeStatisticsRollupSweep"))
}

// keptStore is a store of the test's own whose rows in every store_sales_
// table are deleted when the test ends.
func keptStore(t *testing.T) string {
	t.Helper()
	id := newStore(t)
	_, db := statsEngine(t)
	t.Cleanup(func() {
		for _, table := range storeSalesTables {
			_, _ = db.ExecContext(context.Background(), "DELETE FROM "+table+" WHERE store_id = ?", id)
		}
	})
	return id
}

// storeOf is the store row the run lists, as the stores read answers it.
func storeOf(id string, fields map[string]any) map[string]any {
	m := map[string]any{"id": id, "domain": id + ".myshopify.com", "name": "Store " + id}
	maps.Copy(m, fields)
	return m
}

func reason(code, sentence, concerns string, concepts ...string) map[string]any {
	cs := []any{}
	for _, c := range concepts {
		cs = append(cs, c)
	}
	return map[string]any{"code": code, "sentence": sentence, "concerns": concerns, "concepts": cs}
}

func verdictReply(state string, since time.Time, reasons ...map[string]any) map[string]any {
	rs := []any{}
	for _, r := range reasons {
		rs = append(rs, r)
	}
	return map[string]any{"state": state, "since": since.UTC().Format(time.RFC3339), "reasons": rs, "outsideConcepts": []any{}}
}

var (
	privacyOnly   = reason("privacy_request_failing", "A privacy request failed.", "noRecording")
	productsOnly  = reason("approval_outdated", "Approve the new permissions.", "concepts", "product")
	ordersFailing = reason("check_failing", "The order check is failing.", "concepts", "order")
)

// kept is what a test reads back of a store's row.
type kept struct {
	generation                   int64
	purging                      bool
	domain, name, currency, zone string
	taxesIncluded, development   sql.NullBool
	allOrders                    bool
	historySince                 sql.NullTime
	verdictState, verdictReasons string
	verdictSince                 sql.NullTime
	recordingBlocked             bool
	lastClearAt, walkedThrough   sql.NullTime
	rebuildingSince              sql.NullTime
	definition                   int
}

func keptRow(t *testing.T, id string) kept {
	t.Helper()
	_, db := statsEngine(t)
	var k kept
	err := db.QueryRowContext(context.Background(), `SELECT generation, purging, store_domain, store_name, currency, timezone,
		taxes_included, development, all_orders_readable, history_since, verdict_state, verdict_reasons::text, verdict_since,
		recording_blocked, last_clear_at, walked_through, rebuilding_since, definition FROM store_sales_store WHERE store_id = ?`, id).
		Scan(&k.generation, &k.purging, &k.domain, &k.name, &k.currency, &k.zone, &k.taxesIncluded, &k.development, &k.allOrders,
			&k.historySince, &k.verdictState, &k.verdictReasons, &k.verdictSince, &k.recordingBlocked, &k.lastClearAt,
			&k.walkedThrough, &k.rebuildingSince, &k.definition)
	if err != nil {
		t.Fatalf("read the store_sales_store row of %s: %v", id, err)
	}
	return k
}

// keep runs keepStore for the store at now and fails the test on an error.
func keep(t *testing.T, i *Integration, store map[string]any, now time.Time) string {
	t.Helper()
	outcome, err := i.keepStore(sweepCtx(t), store, now)
	if err != nil {
		t.Fatalf("keepStore(%s) at %s: %v", store["id"], now, err)
	}
	return outcome
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func exec(t *testing.T, query string, args ...any) {
	t.Helper()
	_, db := statsEngine(t)
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// orderFact writes an order fact of the store directly.
func orderFact(t *testing.T, id, gid string, orderedAt time.Time, deleted bool) {
	t.Helper()
	exec(t, `INSERT INTO store_sales_order_fact (store_id, order_gid, order_version, deleted, recorded_at, ordered_at, currency,
		cancelled, test, taxes_included, shipping, tax, duties, expected_quantity)
		VALUES (?, ?, ?, ?, ?, ?, 'EUR', false, false, true, 0, 0, 0, 1)`, id, gid, orderedAt, deleted, orderedAt, orderedAt)
}

func lineFact(t *testing.T, id, gid, orderGID string) {
	t.Helper()
	exec(t, `INSERT INTO store_sales_line_fact (store_id, line_gid, order_gid, line_version, gift_card, product_key, title,
		quantity, current_quantity, unit_price, allocated_discounts) VALUES (?, ?, ?, now(), false, 'p', 'Item', 1, 1, 10, 0)`, id, gid, orderGID)
}

// everyTableOf writes a row of the store in each of the seven tables.
func everyTableOf(t *testing.T, id string) {
	t.Helper()
	orderFact(t, id, "o-1", at("2025-10-01T10:00:00Z"), false)
	lineFact(t, id, "l-1", "o-1")
	exec(t, `INSERT INTO store_sales_refund_fact (store_id, refund_gid, order_gid, refund_version, recorded_at, refunded_at,
		money_refunded, detail_fetched, lines_complete) VALUES (?, 'r-1', 'o-1', now(), now(), now(), 0, false, false)`, id)
	exec(t, `INSERT INTO store_sales_refund_line_fact (store_id, refund_gid, position, order_gid, line_gid, quantity, subtotal, tax)
		VALUES (?, 'r-1', 0, 'o-1', 'l-1', 1, 0, 0)`, id)
	exec(t, `INSERT INTO store_sales_watermark (store_id, source, through) VALUES (?, 'order', now())`, id)
	exec(t, `INSERT INTO store_sales_check (store_id) VALUES (?)`, id)
	exec(t, `INSERT INTO store_sales_store (store_id) VALUES (?) ON CONFLICT DO NOTHING`, id)
}

// rowsOf counts the store's rows in each table named.
func rowsOf(t *testing.T, id string, tables ...string) map[string]int {
	t.Helper()
	_, db := statsEngine(t)
	out := map[string]int{}
	for _, table := range tables {
		var n int
		if err := db.QueryRowContext(context.Background(), "SELECT count(*) FROM "+table+" WHERE store_id = ?", id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

func lower(t *testing.T, v *int, n int) {
	t.Helper()
	was := *v
	*v = n
	t.Cleanup(func() { *v = was })
}

func markRebuild(t *testing.T, id string, now time.Time) {
	t.Helper()
	_, db := statsEngine(t)
	if err := writeTx(context.Background(), db, id, anyGeneration, func(ctx context.Context, tx bun.Tx) error {
		return rebuildStore(ctx, tx, id, now)
	}); err != nil {
		t.Fatal(err)
	}
}

// staleWrite writes an order fact under the generation named, through
// writeTx, as a walk's page would.
func staleWrite(t *testing.T, id string, generation int64, gid string) error {
	t.Helper()
	_, db := statsEngine(t)
	return writeTx(context.Background(), db, id, generation, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO store_sales_order_fact (store_id, order_gid, order_version, recorded_at, ordered_at,
			currency, cancelled, test, taxes_included, shipping, tax, duties, expected_quantity)
			VALUES (?, ?, now(), now(), now(), 'EUR', false, false, true, 0, 0, 0, 1)`, id, gid)
		return err
	})
}

// ---- the store's row ----

func TestTheShopsCurrencyZoneAndTaxSettingAreCopied(t *testing.T) {
	i := testIntegration(t)
	answerVerdict(i, verdictReply("recording", time.Now()))
	id := keptStore(t)
	shopRow(t, id, "EUR", "Europe/Madrid", false)
	keep(t, i, storeOf(id, nil), time.Now())
	k := keptRow(t, id)
	if k.currency != "EUR" || k.zone != "Europe/Madrid" || !k.taxesIncluded.Valid || !k.taxesIncluded.Bool {
		t.Errorf("kept currency %q, zone %q, taxes included %v; want EUR, Europe/Madrid, true", k.currency, k.zone, k.taxesIncluded)
	}
	writeShopRow(t, id, map[string]any{"currencyCode": "USD", "ianaTimezone": "America/Los_Angeles", "taxesIncluded": false})
	keep(t, i, storeOf(id, nil), time.Now())
	k = keptRow(t, id)
	if k.currency != "USD" || k.zone != "America/Los_Angeles" || !k.taxesIncluded.Valid || k.taxesIncluded.Bool {
		t.Errorf("after the shop changed, kept currency %q, zone %q, taxes included %v; want USD, America/Los_Angeles, false", k.currency, k.zone, k.taxesIncluded)
	}
}

func TestAStoreWithNoShopRowKeepsNoCurrencyAndNoZone(t *testing.T) {
	i := testIntegration(t)
	answerVerdict(i, verdictReply("recording", time.Now()))
	id := keptStore(t)
	keep(t, i, storeOf(id, nil), time.Now())
	k := keptRow(t, id)
	if k.currency != "" || k.zone != "" || k.taxesIncluded.Valid {
		t.Errorf("with no shop row, kept currency %q, zone %q, taxes included %v; want none", k.currency, k.zone, k.taxesIncluded)
	}

	shopRow(t, id, "EUR", "Europe/Madrid", false)
	keep(t, i, storeOf(id, nil), time.Now())
	// The shop row retired, with other values on its tombstone: what the
	// store had is kept, and the tombstone's values are not read.
	writeShopRow(t, id, map[string]any{"deleted": true, "currencyCode": "GBP", "ianaTimezone": "Europe/London", "taxesIncluded": false})
	keep(t, i, storeOf(id, nil), time.Now())
	k = keptRow(t, id)
	if k.currency != "EUR" || k.zone != "Europe/Madrid" || !k.taxesIncluded.Valid || !k.taxesIncluded.Bool {
		t.Errorf("after the shop row was retired, kept currency %q, zone %q, taxes included %v; want EUR, Europe/Madrid, true kept", k.currency, k.zone, k.taxesIncluded)
	}
}

func TestADevelopmentStoreIsToldFromALiveOne(t *testing.T) {
	i := testIntegration(t)
	answerVerdict(i, verdictReply("recording", time.Now()))
	cases := []struct {
		name string
		shop *bool // nil: no shop row
		flag any   // the store row's isDevelopment
		want sql.NullBool
	}{
		{"the shop's plan says development, the store row says not", ptr(true), false, sql.NullBool{Bool: true, Valid: true}},
		{"the shop's plan says live, the store row says development", ptr(false), true, sql.NullBool{Bool: false, Valid: true}},
		{"no shop row, the store row says development", nil, true, sql.NullBool{Bool: true, Valid: true}},
		{"no shop row, the store row says not", nil, false, sql.NullBool{Bool: false, Valid: true}},
		{"neither says", nil, nil, sql.NullBool{}},
	}
	for _, c := range cases {
		id := keptStore(t)
		if c.shop != nil {
			shopRow(t, id, "EUR", "Europe/Madrid", *c.shop)
		}
		fields := map[string]any{}
		if c.flag != nil {
			fields["isDevelopment"] = c.flag
		}
		keep(t, i, storeOf(id, fields), time.Now())
		if got := keptRow(t, id).development; got != c.want {
			t.Errorf("%s: development is %v, want %v", c.name, got, c.want)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestTheStoresNameAndDomainAreCopied(t *testing.T) {
	i := testIntegration(t)
	answerVerdict(i, verdictReply("recording", time.Now()))
	id := keptStore(t)
	shopRow(t, id, "EUR", "Europe/Madrid", false)
	keep(t, i, storeOf(id, nil), time.Now())
	if k := keptRow(t, id); k.name != "Store "+id || k.domain != id+".myshopify.com" {
		t.Errorf("kept name %q and domain %q, want the store row's", k.name, k.domain)
	}
	keep(t, i, storeOf(id, map[string]any{"name": ""}), time.Now())
	if k := keptRow(t, id); k.name != "Shop "+id {
		t.Errorf("with no name on the store row, kept name %q, want the shop row's", k.name)
	}
}

func TestAGrantWithoutAllOrdersIsRecorded(t *testing.T) {
	i := testIntegration(t)
	answerVerdict(i, verdictReply("recording", time.Now()))
	id := keptStore(t)
	keep(t, i, storeOf(id, map[string]any{"scopesGranted": []any{"read_orders", "read_products"}}), time.Now())
	if keptRow(t, id).allOrders {
		t.Error("a grant without read_all_orders was kept as all orders readable")
	}
	keep(t, i, storeOf(id, map[string]any{"scopesGranted": []any{"read_orders", "read_all_orders"}}), time.Now())
	if !keptRow(t, id).allOrders {
		t.Error("a grant with read_all_orders was kept as not all orders readable")
	}
}

func TestOrdersSinceIsTheStoreLocalMidnightOfTheFirstRecordedDay(t *testing.T) {
	i := testIntegration(t)
	answerVerdict(i, verdictReply("recording", time.Now()))
	withDay := map[string]any{"health": map[string]any{"ordersFrom": "2025-09-12"}}
	for zone, want := range map[string]string{
		"Europe/Madrid":       "2025-09-11T22:00:00Z",
		"America/Los_Angeles": "2025-09-12T07:00:00Z",
		"":                    "2025-09-12T00:00:00Z",
		"Mars/Olympus_Mons":   "2025-09-12T00:00:00Z", // a zone that does not load: UTC
	} {
		id := keptStore(t)
		if zone != "" {
			shopRow(t, id, "EUR", zone, false)
		}
		keep(t, i, storeOf(id, withDay), time.Now())
		if got := keptRow(t, id).historySince; !got.Valid || !got.Time.Equal(at(want)) {
			t.Errorf("ordersFrom 2025-09-12 in zone %q: history_since %v, want %s", zone, got, want)
		}
	}

	// No ordersFrom: the local midnight of the day of the oldest order that
	// is not deleted. 23:30 UTC on the 4th is 00:30 on the 5th in Madrid.
	id := keptStore(t)
	shopRow(t, id, "EUR", "Europe/Madrid", false)
	keep(t, i, storeOf(id, nil), time.Now()) // the first fill, a rebuild that deletes every fact
	orderFact(t, id, "o-deleted", at("2025-01-01T12:00:00Z"), true)
	orderFact(t, id, "o-oldest", at("2025-03-04T23:30:00Z"), false)
	orderFact(t, id, "o-later", at("2025-06-01T12:00:00Z"), false)
	keep(t, i, storeOf(id, nil), time.Now())
	if got := keptRow(t, id).historySince; !got.Valid || !got.Time.Equal(at("2025-03-04T23:00:00Z")) {
		t.Errorf("with no ordersFrom, history_since %v, want 2025-03-04T23:00:00Z (Madrid midnight of the 5th)", got)
	}
	// The zone changes: the next run moves it.
	writeShopRow(t, id, map[string]any{"currencyCode": "EUR", "ianaTimezone": "America/Los_Angeles"})
	keep(t, i, storeOf(id, nil), time.Now())
	if got := keptRow(t, id).historySince; !got.Valid || !got.Time.Equal(at("2025-03-04T08:00:00Z")) {
		t.Errorf("after the zone changed, history_since %v, want 2025-03-04T08:00:00Z (Los Angeles midnight of the 4th)", got)
	}
}

// A day that cannot be read is the store's stated problem, never the
// beginning of time.
func TestAnOrdersFromThatIsNotADayIsAStatedProblem(t *testing.T) {
	i := testIntegration(t)
	answerVerdict(i, verdictReply("recording", time.Now()))
	for _, bad := range []any{"2025-13-45", "12 September", 20250912} {
		id := keptStore(t)
		_, err := i.keepStore(sweepCtx(t), storeOf(id, map[string]any{"health": map[string]any{"ordersFrom": bad}}), time.Now())
		if err == nil || !strings.Contains(err.Error(), "ordersFrom") {
			t.Errorf("ordersFrom %v answered %v, want a stated problem naming ordersFrom", bad, err)
		}
		if row := storeRow(t, id); row != nil && row["history_since"] != nil {
			t.Errorf("ordersFrom %v kept history_since %v, want none", bad, row["history_since"])
		}
	}
}

// ---- the verdict ----

func TestTheVerdictIsCopied(t *testing.T) {
	i := testIntegration(t)
	since := at("2026-11-02T09:00:00Z")
	answerVerdict(i, verdictReply("needs attention", since, privacyOnly, ordersFailing))
	id := keptStore(t)
	keep(t, i, storeOf(id, nil), time.Now())
	k := keptRow(t, id)
	if k.verdictState != "needs attention" || !k.verdictSince.Valid || !k.verdictSince.Time.Equal(since) {
		t.Errorf("kept state %q since %v, want needs attention since %s", k.verdictState, k.verdictSince, since)
	}
	want := `[{"code": "privacy_request_failing", "sentence": "A privacy request failed."}, {"code": "check_failing", "sentence": "The order check is failing."}]`
	if k.verdictReasons != want {
		t.Errorf("kept reasons %s, want every reason's code and sentence: %s", k.verdictReasons, want)
	}
}

func TestAReasonAboutNothingTheWalkReadsKeepsTheRunClear(t *testing.T) {
	i := testIntegration(t)
	for name, r := range map[string]map[string]any{"privacy only": privacyOnly, "an approval naming product": productsOnly} {
		id := keptStore(t)
		answerVerdict(i, verdictReply("needs attention", time.Now(), r))
		now := time.Now().UTC().Truncate(time.Microsecond)
		keep(t, i, storeOf(id, nil), now)
		k := keptRow(t, id)
		if k.recordingBlocked || !k.lastClearAt.Valid || !k.lastClearAt.Time.Equal(now) {
			t.Errorf("%s: recording_blocked %v, last_clear_at %v; want false and the run's clock %s", name, k.recordingBlocked, k.lastClearAt, now)
		}
	}
}

func TestAFailingOrderCheckBlocksAndLastClearAtStays(t *testing.T) {
	i := testIntegration(t)
	id := keptStore(t)
	t1, t2 := at("2026-11-02T09:00:00Z"), at("2026-11-02T10:00:00Z")
	answerVerdict(i, verdictReply("recording", t1))
	keep(t, i, storeOf(id, nil), t1)
	answerVerdict(i, verdictReply("needs attention", t2, ordersFailing))
	keep(t, i, storeOf(id, nil), t2)
	k := keptRow(t, id)
	if !k.recordingBlocked || !k.lastClearAt.Valid || !k.lastClearAt.Time.Equal(t1) {
		t.Errorf("recording_blocked %v, last_clear_at %v; want true and %s", k.recordingBlocked, k.lastClearAt, t1)
	}
}

func TestTheLastClearRunIsTheWalksNotTheVerdictsSince(t *testing.T) {
	i := testIntegration(t)
	id := keptStore(t)
	monday := at("2026-11-02T09:00:00Z")
	answerVerdict(i, verdictReply("needs attention", monday, privacyOnly))
	var lastBefore time.Time
	for run := monday; run.Before(at("2026-11-04T10:00:00Z")); run = run.Add(7 * time.Hour) {
		keep(t, i, storeOf(id, nil), run)
		lastBefore = run
	}
	answerVerdict(i, verdictReply("needs attention", monday, privacyOnly, ordersFailing))
	keep(t, i, storeOf(id, nil), at("2026-11-04T10:00:00Z"))
	keep(t, i, storeOf(id, nil), at("2026-11-04T11:00:00Z"))
	k := keptRow(t, id)
	if !k.recordingBlocked || !k.lastClearAt.Valid || !k.lastClearAt.Time.Equal(lastBefore) {
		t.Errorf("recording_blocked %v, last_clear_at %v; want true and Wednesday's last run before 10:00, %s, not Monday", k.recordingBlocked, k.lastClearAt, lastBefore)
	}
}

func TestAPausedOrNotConnectedStoreIsBlocked(t *testing.T) {
	i := testIntegration(t)
	t1, t2 := at("2026-11-02T09:00:00Z"), at("2026-11-02T10:00:00Z")
	for _, state := range []string{"paused", "not connected"} {
		id := keptStore(t)
		answerVerdict(i, verdictReply("recording", t1))
		keep(t, i, storeOf(id, nil), t1)
		answerVerdict(i, verdictReply(state, t2))
		keep(t, i, storeOf(id, nil), t2)
		k := keptRow(t, id)
		if !k.recordingBlocked || !k.lastClearAt.Valid || !k.lastClearAt.Time.Equal(t1) {
			t.Errorf("%s: recording_blocked %v, last_clear_at %v; want true and %s", state, k.recordingBlocked, k.lastClearAt, t1)
		}
	}
}

func TestAStartingStoreMovesNothing(t *testing.T) {
	i := testIntegration(t)
	id := keptStore(t)
	answerVerdict(i, verdictReply("starting", time.Now()))
	keep(t, i, storeOf(id, nil), time.Now())
	if k := keptRow(t, id); k.recordingBlocked || k.lastClearAt.Valid || k.verdictState != "starting" {
		t.Errorf("starting: state %q, recording_blocked %v, last_clear_at %v; want starting, false, null", k.verdictState, k.recordingBlocked, k.lastClearAt)
	}
}

func TestAVerdictThatCannotBeReadIsNotKnown(t *testing.T) {
	i := testIntegration(t)
	id := keptStore(t)
	t1 := at("2026-11-02T09:00:00Z")
	answerVerdict(i, verdictReply("recording", t1))
	keep(t, i, storeOf(id, nil), t1)
	answerVerdict(i, nil)
	keep(t, i, storeOf(id, nil), t1.Add(time.Hour))
	k := keptRow(t, id)
	if k.verdictState != "" || k.recordingBlocked || !k.lastClearAt.Valid || !k.lastClearAt.Time.Equal(t1) {
		t.Errorf("unreadable: state %q, recording_blocked %v, last_clear_at %v; want \"\", false, %s not moved", k.verdictState, k.recordingBlocked, k.lastClearAt, t1)
	}
	// And the default reading: a malformed reply is not read as clear.
	for name, reply := range map[string]map[string]any{
		"no state":           {"since": t1.Format(time.RFC3339), "reasons": []any{}, "outsideConcepts": []any{}},
		"a since not a time": {"state": "recording", "since": "yesterday", "reasons": []any{}, "outsideConcepts": []any{}},
		"reasons not a list": {"state": "recording", "since": t1.Format(time.RFC3339), "reasons": "none", "outsideConcepts": []any{}},
		"a reason not a map": {"state": "recording", "since": t1.Format(time.RFC3339), "reasons": []any{"x"}, "outsideConcepts": []any{}},
		"no reasons at all":  {"state": "recording", "since": t1.Format(time.RFC3339), "outsideConcepts": []any{}},
	} {
		if _, err := verdictFromReply(reply); err == nil {
			t.Errorf("%s: the reply was read as a verdict, want it unreadable", name)
		}
	}
}

// WHAT FREEZES THE FIGURES, AND WHAT DOES NOT. Each reply is recording or
// needs attention; the store's kept state says frozen exactly when what is in
// the way is something the walk reads.
func TestOnlyWhatStandsInTheWayOfTheWalkedConceptsFreezes(t *testing.T) {
	i := testIntegration(t)
	t1, t2 := at("2026-11-02T09:00:00Z"), at("2026-11-02T10:00:00Z")
	withOutside := func(outside ...any) map[string]any {
		r := verdictReply("recording", t2)
		r["outsideConcepts"] = outside
		return r
	}
	older := verdictReply("recording", t2)
	delete(older, "outsideConcepts")
	cases := []struct {
		name   string
		reply  map[string]any
		frozen bool
	}{
		{"a reason on orders", verdictReply("needs attention", t2, ordersFailing), true},
		{"a reason on order lines", verdictReply("needs attention", t2, reason("check_failing", "s", "concepts", "orderLineItem")), true},
		{"a reason on the whole store", verdictReply("needs attention", t2, reason("grant_lost", "s", "wholeStore")), true},
		{"a reason on products", verdictReply("needs attention", t2, productsOnly), false},
		{"a privacy reason", verdictReply("needs attention", t2, privacyOnly), false},
		{"orders outside what the store records", withOutside("order"), true},
		{"products outside what the store records", withOutside("product"), false},
		{"an older node's reply, with no outsideConcepts", older, true},
	}
	for _, c := range cases {
		id := keptStore(t)
		answerVerdict(i, verdictReply("recording", t1))
		keep(t, i, storeOf(id, nil), t1)
		answerVerdict(i, c.reply)
		keep(t, i, storeOf(id, nil), t2)
		k := keptRow(t, id)
		wantClear := t2
		if c.frozen {
			wantClear = t1
		}
		if k.recordingBlocked != c.frozen || !k.lastClearAt.Valid || !k.lastClearAt.Time.Equal(wantClear) {
			t.Errorf("%s: recording_blocked %v, last_clear_at %v; want %v and %s", c.name, k.recordingBlocked, k.lastClearAt, c.frozen, wantClear)
		}
	}
	// A verdict that cannot be read is not clear either: last_clear_at stays.
	id := keptStore(t)
	answerVerdict(i, verdictReply("recording", t1))
	keep(t, i, storeOf(id, nil), t1)
	answerVerdict(i, nil)
	keep(t, i, storeOf(id, nil), t2)
	if k := keptRow(t, id); !k.lastClearAt.Time.Equal(t1) || k.verdictState != "" {
		t.Errorf("unreadable: last_clear_at %v, state %q; want %s and not known", k.lastClearAt, k.verdictState, t1)
	}
}

// ---- a rebuild ----

func TestALowerDefinitionRebuilds(t *testing.T) {
	i := testIntegration(t)
	answerVerdict(i, verdictReply("recording", time.Now()))
	id := keptStore(t)
	// The walk: writes the store's two facts under the generation it was
	// handed, and walks through the instant it is set to.
	var through time.Time
	walked := 0
	i.walkStore = func(ctx context.Context, store map[string]any, generation int64, _ time.Time, _ wholesaleState, _ *RollupReport) error {
		if store["id"] != id {
			return nil
		}
		walked++
		_, db := statsEngine(t)
		return writeTx(ctx, db, id, generation, func(ctx context.Context, tx bun.Tx) error {
			for _, gid := range []string{"o-1", "o-2"} {
				if _, err := tx.ExecContext(ctx, `INSERT INTO store_sales_order_fact (store_id, order_gid, order_version, recorded_at,
					ordered_at, currency, cancelled, test, taxes_included, shipping, tax, duties, expected_quantity)
					VALUES (?, ?, '2025-10-01T10:00:00Z', '2025-10-01T10:00:00Z', '2025-10-01T10:00:00Z', 'EUR', false, false, true, 0, 0, 0, 1)
					ON CONFLICT DO NOTHING`, id, gid); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(ctx, `UPDATE store_sales_store SET walked_through = ? WHERE store_id = ?`, through, id)
			return err
		})
	}
	facts := func() []string {
		_, db := statsEngine(t)
		var gids []string
		if err := db.NewSelect().Table("store_sales_order_fact").Column("order_gid").Where("store_id = ?", id).Order("order_gid").Scan(context.Background(), &gids); err != nil {
			t.Fatal(err)
		}
		return gids
	}

	// The first fill is a rebuild with nothing to delete.
	now := time.Now().UTC().Truncate(time.Microsecond)
	through = now
	rep, err := runRollup(principalCtx(t, "storeStatisticsRollupSweep"), i, now)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rep.Rebuilt, id) {
		t.Errorf("the first fill did not report the store rebuilt: %v", rep.Rebuilt)
	}
	k := keptRow(t, id)
	if k.definition != storeStatsDefinition || k.generation != 1 || k.rebuildingSince.Valid || k.purging || walked != 1 {
		t.Errorf("after the first fill: definition %d, generation %d, rebuilding_since %v, purging %v, walked %d; want %d, 1, cleared, false, 1",
			k.definition, k.generation, k.rebuildingSince, k.purging, walked, storeStatsDefinition)
	}
	before := facts()

	// An older definition: rebuilt, and while the walk is behind the clock
	// the rebuild is not over.
	exec(t, `UPDATE store_sales_store SET definition = 0 WHERE store_id = ?`, id)
	now = now.Add(time.Minute)
	through = now.Add(-time.Hour)
	rep, err = runRollup(principalCtx(t, "storeStatisticsRollupSweep"), i, now)
	if err != nil {
		t.Fatal(err)
	}
	k = keptRow(t, id)
	if !slices.Contains(rep.Rebuilt, id) || k.generation != 2 || !k.rebuildingSince.Valid || !k.rebuildingSince.Time.Equal(now) || k.definition != storeStatsDefinition {
		t.Errorf("definition 0: rebuilt %v, generation %d, rebuilding_since %v, definition %d; want rebuilt, 2, %s, %d",
			rep.Rebuilt, k.generation, k.rebuildingSince, k.definition, now, storeStatsDefinition)
	}
	if got := facts(); !slices.Equal(got, before) {
		t.Errorf("after the rebuild the facts are %v, want the same as before, %v", got, before)
	}

	// The walk reaches the clock: the rebuild is over.
	now = now.Add(time.Minute)
	through = now.Add(-4 * time.Minute)
	if _, err := runRollup(principalCtx(t, "storeStatisticsRollupSweep"), i, now); err != nil {
		t.Fatal(err)
	}
	if k := keptRow(t, id); k.rebuildingSince.Valid || k.generation != 2 {
		t.Errorf("once the walk was within five minutes of the clock, rebuilding_since %v and generation %d, want cleared and 2", k.rebuildingSince, k.generation)
	}
}

// WHAT A REBUILD CLEARS AND WHAT IT KEEPS, column by column: it raises
// generation, sets purging and rebuilding_since, clears walked_through, sets
// definition, and deletes the watermarks and the check row. Every other
// column of the store's row, and every fact, is as it was.
func TestARebuildClearsWhatItShouldAndKeepsTheRest(t *testing.T) {
	_, db := statsEngine(t)
	id := keptStore(t)
	everyTableOf(t, id)
	exec(t, `UPDATE store_sales_store SET generation = 4, store_domain = 'd.example.com', store_name = 'N', currency = 'EUR',
		timezone = 'Europe/Madrid', taxes_included = true, development = false, all_orders_readable = true,
		history_floor = '2025-01-01T00:00:00Z', history_since = '2025-01-02T00:00:00Z', wholesale_enabled = true,
		wholesale_digest = 'dg', wholesale_cursor = 'cur', wholesale_window_seconds = 600, verdict_state = 'recording',
		verdict_reasons = '[{"code":"x","sentence":"y"}]', verdict_since = '2025-01-03T00:00:00Z', recording_blocked = true,
		last_clear_at = '2025-01-04T00:00:00Z', last_change_at = '2025-01-05T00:00:00Z', walked_through = '2025-01-06T00:00:00Z',
		definition = 0 WHERE store_id = ?`, id)
	before := storeRow(t, id)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := writeTx(context.Background(), db, id, anyGeneration, func(ctx context.Context, tx bun.Tx) error {
		return rebuildStore(ctx, tx, id, now)
	}); err != nil {
		t.Fatal(err)
	}
	after := storeRow(t, id)
	changed := map[string]bool{"generation": true, "purging": true, "rebuilding_since": true, "walked_through": true, "definition": true}
	if after["generation"] != int64(5) || after["purging"] != true || after["walked_through"] != nil || after["definition"] != int64(storeStatsDefinition) {
		t.Errorf("after a rebuild: generation %v, purging %v, walked_through %v, definition %v; want 5, true, null, %d",
			after["generation"], after["purging"], after["walked_through"], after["definition"], storeStatsDefinition)
	}
	if since, ok := after["rebuilding_since"].(time.Time); !ok || !since.Equal(now) {
		t.Errorf("after a rebuild rebuilding_since is %v, want %s", after["rebuilding_since"], now)
	}
	cols := slices.Sorted(maps.Keys(before))
	for _, col := range cols {
		if changed[col] {
			continue
		}
		if fmt.Sprint(before[col]) != fmt.Sprint(after[col]) {
			t.Errorf("a rebuild changed %s from %v to %v, want it kept", col, before[col], after[col])
		}
	}
	got := rowsOf(t, id, storeSalesTables...)
	want := map[string]int{"store_sales_order_fact": 1, "store_sales_line_fact": 1, "store_sales_refund_fact": 1,
		"store_sales_refund_line_fact": 1, "store_sales_store": 1, "store_sales_watermark": 0, "store_sales_check": 0}
	if !maps.Equal(got, want) {
		t.Errorf("after a rebuild the store's rows are %v, want %v: facts kept, watermarks and check row gone", got, want)
	}
}

// ---- a purge ----

func TestAStoresFactsAreDeletedInBatchesBeforeItIsWalkedAgain(t *testing.T) {
	i := testIntegration(t)
	answerVerdict(i, verdictReply("recording", time.Now()))
	lower(t, &deleteBatch, 2)
	id := keptStore(t)
	seed := func() {
		for n := 1; n <= 5; n++ {
			gid := fmt.Sprintf("o-%d", n)
			orderFact(t, id, gid, at("2025-10-01T10:00:00Z"), false)
			lineFact(t, id, fmt.Sprintf("l-%d", n), gid)
		}
	}
	keep(t, i, storeOf(id, nil), time.Now()) // the first fill: definition and generation 1
	seed()

	// The whole purge, in one call: five rows of each table in batches of
	// two is three transactions.
	markRebuild(t, id, time.Now())
	queries := storeSalesQueries(t)
	done, err := purgeStore(context.Background(), i, id, purgeBudget)
	if err != nil || !done {
		t.Fatalf("purgeStore answered done %v, error %v; want done", done, err)
	}
	locks := 0
	for _, q := range queries() {
		if strings.HasPrefix(q, "LOCK TABLE store_sales_store") {
			locks++
		}
	}
	if locks != 3 {
		t.Errorf("the purge took %d transactions, want 3", locks)
	}
	if n := rowsOf(t, id, factTables...); n["store_sales_order_fact"]+n["store_sales_line_fact"] != 0 || keptRow(t, id).purging {
		t.Errorf("after the purge the store has %v facts and purging %v, want none and cleared", n, keptRow(t, id).purging)
	}

	// Across runs: one transaction a run.
	seed()
	old := keptRow(t, id).generation
	markRebuild(t, id, time.Now())
	lower(t, &purgeBudget, 1)
	rec := &walkRecorder{}
	i.walkStore = rec.walk
	if done, err := purgeStore(context.Background(), i, id, 1); err != nil || done {
		t.Fatalf("the first batch answered done %v, error %v; want not done", done, err)
	}
	if err := staleWrite(t, id, old, "o-stale"); !errors.Is(err, errRebuilt) {
		t.Errorf("a page read before the purge began wrote with %v, want errRebuilt", err)
	}
	for run, left := range []int{1, 0} {
		rep, err := runRollup(principalCtx(t, "storeStatisticsRollupSweep"), i, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		n := rowsOf(t, id, "store_sales_order_fact", "store_sales_line_fact")
		if n["store_sales_order_fact"] != left || n["store_sales_line_fact"] != left {
			t.Errorf("run %d left %v, want %d of each", run+1, n, left)
		}
		if run == 0 && rec.count(id) != 0 {
			t.Errorf("a run walked the store while its purge was under way")
		}
		if run == 1 && rec.count(id) != 1 {
			t.Errorf("the run that finished the purge walked the store %d times, want once", rec.count(id))
		}
		_ = rep
	}
	if n := rowsOf(t, id, "store_sales_order_fact"); n["store_sales_order_fact"] != 0 {
		t.Errorf("a fact was written while purging was set: %v", n)
	}
}

func TestAPurgeAnotherNodeFinishedDeletesNothingMore(t *testing.T) {
	i := testIntegration(t)
	lower(t, &deleteBatch, 2)
	id := keptStore(t)
	exec(t, `INSERT INTO store_sales_store (store_id, definition) VALUES (?, 1) ON CONFLICT DO NOTHING`, id)
	for n := 1; n <= 5; n++ {
		orderFact(t, id, fmt.Sprintf("o-%d", n), at("2025-10-01T10:00:00Z"), false)
	}
	markRebuild(t, id, time.Now())
	if done, err := purgeStore(context.Background(), i, id, 1); err != nil || done {
		t.Fatalf("the first batch answered done %v, error %v", done, err)
	}
	// Another node finishes the purge and the walk refills a fact.
	exec(t, `UPDATE store_sales_store SET purging = false WHERE store_id = ?`, id)
	orderFact(t, id, "o-refilled", at("2025-10-02T10:00:00Z"), false)
	done, err := purgeStore(context.Background(), i, id, 1)
	if err != nil || !done {
		t.Fatalf("the next batch answered done %v, error %v; want done", done, err)
	}
	if n := rowsOf(t, id, "store_sales_order_fact")["store_sales_order_fact"]; n != 4 {
		t.Errorf("the store has %d facts after a batch of a purge another node finished, want the 4 there were", n)
	}
}

func TestAStoreWhoseDataWasRemovedLosesItsRowsAndGetsNoNewOne(t *testing.T) {
	eng, _ := statsEngine(t)
	i := testIntegration(t)
	answerVerdict(i, verdictReply("recording", time.Now()))
	rec := &walkRecorder{}
	i.walkStore = rec.walk
	redact := func(id string) {
		call := fmt.Sprintf(`mutation markStoreRedacted(storeId: %s, redactedAt: %s)`, langparser.QuoteString(id),
			langparser.QuoteString(time.Now().UTC().Format(time.RFC3339)))
		if _, err := eng.Execute(auth.ContextWithInternalOrigin(ownerCtx()), call); err != nil {
			t.Fatalf("mark %s redacted: %v", id, err)
		}
	}

	id := keptStore(t)
	keep(t, i, storeOf(id, nil), time.Now()) // the first fill
	everyTableOf(t, id)
	orderFact(t, id, "o-2", at("2025-10-01T10:00:00Z"), false)
	lineFact(t, id, "l-2", "o-2")
	old := keptRow(t, id).generation
	lower(t, &deleteBatch, 1)
	lower(t, &purgeBudget, 1)
	never := keptStore(t) // removed before it ever had a row
	redact(id)
	redact(never)

	// One batch a run: the purge is interrupted, and each run carries it on.
	removed := false
	for run := 1; run <= 10 && !removed; run++ {
		rep, err := runRollup(principalCtx(t, "storeStatisticsRollupSweep"), i, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if run == 1 {
			if err := staleWrite(t, id, old, "o-stale"); !errors.Is(err, errRebuilt) {
				t.Errorf("a page read before the purge began wrote with %v, want errRebuilt", err)
			}
			if storeRow(t, id) == nil {
				t.Fatal("one batch a run, and the first run left no row: the purge did not take batches")
			}
		}
		if !slices.Contains(rep.Removed, never) || storeRow(t, never) != nil {
			t.Errorf("run %d: a removed store with no row got one (%v) or was not reported removed (%v)", run, storeRow(t, never), rep.Removed)
		}
		removed = slices.Contains(rep.Removed, id)
	}
	if !removed {
		t.Fatal("the store was never reported removed")
	}
	for table, n := range rowsOf(t, id, storeSalesTables...) {
		if n != 0 {
			t.Errorf("the removed store has %d rows left in %s", n, table)
		}
	}
	if rec.count(id) != 0 || rec.count(never) != 0 {
		t.Errorf("a removed store was walked: %d and %d times", rec.count(id), rec.count(never))
	}
	// The next run makes no row for it.
	if _, err := runRollup(principalCtx(t, "storeStatisticsRollupSweep"), i, time.Now()); err != nil {
		t.Fatal(err)
	}
	if storeRow(t, id) != nil {
		t.Error("the run after the removal made the store a new row")
	}
}

// THE PURGE LIST NAMES EVERY TABLE. Every store_sales_ table the migrations
// create is in storeSalesTables, which the removal test asserts empty, and
// the four fact tables purgeStore deletes in batches are among them. Not a
// database test.
func TestEveryStoreSalesTableIsOnThePurgeList(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "database", "memory-nodes", "migrations", "*.up.sql"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no migrations found (%v)", err)
	}
	create := regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?(store_sales_[a-z0-9_]+)`)
	var found []string
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range create.FindAllStringSubmatch(string(src), -1) {
			found = append(found, strings.ToLower(m[1]))
		}
	}
	sort.Strings(found)
	found = slices.Compact(found)
	if len(found) < 7 {
		t.Fatalf("found %v in the migrations, want at least the seven", found)
	}
	for _, table := range found {
		if !slices.Contains(storeSalesTables, table) {
			t.Errorf("the migrations create %s, which storeSalesTables does not name: a removed store's rows in it would be left", table)
		}
	}
	for _, table := range factTables {
		if !slices.Contains(found, table) {
			t.Errorf("purgeStore deletes from %s, which no migration creates", table)
		}
	}
}
