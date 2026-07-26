package memql

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// query_by_row_id_2784_db_test.go is the real-engine guard for memql#2784.
//
// Two by-id queries used to filter a payload field that duplicated their own
// row id -- `deploymentId == args.deploymentId` and `clientId ==
// args.clientId` -- rather than the row intrinsic. The in-file justification
// claimed the row id "would need partition+concept prefixing"; that was stale
// on both counts (the {partition}: prefix went in #56, and the concept prefix
// is composed by the engine via resolveFullId, not by the caller).
//
// Migrating a by-id filter is the kind of change that fails SILENTLY -- a
// wrong composition returns zero rows rather than erroring -- so these assert
// against a live engine. TestRowIdFilterRoundTripsWithStorageId
// (executor_filter_test.go) pins the composition itself without a database;
// this pins that the query actually finds the row the mutation wrote.
//
// Both halves are asserted per the harness convention: a single-sided
// assertion passes against a too-broad filter just as happily as a correct
// one.
//
// Postgres-gated: skips when no DB is reachable, reusing readMergeTestEngine.

func TestQueryDeploymentById_FiltersOnRowIdNotThePayloadMirror(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-depl-2784")
	sfx := uniqueSuffix("depl2784")

	// Bare, colon-free ids -- what deploycontrol passes in production
	// (id.NewShortId()). The concept layer canonicalises them on write.
	wantID := fmt.Sprintf("want-%s", sfx)
	otherID := fmt.Sprintf("other-%s", sfx)

	for _, d := range []string{wantID, otherID} {
		runMutation(t, ctx, eng, "createDeployment", map[string]any{
			"deploymentId": d,
			"status":       "pending",
			"version":      "1.0.0",
			"clusterId":    "c-" + sfx,
		})
	}

	got := queryIds(t, ctx, eng, fmt.Sprintf("deploymentById(deploymentId:%q)", wantID))

	wantCanonical := "v1:cluster:deployment:" + wantID
	otherCanonical := "v1:cluster:deployment:" + otherID

	// Inclusion: a bare deploymentId must find the row the mutation wrote.
	require.True(t, contains(got, wantCanonical),
		"deploymentById(%q) MUST return the deployment created with that bare id; got %v", wantID, got)
	// Exclusion: proves the filter is not simply matching everything.
	require.False(t, contains(got, otherCanonical),
		"deploymentById(%q) MUST NOT return a different deployment; got %v", wantID, got)
}

// TestQueryDeploymentById_ReturnsTheWholeAppendOnlyTimeline covers the
// property the query exists for: every status transition appends a new row
// under the SAME concept id, and the query must return all of them so a
// deployment's status is reconstructable asOf any point in time.
//
// This is the half a naive migration would break -- filtering the row id is
// only equivalent to filtering the payload mirror if every appended version
// shares that id.
func TestQueryDeploymentById_ReturnsTheWholeAppendOnlyTimeline(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-depltl-2784")
	sfx := uniqueSuffix("depltl2784")

	depID := fmt.Sprintf("timeline-%s", sfx)
	runMutation(t, ctx, eng, "createDeployment", map[string]any{
		"deploymentId": depID,
		"status":       "pending",
		"version":      "2.0.0",
	})
	runMutation(t, ctx, eng, "updateDeploymentStatus", map[string]any{
		"deploymentId": depID,
		"status":       "in_progress",
	})

	res, err := eng.Execute(ctx, fmt.Sprintf("deploymentById(deploymentId:%q)", depID))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotNil(t, res.Bundle)

	require.GreaterOrEqual(t, len(res.Bundle.Nodes), 2,
		"deploymentById must return EVERY appended version (create + status transition), got %d", len(res.Bundle.Nodes))
}

func TestQueryOAuthClientByClientId_FiltersOnRowIdNotThePayloadMirror(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-oauth-2784")
	sfx := uniqueSuffix("oauth2784")

	wantID := fmt.Sprintf("want-%s", sfx)
	otherID := fmt.Sprintf("other-%s", sfx)

	for _, c := range []string{wantID, otherID} {
		runMutation(t, ctx, eng, "createOAuthClient", map[string]any{
			"clientId":         c,
			"clientName":       "Test " + c,
			"redirectURIsJSON": `["https://app.example.com/cb"]`,
			"registeredAt":     "2026-07-26T00:00:00Z",
		})
	}

	got := queryIds(t, ctx, eng, fmt.Sprintf("oAuthClientByClientId(clientId:%q)", wantID))

	require.True(t, contains(got, "v1:identity:oauthClient:"+wantID),
		"oAuthClientByClientId(%q) MUST return the client registered under that id; got %v", wantID, got)
	require.False(t, contains(got, "v1:identity:oauthClient:"+otherID),
		"oAuthClientByClientId(%q) MUST NOT return a different client; got %v", wantID, got)
}
