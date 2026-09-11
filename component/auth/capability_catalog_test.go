package auth

import "testing"

// fakeCatalog is a hand-built catalog for the read-through tests. It answers
// exactly what it is given and nothing else, which is the property under test:
// an installed catalog is AUTHORITATIVE, so a slug it does not know holds
// nothing rather than falling through to the compiled mirror.
type fakeCatalog struct {
	ranks  map[string]int
	names  map[string]string
	grants map[string]map[VerbResource]bool
	scopes map[string]string
	off    map[string]bool
}

func (f *fakeCatalog) Rank(slug string) (int, bool) { r, ok := f.ranks[slug]; return r, ok }
func (f *fakeCatalog) Name(slug string) string      { return f.names[slug] }
func (f *fakeCatalog) Holds(slug, verb, resource string) bool {
	if f.off[slug] {
		return false
	}
	return f.grants[slug][VerbResource{Verb: verb, Resource: resource}]
}

func (f *fakeCatalog) Grants(slug string) []VerbResource {
	out := make([]VerbResource, 0, len(f.grants[slug]))
	for vr := range f.grants[slug] {
		out = append(out, vr)
	}
	return out
}
func (f *fakeCatalog) Scope(slug string) string { return f.scopes[slug] }
func (f *fakeCatalog) Active(slug string) bool  { return !f.off[slug] }

// installFake installs a catalog for one test and clears it afterwards. Every
// test that installs one MUST use this: the holder is package state, and a
// catalog left behind would silently change every later test in the package.
func installFake(t *testing.T, c CapabilityCatalog) {
	t.Helper()
	SetCapabilityCatalog(c)
	t.Cleanup(func() { SetCapabilityCatalog(nil) })
}

func TestCapableReadsTheCatalogWhenInstalled(t *testing.T) {
	installFake(t, &fakeCatalog{
		ranks: map[string]int{"support-lead": 150},
		names: map[string]string{"support-lead": "Support Lead"},
		grants: map[string]map[VerbResource]bool{
			"support-lead": {{Verb: VerbRead, Resource: ResourcePrincipal}: true},
		},
	})

	if !Capable(Role("support-lead"), VerbRead, ResourcePrincipal) {
		t.Fatal("a custom role holding read-on-principal must be Capable of it -- " +
			"this is the whole point of the epic: a role is data the engine enforces")
	}
	if Capable(Role("support-lead"), VerbDelete, ResourcePrincipal) {
		t.Fatal("a custom role must hold exactly the pairs the catalog gives it and no others")
	}
	if got := RoleRank(Role("support-lead")); got != 150 {
		t.Fatalf("RoleRank(support-lead) = %d, want 150", got)
	}
}

// TestUnknownSlugHoldsNothingAndRanksZero is D2, asserted across the whole
// vocabulary rather than on one pair: fail-closed has to hold everywhere or it
// holds nowhere.
func TestUnknownSlugHoldsNothingAndRanksZero(t *testing.T) {
	installFake(t, &fakeCatalog{
		ranks:  map[string]int{"owner": 400},
		grants: map[string]map[VerbResource]bool{"owner": {{Verb: VerbCreate, Resource: ResourcePrincipal}: true}},
	})

	verbs := []string{VerbRead, VerbCreate, VerbUpdate, VerbDelete, VerbExecute}
	resources := []string{
		ResourcePrincipal, ResourceConstruct, ResourceData, ResourceDeployment,
		ResourceAdmission, ResourceAgent, ResourceGroup, ResourceRole,
	}
	for _, verb := range verbs {
		for _, res := range resources {
			if Capable(Role("ghost"), verb, res) {
				t.Fatalf("an unknown slug held %s on %s", verb, res)
			}
		}
	}
	if got := RoleRank(Role("ghost")); got != 0 {
		t.Fatalf("RoleRank(unknown) = %d, want 0 -- an unrankable role must sit below every rung", got)
	}
}

