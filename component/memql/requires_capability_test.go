package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// @requiresCapability (epic memql#5166, section E).
//
// The SIBLING of @requiresRank, and the difference is what these tests pin: a
// rank is a FLOOR on the ladder, a capability is a GRANT, and a cluster can
// hold one without the other. That is exactly why the slug-comparing specs this
// annotation replaces were wrong -- `role == "admin"` is neither, and it went
// on saying "admin" while the model it stood for grew a developer rung above
// admin holding strictly fewer principal verbs.

// capabilityFake is a catalog for the enforcement tests.
type capabilityFake struct {
	ranks  map[string]int
	grants map[string]map[auth.VerbResource]bool
}

func (f *capabilityFake) Rank(slug string) (int, bool) { r, ok := f.ranks[slug]; return r, ok }
func (f *capabilityFake) Name(slug string) string      { return slug }
func (f *capabilityFake) Holds(slug, verb, resource string) bool {
	return f.grants[slug][auth.VerbResource{Verb: verb, Resource: resource}]
}

func (f *capabilityFake) Grants(slug string) []auth.VerbResource {
	out := []auth.VerbResource{}
	for vr := range f.grants[slug] {
		out = append(out, vr)
	}
	return out
}
func (f *capabilityFake) Scope(string) string     { return "" }
func (f *capabilityFake) Active(slug string) bool { _, ok := f.ranks[slug]; return ok }
func (f *capabilityFake) Slugs() []string         { return nil }
func (f *capabilityFake) CanonicalSlug(s string) string {
	if _, ok := f.ranks[s]; ok {
		return s
	}
	return ""
}

func installCapabilityFake(t *testing.T) {
	t.Helper()
	auth.SetCapabilityCatalog(&capabilityFake{
		ranks: map[string]int{"owner": 400, "developer": 300, "admin": 200, "user": 100, "viewer": 50},
		grants: map[string]map[auth.VerbResource]bool{
			"owner": {
				{Verb: auth.VerbRead, Resource: auth.ResourcePrincipal}:   true,
				{Verb: auth.VerbUpdate, Resource: auth.ResourcePrincipal}: true,
			},
			"admin": {
				{Verb: auth.VerbRead, Resource: auth.ResourcePrincipal}:   true,
				{Verb: auth.VerbUpdate, Resource: auth.ResourcePrincipal}: true,
			},
			// The pair the whole migration turns on: a developer READS the user
			// list and holds no update on principal, so it reaches userById and
			// is refused the credential-adjacent reads.
			"developer": {{Verb: auth.VerbRead, Resource: auth.ResourcePrincipal}: true},
			"user":      {},
			"viewer":    {},
		},
	})
	t.Cleanup(func() { auth.SetCapabilityCatalog(nil) })
}

func asCaller(role string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:caller",
		Role:   auth.Role(role),
	})
}

func TestRequiresCapabilityAdmitsAHolderAndRefusesEverybodyElse(t *testing.T) {
	installCapabilityFake(t)
	e := &MemQLEngine{}
	fn := &Function{RequiresCapability: CapabilityRequirement{
		Verb: auth.VerbRead, Resource: auth.ResourcePrincipal,
	}}

	for _, tc := range []struct {
		role    string
		refused bool
	}{
		{"owner", false},
		{"admin", false},
		// A DEVELOPER IS ADMITTED to a read-on-principal construct, which is
		// the migration's whole point for userById: developers already read the
		// user list through searchUsers and reach the Users app, and the spec
		// this replaces refused them.
		{"developer", false},
		{"user", true},
		{"viewer", true},
		{"ghost", true},
	} {
		err := e.refuseBelowRequiredCapability(asCaller(tc.role), fn, "userById")
		if tc.refused && err == nil {
			t.Errorf("%s was admitted to a read-on-principal construct", tc.role)
		}
		if !tc.refused && err != nil {
			t.Errorf("%s was refused: %v", tc.role, err)
		}
		// THE CODE LEADS THE SENTENCE (epic memql#5289). The OS reads a
		// refusal's code out of the message as `<code>: <detail>`, so a
		// refusal that carried no code would render under the neutral
		// heading beside a control the shell had already hidden for the
		// same reason.
		if tc.refused && err != nil && !strings.HasPrefix(err.Error(), CodeCapabilityNotHeld+": ") {
			t.Errorf("%s's refusal does not lead with %s: %v", tc.role, CodeCapabilityNotHeld, err)
		}
	}
}

