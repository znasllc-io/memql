package memql

import (
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// THE CALLS PART REACHES THE SHARED ORGANIZATION GATE, for the reason the access
// codes part does (site_access_capability_test.go): `execute app:deployables/calls`
// is held per organization, and the gate can only judge the organization a call
// selected when organizationCapabilityTarget names the row the call is about. A
// builtin in no row of that table is judged against the caller's GLOBAL capability
// alone, which looks identical on the page and lets a part held in one organization
// act on another's deployable.
//
// So the targets are pinned against the SHIPPED tree: the three acts that name a
// deployable (a test key, the site switch, the site's limits) and the site write the
// switch makes are asked about the SITE. The media servers name no deployable: they
// are the cluster's, so their builtin is untargeted and its handler admits a cluster
// owner and nobody else (integrations/calls operator.go; the behaviour is
// operator_db_test.go's).
func TestCallsActsNameTheirOrganizationTarget(t *testing.T) {
	fns := loadedSiteAccessTree(t)

	if !organizationDeployableCapability("app:deployables/calls") {
		t.Fatal("app:deployables/calls is not an organization deployable capability, so every act under it is judged against the caller's global capability alone")
	}
	for _, tc := range []struct {
		name, kind, executor, concept, argument string
	}{
		{"callsTestKeyCreate", "builtin", "integration.calls.testKeyCreate", "v1:platform:site", "siteId"},
		{"callsSiteEnable", "builtin", "integration.calls.siteEnable", "v1:platform:site", "siteId"},
		{"updateSiteCalls", "mutation", "", "v1:platform:site", "siteId"},
		{"callsPolicySet", "builtin", "integration.calls.policySet", "v1:platform:site", "siteId"},
		{"callsMediaServers", "builtin", "integration.calls.mediaServers", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn, err := fns.Get(tc.name)
			if err != nil || fn == nil {
				t.Fatalf("%s is not in the loaded tree: %v", tc.name, err)
			}
			if fn.FunctionKind != tc.kind {
				t.Fatalf("%s is a %s, want a %s", tc.name, fn.FunctionKind, tc.kind)
			}
			if tc.executor != "" && fn.Executor != tc.executor {
				t.Errorf("%s runs %q, want %q", tc.name, fn.Executor, tc.executor)
			}
			if fn.RequiresCapability != (CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/calls"}) {
				t.Errorf("%s declares %v, want execute on app:deployables/calls", tc.name, fn.RequiresCapability)
			}
			if fn.ServerOnly {
				t.Errorf("%s is @serverOnly, so no person could make the act it exists for", tc.name)
			}
			concept, argument := organizationCapabilityTarget(fn)
			if concept != tc.concept || argument != tc.argument {
				t.Errorf("%s resolves its organization target to (%q, %q), want (%q, %q)", tc.name, concept, argument, tc.concept, tc.argument)
			}
		})
	}
}
