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

// TestQueryDeploymentById_ReturnsTheLatestVersionAfterAnAppend pins the
// behaviour across a status transition, which is where a by-id filter could
// plausibly diverge from a payload filter: every transition APPENDS a new
// row under the same concept id, so the filter must still find the row and
// must surface the newest version.
//
// An earlier draft of this test asserted the opposite -- that the query
// returns every appended version -- because the DSL comment, the commit
// message and component/deploycontrol/deploy.go all say so. The engine does
// not do that. executeFilterQuery runs loadLatestNodes (DISTINCT ON (id)
// ORDER BY id, "createdAt" DESC) plus a seen[id] skip, and nodesToMap /
// intersectNodeSets are keyed by id, so a result set collapses to one row
// per id whether or not asOf is present. The assertion below is what
// actually holds; #1872's asOf-reconstructability criterion is a separate
// gap, filed as its own issue.
func TestQueryDeploymentById_ReturnsTheLatestVersionAfterAnAppend(t *testing.T) {
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

	require.Len(t, res.Bundle.Nodes, 1,
		"every query result collapses to one row per id; got %d", len(res.Bundle.Nodes))

	// The surviving row must be the NEWEST append, not the create. This is
	// the half that would break if the row-id filter and the version
	// collapse disagreed about which id they key on.
	payload := res.Bundle.Nodes[0].GetPayload().AsMap()
	require.Equal(t, "in_progress", payload["status"],
		"deploymentById must surface the latest appended version, not the original create")
	require.Equal(t, "2.0.0", payload["version"],
		"read-merge update must carry create-time metadata forward")
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
