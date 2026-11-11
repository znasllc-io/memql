package shopify

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// caller_gate_test.go -- who may reach a store through a builtin a client can
// call.
//
// A builtin with no @requiresCapability has no rank floor, and one with it
// says only that the caller holds a part; neither says which store. So each
// handler that takes a storeId asks it of the caller itself, before any read
// other than the store row's and before any call to Shopify, and a store that
// exists and one that does not answer a refused caller alike.

const gateOtherStoreID = "beta"

// withTwoStores configures a second store, owned by somebody else.
func withTwoGateStores(h *testHarness, over map[string]any) {
	acme := map[string]any{
		"id": testStoreID, "domain": "acme-widgets.myshopify.com", "name": "Acme Widgets",
		"adminTokenRef": "ACME_ADMIN", "webhookSecretRef": "ACME_WEBHOOK", "storefrontTokenRef": "ACME_ADMIN",
		"apiVersion": generated.APIVersion, "protectedDataLevel": ProtectedLevel2, "status": StatusLive, "ownerUserId": "user-1",
	}
	for k, v := range over {
		acme[k] = v
	}
	h.engine.setRows("stores", []map[string]any{acme, {
		"id": gateOtherStoreID, "domain": "beta-goods.myshopify.com", "name": "Beta Goods",
		"adminTokenRef": "ACME_ADMIN", "webhookSecretRef": "ACME_WEBHOOK", "storefrontTokenRef": "ACME_ADMIN",
		"apiVersion": generated.APIVersion, "protectedDataLevel": ProtectedLevel2, "status": StatusLive, "ownerUserId": "user-3",
	}})
}

// clusterOwnerCall is a cluster owner's call: who reads every store's health.
func clusterOwnerCall() context.Context { return actorCtx("user-9", auth.RoleOwner) }

// withStoreRow replaces the harness store with one row of the given fields.
func withStoreRow(h *testHarness, over map[string]any) {
	row := map[string]any{
		"id": testStoreID, "domain": "acme-widgets.myshopify.com", "name": "Acme Widgets",
		"adminTokenRef": "ACME_ADMIN", "webhookSecretRef": "ACME_WEBHOOK", "storefrontTokenRef": "ACME_ADMIN",
		"apiVersion": generated.APIVersion, "protectedDataLevel": ProtectedLevel2, "status": StatusLive, "ownerUserId": "user-1",
	}
	for k, v := range over {
		row[k] = v
	}
	h.engine.setRows("stores", []map[string]any{row})
}

// readsStores puts the store tier's row-free read answer in front of the
// harness engine: a cluster owner or a developer may read stores.
func readsStores(h *testHarness) *connectEngine {
	e := &connectEngine{fakeEngine: h.engine, writable: map[string]bool{}}
	h.conn.engine = e
	return e
}

// onlyTheStoreRowWasRead fails when anything but the store row was read.
func onlyTheStoreRowWasRead(t *testing.T, h *testHarness, what string) {
	t.Helper()
	ran := map[string]bool{}
	for _, call := range h.engine.calls() {
		if name := callName(call); name != "storesForConnector" && !ran[name] {
			ran[name] = true
			t.Errorf("%s: %s ran; only the store row may be read", what, name)
		}
	}
	if n := len(h.admin.seen()); n != 0 {
		t.Errorf("%s: Shopify's Admin API was asked %d times", what, n)
	}
}

// storefrontCalls counts the requests that leave for a Storefront API, which
// the probe makes through http.DefaultClient.
type storefrontCalls struct {
	mu stdsync.Mutex
	n  int
}

func (s *storefrontCalls) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}, Request: req}, nil
}

func (s *storefrontCalls) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func countStorefrontCalls(t *testing.T) *storefrontCalls {
	t.Helper()
	calls := &storefrontCalls{}
	was := http.DefaultClient.Transport
	http.DefaultClient.Transport = calls
	t.Cleanup(func() { http.DefaultClient.Transport = was })
	return calls
}

