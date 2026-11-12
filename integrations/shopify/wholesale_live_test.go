package shopify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/shopify/shopifylive"
)

// wholesale_live_test.go -- the wholesale tag route against a real
// development store.
//
// wholesale_capabilities_test.go and wholesale_gate_test.go hold the route to
// the package's fake Admin API. These hold it to Shopify itself, at the pinned
// Admin API version: that a buyer with no customer gets exactly one, that a
// second grant sends nothing, that a revoke by the recorded id takes the tag
// off and a second revoke sends nothing, that a store whose grant does not
// reach customers answers scope_missing, that Shopify's own "taken" answer is
// the one the route recognises, and that a company stamped with an external id
// holding colons is found by the connector's own lookup.
//
// REQUIREMENT: a Shopify partner DEVELOPMENT store reserved for the live tests,
// the same store the mirror's live tests use. Every test asks the store for its
// plan before anything else (shopifylive.RequireDevelopmentStore, through the
// harness's liveRequireDevelopmentStore) and fails, having created nothing,
// unless the store says it is a development store.
//
// WHAT A RUN TOUCHES, and only what it made itself: each test invents its own
// buyer address, wholesale-live-<random>@example.com, which no other customer
// can hold, records the customer id Shopify answered for it, and deletes
// exactly that id in a t.Cleanup that runs when the test fails too, logging
// what it deleted. A customer the route created whose id the test never heard
// (a grant that failed after its create) is found again by the run's own
// address, which only this run can have used, and deleted the same way. A
// cleanup that cannot delete says which id is left, in a line that begins
// "LEFT IN THE STORE". The company test creates two companies of its own,
// with a location each, and deletes them the same way.
//
// The variables (shopifylive's pair, and this file's own):
//
//	MEMQL_TEST_SHOPIFY_STORE_DOMAIN              <shop>.myshopify.com of the development store
//	MEMQL_TEST_SHOPIFY_ADMIN_TOKEN               an Admin token holding write_customers
//	                                             (and write_companies for the company test)
//	MEMQL_TEST_SHOPIFY_ADMIN_TOKEN_NO_CUSTOMERS  a token of the same store WITHOUT write_customers
//	                                             (the scope test only)
//
// Without the first pair every test skips, naming the variable; the scope test
// also skips without the third. The token is never printed, nor any header;
// the store domain may be.
//
//	MEMQL_DATABASE_DSN=... MEMQL_TEST_SHOPIFY_STORE_DOMAIN=... MEMQL_TEST_SHOPIFY_ADMIN_TOKEN=... \
//	MEMQL_TEST_SHOPIFY_ADMIN_TOKEN_NO_CUSTOMERS=... \
//	  go test -count=1 -timeout 30m ./integrations/shopify/ ./packs/wholesalepack/ -run 'TestLive' -v
//
// NOT VALIDATED against the recorded schema (cmd/shopifyschema/testdata),
// which holds no Mutation type: customerDelete (the mirror's document) and
// companyDelete, which these tests send to clean up. The connector's own
// documents are the route's; a refusal of one of them is what these tests
// exist to find.

// envLiveTokenNoCustomers is this file's own variable: a token of the same
// development store that does not hold write_customers.
const envLiveTokenNoCustomers = "MEMQL_TEST_SHOPIFY_ADMIN_TOKEN_NO_CUSTOMERS"

// liveWholesaleTest names these tests in a skip line.
const liveWholesaleTest = "the live wholesale tag test"

// liveBuyerName is the name every invented buyer is created with.
const liveBuyerName = "Sam Rivers"

// --- counting the Admin calls ---------------------------------------------

// liveAdminCounter is a transport that records the operation name of every
// Admin call the connector sends, and nothing else: no header, no variable.
type liveAdminCounter struct {
	next http.RoundTripper
	mu   stdsync.Mutex
	ops  []string
}

// countAdminCalls puts a counter in front of the connector's Admin transport.
func countAdminCalls(conn *Connector) *liveAdminCounter {
	next := conn.admin.http.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c := &liveAdminCounter{next: next}
	conn.admin.http.Transport = c
	return c
}

