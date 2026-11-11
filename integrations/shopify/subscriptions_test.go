package shopify

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// subscriptions_test.go -- registration and the daily reconcile (#4392).

func webhookList(nodes ...map[string]any) map[string]any {
	list := make([]any, 0, len(nodes))
	for _, n := range nodes {
		list = append(list, n)
	}
	return map[string]any{"webhookSubscriptions": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
		"nodes":    list,
	}}
}

func subscription(id, topic, url, version string, fields []string) map[string]any {
	include := make([]any, 0, len(fields))
	for _, f := range fields {
		include = append(include, f)
	}
	return map[string]any{
		"id": id, "topic": topic, "includeFields": include,
		"apiVersion": map[string]any{"handle": version},
		"uri":        url,
	}
}

const testCallback = "https://api.example.test/inbound/shopify-acme"

func TestEnsureSubscriptionsCreatesEveryMissingTopic(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyWebhookSubscriptions", webhookList())
	h.admin.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{"userErrors": []any{}}})

	report, err := h.conn.EnsureSubscriptionsForStore(context.Background(), h.store(t))
	if err != nil {
		t.Fatal(err)
	}
	if report.Desired != len(generated.SubscribedTopics) {
		t.Fatalf("desired %d, want %d", report.Desired, len(generated.SubscribedTopics))
	}
	if len(report.Created) != report.Desired || len(report.Failed) != 0 {
		t.Fatalf("created %d of %d, failed %v", len(report.Created), report.Desired, report.Failed)
	}
	if report.Desired < 100 {
		t.Fatalf("only %d topics are subscribed; the generated set should be about 150", report.Desired)
	}

	var create *adminRequest
	for i, r := range h.admin.seen() {
		if r.Operation == "ShopifyWebhookCreate" {
			create = &h.admin.seen()[i]
			break
		}
	}
	if create == nil {
		t.Fatal("no create ran")
	}
	sub, _ := create.Variables["sub"].(map[string]any)
	if sub["uri"] != testCallback {
		t.Errorf("uri = %v, want the store's delivery address", sub["uri"])
	}
	// THE INPUT HOLDS ONLY WHAT SHOPIFY'S WebhookSubscriptionInput DEFINES.
	// One key it does not define refuses the whole call ("Field `apiVersion`
	// is not defined by type `WebhookSubscriptionInput`"), and that is how a
	// production store went without a single subscription: every one of its
	// 66 creations was refused, and the mirror moved only when a sweep ran.
	// The version events are serialized at is the app's, set in its
	// configuration at Shopify, and cannot be asked for here.
	for key := range sub {
		switch key {
		case "uri", "format", "includeFields":
		default:
			t.Errorf("the subscription input carries %q, which WebhookSubscriptionInput does not define or has deprecated", key)
		}
	}
	// includeFields is trimmed to identity plus a change timestamp: the
	// payload is a SIGNAL and the object is fetched. A full payload would be
	// bigger, lossier than the API, and would tempt a future reader into
	// writing it.
	include, _ := sub["includeFields"].([]any)
	if len(include) != len(includeFields) {
		t.Fatalf("includeFields = %v, want %v", include, includeFields)
	}
	if create.Token != "shpat_test" {
		t.Errorf("token = %q -- the store's own credential must be used", create.Token)
	}
}

func TestEnsureSubscriptionsUpdatesAStaleOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		sub  map[string]any
	}{
		{"wrong url", subscription("gid://shopify/WebhookSubscription/1", "ORDERS_UPDATED", "https://old.example/inbound/x", generated.APIVersion, includeFields)},
		{"changed includeFields", subscription("gid://shopify/WebhookSubscription/1", "ORDERS_UPDATED", testCallback, generated.APIVersion, []string{"id"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.admin.reply("ShopifyWebhookSubscriptions", webhookList(tc.sub))
			h.admin.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{"userErrors": []any{}}})
			h.admin.reply("ShopifyWebhookUpdate", map[string]any{"webhookSubscriptionUpdate": map[string]any{"userErrors": []any{}}})

			report, err := h.conn.EnsureSubscriptionsForStore(context.Background(), h.store(t))
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Updated) != 1 || report.Updated[0] != "ORDERS_UPDATED" {
				t.Fatalf("updated = %v", report.Updated)
			}
		})
	}
}

