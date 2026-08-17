package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// staged_scope_4040_test.go -- epic memql#3974, task memql#4040.
//
// memql#3976 ruled that a staged row is visible to nobody on the ordinary read
// path AND that it stays reachable "only through an explicitly staged-scoped
// read". memql#3983 shipped the first half; this suite covers the second.
//
// The three decisions memql#4040 asked for, each with a test that fails if it is
// reversed:
//
//	how the intent is declared  -- explicitly, per read, never inferred from
//	                               identity (TestStagedScopeIsNeverInferredFrom
//	                               Identity)
//	who may use it              -- the cluster owner, with authorization split
//	                               from the predicate (TestStagedScopeRequires
//	                               ClusterOwner)
//	does the row gate honour it -- yes, and provably the same set as the
//	                               conjunct (TestBothSeamsWithholdTheSameSet)
//
// Plus the one that is a security bug rather than a feature gap if it is
// missing: TestStagedScopeIsACacheKeyTerm.
//
// Every test here pairs its assertion with a control, because the subject is a
// gate: an implementation that withheld everything and one that withheld nothing
// would each satisfy half of any single-direction assertion.

// stagedScopeOn is the resolved scope an authorized caller gets for `ids`. It
// goes through the real ctx + resolver rather than constructing a StagedScope
// literal, so these tests exercise the declaration and authorization path the
// engine actually uses.
func stagedScopeOn(e *MemQLEngine, ids ...string) StagedScope {
	return e.stagedScopeFor(ContextWithStagedScope(ownerRoleCtx("owner-1"), ids...))
}

// TestStagedScopeWidensOnlyWhatItNames is the rule, with the control that
// matters most: scoping A must not reveal B.
func TestStagedScopeWidensOnlyWhatItNames(t *testing.T) {
	e := &MemQLEngine{}
	e.markConceptDataStaged(stagedTestConceptA)
	e.markConceptDataStaged(stagedTestConceptB)

	// Control: with no scope, both are withheld -- #3983's behaviour, unchanged.
	require.Equal(t, []string{stagedTestConceptB, stagedTestConceptA},
		e.stagedConceptIds(StagedScope{}),
		"the zero scope must reproduce the ordinary read exactly")

	scope := stagedScopeOn(e, stagedTestConceptA)
	require.Equal(t, []string{stagedTestConceptB}, e.stagedConceptIds(scope),
		"a scope naming A must drop A from the withheld set and leave B in it")

	require.True(t, e.admitStagedRow(scope, stagedTestNode("n1", stagedTestConceptA)),
		"the row gate must admit a row of the scoped concept, or the scope is a conjunct-only "+
			"half-measure -- exactly what memql#3983 refused to ship")
	require.False(t, e.admitStagedRow(scope, stagedTestNode("n2", stagedTestConceptB)),
		"a scope naming A must not reveal B: this is the control, and without it a scope that "+
			"simply disabled staging would pass every other assertion here")
}

// TestBothSeamsWithholdTheSameSet is the invariant memql#4040 named as the thing
// that must not break: "a scope honoured by one and not the other yields a read
// that returns some staged rows and not others, which is worse than either
// answer."
//
// The conjunct's excluded set is stagedConceptIds; the row gate's is whatever
// admitStagedRow denies. They are compared over every combination of staged set
// and scope, which is what makes this a statement about the mechanism rather
// than about one case.
func TestBothSeamsWithholdTheSameSet(t *testing.T) {
	concepts := []string{stagedTestConceptA, stagedTestConceptB, stagedTestLive}

	for _, staged := range [][]string{
		{},
		{stagedTestConceptA},
		{stagedTestConceptA, stagedTestConceptB},
	} {
		for _, scoped := range [][]string{
			{},
			{stagedTestConceptA},
			{stagedTestConceptB},
			{stagedTestConceptA, stagedTestConceptB},
			{stagedTestLive},
		} {
			e := &MemQLEngine{}
			for _, c := range staged {
				e.markConceptDataStaged(c)
			}
			scope := stagedScopeOn(e, scoped...)

			// What the PUSHDOWN excludes.
			conjunct := map[string]bool{}
			for _, id := range e.stagedConceptIds(scope) {
				conjunct[id] = true
			}
			// What the ROW GATE withholds.
			gate := map[string]bool{}
			for _, c := range concepts {
				if !e.admitStagedRow(scope, stagedTestNode("n", c)) {
					gate[c] = true
				}
			}

			require.Equal(t, gate, conjunct,
				"the conjunct and the row gate disagree for staged=%v scope=%v. "+
					"A pushdown that hides rows the gate would admit is wrong, and a gate that "+
					"withholds rows the pushdown fetched makes the read return some staged rows "+
					"and not others", staged, scoped)
		}
	}
}

