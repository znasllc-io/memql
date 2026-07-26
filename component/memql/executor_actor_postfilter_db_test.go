package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// activeStatusNode builds an in-memory node whose payload carries
// status=="active" -- the row shape the post-filter evaluates against.
func activeStatusNode(t *testing.T) memorynodes.MemoryNode {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"status": "active"})
	require.NoError(t, err)
	return memorynodes.MemoryNode{
		ID:      "v1:authoring:bundle:b-unit",
		Concept: "v1:authoring:bundle",
		Payload: raw,
	}
}

// executor_actor_postfilter_db_test.go is the real-engine reproduction +
// fix guard for memql#1659: an `actor.isClusterOwner == true` term in a query
// filter blew up with `field "actor.isClusterOwner" is not supported in
// queries` whenever the SQL scan returned at least one candidate row. The
// SQL-compile path resolves actor fields fine (the gate is a query-time
// constant bound into the WHERE clause), but executeCombinedFilterQuery ALSO
// post-filters every candidate via the ctx-less nodeMatchesExpression, which
// has no actor.* handling. With zero candidate rows the post-filter loop never
// ran, so the query "worked" only when its result set was empty -- exactly the
// systemActiveAuthoringBundles symptom in the issue.
//
// The fix resolves actor.* comparisons to their query-time boolean constant
// BEFORE post-filtering (the gate is already enforced once in the SQL WHERE),
// preserving AND/OR truth values without re-resolving actor state the
// post-filter cannot see. The actor gate stays enforced exactly once.
//
// Postgres-gated: skips when no DB is reachable, like
// executor_mutation_readmerge_db_test.go (whose readMergeTestEngine helper
// this reuses).

func clusterOwnerCtx(userId string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   auth.RoleOwner,
	})
	// Mutations resolve their actor from the TokenInfo (mutationActor ->
	// ActorFromContext); the query resolves actor.* from the AccessContext.
	// Carry both so seeding AND reading work under one context.
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

func nonClusterOwnerCtx(userId string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   auth.RoleWriter,
	})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

// seedActiveAuthoringBundle creates a draft authoring bundle owned by the
// caller, then activates it (status -> active) so it satisfies the
// statusIsActive half of systemActiveAuthoringBundles' filter.
func seedActiveAuthoringBundle(t *testing.T, ctx context.Context, eng *MemQLEngine, bundleId string) {
	t.Helper()
	create := fmt.Sprintf(`createAuthoringBundle(bundleId:%q,title:%q)`, bundleId, "actor-postfilter #1659 guard")
	if _, err := eng.Execute(ctx, create); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	activate := fmt.Sprintf(`activateAuthoringBundle(bundleId:%q)`, bundleId)
	if _, err := eng.Execute(ctx, activate); err != nil {
		t.Fatalf("activate bundle: %v", err)
	}
}

// TestActorClusterOwnerPostFilter_WithActiveRows is the headline #1659 guard.
// With an active bundle present, the cluster-owner caller must read it back
// through systemActiveAuthoringBundles WITHOUT the post-filter blowing up
// on the actor.isClusterOwner term.
func TestActorClusterOwnerPostFilter_WithActiveRows(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)

	ownerCtx := clusterOwnerCtx("u-owner-1659")
	bundleId := "v1:authoring:bundle:b-1659-owner"
	seedActiveAuthoringBundle(t, ownerCtx, eng, bundleId)

	res, err := eng.Execute(ownerCtx, "systemActiveAuthoringBundles()")
	// Before the fix this fails with:
	//   field "actor.isClusterOwner" is not supported in queries
	require.NoError(t, err, "cluster-owner query must not error on the actor.isClusterOwner post-filter (#1659)")
	require.NotNil(t, res)
	require.NotNil(t, res.Bundle)

	var found bool
	for _, node := range res.Bundle.Nodes {
		if node.GetId() == bundleId {
			found = true
			break
		}
	}
	require.True(t, found, "cluster owner must see the seeded active bundle %q (currently only works when the result set is empty)", bundleId)
}

