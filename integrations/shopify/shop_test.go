package shopify

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// shop_test.go -- every connected store has its shop row (shop.go).
//
// The shop is a singleton with no listing and no backfill, written only by a
// SHOP_UPDATE delivery until it was also read at a connect, at a boot (for a
// store with no row), at the daily pass and at a resume. Each test here drives
// one of those through its real call site against the fake origin, and counts
// the ShopifyFetchShop calls it made and the shop rows it wrote.

const shopFetchOp = "ShopifyFetchShop"

// shopAnswer is the origin's ShopifyFetchShop answer for a bicycle shop.
func shopAnswer(currency, timezone, updatedAt string) map[string]any {
	return map[string]any{"shop": map[string]any{
		"id":           "gid://shopify/Shop/1",
		"updatedAt":    updatedAt,
		"name":         "Acme Widgets",
		"currencyCode": currency,
		"ianaTimezone": timezone,
		"plan":         map[string]any{"publicDisplayName": "Basic", "partnerDevelopment": true, "shopifyPlus": false},
	}}
}

// shopWritesIn is the raw mirror inserts of a shop row among the statements,
// with the store's id each names.
func shopWritesIn(statements []string) []string {
	var out []string
	for _, q := range statements {
		if strings.HasPrefix(strings.TrimSpace(q), `insert("v1:shopify:shop"`) {
			out = append(out, q)
		}
	}
	return out
}

// webhookPassAnswers scripts the subscription pass's two operations, so the
// daily pass itself succeeds.
func webhookPassAnswers(a *fakeAdmin) {
	a.reply("ShopifyWebhookSubscriptions", map[string]any{"webhookSubscriptions": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": false}, "nodes": []any{},
	}})
	a.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{
		"webhookSubscription": map[string]any{"id": "gid://shopify/WebhookSubscription/1"}, "userErrors": []any{},
	}})
}

func TestAConnectReadsTheStoresShop(t *testing.T) {
	h := newWriteHarness(t)
	h.admin.reply(shopFetchOp, shopAnswer("EUR", "Europe/Madrid", "2026-08-01T00:00:00Z"))

	if got := h.write(credentialSourcePending); got != connectResultConnected {
		t.Fatalf("result = %q, want connected", got)
	}
	if n := h.admin.countOp(shopFetchOp); n != 1 {
		t.Fatalf("the connect read the shop %d times, want once", n)
	}
	var log []string
	h.engine.mu.Lock()
	for _, c := range h.engine.log {
		log = append(log, c.query)
	}
	h.engine.mu.Unlock()
	writes := shopWritesIn(log)
	if len(writes) != 1 {
		t.Fatalf("the connect wrote %d shop rows, want one: %v", len(writes), writes)
	}
	for _, want := range []string{`"currencyCode":"EUR"`, `"ianaTimezone":"Europe/Madrid"`, `"storeId":"` + connectStoreID + `"`, `"versionSource":"origin"`} {
		if !strings.Contains(writes[0], want) {
			t.Errorf("the shop row lacks %s: %s", want, writes[0])
		}
	}
	// After the backfills are queued: the store row and its grant are written.
	if at, queued := h.statementIndex("insert", `"v1:shopify:shop"`), h.statementIndex("datasyncQueueBackfill"); queued < 0 || at < queued {
		t.Errorf("the shop row was written at %d, before the first backfill was queued (%d)", at, queued)
	}
}