// TestUpdateOnPrincipalPreservesTodaysExclusion. The three credential-adjacent
// reads move to `update` on `principal` rather than `read`, and the reason is
// that developer holds the one and not the other -- so the exclusion the
// requiresOwnerOrAdmin spec produced is preserved by a rule that says what it
// means instead of naming two slugs.
func TestUpdateOnPrincipalPreservesTodaysExclusion(t *testing.T) {
	installCapabilityFake(t)
	e := &MemQLEngine{}
	fn := &Function{RequiresCapability: CapabilityRequirement{
		Verb: auth.VerbUpdate, Resource: auth.ResourcePrincipal,
	}}

	for _, admitted := range []string{"owner", "admin"} {
		if err := e.refuseBelowRequiredCapability(asCaller(admitted), fn, "patIdentitiesForUser"); err != nil {
			t.Errorf("%s was refused an update-on-principal read: %v", admitted, err)
		}
	}
	if err := e.refuseBelowRequiredCapability(asCaller("developer"), fn, "patIdentitiesForUser"); err == nil {
		t.Error("a developer was admitted to an update-on-principal read; " +
			"requiresOwnerOrAdmin excluded them and the annotation must too")
	}
}

func TestRequiresCapabilityRefusesACallWithNoIdentity(t *testing.T) {
	installCapabilityFake(t)
	e := &MemQLEngine{}
	fn := &Function{RequiresCapability: CapabilityRequirement{
		Verb: auth.VerbRead, Resource: auth.ResourcePrincipal,
	}}

	if err := e.refuseBelowRequiredCapability(context.Background(), fn, "userById"); err == nil {
		t.Fatal("a call carrying no caller identity was admitted")
	}
}

// TestInternalOriginPassesTheCapabilityGate, for the reason it passes the rank
// gate and the @serverOnly gate: trusted server-side Go stamped for one call is
// not a principal, and these rules govern principals.
func TestInternalOriginPassesTheCapabilityGate(t *testing.T) {
	installCapabilityFake(t)
	e := &MemQLEngine{}
	fn := &Function{RequiresCapability: CapabilityRequirement{
		Verb: auth.VerbUpdate, Resource: auth.ResourcePrincipal,
	}}

	ctx := auth.ContextWithInternalOrigin(asCaller("viewer"))
	if err := e.refuseBelowRequiredCapability(ctx, fn, "patIdentitiesForUser"); err != nil {
		t.Fatalf("an internal-origin call was refused: %v", err)
	}
}

// TestBothAnnotationsTogetherRequireBoth. They compose rather than override:
// each answers a different question, and a construct declaring both is
// declaring that a caller must clear the floor AND hold the grant.
func TestBothAnnotationsTogetherRequireBoth(t *testing.T) {
	installCapabilityFake(t)
	e := &MemQLEngine{}
	fn := &Function{
		RequiresRank:       "developer",
		RequiresCapability: CapabilityRequirement{Verb: auth.VerbUpdate, Resource: auth.ResourcePrincipal},
	}

	// admin clears the capability and NOT the rank floor (200 < 300).
	if err := e.refuseBelowRequiredRank(asCaller("admin"), fn, "both"); err == nil {
		t.Error("admin cleared a developer floor")
	}
	if err := e.refuseBelowRequiredCapability(asCaller("admin"), fn, "both"); err != nil {
		t.Errorf("admin was refused a grant it holds: %v", err)
	}
	// developer clears the rank floor and NOT the capability.
	if err := e.refuseBelowRequiredRank(asCaller("developer"), fn, "both"); err != nil {
		t.Errorf("developer was refused its own floor: %v", err)
	}
	if err := e.refuseBelowRequiredCapability(asCaller("developer"), fn, "both"); err == nil {
		t.Error("developer cleared a grant it does not hold")
	}
	// Only owner clears both.
	if err := e.refuseBelowRequiredRank(asCaller("owner"), fn, "both"); err != nil {
		t.Errorf("owner was refused the floor: %v", err)
	}
	if err := e.refuseBelowRequiredCapability(asCaller("owner"), fn, "both"); err != nil {
		t.Errorf("owner was refused the grant: %v", err)
	}
}

