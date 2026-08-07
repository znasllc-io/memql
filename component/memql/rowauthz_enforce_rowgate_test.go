package memql

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The per-row half of enforcement (memql#3172).
//
// Filter injection resolves the tier from the construct's declared
// binding, which a RAW query string does not have -- `handleExecuteQuery`
// hands the engine a client-supplied string with no construct behind it,
// and graph expansion reaches rows with no filter at all. This gate
// resolves the tier from the concept the ROW itself names, so no
// spelling of a filter can steer it.

// callerCtx is an authenticated caller with a concrete user id, the
// shape ensureAccess builds from verified claims.
func callerCtx(userId string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   auth.Role("writer"),
	})
}

// ownerRoleCtx is a caller the envelope resolves as cluster owner.
// (clusterOwnerCtx is taken by executor_actor_postfilter_db_test.go,
// which also carries a TokenInfo for its seeding mutations.)
func ownerRoleCtx(userId string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   auth.RoleOwner,
	})
}

func rowOf(t *testing.T, conceptName, id string, payload map[string]any) memorynodes.MemoryNode {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return memorynodes.MemoryNode{ID: id, Concept: conceptName, Payload: raw}
}

// The gate admits the owner and denies everyone else, on a REAL declared
// concept rather than a hand-built tier.
func TestRowGateAdmitsOwnerDeniesStranger(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	if decl.Tier != langparser.RowAuthzOwned {
		t.Fatalf("%s declares %q; this fixture needs the owned tier", declaredOwnedConcept, decl.Tier)
	}

	mine := rowOf(t, declaredOwnedConcept, declaredOwnedConcept+":mine",
		map[string]any{decl.Owner: "user-a", "title": "mine"})
	theirs := rowOf(t, declaredOwnedConcept, declaredOwnedConcept+":theirs",
		map[string]any{decl.Owner: "user-b", "title": "theirs"})

	if !admitRowAuthzNode(callerCtx("user-a"), mine) {
		t.Error("the owner was denied their own row")
	}
	if admitRowAuthzNode(callerCtx("user-a"), theirs) {
		t.Errorf("caller user-a was shown a row owned by user-b on a concept declaring %q -- "+
			"this is the read a raw query string reaches around the declared binding "+
			"(memql#3172 finding 1)", InjectedPredicate(decl))
	}
}

// FINDING 4, the row side: no caller identity, no rows. The refused
// implementation compiled the injected term to an `ownerUserId` equality
// against the EMPTY STRING,
// which MATCHES any row stored with an empty owner.
func TestRowGateDeniesWhenTheCallerHasNoIdentity(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	orphan := rowOf(t, declaredOwnedConcept, declaredOwnedConcept+":orphan",
		map[string]any{decl.Owner: "", "title": "owned by the empty string"})

	if admitRowAuthzNode(context.Background(), orphan) {
		t.Fatalf("a caller with no identity was shown a row whose owner field is the empty "+
			"string. `%s` is a predicate that MATCHES; the self-owned spelling fails closed, "+
			"so the two forms of one tier disagreed (memql#3172 finding 4)",
			InjectedPredicate(decl))
	}
	// And the same row is denied to a real caller who is not the owner.
	if admitRowAuthzNode(callerCtx("user-a"), orphan) {
		t.Error("an empty-owner row was shown to a named caller")
	}
}

// A row missing the declared owner field entirely cannot say who owns
// it, so it is denied rather than admitted by default.
func TestRowGateDeniesARowWithNoOwnerField(t *testing.T) {
	row := rowOf(t, declaredOwnedConcept, declaredOwnedConcept+":headless",
		map[string]any{"title": "no owner key at all"})
	if admitRowAuthzNode(callerCtx("user-a"), row) {
		t.Fatal("a row carrying no owner field was admitted; a row that cannot state its owner " +
			"is not a row this caller can be shown to own")
	}
}

