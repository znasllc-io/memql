package shopify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// mirror_live_test.go -- the mirror against a real development store.
//
// REQUIREMENT: the store must be a Shopify partner DEVELOPMENT store reserved
// for this test. The tests create, edit, refund and cancel paid orders on a
// real variant, and the updated_at sweep reads EVERY order changed since the
// test's own was made, so pointed at a merchant's store this file would
// consume order numbers, move inventory, notify staff and read the
// merchant's orders. Every test therefore asks shop.plan.partnerDevelopment
// before its first mutation and fails, having created nothing, unless the
// store says it is a development store (liveRequireDevelopmentStore).
//
// Every other test in this package talks to the fake Admin in
// harness_test.go, which checks the connector's half of each conversation.
// These check the other half: that Shopify, at the pinned Admin API version,
// answers the generated documents the way the mapper expects, for an order
// the test makes and changes itself.
//
// THE VARIABLES ARE TEST INPUTS, NOT A PRODUCT CONFIGURATION PATH, read here
// the way integrations/openai/asr_live_test.go reads its own: nothing outside
// this file reads these names and no manifest registers them. Without the
// first pair every test skips, naming the variable; the two-store test also
// skips without the second pair. No CI lane sets them.
//
//	MEMQL_TEST_SHOPIFY_STORE_DOMAIN    <shop>.myshopify.com of a development store
//	MEMQL_TEST_SHOPIFY_ADMIN_TOKEN     an Admin API token of an app installed on it
//	MEMQL_TEST_SHOPIFY_STORE_DOMAIN_2  a SECOND development store (two-store test)
//	MEMQL_TEST_SHOPIFY_ADMIN_TOKEN_2   its token
//
//	MEMQL_DATABASE_DSN=... MEMQL_TEST_SHOPIFY_STORE_DOMAIN=... MEMQL_TEST_SHOPIFY_ADMIN_TOKEN=... \
//	  go test -count=1 -timeout 30m ./integrations/shopify/ -run 'TestLive' -v
//
// -timeout 30m is part of the command: five tests of up to four minutes each
// exceed go test's ten-minute default, and a timeout panic skips t.Cleanup,
// which is what removes the orders.
//
// What a run does in the store, and only to what it made itself: a draft
// order completed as paid, tagged memql-live-test with the run's time in its
// note and an invented buyer at example.com; for the change test an edit, a
// refund of one unit and a cancellation. Cleanup cancels what is still open
// (restocking), deletes the order, the completed draft and the invented
// customer, and logs whatever Shopify would not delete, so a leftover is
// recognisable by its tag. The refund is NO_RESTOCK, so a run of the change
// test is expected to leave the variant one unit lower; the other tests
// should leave it where it was.
//
// The setup calls go through the connector's own Admin client (adminCall), so
// they are paced against the same cost bucket as the mirror's reads.
//
// The token must hold the commerce set and its app must have made the
// protected customer data declaration; for its own setup it also needs
// write_draft_orders and write_orders. A refusal for any of these is reported
// as that, from the Admin API's own ACCESS_DENIED, and never as a failure of
// the mirror.
//
// NOT VALIDATED against the recorded schema (cmd/shopifyschema/testdata),
// which holds no Mutation type and no input fields: every mutation here --
// draftOrderCreate, draftOrderComplete, orderEditBegin, orderEditSetQuantity,
// orderEditCommit, refundCreate, orderCancel, orderDelete, draftOrderDelete,
// customerDelete -- and their inputs. Two are uncertain at the pinned version:
//   - orderCancel's `refund: Boolean`: later versions prefer
//     `refundMethod: { originalPaymentMethodsRefund: false }`; if the first
//     live run refuses `refund`, send that instead.
//   - the id the order-edit mutations take after orderEditBegin: the
//     calculated order's id here; newer versions answer an orderEditSession
//     too, and if Shopify refuses the calculated order's id, select
//     `orderEditSession { id }` in liveOrderEditBeginDoc and pass that.
// The queries were validated field by field.

const (
	envLiveDomain  = "MEMQL_TEST_SHOPIFY_STORE_DOMAIN"
	envLiveToken   = "MEMQL_TEST_SHOPIFY_ADMIN_TOKEN"
	envLiveDomain2 = "MEMQL_TEST_SHOPIFY_STORE_DOMAIN_2"
	envLiveToken2  = "MEMQL_TEST_SHOPIFY_ADMIN_TOKEN_2"

	// liveTag marks everything a run creates.
	liveTag = "memql-live-test"

	// liveTestTimeout bounds a whole test; liveCallTimeout bounds one Admin
	// call, throttle retries included, so an unreachable host fails the test
	// rather than hanging it.
	liveTestTimeout = 4 * time.Minute
	liveCallTimeout = 30 * time.Second

	// A read that follows a write is retried this often, this far apart:
	// Shopify's search-backed listing and some order fields settle a few
	// seconds after the mutation returns.
	livePollTries = 6
	livePollWait  = 2500 * time.Millisecond
)

// liveSetupScopes are the test's own, not the connection's.
var liveSetupScopes = []string{"write_draft_orders", "write_orders"}

type liveCredentials struct {
	domain, token string
	// domainVar names the variable the domain came from, for messages.
	domainVar string
}

// liveEnv reads one store's pair, skipping when either half is unset. The
// domain is checked here with the connector's own guard, so a malformed value
// fails before any request. The token is never printed; the domain is not
// printed by this file, but a transport error from the client names the
// endpoint, shop host included.
func liveEnv(t *testing.T, domainVar, tokenVar string) liveCredentials {
	t.Helper()
	domain := strings.TrimSpace(os.Getenv(domainVar))
	if domain == "" {
		t.Skipf("%s not set; skipping the live mirror test against a development store", domainVar)
	}
	token := strings.TrimSpace(os.Getenv(tokenVar))
	if token == "" {
		t.Skipf("%s not set; skipping the live mirror test against a development store", tokenVar)
	}
	if _, err := NormalizeShopDomain(domain); err != nil {
		t.Fatalf("%s is not a bare <shop>.myshopify.com host; no request was sent", domainVar)
	}
	return liveCredentials{domain: domain, token: token, domainVar: domainVar}
}

