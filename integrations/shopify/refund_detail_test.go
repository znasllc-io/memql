package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// refund_detail_test.go -- every order is read once more, so a refund mirrored
// before its detail gets it.
//
// A refund row written before the mirror recorded a refund's lines, or by a
// bulk line, holds none of them, and an hourly sweep that reads only what
// changed never reads its order again. A store's order sweep reads the whole
// listing once, from the start, and the store's health says when that read
// began once it has ended. What the runtime does with the sweep's answer is its
// half, played in the root module
// (component/datasync/shopify_refund_detail_db_test.go).

// healthKeeper is the rig's engine with the store's health kept: a
// recordStoreHealth write replaces the row's health, so the next read of the
// store answers it.
type healthKeeper struct {
	memql.IntegrationEngineAccess
	fake *fakeEngine
	row  map[string]any
}

func (k *healthKeeper) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if callName(q) == "recordStoreHealth" {
		body := strings.TrimPrefix(q, "recordStoreHealth(health: ")
		body = body[:strings.LastIndex(body, ", storeId: ")]
		var health map[string]any
		if err := json.Unmarshal([]byte(body), &health); err != nil {
			return nil, fmt.Errorf("the rig could not read the health it was sent: %w", err)
		}
		k.fake.mu.Lock()
		k.row["health"] = health
		k.fake.mu.Unlock()
	}
	return k.IntegrationEngineAccess.Execute(ctx, q)
}

// refundRig is the mirror rig with one store whose row, and its health, the
// connector reads and writes.
type refundRig struct {
	*mirrorRig
	record map[string]any // the store's row
	keeper *healthKeeper
}

func newRefundRig(t *testing.T, status string, health map[string]any) *refundRig {
	t.Helper()
	r := newMirrorRig(t)
	extra := map[string]any{}
	if health != nil {
		extra["health"] = health
	}
	row := storeRow(testStoreID, "acme-widgets.myshopify.com", status, extra)
	r.engine.setRows("stores", []map[string]any{row})
	r.engine.setRows("storeById", []map[string]any{row})
	r.conn.stores.Invalidate()
	keeper := &healthKeeper{IntegrationEngineAccess: r.mirror, fake: r.engine, row: row}
	r.conn.engine = keeper
	return &refundRig{mirrorRig: r, record: row, keeper: keeper}
}

// refuses makes the mirror refuse the rows of these orders, health kept.
func (r *refundRig) refuses(gids ...string) {
	rows := map[string]bool{}
	for _, gid := range gids {
		rows[MirrorRowID(testStoreID, gid)] = true
	}
	r.conn.engine = &unwritable{IntegrationEngineAccess: r.keeper, rows: rows}
}

// health is the store's health as the row holds it now.
func (r *refundRig) health() map[string]any {
	r.engine.mu.Lock()
	defer r.engine.mu.Unlock()
	h, _ := r.record["health"].(map[string]any)
	return h
}

// sweep runs the store's sweep of a concept, as the runtime does, from since.
func (r *refundRig) sweep(concept string, since time.Time) (memqlsync.ReconcileReport, error) {
	return r.conn.ReconcilePartition(context.Background(), testStoreID, generated.ConceptID(concept), since)
}

// listings is every list request of op, in order.
func (r *refundRig) listings(op string) []adminRequest {
	var out []adminRequest
	for _, req := range r.admin.seen() {
		if req.Operation == op {
			out = append(out, req)
		}
	}
	return out
}

func refundOrder(n int) map[string]any {
	return map[string]any{"id": fmt.Sprintf("gid://shopify/Order/%d", n), "name": fmt.Sprintf("#10%02d", n), "updatedAt": "2026-08-23T10:00:00Z"}
}

