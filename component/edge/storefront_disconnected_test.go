package edge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// storefront_disconnected_test.go -- a store Shopify no longer authorizes the
// app for is served as unavailable (memql#5638, G1).
//
// After app/uninstalled the store row keeps its domain and its Storefront
// token reference: the mirror waits for shop/redact, and a reinstall re-checks
// the token before minting another. The edge read only those two, so it kept
// answering "connected" and handing every visitor a token Shopify refuses --
// shoppers met the refusal, and nothing said the store was gone.

func TestADisconnectedStoreIsServedAsUnavailableWithNoToken(t *testing.T) {
	var lookups []string
	resolve, _ := storefrontSecrets(t)
	counting := func(ctx context.Context, name string) (string, error) {
		lookups = append(lookups, name)
		return resolve(ctx, name)
	}
	site := storefrontSite()
	site.Store.Disconnected = true

	doc := runtimeConfigForSite(context.Background(), site, fakeEnv(nil), true, counting)
	if doc.Storefront == nil {
		t.Fatal("a disconnected storefront lost its storefront block; it must still say unavailable")
	}
	if doc.Storefront.ConnectionState != "unavailable" {
		t.Errorf("connectionState = %q, want unavailable -- the store's grant is gone", doc.Storefront.ConnectionState)
	}
	if doc.Storefront.StorefrontToken != "" {
		t.Errorf("a disconnected store's Storefront token was published: %q", doc.Storefront.StorefrontToken)
	}
	if len(lookups) != 0 {
		t.Errorf("the edge resolved %v for a disconnected store; it has nothing to publish", lookups)
	}
	raw, _ := json.Marshal(doc)
	if strings.Contains(string(raw), "shpat_") {
		t.Errorf("a token reached the served document: %s", raw)
	}

	// The reachable positive over the same site: connected once it is not.
	site.Store.Disconnected = false
	if got := runtimeConfigForSite(context.Background(), site, fakeEnv(nil), true, resolve); got.Storefront == nil || got.Storefront.ConnectionState != "connected" {
		t.Fatalf("the same store, connected: %+v", got.Storefront)
	}
}

// StoreByID reads the two markers for ONE bit. Either an uninstall or a purge
// disconnects the store; neither timestamp reaches the Site the edge caches.
func TestEngineExecutorStoreByIDReadsDisconnectedFromEitherMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  map[string]any
		want bool
	}{
		{"connected", map[string]any{}, false},
		{"uninstalled", map[string]any{"uninstalledAt": "2026-10-01T12:00:00Z"}, true},
		{"purged", map[string]any{"redactedAt": "2026-10-03T12:00:00Z"}, true},
		{"cleared by a reinstall", map[string]any{"uninstalledAt": "", "redactedAt": ""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := map[string]any{"id": "v1:shopify:store:abc123", "domain": "acme.myshopify.com", "storefrontTokenRef": "SHOPIFY_ABC123_STOREFRONT_TOKEN"}
			for k, v := range tc.row {
				row[k] = v
			}
			got, err := NewEngineExecutor(&fakeEngine{rows: []map[string]any{row}}).StoreByID(context.Background(), "abc123")
			if err != nil || got == nil {
				t.Fatalf("StoreByID = %+v, %v", got, err)
			}
			if got.Disconnected != tc.want {
				t.Errorf("Disconnected = %v, want %v", got.Disconnected, tc.want)
			}
		})
	}
}
