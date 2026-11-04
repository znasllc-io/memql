package reviewspack_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// seedCtx is the engine's own seeding identity, as its account tests use it:
// internal origin and a synthetic owner, so a fixture row is written without
// standing in for anyone.
func seedCtx() context.Context {
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "reviews-e2e-seeder", Role: auth.RoleOwner, Unranked: true, Synthetic: true,
	}))
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: "reviews-e2e-seeder"})
}

// seedSite writes a storefront site the merchant owns, bound to the store: the
// site every store-wide reviews act names, and the organization gate judges.
func seedSite(t *testing.T, eng *memql.MemQLEngine, siteID, owner, storeID string) {
	t.Helper()
	// The store first: a storefront may be bound only to a store row that
	// exists and its writer can read.
	store, _ := json.Marshal(map[string]any{"domain": storeID + ".myshopify.com"})
	if _, err := eng.Execute(seedCtx(), fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString("v1:shopify:store"), langparser.QuoteString(storeID), string(store))); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"ownerUserId": owner, "title": siteID, "hostname": siteID + ".example.test",
		"status": "live", "kind": "shopify_storefront", "bundleRef": "blob://sites/" + siteID + "/v1/",
		"binding": map[string]any{"storeId": storeID},
	})
	q := fmt.Sprintf(`insert(%s, id=%s, payload=%s)`, langparser.QuoteString("v1:platform:site"),
		langparser.QuoteString(siteID), string(payload))
	if _, err := eng.Execute(seedCtx(), q); err != nil {
		t.Fatalf("seed site: %v", err)
	}
}

// grantReviewsPart gives one person the reviews part, the way a store's
// moderator is given it: a grant on top of a role that does not hold it.
func grantReviewsPart(t *testing.T, eng *memql.MemQLEngine, userID string) {
	t.Helper()
	call := `writeGrant(grantId: ` + quote("grant-reviews-"+userID) + `, subjectKind: "user", subjectId: ` + quote(userID) +
		`, verb: "execute", resourceType: "app:deployables/reviews", effect: "allow", grantedBy: "reviews-e2e-seeder")`
	if _, err := eng.Execute(seedCtx(), call); err != nil {
		t.Fatalf("grant the reviews part: %v", err)
	}
}
