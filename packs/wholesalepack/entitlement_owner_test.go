package wholesalepack_test

import (
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/packs/wholesalepack"
)

// The approval automation that provisions an entitlement runs as a synthetic
// system actor (component/automations/executor.go), and the engine blanks an
// owner stamped from one (memql#4817). The row was then owned by nobody: the
// merchant who approved the buyer could neither list it nor revoke it. It must
// be the application owner's row.
func TestAnEntitlementTheAutomationWritesIsTheMerchants(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, storeLive, _ := wholesaleScope()
	// Canonical throughout, as in production: a signed-in merchant's id and the
	// owner the edge stamps on the shopper path are both v1:identity:user:<id>.
	ctx := asSiteOwner("v1:identity:user:" + merchant)

	openApplications(t, eng, ctx, storeLive, wholesalepack.AdapterCustomerTag)
	_ = submit(t, eng, ctx, storeLive, "Owner Supply", "owner@supply.example")
	rows := wsRows(t, eng, ctx, `query applicationsForStore(storeId: `+wsQuote(storeLive)+`)`)
	if len(rows) != 1 {
		t.Fatalf("the merchant sees %d applications, want 1", len(rows))
	}
	appID, _ := rows[0]["id"].(string)
	decide(t, eng, ctx, appID, wholesalepack.TransitionApprove, merchant, "ok")

	automation := auth.ContextWithSystemActor(ctx, "grantOnApproval")
	wsExec(t, eng, automation, `builtin wholesaleProvisionEntitlement(applicationId: `+wsQuote(appID)+`)`)

	if seen := wsRows(t, eng, ctx, `query entitlementsForStore(storeId: `+wsQuote(storeLive)+`)`); len(seen) != 1 {
		stored := wsRows(t, eng, automation, `query entitlementForApplication(applicationId: `+wsQuote(appID)+`)`)
		t.Fatalf("the merchant who approved the buyer sees %d entitlements, want 1 (stored rows: %v)", len(seen), stored)
	}
	decide(t, eng, ctx, appID, wholesalepack.TransitionRevoke, merchant, "closed")
	wsExec(t, eng, ctx, `builtin wholesaleRevokeEntitlement(applicationId: `+wsQuote(appID)+`)`)
}
