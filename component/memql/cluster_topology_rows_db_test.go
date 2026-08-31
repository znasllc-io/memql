package memql

// cluster_topology_rows_db_test.go -- the LIVE half of memql#4766.
//
// v1:cluster:database and v1:cluster:identityProvider were written once at
// bootstrap and never completed: `engineVersion`, `extensions`,
// `extensionVersions`, `jwksUrl`, `acceptedAudiences` and every link field
// (`clusterId` on both children, `databaseId` / `identityProviderId` on the
// cluster) had no writer at all. Two of them were already COMPUTED in
// app/cluster.go and dropped by the automation step.
//
// test/dslconformance/bootstrap_forwards_every_field_test.go gates the
// FORWARDING statically -- every arg a mutation declares is passed by its one
// caller. That is the check that would have caught the original defect, and it
// is blind to two things this file covers:
//
//   - Whether the mutations actually PERSIST what they accept. A field can be
//     declared, forwarded, and still dropped on the write.
//   - Whether the rows are READABLE. Nothing read these concepts before, which
//     is the reason the gap survived; the two new queries are their first
//     readers, and they carry a @rowAuthz(clusterOwner) tier that did not
//     exist when the rows were designed.
//
// The tier is why the write half is asserted under a NON-owner actor. Bootstrap
// runs as a system actor, not as a cluster owner, and a tier that refused the
// write would break cluster bootstrap on a fresh database -- an outage, arrived
// at by adding a read.

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
)

// The three literal ids bootstrapCluster writes. Repeated here rather than
// imported because the test's job is to notice if they change: they are the
// only thing wiring the rows together, so a silent edit to one side would
// leave the links dangling exactly as they were before this issue.
const (
	testClusterID  = "v1:cluster:cluster:self"
	testDatabaseID = "v1:cluster:database:primary"
	testIdpID      = "v1:cluster:identityProvider:primary"
)

// The cluster-owner context comes from executor_actor_postfilter_db_test.go's
// clusterOwnerCtx, which carries BOTH the AccessContext and the TokenInfo --
// mutations resolve their actor from the latter and `actor.*` from the former,
// so a helper carrying only one works for reads and silently writes rows owned
// by nobody. Not redeclared here: two spellings of "the cluster owner" in one
// package is how a tier assertion becomes a no-op.

func writeTopologyRows(t *testing.T, eng *MemQLEngine) {
	t.Helper()
	// As bootstrap writes them: internal origin, and an actor that is NOT a
	// cluster owner. If the tier ever refuses this, cluster bootstrap breaks
	// on every fresh database and the failure is a dead cluster, not a blank
	// panel.
	ctx := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), "system:bootstrap-test"))

	_, err := eng.Execute(ctx, fmt.Sprintf(
		`createDatabase(databaseId: %q, clusterId: %q, host: "db.example.com", dbName: "memql", `+
			`sslMode: "require", port: 15432, engineVersion: "16.4", `+
			`extensions: ["timescaledb", "vector"], `+
			`extensionVersions: {"timescaledb": "2.25.2", "vector": "0.8.2"})`,
		testDatabaseID, testClusterID))
	require.NoError(t, err, "createDatabase must persist under the bootstrap actor -- a tier that "+
		"refuses this breaks cluster bootstrap on a fresh database")

	_, err = eng.Execute(ctx, fmt.Sprintf(
		`createIdentityProvider(identityProviderId: %q, clusterId: %q, name: "MemQL Identity", `+
			`issuerUrl: "https://identity.example.com/", clientIdPrefix: "abc12345", `+
			`redirectUrl: "https://app.example.com/callback", `+
			`acceptedAudiences: ["memql-api"], jwksUrl: "https://identity.example.com/.well-known/jwks.json")`,
		testIdpID, testClusterID))
	require.NoError(t, err, "createIdentityProvider must persist under the bootstrap actor")

	_, err = eng.Execute(ctx, fmt.Sprintf(
		`createCluster(clusterId: %q, name: "memql", region: "local", provider: "docker-local", `+
			`version: "1.0.0", databaseId: %q, identityProviderId: %q)`,
		testClusterID, testDatabaseID, testIdpID))
	require.NoError(t, err, "createCluster must persist under the bootstrap actor")
}

