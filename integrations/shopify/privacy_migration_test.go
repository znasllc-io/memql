package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// privacy_migration_test.go -- the operator migration memql#5707 left behind.
//
// Before memql#5707 the runbook told operators to point the three compliance
// topics at the per-store URL /inbound/shopify-<storeId>. Since then a store
// installed through the managed app has its privacy topics refused there, so
// a cluster configured by the old runbook rejects every mandatory privacy
// delivery. The refusal stays (a captured delivery replays onto the per-store
// URL); what these tests pin is that it cannot be missed: the staged row's
// lastError names the URL to configure, an ERROR is logged, every refusal is
// on the audit trail, and the store's health counts them.

const managedStoreBody = `{"shop_domain":"acme.myshopify.com","shop_id":954889}`

func managedStoreHarness(t *testing.T) *testHarness {
	t.Helper()
	h := managedWebhookHarness(t)
	h.engine.setRows("stores", []map[string]any{{"id": "acme", "domain": "acme.myshopify.com", "appClientId": "managed-app", "status": StatusLive}})
	return h
}

func privacyOnStoreURL(topic, body, requestID string) memqlsync.InboundRequest {
	return memqlsync.InboundRequest{
		RequestId: requestID, Source: "shopify-acme", Topic: topic, Body: []byte(body),
		Headers: map[string]string{strings.ToLower(HeaderTopic): topic, strings.ToLower(HeaderShopDomain): "acme.myshopify.com"},
	}
}

func TestAPrivacyDeliveryOnAManagedStoresOwnURLNamesTheURLToConfigure(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "shop.example.test")
	bodies := map[string]string{
		TopicDataRequest: `{"shop_domain":"acme.myshopify.com","customer":{"id":991},"data_request":{"id":42}}`,
		TopicRedact:      `{"shop_domain":"acme.myshopify.com","customer":{"id":991},"orders_to_redact":[1]}`,
		TopicShopRedact:  managedStoreBody,
	}
	for topic, body := range bodies {
		t.Run(topic, func(t *testing.T) {
			h := managedStoreHarness(t)
			var logs bytes.Buffer
			h.conn.logger = slog.New(slog.NewTextHandler(&logs, nil))

			_, err := h.conn.Apply(context.Background(), privacyOnStoreURL(topic, body, "inb-1"))
			if err == nil {
				t.Fatal("a managed store's privacy delivery on its per-store URL was accepted")
			}
			if n := len(h.engine.callsTo("queueComplianceJob")); n != 0 {
				t.Fatalf("the refused delivery queued %d privacy jobs", n)
			}
			// The lastError the dispatcher stamps IS this error, so it has to
			// be the whole instruction: what was refused, where it arrived,
			// and the exact URL to configure instead.
			for _, want := range []string{
				topic, "/inbound/shopify-acme", "https://api.shop.example.test/inbound/shopify ",
				"app's configuration at Shopify", "customers/data_request, customers/redact and shop/redact",
				"Step 5", "does not resend", privacyRefusalAction,
			} {
				if !strings.Contains(err.Error()+" ", want) {
					t.Errorf("the refusal does not say %q:\n%s", want, err)
				}
			}

			audits := h.engine.callsTo("createAuditEvent")
			if len(audits) != 1 {
				t.Fatalf("the refusal wrote %d audit events, want exactly 1", len(audits))
			}
			for _, want := range []string{
				`action: "` + privacyRefusalAction + `"`, `outcome: "blocked"`, `failureReason: "` + privacyRefusalReason + `"`,
				`targetType: "shopifyStore"`, `targetId: "acme"`, `"topic": "` + topic + `"`, `"source": "shopify-acme"`,
				`"inboundRequestId": "inb-1"`,
			} {
				if !strings.Contains(audits[0], want) {
					t.Errorf("the refusal's audit event does not carry %s:\n%s", want, audits[0])
				}
			}

			out := logs.String()
			for _, want := range []string{"level=ERROR", "store=acme", "https://api.shop.example.test/inbound/shopify"} {
				if !strings.Contains(out, want) {
					t.Errorf("the refusal's log line does not say %q:\n%s", want, out)
				}
			}
		})
	}
}

// A staged row the dispatcher works twice is ONE refusal: the audit event id
// derives from the staged request, so storeHealth does not count a retry of
// the same delivery as a second lost request.
func TestARedispatchedRefusalIsOneAuditEvent(t *testing.T) {
	h := managedStoreHarness(t)
	for i := 0; i < 2; i++ {
		_, _ = h.conn.Apply(context.Background(), privacyOnStoreURL(TopicShopRedact, managedStoreBody, "inb-same"))
	}
	_, _ = h.conn.Apply(context.Background(), privacyOnStoreURL(TopicShopRedact, managedStoreBody, "inb-other"))
	ids := map[string]int{}
	for _, call := range h.engine.callsTo("createAuditEvent") {
		ids[between(call, `eventId: "`, `"`)]++
	}
	if len(ids) != 2 {
		t.Fatalf("three dispatches of two staged rows produced %d distinct audit ids, want 2: %v", len(ids), ids)
	}
}