func TestSetRecordingIsForTheStoresOwnerOrAClusterOwner(t *testing.T) {
	refusal := "shopify: shopifyStoreSetRecording " + actRefusal
	for _, storeID := range []string{testStoreID, "no-such-store"} {
		for _, who := range []context.Context{actorCtx("user-2", auth.RoleDeveloper), actorCtx("user-2", auth.RoleWriter)} {
			h := newHarness(t)
			_, err := NewIntegration(h.conn).handleStoreSetRecording(who, map[string]any{"storeId": storeID, "recording": false}, 0)
			if err == nil || err.Error() != refusal {
				t.Errorf("storeId %q: err = %v, want %q", storeID, err, refusal)
			}
			onlyTheStoreRowWasRead(t, h, "a refused pause of "+storeID)
		}
	}

	for _, who := range []context.Context{actorCtx("user-1", auth.RoleDeveloper), actorCtx("user-9", auth.RoleOwner)} {
		h := newHarness(t)
		e := readsStores(h)
		reply := invoke(t, who, NewIntegration(h.conn).handleStoreSetRecording, map[string]any{"storeId": testStoreID, "recording": false})
		if reply["status"] != StatusPaused || reply["storeId"] != testStoreID {
			t.Errorf("reply = %v, want {status: paused, storeId: %s}", reply, testStoreID)
		}
		writes := e.callsNamed("setStoreStatus")
		if len(writes) != 1 {
			t.Fatalf("setStoreStatus ran %d times, want 1", len(writes))
		}
		if !strings.Contains(writes[0].query, `status: "paused"`) || !strings.Contains(writes[0].query, `storeId: "`+testStoreID+`"`) {
			t.Errorf("the write = %s", writes[0].query)
		}
		// Written as the deployment, so the store row's own gate -- a cluster
		// owner's -- is met without being widened.
		if !writes[0].internal || !auth.IsSystemActorId(writes[0].userId) {
			t.Errorf("setStoreStatus ran as %q (internal %t), want the operator", writes[0].userId, writes[0].internal)
		}
	}
}

// The operator's write is reached only past the store part, which the engine
// asks at the builtin before the handler runs, and the caller check in the
// handler: the declaration is the first of the two.
func TestSetRecordingDeclaresTheStorePart(t *testing.T) {
	fn, err := newRealEngine(t).Functions().Get("shopifyStoreSetRecording")
	if err != nil || fn == nil || !fn.IsBuiltin() {
		t.Fatalf("shopifyStoreSetRecording is not a builtin in the function registry: %v", err)
	}
	if req := fn.RequiresCapability; req.Verb != "execute" || req.Resource != "app:deployables/store" {
		t.Errorf("shopifyStoreSetRecording requires %+v, want execute app:deployables/store", req)
	}
}

func TestSetRecordingMovesOnlyBetweenLiveAndPaused(t *testing.T) {
	refusal := "shopify: a store that is not connected cannot be paused or resumed"
	for name, row := range map[string]map[string]any{
		"uninstalled": {"uninstalledAt": "2026-08-20T10:00:00Z"},
		"purged":      {"redactedAt": "2026-08-20T10:00:00Z", "status": StatusPaused},
		"configured":  {"status": StatusConfigured},
		"backfilling": {"status": StatusBackfilling},
		"error":       {"status": StatusError},
	} {
		for _, recording := range []bool{true, false} {
			h := newHarness(t)
			withStoreRow(h, row)
			_, err := NewIntegration(h.conn).handleStoreSetRecording(actorCtx("user-1", auth.RoleDeveloper), map[string]any{"storeId": testStoreID, "recording": recording}, 0)
			if err == nil || err.Error() != refusal {
				t.Errorf("%s, recording %t: err = %v, want %q", name, recording, err, refusal)
			}
			if w := h.engine.callsTo("setStoreStatus"); len(w) != 0 {
				t.Errorf("%s: a store that is not connected was written: %v", name, w)
			}
		}
	}

	h := newHarness(t)
	withStoreRow(h, map[string]any{"status": StatusPaused})
	reply := invoke(t, actorCtx("user-1", auth.RoleDeveloper), NewIntegration(h.conn).handleStoreSetRecording, map[string]any{"storeId": testStoreID, "recording": true})
	if reply["status"] != StatusLive {
		t.Errorf("resume answered %v, want live", reply)
	}
	if w := h.engine.callsTo("setStoreStatus"); len(w) != 1 || !strings.Contains(w[0], `status: "live"`) {
		t.Errorf("resume wrote %v, want one setStoreStatus to live", w)
	}
}