func TestBootReadsTheShopOfAStoreThatHasNone(t *testing.T) {
	h := newHarness(t)
	h.admin.reply(shopFetchOp, shopAnswer("EUR", "Europe/Madrid", "2026-08-01T00:00:00Z"))
	Bootstrap(context.Background(), NewIntegration(h.conn), nil)
	if n := h.admin.countOp(shopFetchOp); n != 1 {
		t.Fatalf("a boot read the shop of a store with none %d times, want once", n)
	}
	if writes := shopWritesIn(h.engine.calls()); len(writes) != 1 || !strings.Contains(writes[0], `"currencyCode":"EUR"`) {
		t.Fatalf("the boot wrote %v, want the store's shop row", writes)
	}
	if reads := h.engine.callsTo("shopifyShopForStore"); len(reads) != 1 || !strings.Contains(reads[0], `storeId: "`+testStoreID+`"`) {
		t.Errorf("the boot asked the mirror %v, want one read of the store's shop", reads)
	}

	// A store that has its row is asked nothing: a node starts often.
	h = newHarness(t)
	h.admin.reply(shopFetchOp, shopAnswer("EUR", "Europe/Madrid", "2026-08-01T00:00:00Z"))
	h.engine.setRows("shopifyShopForStore", []map[string]any{{"id": "shop-row", "storeId": testStoreID, "currencyCode": "EUR"}})
	Bootstrap(context.Background(), NewIntegration(h.conn), nil)
	if n := h.admin.countOp(shopFetchOp); n != 0 {
		t.Errorf("a boot read the shop of a store that has its row %d times, want never", n)
	}
	if writes := shopWritesIn(h.engine.calls()); len(writes) != 0 {
		t.Errorf("a boot wrote %v for a store that has its row", writes)
	}

	// A paused store is not read, row or not.
	h = newHarness(t)
	h.engine.setRows("stores", []map[string]any{storeRow(testStoreID, "acme-widgets.myshopify.com", StatusPaused, nil)})
	Bootstrap(context.Background(), NewIntegration(h.conn), nil)
	if n := h.admin.countOp(shopFetchOp); n != 0 {
		t.Errorf("a boot read a paused store's shop %d times", n)
	}
}

func TestTheDailyPassReadsEveryStoresShopAgain(t *testing.T) {
	h := newHarness(t)
	second := withTwoStores(t, h)
	for _, a := range []*fakeAdmin{h.admin, second} {
		webhookPassAnswers(a)
	}
	h.admin.reply(shopFetchOp, shopAnswer("EUR", "Europe/Madrid", "2026-08-01T00:00:00Z"))
	second.reply(shopFetchOp, shopAnswer("USD", "America/Chicago", "2026-08-02T00:00:00Z"))
	// Both stores have their row already: the daily pass reads them again
	// all the same, so a SHOP_UPDATE that was lost is healed within a day.
	h.engine.setRows("shopifyShopForStore", []map[string]any{{"id": "shop-row", "currencyCode": "EUR"}})

	invoke(t, operatorContext(context.Background()), NewIntegration(h.conn).handleEnsureSubscriptions, nil)

	if a, b := h.admin.countOp(shopFetchOp), second.countOp(shopFetchOp); a != 1 || b != 1 {
		t.Fatalf("the daily pass read the shops %d and %d times, want each store's once", a, b)
	}
	writes := shopWritesIn(h.engine.calls())
	if len(writes) != 2 || !strings.Contains(strings.Join(writes, "\n"), `"storeId":"`+secondStoreID+`"`) {
		t.Fatalf("the daily pass wrote %v, want both stores' shop rows", writes)
	}
}

func TestAResumeReadsTheStoresShop(t *testing.T) {
	// Through the act.
	h, e := pauseHarness(t, StatusPaused, nil)
	e.row["adminTokenRef"] = "ACME_ADMIN"
	h.admin.reply(shopFetchOp, shopAnswer("EUR", "Europe/Madrid", "2026-08-01T00:00:00Z"))
	got := invoke(t, actorCtx("user-1", auth.RoleWriter), NewIntegration(h.conn).handleStoreSetRecording, map[string]any{"storeId": testStoreID, "recording": true})
	if got["status"] != StatusLive {
		t.Fatalf("the resume answered %v, want live", got)
	}
	if n := h.admin.countOp(shopFetchOp); n != 1 {
		t.Fatalf("the act that resumed the store read its shop %d times, want once", n)
	}
	if writes := shopWritesIn(h.engine.calls()); len(writes) != 1 {
		t.Fatalf("the resume wrote %v, want the store's shop row", writes)
	}

	// Through the watch's pass that sees the store leave its pause.
	h, e = pauseHarness(t, StatusLive, nil)
	e.row["adminTokenRef"] = "ACME_ADMIN"
	h.admin.reply(shopFetchOp, shopAnswer("EUR", "Europe/Madrid", "2026-08-01T00:00:00Z"))
	v := Verdict{ResumedAt: watchNow.Add(-time.Minute)}
	if err := h.conn.resumeSeenByWatch(context.Background(), h.store(t), map[string]any{"resumedAt": ""}, v); err != nil {
		t.Fatalf("the watch's resume edge: %v", err)
	}
	if n := h.admin.countOp(shopFetchOp); n != 1 {
		t.Fatalf("the watch that saw the store resume read its shop %d times, want once", n)
	}
	// A pass after that one, which has recorded the resume, reads nothing.
	if err := h.conn.resumeSeenByWatch(context.Background(), h.store(t), map[string]any{"resumedAt": timeOrEmpty(v.ResumedAt)}, v); err != nil {
		t.Fatalf("a later pass: %v", err)
	}
	if n := h.admin.countOp(shopFetchOp); n != 1 {
		t.Errorf("a pass that sees no resume read the shop again (%d reads in all)", n)
	}
}

