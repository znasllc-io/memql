//go:build bff

package app

import (
	"testing"

	"github.com/znasllc-io/memql/component/server"
)

// The projection of a site row onto what the shopper handler decides on.
//
// The handler's tests (component/server) fake the reader, so nothing there can
// notice a field this projection stopped filling. For four of the five the
// result is a refusal somebody sees. For the account it is silent: a site with
// no account states none, and every submission through the storefront is then
// recorded in whichever account the site's OWNER resolves to (design record
// 2026-11-12 account row ownership, section C).
func TestAShopperSiteIsProjectedFromItsRow(t *testing.T) {
	row := func(account any) map[string]any {
		r := map[string]any{
			"id":             "v1:platform:site:site1",
			"ownerUserId":    "user-merchant",
			"shopperForms":   true,
			"binding":        map[string]any{"storeId": " store-live "},
			"previewBinding": map[string]any{"storeId": "store-dev"},
			// What the row holds and the handler must never see.
			"bundleRef": "blob://sites/site1/v1/",
			"title":     "Boots",
			"settings":  map[string]any{"theme": "dark"},
		}
		if account != nil {
			r["accountId"] = account
		}
		return r
	}

	for name, tc := range map[string]struct {
		account any
		want    string
	}{
		"bare, as a read answers it":          {"acct-merchant", "acct-merchant"},
		"canonical, as the row stores it":     {"v1:accounts:account:acct-merchant", "acct-merchant"},
		"the primary account":                 {"self", "self"},
		"surrounded by space":                 {"  acct-merchant ", "acct-merchant"},
		"a site with no account":              {nil, ""},
		"an empty account":                    {"", ""},
		"an account that is not a string":     {[]any{"acct-merchant"}, ""},
		"an account that is not a string too": {42, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := shopperSiteFromRow("site1", row(tc.account))
			want := server.ShopperSite{
				ID: "site1", ShopperForms: true, StoreID: "store-live", PreviewStoreID: "store-dev", AccountID: tc.want,
			}
			if got == nil || *got != want {
				t.Fatalf("shopperSiteFromRow = %+v, want %+v", got, want)
			}
		})
	}

	// A row written before any of these fields existed resolves with no surface,
	// no store and no account, rather than failing.
	if got := shopperSiteFromRow("site1", map[string]any{}); got == nil || *got != (server.ShopperSite{ID: "site1"}) {
		t.Fatalf("an empty row projected to %+v", got)
	}
}
