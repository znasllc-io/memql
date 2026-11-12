package shopify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// apply_test.go -- the live path (#4393).
//
// Every test here asserts something about the SHAPE of the conversation
// rather than about a value the fake happened to return: which Admin
// operation ran, whether one ran at all, and which MemQL function the
// connector called with what. That is the half of the exchange this package
// owns.

func productReply(gid, title, updatedAt string) map[string]any {
	return map[string]any{
		"node": map[string]any{
			"__typename": "Product",
			"id":         gid,
			"title":      title,
			"updatedAt":  updatedAt,
			"handle":     "linen-shirt",
			"variants": map[string]any{"nodes": []any{
				map[string]any{"id": "gid://shopify/ProductVariant/9", "title": "M", "availableForSale": true, "updatedAt": updatedAt},
			}},
			"metafields": map[string]any{"nodes": []any{
				map[string]any{"namespace": "custom", "key": "care", "type": "single_line_text_field", "value": "cold wash"},
			}},
		},
	}
}

func delivery(topic, gid string) memqlsync.InboundRequest {
	body, _ := json.Marshal(map[string]any{"admin_graphql_api_id": gid, "updated_at": "2026-08-23T11:00:00Z"})
	return memqlsync.InboundRequest{
		RequestId: "inb1",
		Source:    "shopify-" + testStoreID,
		Topic:     topic,
		Body:      body,
		Headers: map[string]string{
			"x-shopify-topic":       topic,
			"x-shopify-shop-domain": "acme-widgets.myshopify.com",
			// The webhook id rides a HEADER, not a field: the contract's
			// InboundRequest carries the delivery as staged, and the
			// sender's idempotency key is one of its headers.
			"x-shopify-webhook-id": "wh-" + gid + "-" + topic,
		},
	}
}