// liveHarness is newHarness with the real Admin endpoint: the same registry,
// client and connector constructors, a recording engine, and stores whose
// tokens come from the environment. Nothing is overridden on the Admin
// client, so it resolves AdminEndpoint at generated.APIVersion and paces on
// the real bucket.
type liveHarness struct {
	engine *fakeEngine
	conn   *Connector
	ctx    context.Context
	ids    []string
	vars   map[string]string
	// developmentChecked holds the stores this test has confirmed are
	// development stores; each store is asked for itself.
	developmentChecked map[string]bool
}

func newLiveHarness(t *testing.T, stores ...liveCredentials) *liveHarness {
	t.Helper()
	engine := newFakeEngine()
	tokens := map[string]string{}
	rows := make([]map[string]any, 0, len(stores))
	ids := make([]string, 0, len(stores))
	vars := map[string]string{}
	for i, s := range stores {
		id := fmt.Sprintf("live%d", i+1)
		ref := storeSecretName(id, suffixAdminToken)
		tokens[ref] = s.token
		ids = append(ids, id)
		vars[id] = s.domainVar
		rows = append(rows, map[string]any{
			"id":                 id,
			"domain":             s.domain,
			"name":               "Live test store " + id,
			"adminTokenRef":      ref,
			"webhookSecretRef":   storeSecretName(id, suffixWebhookSecret),
			"apiVersion":         generated.APIVersion,
			"protectedDataLevel": ProtectedLevel1,
			"status":             StatusLive,
			"ownerUserId":        "live-test",
		})
	}
	engine.setRows("stores", rows)
	registry := NewStoreRegistry(engine, func(_ context.Context, name string) (string, error) {
		if tok, ok := tokens[name]; ok {
			return tok, nil
		}
		return "", fmt.Errorf("no such secret %q", name)
	})
	registry.ttl = 0
	conn := NewConnector(engine, slog.New(slog.DiscardHandler), registry, NewAdminClient())
	conn.deliver = func(s Store) string { return "https://api.example.test/inbound/shopify-" + s.ID }

	ctx, cancel := context.WithTimeout(context.Background(), liveTestTimeout)
	t.Cleanup(cancel)
	return &liveHarness{engine: engine, conn: conn, ctx: ctx, ids: ids, vars: vars, developmentChecked: map[string]bool{}}
}

func (h *liveHarness) store(t *testing.T, i int) Store {
	t.Helper()
	s, ok := h.conn.stores.ByID(h.ctx, h.ids[i])
	if !ok {
		t.Fatalf("the live store %s did not resolve", h.ids[i])
	}
	h.liveRequireDevelopmentStore(t, s)
	return s
}

// --- Admin calls --------------------------------------------------------

// liveCall runs one document through the connector's Admin client with a
// bounded wait, and returns the data or the refusal said plainly.
func (h *liveHarness) liveCall(ctx context.Context, store Store, document, op string, vars map[string]any) (map[string]any, error) {
	callCtx, cancel := context.WithTimeout(ctx, liveCallTimeout)
	defer cancel()
	resp, err := h.conn.adminCall(callCtx, store, document, op, vars)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", op, liveRefusal(err))
	}
	return resp.DataMap()
}