func (c *liveAdminCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	op := "(unreadable)"
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		var doc struct {
			OperationName string `json:"operationName"`
		}
		if json.Unmarshal(body, &doc) == nil && doc.OperationName != "" {
			op = doc.OperationName
		}
		req.Body = io.NopCloser(strings.NewReader(string(body)))
	}
	c.mu.Lock()
	c.ops = append(c.ops, op)
	c.mu.Unlock()
	return c.next.RoundTrip(req)
}

// mark is where the calls stand now.
func (c *liveAdminCounter) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ops)
}

// since is every operation sent after mark, in order.
func (c *liveAdminCounter) since(mark int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.ops[mark:]...)
}

// countSince is how many times op was sent after mark.
func (c *liveAdminCounter) countSince(mark int, op string) int {
	n := 0
	for _, o := range c.since(mark) {
		if o == op {
			n++
		}
	}
	return n
}

// --- the harness ------------------------------------------------------------

// liveWholesale is the mirror's live harness with the wholesale integration
// over it and a counter on its Admin transport.
type liveWholesale struct {
	h     *liveHarness
	calls *liveAdminCounter
	integ *Integration
	// store is the first store, checked a development store; the route's
	// writes and every cleanup go to it.
	store Store
	// customerScopesChecked is set once the token is known to reach customers.
	customerScopesChecked bool
}

func newLiveWholesale(t *testing.T, stores ...liveCredentials) *liveWholesale {
	t.Helper()
	return newLiveWholesaleOn(t, newLiveHarness(t, stores...))
}

// newLiveWholesaleOn builds the harness over h. The first store is checked a
// development store here, before anything else is sent, and that check is the
// counter's own proof that it counts.
func newLiveWholesaleOn(t *testing.T, h *liveHarness) *liveWholesale {
	t.Helper()
	w := &liveWholesale{h: h, calls: countAdminCalls(h.conn), integ: NewIntegration(h.conn)}
	w.store = h.store(t, 0)
	if w.calls.countSince(0, shopifylive.ShopPlanOperation) == 0 {
		t.Fatal("the Admin call counter did not see the development-store check, so it would count nothing")
	}
	return w
}

// packCall is the wholesale pack's own call as engineCaller makes it: internal
// origin and the pack's mark, bounded by the test's deadline.
func (w *liveWholesale) packCall() context.Context {
	return auth.ContextWithPackCaller(auth.ContextWithInternalOrigin(
		auth.ContextWithUserActor(w.h.ctx, "live-test")), wholesalePackCaller)
}

// requireCustomerScopes fails unless the token reaches customers, which every
// test that writes one needs: the grant, not the route.
func (w *liveWholesale) requireCustomerScopes(t *testing.T) {
	t.Helper()
	if w.customerScopesChecked {
		return
	}
	if !grantIncludes(w.h.grantedScopes(t, w.store))["write_customers"] {
		t.Fatalf("the token named by %s lacks write_customers, which the tag route needs; the token's grant, not a failure of the route, and nothing was created", envLiveToken)
	}
	w.customerScopesChecked = true
}

// liveRandom is a short random suffix, unique to one run.
func liveRandom(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("no randomness for the run's own address: %v", err)
	}
	return hex.EncodeToString(b)
}

// liveBuyer is one test's invented buyer: its address, and every customer id
// Shopify answered for it.
type liveBuyer struct {
	email string
	mu    stdsync.Mutex
	ids   []string
}

func (b *liveBuyer) record(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, have := range b.ids {
		if have == id {
			return
		}
	}
	b.ids = append(b.ids, id)
}

func (b *liveBuyer) recorded() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string{}, b.ids...)
}