// A NESTED key that happens to share the owner field's name is a
// different field. The analyzer's topLevelPayloadField makes the same
// distinction; a gate looser than the analyzer would credit a scope the
// tier never declared.
func TestRowGateIgnoresANestedFieldOfTheSameName(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	row := rowOf(t, declaredOwnedConcept, declaredOwnedConcept+":nested",
		map[string]any{"input": map[string]any{decl.Owner: "user-a"}})
	if admitRowAuthzNode(callerCtx("user-a"), row) {
		t.Fatalf("`input.%s == user-a` was accepted as ownership of the row", decl.Owner)
	}
}

// The clusterOwner tier behaves as declared on the row side too.
func TestRowGateClusterOwnerTier(t *testing.T) {
	decl := declFor(t, declaredClusterOwnerConcept)
	if decl.Tier != langparser.RowAuthzClusterOwner {
		t.Skipf("%s no longer declares clusterOwner", declaredClusterOwnerConcept)
	}
	row := rowOf(t, declaredClusterOwnerConcept, declaredClusterOwnerConcept+":c1",
		map[string]any{"number": "+15550000"})

	if admitRowAuthzNode(ownerRoleCtx("root"), row) != true {
		t.Error("the cluster owner was denied an administrative row")
	}
	if admitRowAuthzNode(callerCtx("user-a"), row) {
		t.Error("a non-owner was shown an administrative row")
	}
	if admitRowAuthzNode(context.Background(), row) {
		t.Error("a caller with no access context was shown an administrative row (memql#2801: nil denies)")
	}
}

// An UNDECLARED concept is untouched by the gate -- enforcement covers
// the declared population only.
func TestRowGateIgnoresUndeclaredConcepts(t *testing.T) {
	row := rowOf(t, "v1:identity:user", "v1:identity:user:u1", map[string]any{"email": "a@b.c"})
	if !admitRowAuthzNode(context.Background(), row) {
		t.Fatal("a row of an undeclared concept was denied; enforcement must not invent a tier")
	}
}

// The four tiers, at the predicate level, including the two the tree
// declares nowhere today.
//
// `public` injects NOTHING and must keep doing so -- that is the tier's
// entire meaning, and a public concept that started injecting a term
// would silently empty every unauthenticated read of it.
func TestPredicateRenderingPerTier(t *testing.T) {
	cases := []struct {
		name    string
		decl    *langparser.RowAuthzDecl
		want    string // canonicalExpression of the built predicate, "" == none
		wantErr string
	}{
		{
			name: "public injects nothing",
			decl: &langparser.RowAuthzDecl{Tier: langparser.RowAuthzPublic},
			want: "",
		},
		{
			name: "owned",
			decl: &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"},
			want: `owneruserid==&{userId}`,
		},
		{
			name: "self-owned names the row id, not a payload property",
			decl: &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: langparser.RowAuthzSelfOwnedField},
			want: `row.id==&{userId}`,
		},
		{
			name: "clusterOwner",
			decl: &langparser.RowAuthzDecl{Tier: langparser.RowAuthzClusterOwner},
			want: `actor.isclusterowner==true`,
		},
		{
			name: "granted resolves to the declared spec",
			decl: &langparser.RowAuthzDecl{Tier: langparser.RowAuthzGranted, Spec: "spaceMember"},
			want: `spec(spaceMember)`,
		},
		{
			name:    "owned with no owner field refuses",
			decl:    &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned},
			wantErr: "names no owner field",
		},
		{
			name:    "granted with no spec refuses",
			decl:    &langparser.RowAuthzDecl{Tier: langparser.RowAuthzGranted},
			wantErr: "names no spec",
		},
		{
			name:    "unknown tier refuses",
			decl:    &langparser.RowAuthzDecl{Tier: langparser.RowAuthzTier("sudo")},
			wantErr: "unknown tier",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := rowAuthzPredicateExpr(tc.decl)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("wanted a refusal containing %q, got predicate %v", tc.wantErr, expr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want == "" {
				if expr != nil {
					t.Fatalf("the %s tier injected %s; it must inject nothing",
						tc.decl.Tier, canonicalExpression(expr))
				}
				return
			}
			if expr == nil {
				t.Fatalf("tier %s injected nothing, wanted %q", tc.decl.Tier, tc.want)
			}
			if got := strings.ToLower(canonicalExpression(expr)); got != strings.ToLower(tc.want) {
				t.Fatalf("predicate = %q, want %q", got, tc.want)
			}
		})
	}
}

