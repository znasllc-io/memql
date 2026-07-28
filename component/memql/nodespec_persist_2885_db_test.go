package memql

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Issue #2885, end to end against a real database: createDeploymentNodeSpec
// must actually persist a row, and nodeSpecsForDeployment must find it.
//
// The DB-free half lives in nodespec_composite_id_2885_test.go and is the
// assertion that runs everywhere. These two add what only a database can
// show: that the row survives the full executor write path and is readable
// through the DSL query the deploy pack calls.
//
// Green-by-skip warning: this file skips wherever no Postgres is reachable
// (dbtest.Unreachable), so a local `go test ./...` proves nothing here. To
// verify the fix for real:
//
//	MEMQL_DATABASE_DSN=... MEMQL_REQUIRE_DB=1 go test -count=1 \
//	  -run TestDeploymentNodeSpec ./component/memql/

func TestDeploymentNodeSpec_CreatePersistsAndIsReadable(t *testing.T) {
	eng, db, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-nodespec-2885")
	depID := fmt.Sprintf("d-%s", uniqueSuffix("nodespec2885"))

	const conceptName = "v1:cluster:deploymentNodeSpec"

	// The parent deployment, written with a BARE deploymentId -- what
	// component/deploycontrol passes in production (id.NewShortId()).
	runMutation(t, ctx, eng, "createDeployment", map[string]any{
		"deploymentId": depID, "status": "pending", "version": "1.0.0",
	})

	// Executed directly rather than via runMutation so a failure surfaces
	// the engine's own rejection rather than a helper assertion.
	res, err := eng.Execute(ctx, fmt.Sprintf(
		`mutation createDeploymentNodeSpec(deploymentId: %q, nodeType: "bff", replicas: 2, version: "1.0.0")`, depID))
	require.NoError(t, err,
		"memql#2885: createDeploymentNodeSpec must persist a row; the raw composite id was rejected by the shortId gate before the store")
	require.NotNil(t, res)
	require.NotNil(t, res.Bundle)
	require.NotEmpty(t, res.Bundle.Nodes, "createDeploymentNodeSpec wrote no node")
	storedID := res.Bundle.Nodes[0].Id

	// The row really is on the append-only table, with both key parts
	// readable as payload fields.
	p := latestPayload(t, ctx, db, conceptName, storedID)
	require.Equal(t, depID, p["deploymentId"])
	require.Equal(t, "bff", p["nodeType"])
	require.Equal(t, float64(2), p["replicas"])

	// And the DSL query the deploy pack calls actually finds it. This is the
	// user-visible half of the defect: with the write failing,
	// nodeSpecsForDeployment returned empty in every environment.
	got := queryIds(t, ctx, eng, fmt.Sprintf("nodeSpecsForDeployment(deploymentId:%q)", depID))
	require.True(t, contains(got, storedID),
		"nodeSpecsForDeployment(%q) must return the spec just written; got %v", depID, got)
}

// A re-pin must append under the SAME concept id rather than mint a second
// timeline. Both re-pin shapes are exercised deliberately:
//
//   - repeated createDeploymentNodeSpec, which is what actually happens in
//     production -- examples/deploypack/pack.go issues the create on every
//     reconciliation, and updateDeploymentNodeSpec has no non-test caller;
//   - updateDeploymentNodeSpec, because it re-derives the id independently
//     and a divergence there would fork the timeline silently.
func TestDeploymentNodeSpec_RePinAppendsUnderTheSameId(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-nodespec-repin-2885")
	depID := fmt.Sprintf("d-%s", uniqueSuffix("nodespecrepin2885"))

	runMutation(t, ctx, eng, "createDeployment", map[string]any{
		"deploymentId": depID, "status": "pending", "version": "1.0.0",
	})

	createdID := runMutation(t, ctx, eng, "createDeploymentNodeSpec", map[string]any{
		"deploymentId": depID, "nodeType": "bff", "replicas": 1,
	})
	recreatedID := runMutation(t, ctx, eng, "createDeploymentNodeSpec", map[string]any{
		"deploymentId": depID, "nodeType": "bff", "replicas": 2, "version": "1.0.1",
	})
	require.Equal(t, createdID, recreatedID,
		"the deploy pack re-issues createDeploymentNodeSpec on every reconciliation; it must append to the same timeline, not mint a second row")

	repinnedID := runMutation(t, ctx, eng, "updateDeploymentNodeSpec", map[string]any{
		"deploymentId": depID, "nodeType": "bff", "replicas": 3, "version": "1.0.2",
	})
	require.Equal(t, createdID, repinnedID,
		"updateDeploymentNodeSpec re-derives the id independently; it must match createDeploymentNodeSpec byte for byte")

	// asOf latest collapses the append stream to current state per node type.
	got := queryIds(t, ctx, eng, fmt.Sprintf("nodeSpecsForDeployment(deploymentId:%q)", depID))
	require.Len(t, got, 1,
		"three writes for one (deploymentId, nodeType) must read back as ONE current row; got %v", got)
}

// A second node type under the same deployment is a distinct row. This is
// the collision guard: hashing must not collapse two node types onto one
// timeline.
func TestDeploymentNodeSpec_DistinctNodeTypesAreDistinctRows(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-nodespec-distinct-2885")
	depID := fmt.Sprintf("d-%s", uniqueSuffix("nodespecdistinct2885"))

	runMutation(t, ctx, eng, "createDeployment", map[string]any{
		"deploymentId": depID, "status": "pending", "version": "1.0.0",
	})

	bffID := runMutation(t, ctx, eng, "createDeploymentNodeSpec", map[string]any{
		"deploymentId": depID, "nodeType": "bff", "replicas": 2,
	})
	voiceID := runMutation(t, ctx, eng, "createDeploymentNodeSpec", map[string]any{
		"deploymentId": depID, "nodeType": "voice", "replicas": 1,
	})
	require.NotEqual(t, bffID, voiceID, "distinct node types must not share a concept id")

	got := queryIds(t, ctx, eng, fmt.Sprintf("nodeSpecsForDeployment(deploymentId:%q)", depID))
	require.Len(t, got, 2, "a deployment's per-node spec set must carry one row per node type; got %v", got)
}