// The version a subscription's events are serialized at is inherited from the
// app that created it: Shopify takes it from the app's configuration and an
// update cannot change it. So a subscription at another version is not
// rewritten -- the daily pass would rewrite it every day and change nothing --
// and the version is REPORTED, because the fix is in the app's settings.
func TestAWebhookVersionTheAppDeliversAtIsReportedNotRewritten(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyWebhookSubscriptions", webhookList(
		subscription("gid://shopify/WebhookSubscription/1", "ORDERS_UPDATED", testCallback, "2024-01", includeFields)))
	h.admin.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{"userErrors": []any{}}})

	report, err := h.conn.EnsureSubscriptionsForStore(context.Background(), h.store(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range h.admin.seen() {
		if r.Operation == "ShopifyWebhookUpdate" {
			t.Fatal("a subscription was rewritten for its version, which an update cannot change")
		}
	}
	if report.DeliveredAtVersion != "2024-01" {
		t.Errorf("DeliveredAtVersion = %q, want the version Shopify reports, 2024-01", report.DeliveredAtVersion)
	}
}

// A store whose subscriptions are at the pinned version reports no other.
func TestAWebhookVersionThatMatchesThePinIsNotReported(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyWebhookSubscriptions", webhookList(
		subscription("gid://shopify/WebhookSubscription/1", "ORDERS_UPDATED", testCallback, generated.APIVersion, includeFields)))
	h.admin.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{"userErrors": []any{}}})

	report, err := h.conn.EnsureSubscriptionsForStore(context.Background(), h.store(t))
	if err != nil {
		t.Fatal(err)
	}
	if report.DeliveredAtVersion != "" {
		t.Errorf("DeliveredAtVersion = %q, want none when Shopify delivers at the pin", report.DeliveredAtVersion)
	}
}

func TestEnsureSubscriptionsLeavesAHealthyOneAlone(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyWebhookSubscriptions", webhookList(
		subscription("gid://shopify/WebhookSubscription/1", "ORDERS_UPDATED", testCallback, generated.APIVersion, includeFields)))
	h.admin.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{"userErrors": []any{}}})

	report, err := h.conn.EnsureSubscriptionsForStore(context.Background(), h.store(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, topic := range report.Updated {
		if topic == "ORDERS_UPDATED" {
			t.Error("a healthy subscription was rewritten")
		}
	}
}

// Ours that the allowlist no longer wants is removed; ANOTHER app's
// subscription -- or one pointing anywhere else -- is not. The difference
// between "we tidy up after ourselves" and "we delete whatever we can see",
// and only the first is safe to run daily.
func TestEnsureSubscriptionsRemovesOnlyItsOwnExtras(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyWebhookSubscriptions", webhookList(
		subscription("gid://shopify/WebhookSubscription/9", "APP_UNINSTALLED", testCallback, generated.APIVersion, includeFields),
		subscription("gid://shopify/WebhookSubscription/8", "APP_SCOPES_UPDATE", "https://someone-else.example/hook", generated.APIVersion, includeFields),
	))
	h.admin.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{"userErrors": []any{}}})
	h.admin.reply("ShopifyWebhookDelete", map[string]any{"webhookSubscriptionDelete": map[string]any{"userErrors": []any{}}})

	report, err := h.conn.EnsureSubscriptionsForStore(context.Background(), h.store(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Removed) != 1 || report.Removed[0] != "APP_UNINSTALLED" {
		t.Fatalf("removed = %v, want only our own extra", report.Removed)
	}
}

