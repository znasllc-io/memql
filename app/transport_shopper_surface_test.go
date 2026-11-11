//go:build bff

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// transport_shopper_surface_test.go -- the site the shopper handler decides on,
// read off a real site row through a real engine.
//
// Postgres-gated like every *_db_test.go in this package, through
// workTemplateDBEngineAndDB.

func shopperSurfaceSeedContext() context.Context {
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "shopper-surface-seeder", Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	}))
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "shopper-surface-seeder"})
}

func shopperSurfaceInsert(t *testing.T, engine *memql.MemQLEngine, concept, rowID string, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	q := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, langparser.QuoteString(concept), langparser.QuoteString(rowID), string(raw))
	if _, err := engine.Execute(shopperSurfaceSeedContext(), q); err != nil {
		t.Fatalf("seed %s: %v", concept, err)
	}
}

// THE SITE CARRIES ITS ORGANIZATION, so the handler can stamp it into the rows a
// shopper writes; a site with none carries none, which is a legitimate state.
func TestTheShopperSiteCarriesItsAccount(t *testing.T) {
	engine := workTemplateDBEngine(t)
	s := id.NewShortId()
	owner, account, store := "shopper-owner-"+s, "shopper-acct-"+s, "shopper-store-"+s
	shopperSurfaceInsert(t, engine, "v1:accounts:account", account,
		map[string]any{"name": account, "status": "active", "domainStatus": "unverified"})
	shopperSurfaceInsert(t, engine, "v1:shopify:store", store, map[string]any{"domain": store + ".myshopify.com"})
	// A site is read under its owner's own actor: an identified person, and,
	// for an organization's site, one of its people.
	shopperSurfaceInsert(t, engine, "v1:identity:user", owner, map[string]any{"displayName": owner,
		"primaryEmail": owner + "@example.test", "role": "writer", "active": true})
	group := "acct-" + account
	for _, call := range []string{
		`mutation writeGroup(groupId: ` + langparser.QuoteString(group) + `, name: ` + langparser.QuoteString(group) +
			`, kind: "account", accountId: ` + langparser.QuoteString(account) + `, status: "active")`,
		`mutation writeGroupMembership(membershipId: ` + langparser.QuoteString(group+"-"+owner) + `, groupId: ` +
			langparser.QuoteString(group) + `, userId: ` + langparser.QuoteString(owner) +
			`, origin: "added", status: "active", accountId: ` + langparser.QuoteString(account) + `)`,
	} {
		if _, err := engine.Execute(shopperSurfaceSeedContext(), call); err != nil {
			t.Fatalf("seed the organization's people: %v", err)
		}
	}
	site := func(siteID, accountID string) {
		payload := map[string]any{
			"ownerUserId": owner, "title": siteID, "hostname": siteID + ".example.test",
			"status": "live", "kind": "shopify_storefront", "bundleRef": "blob://sites/" + siteID + "/v1/",
			"binding": map[string]any{"storeId": store}, "shopperForms": true,
		}
		if accountID != "" {
			payload["accountId"] = accountID
		}
		shopperSurfaceInsert(t, engine, "v1:platform:site", siteID, payload)
	}
	tied, untied := "shopper-site-tied-"+s, "shopper-site-untied-"+s
	site(tied, account)
	site(untied, "")

	reader := &shopperSiteReader{engine: engine}
	got, err := reader.ShopperSite(context.Background(), tied, owner)
	if err != nil || got == nil {
		t.Fatalf("the owner did not read their own site: %v, %v", got, err)
	}
	// THE ORGANIZATION AS THE SITE ROW CARRIES IT: the engine canonicalizes a
	// reference, so the bare id is what is compared.
	if strings.TrimPrefix(got.AccountID, "v1:accounts:account:") != account {
		t.Fatalf("AccountID = %q, want the site's organization %q", got.AccountID, account)
	}
	if got.StoreID != store || !got.ShopperForms {
		t.Fatalf("the site lost its other fields: %+v", got)
	}

	none, err := reader.ShopperSite(context.Background(), untied, owner)
	if err != nil || none == nil {
		t.Fatalf("the owner did not read their untied site: %v, %v", none, err)
	}
	if none.AccountID != "" {
		t.Fatalf("a site with no organization carries %q", none.AccountID)
	}
}