func (h *liveHarness) query(t *testing.T, store Store, document, op string, vars map[string]any) map[string]any {
	t.Helper()
	data, err := h.liveCall(h.ctx, store, document, op, vars)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// mutate runs a mutation and refuses on its userErrors.
func (h *liveHarness) mutate(t *testing.T, store Store, document, op, field string, vars map[string]any) map[string]any {
	t.Helper()
	data, err := h.tryMutate(h.ctx, store, document, op, field, vars)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (h *liveHarness) tryMutate(ctx context.Context, store Store, document, op, field string, vars map[string]any) (map[string]any, error) {
	data, err := h.liveCall(ctx, store, document, op, vars)
	if err != nil {
		return nil, err
	}
	payload, _ := data[field].(map[string]any)
	if payload == nil {
		return nil, fmt.Errorf("%s: the reply carried no %s", op, field)
	}
	if msgs := liveUserErrors(payload); len(msgs) > 0 {
		return nil, fmt.Errorf("%s refused: %s", op, strings.Join(msgs, "; "))
	}
	return payload, nil
}

func liveUserErrors(payload map[string]any) []string {
	var out []string
	for _, key := range []string{"userErrors", "orderCancelUserErrors"} {
		list, _ := payload[key].([]any)
		for _, e := range list {
			m, _ := e.(map[string]any)
			msg, _ := m["message"].(string)
			if f, ok := m["field"].([]any); ok && len(f) > 0 {
				msg = fmt.Sprint(f) + ": " + msg
			}
			out = append(out, msg)
		}
	}
	return out
}

// liveRequiredAccessRE reads the scope out of Shopify's requiredAccess
// extension ("`write_orders` access scope.").
var liveRequiredAccessRE = regexp.MustCompile("`([a-z_]+)`")

// liveRefusal says what an Admin failure is. A missing scope and a missing
// protected customer data declaration are the token's grant, and are named
// as that so nobody reads them as the mirror failing.
func liveRefusal(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("no answer within %s (an unreachable host or a hung connection): %v", liveCallTimeout, err)
	}
	var ae *AdminError
	if !errors.As(err, &ae) {
		return err.Error()
	}
	if ae.StatusCode == 401 || ae.StatusCode == 403 {
		return fmt.Sprintf("Shopify refused the token (HTTP %d): check the store's admin token variable and the app's installation on the store; not a failure of the mirror",
			ae.StatusCode)
	}
	var said []string
	for _, e := range ae.Errors {
		if e.Code() != "ACCESS_DENIED" {
			continue
		}
		where := fmt.Sprint(e.Path)
		if protectedDataRefusal(e.Message) {
			said = append(said, "Shopify refused "+where+" because the app has not made the protected customer data declaration")
			continue
		}
		scope := requiredScopeOf(e.Message)
		if ra, ok := e.Extensions["requiredAccess"].(string); ok {
			if protectedDataRefusal(ra) {
				said = append(said, "Shopify refused "+where+" because the app has not made the protected customer data declaration")
				continue
			}
			if m := liveRequiredAccessRE.FindStringSubmatch(ra); m != nil {
				scope = m[1]
			}
		}
		if scope != "" {
			said = append(said, "the token lacks the `"+scope+"` access scope (Shopify refused "+where+")")
			continue
		}
		said = append(said, "Shopify refused "+where+": "+e.Message)
	}
	if len(said) == 0 {
		return err.Error()
	}
	return strings.Join(said, "; ") + "; this is the token's grant, not a failure of the mirror"
}

// --- the documents the test sends ---------------------------------------

const liveAccessScopesDoc = `query LiveTestAccessScopes {
  currentAppInstallation {
    accessScopes { handle }
  }
}`

const liveShopPlanDoc = `query LiveTestShopPlan {
  shop {
    plan { partnerDevelopment }
  }
}`

const liveFirstVariantDoc = `query LiveTestFirstVariant {
  productVariants(first: 1) {
    nodes { id }
  }
}`

const liveDraftOrderCreateDoc = `mutation LiveTestDraftOrderCreate($input: DraftOrderInput!) {
  draftOrderCreate(input: $input) {
    draftOrder { id }
    userErrors { field message }
  }
}`

const liveDraftOrderCompleteDoc = `mutation LiveTestDraftOrderComplete($id: ID!) {
  draftOrderComplete(id: $id) {
    draftOrder { id order { id } }
    userErrors { field message }
  }
}`

// liveProbeDoc asks for the order's protected and scoped parts in one small
// document BEFORE the connector reads it, so a grant that does not reach
// them is reported from Shopify's own refusal. The connector would instead
// prune the refused fields and mirror the rest (denied.go), which is right
// for the product and would hide the reason here.
const liveProbeDoc = `query LiveTestOrderProbe($id: ID!) {
  order(id: $id) {
    id
    email
    customer { id }
    lineItems(first: 1) { nodes { id } }
    transactions { id }
  }
}`

const liveOrderEditBeginDoc = `mutation LiveTestOrderEditBegin($id: ID!) {
  orderEditBegin(id: $id) {
    calculatedOrder {
      id
      lineItems(first: 10) { nodes { id quantity } }
    }
    userErrors { field message }
  }
}`

const liveOrderEditSetQuantityDoc = `mutation LiveTestOrderEditSetQuantity($id: ID!, $lineItemId: ID!, $quantity: Int!) {
  orderEditSetQuantity(id: $id, lineItemId: $lineItemId, quantity: $quantity, restock: false) {
    calculatedOrder { id }
    userErrors { field message }
  }
}`

const liveOrderEditCommitDoc = `mutation LiveTestOrderEditCommit($id: ID!) {
  orderEditCommit(id: $id, notifyCustomer: false, staffNote: "memql-live-test") {
    order { id }
    userErrors { field message }
  }
}`

const liveSuggestedRefundDoc = `query LiveTestSuggestedRefund($id: ID!, $refundLineItems: [RefundLineItemInput!]) {
  order(id: $id) {
    suggestedRefund(refundLineItems: $refundLineItems) {
      suggestedTransactions {
        gateway
        kind
        parentTransaction { id }
        amountSet { shopMoney { amount } }
      }
    }
  }
}`

const liveRefundCreateDoc = `mutation LiveTestRefundCreate($input: RefundInput!) {
  refundCreate(input: $input) {
    refund { id }
    userErrors { field message }
  }
}`

// liveRefundCreateIdempotentDoc is the same mutation with the idempotency
// directive, sent only when Shopify answers that the version requires it.
const liveRefundCreateIdempotentDoc = `mutation LiveTestRefundCreate($input: RefundInput!, $key: String!) {
  refundCreate(input: $input) @idempotent(key: $key) {
    refund { id }
    userErrors { field message }
  }
}`

const liveOrderCancelDoc = `mutation LiveTestOrderCancel($orderId: ID!) {
  orderCancel(orderId: $orderId, reason: OTHER, refund: false, restock: true, notifyCustomer: false, staffNote: "memql-live-test") {
    job { id }
    orderCancelUserErrors { field message code }
  }
}`

const liveOrderStateDoc = `query LiveTestOrderState($id: ID!) {
  order(id: $id) {
    id
    cancelledAt
    customer { id defaultEmailAddress { emailAddress } }
  }
}`

const liveOrderDeleteDoc = `mutation LiveTestOrderDelete($orderId: ID!) {
  orderDelete(orderId: $orderId) {
    deletedId
    userErrors { field message code }
  }
}`

const liveDraftOrderDeleteDoc = `mutation LiveTestDraftOrderDelete($input: DraftOrderDeleteInput!) {
  draftOrderDelete(input: $input) {
    deletedId
    userErrors { field message }
  }
}`

const liveCustomerDeleteDoc = `mutation LiveTestCustomerDelete($input: CustomerDeleteInput!) {
  customerDelete(input: $input) {
    deletedCustomerId
    userErrors { field message }
  }
}`

// --- setup and cleanup --------------------------------------------------

// liveRequireDevelopmentStore fails the test unless the store says it is a
// partner development store. It runs when a test first resolves a store,
// before any mutation and before any sweep, once per store per test.
func (h *liveHarness) liveRequireDevelopmentStore(t *testing.T, store Store) {
	t.Helper()
	if h.developmentChecked[store.ID] {
		return
	}
	name := h.vars[store.ID]
	data, err := h.liveCall(h.ctx, store, liveShopPlanDoc, "LiveTestShopPlan", nil)
	if err != nil {
		t.Fatalf("could not read whether the store named by %s is a development store, so nothing was created: %v", name, err)
	}
	shop, _ := data["shop"].(map[string]any)
	plan, _ := shop["plan"].(map[string]any)
	dev, ok := plan["partnerDevelopment"].(bool)
	if !ok {
		t.Fatalf("could not read whether the store named by %s is a development store (shop.plan.partnerDevelopment was absent), so nothing was created", name)
	}
	if !dev {
		t.Fatalf("the store named by %s is not a partner development store (shop.plan.partnerDevelopment is false); this test runs only against a development store reserved for it, and nothing was created", name)
	}
	h.developmentChecked[store.ID] = true
}

// liveOrder is what one test made, and the only thing it may touch.
type liveOrder struct {
	store     Store
	runAt     time.Time
	email     string
	draftGID  string
	orderGID  string
	cancelled bool
}

func (h *liveHarness) grantedScopes(t *testing.T, store Store) []string {
	t.Helper()
	data := h.query(t, store, liveAccessScopesDoc, "LiveTestAccessScopes", nil)
	inst, _ := data["currentAppInstallation"].(map[string]any)
	list, _ := inst["accessScopes"].([]any)
	var out []string
	for _, s := range list {
		m, _ := s.(map[string]any)
		if handle, _ := m["handle"].(string); handle != "" {
			out = append(out, handle)
		}
	}
	if len(out) == 0 {
		t.Fatal("currentAppInstallation answered no access scopes for this token")
	}
	return out
}

// createPaidOrder makes a draft order of two units of the store's first
// variant, completes it as paid, and registers its removal before anything
// else can fail.
func (h *liveHarness) createPaidOrder(t *testing.T, store Store) *liveOrder {
	t.Helper()
	have := grantIncludes(h.grantedScopes(t, store))
	var lacking []string
	for _, s := range liveSetupScopes {
		if !have[s] {
			lacking = append(lacking, s)
		}
	}
	if len(lacking) > 0 {
		t.Fatalf("the token lacks %v, which the test's own setup needs (they are the test's, not the connection's); not a failure of the mirror", lacking)
	}

	data := h.query(t, store, liveFirstVariantDoc, "LiveTestFirstVariant", nil)
	variants, _ := data["productVariants"].(map[string]any)
	nodes, _ := variants["nodes"].([]any)
	if len(nodes) == 0 {
		t.Fatal("the development store has no product variant to order; add one product to it")
	}
	variantGID, _ := nodes[0].(map[string]any)["id"].(string)

	runAt := time.Now().UTC()
	o := &liveOrder{
		store: store,
		runAt: runAt,
		email: fmt.Sprintf("memql-live-test+%d@example.com", runAt.UnixNano()),
	}
	t.Cleanup(func() { h.remove(t, o) })

	created := h.mutate(t, store, liveDraftOrderCreateDoc, "LiveTestDraftOrderCreate", "draftOrderCreate", map[string]any{
		"input": map[string]any{
			"lineItems": []any{map[string]any{"variantId": variantGID, "quantity": 2}},
			"email":     o.email,
			"tags":      []any{liveTag},
			"note":      "memql live test, run " + runAt.Format(time.RFC3339),
		},
	})
	draft, _ := created["draftOrder"].(map[string]any)
	o.draftGID, _ = draft["id"].(string)
	if o.draftGID == "" {
		t.Fatal("draftOrderCreate answered no draft order id")
	}

	completed := h.mutate(t, store, liveDraftOrderCompleteDoc, "LiveTestDraftOrderComplete", "draftOrderComplete",
		map[string]any{"id": o.draftGID})
	draft, _ = completed["draftOrder"].(map[string]any)
	order, _ := draft["order"].(map[string]any)
	o.orderGID, _ = order["id"].(string)
	if o.orderGID == "" {
		t.Fatal("draftOrderComplete answered no order id")
	}
	return o
}

// probe reports a grant that does not reach the order's buyer, lines or
// payment, from Shopify's own refusal, before the connector reads it.
func (h *liveHarness) probe(t *testing.T, o *liveOrder) {
	t.Helper()
	h.query(t, o.store, liveProbeDoc, "LiveTestOrderProbe", map[string]any{"id": o.orderGID})
}

// cancel cancels the order and waits until Shopify reports it cancelled;
// orderCancel answers with a job, not a cancelled order.
func (h *liveHarness) cancel(ctx context.Context, o *liveOrder) error {
	if _, err := h.tryMutate(ctx, o.store, liveOrderCancelDoc, "LiveTestOrderCancel", "orderCancel",
		map[string]any{"orderId": o.orderGID}); err != nil {
		return err
	}
	for try := 0; try < livePollTries; try++ {
		data, err := h.liveCall(ctx, o.store, liveOrderStateDoc, "LiveTestOrderState", map[string]any{"id": o.orderGID})
		if err == nil {
			order, _ := data["order"].(map[string]any)
			if at, _ := order["cancelledAt"].(string); at != "" {
				o.cancelled = true
				return nil
			}
		}
		if err := sleepCtx(ctx, livePollWait); err != nil {
			return err
		}
	}
	return fmt.Errorf("waited %s for Shopify to report the order cancelled", time.Duration(livePollTries)*livePollWait)
}

// remove deletes what the test made, on a fresh context so a test that ran
// out of time still cleans up, and logs whatever Shopify would not delete.
func (h *liveHarness) remove(t *testing.T, o *liveOrder) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	left := func(what string, err error) {
		t.Logf("left in the store, tagged %s (run %s): %s: %v", liveTag, o.runAt.Format(time.RFC3339), what, err)
	}

	customerGID := ""
	if o.orderGID != "" {
		data, err := h.liveCall(ctx, o.store, liveOrderStateDoc, "LiveTestOrderState", map[string]any{"id": o.orderGID})
		if err == nil {
			order, _ := data["order"].(map[string]any)
			customer, _ := order["customer"].(map[string]any)
			// Only the buyer this run invented: the email is unique to it.
			if email := liveString(customer, "defaultEmailAddress", "emailAddress"); strings.EqualFold(email, o.email) {
				customerGID, _ = customer["id"].(string)
			}
			if at, _ := order["cancelledAt"].(string); at != "" {
				o.cancelled = true
			}
		} else {
			left("the invented buyer "+o.email+" may be (its customer could not be looked up)", err)
		}
		if !o.cancelled {
			if err := h.cancel(ctx, o); err != nil {
				left("order "+o.orderGID+" (not cancelled)", err)
			}
		}
		if _, err := h.tryMutate(ctx, o.store, liveOrderDeleteDoc, "LiveTestOrderDelete", "orderDelete",
			map[string]any{"orderId": o.orderGID}); err != nil {
			left("order "+o.orderGID, err)
		}
	}
	if o.draftGID != "" {
		if _, err := h.tryMutate(ctx, o.store, liveDraftOrderDeleteDoc, "LiveTestDraftOrderDelete", "draftOrderDelete",
			map[string]any{"input": map[string]any{"id": o.draftGID}}); err != nil {
			left("draft order "+o.draftGID, err)
		}
	}
	if customerGID != "" {
		if _, err := h.tryMutate(ctx, o.store, liveCustomerDeleteDoc, "LiveTestCustomerDelete", "customerDelete",
			map[string]any{"input": map[string]any{"id": customerGID}}); err != nil {
			left("customer "+customerGID+" (the invented buyer at example.com)", err)
		}
	}
}