func TestSetRecordingToTheSameStateWritesNothing(t *testing.T) {
	for status, recording := range map[string]bool{StatusLive: true, StatusPaused: false} {
		h := newHarness(t)
		withStoreRow(h, map[string]any{"status": status})
		reply := invoke(t, actorCtx("user-1", auth.RoleDeveloper), NewIntegration(h.conn).handleStoreSetRecording, map[string]any{"storeId": testStoreID, "recording": recording})
		if reply["status"] != status || reply["storeId"] != testStoreID {
			t.Errorf("%s: reply = %v, want {status: %s, storeId: %s}", status, reply, status, testStoreID)
		}
		if w := h.engine.callsTo("setStoreStatus"); len(w) != 0 {
			t.Errorf("%s: asking for the state the store is in wrote %v", status, w)
		}
	}
}

func TestShopifyqlRefusesANonOwnerBeforeItCallsShopify(t *testing.T) {
	refusal := "shopify: shopifyql " + actRefusal
	for _, storeID := range []string{testStoreID, "no-such-store"} {
		h := newHarness(t)
		withStoreRow(h, nil)
		h.admin.reply("ShopifyQL", map[string]any{"shopifyqlQuery": map[string]any{"__typename": "TableResponse"}})
		_, err := NewIntegration(h.conn).handleShopifyQL(actorCtx("user-2", auth.RoleDeveloper), map[string]any{"storeId": storeID, "query": "FROM sales SHOW total_sales"}, 0)
		if err == nil || err.Error() != refusal {
			t.Errorf("storeId %q: err = %v, want %q", storeID, err, refusal)
		}
		onlyTheStoreRowWasRead(t, h, "a refused shopifyql on "+storeID)
	}

	// The counter can say otherwise: the store's owner reaches Shopify.
	h := newHarness(t)
	withStoreRow(h, nil)
	h.admin.reply("ShopifyQL", map[string]any{"shopifyqlQuery": map[string]any{"__typename": "TableResponse"}})
	invoke(t, actorCtx("user-1", auth.RoleWriter), NewIntegration(h.conn).handleShopifyQL, map[string]any{"storeId": testStoreID, "query": "FROM sales SHOW total_sales"})
	if n := h.admin.countOp("ShopifyQL"); n != 1 {
		t.Errorf("the store's owner reached Shopify %d times, want 1", n)
	}
}

func TestTheStorefrontProbeRefusesACallerWhoMayNotReadTheStore(t *testing.T) {
	refusal := "shopify: storefrontProbe is for the store's owner, a cluster owner, or a caller who may read stores"
	calls := countStorefrontCalls(t)
	for _, storeID := range []string{testStoreID, "no-such-store"} {
		h := newHarness(t)
		withStoreRow(h, nil)
		readsStores(h)
		_, err := NewIntegration(h.conn).handleStorefrontProbe(actorCtx("user-2", auth.RoleWriter), map[string]any{"storeId": storeID}, 0)
		if err == nil || err.Error() != refusal {
			t.Errorf("storeId %q: err = %v, want %q", storeID, err, refusal)
		}
		onlyTheStoreRowWasRead(t, h, "a refused probe of "+storeID)
	}
	if n := calls.count(); n != 0 {
		t.Fatalf("a refused probe made %d Storefront calls", n)
	}

	// The store's owner, a cluster owner and a caller who may read stores
	// each reach the Storefront API: the counter can say otherwise.
	for _, who := range []context.Context{actorCtx("user-1", auth.RoleWriter), actorCtx("user-9", auth.RoleOwner), actorCtx("user-5", auth.RoleDeveloper)} {
		h := newHarness(t)
		withStoreRow(h, nil)
		readsStores(h)
		before := calls.count()
		invoke(t, who, NewIntegration(h.conn).handleStorefrontProbe, map[string]any{"storeId": testStoreID})
		if calls.count() == before {
			ac, _ := auth.AccessFromContext(who)
			t.Errorf("%s made no Storefront call", ac.UserId)
		}
	}
}