// newBuyer invents the test's buyer, registers its removal, and holds the
// precondition that nobody on the store has the address yet.
func (w *liveWholesale) newBuyer(t *testing.T) *liveBuyer {
	t.Helper()
	w.requireCustomerScopes(t)
	b := &liveBuyer{email: "wholesale-live-" + liveRandom(t) + "@example.com"}
	t.Cleanup(func() { w.removeBuyer(t, b) })
	if holders := w.holders(t, b); len(holders) != 0 {
		for _, c := range holders {
			// Not this run's: never recorded, never deleted.
			t.Logf("a customer %s already holds the invented address; the test leaves it alone", c.id)
		}
		t.Fatalf("the invented address %s is already held on the store, so the test cannot say what it created; nothing was created", b.email)
	}
	return b
}

// holders is every customer the route's own read finds for the buyer's address.
func (w *liveWholesale) holders(t *testing.T, b *liveBuyer) []wholesaleCustomer {
	t.Helper()
	ctx, cancel := context.WithTimeout(w.h.ctx, liveCallTimeout)
	defer cancel()
	all, err := w.h.conn.customersByEmail(ctx, w.store, b.email)
	if err != nil {
		t.Fatalf("read the customers holding %s: %s", b.email, liveRefusal(err))
	}
	return all
}

// awaitOneHolder waits until the store's customer search answers exactly the
// one customer id for the address: the search settles a few seconds after a
// create.
func (w *liveWholesale) awaitOneHolder(t *testing.T, b *liveBuyer, id string) {
	t.Helper()
	liveAwait(t, w.h.ctx, "the customer search to answer exactly customer "+id+" for "+b.email, func() (string, bool) {
		ctx, cancel := context.WithTimeout(w.h.ctx, liveCallTimeout)
		defer cancel()
		all, err := w.h.conn.customersByEmail(ctx, w.store, b.email)
		if err != nil {
			return liveRefusal(err), false
		}
		ids := make([]string, 0, len(all))
		for _, c := range all {
			ids = append(ids, c.id)
			// A customer the address holds can only be this run's.
			b.record(c.id)
		}
		return fmt.Sprint(ids), len(ids) == 1 && ids[0] == id
	})
}

const liveCustomerDoc = `query LiveTestCustomer($id: ID!) {
  customer(id: $id) {
    id
    tags
    defaultEmailAddress { emailAddress }
  }
}`

// liveCustomer is one customer as the store answers it by id.
type liveCustomer struct {
	found bool
	email string
	tags  []string
}

func (w *liveWholesale) customer(t *testing.T, id string) liveCustomer {
	t.Helper()
	data := w.h.query(t, w.store, liveCustomerDoc, "LiveTestCustomer", map[string]any{"id": id})
	c, _ := data["customer"].(map[string]any)
	if c == nil {
		return liveCustomer{}
	}
	out := liveCustomer{found: true, email: liveString(c, "defaultEmailAddress", "emailAddress")}
	list, _ := c["tags"].([]any)
	for _, tag := range list {
		out.tags = append(out.tags, fmt.Sprint(tag))
	}
	return out
}

// tagCount is how many of tags are tag, compared as Shopify compares tags.
func tagCount(tags []string, tag string) int {
	n := 0
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), tag) {
			n++
		}
	}
	return n
}

// requireTagged fails unless the customer exists, holds the buyer's address and
// carries the tag want times.
func (w *liveWholesale) requireTagged(t *testing.T, b *liveBuyer, id string, want int) {
	t.Helper()
	c := w.customer(t, id)
	if !c.found {
		t.Fatalf("customer %s is not on the store", id)
	}
	if !strings.EqualFold(c.email, b.email) {
		t.Fatalf("customer %s holds %q, want the invented address %s", id, c.email, b.email)
	}
	if got := tagCount(c.tags, WholesaleTagDefault); got != want {
		t.Fatalf("customer %s carries %s %d times (tags %v), want %d", id, WholesaleTagDefault, got, c.tags, want)
	}
}

// grant runs the tag route as the pack calls it, on storeID, and records the
// customer it answers.
func (w *liveWholesale) grant(t *testing.T, storeID string, b *liveBuyer) map[string]any {
	t.Helper()
	out := invoke(t, w.packCall(), w.integ.handleTagWholesaleCustomer, map[string]any{
		"storeId": storeID, "buyerEmail": b.email, "buyerName": liveBuyerName, "tag": WholesaleTagDefault,
	})
	if id, _ := out["customerId"].(string); id != "" {
		b.record(id)
	}
	return out
}