// TestAnInstalledCatalogDoesNotFallBackToTheMirror is the sharp edge of D1.
// `admin` is a base slug the compiled mirror knows well; a catalog that does
// not carry it must still answer "nothing", because a fallback here is how a
// role deleted from the catalog keeps its old permissions forever.
func TestAnInstalledCatalogDoesNotFallBackToTheMirror(t *testing.T) {
	installFake(t, &fakeCatalog{ranks: map[string]int{"owner": 400}})

	if Capable(RoleAdmin, VerbCreate, ResourcePrincipal) {
		t.Fatal("an installed catalog that does not carry `admin` must answer that admin holds " +
			"nothing. Falling back to the compiled mirror would make the rows advisory")
	}
	if got := RoleRank(RoleAdmin); got != 0 {
		t.Fatalf("RoleRank(admin) = %d against a catalog that does not carry it, want 0", got)
	}
}

func TestMirrorAnswersBeforeACatalogIsInstalled(t *testing.T) {
	SetCapabilityCatalog(nil)

	if !Capable(RoleOwner, VerbCreate, ResourcePrincipal) {
		t.Fatal("with no catalog installed the compiled mirror must answer for a base role -- " +
			"the identity node's gates run before the seed is readable")
	}
	if got := RoleRank(RoleDeveloper); got != rankDeveloper {
		t.Fatalf("RoleRank(developer) = %d with no catalog, want the compiled %d", got, rankDeveloper)
	}
	if InstalledCapabilityCatalog() != nil {
		t.Fatal("InstalledCapabilityCatalog must report nil when none is installed")
	}
}

// TestDenyIsResolvedByTheCatalog pins the contract Capable relies on: the
// catalog resolves allow-minus-deny itself and reports one boolean, so no gate
// has to remember that deny wins.
func TestDenyIsResolvedByTheCatalog(t *testing.T) {
	installFake(t, &fakeCatalog{
		ranks: map[string]int{"finance": 150},
		// execute-on-deployment is deliberately ABSENT: the catalog removed it
		// when it resolved the deny row against the allow row.
		grants: map[string]map[VerbResource]bool{
			"finance": {{Verb: VerbRead, Resource: ResourceData}: true},
		},
	})

	if Capable(Role("finance"), VerbExecute, ResourceDeployment) {
		t.Fatal("a pair the catalog does not report held must not be Capable")
	}
	if !Capable(Role("finance"), VerbRead, ResourceData) {
		t.Fatal("the pairs it does report held must be Capable")
	}
}

// TestADeactivatedRoleAnswersNothingEverywhere is the design record's
// failure-mode sentence, asserted: a role deactivated under a holder leaves the
// resolver treating that holder "as unknown: nothing, everywhere, until
// re-roled".
//
// RANK IS PART OF "EVERYWHERE", and that is the half worth a test. A rung that
// survived retirement would keep clearing every @requiresRank floor and keep
// the holder visible to their old peers under rankVisible, while Holds answered
// false for every pair -- half-retired, which is the one state this must not
// produce. The catalog still CARRIES the role (D8: deactivate, never delete),
// which is what keeps its slug and rung taken against a later create.
func TestADeactivatedRoleAnswersNothingEverywhere(t *testing.T) {
	installFake(t, &fakeCatalog{
		ranks:  map[string]int{"retired": 120},
		grants: map[string]map[VerbResource]bool{"retired": {{Verb: VerbRead, Resource: ResourceData}: true}},
		off:    map[string]bool{"retired": true},
	})

	if Capable(Role("retired"), VerbRead, ResourceData) {
		t.Fatal("a deactivated role must hold nothing (D8: deactivate, never delete)")
	}
	if got := RoleRank(Role("retired")); got != 0 {
		t.Fatalf("RoleRank(retired) = %d, want 0 -- a rung that outlives retirement clears "+
			"every rank floor while the role holds nothing, which is half-retired", got)
	}
	if IsValidRole(Role("retired")) {
		t.Fatal("a deactivated role must not be assignable")
	}
}