func storeIDsOf(t *testing.T, reply map[string]any) []string {
	t.Helper()
	list, _ := reply["stores"].([]any)
	ids := []string{}
	for _, entry := range list {
		m, _ := entry.(map[string]any)
		ids = append(ids, fmt.Sprint(m["storeId"]))
	}
	return ids
}

func TestStoreHealthAnswersOnlyTheStoresTheCallerMayRead(t *testing.T) {
	for _, tc := range []struct {
		who     context.Context
		name    string
		storeID string
		want    string
	}{
		{actorCtx("user-1", auth.RoleWriter), "the owner of one", "", testStoreID},
		{actorCtx("user-1", auth.RoleWriter), "the owner of one, naming the other", gateOtherStoreID, ""},
		{actorCtx("user-9", auth.RoleOwner), "a cluster owner", "", testStoreID + "," + gateOtherStoreID},
		{actorCtx("user-5", auth.RoleDeveloper), "a caller who may read stores", "", testStoreID + "," + gateOtherStoreID},
		{actorCtx("user-2", auth.RoleWriter), "another person", "", ""},
		{actorCtx("user-2", auth.RoleWriter), "another person, naming a store", testStoreID, ""},
		{actorCtx("user-2", auth.RoleWriter), "another person, naming no such store", "no-such-store", ""},
	} {
		h := newHarness(t)
		withTwoGateStores(h, nil)
		readsStores(h)
		args := map[string]any{}
		if tc.storeID != "" {
			args["storeId"] = tc.storeID
		}
		reply := invoke(t, tc.who, NewIntegration(h.conn).handleStoreHealth, args)
		if got := strings.Join(storeIDsOf(t, reply), ","); got != tc.want {
			t.Errorf("%s: stores %q, want %q", tc.name, got, tc.want)
		}
		if tc.want == "" {
			onlyTheStoreRowWasRead(t, h, tc.name)
		}
	}
}

func TestTheTwoPassesAreForACallWithInternalOriginOrAClusterOwner(t *testing.T) {
	type pass struct {
		builtin string
		handle  mirrorHandler
		reads   string
	}
	passes := func(integ *Integration) []pass {
		return []pass{
			{"shopifyEnsureSubscriptions", integ.handleEnsureSubscriptions, "storesForConnector"},
			{"shopifyRunComplianceJobs", integ.handleRunComplianceJobs, "complianceJobsDue"},
		}
	}
	for i := range passes(nil) {
		// Refused: a writer who owns the store, a developer, nobody at all.
		for _, who := range []context.Context{actorCtx("user-1", auth.RoleWriter), actorCtx("user-5", auth.RoleDeveloper), context.Background()} {
			h := newHarness(t)
			p := passes(NewIntegration(h.conn))[i]
			_, err := p.handle(who, nil, 0)
			if want := "shopify: " + p.builtin + " is for a cluster owner"; err == nil || err.Error() != want {
				t.Errorf("%s: err = %v, want %q", p.builtin, err, want)
			}
			if calls := h.engine.calls(); len(calls) != 0 {
				t.Errorf("%s: a refused call reached the engine: %v", p.builtin, calls)
			}
			if n := len(h.admin.seen()); n != 0 {
				t.Errorf("%s: a refused call asked Shopify %d times", p.builtin, n)
			}
		}
		// Admitted: the scheduled automation's internal origin, and a cluster owner.
		for _, who := range []context.Context{auth.ContextWithInternalOrigin(context.Background()), actorCtx("user-9", auth.RoleOwner)} {
			h := newHarness(t)
			p := passes(NewIntegration(h.conn))[i]
			if _, err := p.handle(who, nil, 0); err != nil && strings.Contains(err.Error(), "is for a cluster owner") {
				t.Errorf("%s: an admitted call was refused: %v", p.builtin, err)
			}
			if len(h.engine.callsTo(p.reads)) == 0 {
				t.Errorf("%s: an admitted call did not run (no %s)", p.builtin, p.reads)
			}
		}
	}
}