// grantOK is grant on the first store, required to answer ok with a customer.
func (w *liveWholesale) grantOK(t *testing.T, b *liveBuyer) (out map[string]any, id string) {
	t.Helper()
	out = w.grant(t, w.store.ID, b)
	id, _ = out["customerId"].(string)
	if out["status"] != "ok" || id == "" {
		t.Fatalf("the grant answered %v, want ok naming the buyer's customer", out)
	}
	return out, id
}

// revokeOK runs the revoke as the pack's adapter does: the customer the grant
// recorded, by id, with the address beside it.
func (w *liveWholesale) revokeOK(t *testing.T, b *liveBuyer, id string) {
	t.Helper()
	out := invoke(t, w.packCall(), w.integ.handleUntagWholesaleCustomer, map[string]any{
		"storeId": w.store.ID, "customerId": id, "buyerEmail": b.email, "tag": WholesaleTagDefault,
	})
	if out["status"] != "ok" {
		t.Fatalf("the revoke answered %v, want ok", out)
	}
}

// removeBuyer deletes every customer the run made for its address, on a fresh
// context so a test that ran out of time still cleans up.
func (w *liveWholesale) removeBuyer(t *testing.T, b *liveBuyer) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ids := b.recorded()
	if len(ids) == 0 {
		// The route may have created a customer and failed before answering
		// its id. The address is the run's own, so whatever holds it is too.
		all, err := w.h.conn.customersByEmail(ctx, w.store, b.email)
		if err != nil {
			t.Logf("LEFT IN THE STORE, perhaps: a customer holding %s could not be looked up, so none was deleted; if one exists it is this run's, delete it in the Shopify admin: %s",
				b.email, liveRefusal(err))
			return
		}
		for _, c := range all {
			ids = append(ids, c.id)
		}
	}
	for _, id := range ids {
		if _, err := w.h.tryMutate(ctx, w.store, liveCustomerDeleteDoc, "LiveTestCustomerDelete", "customerDelete",
			map[string]any{"input": map[string]any{"id": id}}); err != nil {
			t.Logf("LEFT IN THE STORE: customer %s (the invented buyer %s); delete it in the Shopify admin: %v", id, b.email, err)
			continue
		}
		t.Logf("deleted customer %s, the invented buyer %s", id, b.email)
	}
}

// --- the tests ----------------------------------------------------------------

// TestLiveAGrantWithoutWriteCustomersAnswersScopeMissing grants through a token
// of the same store that does not hold write_customers, and holds the reply to
// a refusal coded scope_missing that tagged and created nothing.
func TestLiveAGrantWithoutWriteCustomersAnswersScopeMissing(t *testing.T) {
	full := liveEnvFor(t, liveWholesaleTest, envLiveDomain, envLiveToken)
	limited := liveEnvFor(t, liveWholesaleTest, envLiveDomain, envLiveTokenNoCustomers)
	w := newLiveWholesale(t, full, limited)
	limitedStore := w.h.store(t, 1)
	// THE PRECONDITION, before anything is sent with it: the second token
	// really lacks write_customers. If it held it, the grant would create a
	// customer, and the test would say nothing about the refusal.
	if grantIncludes(w.h.grantedScopes(t, limitedStore))["write_customers"] {
		t.Fatalf("the token named by %s holds write_customers, so it cannot show the refusal; nothing was created", envLiveTokenNoCustomers)
	}
	b := w.newBuyer(t)

	mark := w.calls.mark()
	out := w.grant(t, limitedStore.ID, b)
	if out["status"] != "refused" || out["code"] != RefusalScopeMissing {
		t.Fatalf("the grant through a token without write_customers answered %v, want refused with code %s", out, RefusalScopeMissing)
	}
	if n := w.calls.countSince(mark, "ShopifyTagsAdd"); n != 0 {
		t.Fatalf("a refused grant sent %d tagsAdd", n)
	}
	if holders := w.holders(t, b); len(holders) != 0 {
		t.Fatalf("a refused grant left %d customers holding the invented address", len(holders))
	}
}