func TestClusterTopologyRowsAreCompleteAndLinked(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	writeTopologyRows(t, eng)

	owner := clusterOwnerCtx("v1:identity:user:topology-owner")

	dbRows := bundleRows(t, mustExecute(t, eng, owner, `query clusterDatabase()`))
	row := pickRow(t, dbRows, testDatabaseID)

	// EVERY ONE OF THESE WAS PERMANENTLY EMPTY before memql#4766.
	require.Equal(t, "16.4", row["engineVersion"],
		"engineVersion had no writer at all; app/cluster.go now probes it from the live connection")
	require.ElementsMatch(t, []any{"timescaledb", "vector"}, row["extensions"])
	require.Equal(t,
		map[string]any{"timescaledb": "2.25.2", "vector": "0.8.2"},
		row["extensionVersions"])
	require.Equal(t, testClusterID, row["clusterId"],
		"clusterId was never written, which left NO path from either child to the cluster row")

	// And this one was WRONG rather than empty: createDatabase stamped the
	// literal 5432 while the DSN parse had the real port in the payload.
	require.EqualValues(t, 15432, row["port"],
		"port was a stamped constant; a cluster on any other port recorded 5432")

	idpRows := bundleRows(t, mustExecute(t, eng, owner, `query clusterIdentityProvider()`))
	idp := pickRow(t, idpRows, testIdpID)

	// Both of these were computed by parseIdentityProviderInfo and dropped by
	// the automation step, which forwarded four fields out of six.
	require.Equal(t, "https://identity.example.com/.well-known/jwks.json", idp["jwksUrl"])
	require.ElementsMatch(t, []any{"memql-api"}, idp["acceptedAudiences"])
	require.Equal(t, testClusterID, idp["clusterId"])

	// The links resolve in BOTH directions, which is the whole point of the
	// literal ids: before this, cluster.databaseId / identityProviderId were
	// empty because createCluster accepted them and the automation never
	// passed them.
	clusterRows := bundleRows(t, mustExecute(t, eng, owner, `query existingCluster()`))
	cluster := pickRow(t, clusterRows, testClusterID)
	require.Equal(t, testDatabaseID, cluster["databaseId"])
	require.Equal(t, testIdpID, cluster["identityProviderId"])
}

// A re-run REFRESHES rather than duplicating, which is the backfill answer.
//
// The issue offered two ways to reach existing clusters -- a migration or a
// documented rebuild -- and there is a third that needs neither: the rows are
// written at LITERAL ids, so a second write is a new version of the same
// logical row and a read collapses to the latest. Bootstrap's infra steps are
// therefore gated on "a bff started" rather than on "the cluster does not
// exist yet", and any cluster completes its rows on the next restart.
//
// That only holds while three things do, and each fails silently on its own:
// the ids stay literal (generated ids would accumulate rows), the write is
// accepted a second time under the bootstrap actor (a tier that refused it
// would freeze the rows again), and the read collapses versions rather than
// returning one row per write.
func TestClusterTopologyRowsRefreshRatherThanDuplicate(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	writeTopologyRows(t, eng)

	// A second start, reporting a database that has since been upgraded.
	ctx := auth.ContextWithInternalOrigin(
		auth.ContextWithUserActor(context.Background(), "system:bootstrap-test"))
	_, err := eng.Execute(ctx, fmt.Sprintf(
		`createDatabase(databaseId: %q, clusterId: %q, host: "db.example.com", dbName: "memql", `+
			`sslMode: "require", port: 15432, engineVersion: "17.2", `+
			`extensions: ["timescaledb", "vector", "pgcrypto"], `+
			`extensionVersions: {"timescaledb": "2.26.0", "vector": "0.8.2", "pgcrypto": "1.3"})`,
		testDatabaseID, testClusterID))
	require.NoError(t, err,
		"the second write must be accepted under the bootstrap actor -- if the tier refuses it, "+
			"the rows freeze at first boot again, which is the defect memql#4766 is named for")

	rows := bundleRows(t, mustExecute(t, eng,
		clusterOwnerCtx("v1:identity:user:topology-owner"), `query clusterDatabase()`))
	require.Len(t, rows, 1,
		"a refresh must produce ONE logical row, not one per start -- got %d", len(rows))
	require.Equal(t, "17.2", rows[0]["engineVersion"],
		"the read must collapse to the LATEST version; a stale answer here means the row is "+
			"frozen exactly as it was before this change")
	require.Contains(t, rows[0]["extensions"], "pgcrypto",
		"a newly installed extension must appear on the next start")
}

