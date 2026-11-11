package wholesalepack_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/packs/wholesalepack"
)

// The applications switch is data a client's people flip (wholesaleSetSettings), stamped with whoever wrote it, while
// the shopper surface reads it as the BORROWED site owner. It was invisible unless the site owner had written it, so
// applications opened by a developer or by a second owner refused every applicant (/wholesale/problem?reason=failed).
// The switch is now read as the platform for the stamped store; a switch somebody else CLOSED still refuses.
func TestTheApplicationsSwitchHoldsWhoeverFlippedIt(t *testing.T) {
	eng := liveWholesaleEngine(t)
	ranked := func(user string, role auth.Role) context.Context {
		return auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: role}), &auth.TokenInfo{Subject: user})
	}
	merchant, _, _ := wholesaleScope()
	shopper := asSiteOwner("v1:identity:user:" + merchant) // the canonical spelling the edge stamps
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	apply := func(store string) error {
		_, err := eng.Execute(shopper, `builtin wholesaleSubmitApplication(applicantEmail: "sam@acme.example", applicantName: "Sam Rivers"`+
			`, companyName: "Acme", siteId: "site-wholesale-e2e", storeId: `+wsQuote(store)+`, submissionId: `+wsQuote("sub-"+store)+`)`)
		return err
	}
	for name, opener := range map[string]context.Context{
		"developer":    ranked("v1:identity:user:dev-"+n, auth.RoleDeveloper),
		"second-owner": ranked("v1:identity:user:owner2-"+n, auth.RoleOwner),
	} {
		open, closed := "store-open-"+name+"-"+n, "store-closed-"+name+"-"+n
		openApplications(t, eng, opener, open, wholesalepack.AdapterCustomerTag)
		if err := apply(open); err != nil {
			t.Errorf("applications opened by a %s refuse the shopper: %v", name, err)
		}
		wsExec(t, eng, opener, `builtin wholesaleSetSettings(applicationsOpen: false, entitlementAdapter: `+
			wsQuote(wholesalepack.AdapterCustomerTag)+`, storeId: `+wsQuote(closed)+`)`)
		if err := apply(closed); err == nil {
			t.Errorf("applications a %s CLOSED took an application", name)
		}
	}
}