// --- driving the connector ----------------------------------------------

// deliver runs the webhook apply path for one topic naming the order.
func (h *liveHarness) deliver(store Store, topic, gid string) ([]memqlsync.MirrorWrite, error) {
	ctx, cancel := context.WithTimeout(h.ctx, liveCallTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"admin_graphql_api_id": gid})
	return h.conn.Apply(ctx, memqlsync.InboundRequest{
		Source:     storeInboundSource(store.ID),
		Topic:      topic,
		Body:       body,
		ReceivedAt: time.Now().UTC(),
	})
}

// liveAwait retries a read that follows a write, a bounded number of times, and
// fails saying what it waited for and what it last saw.
func liveAwait(t *testing.T, ctx context.Context, what string, try func() (seen string, done bool)) {
	t.Helper()
	seen := ""
	for i := 0; i < livePollTries; i++ {
		var done bool
		if seen, done = try(); done {
			return
		}
		if err := sleepCtx(ctx, livePollWait); err != nil {
			break
		}
	}
	t.Fatalf("waited %s (%d tries) for %s; last seen: %s", time.Duration(livePollTries)*livePollWait, livePollTries, what, seen)
}

// deliverUntil applies a topic until the mirror writes it produces satisfy
// ok, and returns them.
func (h *liveHarness) deliverUntil(t *testing.T, store Store, topic, gid, what string, ok func([]memqlsync.MirrorWrite) (string, bool)) []memqlsync.MirrorWrite {
	t.Helper()
	var writes []memqlsync.MirrorWrite
	liveAwait(t, h.ctx, what+" after "+topic, func() (string, bool) {
		var err error
		writes, err = h.deliver(store, topic, gid)
		if err != nil {
			return liveRefusal(err), false
		}
		return ok(writes)
	})
	return writes
}