func TestANonOwnerWhoNamesNoStoreIsNotToldHowManyThereAre(t *testing.T) {
	// No probe leaves for a real store.
	countStorefrontCalls(t)
	cases := []struct {
		builtin string
		refusal string
		handle  func(*Integration) mirrorHandler
		args    map[string]any
	}{
		{"commerceSold", actRefusal, func(i *Integration) mirrorHandler { return i.handleCommerceSold }, map[string]any{}},
		{"shopifyFetchProduct", actRefusal, func(i *Integration) mirrorHandler { return i.handleFetchProduct }, map[string]any{"handle": "linen-shirt"}},
		{"shopifyql", actRefusal, func(i *Integration) mirrorHandler { return i.handleShopifyQL }, map[string]any{"query": "FROM sales SHOW total_sales"}},
		{"storefrontProbe", "is for the store's owner, a cluster owner, or a caller who may read stores", func(i *Integration) mirrorHandler { return i.handleStorefrontProbe }, map[string]any{}},
	}
	for _, tc := range cases {
		// Two stores, neither the caller's: the refusal, and no count.
		h := newHarness(t)
		withTwoGateStores(h, nil)
		_, err := tc.handle(NewIntegration(h.conn))(actorCtx("user-2", auth.RoleWriter), tc.args, 0)
		if want := "shopify: " + tc.builtin + " " + tc.refusal; err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", tc.builtin, err, want)
		}
		onlyTheStoreRowWasRead(t, h, tc.builtin+" for a caller who owns neither store")

		// Two stores, one the caller's: that one.
		h = newHarness(t)
		withTwoGateStores(h, nil)
		h.admin.reply("ShopifyQL", map[string]any{"shopifyqlQuery": map[string]any{"__typename": "TableResponse"}})
		reply := invoke(t, actorCtx("user-1", auth.RoleWriter), tc.handle(NewIntegration(h.conn)), tc.args)
		if got := fmt.Sprint(reply["storeId"]); got != testStoreID {
			t.Errorf("%s: the caller's own store was not the default: storeId %q, reply %v", tc.builtin, got, reply)
		}

		// A cluster owner may be told.
		h = newHarness(t)
		withTwoGateStores(h, nil)
		_, err = tc.handle(NewIntegration(h.conn))(actorCtx("user-9", auth.RoleOwner), tc.args, 0)
		if err == nil || !strings.Contains(err.Error(), "2 stores are configured") {
			t.Errorf("%s: a cluster owner who names no store of two: err = %v", tc.builtin, err)
		}
	}
}

// pagedProductEngine answers shopifyProductForStore in pages of 100, the
// cursor naming the next page.
type pagedProductEngine struct {
	*fakeEngine
	pages  [][]map[string]any
	repeat bool // every page names the same cursor
}

func (e *pagedProductEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if callName(q) != "shopifyProductForStore" {
		return e.fakeEngine.Execute(ctx, q)
	}
	e.fakeEngine.mu.Lock()
	e.fakeEngine.executed = append(e.fakeEngine.executed, q)
	e.fakeEngine.mu.Unlock()
	page := 0
	if c := memql.CursorFromContext(ctx); c != "" {
		fmt.Sscanf(c, "page-%d", &page)
	}
	res := resultWithRows(e.pages[page])
	switch {
	case e.repeat:
		res.Meta = &memql.ResultMeta{Cursor: "page-1", HasMore: true}
	case page+1 < len(e.pages):
		res.Meta = &memql.ResultMeta{Cursor: fmt.Sprintf("page-%d", page+1), HasMore: true}
	}
	return res, nil
}