// TestActorClusterOwnerPostFilter_GateStillEnforced proves the fix does not
// weaken the gate: a NON-cluster-owner caller sees zero rows through the
// cluster-owner-gated query even though an active bundle exists. The actor
// gate is enforced exactly once (in the SQL WHERE: the actor constant binds
// false, so the DB returns no rows).
func TestActorClusterOwnerPostFilter_GateStillEnforced(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)

	ownerCtx := clusterOwnerCtx("u-owner-1659b")
	bundleId := "v1:authoring:bundle:b-1659-gate"
	seedActiveAuthoringBundle(t, ownerCtx, eng, bundleId)

	// A different, non-cluster-owner user runs the system-scoped query.
	res, err := eng.Execute(nonClusterOwnerCtx("u-intruder-1659"), "systemActiveAuthoringBundles()")
	require.NoError(t, err)
	require.NotNil(t, res)

	if res.Bundle != nil {
		for _, node := range res.Bundle.Nodes {
			require.NotEqual(t, bundleId, node.GetId(),
				"non-cluster-owner must NOT see owner-gated active bundles -- gate would be weakened")
		}
		require.Empty(t, res.Bundle.Nodes,
			"non-cluster-owner must see zero rows through the cluster-owner-gated query")
	}
}

// TestActorComparisonPostFilterResolvesToConstant is a DB-free unit guard on
// the resolution helper: an actor.* comparison resolves to its query-time
// boolean constant so the ctx-less post-filter evaluator can run it.
func TestActorComparisonPostFilterResolvesToConstant(t *testing.T) {
	expr := &LogicalExpression{
		Op:    LogicalAnd,
		Left:  actorCmp("isClusterOwner", OpEq, true),
		Right: payloadCmp("status", OpEq, "active"),
	}

	// Cluster owner: the actor term resolves to TRUE, so the AND reduces to the
	// payload predicate -- an active row matches.
	ownerExpr, err := resolveActorComparisonsToConstants(clusterOwnerCtx("u1"), expr)
	require.NoError(t, err)
	node := activeStatusNode(t)
	match, err := nodeMatchesExpression(node, ownerExpr, map[string]map[string]any{})
	require.NoError(t, err, "post-filter must not error on a resolved actor term")
	require.True(t, match)

	// Non-cluster owner: the actor term resolves to FALSE, so the AND is false
	// regardless of the payload -- the row is filtered out.
	nonOwnerExpr, err := resolveActorComparisonsToConstants(nonClusterOwnerCtx("u2"), expr)
	require.NoError(t, err)
	match, err = nodeMatchesExpression(node, nonOwnerExpr, map[string]map[string]any{})
	require.NoError(t, err)
	require.False(t, match, "non-owner actor constant must drop the AND to false")

	// The raw, unresolved actor comparison is still unsupported in the
	// ctx-less post-filter (it must be resolved first).
	_, err = nodeMatchesExpression(node, expr, map[string]map[string]any{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not supported in queries")
}

// memql#2822 part B: with NO AccessContext, the admin gate must DENY on
// both paths a query takes -- which is what makes an admin-gated query
// return zero rows.
//
// memql#2801 was a silent fail-open here: the actor envelope resolved
// `isClusterOwner` to TRUE on a nil context, and since
// `actor.isClusterOwner==true` is the entire gate on the admin-tier
// constructs, those queries returned EVERY row rather than none. The
// telephony admin reads are full call-detail-record and DID inventory
// dumps across every partition.
//
// #2811 fixed the resolver and #2824 pinned the bindings; neither
// asserted the end property. This does, at the DB-free helper level --
// the file's DB variants skip without Postgres, which is the lane where
// #2801 shipped green.
//
// Scope, stated honestly: this asserts the GATE denies, not that a query
// executes and returns nothing. The step between the two is that the
// gate is a top-level `&&` conjunct in all five shipped admin
// constructs (dsl/telephony/queries.memql:13,22,35,48 --
// note :48 keeps its `||` parenthesized so the gate sits outside it --
// and dsl/authoring/queries.memql:70), so a false leaf zeroes the
// result. That composition is a corpus property rather than an engine
// one; memql#2839 tracks gating it.
func TestActorGate_NoAccessContext_DeniesOnBothPaths(t *testing.T) {
	// A request that never carried auth. Not "dev mode": with
	// MEMQL_IDENTITY_ENABLED=false the local-dev interceptor injects
	// claims that ensureAccess turns into a real owner AccessContext, so
	// dev mode does NOT reach here nil (memql#2801).
	noAuth := context.Background()

	gate := actorCmp("isClusterOwner", OpEq, true)
	gated := &LogicalExpression{
		Op:    LogicalAnd,
		Left:  gate,
		Right: payloadCmp("status", OpEq, "active"),
	}
	node := activeStatusNode(t)

	// Path 1 -- the in-process post-filter. The actor term must resolve
	// to FALSE, dropping the AND regardless of the payload.
	resolved, err := resolveActorComparisonsToConstants(noAuth, gated)
	require.NoError(t, err)
	postFilterMatch, err := nodeMatchesExpression(node, resolved, map[string]map[string]any{})
	require.NoError(t, err)
	// assert, not require: the agreement check below must stay reachable
	// when one path regresses, or it is decoration (memql#2822 review).
	assert.False(t, postFilterMatch,
		"an active row MATCHED the cluster-owner-gated filter with NO AccessContext -- "+
			"the admin gate is fail-open (memql#2801/#2822)")

	// Path 2 -- the SQL push-down. The gate compiles to a bound `? op ?`
	// and the LEFT bind is the resolved actor value; it must be false, so
	// the DB returns nothing. Asserting the bound VALUE rather than the
	// SQL text is the point -- the fragment is byte-identical whether the
	// gate is open or shut, so a text assertion passes either way. (The
	// text itself is already pinned by TestCompileActorFieldComparison.)
	compiled, err := compileActorFieldComparison(noAuth, gate)
	require.NoError(t, err)
	require.Len(t, compiled.args, 2)
	assert.Equal(t, false, compiled.args[0],
		"the actor constant bound into the WHERE clause must be FALSE with no AccessContext; "+
			"binding true makes every admin-tier query return the full table")

	// Both paths must agree, or a combined-filter query returns different
	// rows depending on which one ran. Modelled through the compiled
	// OPERATOR rather than assuming equality, so changing the gate's
	// operator cannot silently invalidate the comparison.
	sqlGateOpen, err := sqlBoundComparisonHolds(compiled)
	require.NoError(t, err)
	assert.Equal(t, postFilterMatch, sqlGateOpen,
		"SQL bind and post-filter disagree about a nil AccessContext")

	// Control: a real cluster owner still passes, so this is a gate and
	// not a denial of service on the admin surface.
	ownerResolved, err := resolveActorComparisonsToConstants(clusterOwnerCtx("u-owner-2822"), gated)
	require.NoError(t, err)
	ownerMatch, err := nodeMatchesExpression(node, ownerResolved, map[string]map[string]any{})
	require.NoError(t, err)
	assert.True(t, ownerMatch, "a real cluster owner must still see the row")
}

// sqlBoundComparisonHolds evaluates a compiled `? op ?` fragment over its
// own bound arguments, so the test models what the DB does with the
// emitted operator instead of hardcoding equality.
func sqlBoundComparisonHolds(compiled compiledExpression) (bool, error) {
	// Self-guarded: the sole call site checks this too, but the helper is
	// package-level and a future caller should not have to know.
	if len(compiled.args) != 2 {
		return false, fmt.Errorf("actor-gate fragment %q has %d bound args, want 2",
			compiled.sql, len(compiled.args))
	}
	switch compiled.sql {
	case "? = ?":
		return compiled.args[0] == compiled.args[1], nil
	case "? <> ?":
		return compiled.args[0] != compiled.args[1], nil
	}
	return false, fmt.Errorf("unmodelled actor-gate fragment %q -- extend sqlBoundComparisonHolds "+
		"deliberately rather than letting a new operator go unchecked", compiled.sql)
}