// TestLiveAMissingCustomerIsCreatedOnceAndTagged grants a buyer the store has
// no customer for: the reply says customerCreated, one customer is created and
// tagged once, and it is the only one holding the address.
func TestLiveAMissingCustomerIsCreatedOnceAndTagged(t *testing.T) {
	w := newLiveWholesale(t, liveEnvFor(t, liveWholesaleTest, envLiveDomain, envLiveToken))
	b := w.newBuyer(t)

	mark := w.calls.mark()
	out, id := w.grantOK(t, b)
	if out["customerCreated"] != true {
		t.Fatalf("the grant answered %v, want customerCreated true for a buyer the store had no customer for", out)
	}
	if ref := out["reference"]; ref != "customer="+id+";tag="+WholesaleTagDefault {
		t.Fatalf("reference = %v, want customer=%s;tag=%s", ref, id, WholesaleTagDefault)
	}
	if n := w.calls.countSince(mark, "ShopifyCustomerCreate"); n != 1 {
		t.Fatalf("the grant sent customerCreate %d times, want once (%v)", n, w.calls.since(mark))
	}
	if n := w.calls.countSince(mark, "ShopifyTagsAdd"); n != 1 {
		t.Fatalf("the grant sent tagsAdd %d times, want once (%v)", n, w.calls.since(mark))
	}
	w.requireTagged(t, b, id, 1)
	w.awaitOneHolder(t, b, id)
}

// TestLiveASecondProvisionCreatesNothingAndAddsNoSecondTag grants the same
// buyer twice: the second grant finds the first customer, creates nothing and
// sends no tagsAdd, and the customer carries the tag once.
func TestLiveASecondProvisionCreatesNothingAndAddsNoSecondTag(t *testing.T) {
	w := newLiveWholesale(t, liveEnvFor(t, liveWholesaleTest, envLiveDomain, envLiveToken))
	b := w.newBuyer(t)
	_, id := w.grantOK(t, b)
	// The second grant reads the address first, so it waits for the search
	// to answer the customer the first one made.
	w.awaitOneHolder(t, b, id)

	mark := w.calls.mark()
	out, again := w.grantOK(t, b)
	if out["customerCreated"] != false || again != id {
		t.Fatalf("the second grant answered %v, want customerCreated false on customer %s", out, id)
	}
	if n := w.calls.countSince(mark, "ShopifyCustomerCreate"); n != 0 {
		t.Fatalf("the second grant sent customerCreate %d times (%v)", n, w.calls.since(mark))
	}
	if n := w.calls.countSince(mark, "ShopifyTagsAdd"); n != 0 {
		t.Fatalf("the second grant sent tagsAdd %d times for a tag the customer carries (%v)", n, w.calls.since(mark))
	}
	w.requireTagged(t, b, id, 1)
	w.awaitOneHolder(t, b, id)
}

// TestLiveRevokeRemovesTheTagAndASecondRevokeSucceeds revokes by the customer
// id the grant answered: the tag comes off, and a second revoke reads the
// customer by id, sends no tagsRemove and succeeds.
func TestLiveRevokeRemovesTheTagAndASecondRevokeSucceeds(t *testing.T) {
	w := newLiveWholesale(t, liveEnvFor(t, liveWholesaleTest, envLiveDomain, envLiveToken))
	b := w.newBuyer(t)
	_, id := w.grantOK(t, b)
	w.requireTagged(t, b, id, 1)

	mark := w.calls.mark()
	w.revokeOK(t, b, id)
	if n := w.calls.countSince(mark, "ShopifyTagsRemove"); n != 1 {
		t.Fatalf("the revoke sent tagsRemove %d times, want once (%v)", n, w.calls.since(mark))
	}
	if n := w.calls.countSince(mark, "ShopifyCustomerByEmail"); n != 0 {
		t.Fatalf("the revoke read the address %d times; a revoke naming the customer reads it by id (%v)", n, w.calls.since(mark))
	}
	w.requireTagged(t, b, id, 0)

	mark = w.calls.mark()
	w.revokeOK(t, b, id)
	if n := w.calls.countSince(mark, "ShopifyTagsRemove"); n != 0 {
		t.Fatalf("the second revoke sent tagsRemove %d times for a tag the customer no longer carries (%v)", n, w.calls.since(mark))
	}
	if n := w.calls.countSince(mark, "ShopifyCustomerById"); n != 1 {
		t.Fatalf("the second revoke read the customer by id %d times, want once (%v)", n, w.calls.since(mark))
	}
	w.requireTagged(t, b, id, 0)
}