// The tier is real: a non-owner reads nothing (memql#4766).
//
// Both concepts declare @rowAuthz(clusterOwner) and both queries spell out
// `requiresOwner && actor.isClusterOwner==true`. Asserting the owner path alone
// would pass with no tier at all, which is the state these rows were in.
func TestClusterTopologyRowsAreClusterOwnerOnly(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}
	writeTopologyRows(t, eng)

	// The positive control first: without it, "a writer sees nothing" is
	// satisfied by a query that returns nothing to anybody.
	owner := clusterOwnerCtx("v1:identity:user:topology-owner")
	require.NotEmpty(t,
		bundleRows(t, mustExecute(t, eng, owner, `query clusterDatabase()`)),
		"the owner must see the row, or the refusal below proves nothing")

	writer := auth.ContextWithUserActor(context.Background(), "v1:identity:user:topology-writer")
	for _, q := range []string{`query clusterDatabase()`, `query clusterIdentityProvider()`} {
		require.Empty(t,
			bundleRows(t, mustExecute(t, eng, writer, q)),
			"%s returned rows to a non-owner; these carry the cluster's infrastructure "+
				"topology and are cluster-owner only", q)
	}
}

// bundleRows reads a SHAPELESS query's rows.
//
// The two queries under test declare no shape on purpose -- a topology row is
// read whole and carries no credential -- and that decides how the result is
// read. A shaped query sets `output` and `OutputPayload()` returns row maps; a
// shapeless one leaves `output` nil and OutputPayload falls back to the
// Bundle. Reading the wrong one does not return empty, it returns a
// *GraphBundle where a []any was expected, which is at least loud.
//
// A bundle node keeps intrinsics on the envelope and concept fields under
// `payload`, so both are flattened here -- the envelope wins so `id` stays the
// row's own id.
func bundleRows(t *testing.T, res *ExecuteResult) []map[string]any {
	t.Helper()
	require.NotNil(t, res.Bundle, "a shapeless query must return a bundle")
	out := make([]map[string]any, 0, len(res.Bundle.Nodes))
	for _, node := range res.Bundle.Nodes {
		if node == nil {
			continue
		}
		row := map[string]any{}
		if node.Payload != nil {
			for k, v := range node.Payload.AsMap() {
				row[k] = v
			}
		}
		row["id"] = node.Id
		out = append(out, row)
	}
	return out
}

func mustExecute(t *testing.T, eng *MemQLEngine, ctx context.Context, call string) *ExecuteResult {
	t.Helper()
	res, err := eng.Execute(ctx, call)
	require.NoError(t, err, "executing %s", call)
	require.NotNil(t, res, "%s returned no result", call)
	return res
}

func pickRow(t *testing.T, rows []map[string]any, id string) map[string]any {
	t.Helper()
	for _, row := range rows {
		if row["id"] == id {
			return row
		}
	}
	t.Fatalf("no row with id %q among %d rows", id, len(rows))
	return nil
}