func TestApplyFetchesByGidAndWritesTheRowAndItsChildren(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyFetchProduct", productReply("gid://shopify/Product/1", "Linen shirt", "2026-08-23T11:00:00Z"))

	writes, err := h.conn.Apply(context.Background(), delivery("products/update", "gid://shopify/Product/1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 2 {
		t.Fatalf("want the product and its one variant, got %d writes", len(writes))
	}
	if writes[0].Concept != "v1:shopify:product" || writes[0].Payload["gid"] != "gid://shopify/Product/1" {
		t.Errorf("parent write = %+v", writes[0])
	}
	if writes[1].Concept != "v1:shopify:productVariant" || writes[1].Payload["parentGid"] != "gid://shopify/Product/1" {
		t.Errorf("child write = %+v", writes[1])
	}
	// PARENT BEFORE CHILD, in the slice. The writer applies in order, and a
	// child landing first is a row whose parentGid names nothing.
	if writes[0].Concept == writes[1].Concept {
		t.Error("the parent and the child must be different concepts")
	}
	if got := writes[0].Payload["title"]; got != "Linen shirt" {
		t.Errorf("title = %v", got)
	}
	metafields, ok := writes[0].Payload["metafields"].(map[string]any)
	if !ok || metafields["custom.care"] == nil {
		t.Errorf("metafields = %v, want a namespace.key map", writes[0].Payload["metafields"])
	}
	if writes[0].Version != "2026-08-23T11:00:00Z" {
		t.Errorf("version = %s, want the ORIGIN's updatedAt", writes[0].Version)
	}
	// The identity fields ride the PAYLOAD, because the runtime writes the
	// payload wholesale and nothing else reaches the row.
	if writes[0].Payload["storeId"] != testStoreID || writes[0].Payload["syncedAt"] == nil {
		t.Errorf("payload is missing its mirror columns: %v", writes[0].Payload)
	}
}

// The payload is a SIGNAL. Nothing in the apply path may read a business
// field out of it -- webhook payloads lose fields, truncate at 100 variants
// and arrive out of order, so a mirror built from them is a copy of the
// webhook rather than of the store.
func TestApplyReadsNoBusinessFieldFromThePayload(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyFetchProduct", productReply("gid://shopify/Product/1", "The API's title", "2026-08-23T11:00:00Z"))

	req := delivery("products/update", "gid://shopify/Product/1")
	body, _ := json.Marshal(map[string]any{
		"admin_graphql_api_id": "gid://shopify/Product/1",
		"title":                "The PAYLOAD's title",
		"handle":               "payload-handle",
	})
	req.Body = body

	writes, err := h.conn.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := writes[0].Payload["title"]; got != "The API's title" {
		t.Errorf("title = %v -- the payload's value reached the mirror", got)
	}
	if got := writes[0].Payload["handle"]; got != "linen-shirt" {
		t.Errorf("handle = %v -- the payload's value reached the mirror", got)
	}
}

func TestApplyIgnoresADuplicateWebhookId(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyFetchProduct", productReply("gid://shopify/Product/1", "Linen shirt", "2026-08-23T11:00:00Z"))
	req := delivery("products/update", "gid://shopify/Product/1")

	if _, err := h.conn.Apply(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	writes, err := h.conn.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 0 {
		t.Errorf("a redelivery produced %d writes", len(writes))
	}
	if n := h.admin.countOp("ShopifyFetchProduct"); n != 1 {
		t.Errorf("the Admin API was called %d times for one event -- the dedupe short-circuit is what saves the cost points", n)
	}
}

func TestEveryDeleteTopicTombstonesWithoutAFetch(t *testing.T) {
	h := newHarness(t)
	deletes := 0
	for topic, route := range generated.Topics {
		if route.Action != generated.ActionDelete {
			continue
		}
		deletes++
		spec := generated.Types[route.Concept]
		gid := "gid://shopify/" + spec.GraphQLType + "/1"
		req := delivery(generated.EnumToTopicHeader(topic), gid)
		writes, err := h.conn.Apply(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", topic, err)
		}
		if len(writes) != 1 || !writes[0].Retire {
			t.Fatalf("%s produced %+v, want one tombstone", topic, writes)
		}
	}
	if deletes < 10 {
		t.Fatalf("only %d delete topics found -- the routing table is not being read", deletes)
	}
	if len(h.admin.seen()) != 0 {
		t.Errorf("a delete topic called the Admin API %d times; a tombstone needs no fetch", len(h.admin.seen()))
	}
}

// A create/update topic for an object Shopify no longer has. The delete
// delivery may arrive later, out of order, or never -- so an empty fetch
// tombstones, which is what makes the mirror converge without it.
func TestAFetchThatFindsNothingTombstones(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyFetchProduct", map[string]any{"node": nil})
	// A record the mirror holds: one it does not hold has nothing to retire
	// (TestAnEmptyFetchForARecordNotHeldWritesNothing).
	rowID := MirrorRowID(testStoreID, "gid://shopify/Product/1")
	h.engine.setRows(`concept==v1:shopify:product && id=="`+rowID+`"`, []map[string]any{{"id": rowID, "gid": "gid://shopify/Product/1"}})

	// On the retry after an empty answer: one empty answer is asked again
	// before it is believed (TestTwoCleanEmptyAnswersInARowAreADeletion).
	req := delivery("products/update", "gid://shopify/Product/1")
	req.Attempts, req.AnsweredNothingBefore = 1, true
	writes, err := h.conn.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || !writes[0].Retire {
		t.Fatalf("writes = %+v, want one tombstone", writes)
	}
}

// node(id:) resolves ANY type, and an inline fragment on the wrong one comes
// back carrying nothing but __typename. Without this check that is a mirror
// row with every field blank -- a silent corruption rather than an error.
func TestAFetchThatResolvesTheWrongTypeIsRefused(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyFetchProduct", map[string]any{
		"node": map[string]any{"__typename": "Collection", "id": "gid://shopify/Collection/7"},
	})
	_, err := h.conn.Apply(context.Background(), delivery("products/update", "gid://shopify/Product/1"))
	if err == nil || !strings.Contains(err.Error(), "not a Product") {
		t.Fatalf("err = %v, want a refusal naming the resolved type", err)
	}
}