// TestStagedScopeRequiresClusterOwner: the authorization half, and the fact that
// it is IDENTITY-derived while the predicate stays constant.
func TestStagedScopeRequiresClusterOwner(t *testing.T) {
	e := &MemQLEngine{}
	e.markConceptDataStaged(stagedTestConceptA)

	ownerCtx := ContextWithStagedScope(ownerRoleCtx("owner-1"), stagedTestConceptA)
	writerCtx := ContextWithStagedScope(callerCtx("writer-1"), stagedTestConceptA)

	require.False(t, e.stagedScopeFor(ownerCtx).IsEmpty(),
		"the cluster owner's declaration must resolve; this is the control for the denial below")
	require.NoError(t, e.refuseUnauthorizedStagedScope(ownerCtx))

	require.True(t, e.stagedScopeFor(writerCtx).IsEmpty(),
		"an unauthorized declaration must resolve to the EMPTY scope. This is what makes the "+
			"mechanism safe without entry-point cooperation: a path that never calls "+
			"refuseUnauthorizedStagedScope still gets no staged rows")
	require.False(t, e.admitStagedRow(e.stagedScopeFor(writerCtx), stagedTestNode("n", stagedTestConceptA)),
		"the row gate must withhold from a caller whose scope was refused")

	err := e.refuseUnauthorizedStagedScope(writerCtx)
	require.Error(t, err, "a declared-but-unauthorized scope must REFUSE the read rather than "+
		"silently downgrade to an ordinary one -- an empty result cannot be told apart from "+
		"nothing being staged")
	var denied *StagedScopeDeniedError
	require.ErrorAs(t, err, &denied, "the refusal must be a typed error a transport can map")
	require.Contains(t, denied.ConceptIds, stagedTestConceptA)
}

// TestStagedScopeIsNeverInferredFromIdentity is memql#3976's rule, restated as
// an assertion: being the cluster owner AUTHORIZES a scope, it does not GRANT
// one. Without this, "who may use it" would silently become "who gets it".
//
// It also pins the separation memql#4040 asked to be made deliberately: the
// predicate is a pure function of the RESOLVED scope, so an owner and a writer
// who both declared nothing produce byte-identical predicates.
func TestStagedScopeIsNeverInferredFromIdentity(t *testing.T) {
	e := &MemQLEngine{}
	e.markConceptDataStaged(stagedTestConceptA)

	ownerNoDeclaration := e.stagedScopeFor(ownerRoleCtx("owner-1"))
	require.True(t, ownerNoDeclaration.IsEmpty(),
		"the cluster owner who declared no scope must get no scope. Deriving it from identity is "+
			"exactly what memql#3976 ruled against: it turns the injected predicate from a "+
			"constant into something that depends on the caller")
	require.False(t, e.admitStagedRow(ownerNoDeclaration, stagedTestNode("n", stagedTestConceptA)),
		"a staged row must stay withheld from the cluster owner's ORDINARY read")

	ownerPredicate, err := e.stagedVisibilityPredicate(e.stagedScopeFor(ownerRoleCtx("owner-1")))
	require.NoError(t, err)
	writerPredicate, err := e.stagedVisibilityPredicate(e.stagedScopeFor(callerCtx("writer-1")))
	require.NoError(t, err)
	require.Equal(t, canonicalExpression(ownerPredicate), canonicalExpression(writerPredicate),
		"two callers who declared nothing must get the same predicate -- the predicate is a "+
			"function of the resolved scope, never of the identity that resolved it")
}