func TestAShopThatCannotBeReadStopsNothing(t *testing.T) {
	// The connect still connects.
	w := newWriteHarness(t)
	w.admin.httpStatus(shopFetchOp, http.StatusForbidden)
	if got := w.write(credentialSourcePending); got != connectResultConnected {
		t.Fatalf("a connect whose shop could not be read answered %q, want connected", got)
	}
	if n := w.admin.countOp(shopFetchOp); n != 1 {
		t.Errorf("the connect asked for the shop %d times, want once", n)
	}
	if !strings.Contains(w.logs.String(), "could not read the store's shop") || !strings.Contains(w.logs.String(), connectStoreID) {
		t.Errorf("the failure was not logged with the store's id:\n%s", w.logs.String())
	}

	// The resume still resumes.
	h, e := pauseHarness(t, StatusPaused, nil)
	e.row["adminTokenRef"] = "ACME_ADMIN"
	h.admin.httpStatus(shopFetchOp, http.StatusForbidden)
	got := invoke(t, actorCtx("user-1", auth.RoleWriter), NewIntegration(h.conn).handleStoreSetRecording, map[string]any{"storeId": testStoreID, "recording": true})
	if got["status"] != StatusLive || h.admin.countOp(shopFetchOp) != 1 {
		t.Fatalf("a resume whose shop could not be read answered %v after %d reads, want live after one", got, h.admin.countOp(shopFetchOp))
	}
	// And the watch's edge asks again, and answers no error for it: the
	// verdict is recorded.
	if err := h.conn.resumeSeenByWatch(context.Background(), h.store(t), map[string]any{"resumedAt": ""}, Verdict{ResumedAt: watchNow}); err != nil {
		t.Errorf("the watch's edge failed for a shop it could not read: %v", err)
	}
	if n := h.admin.countOp(shopFetchOp); n != 2 {
		t.Errorf("the watch's edge did not try the shop it could not read: %d reads in all, want the act's and the edge's", n)
	}

	// The daily pass still passes, and the next store is still read.
	h = newHarness(t)
	second := withTwoStores(t, h)
	for _, a := range []*fakeAdmin{h.admin, second} {
		webhookPassAnswers(a)
	}
	h.admin.httpStatus(shopFetchOp, http.StatusForbidden)
	second.reply(shopFetchOp, shopAnswer("USD", "America/Chicago", "2026-08-02T00:00:00Z"))
	reply := invoke(t, operatorContext(context.Background()), NewIntegration(h.conn).handleEnsureSubscriptions, nil)
	if reply["status"] != "reconciled" {
		t.Fatalf("the daily pass answered %v, want reconciled", reply)
	}
	if n := second.countOp(shopFetchOp); n != 1 {
		t.Fatalf("the store after the one that failed was read %d times, want once", n)
	}
	if writes := shopWritesIn(h.engine.calls()); len(writes) != 1 || !strings.Contains(writes[0], `"storeId":"`+secondStoreID+`"`) {
		t.Fatalf("wrote %v, want the second store's shop row alone", writes)
	}

	// A clean answer with no shop retires nothing.
	h = newHarness(t)
	h.admin.reply(shopFetchOp, map[string]any{"shop": nil})
	if err := h.conn.refreshShop(context.Background(), h.store(t)); err != nil {
		t.Fatalf("a clean answer with no shop: %v", err)
	}
	if writes := shopWritesIn(h.engine.calls()); len(writes) != 0 {
		t.Errorf("a clean answer with no shop wrote %v", writes)
	}

	// A read that hangs is let go when the context it was given ends.
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(hung.Close)
	t.Cleanup(func() { close(release) })
	h = newHarness(t)
	h.conn.admin.endpoint = func(Store) string { return hung.URL + "/graphql" }
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := h.conn.refreshShop(ctx, h.store(t)); err == nil {
		t.Error("a read that never answered reported no error")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("a hung read held its caller %s after its context ended", took)
	}

	// A read that panics is an error of that store's, not of its caller's.
	h = newHarness(t)
	h.admin.reply(shopFetchOp, shopAnswer("EUR", "Europe/Madrid", "2026-08-01T00:00:00Z"))
	h.conn.engine = &panickingEngine{fakeEngine: h.engine}
	if err := h.conn.refreshShop(context.Background(), h.store(t)); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Errorf("a read that panicked answered %v, want an error saying so", err)
	}

	// A cancelled context reads nothing and fails nothing.
	h = newHarness(t)
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	h.conn.refreshShops(cancelled, false)
	if n := h.admin.countOp(shopFetchOp); n != 0 {
		t.Errorf("a cancelled pass asked the origin %d times", n)
	}
}

