package memql

// staged_enforce_test.go -- the parts of the staged-DATA read gate (epic
// memql#3974, task memql#3983) that need no database.
//
// The end-to-end evidence -- rows actually withheld, the loadLatestNodes swap,
// the collapse position -- is in staged_enforce_db_test.go, because those are
// claims about what SQL returns and a fake cannot vouch for them. What is here
// is the set of claims that are true about the PREDICATE itself, and one of
// them (TestStagedPredicateIsEvaluableInBothCompilers) is the single most
// load-bearing test in the change: it is what says the gate does not evaporate
// between the SQL scan and the in-process re-check.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

const (
	stagedTestConceptA = "v1:cognition:utterance"
	stagedTestConceptB = "v1:cognition:space"
	stagedTestLive     = "v1:identity:user"
)

// stagedTestNode builds a bare row carrying only what the gates read.
func stagedTestNode(id, concept string) memorynodes.MemoryNode {
	return memorynodes.MemoryNode{
		ID:      id,
		Concept: concept,
		Payload: json.RawMessage(`{"name":"x"}`),
	}
}

// TestStagedKeyPrefixMatchesTierKeyBuilder pins this file's enumeration prefix
// against the tier's own key builder.
//
// It guards a FAIL-OPEN drift, which is why it exists at all. memql#3980 owns
// conceptDataStagedKey and composes a key for ONE concept; enforcement needs to
// ENUMERATE them, which that function cannot do, so the prefix is spelled twice.
// If the tier renamed its namespace, stagedConceptIds would match nothing,
// report an empty staged set, inject no conjunct, admit every row -- and every
// other test in this file would still pass, because they all set the marker
// through the tier's own writer. The whole feature would be off and silent.
func TestStagedKeyPrefixMatchesTierKeyBuilder(t *testing.T) {
	require.Equal(t, conceptDataStagedPrefix+stagedTestConceptA,
		conceptDataStagedKey(stagedTestConceptA),
		"the staged marker key namespace moved; stagedConceptIds would enumerate nothing and the read gate would fail OPEN")
}

func TestStagedConceptIdsIsNilWhenNothingIsStaged(t *testing.T) {
	e := &MemQLEngine{}
	require.Nil(t, e.stagedConceptIds(StagedScope{}))

	predicate, err := e.stagedVisibilityPredicate(StagedScope{})
	require.NoError(t, err)
	require.Nil(t, predicate, "nothing staged must inject nothing at all -- the empty set is the common case and has to stay free")
}

func TestStagedConceptIdsEnumeratesEveryMarkedConceptSorted(t *testing.T) {
	e := &MemQLEngine{}
	// Marked out of order; the enumeration must sort so the cache signature is
	// stable across calls rather than depending on sync.Map iteration order.
	e.markConceptDataStaged(stagedTestConceptA)
	e.markConceptDataStaged(stagedTestConceptB)
	// Sorted, so the cache signature is stable rather than dependent on
	// sync.Map iteration order. stagedTestConceptB ("...:space") sorts before
	// stagedTestConceptA ("...:utterance").
	require.Equal(t, []string{stagedTestConceptB, stagedTestConceptA}, e.stagedConceptIds(StagedScope{}))

	// Other promoted markers share the map and must not be mistaken for staged
	// concepts -- the prefix is what separates them.
	e.promotedAuthored.Store("concept:"+stagedTestLive, promotedMarker{})
	e.promotedAuthored.Store(conceptRetiredKey(stagedTestLive), promotedMarker{})
	require.Equal(t, []string{stagedTestConceptB, stagedTestConceptA}, e.stagedConceptIds(StagedScope{}),
		"only the conceptDataStaged: namespace may feed the read gate")

	e.clearConceptDataStaging(stagedTestConceptA)
	require.Equal(t, []string{stagedTestConceptB}, e.stagedConceptIds(StagedScope{}))
}