func TestASubscriptionUserErrorIsReportedNotSwallowed(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyWebhookSubscriptions", webhookList())
	h.admin.userError("ShopifyWebhookCreate", "webhookSubscriptionCreate", "Address is not allowed")

	report, err := h.conn.EnsureSubscriptionsForStore(context.Background(), h.store(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failed) == 0 {
		t.Fatal("a userError inside a 200 response was reported as success -- the trap of this API")
	}
	if !strings.Contains(report.Failed[0], "Address is not allowed") {
		t.Errorf("failure = %q, want the vendor's own message", report.Failed[0])
	}
}

func TestEnsureSubscriptionsRecordsHealthOnTheStore(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyWebhookSubscriptions", webhookList())
	h.admin.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{"userErrors": []any{}}})

	if err := h.conn.EnsureSubscriptions(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := h.engine.callsTo("recordStoreHealth")
	if len(calls) != 1 {
		t.Fatalf("recordStoreHealth ran %d times", len(calls))
	}
	// The reconcile TIME is what tells an operator the daily pass ran at
	// all. Shopify deletes a subscription after eight consecutive failures,
	// so a store that has gone quiet and a store nobody checked look the
	// same without it.
	if !strings.Contains(calls[0], "subscriptionsCheckedAt") {
		t.Errorf("health write carries no reconcile time:\n%s", calls[0])
	}
	if !strings.Contains(calls[0], "subscriptions") {
		t.Errorf("health write carries no subscription report:\n%s", calls[0])
	}
}

// The version Shopify delivers at goes onto the store's health with the rest
// of the pass: it is the one part of a subscription nothing here can change,
// so an operator has to be able to read it.
func TestTheDeliveredVersionIsRecordedOnTheStoresHealth(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyWebhookSubscriptions", webhookList(
		subscription("gid://shopify/WebhookSubscription/1", "ORDERS_UPDATED", testCallback, "2024-01", includeFields)))
	h.admin.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{"userErrors": []any{}}})

	if err := h.conn.EnsureSubscriptions(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := h.engine.callsTo("recordStoreHealth")
	if len(calls) != 1 {
		t.Fatalf("recordStoreHealth ran %d times", len(calls))
	}
	if !strings.Contains(calls[0], "deliveredAtVersion") || !strings.Contains(calls[0], "2024-01") {
		t.Errorf("health write does not say which version Shopify delivers at:\n%s", calls[0])
	}
}

func TestEnsureSubscriptionsSkipsAPausedStore(t *testing.T) {
	h := newHarness(t)
	h.engine.setRows("stores", []map[string]any{{
		"id": testStoreID, "domain": "acme.myshopify.com", "adminTokenRef": "ACME_ADMIN", "status": StatusPaused,
	}})
	h.conn.stores.Invalidate()
	if err := h.conn.EnsureSubscriptions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.admin.seen()) != 0 {
		t.Error("a paused store's subscriptions were reconciled")
	}
}

// BULK_OPERATIONS_FINISH is subscribed even though it mirrors nothing: the
// connector needs its own bulk completions delivered, and a backfill that
// waited for the poll interval instead finishes on the hour rather than in
// minutes.
func TestBulkOperationsFinishIsSubscribed(t *testing.T) {
	found := false
	for _, topic := range generated.SubscribedTopics {
		if topic == generated.TopicBulkOperationsFinish {
			found = true
		}
	}
	if !found {
		t.Error("BULK_OPERATIONS_FINISH is not in the subscribed set")
	}
	if _, mirrored := generated.Topics[generated.TopicBulkOperationsFinish]; mirrored {
		t.Error("BULK_OPERATIONS_FINISH must not route to a concept -- it routes to the backfill runner")
	}
}

// The compliance topics are declared in the app configuration, not created
// through webhookSubscriptionCreate: they are not members of the
// WebhookSubscriptionTopic enum, so a create would fail.
func TestComplianceTopicsAreNotSubscribedThroughTheApi(t *testing.T) {
	for _, topic := range generated.ComplianceTopics {
		for _, subscribed := range generated.SubscribedTopics {
			if generated.TopicHeaderToEnum(topic) == subscribed {
				t.Errorf("%s is subscribed through the API; it is declared on the app instead", topic)
			}
		}
	}
	if len(generated.ComplianceTopics) != 3 {
		t.Errorf("want the three mandatory topics, got %v", generated.ComplianceTopics)
	}
}

// createdTopics is every topic a ShopifyWebhookCreate asked Shopify for.
func createdTopics(h *testHarness) map[string]bool {
	out := map[string]bool{}
	for _, r := range h.admin.seen() {
		if r.Operation == "ShopifyWebhookCreate" {
			topic, _ := r.Variables["topic"].(string)
			out[topic] = true
		}
	}
	return out
}