// pagedOrders answers the order listing: a filtered request with changed, and
// the whole listing of total orders from its cursor ("c<n>" after the n-th),
// as many as the request asks for.
func (r *refundRig) pagedOrders(total int, changed ...map[string]any) {
	r.admin.replyWith("ShopifyListOrder", func(vars map[string]any) any {
		if q, _ := vars["query"].(string); q != "" {
			return listPageReply("orders", false, "", changed...)
		}
		from := 0
		if after, _ := vars["after"].(string); after != "" {
			fmt.Sscanf(after, "c%d", &from)
		}
		first := total
		if f, ok := vars["first"].(float64); ok {
			first = int(f)
		}
		var nodes []map[string]any
		for n := from + 1; n <= total && len(nodes) < first; n++ {
			nodes = append(nodes, refundOrder(n))
		}
		end := from + len(nodes)
		return listPageReply("orders", end < total, fmt.Sprintf("c%d", end), nodes...)
	})
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func TestAStoresFirstOrderSweepUnderThisVersionReadsFromTheStart(t *testing.T) {
	r := newRefundRig(t, StatusLive, map[string]any{"ordersFrom": "2026-06-24"})
	r.now = at("12:00:00")
	r.pagedOrders(3)
	since := at("11:00:00")

	report, err := r.sweep("order", since)
	if err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	lists := r.listings("ShopifyListOrder")
	if len(lists) != 1 {
		t.Fatalf("the sweep listed the orders %d times, want once", len(lists))
	}
	if q, present := lists[0].Variables["query"]; present {
		t.Fatalf("the read of every order was sent the filter %v, want none: it reads from the start whatever watermark it is handed", q)
	}
	if report.Checked != 3 {
		t.Errorf("report = %+v, want the store's 3 orders checked", report)
	}
	for n := 1; n <= 3; n++ {
		if r.row("order", fmt.Sprintf("gid://shopify/Order/%d", n)) == nil {
			t.Errorf("order %d was not written", n)
		}
	}
	h := r.health()
	if got := h[refundDetailFromKey]; got != stamp(r.now) {
		t.Errorf("health %s = %v, want when the read began, %s", refundDetailFromKey, got, stamp(r.now))
	}
	if got, present := h[refundDetailAfterKey]; present {
		t.Errorf("health %s = %v after the read ended, want it absent", refundDetailAfterKey, got)
	}
	if h["ordersFrom"] != "2026-06-24" {
		t.Errorf("the stamp lost the health's other keys: %v", h)
	}
}

func TestTheSweepAfterItReadsOnlyWhatChanged(t *testing.T) {
	r := newRefundRig(t, StatusLive, map[string]any{refundDetailFromKey: "2026-08-22T09:00:00Z"})
	r.now = at("12:00:00")
	r.pagedOrders(3, refundOrder(2))
	since := at("11:00:00")

	report, err := r.sweep("order", since)
	if err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	lists := r.listings("ShopifyListOrder")
	if len(lists) != 1 {
		t.Fatalf("the sweep listed the orders %d times, want once", len(lists))
	}
	if q, want := lists[0].Variables["query"], "updated_at:>'"+stamp(since)+"'"; q != want {
		t.Fatalf("the sweep of a store that was read whole was sent %v, want %q", q, want)
	}
	if report.Checked != 1 {
		t.Errorf("report = %+v, want the one order that changed", report)
	}
	if writes := r.engine.callsTo("recordStoreHealth"); len(writes) != 0 {
		t.Errorf("an ordinary sweep wrote the store's health: %v", writes)
	}
}

func TestOnlyTheOrderDomainIsReadFromTheStart(t *testing.T) {
	r := newRefundRig(t, StatusLive, nil)
	r.now = at("12:00:00")
	r.admin.reply("ShopifyListProduct", listPageReply("products", false, "",
		map[string]any{"id": "gid://shopify/Product/1", "title": "City bike", "updatedAt": "2026-08-23T11:30:00Z"}))
	since := at("11:00:00")

	if _, err := r.sweep("product", since); err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	lists := r.listings("ShopifyListProduct")
	if len(lists) != 1 {
		t.Fatalf("the sweep listed the products %d times, want once", len(lists))
	}
	if q, want := lists[0].Variables["query"], "updated_at:>'"+stamp(since)+"'"; q != want {
		t.Fatalf("a product sweep was sent %v, want its watermark %q", q, want)
	}
	if writes := r.engine.callsTo("recordStoreHealth"); len(writes) != 0 {
		t.Errorf("a product sweep wrote the store's health: %v", writes)
	}
}

func TestASweepInWhichAWriteFailedIsNotStamped(t *testing.T) {
	r := newRefundRig(t, StatusLive, nil)
	r.now = at("12:00:00")
	r.pagedOrders(3)
	r.refuses("gid://shopify/Order/2")

	_, err := r.sweep("order", at("11:00:00"))
	if err == nil || !memqlsync.IsWaitingCadence(err) {
		t.Fatalf("a read that could not write an order answered %v, want the failure that waits its cadence", err)
	}
	if q, present := r.listings("ShopifyListOrder")[0].Variables["query"]; present {
		t.Fatalf("the read was sent the filter %v, so this test proves nothing", q)
	}
	h := r.health()
	for _, key := range []string{refundDetailFromKey, refundDetailAfterKey} {
		if got, present := h[key]; present {
			t.Errorf("health %s = %v after a read that could not write an order, want it absent", key, got)
		}
	}
	for _, n := range []int{1, 3} {
		if r.row("order", fmt.Sprintf("gid://shopify/Order/%d", n)) == nil {
			t.Errorf("order %d, listed beside the one that failed, was not written", n)
		}
	}
}

func TestAReadCutAtItsRowBudgetIsNotStampedAndGoesOnFromItsCursor(t *testing.T) {
	saved := refundDetailRowBudget
	refundDetailRowBudget = 3
	t.Cleanup(func() { refundDetailRowBudget = saved })

	r := newRefundRig(t, StatusLive, nil)
	r.pagedOrders(5, refundOrder(1))
	began := at("12:00:00")
	r.now = began

	if _, err := r.sweep("order", at("11:00:00")); err != nil {
		t.Fatalf("the first sweep: %v", err)
	}
	h := r.health()
	if got, present := h[refundDetailFromKey]; present {
		t.Fatalf("a read cut at its budget was stamped %v", got)
	}
	after, _ := h[refundDetailAfterKey].(map[string]any)
	if after["cursor"] != "c3" || after["began"] != stamp(began) {
		t.Fatalf("health %s = %v, want the cursor after the third order and when the read began, %s", refundDetailAfterKey, h[refundDetailAfterKey], stamp(began))
	}
	for n := 4; n <= 5; n++ {
		if r.row("order", fmt.Sprintf("gid://shopify/Order/%d", n)) != nil {
			t.Fatalf("order %d was read past the budget", n)
		}
	}

	r.now = at("13:00:00")
	since := at("12:00:00")
	if _, err := r.sweep("order", since); err != nil {
		t.Fatalf("the second sweep: %v", err)
	}
	lists := r.listings("ShopifyListOrder")[1:]
	if len(lists) != 2 {
		t.Fatalf("the second sweep listed the orders %d times, want its own check and then the read", len(lists))
	}
	if q, want := lists[0].Variables["query"], "updated_at:>'"+stamp(since)+"'"; q != want {
		t.Errorf("the second sweep asked first %v, want its handed filter %q", q, want)
	}
	if _, present := lists[1].Variables["query"]; present || lists[1].Variables["after"] != "c3" {
		t.Errorf("the read went on with %v, want the listing after c3 with no filter", lists[1].Variables)
	}
	for n := 4; n <= 5; n++ {
		if r.row("order", fmt.Sprintf("gid://shopify/Order/%d", n)) == nil {
			t.Errorf("order %d was not read when the read went on", n)
		}
	}
	h = r.health()
	if got := h[refundDetailFromKey]; got != stamp(began) {
		t.Errorf("health %s = %v, want when the first read began, %s", refundDetailFromKey, got, stamp(began))
	}
	if got, present := h[refundDetailAfterKey]; present {
		t.Errorf("health %s = %v after the read ended, want it absent", refundDetailAfterKey, got)
	}
}

func TestAPausedStoresSweepIsSkippedAndNothingIsStamped(t *testing.T) {
	r := newRefundRig(t, StatusPaused, nil)
	r.now = at("12:00:00")
	r.pagedOrders(2)

	_, err := r.sweep("order", at("11:00:00"))
	if !memqlsync.IsBusy(err) {
		t.Fatalf("a paused store's sweep answered %v, want busy", err)
	}
	if seen := r.admin.seen(); len(seen) != 0 {
		t.Fatalf("Shopify was asked %d times for a paused store", len(seen))
	}
	if writes := r.engine.callsTo("recordStoreHealth"); len(writes) != 0 {
		t.Fatalf("a paused store's health was written: %v", writes)
	}

	r.engine.mu.Lock()
	r.record["status"] = StatusLive
	r.engine.mu.Unlock()
	r.conn.stores.Invalidate()
	if _, err := r.sweep("order", at("11:00:00")); err != nil {
		t.Fatalf("the resumed store's sweep: %v", err)
	}
	lists := r.listings("ShopifyListOrder")
	if len(lists) != 1 {
		t.Fatalf("the resumed store's orders were listed %d times, want once", len(lists))
	}
	if q, present := lists[0].Variables["query"]; present {
		t.Errorf("the resumed store's sweep was sent %v, want the read from the start", q)
	}
	if got := r.health()[refundDetailFromKey]; got != stamp(r.now) {
		t.Errorf("health %s = %v, want %s", refundDetailFromKey, got, stamp(r.now))
	}
}

func TestACursorTheStoreRefusesStartsTheReadAgain(t *testing.T) {
	r := newRefundRig(t, StatusLive, map[string]any{refundDetailAfterKey: map[string]any{"cursor": "stale", "began": "2026-08-22T09:00:00Z"}})
	r.now = at("12:00:00")
	r.admin.replyWith("ShopifyListOrder", func(vars map[string]any) any {
		if vars["after"] == "stale" {
			// Refused, this request alone: the cursor is one Shopify no
			// longer takes.
			r.admin.failOnce("ShopifyListOrder", GraphQLError{Message: "Invalid cursor for current pagination sort."})
		}
		return listPageReply("orders", false, "", refundOrder(1))
	})

	if _, err := r.sweep("order", at("11:00:00")); err == nil {
		t.Fatal("a read whose cursor was refused answered a clean sweep")
	}
	lists := r.listings("ShopifyListOrder")
	if len(lists) != 2 || lists[1].Variables["after"] != "stale" {
		t.Fatalf("the sweep asked %d listings, want its own check and then the read after the stored cursor", len(lists))
	}
	h := r.health()
	if got, present := h[refundDetailAfterKey]; present {
		t.Errorf("health %s = %v after the cursor was refused, want it dropped", refundDetailAfterKey, got)
	}
	if got, present := h[refundDetailFromKey]; present {
		t.Errorf("a read that failed was stamped %v", got)
	}

	if _, err := r.sweep("order", at("11:00:00")); err != nil {
		t.Fatalf("the next sweep: %v", err)
	}
	lists = r.listings("ShopifyListOrder")
	if len(lists) != 3 || lists[2].Variables["after"] != nil || lists[2].Variables["query"] != nil {
		t.Fatalf("the next sweep's listings %v, want one read from the start", lists[2:])
	}
	if got := r.health()[refundDetailFromKey]; got != stamp(r.now) {
		t.Errorf("health %s = %v, want %s", refundDetailFromKey, got, stamp(r.now))
	}
}