func TestAStoreWithoutTheShopsScopeIsNotAsked(t *testing.T) {
	h := newHarness(t)
	h.engine.setRows("stores", []map[string]any{storeRow(testStoreID, "acme-widgets.myshopify.com", StatusLive, map[string]any{
		"scopesGranted": []any{"read_orders", "read_customers"},
	})})
	h.admin.reply(shopFetchOp, shopAnswer("EUR", "Europe/Madrid", "2026-08-01T00:00:00Z"))
	webhookPassAnswers(h.admin)

	if err := h.conn.refreshShop(context.Background(), h.store(t)); err != nil {
		t.Fatalf("a store without the shop's scope: %v", err)
	}
	Bootstrap(context.Background(), NewIntegration(h.conn), nil)
	invoke(t, operatorContext(context.Background()), NewIntegration(h.conn).handleEnsureSubscriptions, nil)
	if n := h.admin.countOp(shopFetchOp); n != 0 {
		t.Errorf("a store whose grant holds none of the shop's scopes was asked for it %d times", n)
	}
}

// panickingEngine panics on the mirror's raw read of a row, which heal makes
// before it writes: a fault inside the read the callers must survive. With
// times set it panics that many times and then answers.
type panickingEngine struct {
	*fakeEngine
	mu    stdsync.Mutex
	times int
	seen  int
}

func (e *panickingEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(strings.TrimSpace(q), "concept==") {
		e.mu.Lock()
		e.seen++
		panics := e.times == 0 || e.seen <= e.times
		e.mu.Unlock()
		if panics {
			panic("the engine fell over")
		}
	}
	return e.fakeEngine.Execute(ctx, q)
}

// A PURGED STORE THAT IS REINSTALLED is configured again (markReconnected),
// with nothing mirrored, the shop included: its connect reads the shop. A
// store a person paused keeps its pause through a reconnect and is not read.
func TestAReinstalledPurgedStoreReadsItsShopAtConnect(t *testing.T) {
	h := reconnectHarness(t, map[string]any{
		"uninstalledAt": "2026-10-01T12:00:00Z", "redactedAt": "2026-10-03T12:00:00Z", "adminTokenRef": "", "status": StatusPaused,
	})
	h.admin.reply(shopFetchOp, shopAnswer("EUR", "Europe/Madrid", "2026-08-01T00:00:00Z"))
	if got := h.write(credentialSourceCurrent); got != connectResultConnected {
		t.Fatalf("result = %q, want connected", got)
	}
	if n := h.admin.countOp(shopFetchOp); n != 1 {
		t.Fatalf("the reinstall of a purged store read its shop %d times, want once", n)
	}
	if h.statementIndex("insert", `"v1:shopify:shop"`, `"currencyCode":"EUR"`) < 0 {
		t.Error("the reinstall of a purged store wrote no shop row")
	}

	h = reconnectHarness(t, map[string]any{"status": StatusPaused})
	h.admin.reply(shopFetchOp, shopAnswer("EUR", "Europe/Madrid", "2026-08-01T00:00:00Z"))
	if got := h.write(credentialSourceCurrent); got != connectResultReconnected {
		t.Fatalf("result = %q, want reconnected", got)
	}
	if n := h.admin.countOp(shopFetchOp); n != 0 {
		t.Errorf("the reconnect of a store a person paused read its shop %d times, want never", n)
	}
}

