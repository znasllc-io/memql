package storestats

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/uptrace/bun"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

func TestAMissingFrameDistinguishesErasedDisconnectedAndStartingStores(t *testing.T) {
	for _, c := range []struct {
		name, marker, state string
	}{
		{"never started", "", "starting"},
		{"erased", "redactedAt", "not connected"},
		{"uninstalled", "uninstalledAt", "not connected"},
	} {
		t.Run(c.name, func(t *testing.T) {
			id := newStore(t)
			if c.marker != "" {
				change, _ := json.Marshal(map[string]any{c.marker: time.Now().UTC().Format(time.RFC3339)})
				exec(t, `UPDATE "MemoryNodes" SET payload = payload || ?::jsonb WHERE concept = 'v1:shopify:store' AND id = ?`, string(change), "v1:shopify:store:"+id)
			}
			site := siteFor(t, id, nil)
			answer := mustAsk(t, memberCtx(t, site, true), site, id, "7d")
			state := recordingOf(t, answer)
			if state["state"] != c.state || state["blocked"] != (c.state == "not connected") {
				t.Fatalf("missing frame state %v, want %s", state, c.state)
			}
			assertEightNulls(t, "a missing frame", answer)
			for _, field := range []string{"adminTokenRef", "redactedAt", "uninstalledAt", "health"} {
				if _, found := answer[field]; found {
					t.Errorf("answer carries the connection field %s", field)
				}
			}
		})
	}
}

func TestConnectionMarkersAreReadOnlyAfterTheWholeSalesGate(t *testing.T) {
	id := newStore(t)
	site := siteFor(t, id, nil)
	other := newStore(t)
	eng, db := statsEngine(t)
	rec := &recordingEngine{MemQLEngine: eng}
	i := NewIntegration(rec, func() *bun.DB { return db }, quietLogger())
	holder := memberCtx(t, site, true)
	for _, c := range []struct {
		name, site, store string
		ctx               context.Context
	}{
		{"no part", site, id, memberCtx(t, site, false)},
		{"another store", site, other, holder},
		{"missing site", "absent", id, holder},
		{"borrowed authority", site, id, auth.ContextWithUserActor(context.Background(), "statistics-holder")},
	} {
		rec.calls = nil
		if _, err := i.handleStatistics(c.ctx, map[string]any{"siteId": c.site, "storeId": c.store, "period": "7d"}, 0); err != errNotServed {
			t.Errorf("%s: got %v, want refusal", c.name, err)
		}
		for _, call := range rec.calls {
			if strings.Contains(call.query, "storeStatisticsConnection") {
				t.Errorf("%s read connection markers before admission", c.name)
			}
		}
	}
	rec.calls = nil
	if _, err := i.handleStatistics(holder, map[string]any{"siteId": site, "storeId": id, "period": "7d"}, 0); err != nil {
		t.Fatal(err)
	}
	connections := 0
	for _, call := range rec.calls {
		if strings.Contains(call.query, "storeStatisticsConnection") {
			connections++
			ac, _ := auth.AccessFromContext(call.ctx)
			if !auth.OriginFromContext(call.ctx).IsInternal() || ac == nil || ac.UserId != auth.MaintenanceActor("storeStatisticsRollupSweep").UserId {
				t.Error("admitted connection read did not carry its maintenance authority")
			}
		}
	}
	if connections != 1 {
		t.Fatalf("admitted missing frame made %d connection reads, want one", connections)
	}
}

func TestArrivingOrdersKeepTheStatisticsStartingBesideAnotherReason(t *testing.T) {
	reply := verdictReply("needs attention", madridNow, privacyOnly)
	reply["arrivingConcepts"] = []any{"order"}
	v, err := verdictFromReply(reply)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "starting" || v.clear() || len(v.Reasons) != 1 {
		t.Fatalf("partial history read as current: %+v", v)
	}
}
func TestTheReadMarksOldFiguresAsOfTheirLastWalk(t *testing.T) {
	for _, age := range []time.Duration{14 * time.Minute, 16 * time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			id := keptStore(t)
			walked := madridNow.Add(-age)
			storeFrame(t, id, recordingFrame(map[string]any{"walked_through": walked}))
			_, db := statsEngine(t)
			var s Statistics
			err := readTx(context.Background(), db, func(ctx context.Context, tx bun.Tx) error {
				var err error
				s, err = readFrame(ctx, tx, statisticsRequest{StoreID: id, Period: "7d"}, madridNow)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if age > 15*time.Minute {
				if !s.Recording.Blocked || s.AsOf == nil || !s.AsOf.Equal(walked) {
					t.Fatalf("stale figures read current: %+v", s)
				}
			} else if s.Recording.Blocked || s.AsOf != nil {
				t.Fatalf("recent figures frozen: %+v", s)
			}
		})
	}
}

