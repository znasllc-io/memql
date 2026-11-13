package shopify

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// The background job deliberately runs after the callback returns, while the
// recording engine still answers the prior row. Registration must use the
// grant just persisted, without relying on a registry refresh or another pass.
func TestConnectWebhookReconciliationUsesTheFreshGrant(t *testing.T) {
	for _, tc := range []struct {
		name      string
		first     bool
		prior     []string
		granted   []string
		customers bool
	}{
		{"expanded", false, []string{"read_products"}, []string{"read_products", "write_customers"}, true},
		{"unchanged", false, []string{"read_products", "write_customers"}, []string{"read_products", "write_customers"}, true},
		{"narrowed", false, []string{"read_products", "write_customers"}, []string{"read_products"}, false},
		{"first connection", true, nil, []string{"read_products"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWriteHarness(t)
			c := h.integ.connector
			if !tc.first {
				h.secrets["SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN"] = connectToken
				h.store(map[string]any{
					"adminTokenRef":      storeSecretName(connectStoreID, suffixAdminToken),
					"storefrontTokenRef": "SHOPIFY_ACME-WIDGETS_STOREFRONT_TOKEN",
					"scopesGranted":      toAny(append(slices.Clone(StorefrontScopes), tc.prior...)), "ownerUserId": "original-owner",
					"appClientId": connectClientID, "webhookSecretRef": "existing-webhook-secret",
					"name": "Existing shop", "status": StatusPaused, "plan": "Plus",
					"protectedDataLevel": ProtectedLevel1, "isDevelopment": true,
					"developmentOfStoreId": "live-shop", "connectedAt": "2026-10-01T12:00:00Z",
					"health": map[string]any{"existing": "preserved"},
				})
			}
			prior, _, err := c.storeByID(context.Background(), connectStoreID)
			if err != nil {
				t.Fatal(err)
			}
			var observed Store
			c.deliver = func(store Store) string {
				observed = store
				return testCallback
			}
			var jobs []func()
			c.background = func(job func()) { jobs = append(jobs, job) }
			grant := h.grant()
			grant.Scopes = append(slices.Clone(StorefrontScopes), tc.granted...)
			result, kept := c.WriteShopifyConnect(context.Background(), callbackState(credentialSourceCurrent), grant)
			wantResult := connectResultReconnected
			if tc.first {
				wantResult = connectResultConnected
			}
			if result != wantResult || kept != wantResult || len(jobs) != 1 {
				t.Fatalf("callback result %q, kept %q, jobs %d", result, kept, len(jobs))
			}
			if h.admin.countOp("ShopifyWebhookSubscriptions") != 0 {
				t.Fatal("registration held the callback redirect")
			}
			jobs[0]()
			if !slices.Equal(observed.ScopesGranted, grant.Scopes) {
				t.Errorf("registration used scopes %v; newly recorded grant is %v", observed.ScopesGranted, grant.Scopes)
			}
			// Only the callback's credential/version fields, disconnection state
			// and fresh scopes change in this handoff. In particular a paused
			// store, its owner, health and protected-data decision stay intact.
			want := prior
			want.ID, want.Domain = connectStoreID, connectShop
			want.AdminTokenRef = storeSecretName(connectStoreID, suffixAdminToken)
			want.StorefrontTokenRef = storeSecretName(connectStoreID, suffixStorefrontToken)
			want.APIVersion, want.ScopesGranted = generated.APIVersion, grant.Scopes
			want.UninstalledAt, want.RedactedAt = "", ""
			if !reflect.DeepEqual(observed, want) {
				t.Errorf("registration changed unrelated store fields: got %+v, want %+v", observed, want)
			}
			topics := map[string]bool{}
			for _, request := range h.admin.seen() {
				if request.Operation == "ShopifyWebhookCreate" {
					topics[request.Variables["topic"].(string)] = true
				}
			}
			if topics["CUSTOMERS_UPDATE"] != tc.customers || !topics["PRODUCTS_UPDATE"] || topics["ORDERS_UPDATED"] {
				t.Errorf("callback registered topics outside the fresh grant: customers=%t products=%t orders=%t", topics["CUSTOMERS_UPDATE"], topics["PRODUCTS_UPDATE"], topics["ORDERS_UPDATED"])
			}
			h.assertNoCredential(t)
		})
	}
}
