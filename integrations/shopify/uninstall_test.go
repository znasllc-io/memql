package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// uninstall_test.go -- app/uninstalled on the app-level endpoint (memql#5638,
// G1), against the recording engine and the fake Admin API.

const uninstallShop = "acme-widgets.myshopify.com"

// uninstallHarness is a managed store: installed through the cluster's app,
// with an Admin grant, a Storefront token and an owner whose saved connection
// names it -- the state Connect Shopify leaves.
func uninstallHarness(t *testing.T) *testHarness {
	t.Helper()
	h := managedWebhookHarness(t)
	row := map[string]any{
		"id": "v1:shopify:store:" + testStoreID, "domain": uninstallShop, "appClientId": "managed-app",
		"adminTokenRef":      storeSecretName(testStoreID, suffixAdminToken),
		"storefrontTokenRef": storeSecretName(testStoreID, suffixStorefrontToken),
		"status":             StatusConfigured, "ownerUserId": "user-1",
	}
	h.engine.setRows("stores", []map[string]any{row})
	h.engine.setRows("storeById", []map[string]any{row})
	h.engine.setRows("externalConnectionsMine", []map[string]any{
		{"id": "conn-acme", "ownerUserId": "user-1", "provider": "shopify", "resourceId": testStoreID, "label": uninstallShop, "status": "active"},
		{"id": "conn-other", "ownerUserId": "user-1", "provider": "shopify", "resourceId": "other-store", "label": "other.myshopify.com", "status": "active"},
	})
	h.conn.stores.secrets = func(_ context.Context, name string) (string, error) {
		switch name {
		case managedClientSecret:
			return "managed-secret", nil
		case storeSecretName(testStoreID, suffixAdminToken):
			return "shpat_the_grant", nil
		}
		return "", fmt.Errorf("no secret %q", name)
	}
	// The grant Shopify ended answers 401, as every token does after an
	// uninstall. A test that wants the reinstall case scripts a reply.
	h.admin.httpStatus("ShopifyReachable", http.StatusUnauthorized)
	h.conn.connectAppGate = func(context.Context, string) (func(), error) { return func() {}, nil }
	return h
}

// uninstallDelivery is app/uninstalled as Shopify delivers it to the app-level
// endpoint: the Shop resource, which names the shop `myshopify_domain`.
func uninstallDelivery(source string, body map[string]any) memqlsync.InboundRequest {
	raw, _ := json.Marshal(body)
	return memqlsync.InboundRequest{
		RequestId: "inb-uninstall", Source: source, Topic: TopicAppUninstalled, Body: raw,
		Headers: map[string]string{
			strings.ToLower(HeaderTopic):       TopicAppUninstalled,
			strings.ToLower(HeaderShopDomain):  uninstallShop,
			strings.ToLower(HeaderWebhookID):   "wh-uninstall-1",
			strings.ToLower(HeaderTriggeredAt): "2026-10-01T12:00:00Z",
		},
	}
}

func shopBody(domain string) map[string]any {
	return map[string]any{"id": 548380009, "name": "Acme Widgets", "domain": "shop.acme.example", "myshopify_domain": domain}
}

func TestAppUninstalledDisconnectsTheStore(t *testing.T) {
	h := uninstallHarness(t)
	var held bool
	h.conn.connectAppGate = func(_ context.Context, key string) (func(), error) {
		if key != "offline:"+testStoreID {
			t.Errorf("locked %q, want the store's offline lock -- the one a reinstall and a refresh hold", key)
		}
		if h.admin.countOp("ShopifyReachable") != 0 {
			t.Error("the grant was judged before the lock was held")
		}
		held = true
		return func() {
			if len(h.engine.callsTo("markStoreUninstalled")) != 1 {
				t.Error("the lock was released before the store was disconnected")
			}
			held = false
		}, nil
	}

	writes, err := h.conn.Apply(context.Background(), uninstallDelivery(ConnectorName, shopBody(uninstallShop)))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(writes) != 0 {
		t.Errorf("app/uninstalled produced %d mirror writes; it mirrors nothing", len(writes))
	}
	if held {
		t.Error("the offline lock was never released")
	}

	marks := h.engine.callsTo("markStoreUninstalled")
	if len(marks) != 1 || !strings.Contains(marks[0], `storeId: "`+testStoreID+`"`) || !strings.Contains(marks[0], `uninstalledAt: "2026-10-01T12:00:00Z"`) {
		t.Fatalf("markStoreUninstalled = %v, want one, for the store, at the delivery's triggered-at", marks)
	}
	// The dead grant is neutralised: its sealed row blanked, at the name the
	// store named.
	blanked := false
	for _, call := range h.engine.callsTo("setGlobalSecret") {
		if strings.Contains(call, storeSecretName(testStoreID, suffixAdminToken)) && strings.Contains(call, `encryptedValue: ""`) {
			blanked = true
		}
	}
	if !blanked {
		t.Errorf("the dead grant was not blanked: %v", h.engine.callsTo("setGlobalSecret"))
	}
	// The owner's saved connection to THIS store reads disconnected; the one to
	// another store is untouched.
	disconnected := h.engine.callsTo("markExternalConnectionDisconnected")
	if len(disconnected) != 1 || !strings.Contains(disconnected[0], `"conn-acme"`) {
		t.Errorf("markExternalConnectionDisconnected = %v, want only the connection naming this store", disconnected)
	}
	audits := h.engine.callsTo("createAuditEvent")
	if len(audits) != 1 || !strings.Contains(audits[0], `action: "shopify_app_uninstalled"`) || !strings.Contains(audits[0], `targetId: "`+testStoreID+`"`) {
		t.Errorf("audit = %v, want one shopify_app_uninstalled event on the store", audits)
	}
	// Purging is shop/redact's: nothing here queues or runs one.
	if n := len(h.engine.callsTo("queueComplianceJob")) + len(h.engine.callsTo("markStoreRedacted")); n != 0 {
		t.Errorf("an uninstall queued or ran a purge (%d calls)", n)
	}
}