// With no MEMQL_DOMAIN the URL still says what to configure, rather than
// printing "https://api./inbound/shopify".
func TestTheAppLevelURLNamesThePlaceholderWithoutADomain(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "")
	if got, want := appLevelDeliveryURL(), "https://api.<your-domain>/inbound/shopify"; got != want {
		t.Fatalf("appLevelDeliveryURL() = %q, want %q", got, want)
	}
}

func TestStoreHealthCountsRefusedPrivacyDeliveries(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "shop.example.test")
	h := managedStoreHarness(t)
	h.engine.setRows("auditEventsByTarget", []map[string]any{
		{"id": "aud1", "action": privacyRefusalAction, "targetType": "shopifyStore", "targetId": "acme", "occurredAt": "2026-09-01T10:00:00Z"},
		{"id": "aud2", "action": "shopify_customer_redact_received", "targetType": "shopifyStore", "targetId": "acme", "occurredAt": "2026-09-03T10:00:00Z"},
		{"id": "aud3", "action": privacyRefusalAction, "targetType": "shopifyStore", "targetId": "acme", "occurredAt": "2026-09-02T10:00:00Z"},
		// Another kind of target that happens to share the id is not this
		// store's refusal.
		{"id": "aud4", "action": privacyRefusalAction, "targetType": "user", "targetId": "acme", "occurredAt": "2026-09-04T10:00:00Z"},
	})
	got := privacyDeliveriesOf(t, h)
	if got["refused"] != float64(2) || got["lastRefusedAt"] != "2026-09-02T10:00:00Z" || got["capped"] != false {
		t.Fatalf("privacyDeliveries = %v; want refused=2, lastRefusedAt=2026-09-02T10:00:00Z, capped=false", got)
	}
	if got["appLevelUrl"] != "https://api.shop.example.test/inbound/shopify" {
		t.Errorf("privacyDeliveries.appLevelUrl = %v", got["appLevelUrl"])
	}
	if calls := h.engine.callsTo("auditEventsByTarget"); len(calls) != 1 || !strings.Contains(calls[0], `targetId: "acme"`) {
		t.Errorf("the count was not read from the store's own audit trail: %v", calls)
	}
}

// A store with nothing refused reports a measured zero -- the audit trail WAS
// read -- which the Store panel renders as no warning at all.
func TestStoreHealthReportsZeroRefusedPrivacyDeliveries(t *testing.T) {
	h := managedStoreHarness(t)
	got := privacyDeliveriesOf(t, h)
	if got["refused"] != float64(0) || got["lastRefusedAt"] != "" || got["capped"] != false {
		t.Fatalf("privacyDeliveries = %v; want a measured zero", got)
	}
}

// The walk is bounded, and when it stops early the figure says it is a floor.
func TestStoreHealthSaysWhenTheRefusalCountIsCapped(t *testing.T) {
	h := managedStoreHarness(t)
	h.engine.setRows("auditEventsByTarget", []map[string]any{
		{"id": "aud1", "action": privacyRefusalAction, "targetType": "shopifyStore", "targetId": "acme", "occurredAt": "2026-09-01T10:00:00Z"},
	})
	h.engine.cursors["auditEventsByTarget"] = "more"
	got := privacyDeliveriesOf(t, h)
	if got["capped"] != true || got["refused"] != float64(privacyRefusalMaxPages) {
		t.Fatalf("privacyDeliveries = %v; want capped=true after %d pages", got, privacyRefusalMaxPages)
	}
	if n := len(h.engine.callsTo("auditEventsByTarget")); n != privacyRefusalMaxPages {
		t.Fatalf("the walk read %d pages, want the cap of %d", n, privacyRefusalMaxPages)
	}
}

func privacyDeliveriesOf(t *testing.T, h *testHarness) map[string]any {
	t.Helper()
	nodes, err := NewIntegration(h.conn).handleStoreHealth(context.Background(), map[string]any{"storeId": "acme"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Stores []map[string]any `json:"stores"`
	}
	if len(nodes) != 1 || json.Unmarshal(nodes[0].Payload, &reply) != nil || len(reply.Stores) != 1 {
		t.Fatalf("storeHealth replied %d nodes: %v", len(nodes), nodes)
	}
	got, ok := reply.Stores[0]["privacyDeliveries"].(map[string]any)
	if !ok {
		t.Fatalf("the store's health carries no privacyDeliveries: %v", reply.Stores[0])
	}
	return got
}

func between(s, open, close string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	s = s[i+len(open):]
	if j := strings.Index(s, close); j >= 0 {
		return s[:j]
	}
	return s
}