// requireNothingDenied fails when the connector had to leave fields out of a
// domain for this store: the grant, not the mapper.
func (h *liveHarness) requireNothingDenied(t *testing.T, store Store) {
	t.Helper()
	if denied := h.conn.FieldsDenied(store.ID); len(denied) > 0 {
		t.Fatalf("Shopify refused fields of the mirror's selection for this token, and the connector mirrored without them: %v; the app lacks a scope or the protected customer data declaration, not a failure of the mirror", denied)
	}
}

func livePayloadsOf(writes []memqlsync.MirrorWrite, concept string) []map[string]any {
	var out []map[string]any
	for _, w := range writes {
		if w.Concept == generated.ConceptID(concept) {
			out = append(out, w.Payload)
		}
	}
	return out
}

func liveOrderPayload(writes []memqlsync.MirrorWrite, gid string) map[string]any {
	for _, p := range livePayloadsOf(writes, "order") {
		if p["gid"] == gid {
			return p
		}
	}
	return nil
}

func liveChildrenOf(writes []memqlsync.MirrorWrite, concept, parentGID string) []map[string]any {
	var out []map[string]any
	for _, p := range livePayloadsOf(writes, concept) {
		if p["parentGid"] == parentGID {
			out = append(out, p)
		}
	}
	return out
}

// liveRequirePartition fails on any write that is not on the store's partition.
func liveRequirePartition(t *testing.T, writes []memqlsync.MirrorWrite, storeID string) {
	t.Helper()
	for _, w := range writes {
		if w.Partition != storeID || w.Payload["storeId"] != storeID {
			t.Errorf("%s row %s is on partition %q with storeId %v, want %s", w.Concept, w.RowId, w.Partition, w.Payload["storeId"], storeID)
		}
	}
}

