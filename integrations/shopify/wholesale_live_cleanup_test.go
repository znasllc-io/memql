package shopify

import (
	"context"
	"fmt"
	"slices"
	"strings"
	stdsync "sync"
	"testing"
	"time"
)

// wholesale_live_cleanup_test.go -- the live tests' cleanup, held to the
// package's fake Admin API, because a cleanup that silently leaves a customer
// in a development store is the one failure a live run cannot show.

// liveWholesaleOnFake is the live harness over h's fake Admin: the same
// transport, cleanup and development-store check, with no retry on a 5xx (so a
// lost answer stays lost) and a short search wait.
func liveWholesaleOnFake(t *testing.T, h *testHarness, logs *liveLogs) *liveWholesale {
	t.Helper()
	h.admin.reply("LiveTestShopPlan", map[string]any{"shop": map[string]any{"plan": map[string]any{"partnerDevelopment": true}}})
	h.conn.admin.MaxRetries = 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	w := newLiveWholesaleOn(t, &liveHarness{
		engine: h.engine, conn: h.conn, ctx: ctx, ids: []string{testStoreID},
		vars: map[string]string{testStoreID: envLiveDomain}, developmentChecked: map[string]bool{},
	})
	w.logf = logs.logf
	w.findTries, w.findWait = 2, time.Millisecond
	return w
}

// liveLogs keeps what the cleanup said.
type liveLogs struct {
	mu    stdsync.Mutex
	lines []string
}

func (l *liveLogs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *liveLogs) said(want string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.ContainsFunc(l.lines, func(line string) bool { return strings.Contains(line, want) })
}

// deletableCustomers answers the cleanup's customerDelete on fc's list.
func deletableCustomers(h *testHarness, fc *fakeCustomers) {
	h.admin.replyWith("LiveTestCustomerDelete", func(vars map[string]any) any {
		fc.mu.Lock()
		defer fc.mu.Unlock()
		input, _ := vars["input"].(map[string]any)
		id, _ := input["id"].(string)
		fc.list = slices.DeleteFunc(fc.list, func(c fakeCustomer) bool { return c.id == id })
		return map[string]any{"customerDelete": map[string]any{"deletedCustomerId": id, "userErrors": []any{}}}
	})
}

func (fc *fakeCustomers) count() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return len(fc.list)
}

// A grant whose create succeeded and whose tag write failed answers the
// handler an error and no customer id; the cleanup still deletes the customer
// the transport saw created, and says so.
func TestTheLiveCleanupDeletesACustomerWhoseTagWriteFailed(t *testing.T) {
	h := newHarness(t)
	fc := customersOn(h)
	deletableCustomers(h, fc)
	h.admin.graphQLError("ShopifyTagsAdd", "INTERNAL_SERVER_ERROR", "tagsAdd failed")
	logs := &liveLogs{}
	t.Run("the test", func(t *testing.T) {
		w := liveWholesaleOnFake(t, h, logs)
		nodes, err := w.integ.handleTagWholesaleCustomer(w.packCall(), map[string]any{
			"storeId": testStoreID, "buyerEmail": "wholesale-live-tagfails@example.com", "buyerName": liveBuyerName,
		}, 0)
		if err == nil || nodes != nil {
			t.Fatalf("the grant answered %v, %v; want the tag write's error and no reply", nodes, err)
		}
		if fc.count() != 1 {
			t.Fatalf("the fake holds %d customers after the create, want 1", fc.count())
		}
	})
	if n := fc.count(); n != 0 {
		t.Fatalf("the cleanup left %d customers behind (said %v)", n, logs.lines)
	}
	if !logs.said("deleted customer gid://shopify/Customer/101") {
		t.Fatalf("the cleanup did not say what it deleted: %v", logs.lines)
	}
}

