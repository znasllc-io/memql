package shopify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
// changed never reads its order again. After a store's ordinary order check, a
// piece of one read of the whole listing runs, from the start or from its
// cursor, and the store's health says when that read began once it has ended.
// The read never stands in the ordinary check's way: the check's answer is the
// sweep's, and the read's failure is recorded on the store. What the runtime
// does with the sweep's answer is its half, played in the root module
// (component/datasync/shopify_refund_detail_db_test.go).

// healthKeeper is the rig's engine with the store's health kept: a
// recordStoreHealth write replaces the row's health, so the next read of the
// store answers it. It answers the domains' sync states by id from states.
type healthKeeper struct {
	memql.IntegrationEngineAccess
	fake   *fakeEngine
	row    map[string]any
	states map[string]map[string]any
}

func (k *healthKeeper) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	switch callName(q) {
	case "recordStoreHealth":
		body := strings.TrimPrefix(q, "recordStoreHealth(health: ")
		body = body[:strings.LastIndex(body, ", storeId: ")]
		var health map[string]any
		if err := json.Unmarshal([]byte(body), &health); err != nil {
			return nil, fmt.Errorf("the rig could not read the health it was sent: %w", err)
		}
		k.fake.mu.Lock()
		k.row["health"] = health
		k.fake.mu.Unlock()
	case "syncStateById":
		m := stateIDArgRE.FindStringSubmatch(q)
		if m == nil {
			return nil, fmt.Errorf("syncStateById without a stateId: %s", q)
		}
		k.fake.mu.Lock()
		row := k.states[m[1]]
		k.fake.mu.Unlock()
		if row == nil {
			return resultWithRows(nil), nil
		}
		return resultWithRows([]map[string]any{row}), nil
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
	return newRefundRigWith(t, status, health, nil)
}

func newRefundRigWith(t *testing.T, status string, health map[string]any, extra map[string]any) *refundRig {
	t.Helper()
	r := newMirrorRig(t)
	if extra == nil {
		extra = map[string]any{}
	}
	if health != nil {
		extra["health"] = health
	}
	row := storeRow(testStoreID, "acme-widgets.myshopify.com", status, extra)
	r.engine.setRows("stores", []map[string]any{row})
	r.engine.setRows("storeById", []map[string]any{row})
	r.conn.stores.Invalidate()
	keeper := &healthKeeper{IntegrationEngineAccess: r.mirror, fake: r.engine, row: row, states: map[string]map[string]any{}}
	r.conn.engine = keeper
	return &refundRig{mirrorRig: r, record: row, keeper: keeper}
}

// orderBackfill gives the store's order domain a sync state: its backfill's
// status and cursor.
func (r *refundRig) orderBackfill(status, cursor string) {
	id := memqlsync.StateID(generated.ConceptID("order"), ConnectorName, string(memqlsync.DirectionInbound), testStoreID)
	r.engine.mu.Lock()
	r.keeper.states[id] = map[string]any{"id": id, "conceptId": generated.ConceptID("order"), "connector": ConnectorName,
		"partitionKey": testStoreID, "backfillStatus": status, "backfillCursor": cursor}
	r.engine.mu.Unlock()
}