// TestStagedPredicateIsEvaluableInBothCompilers is THE test this change exists
// around.
//
// Both filtered read seams return through latestMatchingNodes, which reloads
// each candidate's true latest version through loadLatestNodes -- a query
// filtered on `id IN (?)` and nothing else -- and SWAPS it in before
// re-evaluating the expression in process. A predicate the SQL compiler
// understands and the in-process evaluator does not is therefore a gate that
// passes every SQL-level assertion and leaks the row anyway.
//
// So: the same predicate node, through both compilers, must agree.
func TestStagedPredicateIsEvaluableInBothCompilers(t *testing.T) {
	e := &MemQLEngine{}
	e.markConceptDataStaged(stagedTestConceptA)

	predicate, err := e.stagedVisibilityPredicate(StagedScope{})
	require.NoError(t, err)
	require.NotNil(t, predicate)
	// The plan path runs this immediately after injection; the predicate is
	// authored as `row.concept` and both compilers consume the stripped form.
	require.NoError(t, rewriteFilterFieldRefs(predicate))

	cmp, ok := predicate.(*ComparisonExpression)
	require.True(t, ok, "one staged concept renders one comparison")
	require.Equal(t, []string{"concept"}, cmp.Field.Parts)
	require.Equal(t, OpNe, cmp.Operator)

	// (a) the SQL half compiles.
	compiled, err := e.compileComparisonExpressionWithContext(cmp, "")
	require.NoError(t, err, "the injected conjunct must compile to SQL or the pushdown is dead")
	require.Contains(t, compiled.sql, "IS DISTINCT FROM")

	// (b) the IN-PROCESS half evaluates, and agrees.
	staged, err := nodeMatchesExpression(stagedTestNode("a", stagedTestConceptA), predicate, map[string]map[string]any{})
	require.NoError(t, err, "the in-process evaluator must handle the injected conjunct; if it errors, every read breaks once a concept is staged")
	require.False(t, staged, "a staged row must not match the conjunct")

	live, err := nodeMatchesExpression(stagedTestNode("b", stagedTestLive), predicate, map[string]map[string]any{})
	require.NoError(t, err)
	require.True(t, live, "a live row must still match")
}

// TestStagedPredicateRejectsEveryStagedConceptInTheSet is the multi-concept
// shape: a conjunction, still evaluable in process on both sides.
func TestStagedPredicateRejectsEveryStagedConceptInTheSet(t *testing.T) {
	e := &MemQLEngine{}
	e.markConceptDataStaged(stagedTestConceptA)
	e.markConceptDataStaged(stagedTestConceptB)

	predicate, err := e.stagedVisibilityPredicate(StagedScope{})
	require.NoError(t, err)
	require.NoError(t, rewriteFilterFieldRefs(predicate))
	_, isConjunction := predicate.(*LogicalExpression)
	require.True(t, isConjunction, "two staged concepts render an AND of two comparisons")

	for _, tc := range []struct {
		concept string
		visible bool
	}{
		{stagedTestConceptA, false},
		{stagedTestConceptB, false},
		{stagedTestLive, true},
		{"", true}, // no concept is not a staged concept; see admitStagedRow
	} {
		got, err := nodeMatchesExpression(stagedTestNode("n", tc.concept), predicate, map[string]map[string]any{})
		require.NoError(t, err, "concept %q", tc.concept)
		require.Equal(t, tc.visible, got, "concept %q", tc.concept)
	}
}

// TestConceptNotInSpellingBreaksTheInProcessEvaluator records WHY the predicate
// is a chain of `!=` rather than the tidier `row.concept out [...]`.
//
// This is the mistake a reviewer would suggest and a future author would make.
// `out` compiles to a clean `concept NOT IN (...)`, so it looks strictly
// better -- and it HARD-ERRORS in the in-process re-check, because that arm
// runs ensureString (a []string fails) and compareStringValues (which
// implements OpEq and OpNe only). Once any concept were staged, every read
// through latestMatchingNodes would fail rather than leak.
//
// The test asserts the breakage rather than describing it, so the reason the
// spelling was chosen cannot quietly stop being true.
func TestConceptNotInSpellingBreaksTheInProcessEvaluator(t *testing.T) {
	notIn := &ComparisonExpression{
		Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
		Operator: OpOut,
		Value:    []string{stagedTestConceptA, stagedTestConceptB},
	}
	_, err := nodeMatchesExpression(stagedTestNode("a", stagedTestLive), notIn, map[string]map[string]any{})
	require.Error(t, err, "if `out` ever becomes evaluable in process, the chain-of-!= spelling may be revisited -- until then it is the only shape both halves accept")
}