func TestApplyIsANoOpForAnUnknownStoreAndAnUnmirroredTopic(t *testing.T) {
	h := newHarness(t)

	unknown := delivery("products/update", "gid://shopify/Product/1")
	unknown.Source = "shopify-nobody"
	unknown.Headers = map[string]string{}
	if writes, err := h.conn.Apply(context.Background(), unknown); err != nil || len(writes) != 0 {
		t.Errorf("unknown store: writes=%d err=%v -- a removed store must not fail a delivery Shopify retries for four hours", len(writes), err)
	}

	if writes, err := h.conn.Apply(context.Background(), delivery("app/scopes_update", "gid://shopify/Shop/1")); err != nil || len(writes) != 0 {
		t.Errorf("unmirrored topic: writes=%d err=%v", len(writes), err)
	}

	// app/uninstalled is not unmirrored any more (memql#5638): it disconnects
	// the store, but only on the APP-LEVEL source, where it is declared and
	// bound by the signed body (uninstall_test.go). On a per-store URL, which a
	// managed store's app secret also verifies, it is still the no-op above.
	if writes, err := h.conn.Apply(context.Background(), delivery(TopicAppUninstalled, "gid://shopify/Shop/1")); err != nil || len(writes) != 0 {
		t.Errorf("app/uninstalled on a per-store source: writes=%d err=%v", len(writes), err)
	}
	if n := len(h.engine.callsTo("markStoreUninstalled")); n != 0 {
		t.Errorf("app/uninstalled on a per-store source disconnected the store (%d writes)", n)
	}
}

