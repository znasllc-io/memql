package memql

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// THE SALES PART REACHES THE SHARED ORGANIZATION GATE. A storefront's sales
// figures are a Deployables part: a person granted `execute
// app:deployables/sales` in one organization must not read another's sales
// with it. The gate judges a builtin against the organization of its target
// row, so the statistics builtin names the site it reads. The builtin itself is
// declared with the statistics page's reads; this pins the Go side of the gate.
func TestTheSalesBuiltinNamesItsOrganizationTarget(t *testing.T) {
	if !organizationDeployableCapability("app:deployables/sales") {
		t.Fatal("app:deployables/sales is not an organization deployable capability, so the read is judged against the caller's global capability alone")
	}
	fn := &Function{Name: "storeStatistics", FunctionKind: "builtin",
		RequiresCapability: CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/sales"}}
	if concept, argument := organizationCapabilityTarget(fn); concept != "v1:platform:site" || argument != "siteId" {
		t.Errorf("storeStatistics resolves its organization target to (%q, %q)", concept, argument)
	}

	// The other side: "no organization target" is the answer ("", ""). The
	// name alone does not bind a function to the site, and neither does the
	// part alone on a function that is not a builtin.
	for name, other := range map[string]*Function{
		"storeStatistics under another resource": {Name: "storeStatistics", FunctionKind: "builtin",
			RequiresCapability: CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/health"}},
		"storeStatistics as a query": {Name: "storeStatistics", FunctionKind: "query",
			RequiresCapability: CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/sales"}},
		"storeStatistics as a mutation": {Name: "storeStatistics", FunctionKind: "mutation",
			RequiresCapability: CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/sales"}},
		"storeStatistics under another verb": {Name: "storeStatistics", FunctionKind: "builtin",
			RequiresCapability: CapabilityRequirement{Verb: auth.VerbRead, Resource: "app:deployables/sales"}},
		"another builtin under the sales part": {Name: "someOtherBuiltin", FunctionKind: "builtin",
			RequiresCapability: CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/sales"}},
	} {
		if concept, argument := organizationCapabilityTarget(other); concept != "" || argument != "" {
			t.Errorf("%s resolves an organization target (%q, %q), want none", name, concept, argument)
		}
	}
}

// OWNER AND DEVELOPER HOLD THE SALES PART AND NOBODY ELSE. Revenue is more
// sensitive than visits, so the part is its own and the seeds give it to the
// two roles that run the cluster. The answer is read from the SEEDED catalog
// (the real loader), not from a table this test builds, and the compiled
// mirror in component/auth must say the same before the rows are readable.
// The part is also separate from `read app:deployables`: holding the app is
// not holding the part.
func TestOwnerAndDeveloperHoldTheSalesPartAndNobodyElse(t *testing.T) {
	reg := NewSeedRegistry()
	if _, err := LoadUnifiedSeeds(slog.New(slog.NewTextHandler(io.Discard, nil)), reg); err != nil {
		t.Fatalf("LoadUnifiedSeeds: %v", err)
	}
	grants := collectCapabilityGrants(t, reg)
	const part = "app:deployables/sales"

	// The seeded base roles: the member tier is `user` and the viewer tier
	// `viewer` in the catalog; `writer` and `reader` are their user-row spellings.
	for role, want := range map[string]bool{"owner": true, "developer": true, "admin": false, "user": false, "viewer": false} {
		if got := grants[capKey{role, "execute", part}]; got != want {
			t.Errorf("seeded role %q holds execute %s = %v, want %v", role, part, got, want)
		}
	}
	// Nothing but execute on the part is granted to anyone, and every execute
	// grant on it belongs to owner or developer: a role nobody named here
	// cannot be granted it without this failing.
	for key := range grants {
		if key.resource != part {
			continue
		}
		if key.verb != "execute" {
			t.Errorf("seeded role %q holds %s on %s; only execute is granted", key.role, key.verb, part)
		}
		if key.role != "owner" && key.role != "developer" {
			t.Errorf("seeded role %q holds %s on %s; only owner and developer may", key.role, key.verb, part)
		}
	}
	// The same over the catalog the seeds build, for EVERY role it holds.
	catalog, _ := seededCatalog(t)
	if len(catalog.Slugs()) == 0 {
		t.Fatal("the seeded catalog holds no roles")
	}
	for _, slug := range catalog.Slugs() {
		if want := slug == "owner" || slug == "developer"; catalog.Holds(slug, "execute", part) != want {
			t.Errorf("seeded catalog: role %q holds execute %s = %v, want %v", slug, part, !want, want)
		}
	}

	// The capability check the engine calls (the compiled mirror answers while
	// no catalog is installed, and the mirror is pinned to these seeds by
	// component/auth's TestSeedMatchesCompiledMirror). No grant source is
	// installed, so only the role answers.
	previousCatalog, previousSource := auth.InstalledCapabilityCatalog(), auth.InstalledGrantSource()
	auth.SetCapabilityCatalog(nil)
	auth.SetGrantSource(nil)
	t.Cleanup(func() {
		auth.SetCapabilityCatalog(previousCatalog)
		auth.SetGrantSource(previousSource)
	})
	ctx := context.Background()
	for _, role := range auth.ValidRoles() {
		want := role == auth.RoleOwner || role == auth.RoleDeveloper
		subject := auth.Subject{Role: role}
		if got := auth.CapableFor(ctx, subject, auth.VerbExecute, part); got != want {
			t.Errorf("role %q: CapableFor execute %s = %v, want %v", role, part, got, want)
		}
		// Holding the Deployables app is not holding the part: every role
		// reads the app, and only the two above hold the part.
		if !auth.CapableFor(ctx, subject, auth.VerbRead, "app:deployables") {
			t.Errorf("role %q does not read app:deployables; the test no longer shows the part is separate from the app", role)
		}
	}
}
