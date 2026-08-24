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
		"endpoint":   map[string]any{"__typename": "WebhookHttpEndpoint", "callbackUrl": url},
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
	if sub["callbackUrl"] != testCallback {
		t.Errorf("callbackUrl = %v", sub["callbackUrl"])
	}
	if sub["apiVersion"] != generated.APIVersion {
		t.Errorf("apiVersion = %v, want the pinned %s", sub["apiVersion"], generated.APIVersion)
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
		{"stale api version", subscription("gid://shopify/WebhookSubscription/1", "ORDERS_UPDATED", testCallback, "2024-01", includeFields)},
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