// The predicate cache must hand out a COPY. rewriteFilterFieldRefs
// mutates the node it is given (bare `ownerUserId` becomes
// `payload.ownerUserId`), so a shared node would be rewritten once and
// then handed to every later query already rewritten -- and rewritten
// twice is `payload.payload.ownerUserId`, a JSON path no row carries.
func TestPredicateCacheHandsOutACopy(t *testing.T) {
	decl := &langparser.RowAuthzDecl{Tier: langparser.RowAuthzOwned, Owner: "ownerUserId"}
	first, err := rowAuthzPredicateExpr(decl)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := rewriteFilterFieldRefs(first); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	second, err := rowAuthzPredicateExpr(decl)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first == second {
		t.Fatal("the cache handed out the same node twice")
	}
	if got := strings.ToLower(canonicalExpression(second)); got != "owneruserid==&{userid}" {
		t.Fatalf("the cached predicate was corrupted by a caller's rewrite: %q", got)
	}
}

// FINDING 4, the plan side: an owned tier injected for a caller with no
// identity REFUSES the read instead of comparing ownerUserId against the
// empty string.
func TestEnforcedReadRefusesAnActorlessCaller(t *testing.T) {
	e := probeEngine(t, declaredOwnedConcept, map[string]string{
		"probeActorless": `concept=="` + declaredOwnedConcept + `"`,
	})
	plan, err := e.parseWithFunctions("probeActorless()", e.functions, nil, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !plan.RowAuthzInjected {
		t.Fatal("the probe was not enforced, so this test measures nothing")
	}

	if err := refuseRowAuthzWithoutActor(context.Background(), plan); err == nil {
		t.Fatal("a read over a concept declaring the owned tier was allowed to run with no " +
			"caller identity. The injected term compiles to `ownerUserId = ''` and MATCHES " +
			"rows owned by the empty string, while the self-owned spelling fails closed -- " +
			"the two disagreed (memql#3172 finding 4)")
	} else if !strings.Contains(err.Error(), declaredOwnedConcept) {
		t.Errorf("the refusal does not name the concept: %v", err)
	}

	// An authenticated caller is not refused.
	if err := refuseRowAuthzWithoutActor(callerCtx("user-a"), plan); err != nil {
		t.Fatalf("an authenticated caller was refused: %v", err)
	}
}

// The clusterOwner tier does not compare against a user id, so the
// actorless refusal must not fire for it -- the admin gate itself
// already denies (memql#2801: nil resolves isClusterOwner false).
func TestActorlessRefusalIsScopedToTheOwnedTier(t *testing.T) {
	e := probeEngine(t, declaredClusterOwnerConcept, map[string]string{
		"probeAdminActorless": `concept=="` + declaredClusterOwnerConcept + `"`,
	})
	plan, err := e.parseWithFunctions("probeAdminActorless()", e.functions, nil, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := refuseRowAuthzWithoutActor(context.Background(), plan); err != nil {
		t.Fatalf("the clusterOwner tier refused on the owned tier's grounds: %v", err)
	}
}

// filterRowAuthzSet drops exactly the denied rows and leaves an
// all-admitted set untouched.
func TestFilterRowAuthzSetDropsOnlyDeniedRows(t *testing.T) {
	decl := declFor(t, declaredOwnedConcept)
	set := map[string]memorynodes.MemoryNode{
		"mine":   rowOf(t, declaredOwnedConcept, declaredOwnedConcept+":mine", map[string]any{decl.Owner: "user-a"}),
		"theirs": rowOf(t, declaredOwnedConcept, declaredOwnedConcept+":theirs", map[string]any{decl.Owner: "user-b"}),
		"other":  rowOf(t, "v1:identity:user", "v1:identity:user:u1", map[string]any{"email": "a@b.c"}),
	}
	got := filterRowAuthzSet(callerCtx("user-a"), set)
	if _, ok := got["theirs"]; ok {
		t.Error("a row owned by another caller survived the gate")
	}
	if _, ok := got["mine"]; !ok {
		t.Error("the caller's own row was dropped")
	}
	if _, ok := got["other"]; !ok {
		t.Error("an undeclared concept's row was dropped")
	}
}

// registerProbeConcept adds a synthetic declared concept to the loaded
// registry so the tiers the tree declares NOWHERE (public, granted) are
// exercised through the real lookup rather than around it.
//
// The registry is process-global and a synthetic concept carries no
// schema variants, so the probe is REMOVED again on cleanup. Left
// behind, it fails every later test in the package that boots the engine
// over the embedded tree ("schema variant \"definition\" not
// registered for concept ...") -- a real failure of a real invariant,
// caused entirely by the fixture.
func registerProbeConcept(t *testing.T, name string, decl *langparser.RowAuthzDecl) string {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	before := memorynodes.All()
	t.Cleanup(func() { memorynodes.ReplaceAll(before) })
	memorynodes.MergeAll(map[string]*memorynodes.Concept{
		name: {Name: name, RowAuthz: decl},
	})
	return name
}

// The `public` tier admits everyone, including a caller with no
// identity. "Declared public" and "declares nothing" are different
// states (#2803), and enforcement must keep the first one readable.
func TestRowGatePublicTierAdmitsEveryone(t *testing.T) {
	name := registerProbeConcept(t, "v1:rowauthzprobe:publicThing",
		&langparser.RowAuthzDecl{Tier: langparser.RowAuthzPublic})
	row := rowOf(t, name, name+":p1", map[string]any{"ownerUserId": "somebody-else"})

	if !admitRowAuthzNode(context.Background(), row) {
		t.Error("the public tier denied an unauthenticated caller")
	}
	if !admitRowAuthzTraversal(context.Background(), row) {
		t.Error("the public tier denied a traversal")
	}
}

// GRAPH EXPANSION (memql#3172 DoD item / #2803 design decision 3).
//
// Traversal has no filter, so `granted` -- whose predicate is a
// relationship spec -- cannot be deferred to one and fails closed
// there, while the filtered path admits it because the filter carried
// the spec. This pins that asymmetry, the only place the two gates
// differ.
func TestTraversalGateFailsClosedWhereTheFilterGateDefers(t *testing.T) {
	name := registerProbeConcept(t, "v1:rowauthzprobe:grantedThing",
		&langparser.RowAuthzDecl{Tier: langparser.RowAuthzGranted, Spec: "spaceMember"})
	row := rowOf(t, name, name+":g1", map[string]any{"spaceId": "space-1"})

	if got := rowAuthzAdmits(callerCtx("user-a"), row.Concept, row.ID, row.Payload); got != rowAuthzUndecided {
		t.Fatalf("granted tier resolved to %v against a lone row, want undecided -- a relationship "+
			"grant cannot be decided without the join the filter performs", got)
	}
	if !admitRowAuthzNode(callerCtx("user-a"), row) {
		t.Error("the FILTERED path denied a granted-tier row; there the filter carries the spec " +
			"as a top-level conjunct and has already done the join")
	}
	if admitRowAuthzTraversal(callerCtx("user-a"), row) {
		t.Fatal("graph expansion admitted a granted-tier row. Traversal runs no filter, so there " +
			"is nothing for the undecidable verdict to defer to -- it must fail closed " +
			"(memql#3172, the graph-expansion half of the DoD)")
	}
}

// An unknown tier is a broken declaration, not a permission: it denies
// on both paths.
func TestRowGateDeniesAnUnknownTier(t *testing.T) {
	name := registerProbeConcept(t, "v1:rowauthzprobe:brokenThing",
		&langparser.RowAuthzDecl{Tier: langparser.RowAuthzTier("sudo")})
	row := rowOf(t, name, name+":b1", map[string]any{"ownerUserId": "user-a"})

	if admitRowAuthzNode(callerCtx("user-a"), row) {
		t.Error("an unrecognised tier was read as permission on the filtered path")
	}
	if admitRowAuthzTraversal(callerCtx("user-a"), row) {
		t.Error("an unrecognised tier was read as permission on the traversal path")
	}
}