// TestLiveProvisionAgainReTags grants, revokes and grants again: the third act
// finds the same customer, creates nothing and puts the tag back.
func TestLiveProvisionAgainReTags(t *testing.T) {
	w := newLiveWholesale(t, liveEnvFor(t, liveWholesaleTest, envLiveDomain, envLiveToken))
	b := w.newBuyer(t)
	_, id := w.grantOK(t, b)
	w.revokeOK(t, b, id)
	w.requireTagged(t, b, id, 0)
	w.awaitOneHolder(t, b, id)

	mark := w.calls.mark()
	out, again := w.grantOK(t, b)
	if out["customerCreated"] != false || again != id {
		t.Fatalf("the grant after a revoke answered %v, want customerCreated false on customer %s", out, id)
	}
	if n := w.calls.countSince(mark, "ShopifyCustomerCreate"); n != 0 {
		t.Fatalf("the grant after a revoke sent customerCreate %d times (%v)", n, w.calls.since(mark))
	}
	if n := w.calls.countSince(mark, "ShopifyTagsAdd"); n != 1 {
		t.Fatalf("the grant after a revoke sent tagsAdd %d times, want once (%v)", n, w.calls.since(mark))
	}
	w.requireTagged(t, b, id, 1)
}

// TestLiveASignedInNonOwnerCannotTagACustomerDirectly calls the builtin
// through a real engine over the database with the integration registered, as
// four callers who are not the pack: each is refused, and Shopify's Admin API
// is asked nothing. Nothing is written; the store is real so that a refusal
// that leaked would reach it.
func TestLiveASignedInNonOwnerCannotTagACustomerDirectly(t *testing.T) {
	creds := liveEnvFor(t, liveWholesaleTest, envLiveDomain, envLiveToken)
	// A builtin call needs the engine's database, as every call through the
	// wire does.
	eng, _ := authorityEngine(t)
	if eng == nil {
		return
	}
	w := newLiveWholesale(t, creds)
	if err := eng.RegisterIntegration(w.integ); err != nil {
		t.Fatalf("register the Shopify integration: %v", err)
	}
	// An address the test never means to create. A call that got through
	// would create it, which the zero count below catches; the cleanup then
	// deletes whatever holds the address, which can only be this run's.
	b := &liveBuyer{email: "wholesale-live-" + liveRandom(t) + "@example.com"}
	t.Cleanup(func() { w.removeBuyer(t, b) })
	call := "builtin " + renderCall("shopifyTagWholesaleCustomer", map[string]any{
		"storeId": w.store.ID, "buyerEmail": b.email, "buyerName": liveBuyerName,
	})
	want := "shopify: shopifyTagWholesaleCustomer is called by the wholesale pack, never by a client"
	for _, caller := range []struct {
		name string
		ctx  context.Context
	}{
		{"a signed-in non-owner", clientCall("live-test-other", auth.RoleWriter)},
		{"the store's owner", clientCall("live-test", auth.RoleWriter)},
		{"a cluster owner", clientCall("live-test-operator", auth.RoleOwner)},
		{"an automation's step (internal origin, no mark)", automationStep()},
	} {
		mark := w.calls.mark()
		ctx, cancel := context.WithTimeout(caller.ctx, liveCallTimeout)
		_, err := eng.Execute(ctx, call)
		cancel()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want the refusal %q", caller.name, err, want)
		}
		if ops := w.calls.since(mark); len(ops) != 0 {
			t.Errorf("%s was refused and Shopify's Admin API was still asked: %v", caller.name, ops)
		}
	}
}