func liveString(m map[string]any, path ...string) string {
	var v any = m
	for _, k := range path {
		mm, _ := v.(map[string]any)
		v = mm[k]
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func liveTags(p map[string]any) []string {
	var out []string
	list, _ := p["tags"].([]any)
	for _, s := range list {
		out = append(out, fmt.Sprint(s))
	}
	return out
}

// sweptInserts runs the updated_at sweep of orders and returns the payloads
// it wrote for one gid, decoded from the insert statements the engine
// recorded. The sweep writes rather than returning writes (mirror.go).
func (h *liveHarness) sweptInserts(t *testing.T, store Store, since time.Time, concept string) (memqlsync.ReconcileReport, []map[string]any) {
	t.Helper()
	mark := len(h.engine.calls())
	ctx, cancel := context.WithTimeout(h.ctx, 2*liveCallTimeout)
	defer cancel()
	report, err := h.conn.reconcileByUpdatedAt(ctx, store, generated.Types["order"], since)
	if err != nil {
		t.Fatalf("the updated_at sweep of orders: %s", liveRefusal(err))
	}
	return report, liveInsertedPayloads(t, h.engine.calls()[mark:], concept)
}

func liveInsertedPayloads(t *testing.T, calls []string, concept string) []map[string]any {
	t.Helper()
	prefix := "insert(" + langparser.QuoteString(generated.ConceptID(concept)) + ","
	var out []map[string]any
	for _, q := range calls {
		if !strings.HasPrefix(q, prefix) {
			continue
		}
		i := strings.Index(q, "payload=")
		if i < 0 || !strings.HasSuffix(q, ")") {
			t.Fatalf("an insert of %s did not carry its payload last", concept)
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(q[i+len("payload="):len(q)-1]), &p); err != nil {
			t.Fatalf("an insert of %s carried a payload that is not JSON: %v", concept, err)
		}
		out = append(out, p)
	}
	return out
}

func liveWithGID(payloads []map[string]any, gid string) map[string]any {
	for _, p := range payloads {
		if p["gid"] == gid {
			return p
		}
	}
	return nil
}

// liveOrderMirrored is the condition for a created order: the row, its lines,
// its payment and its buyer.
func liveOrderMirrored(o *liveOrder) func([]memqlsync.MirrorWrite) (string, bool) {
	return func(writes []memqlsync.MirrorWrite) (string, bool) {
		p := liveOrderPayload(writes, o.orderGID)
		if p == nil {
			return fmt.Sprintf("%d writes, none for the order", len(writes)), false
		}
		lines := liveChildrenOf(writes, "orderLineItem", o.orderGID)
		txns := liveChildrenOf(writes, "orderTransaction", o.orderGID)
		seen := fmt.Sprintf("financial status %q, %d lines, %d transactions, email set %t, customer %q",
			p["displayFinancialStatus"], len(lines), len(txns), p["email"] == o.email, p["customerGid"])
		// The buyer is the invented email; a customer row is linked when
		// Shopify made one for it, which the test does not require.
		return seen, p["displayFinancialStatus"] == "PAID" && len(lines) > 0 && len(txns) > 0 &&
			p["email"] == o.email
	}
}

// --- the tests ------------------------------------------------------------

// TestLiveTheGrantHoldsTheCommerceSet reads the token's own access scopes
// and holds them to the commerce set a managed connection asks for.
func TestLiveTheGrantHoldsTheCommerceSet(t *testing.T) {
	creds := liveEnv(t, envLiveDomain, envLiveToken)
	h := newLiveHarness(t, creds)
	store := h.store(t, 0)
	granted := h.grantedScopes(t, store)

	// The connection is measured as a managed one: the commerce set.
	managed := store
	managed.AdminTokenRef = storeSecretName(store.ID, "OFFLINE_GRANT")
	managed.ScopesGranted = granted
	if missing := scopesNotApproved(managed); len(missing) > 0 {
		t.Fatalf("the token's app does not hold %v of the commerce set; approve them on the app and again from the store's Shopify admin (the grant, not the mirror)", missing)
	}
	have := grantIncludes(granted)
	for _, s := range liveSetupScopes {
		if !have[s] {
			t.Logf("the token lacks %s, which the other live tests need for their own setup", s)
		}
	}
}

// TestLiveACreatedOrderIsMirroredWithLinesPaymentAndBuyer drives the
// orders/create apply path for an order the test made, and holds the writes
// to what it made: two units on one line, paid, by the invented buyer.
func TestLiveACreatedOrderIsMirroredWithLinesPaymentAndBuyer(t *testing.T) {
	creds := liveEnv(t, envLiveDomain, envLiveToken)
	h := newLiveHarness(t, creds)
	store := h.store(t, 0)
	o := h.createPaidOrder(t, store)
	h.probe(t, o)

	writes := h.deliverUntil(t, store, "orders/create", o.orderGID, "the order paid with its lines, payment and buyer", liveOrderMirrored(o))
	h.requireNothingDenied(t, store)
	liveRequirePartition(t, writes, store.ID)

	p := liveOrderPayload(writes, o.orderGID)
	if !slices.Contains(liveTags(p), liveTag) {
		t.Errorf("order tags = %v, want %s among them", liveTags(p), liveTag)
	}
	lines := liveChildrenOf(writes, "orderLineItem", o.orderGID)
	if len(lines) != 1 || liveString(lines[0], "quantity") != "2" || liveString(lines[0], "variantGid") == "" {
		t.Errorf("lines = %v, want one line of 2 units of a variant", lines)
	}
	sale := false
	for _, tx := range liveChildrenOf(writes, "orderTransaction", o.orderGID) {
		if tx["kind"] == "SALE" && tx["status"] == "SUCCESS" {
			sale = true
		}
	}
	if !sale {
		t.Errorf("no successful SALE transaction among %v", liveChildrenOf(writes, "orderTransaction", o.orderGID))
	}
}

// TestLiveAnEditARefundAndACancellationFollow changes the order three ways
// and holds each topic's writes to the change, then sweeps and finds the
// last state again on the reconcile path.
func TestLiveAnEditARefundAndACancellationFollow(t *testing.T) {
	creds := liveEnv(t, envLiveDomain, envLiveToken)
	h := newLiveHarness(t, creds)
	store := h.store(t, 0)
	o := h.createPaidOrder(t, store)
	h.probe(t, o)
	created := h.deliverUntil(t, store, "orders/create", o.orderGID, "the order paid with its lines, payment and buyer", liveOrderMirrored(o))
	h.requireNothingDenied(t, store)
	createdAt := liveString(liveOrderPayload(created, o.orderGID), "sourceCreatedAt")

	// The edit: the one line from 2 units to 3.
	begun := h.mutate(t, store, liveOrderEditBeginDoc, "LiveTestOrderEditBegin", "orderEditBegin", map[string]any{"id": o.orderGID})
	calc, _ := begun["calculatedOrder"].(map[string]any)
	calcID, _ := calc["id"].(string)
	calcLines, _ := calc["lineItems"].(map[string]any)
	calcNodes, _ := calcLines["nodes"].([]any)
	if calcID == "" || len(calcNodes) != 1 {
		t.Fatalf("orderEditBegin answered calculated order %q with %d lines, want one line", calcID, len(calcNodes))
	}
	calcLineID, _ := calcNodes[0].(map[string]any)["id"].(string)
	h.mutate(t, store, liveOrderEditSetQuantityDoc, "LiveTestOrderEditSetQuantity", "orderEditSetQuantity",
		map[string]any{"id": calcID, "lineItemId": calcLineID, "quantity": 3})
	h.mutate(t, store, liveOrderEditCommitDoc, "LiveTestOrderEditCommit", "orderEditCommit", map[string]any{"id": calcID})

	edited := h.deliverUntil(t, store, "orders/edited", o.orderGID, "the edit (edited, the line at 3 units)",
		func(writes []memqlsync.MirrorWrite) (string, bool) {
			p := liveOrderPayload(writes, o.orderGID)
			lines := liveChildrenOf(writes, "orderLineItem", o.orderGID)
			if p == nil || len(lines) == 0 {
				return "no order row or no lines", false
			}
			qty := liveString(lines[0], "currentQuantity")
			return fmt.Sprintf("edited %v, %d lines, first at %s", p["edited"], len(lines), qty), p["edited"] == true && qty == "3"
		})
	liveRequirePartition(t, edited, store.ID)
	lineGID := liveString(liveChildrenOf(edited, "orderLineItem", o.orderGID)[0], "gid")

	// The refund: one unit of the line, with the money Shopify suggests.
	refundLines := []any{map[string]any{"lineItemId": lineGID, "quantity": 1, "restockType": "NO_RESTOCK"}}
	suggested := h.query(t, store, liveSuggestedRefundDoc, "LiveTestSuggestedRefund", map[string]any{
		"id": o.orderGID, "refundLineItems": []any{map[string]any{"lineItemId": lineGID, "quantity": 1}},
	})
	var transactions []any
	order, _ := suggested["order"].(map[string]any)
	refund, _ := order["suggestedRefund"].(map[string]any)
	list, _ := refund["suggestedTransactions"].([]any)
	for _, s := range list {
		m, _ := s.(map[string]any)
		amount := liveString(m, "amountSet", "shopMoney", "amount")
		parent := liveString(m, "parentTransaction", "id")
		if amount == "" || parent == "" {
			continue
		}
		transactions = append(transactions, map[string]any{
			"orderId": o.orderGID, "parentId": parent, "amount": amount,
			"gateway": liveString(m, "gateway"), "kind": "REFUND",
		})
	}
	input := map[string]any{
		"orderId":         o.orderGID,
		"note":            "memql live test, run " + o.runAt.Format(time.RFC3339),
		"notify":          false,
		"refundLineItems": refundLines,
	}
	if len(transactions) > 0 {
		input["transactions"] = transactions
	}
	if _, err := h.tryMutate(h.ctx, store, liveRefundCreateDoc, "LiveTestRefundCreate", "refundCreate", map[string]any{"input": input}); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "idempot") {
			t.Fatal(err)
		}
		h.mutate(t, store, liveRefundCreateIdempotentDoc, "LiveTestRefundCreate", "refundCreate",
			map[string]any{"input": input, "key": fmt.Sprintf("memql-live-test-%d", o.runAt.UnixNano())})
	}
	refunded := h.deliverUntil(t, store, "orders/updated", o.orderGID, "the refund row under the order",
		func(writes []memqlsync.MirrorWrite) (string, bool) {
			refunds := liveChildrenOf(writes, "refund", o.orderGID)
			return fmt.Sprintf("%d refund rows", len(refunds)), len(refunds) == 1
		})
	liveRequirePartition(t, refunded, store.ID)
	if len(transactions) > 0 {
		p := liveOrderPayload(refunded, o.orderGID)
		if amt := liveString(p, "totalRefundedSet", "shopMoney", "amount"); amt == "" || amt == "0.0" || amt == "0" {
			t.Errorf("totalRefundedSet = %v after a refund with money, want it above zero", p["totalRefundedSet"])
		}
		found := false
		for _, tx := range liveChildrenOf(refunded, "orderTransaction", o.orderGID) {
			if tx["kind"] == "REFUND" {
				found = true
			}
		}
		if !found {
			t.Error("no REFUND transaction under the order after the refund")
		}
	}

	// The cancellation.
	if err := h.cancel(h.ctx, o); err != nil {
		t.Fatal(err)
	}
	cancelled := h.deliverUntil(t, store, "orders/cancelled", o.orderGID, "cancelledAt on the order",
		func(writes []memqlsync.MirrorWrite) (string, bool) {
			p := liveOrderPayload(writes, o.orderGID)
			at := liveString(p, "cancelledAt")
			return fmt.Sprintf("cancelledAt %q, cancelReason %v", at, p["cancelReason"]), at != ""
		})
	liveRequirePartition(t, cancelled, store.ID)

	// The sweep finds the same last state: cancelled, edited, refunded.
	since, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		t.Fatalf("the mirrored sourceCreatedAt %q is not RFC 3339: %v", createdAt, err)
	}
	since = since.Add(-time.Second)
	mismatch := ""
	liveAwait(t, h.ctx, "the updated_at sweep to write the cancelled order", func() (string, bool) {
		report, payloads := h.sweptInserts(t, store, since, "order")
		p := liveWithGID(payloads, o.orderGID)
		if p == nil {
			return fmt.Sprintf("checked %d, our order not among %d order inserts", report.Checked, len(payloads)), false
		}
		mismatch = ""
		if p["storeId"] != store.ID {
			mismatch = fmt.Sprintf("the swept order carries storeId %v, want %s", p["storeId"], store.ID)
		}
		return fmt.Sprintf("cancelledAt %v, edited %v", p["cancelledAt"], p["edited"]),
			liveString(p, "cancelledAt") != "" && p["edited"] == true
	})
	if mismatch != "" {
		t.Error(mismatch)
	}
}