func TestAStoppedWalkEventuallyAnswersItsLastFiguresAsOf(t *testing.T) {
	for _, failure := range []string{"wholesale", "listing"} {
		t.Run(failure, func(t *testing.T) {
			i, id := runStore(t)
			mirrorThreeLines(t, id)
			settle(t, i, id)
			walked := keptRow(t, id).walkedThrough.Time
			if failure == "wholesale" {
				i.wholesaleFor = func(context.Context, []string) (map[string]wholesaleState, error) {
					return nil, errors.New("test wholesale state unavailable")
				}
			} else {
				i.engine = &pagesWithoutEnd{}
			}
			later := walked.Add(16 * time.Minute)
			if _, err := runRollup(principalCtx(t, "storeStatisticsRollupSweep"), i, later); err == nil {
				t.Fatal("the stopped walk unexpectedly succeeded")
			}
			// The missing provider's unrelated verdict is not what freezes this
			// answer; preserve a readable verdict and let walk age decide it.
			exec(t, `UPDATE store_sales_store SET verdict_state = 'recording', recording_blocked = false, verdict_reasons = '[]'::jsonb WHERE store_id = ?`, id)
			_, db := statsEngine(t)
			var stats Statistics
			if err := readTx(context.Background(), db, func(ctx context.Context, tx bun.Tx) error {
				var err error
				stats, err = readFrame(ctx, tx, statisticsRequest{StoreID: id, Period: "7d"}, later)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if !stats.Recording.Blocked || stats.AsOf == nil || !stats.AsOf.Equal(walked) {
				t.Fatalf("%s stopped the walk but figures remain current: %+v", failure, stats.Recording)
			}
		})
	}
}

func TestATurnThatStopsBehindIsNotCaughtUp(t *testing.T) {
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	settle(t, i, id)
	clock := &budgetClock{at: time.Now()}
	useClock(t, clock)
	ctx := withTurn(principalCtx(t, "storeStatisticsRollupSweep"))
	clock.advance(turnBudget + time.Second)
	if err := i.walkOneStore(ctx, storeOf(id, nil), keptRow(t, id).generation, time.Now(), wholesaleState{}, &RollupReport{}); err != nil {
		t.Fatal(err)
	}
	_, _, ok, err := checkable(context.Background(), i.bunDB(), id, time.Now().Add(40*time.Minute))
	if err != nil || ok {
		t.Fatalf("a turn that stopped before its first source remained caught up: %v, %v", ok, err)
	}
}

func TestAnEntirelyEmptyCheckSourcePreservesFactsAndCannotFinishClean(t *testing.T) {
	i, id := runStore(t)
	mirrorThreeLines(t, id)
	settle(t, i, id)
	before, _ := checkRowOf(t, id)
	exec(t, `DELETE FROM "MemoryNodes" WHERE concept = 'v1:shopify:order' AND payload->>'storeId' = ?`, id)
	logs := captureLogs(i)
	after := checkThrough(t, i, id, wholesaleState{})
	if orderFactsOf(t, id)[theOrder] == nil {
		t.Fatal("the empty source deleted its existing fact")
	}
	if after.CleanAt != before.CleanAt {
		t.Fatalf("empty source advanced clean_at: before %v after %v", before.CleanAt, after.CleanAt)
	}
	warnings := 0
	for _, line := range logs() {
		if strings.Contains(line, "check met no rows for a source with facts") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("the incomplete pass logged %d warnings, want one", warnings)
	}
}

func TestARebuildEndsInTheTurnThatCaughtUp(t *testing.T) {
	for _, spacing := range []time.Duration{6 * time.Minute, 30 * time.Minute} {
		t.Run(spacing.String(), func(t *testing.T) {
			i, id := runStore(t)
			mirrorThreeLines(t, id)
			now := time.Now().UTC()
			i.now = func() time.Time { return now }
			if _, err := run(t, i); err != nil {
				t.Fatal(err)
			}
			if keptRow(t, id).rebuildingSince.Valid {
				t.Fatal("caught-up first fill remains rebuilding until a later turn")
			}
			now = now.Add(spacing)
			if _, err := run(t, i); err != nil {
				t.Fatal(err)
			}
			_, _, ok, err := checkable(context.Background(), i.bunDB(), id, now.Add(40*time.Minute))
			if err != nil || !ok {
				t.Fatalf("caught-up store skipped by a later check: %v, %v", ok, err)
			}
		})
	}
}

func TestUnreadRefundsSeparateOldOrdersFromPendingDetail(t *testing.T) {
	cutoff := nov(1)
	id, site := salesStore(t, map[string]any{"refund_detail_from": cutoff})
	for n := 1; n <= 2; n++ {
		sale := one(n, nov(3), "10.00")
		sell(t, id, sale)
		order := orderRowOfSale(id, sale)
		if n == 1 {
			order["sourceCreatedAt"] = stampOf(cutoff.Add(-time.Hour))
		}
		fold(t, id, "order", order)
		row := refundRowOfSpec(id, refund{gid: refundGID(n), order: orderGID(n), at: nov(4), amount: "1.00"})
		delete(row, "refundLineItems")
		delete(row, "refundShippingLines")
		delete(row, "orderAdjustments")
		fold(t, id, "refund", row)
	}
	answer := ask(t, id, site)
	if js(answer["refundsTooOldToRead"]) != "1" || js(answer["refundsNotRead"]) != "1" {
		t.Fatalf("refund reachability counts: %s", js(answer))
	}
	for _, key := range []string{"refunds", "netSales", "totalSales", "unitsSold"} {
		if figuresIn(t, answer, "figures")[key] != nil {
			t.Errorf("%s was invented while detail is absent", key)
		}
	}
}

func TestRefundReachabilityUsesTheCompletedScansSixtyDayOrderWindow(t *testing.T) {
	i := testIntegration(t)
	id := keptStore(t)
	began := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		all   bool
		stamp any
		known bool
	}{
		{"limited connection", false, stampOf(began), true},
		{"all orders readable", true, stampOf(began), false},
		{"scan has not completed", false, nil, false},
		{"unreadable bookkeeping", false, "not a time", false},
	} {
		store := storeOf(id, map[string]any{"health": map[string]any{"refundDetailFrom": c.stamp}})
		if c.all {
			store["scopesGranted"] = []string{allOrdersScope}
		}
		k, err := i.readKept(sweepCtx(t), store, id)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if (k.refundDetailFrom != nil) != c.known {
			t.Fatalf("%s: reachability floor %v, known %v", c.name, k.refundDetailFrom, c.known)
		}
		if c.known && !k.refundDetailFrom.Equal(began.Add(-60*24*time.Hour)) {
			t.Errorf("%s: treated scan start as oldest readable order: %v", c.name, k.refundDetailFrom)
		}
	}
}

func TestProductTotalsBelongToTheirOwnLists(t *testing.T) {
	id, site := salesStore(t, nil)
	sell(t, id, one(1, nov(3), "10.00"))
	other := one(2, nov(3), "20.00")
	other.currency = "USD"
	sell(t, id, other)
	answer := ask(t, id, site)
	if js(answer["productSalesCount"]) != "1" || js(answer["productUnitsCount"]) != "2" {
		t.Fatalf("different currency products counted in sales list: %s", js(answer))
	}
}

func TestAnEmptyCheckSourceCannotDeleteItsExistingFacts(t *testing.T) {
	id := keptStore(t)
	now := time.Now().UTC()
	orderFact(t, id, "orphan", now.Add(-time.Hour), false)
	_, db := statsEngine(t)
	var empty int
	err := readTx(context.Background(), db, func(ctx context.Context, tx bun.Tx) error {
		var err error
		empty, err = emptyCheckSources(ctx, tx, id, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if empty != 1 {
		t.Fatalf("empty sources with old facts = %d, want one", empty)
	}
}