// TestGrantsPrincipalAuthorityBeyondReadsTheCatalog is the escalation guard the
// invitation path leans on, asserted through a catalog rather than through the
// compiled sets it was written against.
func TestGrantsPrincipalAuthorityBeyondReadsTheCatalog(t *testing.T) {
	installFake(t, &fakeCatalog{
		ranks: map[string]int{"dev": 300, "adm": 200},
		grants: map[string]map[VerbResource]bool{
			"dev": {{Verb: VerbRead, Resource: ResourcePrincipal}: true},
			"adm": {
				{Verb: VerbRead, Resource: ResourcePrincipal}:   true,
				{Verb: VerbUpdate, Resource: ResourcePrincipal}: true,
			},
		},
	})

	if !GrantsPrincipalAuthorityBeyond(Role("dev"), Role("adm")) {
		t.Fatal("adm holds update-on-principal and dev does not, so adm's people-authority " +
			"exceeds dev's -- rank is not authority")
	}
	if GrantsPrincipalAuthorityBeyond(Role("adm"), Role("dev")) {
		t.Fatal("dev holds no principal verb adm lacks")
	}
}

func TestCanonicalSlugResolvesAnAlias(t *testing.T) {
	installFake(t, &fakeCatalog{
		// `writer` is an ALIAS of `user`: Rank answers for both, Active is
		// keyed on the canonical slug alone.
		ranks: map[string]int{"user": 100, "writer": 100},
		names: map[string]string{"user": "Member"},
		grants: map[string]map[VerbResource]bool{
			"user": {{Verb: VerbRead, Resource: ResourceData}: true},
		},
	})

	if !IsValidRole(RoleWriter) {
		t.Fatal("`writer` is the spelling every ordinary principal's row carries; " +
			"IsValidRole must accept an alias or the whole cluster becomes unrankable")
	}
}

// TestAnEmptyCatalogWouldLockOutTheOwner is the reason
// component/memql's ReloadCapabilityCatalog refuses to install a catalog
// carrying no roles.
//
// The engine installs the catalog from its Start while the seed materializer
// that writes v1:rbac:role is gated on <-engine.Ready(), so a fresh-database
// boot has a window with zero rows. If that snapshot were installed,
// roleHasCapability's short-circuit would answer false for EVERY role rather
// than deferring to the compiled mirror -- and the role it refuses first is
// the owner, under a message telling them they lack authority they hold.
//
// This test asserts the failure mode rather than the guard, because the guard
// lives in another module and the failure is the thing that must stay true:
// delete the guard and this test still passes, but it tells the next reader
// exactly what they have re-enabled.
func TestAnEmptyCatalogWouldLockOutTheOwner(t *testing.T) {
	for _, role := range []Role{RoleOwner, RoleDeveloper} {
		if !Capable(role, VerbCreate, ResourceConstruct) {
			t.Fatalf("precondition: %s must hold create x construct in the compiled mirror", role)
		}
	}

	installFake(t, &fakeCatalog{})

	for _, role := range []Role{RoleOwner, RoleDeveloper} {
		if Capable(role, VerbCreate, ResourceConstruct) {
			t.Fatalf("an empty catalog answered TRUE for %s -- the short-circuit at "+
				"roleHasCapability is gone, and the empty-catalog guard in "+
				"component/memql.ReloadCapabilityCatalog is now guarding nothing", role)
		}
	}
}

// TestAdminHoldsNoAuthoringGrant pins the line the D9 DSL-deploy gate rests
// on. auth.CanAuthor is create x construct, and the whole point of routing the
// gate through it is that admin does not hold it (#1529 section 4). If a seed
// or a mirror edit ever grants admin that pair, the DSL-deploy gate silently
// widens to admin and no test in component/packages would notice.
func TestAdminHoldsNoAuthoringGrant(t *testing.T) {
	if Capable(RoleAdmin, VerbCreate, ResourceConstruct) {
		t.Fatal("admin holds create x construct -- the D9 gate and every CanAuthor site just widened to admin")
	}
	if !Capable(RoleAdmin, VerbRead, ResourceConstruct) {
		t.Fatal("admin lost read x construct -- expected admin to keep exactly the read grant")
	}
	if !CanAuthor(UserContext{Role: RoleDeveloper}) || !CanAuthor(UserContext{Role: RoleOwner}) {
		t.Fatal("CanAuthor refused owner or developer")
	}
	if CanAuthor(UserContext{Role: RoleAdmin}) {
		t.Fatal("CanAuthor admitted admin")
	}
}
