package shopify

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// coverage_test.go -- what a store's own connection asks for, and what of
// that its grant reaches.
//
// A store connected through the shared app asks for the commerce set and
// nothing more. A domain outside that set was never requested: it is not an
// error and not something an operator can grant, so it is never reported as
// either.

// commerceSet is the managed connection's scope list, in the order the
// project fixes it.
var commerceSet = []string{
	"unauthenticated_read_product_listings",
	"unauthenticated_read_checkouts",
	"unauthenticated_write_checkouts",
	"unauthenticated_read_customers",
	"write_products",
	"read_inventory",
	"read_locations",
	"read_orders",
	"read_customers",
	"write_customers",
	"read_discounts",
	"read_returns",
}

// oldManagedList is what the managed connection asked for before the
// commerce set.
var oldManagedList = []string{
	"unauthenticated_read_product_listings",
	"unauthenticated_read_checkouts",
	"unauthenticated_write_checkouts",
	"unauthenticated_read_customers",
	"write_products",
	"read_inventory",
	"read_locations",
	"read_orders",
}

func managedStore(granted ...string) Store {
	return Store{
		ID:            testStoreID,
		AdminTokenRef: storeSecretName(testStoreID, "OFFLINE_GRANT"),
		Status:        StatusLive,
		ScopesGranted: granted,
	}
}

func withManagedScopes(t *testing.T, h *testHarness, scopes ...string) {
	t.Helper()
	h.engine.setRows("stores", []map[string]any{{
		"id":                 testStoreID,
		"domain":             "acme-widgets.myshopify.com",
		"name":               "Acme Widgets",
		"adminTokenRef":      storeSecretName(testStoreID, "OFFLINE_GRANT"),
		"webhookSecretRef":   "ACME_WEBHOOK",
		"protectedDataLevel": ProtectedLevel1,
		"status":             StatusLive,
		"ownerUserId":        "user-1",
		"scopesGranted":      toAny(scopes),
	}})
}

func TestTheManagedConnectionAsksForTheCommerceSet(t *testing.T) {
	if got := managedConnectScopes(); !slices.Equal(got, commerceSet) {
		t.Errorf("managedConnectScopes() = %v\nwant %v", got, commerceSet)
	}
	if slices.Contains(managedConnectScopes(), "read_all_orders") {
		t.Error("read_all_orders is not requested")
	}
	if !managedStore().Managed() {
		t.Error("a store whose admin token is an offline grant is managed")
	}
	if (Store{AdminTokenRef: "ACME_ADMIN"}).Managed() {
		t.Error("a store with its own admin token is not managed")
	}
	if got := connectionScopes(managedStore()); !slices.Equal(got, commerceSet) {
		t.Errorf("connectionScopes(managed) = %v", got)
	}
	if got := connectionScopes(Store{AdminTokenRef: "ACME_ADMIN"}); !slices.Equal(got, ConnectScopes()) {
		t.Errorf("connectionScopes(own app) = %v, want ConnectScopes()", got)
	}
}

func TestAStoreGrantedTheOldListLacksExactlyTheNewScopes(t *testing.T) {
	got := scopesNotApproved(managedStore(oldManagedList...))
	want := []string{"read_customers", "read_discounts", "read_returns", "write_customers"}
	if !slices.Equal(got, want) {
		t.Errorf("scopesNotApproved = %v, want %v", got, want)
	}
	if got := scopesNotApproved(managedStore()); got != nil {
		t.Errorf("a store with no recorded grant reports %v missing; nothing is known", got)
	}
}

func TestAWriteScopeCountsAsItsRead(t *testing.T) {
	granted := []string{}
	for _, s := range commerceSet {
		if s != "read_customers" {
			granted = append(granted, s)
		}
	}
	if got := scopesNotApproved(managedStore(granted...)); len(got) != 0 {
		t.Errorf("granted write_customers, still missing %v; a write scope includes its read", got)
	}
}

func TestADomainOutsideTheConnectionIsNeverInScopesMissing(t *testing.T) {
	h := newHarness(t)
	withManagedScopes(t, h, oldManagedList...)

	nodes, err := NewIntegration(h.conn).handleStoreHealth(context.Background(), map[string]any{"storeId": testStoreID}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Stores []struct {
			ScopesNeeded      []string `json:"scopesNeeded"`
			ScopesMissing     []string `json:"scopesMissing"`
			DomainsNotGranted []string `json:"domainsNotGranted"`
			DomainsOutside    []string `json:"domainsOutside"`
		} `json:"stores"`
	}
	if len(nodes) != 1 || json.Unmarshal(nodes[0].Payload, &reply) != nil || len(reply.Stores) != 1 {
		t.Fatalf("storeHealth replied %d nodes: %v", len(nodes), nodes)
	}
	got := reply.Stores[0]
	if !slices.Equal(got.ScopesNeeded, commerceSet) {
		t.Errorf("scopesNeeded = %v; it is what this store's connection asks for", got.ScopesNeeded)
	}
	if want := []string{"read_customers", "read_discounts", "read_returns", "write_customers"}; !slices.Equal(got.ScopesMissing, want) {
		t.Errorf("scopesMissing = %v, want %v", got.ScopesMissing, want)
	}
	if !slices.Contains(got.DomainsOutside, "v1:shopify:blog") {
		t.Errorf("domainsOutside = %v; blogs read under read_content, which the connection never asks for", got.DomainsOutside)
	}
	if slices.Contains(got.DomainsOutside, "v1:shopify:order") || slices.Contains(got.DomainsOutside, "v1:shopify:customer") {
		t.Errorf("domainsOutside = %v names a domain the connection asks for", got.DomainsOutside)
	}
	if slices.Contains(got.DomainsNotGranted, "v1:shopify:blog") {
		t.Errorf("domainsNotGranted = %v names a domain outside the connection", got.DomainsNotGranted)
	}
	if !slices.Contains(got.DomainsNotGranted, "v1:shopify:customer") {
		t.Errorf("domainsNotGranted = %v; customers are asked for and not granted", got.DomainsNotGranted)
	}
	for _, concept := range got.DomainsOutside {
		spec := generated.Types[generated.ConceptFromID(concept)]
		if spec == nil || spec.Reconcile == generated.ReconcileNone {
			t.Errorf("domainsOutside names %s, which is not swept on its own", concept)
		}
	}

	if phase := phaseOf(map[string]any{"lastError": memqlsync.NotRequested(ConnectorName).Error()}); phase != "not requested" {
		t.Errorf("phase = %q for a domain outside the connection", phase)
	}
}

func TestRequiredScopeOfReadsShopifysSentence(t *testing.T) {
	if got := requiredScopeOf("Access denied for inventoryShipments field. Required access: `read_inventory_shipments` access scope."); got != "read_inventory_shipments" {
		t.Errorf("requiredScopeOf = %q", got)
	}
	if got := requiredScopeOf("Access denied for companies field."); got != "" {
		t.Errorf("requiredScopeOf = %q for a message naming no scope", got)
	}
	if !protectedDataRefusal("This app is not approved to access the Order object. See https://shopify.dev/docs/apps/launch/protected-customer-data for more details.") {
		t.Error("the protected-data refusal was not recognised")
	}
	if protectedDataRefusal("Access denied for blogs field. Required access: `read_content` access scope.") {
		t.Error("a scope refusal read as protected data")
	}
}