// A delivery is a trigger and the grant is the truth: Shopify retries an
// undelivered webhook for four hours, so an app/uninstalled can arrive after
// the shop reinstalled and Connect sealed a fresh grant. That grant answers,
// and the store is left alone.
func TestAnUninstallSupersededByAReinstallChangesNothing(t *testing.T) {
	h := uninstallHarness(t)
	h.admin.httpStatus("ShopifyReachable", http.StatusOK)
	h.admin.reply("ShopifyReachable", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/548380009"}})

	if _, err := h.conn.Apply(context.Background(), uninstallDelivery(ConnectorName, shopBody(uninstallShop))); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, name := range []string{"markStoreUninstalled", "setGlobalSecret", "markExternalConnectionDisconnected"} {
		if n := len(h.engine.callsTo(name)); n != 0 {
			t.Errorf("a superseded uninstall ran %s %d times", name, n)
		}
	}
	if h.admin.countOp("ShopifyReachable") != 1 {
		t.Errorf("the grant was asked %d times, want once", h.admin.countOp("ShopifyReachable"))
	}
	audits := h.engine.callsTo("createAuditEvent")
	if len(audits) != 1 || !strings.Contains(audits[0], `action: "shopify_app_uninstall_superseded"`) {
		t.Errorf("audit = %v, want the superseded uninstall recorded", audits)
	}
}

// Bound the way a privacy delivery is (managedAppLevelStore): the shop is the
// one the SIGNED body names, the header may not disagree with it, and the
// store must be this app's. A PAUSED store is disconnected too -- a pause stops
// the mirror, not the uninstall.
func TestAppUninstalledIsBoundLikeAPrivacyDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, source, client, status string
		body                         map[string]any
		header                       string
		want                         bool
	}{
		{"the managed store", ConnectorName, "managed-app", StatusConfigured, shopBody(uninstallShop), uninstallShop, true},
		{"a paused managed store", ConnectorName, "managed-app", StatusPaused, shopBody(uninstallShop), uninstallShop, true},
		{"no header", ConnectorName, "managed-app", StatusConfigured, shopBody(uninstallShop), "", true},
		{"another app's store", ConnectorName, "another-app", StatusConfigured, shopBody(uninstallShop), uninstallShop, false},
		{"a header naming another shop", ConnectorName, "managed-app", StatusConfigured, shopBody(uninstallShop), "beta.myshopify.com", false},
		{"a body naming another shop", ConnectorName, "managed-app", StatusConfigured, shopBody("beta.myshopify.com"), uninstallShop, false},
		{"the shop under the privacy field", ConnectorName, "managed-app", StatusConfigured, map[string]any{"shop_domain": uninstallShop}, uninstallShop, false},
		{"no signed shop", ConnectorName, "managed-app", StatusConfigured, map[string]any{"id": 1}, uninstallShop, false},
		{"the store's per-store source", "shopify-" + testStoreID, "managed-app", StatusConfigured, shopBody(uninstallShop), uninstallShop, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := uninstallHarness(t)
			row := map[string]any{
				"id": "v1:shopify:store:" + testStoreID, "domain": uninstallShop, "appClientId": tc.client,
				"adminTokenRef": storeSecretName(testStoreID, suffixAdminToken), "status": tc.status, "ownerUserId": "user-1",
			}
			h.engine.setRows("stores", []map[string]any{row})
			h.engine.setRows("storeById", []map[string]any{row})
			req := uninstallDelivery(tc.source, tc.body)
			req.Headers[strings.ToLower(HeaderShopDomain)] = tc.header
			if _, err := h.conn.Apply(context.Background(), req); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got := len(h.engine.callsTo("markStoreUninstalled")) == 1; got != tc.want {
				t.Fatalf("store disconnected = %v, want %v", got, tc.want)
			}
		})
	}
}

// A store already disconnected has no grant to ask about and nothing more to
// change; a second delivery for it runs the same idempotent writes and asks
// Shopify nothing.
func TestAnUninstallForAStoreWithNoGrantAsksShopifyNothing(t *testing.T) {
	h := uninstallHarness(t)
	row := map[string]any{
		"id": "v1:shopify:store:" + testStoreID, "domain": uninstallShop, "appClientId": "managed-app",
		"adminTokenRef": "", "status": StatusConfigured, "ownerUserId": "user-1", "uninstalledAt": "2026-09-30T12:00:00Z",
	}
	h.engine.setRows("stores", []map[string]any{row})
	h.engine.setRows("storeById", []map[string]any{row})
	if _, err := h.conn.Apply(context.Background(), uninstallDelivery(ConnectorName, shopBody(uninstallShop))); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if h.admin.countOp("ShopifyReachable") != 0 {
		t.Error("a store with no grant had its grant asked about")
	}
	if len(h.engine.callsTo("markStoreUninstalled")) != 1 {
		t.Error("the disconnect was not re-asserted")
	}
}

// An uninstalled store ingests nothing, whatever an operator's status says --
// its grant is gone, and a Resume must not arm it.
func TestAnUninstalledStoreDoesNotIngest(t *testing.T) {
	for _, status := range []string{StatusConfigured, StatusBackfilling, StatusLive} {
		if (Store{Status: status, UninstalledAt: "2026-10-01T12:00:00Z"}).Ingests() {
			t.Errorf("an uninstalled %s store ingests", status)
		}
		if !(Store{Status: status}).Ingests() {
			t.Errorf("a connected %s store does not ingest", status)
		}
	}
}
