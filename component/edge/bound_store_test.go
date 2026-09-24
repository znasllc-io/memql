// component/edge/bound_store_test.go
package edge

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// THE EDGE HOLDS THREE FIELDS OF A STORE AND NO MORE (epic memql#5530, issue
// memql#5538).
//
// The store row also carries adminTokenRef and webhookSecretRef. Neither is
// projected here, and that is the point: the serving path cannot leak a
// credential reference it was never handed.
// TestRuntimeConfigNeverCarriesTheShopifyAdminToken greps the SERVED
// document; this greps the STRUCT, one rung earlier, so the admin token
// cannot reach a header or a log line either.
func TestTheBoundStoreCarriesOnlyWhatTheServingPathNeeds(t *testing.T) {
	want := []string{"ID", "Domain", "StorefrontTokenRef"}
	typ := reflect.TypeOf(BoundStore{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BoundStore fields = %v, want exactly %v. A field added here is a field the edge can serve.", got, want)
	}
}

// storefrontSiteBoundTo is a live storefront row whose binding NAMES a store
// rather than copying it -- the shape issue memql#5538 made canonical.
func storefrontSiteBoundTo(storeId string) *Site {
	return &Site{
		ID:       "s1",
		Hostname: "shop.example.com",
		Kind:     storefrontKind,
		Status:   "live",
		Binding:  map[string]any{"storeId": storeId},
	}
}

// A STORE EDIT REACHES THE SITE WITH NO SECOND WRITE -- the epic's own test
// (design section 11). The site row is untouched; only the store changed.
func TestAStoreEditReachesTheSiteWithNoSecondWrite(t *testing.T) {
	ex := &stubExec{
		rows:  map[string]*Site{"shop.example.com": storefrontSiteBoundTo("store-1")},
		store: &BoundStore{ID: "store-1", Domain: "before.myshopify.com", StorefrontTokenRef: "ref"},
	}
	r := NewResolver(ex, time.Minute)

	first, err := r.Resolve(context.Background(), "shop.example.com")
	if err != nil || first == nil || first.Store == nil {
		t.Fatalf("first resolve did not bind a store: site=%+v err=%v", first, err)
	}
	if first.Store.Domain != "before.myshopify.com" {
		t.Fatalf("first resolve read %q", first.Store.Domain)
	}

	// The STORE row changes. The site row does not.
	ex.store = &BoundStore{ID: "store-1", Domain: "after.myshopify.com", StorefrontTokenRef: "ref"}
	r.InvalidateAll()

	second, err := r.Resolve(context.Background(), "shop.example.com")
	if err != nil || second == nil || second.Store == nil {
		t.Fatalf("second resolve did not bind a store: site=%+v err=%v", second, err)
	}
	if second.Store.Domain != "after.myshopify.com" {
		t.Errorf("after a store edit the edge still reads %q; the site was never rewritten and the edit must reach it anyway", second.Store.Domain)
	}
}

// THE STORE IS RESOLVED ONCE AND CACHED WITH THE SITE. The policy is rebuilt
// on every asset response, so a per-request store read would be a database
// round trip per image.
func TestTheBoundStoreIsCachedWithTheSite(t *testing.T) {
	ex := &stubExec{
		rows:  map[string]*Site{"shop.example.com": storefrontSiteBoundTo("store-1")},
		store: &BoundStore{ID: "store-1", Domain: "acme.myshopify.com"},
	}
	r := NewResolver(ex, time.Minute)

	for i := 0; i < 5; i++ {
		if _, err := r.Resolve(context.Background(), "shop.example.com"); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	if ex.storeCalls != 1 {
		t.Errorf("StoreByID called %d times over 5 resolutions, want 1 -- the store is not cached with the site", ex.storeCalls)
	}
}

// A BINDING NAMING A STORE THAT IS GONE SERVES NO STOREFRONT BLOCK AND NAMES
// NO STORE. Drop, never guess -- the same discipline validHost already applies
// to a malformed domain.
func TestAnUnresolvableStoreLeavesTheSiteServable(t *testing.T) {
	ex := &stubExec{
		rows:  map[string]*Site{"shop.example.com": storefrontSiteBoundTo("gone")},
		store: nil,
	}
	r := NewResolver(ex, time.Minute)

	site, err := r.Resolve(context.Background(), "shop.example.com")
	if err != nil {
		t.Fatalf("an unresolvable store failed the whole resolve: %v", err)
	}
	if site == nil {
		t.Fatal("an unresolvable store made the site itself unresolvable; the bundle must still serve")
	}
	if site.Store != nil {
		t.Errorf("Store = %+v, want nil", site.Store)
	}
	if got := policyForSite(httptest.NewRequest("GET", "/", nil), site, noEnv, ""); strings.Contains(got, "myshopify") {
		t.Errorf("the policy named a store the edge could not read: %q", got)
	}
	if doc := runtimeConfigForSite(context.Background(), site, noEnv, true, nil); doc.Storefront != nil {
		t.Errorf("an unresolvable store still produced a storefront block: %+v", doc.Storefront)
	}
}

// A FAILED STORE READ IS THE SAME ANSWER, and this is the half a miss cannot
// cover: the engine erroring is not the store being absent, and treating it as
// a resolve failure would take a LIVE bundle dark because a row it merely
// references could not be read.
func TestAStoreReadFailureLeavesTheSiteServable(t *testing.T) {
	ex := &stubExec{
		rows:     map[string]*Site{"shop.example.com": storefrontSiteBoundTo("store-1")},
		store:    &BoundStore{ID: "store-1", Domain: "acme.myshopify.com"},
		storeErr: errors.New("the database is unreachable"),
	}
	r := NewResolver(ex, time.Minute)

	site, err := r.Resolve(context.Background(), "shop.example.com")
	if err != nil {
		t.Fatalf("a failing store read failed the whole resolve: %v", err)
	}
	if site == nil {
		t.Fatal("a failing store read made the site unresolvable; the bundle must still serve")
	}
	if site.Store != nil {
		t.Errorf("Store = %+v, want nil -- a store that could not be read is not a store", site.Store)
	}
}

// AN UNBOUND STOREFRONT READS NO STORE AT ALL, and neither does any other
// kind. The binding is the only thing that can start a second read, so a
// `spa` row is exactly as cheap as it was before this existed.
func TestOnlyABoundStorefrontReadsAStore(t *testing.T) {
	unbound := storefrontSiteBoundTo("")
	spa := storefrontSiteBoundTo("store-1")
	spa.Kind = "spa"

	for name, site := range map[string]*Site{"unbound storefront": unbound, "spa carrying a binding": spa} {
		t.Run(name, func(t *testing.T) {
			ex := &stubExec{
				rows:  map[string]*Site{"shop.example.com": site},
				store: &BoundStore{ID: "store-1", Domain: "acme.myshopify.com"},
			}
			r := NewResolver(ex, time.Minute)
			got, err := r.Resolve(context.Background(), "shop.example.com")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Store != nil {
				t.Errorf("Store = %+v, want nil", got.Store)
			}
			if ex.storeCalls != 0 {
				t.Errorf("StoreByID ran %d time(s); nothing here names a store to read", ex.storeCalls)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The engine seam
// ---------------------------------------------------------------------------

// THE SAME FALSE SIGNAL TestEngineExecutorRunsUnderASyntheticClusterOwnerActor
// EXISTS TO CLOSE OFF, one concept over.
//
// v1:shopify:store declares @rowAuthz(clusterOwner, rankFloor="developer"),
// which storeById's plan carries. A StoreByID
// that quietly ran under no actor would read ZERO ROWS AND NO ERROR: every
// storefront in the cluster would serve no storefront block and name no store
// in its policy, while every stub-driven test above kept passing.
func TestEngineExecutorStoreByIDRunsUnderASyntheticClusterOwnerActor(t *testing.T) {
	fe := &fakeEngine{rows: []map[string]any{
		{"id": "v1:shopify:store:abc123", "domain": "acme.myshopify.com"},
	}}
	ex := NewEngineExecutor(fe)

	if _, err := ex.StoreByID(context.Background(), "abc123"); err != nil {
		t.Fatalf("StoreByID: %v", err)
	}

	ac, ok := auth.AccessFromContext(fe.gotCtx)
	if !ok || ac == nil {
		t.Fatalf("engine.Execute ran with no AccessContext on ctx; the engine will refuse the clusterOwner-tier read")
	}
	if !ac.IsClusterOwner() {
		t.Errorf("engine.Execute ran as role %q, want a cluster owner -- v1:shopify:store's tier answers this actor with zero rows", ac.Role)
	}
}

// THE PROJECTION IS THREE FIELDS OFF A ROW THAT CARRIES MORE. The row the
// engine hands back here holds both credential references; neither may reach
// the Site the edge caches.
func TestEngineExecutorStoreByIDProjectsOnlyTheServingFields(t *testing.T) {
	fe := &fakeEngine{rows: []map[string]any{{
		"id":                 "v1:shopify:store:abc123",
		"domain":             "acme.myshopify.com",
		"storefrontTokenRef": "acme_storefront_token",
		"adminTokenRef":      "acme_admin_token",
		"webhookSecretRef":   "acme_webhook_secret",
		"isDevelopment":      true,
	}}}

	got, err := NewEngineExecutor(fe).StoreByID(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("StoreByID: %v", err)
	}
	want := &BoundStore{ID: "abc123", Domain: "acme.myshopify.com", StorefrontTokenRef: "acme_storefront_token"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StoreByID returned %+v, want %+v", got, want)
	}
}

// A miss is (nil, nil) -- a store that is gone is not a query failure, it is a
// storefront with nothing to talk to. The resolver above depends on the
// distinction to leave the site servable.
func TestEngineExecutorStoreByIDMissReturnsNilWithoutError(t *testing.T) {
	got, err := NewEngineExecutor(&fakeEngine{rows: nil}).StoreByID(context.Background(), "gone")
	if err != nil {
		t.Fatalf("StoreByID returned an error for a miss: %v", err)
	}
	if got != nil {
		t.Fatalf("StoreByID returned %+v for a miss, want nil", got)
	}
}

// An engine FAILURE is an error, not a miss -- the caller logs it rather than
// recording "this storefront has no store", which is what it would otherwise
// cache for the TTL.
func TestEngineExecutorStoreByIDSurfacesEngineError(t *testing.T) {
	if _, err := NewEngineExecutor(&fakeEngine{err: errors.New("boom")}).StoreByID(context.Background(), "abc123"); err == nil {
		t.Fatal("StoreByID swallowed an engine error")
	}
}

// The store id reaches the engine as a quoted argument, never interpolated
// raw -- the same property TestEngineExecutorSiteByHostnameQuotesTheArgument
// pins for the hostname.
func TestEngineExecutorStoreByIDQuotesTheArgument(t *testing.T) {
	fe := &fakeEngine{rows: nil}
	if _, err := NewEngineExecutor(fe).StoreByID(context.Background(), `abc".123`); err != nil {
		t.Fatalf("StoreByID: %v", err)
	}
	if !strings.Contains(fe.gotQuery, "storeById") || !strings.Contains(fe.gotQuery, `\"`) {
		t.Errorf("query %q does not look like an escaped storeById call", fe.gotQuery)
	}
}