// TestLiveAnEmailTakenAnswerIsRecognised drives the create path a second time
// for an address the store already holds, as two grants at once would: the
// route recognises Shopify's own "taken" answer, the second grant settles on
// the first customer, and the store ends with one customer, tagged once.
func TestLiveAnEmailTakenAnswerIsRecognised(t *testing.T) {
	w := newLiveWholesale(t, liveEnvFor(t, liveWholesaleTest, envLiveDomain, envLiveToken))
	b := w.newBuyer(t)
	ctx := w.h.ctx

	// The first grant's create.
	id, err := w.h.conn.createCustomer(ctx, w.store, liveBuyerName, b.email)
	b.record(id)
	if err != nil {
		t.Fatalf("the first create: %s", liveRefusal(err))
	}

	// The origin's own answer to the second create, raw, for the person
	// running this to see its real shape.
	data, err := w.h.liveCall(ctx, w.store, customerCreateMutation, "ShopifyCustomerCreate",
		map[string]any{"input": map[string]any{"email": b.email}})
	if err != nil {
		t.Fatalf("the second create: %v", err)
	}
	payload, _ := data["customerCreate"].(map[string]any)
	if twin := liveString(payload, "customer", "id"); twin != "" {
		b.record(twin)
		t.Fatalf("Shopify created a second customer %s for an address customer %s holds; the route's recovery assumes it refuses", twin, id)
	}
	t.Logf("the origin's answer to a second customerCreate for the address: userErrors %v", payload["userErrors"])

	// The route's classification of that answer.
	twin, err := w.h.conn.createCustomer(ctx, w.store, liveBuyerName, b.email)
	b.record(twin)
	if !errors.Is(err, errEmailTaken) {
		t.Fatalf("the route did not recognise the origin's answer as the address being taken: id %q, err %v", twin, err)
	}

	// The second grant's whole path, whose read may run before the search
	// has the first customer: it finds it, or meets the taken answer and reads
	// again. "Taken, and the read does not find it yet" is an error the pack
	// retries, so it is retried here; a second customer is a failure.
	path := ""
	liveAwait(t, ctx, "the second grant to settle on customer "+id, func() (string, bool) {
		mark := w.calls.mark()
		got, created, err := w.h.conn.findOrCreate(ctx, w.store, b.email, liveBuyerName)
		b.record(got.id)
		if created || (got.id != "" && got.id != id) {
			t.Fatalf("the second grant settled on customer %s (created %t), want the first customer %s", got.id, created, id)
		}
		if err != nil {
			return "an error the pack retries: " + err.Error(), false
		}
		path = "its read found the first customer"
		if w.calls.countSince(mark, "ShopifyCustomerCreate") > 0 {
			path = "its create met the taken answer and its second read found the first customer"
		}
		return "", got.id == id
	})
	t.Logf("the second grant's path: %s", path)

	out, again := w.grantOK(t, b)
	if out["customerCreated"] != false || again != id {
		t.Fatalf("the grant answered %v, want customerCreated false on customer %s", out, id)
	}
	w.requireTagged(t, b, id, 1)
	w.awaitOneHolder(t, b, id)
}

const liveCompaniesProbeDoc = `query LiveTestCompaniesProbe {
  companies(first: 1) { nodes { id } }
}`

const liveCompanyDeleteDoc = `mutation LiveTestCompanyDelete($id: ID!) {
  companyDelete(id: $id) {
    deletedCompanyId
    userErrors { field message }
  }
}`