// A create whose answer was lost after the origin committed it is found by the
// test's own address and deleted.
func TestTheLiveCleanupFindsALostCreateByItsAddress(t *testing.T) {
	h := newHarness(t)
	fc := customersOn(h)
	deletableCustomers(h, fc)
	h.admin.httpStatus("ShopifyCustomerCreate", 502)
	logs := &liveLogs{}
	t.Run("the test", func(t *testing.T) {
		w := liveWholesaleOnFake(t, h, logs)
		if _, err := w.h.conn.createCustomer(w.h.ctx, w.store, liveBuyerName, "wholesale-live-lost@example.com"); err == nil {
			t.Fatal("a create answered 502 was not an error")
		}
		if fc.count() != 1 {
			t.Fatalf("the fake committed %d customers, want 1", fc.count())
		}
	})
	if n := fc.count(); n != 0 {
		t.Fatalf("the cleanup left %d customers behind (said %v)", n, logs.lines)
	}
	if !logs.said("deleted customer gid://shopify/Customer/101") {
		t.Fatalf("the cleanup did not say what it deleted: %v", logs.lines)
	}
}

// A create whose answer was lost and which the search does not find is named
// in a line a person can act on, never passed over.
func TestTheLiveCleanupSaysWhenALostCreateIsNotFound(t *testing.T) {
	h := newHarness(t)
	customersOn(h)
	// The answer is lost, and nothing the search can see was created.
	h.admin.replyWith("ShopifyCustomerCreate", func(map[string]any) any { return map[string]any{} })
	h.admin.httpStatus("ShopifyCustomerCreate", 502)
	h.admin.httpStatus("ShopifyCompanyCreate", 502)
	h.admin.reply("ShopifyCompanyByExternalID", map[string]any{"companies": map[string]any{"nodes": []any{}}})
	logs := &liveLogs{}
	t.Run("the test", func(t *testing.T) {
		w := liveWholesaleOnFake(t, h, logs)
		_, _ = w.h.conn.createCustomer(w.h.ctx, w.store, liveBuyerName, "wholesale-live-vanished@example.com")
		_, _, _ = w.h.conn.createCompany(w.h.ctx, w.store, WholesaleAccountInput{ApplicationID: "app-vanished", CompanyName: "Vanished"},
			wholesaleExternalIDPrefix+"v1:wholesale:application:app-vanished")
	})
	for _, want := range []string{
		"LEFT IN THE STORE, perhaps: a customer for wholesale-live-vanished@example.com",
		"LEFT IN THE STORE, perhaps: a company with the external id memql-wholesale:v1:wholesale:application:app-vanished",
	} {
		if !logs.said(want) {
			t.Errorf("the cleanup did not say %q: %v", want, logs.lines)
		}
	}
}

// A create the origin refused created nothing, so the cleanup neither deletes
// nor searches for it.
func TestTheLiveCleanupLeavesARefusedCreateAlone(t *testing.T) {
	h := newHarness(t)
	customersOn(h)
	h.admin.replyWith("ShopifyCustomerCreate", func(map[string]any) any {
		return map[string]any{"customerCreate": map[string]any{"customer": nil,
			"userErrors": []any{map[string]any{"field": []any{"email"}, "message": "Email is invalid"}}}}
	})
	logs := &liveLogs{}
	var w *liveWholesale
	t.Run("the test", func(t *testing.T) {
		w = liveWholesaleOnFake(t, h, logs)
		_, _ = w.h.conn.createCustomer(w.h.ctx, w.store, liveBuyerName, "wholesale-live-refused@example.com")
	})
	if got := w.calls.created(); len(got) != 0 {
		t.Fatalf("a refused create was kept as %+v", got)
	}
	if len(logs.lines) != 0 {
		t.Fatalf("the cleanup said %v for a test that created nothing", logs.lines)
	}
}