// ---------------------------------------------------------------------
// load-time validation
// ---------------------------------------------------------------------

func TestAMisspelledResourceRefusesLoad(t *testing.T) {
	installCapabilityFake(t)
	e := &MemQLEngine{}
	registry := newFunctionRegistry()
	_ = registry.Upsert(&Function{
		Name: "brokenConstruct",
		RequiresCapability: CapabilityRequirement{
			Verb: auth.VerbRead, Resource: "princpal",
		},
	})

	problems := e.validateRequiresCapabilitySlugs(context.Background(), registry)
	if len(problems) != 1 {
		t.Fatalf("want one load problem, got %d: %v", len(problems), problems)
	}
	msg := problems[0].Error()
	if !strings.Contains(msg, "brokenConstruct") || !strings.Contains(msg, "princpal") {
		t.Errorf("the refusal must name the construct and the typo: %s", msg)
	}
	// AND IT MUST PRINT THE LIST. A boot failure that says "unknown resource"
	// without saying what the known ones are sends the author to grep the
	// seeds, which is the moment the annotation was supposed to save.
	if !strings.Contains(msg, auth.ResourcePrincipal) {
		t.Errorf("the refusal does not name the known resources: %s", msg)
	}
}

func TestAMisspelledVerbRefusesLoad(t *testing.T) {
	installCapabilityFake(t)
	e := &MemQLEngine{}
	registry := newFunctionRegistry()
	_ = registry.Upsert(&Function{
		Name:               "brokenVerb",
		RequiresCapability: CapabilityRequirement{Verb: "manage", Resource: auth.ResourcePrincipal},
	})

	problems := e.validateRequiresCapabilitySlugs(context.Background(), registry)
	if len(problems) != 1 {
		t.Fatalf("want one load problem, got %d: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0].Error(), "manage") {
		t.Errorf("the refusal must name the verb: %s", problems[0].Error())
	}
}

func TestAHalfDeclaredRequirementRefusesLoad(t *testing.T) {
	installCapabilityFake(t)
	e := &MemQLEngine{}
	registry := newFunctionRegistry()
	// A verb with no resource. The parser stores @requiresCapability's
	// arguments as a list, so a one-argument form is what a typo produces --
	// and reading it as "half a requirement" would gate on nothing while still
	// looking like a gate.
	_ = registry.Upsert(&Function{
		Name:               "halfDeclared",
		RequiresCapability: CapabilityRequirement{Verb: auth.VerbRead},
	})

	problems := e.validateRequiresCapabilitySlugs(context.Background(), registry)
	if len(problems) != 1 {
		t.Fatalf("want one load problem for a half-declared requirement, got %v", problems)
	}
}

func TestAWellFormedRequirementLoadsCleanly(t *testing.T) {
	installCapabilityFake(t)
	e := &MemQLEngine{}
	registry := newFunctionRegistry()
	_ = registry.Upsert(&Function{
		Name:               "userById",
		RequiresCapability: CapabilityRequirement{Verb: auth.VerbRead, Resource: auth.ResourcePrincipal},
	})
	_ = registry.Upsert(&Function{Name: "plainConstruct"})

	if problems := e.validateRequiresCapabilitySlugs(context.Background(), registry); len(problems) != 0 {
		t.Fatalf("a well-formed requirement was refused: %v", problems)
	}
}

// TestPlanLevelCapabilitiesAreEnforced. A floor enforced only on the direct
// call is bypassed by a query that EXPANDS the floored construct, which is the
// same hole refusePlanBelowRequiredRank exists to close for ranks.
func TestPlanLevelCapabilitiesAreEnforced(t *testing.T) {
	installCapabilityFake(t)
	e := &MemQLEngine{}
	plan := &QueryPlan{RequiredCapabilities: map[string]CapabilityRequirement{
		"patIdentitiesForUser": {Verb: auth.VerbUpdate, Resource: auth.ResourcePrincipal},
	}}

	if err := e.refusePlanBelowRequiredCapability(asCaller("developer"), plan); err == nil {
		t.Fatal("a plan expanding an update-on-principal construct admitted a developer")
	}
	if err := e.refusePlanBelowRequiredCapability(asCaller("admin"), plan); err != nil {
		t.Fatalf("a plan expanding an update-on-principal construct refused an admin: %v", err)
	}
}