// completedAt is the cursor of an order backfill whose export started at t.
func backfillExportedAt(t time.Time) string {
	return backfillCursor{Rule: originVersionRule, At: memqlsync.ReadTime(t)}.encode()
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

// pieces is the order listings that were pieces of the read: no filter.
func (r *refundRig) pieces() []adminRequest {
	var out []adminRequest
	for _, req := range r.listings("ShopifyListOrder") {
		if _, filtered := req.Variables["query"]; !filtered {
			out = append(out, req)
		}
	}
	return out
}

// pageSize makes the store's order listing start at n rows a page.
func (r *refundRig) pageSize(n int) {
	r.conn.pageSizes.Store(pageSizeKey(Store{ID: testStoreID}, generated.Types["order"]), n)
}

func refundOrder(n int) map[string]any {
	return map[string]any{"id": fmt.Sprintf("gid://shopify/Order/%d", n), "name": fmt.Sprintf("#10%02d", n), "updatedAt": "2026-08-23T10:00:00Z"}
}

func listedOrderGID(n int) string { return fmt.Sprintf("gid://shopify/Order/%d", n) }

// ordersPage is the page of a listing of total orders after cursor ("c<n>"
// after the n-th), as many as vars ask for.
func ordersPage(total int, vars map[string]any) map[string]any {
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
}

// pagedOrders answers the order listing: a filtered request with changed, and
// the whole listing of total orders from its cursor.
func (r *refundRig) pagedOrders(total int, changed ...map[string]any) {
	r.admin.replyWith("ShopifyListOrder", func(vars map[string]any) any {
		if q, _ := vars["query"].(string); q != "" {
			return listPageReply("orders", false, "", changed...)
		}
		return ordersPage(total, vars)
	})
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// requireOrdinaryFirst holds a sweep's listings to the ordinary check from
// since first, and returns the rest.
func requireOrdinaryFirst(t *testing.T, lists []adminRequest, since time.Time) []adminRequest {
	t.Helper()
	if len(lists) == 0 {
		t.Fatal("the sweep listed no orders")
	}
	if q, want := lists[0].Variables["query"], "updated_at:>'"+stamp(since)+"'"; q != want {
		t.Fatalf("the sweep's first listing was sent %v, want its ordinary check %q", q, want)
	}
	return lists[1:]
}

// requireReadError holds the health's record of a piece that failed.
func requireReadError(t *testing.T, h map[string]any, at time.Time, words string) {
	t.Helper()
	rec, _ := h[refundDetailErrorKey].(map[string]any)
	if rec["at"] != stamp(at) || !strings.Contains(fmt.Sprint(rec["error"]), words) {
		t.Fatalf("health %s = %v, want the failure at %s, saying %q", refundDetailErrorKey, h[refundDetailErrorKey], stamp(at), words)
	}
}

func TestAStoresFirstOrderSweepUnderThisVersionReadsFromTheStart(t *testing.T) {
	r := newRefundRig(t, StatusLive, map[string]any{"ordersFrom": "2026-06-24"})
	r.now = at("12:00:00")
	r.pagedOrders(3)
	since := at("11:00:00")

	report, err := r.sweep("order", since)
	if err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	read := requireOrdinaryFirst(t, r.listings("ShopifyListOrder"), since)
	if len(read) != 1 {
		t.Fatalf("after its check the sweep listed the orders %d times, want one piece of the read", len(read))
	}
	if q, present := read[0].Variables["query"]; present {
		t.Fatalf("the read of every order was sent the filter %v, want none", q)
	}
	if report.Checked != 3 {
		t.Errorf("report = %+v, want the store's 3 orders checked by the read", report)
	}
	for n := 1; n <= 3; n++ {
		if r.row("order", listedOrderGID(n)) == nil {
			t.Errorf("order %d was not written", n)
		}
	}
	h := r.health()
	if got := h[refundDetailFromKey]; got != stamp(r.now) {
		t.Errorf("health %s = %v, want when the read began, %s", refundDetailFromKey, got, stamp(r.now))
	}
	for _, key := range []string{refundDetailAfterKey, refundDetailErrorKey} {
		if got, present := h[key]; present {
			t.Errorf("health %s = %v after the read ended, want it absent", key, got)
		}
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
	if rest := requireOrdinaryFirst(t, r.listings("ShopifyListOrder"), since); len(rest) != 0 {
		t.Fatalf("a store that was read whole was listed again after its check: %v", rest)
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

// RULING A (ii): the first page's row refused, with no read under way. The
// ordinary check ran on that sweep and answers it, and runs on the next.
func TestASweepInWhichAWriteFailedIsNotStamped(t *testing.T) {
	r := newRefundRig(t, StatusLive, nil)
	r.now = at("12:00:00")
	r.pagedOrders(3)
	r.refuses(listedOrderGID(2))
	since := at("11:00:00")

	if _, err := r.sweep("order", since); err != nil {
		t.Fatalf("a read that could not write an order failed the sweep, whose check was clean: %v", err)
	}
	if read := requireOrdinaryFirst(t, r.listings("ShopifyListOrder"), since); len(read) != 1 {
		t.Fatalf("after its check the sweep listed %d pieces, want one", len(read))
	}
	h := r.health()
	for _, key := range []string{refundDetailFromKey, refundDetailAfterKey} {
		if got, present := h[key]; present {
			t.Errorf("health %s = %v after a read whose first page could not be written, want it absent", key, got)
		}
	}
	requireReadError(t, h, r.now, "could not be written")
	for _, n := range []int{1, 3} {
		if r.row("order", listedOrderGID(n)) == nil {
			t.Errorf("order %d, listed beside the one that failed, was not written", n)
		}
	}

	r.now = at("13:00:00")
	next := at("12:00:00")
	if _, err := r.sweep("order", next); err != nil {
		t.Fatalf("the next sweep: %v", err)
	}
	requireOrdinaryFirst(t, r.listings("ShopifyListOrder")[2:], next)
}

// RULING A (i): a read under way, one row past the cursor refused for good.
// The ordinary check's answer is the sweep's, so the runtime moves the
// watermark; the cursor stops at the refused row's page, not back at the
// piece's start; the failure is on the store; the next sweep reads that one
// page again, not the piece.
func TestAReadUnderWayStopsAtTheRefusedRowsPage(t *testing.T) {
	r := newRefundRig(t, StatusLive, map[string]any{refundDetailAfterKey: map[string]any{"cursor": "c1", "began": "2026-08-23T09:00:00Z"}})
	r.now = at("12:00:00")
	r.pageSize(2)
	r.pagedOrders(9, refundOrder(1))
	r.refuses(listedOrderGID(4))
	since := at("11:00:00")

	report, err := r.sweep("order", since)
	if err != nil {
		t.Fatalf("the sweep answered the read's failure %v; its ordinary check was clean", err)
	}
	read := requireOrdinaryFirst(t, r.listings("ShopifyListOrder"), since)
	if len(read) != 2 || read[0].Variables["after"] != "c1" || read[1].Variables["after"] != "c3" {
		t.Fatalf("the piece listed %d pages, want two: after c1, and after c3, where the refused order's page begins", len(read))
	}
	if report.Checked != 1+4 {
		t.Errorf("report = %+v, want the check's one order and the piece's four", report)
	}
	h := r.health()
	after, _ := h[refundDetailAfterKey].(map[string]any)
	if after["cursor"] != "c3" || after["began"] != "2026-08-23T09:00:00Z" {
		t.Fatalf("health %s = %v, want the cursor at the refused order's page, c3, and the read's own start", refundDetailAfterKey, h[refundDetailAfterKey])
	}
	if _, stamped := h[refundDetailFromKey]; stamped {
		t.Fatal("a read that stopped at a refused row was stamped")
	}
	requireReadError(t, h, r.now, "could not be written")
	if r.row("order", listedOrderGID(6)) != nil {
		t.Fatal("the piece read past the refused order's page")
	}

	r.now = at("13:00:00")
	if _, err := r.sweep("order", at("12:00:00")); err != nil {
		t.Fatalf("the next sweep: %v", err)
	}
	again := requireOrdinaryFirst(t, r.listings("ShopifyListOrder")[3:], at("12:00:00"))
	if len(again) != 1 || again[0].Variables["after"] != "c3" {
		t.Fatalf("the next sweep's piece listed %d pages, want the refused order's one page again", len(again))
	}
	requireReadError(t, r.health(), r.now, "could not be written")
}

func TestAReadCutAtItsRowBudgetIsNotStampedAndGoesOnFromItsCursor(t *testing.T) {
	saved := refundDetailRowBudget
	refundDetailRowBudget = 3
	t.Cleanup(func() { refundDetailRowBudget = saved })

	r := newRefundRig(t, StatusLive, map[string]any{refundDetailErrorKey: map[string]any{"at": "2026-08-23T09:00:00Z", "error": "an older failure"}})
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
	if got, present := h[refundDetailErrorKey]; present {
		t.Errorf("a piece cut at its budget kept the older failure %v", got)
	}
	after, _ := h[refundDetailAfterKey].(map[string]any)
	if after["cursor"] != "c3" || after["began"] != stamp(began) {
		t.Fatalf("health %s = %v, want the cursor after the third order and when the read began, %s", refundDetailAfterKey, h[refundDetailAfterKey], stamp(began))
	}
	for n := 4; n <= 5; n++ {
		if r.row("order", listedOrderGID(n)) != nil {
			t.Fatalf("order %d was read past the budget", n)
		}
	}

	r.now = at("13:00:00")
	since := at("12:00:00")
	if _, err := r.sweep("order", since); err != nil {
		t.Fatalf("the second sweep: %v", err)
	}
	lists := requireOrdinaryFirst(t, r.listings("ShopifyListOrder")[2:], since)
	if len(lists) != 1 || lists[0].Variables["after"] != "c3" {
		t.Fatalf("the read went on with %v, want the listing after c3 with no filter", lists)
	}
	for n := 4; n <= 5; n++ {
		if r.row("order", listedOrderGID(n)) == nil {
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
	read := requireOrdinaryFirst(t, r.listings("ShopifyListOrder"), at("11:00:00"))
	if len(read) != 1 || read[0].Variables["after"] != nil {
		t.Errorf("the resumed store's read = %v, want one piece from the start", read)
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

	if _, err := r.sweep("order", at("11:00:00")); err != nil {
		t.Fatalf("the read's refused cursor failed the sweep, whose check was clean: %v", err)
	}
	read := requireOrdinaryFirst(t, r.listings("ShopifyListOrder"), at("11:00:00"))
	if len(read) != 1 || read[0].Variables["after"] != "stale" {
		t.Fatalf("the piece listed %v, want the listing after the stored cursor", read)
	}
	h := r.health()
	if got, present := h[refundDetailAfterKey]; present {
		t.Errorf("health %s = %v after the cursor was refused, want it dropped", refundDetailAfterKey, got)
	}
	if got, present := h[refundDetailFromKey]; present {
		t.Errorf("a read that failed was stamped %v", got)
	}
	requireReadError(t, h, r.now, "could not be read")

	if _, err := r.sweep("order", at("11:00:00")); err != nil {
		t.Fatalf("the next sweep: %v", err)
	}
	read = requireOrdinaryFirst(t, r.listings("ShopifyListOrder")[2:], at("11:00:00"))
	if len(read) != 1 || read[0].Variables["after"] != nil {
		t.Fatalf("the next sweep's piece %v, want one read from the start", read)
	}
	if got := r.health()[refundDetailFromKey]; got != stamp(r.now) {
		t.Errorf("health %s = %v, want %s", refundDetailFromKey, got, stamp(r.now))
	}
}

// failingTransport fails, as the network does, every request whose body
// carries fails, and passes every other through.
type failingTransport struct {
	next  http.RoundTripper
	fails string
}

func (f failingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if strings.Contains(string(body), f.fails) {
		return nil, errors.New("connection reset by peer")
	}
	req.Body = io.NopCloser(strings.NewReader(string(body)))
	return f.next.RoundTrip(req)
}

// THE CURSOR IS KEPT on a failure worth retrying, and when the piece had
// already gone past it: only Shopify refusing the first page after the stored
// cursor drops it.
func TestTheCursorIsKeptOnAFailureWorthRetrying(t *testing.T) {
	const stored = "c2"
	underWay := func() map[string]any {
		return map[string]any{refundDetailAfterKey: map[string]any{"cursor": stored, "began": "2026-08-22T09:00:00Z"}}
	}
	keeps := func(t *testing.T, r *refundRig, want string) {
		t.Helper()
		after, _ := r.health()[refundDetailAfterKey].(map[string]any)
		if after["cursor"] != want || after["began"] != "2026-08-22T09:00:00Z" {
			t.Fatalf("health %s = %v, want the cursor %s kept with the read's start", refundDetailAfterKey, r.health()[refundDetailAfterKey], want)
		}
		requireReadError(t, r.health(), r.now, "could not be read")
	}
	statusFor := func(r *refundRig, failing string, code int) {
		r.admin.replyWith("ShopifyListOrder", func(vars map[string]any) any {
			status := 0
			if vars["after"] == failing {
				status = code
			}
			r.admin.httpStatus("ShopifyListOrder", status)
			if q, _ := vars["query"].(string); q != "" {
				return listPageReply("orders", false, "")
			}
			return ordersPage(6, vars)
		})
	}
	for name, code := range map[string]int{"a throttle": http.StatusTooManyRequests, "a server error": http.StatusBadGateway} {
		t.Run(name, func(t *testing.T) {
			r := newRefundRig(t, StatusLive, underWay())
			r.now = at("12:00:00")
			statusFor(r, stored, code)
			if _, err := r.sweep("order", at("11:00:00")); err != nil {
				t.Fatalf("the sweep: %v", err)
			}
			if len(r.pieces()) == 0 {
				t.Fatal("no piece ran, so this test proves nothing")
			}
			keeps(t, r, stored)
		})
	}
	t.Run("a network failure", func(t *testing.T) {
		r := newRefundRig(t, StatusLive, underWay())
		r.now = at("12:00:00")
		r.pagedOrders(6)
		r.conn.admin.http = &http.Client{Transport: failingTransport{next: http.DefaultTransport, fails: `"after":"` + stored + `"`}}
		if _, err := r.sweep("order", at("11:00:00")); err != nil {
			t.Fatalf("the sweep: %v", err)
		}
		keeps(t, r, stored)
	})
	t.Run("a refusal after the piece went past the cursor", func(t *testing.T) {
		r := newRefundRig(t, StatusLive, underWay())
		r.now = at("12:00:00")
		r.pageSize(2)
		r.admin.replyWith("ShopifyListOrder", func(vars map[string]any) any {
			if vars["after"] == "c4" {
				r.admin.failOnce("ShopifyListOrder", GraphQLError{Message: "Invalid cursor for current pagination sort."})
			}
			if q, _ := vars["query"].(string); q != "" {
				return listPageReply("orders", false, "")
			}
			return ordersPage(9, vars)
		})
		if _, err := r.sweep("order", at("11:00:00")); err != nil {
			t.Fatalf("the sweep: %v", err)
		}
		if p := r.pieces(); len(p) != 2 {
			t.Fatalf("the piece listed %d pages, want the page after c2 and the refused one after c4", len(p))
		}
		keeps(t, r, "c4")
	})
}

// A LISTING THAT STALLS -- an empty page that says there is a next one -- is
// the read's failure, said as a stall, and the read goes on from where it was.
func TestAListingThatStallsIsTheReadsFailure(t *testing.T) {
	r := newRefundRig(t, StatusLive, map[string]any{refundDetailAfterKey: map[string]any{"cursor": "c2", "began": "2026-08-22T09:00:00Z"}})
	r.now = at("12:00:00")
	r.admin.replyWith("ShopifyListOrder", func(vars map[string]any) any {
		if q, _ := vars["query"].(string); q != "" {
			return listPageReply("orders", false, "")
		}
		return listPageReply("orders", true, "c2")
	})
	if _, err := r.sweep("order", at("11:00:00")); err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	h := r.health()
	if _, stamped := h[refundDetailFromKey]; stamped {
		t.Fatal("a stalled read was stamped")
	}
	after, _ := h[refundDetailAfterKey].(map[string]any)
	if after["cursor"] != "c2" {
		t.Errorf("health %s = %v, want the cursor kept where the listing stalled", refundDetailAfterKey, h[refundDetailAfterKey])
	}
	requireReadError(t, h, r.now, "stalled")
}

// THE LAST PAGE'S CAP DOES NOT TEACH THE DOMAIN A PAGE SIZE. A page asked
// smaller than the domain's own, because the budget had that little left,
// refused for cost and shrunk, is shrunk for itself alone.
func TestACappedPageRefusedForCostDoesNotTeachThePageSize(t *testing.T) {
	saved := refundDetailRowBudget
	refundDetailRowBudget = 3
	t.Cleanup(func() { refundDetailRowBudget = saved })

	r := newRefundRig(t, StatusLive, nil)
	r.now = at("12:00:00")
	spec := generated.Types["order"]
	refused := false
	r.admin.replyWith("ShopifyListOrder", func(vars map[string]any) any {
		if q, _ := vars["query"].(string); q != "" {
			return listPageReply("orders", false, "")
		}
		if !refused {
			refused = true
			r.admin.failOnce("ShopifyListOrder", GraphQLError{
				Message:    "Query cost is 1649, which exceeds the single query max cost limit (1000).",
				Extensions: map[string]any{"code": "MAX_COST_EXCEEDED"},
			})
		}
		return ordersPage(5, vars)
	})
	if _, err := r.sweep("order", at("11:00:00")); err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	p := r.pieces()
	if len(p) < 2 || p[0].Variables["first"] != float64(3) {
		t.Fatalf("the piece's pages %v, want a capped page of 3 refused and asked again smaller", p)
	}
	if got := r.conn.pageSizeFor(Store{ID: testStoreID}, spec); got != reconcilePageSize {
		t.Fatalf("the order domain's page is %d after a capped page was shrunk, want its own %d", got, reconcilePageSize)
	}
}

// UNDER A GRANT WITHOUT read_all_orders Shopify lists the last sixty days. The
// read retires nothing it does not see, ends where the listing ends and is
// stamped.
func TestUnderAGrantWithoutReadAllOrdersTheReadRetiresNothingAndEnds(t *testing.T) {
	r := newRefundRigWith(t, StatusLive, nil, map[string]any{"scopesGranted": []any{"read_orders", "read_products"}})
	r.now = at("12:00:00")
	r.held("order", listedOrderGID(77), "2026-05-01T10:00:00Z", at("09:00:00"))
	r.pagedOrders(2)

	if _, err := r.sweep("order", at("11:00:00")); err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	if row := r.row("order", listedOrderGID(77)); row == nil || row["deleted"] == true {
		t.Fatalf("an order the listing does not show was retired: %v", row)
	}
	h := r.health()
	if got := h[refundDetailFromKey]; got != stamp(r.now) {
		t.Errorf("health %s = %v, want the read stamped where the listing ended, %s", refundDetailFromKey, got, stamp(r.now))
	}
	for _, key := range []string{refundDetailAfterKey, refundDetailErrorKey} {
		if got, present := h[key]; present {
			t.Errorf("health %s = %v, want it absent", key, got)
		}
	}
}

// RULING B: the stamp does not outlive the rows it describes. An order
// backfill writes rows with no detail, so one that started its export after
// the read began makes the read owed again; while one is queued or running
// no piece runs.
func TestAStampedStoreWhoseOrderBackfillCompletedLaterOwesTheReadAgain(t *testing.T) {
	r := newRefundRig(t, StatusLive, map[string]any{refundDetailFromKey: "2026-08-23T09:00:00Z"})
	r.now = at("12:00:00")
	r.orderBackfill("complete", backfillExportedAt(at("10:00:00")))
	r.pagedOrders(2)

	if _, err := r.sweep("order", at("11:00:00")); err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	if p := r.pieces(); len(p) != 1 {
		t.Fatalf("a store whose orders were backfilled after its read listed %d pieces, want the read again", len(p))
	}
	if got := r.health()[refundDetailFromKey]; got != stamp(r.now) {
		t.Errorf("health %s = %v, want the new read's start %s", refundDetailFromKey, got, stamp(r.now))
	}

	// Stamped after that backfill: the next sweep is the ordinary check alone.
	if _, err := r.sweep("order", at("11:30:00")); err != nil {
		t.Fatalf("the next sweep: %v", err)
	}
	if p := r.pieces(); len(p) != 1 {
		t.Fatalf("the sweep after the new read read again (%d pieces in all)", len(p))
	}
}

func TestAStampedStoreWithNoLaterBackfillDoesNotOweTheRead(t *testing.T) {
	for name, cursor := range map[string]string{
		"a backfill before the read":         backfillExportedAt(at("08:00:00")),
		"a backfill an older build finished": backfillCursor{Rule: originVersionRule}.encode(),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRefundRig(t, StatusLive, map[string]any{refundDetailFromKey: "2026-08-23T09:00:00Z"})
			r.now = at("12:00:00")
			r.orderBackfill("complete", cursor)
			r.pagedOrders(2)
			if _, err := r.sweep("order", at("11:00:00")); err != nil {
				t.Fatalf("the sweep: %v", err)
			}
			if p := r.pieces(); len(p) != 0 {
				t.Fatalf("a stamped store with no later backfill was read again: %v", p)
			}
			if writes := r.engine.callsTo("recordStoreHealth"); len(writes) != 0 {
				t.Errorf("its health was written: %v", writes)
			}
		})
	}
}

func TestABackfillInFlightHoldsTheRead(t *testing.T) {
	for _, status := range []string{"queued", "running"} {
		t.Run(status, func(t *testing.T) {
			r := newRefundRig(t, StatusLive, map[string]any{refundDetailAfterKey: map[string]any{"cursor": "c1", "began": "2026-08-23T09:00:00Z"}})
			r.now = at("12:00:00")
			r.orderBackfill(status, "")
			r.pagedOrders(3)
			if _, err := r.sweep("order", at("11:00:00")); err != nil {
				t.Fatalf("the sweep: %v", err)
			}
			requireOrdinaryFirst(t, r.listings("ShopifyListOrder"), at("11:00:00"))
			if p := r.pieces(); len(p) != 0 {
				t.Fatalf("a piece of the read ran while the order backfill was %s: %v", status, p)
			}
			after, _ := r.health()[refundDetailAfterKey].(map[string]any)
			if after["cursor"] != "c1" {
				t.Errorf("health %s = %v, want the read held where it was", refundDetailAfterKey, r.health()[refundDetailAfterKey])
			}

			r.orderBackfill("complete", backfillExportedAt(at("12:10:00")))
			r.now = at("12:30:00")
			if _, err := r.sweep("order", at("12:00:00")); err != nil {
				t.Fatalf("the sweep after the backfill: %v", err)
			}
			if p := r.pieces(); len(p) != 1 {
				t.Fatalf("the read did not start once the backfill was done (%d pieces)", len(p))
			}
			if got := r.health()[refundDetailFromKey]; got != stamp(r.now) {
				t.Errorf("health %s = %v, want the read begun after the backfill, %s", refundDetailFromKey, got, stamp(r.now))
			}
		})
	}
}

// overtakingEngine answers every health write as overtaken by another writer.
type overtakingEngine struct{ *fakeEngine }

func (e overtakingEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if callName(q) == "recordStoreHealth" {
		return nil, memql.ErrRowVersionChanged
	}
	return e.fakeEngine.Execute(ctx, q)
}

// The sentence a health write that kept losing its fence answers names its
// key, or its keys, truly.
func TestAHealthWriteThatKeptLosingNamesItsKeys(t *testing.T) {
	h := newHarness(t)
	h.conn.engine = overtakingEngine{h.engine}
	err := h.conn.recordStoreHealthKey(context.Background(), testStoreID, "ordersFrom", "2026-06-24", nil)
	if err == nil || !strings.Contains(err.Error(), `writes of its health key "ordersFrom";`) {
		t.Errorf("one key: %v", err)
	}
	err = h.conn.recordStoreHealthKeys(context.Background(), testStoreID, map[string]any{refundDetailFromKey: "x"}, []string{refundDetailAfterKey, refundDetailErrorKey}, nil)
	if err == nil || !strings.Contains(err.Error(), `writes of its health keys "refundDetailAfter", "refundDetailError", "refundDetailFrom";`) {
		t.Errorf("several keys: %v", err)
	}
}

// RULING A: the ordinary check's answer is the sweep's. A check that failed
// answers its error, holding the watermark, and no piece of the read runs.
func TestAnOrdinaryCheckThatFailedIsTheSweepsAnswerAndTheReadWaits(t *testing.T) {
	r := newRefundRig(t, StatusLive, map[string]any{refundDetailAfterKey: map[string]any{"cursor": "c1", "began": "2026-08-23T09:00:00Z"}})
	r.now = at("12:00:00")
	r.admin.replyWith("ShopifyListOrder", func(vars map[string]any) any {
		status := 0
		if _, filtered := vars["query"]; filtered {
			status = http.StatusBadGateway
		}
		r.admin.httpStatus("ShopifyListOrder", status)
		return ordersPage(3, vars)
	})
	if _, err := r.sweep("order", at("11:00:00")); err == nil {
		t.Fatal("a sweep whose ordinary check failed answered clean")
	}
	if p := r.pieces(); len(p) != 0 {
		t.Fatalf("a piece of the read ran after a check that failed: %v", p)
	}
	if writes := r.engine.callsTo("recordStoreHealth"); len(writes) != 0 {
		t.Errorf("the store's health was written: %v", writes)
	}
}
