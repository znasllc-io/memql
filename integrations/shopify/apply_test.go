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

	writes, err := h.conn.Apply(context.Background(), delivery("products/update", "gid://shopify/Product/1"))
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

func TestApplyIsANoOpForAPausedStore(t *testing.T) {
	h := newHarness(t)
	h.engine.setRows("stores", []map[string]any{{
		"id": testStoreID, "domain": "acme-widgets.myshopify.com",
		"adminTokenRef": "ACME_ADMIN", "status": StatusPaused,
	}})
	h.conn.stores.Invalidate()

	writes, err := h.conn.Apply(context.Background(), delivery("products/update", "gid://shopify/Product/1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 0 {
		t.Errorf("a paused store produced %d writes", len(writes))
	}
	if len(h.admin.seen()) != 0 {
		t.Error("a paused store still called the Admin API")
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

	if writes, err := h.conn.Apply(context.Background(), delivery("app/uninstalled", "gid://shopify/Shop/1")); err != nil || len(writes) != 0 {
		t.Errorf("unmirrored topic: writes=%d err=%v", len(writes), err)
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