// TestLiveAnOrdersFulfilmentsAreReadUnderTheCommerceSet answers the
// Unverified entry in docs/public/operate/shopify-connector.md: whether
// Shopify answers an order's fulfilments to a grant of the commerce set,
// which carries read_orders and no fulfilment scope. It reports either way
// and fails only when the order itself is not mirrored.
//
// What it can see is field access, on an order with no fulfilment yet:
// making one would need a fulfilment scope, and a token holding one cannot
// answer what read_orders alone reaches.
func TestLiveAnOrdersFulfilmentsAreReadUnderTheCommerceSet(t *testing.T) {
	creds := liveEnv(t, envLiveDomain, envLiveToken)
	h := newLiveHarness(t, creds)
	store := h.store(t, 0)
	for _, s := range h.grantedScopes(t, store) {
		if strings.Contains(s, "fulfillment") {
			t.Skipf("the token holds %s beyond the commerce set, so this run cannot say what read_orders alone reaches", s)
		}
	}
	o := h.createPaidOrder(t, store)
	writes := h.deliverUntil(t, store, "orders/create", o.orderGID, "the order row", func(writes []memqlsync.MirrorWrite) (string, bool) {
		return fmt.Sprintf("%d writes", len(writes)), liveOrderPayload(writes, o.orderGID) != nil
	})
	p := liveOrderPayload(writes, o.orderGID)
	denied := h.conn.FieldsDenied(store.ID)[generated.ConceptID("order")]
	for _, field := range []string{"fulfillments", "fulfillmentOrders", "fulfillmentsCount"} {
		if slices.Contains(denied, field) {
			t.Logf("FINDING: under the commerce set Shopify refused Order.%s; the order was mirrored without it", field)
			continue
		}
		t.Logf("FINDING: under the commerce set Shopify answered Order.%s", field)
	}
	t.Logf("FINDING: the unfulfilled order mirrored %d fulfilment rows and fulfillmentOrdersGids %v, fulfillmentsCount %v",
		len(liveChildrenOf(writes, "fulfillment", o.orderGID)), p["fulfillmentOrdersGids"], p["fulfillmentsCount"])
}

