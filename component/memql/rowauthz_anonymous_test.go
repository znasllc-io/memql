package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE PUBLIC TIER, tested against the REAL admission function (epic
// memql#4541, D4).
//
// Every assertion below goes through rowAuthzAdmits, the same function the
// read path, the traversal gate and the write guard all call. That is
// deliberate and it is the repo's standing lesson about this area: a mocked
// engine has no gates, so a test that builds its own admission answer is a
// test of itself.
//
// The fixture concept is registered here rather than declared in dsl/,
// because the tier ships EMPTY OF ANONYMOUSLY-REACHABLE CONCEPTS: nothing in
// the engine tree is servable to a stranger, and the conformance suite proves
// it. (One engine concept does now declare the tier --
// v1:knowledge:knowledgeDomain, with the requiresIdentity narrowing of
// memql#4809 -- which is exactly why that gate takes the narrow population and
// this fixture is still needed to test the wide one.) A product bundle declares
// plain `public` on its own content concepts when it means to publish them;
// this is what one of those looks like.

const anonPublicConcept = "v1:rowauthzprobe:publicPage"

// withPublicFixtureConcept registers a concept declaring the public tier
// for the duration of one test.
//
// It goes through registerProbeConcept -- the helper the rowgate tests
// already use for exactly this -- rather than merging into the registry
// itself. That helper's comment records why the cleanup matters (a
// synthetic concept left behind carries no schema variants and fails every
// later test in the package that boots the engine), and a second copy of
// this fixture would be a second place for that lesson to be forgotten.
func withPublicFixtureConcept(t *testing.T) {
	t.Helper()
	registerProbeConcept(t, anonPublicConcept, &langparser.RowAuthzDecl{Tier: langparser.RowAuthzPublic})
}

func anonCtx() context.Context { return auth.ContextWithAnonymousActor(context.Background()) }

// TestAnonymousReachesPublicTierAndNothingElse is the load-bearing rule.
//
// The undeclared case is the one that matters and it is the one that would
// be easy to get backwards. rowAuthzAdmits ADMITS an undeclared concept --
// correct for the callers that branch was written for, and the tree has
// about 88 of them. If the anonymous actor inherited that, the public tier
// would publish most of the graph on the day it shipped, with every gate in
// the system reporting exactly what it was asked.
func TestAnonymousReachesPublicTierAndNothingElse(t *testing.T) {
	withPublicFixtureConcept(t)

	// The reachable positive: the tier works at all.
	row := rowOf(t, anonPublicConcept, anonPublicConcept+":home", map[string]any{"title": "Home"})
	if got := rowAuthzAdmits(anonCtx(), row.Concept, row.ID, row.Payload); got != rowAuthzAdmit {
		t.Fatalf("an anonymous caller was refused a @rowAuthz(public) row (admission=%v) -- the tier does nothing", got)
	}

	// UNDECLARED REFUSES. Undeclared is unmeasured, not public.
	const undeclared = "v1:testfixture:undeclaredThing"
	if _, err := memorynodes.Get(undeclared); err == nil {
		t.Fatalf("%s is registered; this fixture needs a concept the registry does not know", undeclared)
	}
	bare := rowOf(t, undeclared, undeclared+":x", map[string]any{"secret": "value"})
	if got := rowAuthzAdmits(anonCtx(), bare.Concept, bare.ID, bare.Payload); got != rowAuthzDeny {
		t.Errorf("an anonymous caller was admitted to an UNDECLARED concept (admission=%v).\n"+
			"This is the single most important rule in the public tier: rowAuthzAdmits admits undeclared "+
			"concepts for every other caller, and the tree has ~88 of them. Inheriting that default would "+
			"publish most of the graph the day the tier shipped -- silently, because every gate would be "+
			"reporting exactly what it was asked.", got)
	}

	// And the same concept still admits an ordinary caller, so the
	// assertion above is about the ANONYMOUS actor rather than about the
	// concept having been made unreadable.
	if got := rowAuthzAdmits(callerCtx("user-a"), bare.Concept, bare.ID, bare.Payload); got != rowAuthzAdmit {
		t.Errorf("an authenticated caller was refused an undeclared concept (admission=%v) -- the anonymous rule leaked onto every caller", got)
	}

	// The declared tiers refuse anonymous too, whatever they declare.
	ownedDecl := declFor(t, declaredOwnedConcept)
	owned := rowOf(t, declaredOwnedConcept, declaredOwnedConcept+":n1",
		map[string]any{ownedDecl.Owner: "user-a", "title": "private"})
	if got := rowAuthzAdmits(anonCtx(), owned.Concept, owned.ID, owned.Payload); got != rowAuthzDeny {
		t.Errorf("an anonymous caller was admitted to the owned tier (admission=%v)", got)
	}
	admin := rowOf(t, declaredClusterOwnerConcept, declaredClusterOwnerConcept+":c1", map[string]any{})
	if got := rowAuthzAdmits(anonCtx(), admin.Concept, admin.ID, admin.Payload); got != rowAuthzDeny {
		t.Errorf("an anonymous caller was admitted to the clusterOwner tier (admission=%v)", got)
	}
}

// TestAnonymousDoesNotAffectOtherCallers pins the "empty by default"
// property from the other side: with no anonymous actor on the context,
// nothing this change added runs.
func TestAnonymousDoesNotAffectOtherCallers(t *testing.T) {
	withPublicFixtureConcept(t)
	row := rowOf(t, anonPublicConcept, anonPublicConcept+":home", map[string]any{"title": "Home"})

	for name, ctx := range map[string]context.Context{
		"an ordinary caller": callerCtx("user-a"),
		"a cluster owner":    ownerRoleCtx("owner-1"),
		"no actor at all":    context.Background(),
	} {
		if got := rowAuthzAdmits(ctx, row.Concept, row.ID, row.Payload); got != rowAuthzAdmit {
			t.Errorf("%s was refused a public-tier row (admission=%v) -- public means public", name, got)
		}
	}
}

// TestAMalformedAnonymousActorIsNotAnonymous. Both halves of the actor
// have to agree; a half-built one must DENY rather than admit, since the
// anonymous rule is the only thing that would have granted it anything.
func TestAMalformedAnonymousActorIsNotAnonymous(t *testing.T) {
	withPublicFixtureConcept(t)
	row := rowOf(t, anonPublicConcept, anonPublicConcept+":home", map[string]any{"title": "Home"})

	for name, ac := range map[string]*auth.AccessContext{
		"the flag without the role": {UserId: auth.AnonymousUserId, Role: auth.RoleReader, IsAnonymous: true},
		"the role without the flag": {UserId: auth.AnonymousUserId, Role: auth.RoleAnonymous},
	} {
		ctx := auth.ContextWithAccess(context.Background(), ac)
		if auth.IsAnonymousActor(ctx) {
			t.Errorf("%s reads as the anonymous actor", name)
		}
		// It still reads the public row -- public admits everyone -- but
		// by the ORDINARY path, which is the point: it is not being
		// treated as a recognised anonymous principal.
		if got := rowAuthzAdmits(ctx, row.Concept, row.ID, row.Payload); got != rowAuthzAdmit {
			t.Errorf("%s: public-tier admission changed (admission=%v)", name, got)
		}
	}
}

// TestPlanLevelRefusalForAnonymous covers the cheaper second seam: a
// non-public read is refused BEFORE it runs, so a refused anonymous caller
// gets an error rather than an empty result that reads like "there is
// nothing here".
func TestPlanLevelRefusalForAnonymous(t *testing.T) {
	withPublicFixtureConcept(t)

	publicPlan := &QueryPlan{BoundConcept: anonPublicConcept}
	if err := refuseNonPublicReadForAnonymous(anonCtx(), publicPlan); err != nil {
		t.Fatalf("a public-tier read was refused: %v", err)
	}

	ownedPlan := &QueryPlan{BoundConcept: declaredOwnedConcept}
	err := refuseNonPublicReadForAnonymous(anonCtx(), ownedPlan)
	if err == nil {
		t.Fatal("an anonymous read of an owned-tier concept was not refused at the plan")
	}
	if !strings.Contains(err.Error(), declaredOwnedConcept) {
		t.Errorf("the refusal does not name the concept: %v", err)
	}

	// An authenticated caller is untouched on the same plan.
	if err := refuseNonPublicReadForAnonymous(callerCtx("user-a"), ownedPlan); err != nil {
		t.Errorf("an authenticated caller was refused by the anonymous plan gate: %v", err)
	}

	// A plan with no bound concept -- a raw query string -- cannot be
	// decided here and is deliberately left to the row gate.
	if err := refuseNonPublicReadForAnonymous(anonCtx(), &QueryPlan{}); err != nil {
		t.Errorf("a plan with no bound concept was refused at the plan level; the row gate is what decides those: %v", err)
	}
}

// TestAnonymousResultsShareOneCacheKey is the property that puts this work
// in the caching push rather than beside it.
//
// Anonymous reads carry no per-caller dimension, so one computation serves
// every visitor -- which makes public content the best-cached data in the
// system. It only holds because the anonymous actor's identity is a
// CONSTANT; anything per-visitor here would silently give each visitor
// their own cache entry, and the read would still be correct, just
// uncached, which is the kind of regression nothing notices.
//
// The second half is the one that would be a security bug: an
// authenticated caller must never share a key with an anonymous one.
func TestAnonymousResultsShareOneCacheKey(t *testing.T) {
	first := actorCacheKeyComponent(anonCtx())
	second := actorCacheKeyComponent(auth.ContextWithAnonymousActor(context.Background()))
	if first != second {
		t.Fatalf("two anonymous callers produced different cache keys (%q vs %q) -- every visitor would get their own cache entry", first, second)
	}

	for name, ctx := range map[string]context.Context{
		"an ordinary caller": callerCtx("user-a"),
		"a cluster owner":    ownerRoleCtx("owner-1"),
	} {
		if got := actorCacheKeyComponent(ctx); got == first {
			t.Errorf("%s shares the anonymous cache key (%q) -- a public read cached for a stranger could be served to them, or worse, theirs to a stranger", name, got)
		}
	}

	// And it is distinct from the no-actor sentinel, so a genuinely
	// unauthenticated internal read and a public visitor's read do not
	// collide in the cache.
	if bare := actorCacheKeyComponent(context.Background()); bare == first {
		t.Errorf("the anonymous actor shares the no-actor sentinel key %q", bare)
	}
}

// TestAnonymousSubscriptionParity is memql#4309's property, claimed for
// free by the public tier and therefore worth proving rather than
// assuming.
//
// Subscription admission delegates to rowAuthzAdmits -- the SAME function
// the read path calls -- so the live path inherits the read path's
// correctness instead of restating it. The value of the test is that it
// would catch the day somebody gives the fan-out its own opinion: an
// anonymous stream that receives events for a concept an anonymous READ
// refuses is a leak through a door nobody was watching, and it is exactly
// the shape memql#4309 was filed for.
func TestAnonymousSubscriptionParity(t *testing.T) {
	withPublicFixtureConcept(t)
	anon := auth.AnonymousActor()

	public := rowOf(t, anonPublicConcept, anonPublicConcept+":home", map[string]any{"title": "Home"})
	if got := AdmitSubscriptionRow(context.Background(), anon, public.Concept, public.ID, public.Payload); got != SubscriptionAdmit {
		t.Errorf("an anonymous stream was refused a public-tier event (%s) -- a public page cannot go live", got)
	}

	// The undeclared concept: refused on the read, and it must be refused
	// on the feed. Nothing about "we already sent you the row" makes it
	// less of an egress.
	const undeclared = "v1:testfixture:undeclaredThing"
	bare := rowOf(t, undeclared, undeclared+":x", map[string]any{"secret": "value"})
	if got := AdmitSubscriptionRow(context.Background(), anon, bare.Concept, bare.ID, bare.Payload); got != SubscriptionDeny {
		t.Errorf("an anonymous stream was delivered an UNDECLARED concept's event (%s) -- the subscription path has grown its own opinion, which is the drift memql#4309's design D2 exists to prevent", got)
	}

	ownedDecl := declFor(t, declaredOwnedConcept)
	owned := rowOf(t, declaredOwnedConcept, declaredOwnedConcept+":n1",
		map[string]any{ownedDecl.Owner: "user-a", "title": "private"})
	if got := AdmitSubscriptionRow(context.Background(), anon, owned.Concept, owned.ID, owned.Payload); got != SubscriptionDeny {
		t.Errorf("an anonymous stream was delivered an owned-tier event (%s)", got)
	}

	// The reachable positive on the OTHER side: the owner still gets their
	// own row, so the assertions above are about the anonymous actor and
	// not about subscriptions having been switched off.
	if got := AdmitSubscriptionRow(context.Background(),
		&auth.AccessContext{UserId: "user-a", Role: auth.RoleWriter},
		owned.Concept, owned.ID, owned.Payload); got != SubscriptionAdmit {
		t.Errorf("the owner was refused their own row on the feed (%s) -- the anonymous rule leaked onto every subscriber", got)
	}
}

// TestAnonymousWriteIsRefusedAtTheChokepoint.
//
// @rowAuthz(public) is a READ tier. The row-authz write guard returns nil
// for it ("a public tier is a statement that the rows are not owned, so
// there is no owner to compare a writer against") and nil for an
// undeclared concept -- both correct for the callers that guard was
// written for, and both an open door for a stranger. So the refusal lives
// at executeWrite, the single write chokepoint (memql#1709), which is what
// makes one check cover mutations, raw inserts, tool handlers and staged
// writes alike.
func TestAnonymousWriteIsRefusedAtTheChokepoint(t *testing.T) {
	withPublicFixtureConcept(t)
	e := &MemQLEngine{initialized: true, concepts: memorynodes.DefaultRegistry()}

	for _, conceptName := range []string{anonPublicConcept, declaredOwnedConcept, "v1:testfixture:undeclaredThing"} {
		_, _, err := e.executeWrite(anonCtx(), MutationNode{
			Concept:    conceptName,
			PayloadRaw: `{"id":"x","title":"written by a stranger"}`,
		}, false)
		if err == nil {
			t.Errorf("an anonymous write to %s was NOT refused -- there is no anonymous write in MemQL, and the public tier says nothing about who may create a row", conceptName)
			continue
		}
		if !strings.Contains(err.Error(), "no anonymous write") {
			t.Errorf("the write to %s was refused for the wrong reason (%v) -- a refusal that fires for an unrelated reason is not the gate firing", conceptName, err)
		}
	}
}

// The requiresIdentity narrowing (memql#4809), tested through the same real
// admission function everything else in this file goes through.
//
// The rule has TWO halves and both have to hold, because getting either one
// backwards is silent. Withhold too much and every existing public concept
// stops being readable by the anonymous callers it was published for; withhold
// too little and a catalog somebody declared "shared inside this cluster" is
// on the internet the day the flag is turned on.
func TestRequiresIdentityKeepsAConceptOffTheAnonymousDoor(t *testing.T) {
	const narrowed = "v1:rowauthzprobe:sharedCatalog"
	registerProbeConcept(t, narrowed, &langparser.RowAuthzDecl{
		Tier: langparser.RowAuthzPublic, RequiresIdentity: true,
	})
	row := rowOf(t, narrowed, narrowed+":domain-1", map[string]any{"name": "Residential real estate"})

	// Half one: an anonymous caller is refused, exactly as they are refused
	// an undeclared concept. This is the half the flag exists for.
	if got := rowAuthzAdmits(anonCtx(), row.Concept, row.ID, row.Payload); got != rowAuthzDeny {
		t.Errorf("an anonymous caller was admitted to @rowAuthz(public, requiresIdentity) (admission=%v).\n"+
			"The narrowing is the entire difference between 'this catalog has no row-level distinction to "+
			"draw' and 'serve this catalog to the internet', and it lands at exactly one site "+
			"(conceptDeclaresPublicTier). If it stops holding, every concept that took this form is "+
			"published the moment MEMQL_PUBLIC_READS_ENABLED is turned on.", got)
	}

	// Half two: every AUTHENTICATED caller still sees a plain public tier --
	// no predicate, no owner comparison, no narrowing of any kind. A flag
	// that leaked past the anonymous door would make a catalog with no owner
	// field unreadable by everyone, which presents as "this cluster has no
	// knowledge domains".
	if got := rowAuthzAdmits(callerCtx("user-a"), row.Concept, row.ID, row.Payload); got != rowAuthzAdmit {
		t.Errorf("an authenticated caller was refused a requiresIdentity row (admission=%v) -- "+
			"the narrowing leaked past the anonymous door", got)
	}

	// The reachable positive: with the SAME actor and the SAME shape of row,
	// plain `public` admits. So the refusal above is about the flag rather
	// than about anonymous callers being refused everything anyway.
	withPublicFixtureConcept(t)
	open := rowOf(t, anonPublicConcept, anonPublicConcept+":home", map[string]any{"title": "Home"})
	if got := rowAuthzAdmits(anonCtx(), open.Concept, open.ID, open.Payload); got != rowAuthzAdmit {
		t.Fatalf("plain @rowAuthz(public) refused an anonymous caller (admission=%v) -- "+
			"this control did not move, so the assertion above measures nothing", got)
	}
}