// createLiveCompany creates one company of the test's own, with its location,
// stamped with externalID, and registers its removal.
func (w *liveWholesale) createLiveCompany(t *testing.T, applicationID, externalID, suffix string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(w.h.ctx, liveCallTimeout)
	defer cancel()
	company, _, err := w.h.conn.createCompany(ctx, w.store, WholesaleAccountInput{
		ApplicationID: applicationID,
		CompanyName:   "Wholesale live test " + suffix,
	}, externalID)
	if company != "" {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if _, err := w.h.tryMutate(ctx, w.store, liveCompanyDeleteDoc, "LiveTestCompanyDelete", "companyDelete",
				map[string]any{"id": company}); err != nil {
				t.Logf("LEFT IN THE STORE: company %s (external id %s) and its location; delete it in the Shopify admin: %v", company, externalID, err)
				return
			}
			t.Logf("deleted company %s (external id %s) and its location", company, externalID)
		})
	}
	if err != nil {
		t.Fatalf("create the company stamped %s: %s", externalID, liveRefusal(err))
	}
	return company
}

// TestLiveACompanyIsFoundByAnExternalIdThatHoldsColons stamps one company with
// the pack's form of a canonical application id (colons inside the search
// value) and one with the bare form an older build wrote, and finds each
// through the connector's own lookup.
func TestLiveACompanyIsFoundByAnExternalIdThatHoldsColons(t *testing.T) {
	w := newLiveWholesale(t, liveEnvFor(t, liveWholesaleTest, envLiveDomain, envLiveToken))
	if !grantIncludes(w.h.grantedScopes(t, w.store))["write_companies"] {
		t.Skipf("the token named by %s lacks write_companies, so this run cannot create a company; grant it to run the company test", envLiveToken)
	}
	if _, err := w.h.liveCall(w.h.ctx, w.store, liveCompaniesProbeDoc, "LiveTestCompaniesProbe", nil); err != nil {
		var ae *AdminError
		if errors.As(err, &ae) && ae.StatusCode < 500 && len(ae.Errors) > 0 {
			t.Skipf("the development store's plan does not offer B2B companies (Shopify answered the companies read with: %s); nothing was created", liveRefusal(err))
		}
		t.Fatalf("read the store's companies: %s", liveRefusal(err))
	}

	suffix := liveRandom(t)
	canonical := "v1:wholesale:application:wholesale-live-" + suffix + "-a"
	olderCanonical := "v1:wholesale:application:wholesale-live-" + suffix + "-b"
	stamped := w.createLiveCompany(t, canonical, wholesaleExternalIDPrefix+canonical, suffix+"-a")
	older := w.createLiveCompany(t, olderCanonical, wholesaleExternalIDPrefix+memql.BareShortId(olderCanonical), suffix+"-b")

	lookup := func(applicationID, want string) {
		t.Helper()
		liveAwait(t, w.h.ctx, "the company lookup under "+applicationID+" to answer "+want, func() (string, bool) {
			ctx, cancel := context.WithTimeout(w.h.ctx, liveCallTimeout)
			defer cancel()
			company, location, err := w.h.conn.companyForApplication(ctx, w.store, applicationID)
			if err != nil {
				return liveRefusal(err), false
			}
			return fmt.Sprintf("company %q, location %q", company, location), company == want && location != ""
		})
	}
	// The canonical id, stamped as this build stamps it: the search value
	// holds four colons.
	lookup(canonical, stamped)
	// The canonical id of an application an older build stamped bare: found
	// under the bare form, the lookup's second.
	lookup(olderCanonical, older)

	// EXACT MATCH: the bare form of the first application names no company,
	// although Shopify's search may answer the canonical one for it.
	ctx, cancel := context.WithTimeout(w.h.ctx, liveCallTimeout)
	defer cancel()
	company, _, err := w.h.conn.companyForApplication(ctx, w.store, memql.BareShortId(canonical))
	if err != nil {
		t.Fatalf("the lookup under the bare form: %s", liveRefusal(err))
	}
	if company != "" {
		t.Fatalf("the lookup under the bare form %s answered company %s, which is stamped with the canonical form", memql.BareShortId(canonical), company)
	}
}