// THE GRANT DECIDES WHAT IS ASKED FOR (memql#5638, G3). A store connected
// through Connect Shopify holds the managed scopes and nothing else, and 97 of
// the generated topics route to concepts none of those scopes reads -- the
// customer topics under read_customers, fulfillment orders, returns, discounts.
// Shopify refuses each such creation, the refusal lands on the store's
// subscription health, and the daily reconcile asks again. The rule is
// reconciliation's own (scopesMissingFor): a topic is asked for when the grant
// holds ANY scope its concept reads under.
func TestEnsureSubscriptionsAsksOnlyForTopicsTheGrantCovers(t *testing.T) {
	h := newHarness(t)
	withScopes(t, h, managedConnectScopes()...)
	h.admin.reply("ShopifyWebhookSubscriptions", webhookList(
		// One of ours, created under a wider grant this store no longer holds.
		subscription("gid://shopify/WebhookSubscription/5", "CUSTOMERS_UPDATE", testCallback, generated.APIVersion, includeFields),
	))
	h.admin.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{"userErrors": []any{}}})
	h.admin.reply("ShopifyWebhookDelete", map[string]any{"webhookSubscriptionDelete": map[string]any{"userErrors": []any{}}})

	report, err := h.conn.EnsureSubscriptionsForStore(context.Background(), h.store(t))
	if err != nil {
		t.Fatal(err)
	}
	created := createdTopics(h)
	for _, topic := range []string{"CUSTOMERS_CREATE", "CUSTOMERS_UPDATE", "RETURNS_REQUEST", "FULFILLMENT_ORDERS_MOVED"} {
		if created[topic] {
			t.Errorf("%s was asked for under a grant holding none of its scopes", topic)
		}
	}
	// The grant's own reach is still subscribed: write_products reads
	// products, and read_orders and read_inventory are held as they are.
	for _, topic := range []string{"PRODUCTS_UPDATE", "ORDERS_UPDATED", "INVENTORY_ITEMS_UPDATE", generated.TopicBulkOperationsFinish} {
		if !created[topic] {
			t.Errorf("%s was not asked for, and the grant covers it", topic)
		}
	}
	if len(report.Failed) != 0 {
		t.Errorf("failed = %v", report.Failed)
	}
	if report.Desired != len(created) {
		t.Errorf("desired = %d, created = %d: the report must count what was asked for, not the generated set", report.Desired, len(created))
	}
	if len(report.NotGranted) == 0 || report.Desired+len(report.NotGranted) != len(generated.SubscribedTopics) {
		t.Errorf("notGranted = %d, desired = %d, generated = %d: every generated topic is either asked for or reported as outside the grant",
			len(report.NotGranted), report.Desired, len(generated.SubscribedTopics))
	}
	// Ours, for a topic the grant no longer covers: tidied up like any other
	// topic this store does not want.
	if len(report.Removed) != 1 || report.Removed[0] != "CUSTOMERS_UPDATE" {
		t.Errorf("removed = %v, want the out-of-grant subscription", report.Removed)
	}
}

// The other side of the same rule: a fuller grant still subscribes to every
// generated topic, so the filter narrows only what the grant cannot hold.
func TestEnsureSubscriptionsAsksForEveryTopicUnderAFullGrant(t *testing.T) {
	h := newHarness(t)
	withScopes(t, h, append(append([]string(nil), StorefrontScopes...), generated.Scopes...)...)
	h.admin.reply("ShopifyWebhookSubscriptions", webhookList())
	h.admin.reply("ShopifyWebhookCreate", map[string]any{"webhookSubscriptionCreate": map[string]any{"userErrors": []any{}}})

	report, err := h.conn.EnsureSubscriptionsForStore(context.Background(), h.store(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(createdTopics(h)); got != len(generated.SubscribedTopics) || report.Desired != got || len(report.NotGranted) != 0 {
		t.Fatalf("created %d, desired %d, notGranted %v; want every one of %d generated topics", got, report.Desired, report.NotGranted, len(generated.SubscribedTopics))
	}
	if !createdTopics(h)["CUSTOMERS_UPDATE"] {
		t.Error("a grant holding read_customers was not subscribed to CUSTOMERS_UPDATE")
	}
}