// TestStagedScopeIsACacheKeyTerm is the security half of this change, and it
// covers the exact case that makes the omission invisible.
//
// staged_enforce.go argues that a staged and an unstaged read cannot share a
// cache entry because the conjunct is part of plan.Root. That holds until a
// scope covers EVERY staged concept: then there is nothing left to exclude, the
// predicate is empty, and plan.Root is byte-identical to an ordinary caller's.
// Without a scope term in the key the operator's staged-inclusive result is
// cached under that signature and served to the next caller.
//
// So the test is deliberately built on the full-coverage case rather than a
// partial one, because a partial scope differs in plan.Root anyway and would
// pass even with the term removed.
func TestStagedScopeIsACacheKeyTerm(t *testing.T) {
	e := &MemQLEngine{}
	e.markConceptDataStaged(stagedTestConceptA)

	plan := &QueryPlan{Root: fieldCmp("payload.name", OpEq, "x"), BoundConcept: stagedTestConceptA}

	ordinary := e.planCacheSignature(callerCtx("writer-1"), plan)
	scoped := e.planCacheSignature(
		ContextWithStagedScope(ownerRoleCtx("owner-1"), stagedTestConceptA), plan)

	require.NotEqual(t, ordinary, scoped,
		"a staged-SCOPED read shares a result-cache key with an ordinary one. When the scope "+
			"covers every staged concept the injected predicate is EMPTY, so plan.Root alone "+
			"cannot tell the two apart -- the operator's staged rows would be cached under the "+
			"ordinary caller's key and served to them (memql#4040)")
	require.Contains(t, scoped, "stagedScope:")

	// Control 1: two identical scoped reads must still share a key, or the term
	// has made the cache useless for the path it protects.
	scopedAgain := e.planCacheSignature(
		ContextWithStagedScope(ownerRoleCtx("owner-2"), stagedTestConceptA), plan)
	require.Equal(t, scoped, scopedAgain,
		"two reads with the same resolved scope must share a cache key")

	// Control 2: an UNAUTHORIZED declaration resolves to the empty scope, so it
	// must key exactly like an ordinary read -- otherwise a caller could
	// fragment the cache by declaring scopes they may not use.
	require.Equal(t, ordinary,
		e.planCacheSignature(ContextWithStagedScope(callerCtx("writer-1"), stagedTestConceptA), plan),
		"a refused declaration must not appear in the cache key: the read it produces IS the "+
			"ordinary read")
}

// TestOrdinaryReadsKeepTheirCacheKey: the scope term is appended only for a
// non-empty scope, so no existing signature moves. Every installation that is
// not actively inspecting staged data is in this case.
func TestOrdinaryReadsKeepTheirCacheKey(t *testing.T) {
	e := &MemQLEngine{}
	plan := &QueryPlan{Root: fieldCmp("payload.name", OpEq, "x"), BoundConcept: stagedTestConceptA}
	require.NotContains(t, e.planCacheSignature(callerCtx("writer-1"), plan), "stagedScope:",
		"an ordinary read's cache key must be byte-identical to what it was before scopes existed")
}

// TestStagedScopeDeclarationIgnoresBlanks: `ContextWithStagedScope(ctx)` with
// nothing, or with only blanks, is not accidentally a request for something.
func TestStagedScopeDeclarationIgnoresBlanks(t *testing.T) {
	base := ownerRoleCtx("owner-1")
	require.True(t, stagedScopeDeclaredOn(ContextWithStagedScope(base)).IsEmpty())
	require.True(t, stagedScopeDeclaredOn(ContextWithStagedScope(base, "", "  ")).IsEmpty())
	require.False(t, stagedScopeDeclaredOn(ContextWithStagedScope(base, stagedTestConceptA)).IsEmpty())
	require.True(t, stagedScopeDeclaredOn(context.Background()).IsEmpty(),
		"an unstamped context declares nothing")
}