// TestStagedPredicateCacheSurvivesTheFieldRewrite pins the clone.
//
// rewriteFilterFieldRefs mutates in place -- it is what turns `row.concept`
// into `concept`. Handing out the cached tree itself would let the first query
// rewrite the cache entry, so the SECOND query would receive an
// already-stripped node and strip it again. This is the same hazard
// cloneRowAuthzPredicate exists for, one level deeper because this tree is a
// conjunction.
func TestStagedPredicateCacheSurvivesTheFieldRewrite(t *testing.T) {
	e := &MemQLEngine{}
	e.markConceptDataStaged(stagedTestConceptA)
	e.markConceptDataStaged(stagedTestConceptB)

	first, err := e.stagedVisibilityPredicate(StagedScope{})
	require.NoError(t, err)
	require.NoError(t, rewriteFilterFieldRefs(first))

	second, err := e.stagedVisibilityPredicate(StagedScope{})
	require.NoError(t, err)

	// The second caller must receive the AUTHORED form, untouched by the first
	// caller's rewrite.
	logical, ok := second.(*LogicalExpression)
	require.True(t, ok)
	left, ok := logical.Left.(*ComparisonExpression)
	require.True(t, ok)
	require.Equal(t, []string{"row", "concept"}, left.Field.Parts,
		"the cached predicate was corrupted by a previous query's in-place rewrite")

	require.NotSame(t, first, second, "each caller must get its own tree")
}

// TestEnforceStagedDataInjectsWithoutABoundConcept is the deliberate difference
// from enforceRowAuthzOnPlan, which returns early when plan.BoundConcept is
// empty.
//
// Row-authz must resolve a per-concept tier declaration and has nothing to
// resolve from without a binding. The staged predicate names the staged set
// directly and binds nothing, so it is exactly as meaningful on an unbound
// plan -- and unbound plans are 115 of the 619 constructs memql#3981 measured,
// the same population memql#3982 had to reach the row gate for.
func TestEnforceStagedDataInjectsWithoutABoundConcept(t *testing.T) {
	e := &MemQLEngine{}
	e.markConceptDataStaged(stagedTestConceptA)

	root := &ComparisonExpression{
		Field:    FieldReference{Raw: "row.id", Parts: []string{"row", "id"}},
		Operator: OpEq,
		Value:    "some-id",
	}
	plan := &QueryPlan{Root: root} // no BoundConcept, on purpose
	require.NoError(t, e.enforceStagedDataOnPlan(plan, StagedScope{}))

	conjunction, ok := plan.Root.(*LogicalExpression)
	require.True(t, ok, "an unbound plan must still receive the conjunct")
	require.Equal(t, LogicalAnd, conjunction.Op)
	require.Same(t, root, conjunction.Left, "the author's root is preserved as the LEFT operand, so `a || b` becomes `(a || b) && staged` rather than binding to the last disjunct")
}

func TestEnforceStagedDataIsANoOpWhenNothingIsStaged(t *testing.T) {
	e := &MemQLEngine{}
	root := &ComparisonExpression{
		Field:    FieldReference{Raw: "row.id", Parts: []string{"row", "id"}},
		Operator: OpEq,
		Value:    "some-id",
	}
	plan := &QueryPlan{Root: root}
	require.NoError(t, e.enforceStagedDataOnPlan(plan, StagedScope{}))
	require.Same(t, root, plan.Root,
		"an installation with nothing staged must produce a byte-identical plan, so no result-cache signature moves")
}