func productPages(n int, last string) [][]map[string]any {
	var pages [][]map[string]any
	for p := 0; p < n; p++ {
		rows := make([]map[string]any, 0, 100)
		for r := 0; r < 100; r++ {
			rows = append(rows, map[string]any{"id": fmt.Sprintf("p%d-%d", p, r), "gid": fmt.Sprintf("gid://shopify/Product/%d%03d", p, r), "handle": fmt.Sprintf("product-%d-%d", p, r)})
		}
		pages = append(pages, rows)
	}
	if last != "" {
		pages = append(pages, []map[string]any{{"id": "the-one", "gid": "gid://shopify/Product/9", "handle": last}})
	}
	return pages
}

func TestAProductIsFoundByItsHandlePastTheFirstPage(t *testing.T) {
	h := newHarness(t)
	engine := &pagedProductEngine{fakeEngine: h.engine, pages: productPages(3, "linen-shirt")}
	h.conn.engine = engine
	reply := invoke(t, actorCtx("user-1", auth.RoleWriter), NewIntegration(h.conn).handleFetchProduct, map[string]any{"storeId": testStoreID, "handle": "linen-shirt"})
	if mapString(reply, "handle") != "linen-shirt" || mapString(reply, "gid") != "gid://shopify/Product/9" {
		t.Errorf("a product on the fourth page was not found: %v", reply)
	}
	if n := len(h.engine.callsTo("shopifyProductForStore")); n != 4 {
		t.Errorf("read %d pages, want 4", n)
	}

	// Absent from every page: not mirrored, after the last page.
	h = newHarness(t)
	h.conn.engine = &pagedProductEngine{fakeEngine: h.engine, pages: productPages(2, "")}
	reply = invoke(t, actorCtx("user-1", auth.RoleWriter), NewIntegration(h.conn).handleFetchProduct, map[string]any{"storeId": testStoreID, "handle": "linen-shirt"})
	if reply["status"] != "not_mirrored" {
		t.Errorf("a product on no page answered %v", reply)
	}

	// A cursor that does not move is said, never read as not mirrored.
	h = newHarness(t)
	h.conn.engine = &pagedProductEngine{fakeEngine: h.engine, pages: productPages(2, ""), repeat: true}
	if _, err := NewIntegration(h.conn).handleFetchProduct(actorCtx("user-1", auth.RoleWriter), map[string]any{"storeId": testStoreID, "handle": "linen-shirt"}, 0); err == nil {
		t.Error("a read whose cursor did not advance answered as a whole read")
	}
}

func TestAnAppScopedListingIsNeverTombstoned(t *testing.T) {
	for _, concept := range []string{"sellingPlanGroup", "marketingActivity", "marketingEvent"} {
		if _, partial := relistListsPartially[concept]; !partial {
			t.Errorf("%s is not named as listed partially", concept)
			continue
		}
		spec := generated.Types[concept]
		if spec == nil || spec.Reconcile != generated.ReconcileFullRelist {
			t.Fatalf("%s is not a full re-list domain", concept)
		}
		h := newHarness(t)
		h.admin.reply(spec.ListOp, listPageReply(spec.ListQuery, false, "",
			map[string]any{"id": "gid://shopify/" + spec.GraphQLType + "/1"}))
		h.engine.setRows(spec.ForStoreFn, []map[string]any{
			{"id": "shp2", "gid": "gid://shopify/" + spec.GraphQLType + "/2"},
		})
		if _, err := h.conn.relist(context.Background(), h.store(t), spec); err != nil {
			t.Fatalf("%s: %v", concept, err)
		}
		if retired := retiredCalls(h); len(retired) != 0 {
			t.Errorf("%s: an app-scoped listing retired a row:\n%s", concept, retired[0])
		}
		if reads := h.engine.callsTo(spec.ForStoreFn); len(reads) != 0 {
			t.Errorf("%s: the live rows were read for a pass that may not tombstone", concept)
		}
	}
}