// THE CONNECT'S SHOP READ IS OFF THE REDIRECT: it runs in the connect's
// background job, on that job's own context, so the browser does not wait on
// it and a request that ends does not take it along.
func TestTheConnectsShopReadDoesNotHoldTheRedirect(t *testing.T) {
	h := newWriteHarness(t)
	h.admin.reply(shopFetchOp, shopAnswer("EUR", "Europe/Madrid", "2026-08-01T00:00:00Z"))
	var deferred []func()
	h.integ.connector.background = func(f func()) { deferred = append(deferred, f) }
	ctx, cancel := context.WithCancel(context.Background())
	got, _ := h.integ.connector.WriteShopifyConnect(ctx, callbackState(credentialSourcePending), h.grant())
	if got != connectResultConnected {
		t.Fatalf("result = %q, want connected", got)
	}
	if n := h.admin.countOp(shopFetchOp); n != 0 {
		t.Fatalf("the shop was read %d times before the callback answered", n)
	}
	if len(deferred) != 1 {
		t.Fatalf("%d background jobs, want the connect's one", len(deferred))
	}
	// The person closes the tab: the request's context ends before the job runs.
	cancel()
	deferred[0]()
	if n := h.admin.countOp(shopFetchOp); n != 1 {
		t.Fatalf("the connect's job read the shop %d times, want once", n)
	}
	if h.statementIndex("insert", `"v1:shopify:shop"`, `"currencyCode":"EUR"`) < 0 {
		t.Error("the connect's job wrote no shop row after the request ended")
	}
}

// A READ THAT PANICS is logged with its stack, under the store's id, and the
// next store is still read.
func TestAPanickingShopReadIsLoggedWithItsStack(t *testing.T) {
	h := newHarness(t)
	second := withTwoStores(t, h)
	h.admin.reply(shopFetchOp, shopAnswer("EUR", "Europe/Madrid", "2026-08-01T00:00:00Z"))
	second.reply(shopFetchOp, shopAnswer("USD", "America/Chicago", "2026-08-02T00:00:00Z"))
	var logs bytes.Buffer
	h.conn.logger = slog.New(slog.NewTextHandler(&logs, nil))
	h.conn.engine = &panickingEngine{fakeEngine: h.engine, times: 1}

	h.conn.refreshShops(context.Background(), false)

	out := logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "store="+testStoreID) || !strings.Contains(out, "runtime/debug.Stack") {
		t.Errorf("the panic was not logged at error level with the store's id and its stack:\n%s", out)
	}
	if n := second.countOp(shopFetchOp); n != 1 {
		t.Fatalf("the store after the one that panicked was read %d times, want once", n)
	}
	if writes := shopWritesIn(h.engine.calls()); len(writes) != 1 || !strings.Contains(writes[0], `"storeId":"`+secondStoreID+`"`) {
		t.Fatalf("wrote %v, want the second store's shop row", writes)
	}
}

// shopRowsEngine answers the mirror's shop read per store: a store in `has`
// has its live row.
type shopRowsEngine struct {
	*fakeEngine
	has map[string]bool
}

func (e shopRowsEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if callName(q) == "shopifyShopForStore" {
		_, _ = e.fakeEngine.Execute(ctx, q)
		for id, has := range e.has {
			if has && strings.Contains(q, `storeId: "`+id+`"`) {
				return resultWithRows([]map[string]any{{"id": "shop-" + id, "storeId": id}}), nil
			}
		}
		return resultWithRows(nil), nil
	}
	return e.fakeEngine.Execute(ctx, q)
}

// One boot of two stores, one with its row and one without, in either order:
// the one with its row is passed over and the one without is still read.
func TestABootOfTwoStoresReadsOnlyTheOneWithoutARow(t *testing.T) {
	for _, withRow := range []string{testStoreID, secondStoreID} {
		h := newHarness(t)
		second := withTwoStores(t, h)
		h.admin.reply(shopFetchOp, shopAnswer("EUR", "Europe/Madrid", "2026-08-01T00:00:00Z"))
		second.reply(shopFetchOp, shopAnswer("USD", "America/Chicago", "2026-08-02T00:00:00Z"))
		h.conn.engine = shopRowsEngine{fakeEngine: h.engine, has: map[string]bool{withRow: true}}

		Bootstrap(context.Background(), NewIntegration(h.conn), nil)

		first, other := h.admin.countOp(shopFetchOp), second.countOp(shopFetchOp)
		want := [2]int{0, 1}
		if withRow == secondStoreID {
			want = [2]int{1, 0}
		}
		if first != want[0] || other != want[1] {
			t.Errorf("%s has its row: the boot read the shops %d and %d times, want %v", withRow, first, other, want)
		}
	}
}
