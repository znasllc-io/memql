package memql

import (
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// THE WHOLESALE PART REACHES THE SHARED ORGANIZATION GATE. Deciding a store's
// wholesale applications is a Deployables part: a person granted `execute
// app:deployables/wholesale` in one organization must not act on another's
// applications with it. The gate judges a builtin against the organization of
// its target row, so each gated builtin of the wholesale pack names one: an
// application (whose accountId was copied off its site when it was written) or
// the site. The pack's own gate_test.go pins the DSL side -- the part and the
// argument on each builtin -- since the pack's tree is not loadable from here.
// Names a later task declares are in the table already; a case for a builtin no
// tree declares matches nothing.
func TestTheWholesaleBuiltinsNameTheirOrganizationTarget(t *testing.T) {
	if !organizationDeployableCapability("app:deployables/wholesale") {
		t.Fatal("app:deployables/wholesale is not an organization deployable capability, so every wholesale act is judged against the caller's global capability alone")
	}
	application, site := [2]string{"v1:wholesale:application", "applicationId"}, [2]string{"v1:platform:site", "siteId"}
	for name, want := range map[string][2]string{
		"wholesaleRecordDecision":  application,
		"wholesaleGet":             application,
		"wholesaleRetryAccess":     application,
		"wholesaleRemoveBuyer":     application,
		"wholesaleResendEmail":     application,
		"wholesaleList":            site,
		"wholesaleDecisionLog":     site,
		"wholesaleBuyers":          site,
		"wholesaleStoreSettings":   site,
		"wholesaleUpdateSettings":  site,
		"wholesaleDocuments":       site,
		"wholesalePublishDocument": site,
		"wholesaleAddBuyer":        site,
		"wholesaleExportStore":     site,
	} {
		fn := &Function{Name: name, FunctionKind: "builtin",
			RequiresCapability: CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/wholesale"}}
		concept, argument := organizationCapabilityTarget(fn)
		if concept != want[0] || argument != want[1] {
			t.Errorf("%s resolves its organization target to (%q, %q), want (%q, %q)", name, concept, argument, want[0], want[1])
		}
	}
}