// TestLiveASecondStoreKeepsItsOwnState runs the same paths against two
// stores in one connector and holds every write, sweep and piece of
// per-store state to its own store. The runtime's syncState rows are
// component/datasync's and outside this package; what the connector itself
// keeps per store -- the partition, the rows' ids, the cost bucket and the
// fields a grant refused -- is what is checked here.
func TestLiveASecondStoreKeepsItsOwnState(t *testing.T) {
	first := liveEnv(t, envLiveDomain, envLiveToken)
	second := liveEnv(t, envLiveDomain2, envLiveToken2)
	a, _ := NormalizeShopDomain(first.domain)
	b, _ := NormalizeShopDomain(second.domain)
	if a == b {
		t.Fatalf("%s names the same store as %s; the two-store test needs two stores", envLiveDomain2, envLiveDomain)
	}
	h := newLiveHarness(t, first, second)
	stores := []Store{h.store(t, 0), h.store(t, 1)}

	parts, err := h.conn.Partitions(h.ctx)
	if err != nil || !slices.Equal(parts, []string{stores[0].ID, stores[1].ID}) {
		t.Fatalf("Partitions = %v, %v; want both stores", parts, err)
	}

	orders := make([]*liveOrder, len(stores))
	created := make([][]memqlsync.MirrorWrite, len(stores))
	for i, store := range stores {
		orders[i] = h.createPaidOrder(t, store)
		h.probe(t, orders[i])
		created[i] = h.deliverUntil(t, store, "orders/create", orders[i].orderGID,
			"store "+store.ID+"'s order paid with its lines, payment and buyer", liveOrderMirrored(orders[i]))
		liveRequirePartition(t, created[i], store.ID)
	}
	for _, w := range created[0] {
		for _, x := range created[1] {
			if w.RowId == x.RowId {
				t.Errorf("row %s is written by both stores", w.RowId)
			}
		}
	}
	for i, store := range stores {
		if _, ok := h.conn.admin.Bucket(store.ID); !ok {
			t.Errorf("no cost bucket recorded for store %s", store.ID)
		}
		if denied := h.conn.FieldsDenied(store.ID); len(denied) > 0 {
			t.Errorf("store %s's grant refused %v", stores[i].ID, denied)
		}
	}

	// The second store's sweep, through the runtime's partitioned entry,
	// writes the second store's order and nothing of the first's.
	createdAt := liveString(liveOrderPayload(created[1], orders[1].orderGID), "sourceCreatedAt")
	since, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		t.Fatalf("the mirrored sourceCreatedAt %q is not RFC 3339: %v", createdAt, err)
	}
	since = since.Add(-time.Second)
	mismatch := ""
	liveAwait(t, h.ctx, "the second store's sweep to write its order", func() (string, bool) {
		mark := len(h.engine.calls())
		ctx, cancel := context.WithTimeout(h.ctx, 2*liveCallTimeout)
		report, err := h.conn.ReconcilePartition(ctx, stores[1].ID, generated.ConceptID("order"), since)
		cancel()
		if err != nil {
			return liveRefusal(err), false
		}
		payloads := liveInsertedPayloads(t, h.engine.calls()[mark:], "order")
		for _, p := range payloads {
			if p["storeId"] != stores[1].ID {
				mismatch = fmt.Sprintf("the second store's sweep wrote an order with storeId %v", p["storeId"])
			}
			if p["gid"] == orders[0].orderGID {
				mismatch = "the second store's sweep wrote the first store's order"
			}
		}
		return fmt.Sprintf("checked %d, %d order inserts", report.Checked, len(payloads)), liveWithGID(payloads, orders[1].orderGID) != nil
	})
	if mismatch != "" {
		t.Error(mismatch)
	}
}