// TestStagedScopeSignatureIsSorted: the cache-key fragment must not depend on
// map iteration order, or one scoped read would fragment across cache entries.
func TestStagedScopeSignatureIsSorted(t *testing.T) {
	e := &MemQLEngine{}
	forward := stagedScopeOn(e, stagedTestConceptA, stagedTestConceptB).signature()
	reverse := stagedScopeOn(e, stagedTestConceptB, stagedTestConceptA).signature()
	require.Equal(t, forward, reverse)
	require.True(t, strings.Contains(forward, ","), "two ids render as a joined list; got %q", forward)
}

// TestScopedPredicateStillEvaluatesInBothCompilers extends the single most
// load-bearing property of memql#3983 to the scoped case.
//
// The conjunct is re-run in process against the row loadLatestNodes swaps in, so
// a predicate the SQL compiler accepts and the Go evaluator does not is a gate
// that passes every SQL assertion and leaks. A scope changes which ids are
// rendered, so it changes the predicate, so that property has to be re-checked
// rather than assumed to survive.
func TestScopedPredicateStillEvaluatesInBothCompilers(t *testing.T) {
	e := &MemQLEngine{}
	e.markConceptDataStaged(stagedTestConceptA)
	e.markConceptDataStaged(stagedTestConceptB)

	predicate, err := e.stagedVisibilityPredicate(stagedScopeOn(e, stagedTestConceptA))
	require.NoError(t, err)
	require.NotNil(t, predicate, "one concept remains staged and unscoped, so a term is still due")
	require.NoError(t, rewriteFilterFieldRefs(predicate))

	cmp, ok := predicate.(*ComparisonExpression)
	require.True(t, ok, "with A scoped in, only B's term remains, so the predicate collapses to "+
		"a single comparison rather than a conjunction")

	// The in-process arm: the scoped-in concept passes, the still-staged one does not.
	match, err := nodeMatchesExpression(stagedTestNode("n", stagedTestConceptA), cmp, map[string]map[string]any{})
	require.NoError(t, err)
	require.True(t, match, "the scoped concept's rows must survive the in-process re-check, or the "+
		"loadLatestNodes swap would drop exactly the rows the scope exists to show")

	match, err = nodeMatchesExpression(stagedTestNode("n", stagedTestConceptB), cmp, map[string]map[string]any{})
	require.NoError(t, err)
	require.False(t, match, "the unscoped staged concept must still be rejected in process")
}

// TestFullyScopedReadInjectsNothing: when the scope covers everything staged
// there is nothing left to exclude, so no conjunct is injected at all.
//
// Worth its own test because it is the state that makes the cache term
// necessary, and because "the predicate is empty" is the thing a reader would
// most naturally assume cannot happen.
func TestFullyScopedReadInjectsNothing(t *testing.T) {
	e := &MemQLEngine{}
	e.markConceptDataStaged(stagedTestConceptA)

	predicate, err := e.stagedVisibilityPredicate(stagedScopeOn(e, stagedTestConceptA))
	require.NoError(t, err)
	require.Nil(t, predicate,
		"a scope covering every staged concept leaves nothing to exclude, so plan.Root is "+
			"identical to an ordinary caller's -- which is precisely why the resolved scope has "+
			"to be a cache-key term")

	plan := &QueryPlan{Root: fieldCmp("payload.name", OpEq, "x")}
	require.NoError(t, e.enforceStagedDataOnPlan(plan, stagedScopeOn(e, stagedTestConceptA)))
	_, wrapped := plan.Root.(*LogicalExpression)
	require.False(t, wrapped, "nothing withheld means nothing ANDed into the root")
}