// A 200 that carries GraphQL errors and no payload is as ambiguous as a 5xx:
// the origin may have committed the create. The cleanup finds the customer by
// its address and deletes it.
func TestTheLiveCleanupFindsACreateAnsweredWithAnInternalError(t *testing.T) {
	h := newHarness(t)
	fc := customersOn(h)
	deletableCustomers(h, fc)
	create := h.admin.replyFns["ShopifyCustomerCreate"]
	h.admin.replyWith("ShopifyCustomerCreate", func(vars map[string]any) any {
		create(vars) // committed
		return map[string]any{"customerCreate": nil}
	})
	h.admin.graphQLError("ShopifyCustomerCreate", "INTERNAL_SERVER_ERROR", "Internal error. Looks like something went wrong on our end.")
	logs := &liveLogs{}
	t.Run("the test", func(t *testing.T) {
		w := liveWholesaleOnFake(t, h, logs)
		if _, err := w.h.conn.createCustomer(w.h.ctx, w.store, liveBuyerName, "wholesale-live-internal@example.com"); err == nil {
			t.Fatal("a create answered INTERNAL_SERVER_ERROR was not an error")
		}
		if fc.count() != 1 {
			t.Fatalf("the fake committed %d customers, want 1", fc.count())
		}
	})
	if n := fc.count(); n != 0 {
		t.Fatalf("the cleanup left %d customers behind (said %v)", n, logs.lines)
	}
	if !logs.said("deleted customer gid://shopify/Customer/101") {
		t.Fatalf("the cleanup did not say what it deleted: %v", logs.lines)
	}
}

// A create refused for access at status 200 (ACCESS_DENIED, no payload) is
// looked for by the run's own address like any unanswered create; finding
// nothing is the expected outcome, said calmly, not an alarm.
func TestTheLiveCleanupIsCalmAboutACreateRefusedForAccess(t *testing.T) {
	h := newHarness(t)
	customersOn(h)
	h.admin.replyWith("ShopifyCustomerCreate", func(map[string]any) any { return map[string]any{"customerCreate": nil} })
	h.admin.graphQLError("ShopifyCustomerCreate", "ACCESS_DENIED", "Access denied for customerCreate field. Required access: `write_customers` access scope.")
	logs := &liveLogs{}
	t.Run("the test", func(t *testing.T) {
		w := liveWholesaleOnFake(t, h, logs)
		_, _ = w.h.conn.createCustomer(w.h.ctx, w.store, liveBuyerName, "wholesale-live-denied@example.com")
	})
	if logs.said("LEFT IN THE STORE") {
		t.Fatalf("a create refused for access was reported as left behind: %v", logs.lines)
	}
	if !logs.said("refused for access") {
		t.Fatalf("the cleanup did not say the refused create was looked for and not found: %v", logs.lines)
	}
}

// A lost create that carried no key is never searched for: the search for an
// empty external id would answer somebody else's company with none.
func TestTheLiveCleanupNeverSearchesForALostCreateWithNoKey(t *testing.T) {
	h := newHarness(t)
	customersOn(h)
	h.admin.httpStatus("ShopifyCompanyCreate", 502)
	h.admin.httpStatus("ShopifyCustomerCreate", 502)
	// The store holds somebody else's company with no external id.
	h.admin.reply("ShopifyCompanyByExternalID", map[string]any{"companies": map[string]any{"nodes": []any{
		map[string]any{"id": "gid://shopify/Company/7", "externalId": "", "locations": map[string]any{"nodes": []any{map[string]any{"id": "gid://shopify/CompanyLocation/7"}}}},
	}}})
	deletes := 0
	h.admin.replyWith("LiveTestCompanyDelete", func(map[string]any) any {
		deletes++
		return map[string]any{"companyDelete": map[string]any{"deletedCompanyId": "gid://shopify/Company/7", "userErrors": []any{}}}
	})
	logs := &liveLogs{}
	t.Run("the test", func(t *testing.T) {
		w := liveWholesaleOnFake(t, h, logs)
		_, _, _ = w.h.conn.createCompany(w.h.ctx, w.store, WholesaleAccountInput{ApplicationID: "app-nokey", CompanyName: "No key"}, "")
		_, _ = w.h.conn.createCustomer(w.h.ctx, w.store, liveBuyerName, "")
	})
	if deletes != 0 || h.admin.countOp("ShopifyCompanyByExternalID") != 0 {
		t.Fatalf("the cleanup searched for an empty external id (%d searches) and deleted %d companies", h.admin.countOp("ShopifyCompanyByExternalID"), deletes)
	}
	for _, want := range []string{
		"LEFT IN THE STORE, perhaps: a company create carried no external id, so it could not be looked up",
		"LEFT IN THE STORE, perhaps: a customer create carried no address, so it could not be looked up",
	} {
		if !logs.said(want) {
			t.Errorf("the cleanup did not say %q: %v", want, logs.lines)
		}
	}
}