// TestConceptInequalityCompilesNullSafe pins the NULL-safety rule.
//
// The injected conjunct is an AND-ed negative predicate on every read, which is
// the exact shape that turned the isNotDeleted bug (memql#1685) into rows
// silently vanishing: plain SQL `<>` yields NULL, not true, when the left side
// is NULL, so the conjunct drops the row. `concept` is NOT NULL today, which is
// what makes IS DISTINCT FROM free; the point is that the gate's safety no
// longer depends on that annotation staying true.
func TestConceptInequalityCompilesNullSafe(t *testing.T) {
	e := &MemQLEngine{}

	ne, err := e.compileConceptComparison(OpNe, stagedTestConceptA)
	require.NoError(t, err)
	require.Equal(t, "(concept IS DISTINCT FROM ?)", ne.sql)
	require.Equal(t, []any{stagedTestConceptA}, ne.args)

	// Equality is untouched -- absent is correctly NOT equal to a value, and
	// the read-isolation registry check still rides on this arm.
	eq, err := e.compileConceptComparison(OpEq, stagedTestConceptA)
	require.NoError(t, err)
	require.Equal(t, "(concept = ?)", eq.sql)
}

func TestAdmitStagedRow(t *testing.T) {
	e := &MemQLEngine{}
	e.markConceptDataStaged(stagedTestConceptA)

	require.False(t, e.admitStagedRow(StagedScope{}, stagedTestNode("a", stagedTestConceptA)))
	require.True(t, e.admitStagedRow(StagedScope{}, stagedTestNode("b", stagedTestLive)))
	require.True(t, e.admitStagedRow(StagedScope{}, stagedTestNode("c", "  "+stagedTestLive+"  ")),
		"the concept is trimmed before the lookup, matching conceptDataIsStaged")
	require.True(t, e.admitStagedRow(StagedScope{}, stagedTestNode("d", "")),
		"a row with no concept is by definition not a row of a staged concept; admitRowAuthzBuiltinResult records the same decision for the same reason")

	e.clearConceptDataStaging(stagedTestConceptA)
	require.True(t, e.admitStagedRow(StagedScope{}, stagedTestNode("a", stagedTestConceptA)),
		"clearing the marker must make the rows live again with no restart")
}

func TestFilterStagedSetAndNodes(t *testing.T) {
	e := &MemQLEngine{}
	e.markConceptDataStaged(stagedTestConceptA)

	set := map[string]memorynodes.MemoryNode{
		"a": stagedTestNode("a", stagedTestConceptA),
		"b": stagedTestNode("b", stagedTestLive),
	}
	filtered := e.filterStagedSet(context.Background(), set)
	require.Len(t, filtered, 1)
	require.Contains(t, filtered, "b")

	nodes := []memorynodes.MemoryNode{
		stagedTestNode("a", stagedTestConceptA),
		stagedTestNode("b", stagedTestLive),
		stagedTestNode("c", stagedTestConceptA),
	}
	kept := e.filterStagedNodes(context.Background(), nodes)
	require.Len(t, kept, 1)
	require.Equal(t, "b", kept[0].ID)

	// Nothing staged -> the input is returned untouched, allocating nothing.
	clean := []memorynodes.MemoryNode{stagedTestNode("b", stagedTestLive)}
	require.Equal(t, len(clean), len(e.filterStagedNodes(context.Background(), clean)))
}

// TestStagedPredicateRefusesAnUnrenderableConceptId pins the fail-CLOSED
// direction: a concept id that cannot be rendered into a literal refuses the
// read rather than returning a nil predicate, which would publish the very rows
// the tier withholds.
func TestStagedPredicateRefusesAnUnrenderableConceptId(t *testing.T) {
	e := &MemQLEngine{}
	e.markConceptDataStaged(`v1:x:y"injected`)

	_, err := e.stagedVisibilityPredicate(StagedScope{})
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "staged-data")

	plan := &QueryPlan{Root: &ComparisonExpression{
		Field:    FieldReference{Raw: "row.id", Parts: []string{"row", "id"}},
		Operator: OpEq,
		Value:    "x",
	}}
	require.Error(t, e.enforceStagedDataOnPlan(plan, StagedScope{}),
		"the refusal must propagate to the plan, not be swallowed into an ungated read")
}
