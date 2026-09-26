package edge

import (
	"context"
	"errors"
	"testing"
)

func TestStorefrontConnectionStateDoesNotMaskFailuresAsDesignPreview(t *testing.T) {
	bound := map[string]any{"storeId": "store"}
	store := &BoundStore{ID: "store", Domain: "example.myshopify.com", StorefrontTokenRef: "public-token-ref"}
	for _, tc := range []struct {
		name string
		site *Site
		fail bool
		want string
	}{
		{"unbound", &Site{Kind: storefrontKind}, false, "unbound"},
		{"bound row unavailable", &Site{Kind: storefrontKind, Binding: bound}, false, "unavailable"},
		{"credential unavailable", &Site{Kind: storefrontKind, Binding: bound, Store: store}, true, "unavailable"},
		{"connected", &Site{Kind: storefrontKind, Binding: bound, Store: store}, false, "connected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := storefrontForSite(context.Background(), tc.site, func(context.Context, string) (string, error) {
				if tc.fail {
					return "", errors.New("private secret lookup failed")
				}
				return "public-token", nil
			})
			if got.ConnectionState != tc.want {
				t.Fatalf("state = %q, want %q", got.ConnectionState, tc.want)
			}
		})
	}
	original := &Site{Kind: storefrontKind, Binding: bound, Store: store}
	preview := previewSite(original, &PreviewGrant{})
	if got := storefrontForSite(context.Background(), preview, nil); got.ConnectionState != "unbound" {
		t.Fatalf("unbound testing inherited production: %+v", got)
	}
	original.PreviewBinding = bound
	preview = previewSite(original, &PreviewGrant{})
	if got := storefrontForSite(context.Background(), preview, nil); got.ConnectionState != "unavailable" {
		t.Fatalf("failed testing lookup became design preview: %+v", got)
	}
	if original.Store != store || bindingStoreId(original.Binding) != "store" {
		t.Fatal("testing changed cached production binding")
	}
}

func TestDeploymentVersionChangesWhenAnUnresolvedStoreIsBound(t *testing.T) {
	site := &Site{Kind: storefrontKind, BundleRef: "blob://design"}
	unbound := deploymentVersion(site)
	site.Binding = map[string]any{"storeId": "unavailable-store"}
	if deploymentVersion(site) == unbound {
		t.Fatal("bound store failure did not invalidate design preview")
	}
}
