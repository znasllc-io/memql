package reviewspack_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// reviewsIssueRequest is "the cluster's operators only" (its description). Mirrored orders are cluster-owner tier, so it
// used to read the order as the caller and refused every operator below owner -- a developer testing a store before it
// goes live got `order ... is not readable on store ...`. An operator now reads the order as the daily ask does; somebody
// who merely holds the reviews part (a client's moderator) still reads it as themselves and is still refused.
func TestOnlyTheClustersOperatorsIssueAReviewLinkByHand(t *testing.T) {
	eng := livePackEngine(t)
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	owner, developer, moderator := "op-owner-"+n, "op-dev-"+n, "op-mod-"+n
	store, site, account := "op-store-"+n, "op-site-"+n, "op-acct-"+n
	canon := func(id string) string { return "v1:identity:user:" + id }
	seedSiteIn(t, eng, site, owner, store, account)
	seedOrganization(t, eng, account)
	grantReviewsPart(t, eng, developer)
	grantReviewsPart(t, eng, moderator)

	issue := func(ctx context.Context, line string) error {
		t.Helper()
		orderGid, handle := "gid://shopify/Order/"+line+n, "fylo-"+line+n
		money := map[string]any{"shopMoney": map[string]any{"amount": "24.00", "currencyCode": "USD"}}
		seedMirror(t, eng, "v1:shopify:order", "order-"+line+n, map[string]any{"storeId": store, "gid": orderGid, "email": "b@example.com", "name": "#1",
			"displayFinancialStatus": "PAID", "totalPriceSet": money, "processedAt": time.Now().UTC().Format(time.RFC3339)})
		seedMirror(t, eng, "v1:shopify:orderLineItem", "line-"+line+n, map[string]any{"storeId": store, "gid": "gid://shopify/LineItem/" + line + n,
			"parentGid": orderGid, "productGid": "gid://shopify/Product/" + line + n, "priceAfterAllDiscountsBeforeTaxesSet": money, "title": "P"})
		seedMirror(t, eng, "v1:shopify:product", "product-"+line+n, map[string]any{"storeId": store, "gid": "gid://shopify/Product/" + line + n, "handle": handle, "title": "P"})
		_, err := eng.Execute(ctx, `builtin reviewsIssueRequest(storeId: `+quote(store)+`, orderGid: `+quote(orderGid)+`, productHandle: `+quote(handle)+`, siteId: `+quote(site)+`)`)
		return err
	}
	if err := issue(asPerson(canon(developer), auth.RoleDeveloper), "dev"); err != nil {
		t.Fatalf("a developer (one of the cluster's operators) issuing a review link by hand: %v", err)
	}
	if err := issue(asPerson(canon(moderator), auth.RoleWriter), "mod"); err == nil {
		t.Fatal("somebody who only holds the reviews part (not an operator) issued a review link for an order they cannot read")
	}
}
