package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

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