// The three id spellings Shopify has used. A connector that knew only the
// modern one would silently ignore every delivery of the others, which looks
// exactly like a store that has stopped changing.
func TestDeliveredGidReadsEverySpelling(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"admin_graphql_api_id", map[string]any{"admin_graphql_api_id": "gid://shopify/Order/5"}, "gid://shopify/Order/5"},
		{"numeric id", map[string]any{"id": 5}, "gid://shopify/Order/5"},
		{"string id", map[string]any{"id": "5"}, "gid://shopify/Order/5"},
		{"gid under id", map[string]any{"id": "gid://shopify/Order/5"}, "gid://shopify/Order/5"},
		{"nothing", map[string]any{"name": "#1001"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.body)
			if got := deliveredGID(body, "Order"); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// riskAssessmentBody is the body of an orders/risk_assessment_changed
// delivery, with the keys Shopify's webhook reference shows for the topic. It
// is ABOUT an order and carries no `id` or `admin_graphql_api_id` of its own:
// the order is named by `admin_graphql_api_order_id` and `order_id`.
func riskAssessmentBody(orderID any, orderGID any) []byte {
	body, _ := json.Marshal(map[string]any{
		"provider_id":                nil,
		"provider_title":             nil,
		"risk_level":                 "medium",
		"created_at":                 nil,
		"order_id":                   orderID,
		"admin_graphql_api_order_id": orderGID,
	})
	return body
}

// A DELIVERY ABOUT AN ORDER NAMES THE ORDER. Most deliveries are the object:
// they carry its `id`. A few are about one -- a risk assessment is about an
// order -- and carry no id of their own, only the order's, under
// `admin_graphql_api_order_id` and `order_id`. Read as carrying nothing, every
// such delivery failed all four of its tries and ended given up.
func TestDeliveredGidReadsTheOrderADeliveryIsAbout(t *testing.T) {
	marshal := func(body map[string]any) []byte {
		b, _ := json.Marshal(body)
		return b
	}
	cases := []struct {
		name string
		typ  string
		body []byte
		want string
	}{
		{"the order's gid", "Order", riskAssessmentBody(7, "gid://shopify/Order/7"), "gid://shopify/Order/7"},
		{"the order's numeric id alone", "Order", riskAssessmentBody(7, nil), "gid://shopify/Order/7"},
		{"the order's gid alone", "Order", riskAssessmentBody(nil, "gid://shopify/Order/7"), "gid://shopify/Order/7"},
		// A gid key holding something that is not a gid is not believed; the
		// numeric id beside it still names the order.
		{"a gid key that holds no gid", "Order", riskAssessmentBody(7, "7"), "gid://shopify/Order/7"},
		// orders/shopify_protect_eligibility_changed: the numeric key alone.
		{"a protect-eligibility delivery", "Order", marshal(map[string]any{"order_id": 7, "status": "active", "eligibility": map[string]any{"status": "eligible"}}), "gid://shopify/Order/7"},
		// The reference's own sample: both keys present and null.
		{"both keys null", "Order", riskAssessmentBody(nil, nil), ""},
		// THE OBJECT'S OWN ID WINS. A fulfilment and a refund carry their own
		// id AND the id of the order they belong to; they are read by their
		// own, exactly as before.
		{"its own id beside an order's", "Fulfillment", marshal(map[string]any{"id": 9, "order_id": 7}), "gid://shopify/Fulfillment/9"},
		{"an order's own id beside an order_id", "Order", marshal(map[string]any{"id": 9, "order_id": 7}), "gid://shopify/Order/9"},
		{"its own gid beside an order's", "Refund", marshal(map[string]any{
			"admin_graphql_api_id": "gid://shopify/Refund/9", "order_id": 7, "admin_graphql_api_order_id": "gid://shopify/Order/7",
		}), "gid://shopify/Refund/9"},
		// An order's id says nothing about a delivery routed to another type.
		{"an order's id on another type's route", "Fulfillment", riskAssessmentBody(7, "gid://shopify/Order/7"), ""},
		// HELD TO AN ORDER. A customer's consent and segment deliveries name
		// their customer the same way and are deliberately not read: each
		// would start a customer fetch (deliveredGID says why that is not
		// decided here).
		{"a customer's is not read", "Customer", marshal(map[string]any{"shop_id": 1, "customer_id": 5, "segment_id": 9}), ""},
		{"nothing", "Order", marshal(map[string]any{"risk_level": "medium"}), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deliveredGID(tc.body, tc.typ); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// THE DELIVERY IS FETCHED, NOT GIVEN UP. Through Apply: a risk-assessment
// delivery fetches the order it is about and writes it, as an orders/updated
// delivery for that order would.
func TestARiskAssessmentDeliveryFetchesItsOrder(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyFetchOrder", orderWithLines(lineItemPage(1, 1, false, "")))

	req := delivery("orders/risk_assessment_changed", orderGID)
	req.Body = riskAssessmentBody(7, orderGID)
	writes, err := h.conn.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("a risk-assessment delivery failed: %v", err)
	}
	if len(writes) == 0 || writes[0].Concept != generated.ConceptID("order") || writes[0].Payload["gid"] != orderGID {
		t.Fatalf("writes = %+v, want the order the delivery is about first", writes)
	}
	fetched := false
	for _, r := range h.admin.seen() {
		if r.Operation == "ShopifyFetchOrder" && r.Variables["id"] == orderGID {
			fetched = true
		}
	}
	if !fetched {
		t.Fatalf("the order was not fetched by its gid: %v", h.admin.seenOps())
	}
}

// A delivery that names no object still fails, in the same words: the
// reference's sample for the topic carries both keys null.
func TestADeliveryThatNamesNoObjectStillFails(t *testing.T) {
	h := newHarness(t)
	req := delivery("orders/risk_assessment_changed", orderGID)
	req.Body = riskAssessmentBody(nil, nil)
	_, err := h.conn.Apply(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "orders/risk_assessment_changed delivery carried no object id") {
		t.Fatalf("err = %v, want the delivery refused for carrying no object id", err)
	}
	if n := h.admin.countOp("ShopifyFetchOrder"); n != 0 {
		t.Fatalf("a delivery naming no order fetched one %d times", n)
	}
}

func TestTopicHeaderMapsOntoTheEnumSpelling(t *testing.T) {
	// The two spellings exist because subscriptions are created through
	// GraphQL and deliveries arrive over HTTP. A connector that compared the
	// header against the enum would match nothing and mirror an empty store.
	if got := generated.TopicHeaderToEnum("orders/updated"); got != "ORDERS_UPDATED" {
		t.Errorf("got %q", got)
	}
	if _, ok := generated.RouteForHeader("products/create"); !ok {
		t.Error("products/create does not route")
	}
	if _, ok := generated.RouteForHeader("PRODUCTS_CREATE"); !ok {
		t.Error("the enum spelling must route too -- both reach this function")
	}
}

// A RETRY IS FETCHED AGAIN (U4). The short-circuit remembers a webhook id for
// fifteen minutes, and the runtime retries a failed delivery one and five
// minutes later. Were a failed fetch remembered, both retries would be
// answered "already seen" and the change never fetched. So a delivery is
// remembered only once it applied, and the runtime can tell the connector to
// forget one whose writes it could not apply.
func TestARetriedDeliveryIsFetchedAgainNotSwallowedAsADuplicate(t *testing.T) {
	t.Run("the first fetch fails", func(t *testing.T) {
		h := newHarness(t)
		h.admin.reply("ShopifyFetchProduct", productReply("gid://shopify/Product/1", "Linen shirt", "2026-08-23T11:00:00Z"))
		h.admin.failOnce("ShopifyFetchProduct", GraphQLError{Message: "internal error", Extensions: map[string]any{"code": "INTERNAL_SERVER_ERROR"}})
		req := delivery("products/update", "gid://shopify/Product/1")

		if _, err := h.conn.Apply(context.Background(), req); err == nil {
			t.Fatal("the failed fetch reported success")
		}
		writes, err := h.conn.Apply(context.Background(), req)
		if err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if len(writes) == 0 || h.admin.countOp("ShopifyFetchProduct") != 2 {
			t.Fatalf("the retry produced %d writes from %d fetches; want the same webhook id fetched again",
				len(writes), h.admin.countOp("ShopifyFetchProduct"))
		}
	})
	t.Run("the runtime could not apply what was fetched", func(t *testing.T) {
		h := newHarness(t)
		h.admin.reply("ShopifyFetchProduct", productReply("gid://shopify/Product/1", "Linen shirt", "2026-08-23T11:00:00Z"))
		req := delivery("products/update", "gid://shopify/Product/1")

		if _, err := h.conn.Apply(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		var forgetter memqlsync.InboundForgetter = h.conn
		forgetter.ForgetInbound(context.Background(), req)
		writes, err := h.conn.Apply(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if len(writes) == 0 || h.admin.countOp("ShopifyFetchProduct") != 2 {
			t.Fatalf("after ForgetInbound the delivery produced %d writes from %d fetches; want it fetched again",
				len(writes), h.admin.countOp("ShopifyFetchProduct"))
		}
	})
}

// A store pinned to another Admin API version is refused on every try the
// same way until somebody regenerates or repins, so the refusal is permanent:
// the retry pass must not spend four tries on it.
func TestAStorePinnedToAnotherVersionIsAPermanentRefusal(t *testing.T) {
	h := newHarness(t)
	h.engine.setRows("stores", []map[string]any{{
		"id": testStoreID, "domain": "acme-widgets.myshopify.com", "name": "Acme Widgets",
		"adminTokenRef": "ACME_ADMIN", "webhookSecretRef": "ACME_WEBHOOK", "apiVersion": "2020-01",
		"protectedDataLevel": ProtectedLevel1, "status": StatusLive, "ownerUserId": "user-1",
	}})
	_, err := h.conn.Apply(context.Background(), delivery("products/update", "gid://shopify/Product/1"))
	if err == nil || !strings.Contains(err.Error(), "pinned to Admin API 2020-01") {
		t.Fatalf("err = %v, want the version refusal", err)
	}
	if !memqlsync.IsPermanent(err) {
		t.Fatalf("err = %v is not permanent; a pinned store is refused the same way on every retry", err)
	}
	if n := h.admin.countOp("ShopifyFetchProduct"); n != 0 {
		t.Fatalf("the Admin API was called %d times for a store pinned to another version", n)
	}
}
