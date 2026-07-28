package memql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// asof_reconstructability_1872_db_test.go -- the coverage memql#1872's
// acceptance criterion never got, and the reason memql#2880 resolves as a
// correction rather than a build.
//
// #1872 required: "Status transitions are persisted and queryable asOf any
// time (history reconstructable)." #2880 reported that criterion as unmet,
// reasoning from the fact that a query result collapses to one row per id
// (loadLatestNodes' DISTINCT ON, plus the id-keyed maps downstream).
//
// The collapse is real, but it does not defeat the criterion. Those stages
// collapse to one row per id AT THE ASOF INSTANT: executeFilterQuery adds
// `WHERE "createdAt" <= T` to the candidate scan and passes the same T to
// loadLatestNodes, whose `DISTINCT ON (id) ... ORDER BY id ASC, "createdAt"
// DESC` then picks, per id, the newest version at-or-before T. That is a
// point-in-time read, which is exactly what the criterion asks for.
//
// What the engine genuinely lacks is a different, STRONGER capability: one
// query returning every appended version at once. #2880 demonstrated that
// absence and inferred the criterion was unmet. These tests pin the
// distinction, because it is the kind of thing that gets re-litigated from
// the same wrong inference otherwise.
//
// Every existing asOf test in the tree is parse-level; nothing exercised
// time-travel against a real engine before this file.
//
// Postgres-gated: skips when no DB is reachable, reusing readMergeTestEngine.

// deploymentStatusAsOf runs the point-in-time read a caller actually has
// available: the named query wrapped in an asOf directive. It returns the
// status current at t, plus that version's own createdAt.
//
// asOf takes an RFC3339 literal -- `asOf args.at` is rejected by the grammar
// -- so the timestamp is inlined into the query string rather than passed as
// an arg. That limitation is real but separate; it does not affect whether
// the state at t is retrievable.
func deploymentStatusAsOf(t *testing.T, eng *MemQLEngine, ctx context.Context, depID string, at time.Time) (status string, createdAt time.Time, found bool) {
	t.Helper()
	q := fmt.Sprintf("asOf(deploymentById(deploymentId:%q), %q)", depID, at.UTC().Format(time.RFC3339Nano))
	res, err := eng.Execute(ctx, q)
	require.NoError(t, err, "point-in-time read must not error: %s", q)
	require.NotNil(t, res)
	if res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return "", time.Time{}, false
	}
	require.Len(t, res.Bundle.Nodes, 1,
		"an asOf read still collapses to one row per id; got %d", len(res.Bundle.Nodes))
	node := res.Bundle.Nodes[0]
	s, _ := node.GetPayload().AsMap()["status"].(string)
	return s, node.GetCreatedAt().AsTime(), true
}

// seedThreeVersions appends three versions of one deployment under a single
// concept id and returns the id. Each transition is a real append, which is
// what makes the history reconstructable at all.
func seedThreeVersions(t *testing.T, eng *MemQLEngine, ctx context.Context, sfx string) string {
	t.Helper()
	// uniqueSuffix is name+pid, which survives a rerun under the same pid --
	// and these tests assert an EXACT history, so a second run would append
	// three more versions to the same row and see six. Add a per-run
	// component so `go test -count=N`, the standard flake hunt, stays usable.
	depID := fmt.Sprintf("asof-%s-%d", sfx, time.Now().UnixNano())
	runMutation(t, ctx, eng, "createDeployment", map[string]any{
		"deploymentId": depID,
		"status":       "pending",
		"version":      "3.0.0",
	})
	// The append-only table keys on (id, createdAt), so two versions written
	// inside the same clock tick would be indistinguishable to a
	// point-in-time read. Production transitions are seconds apart; the sleep
	// keeps the test honest without pretending sub-tick ordering works.
	time.Sleep(10 * time.Millisecond)
	runMutation(t, ctx, eng, "updateDeploymentStatus", map[string]any{
		"deploymentId": depID,
		"status":       "in_progress",
	})
	time.Sleep(10 * time.Millisecond)
	runMutation(t, ctx, eng, "updateDeploymentStatus", map[string]any{
		"deploymentId": depID,
		"status":       "succeeded",
	})
	return depID
}

// TestDeploymentStateIsQueryableAsOfAnyTime is memql#1872's criterion, stated
// as an executable assertion: for any T, the read returns the state as it was
// at T -- not the newest state overall.
func TestDeploymentStateIsQueryableAsOfAnyTime(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-asof-1872")
	depID := seedThreeVersions(t, eng, ctx, uniqueSuffix("pit1872"))

	// Walk back from the newest version, collecting each version's own
	// createdAt, so the probes below use timestamps the ENGINE reported
	// rather than any out-of-band read of the table.
	var stamps []time.Time
	var statuses []string
	walkedOff := false
	probe := time.Now().UTC().Add(time.Minute)
	for i := 0; i < 8; i++ {
		status, createdAt, found := deploymentStatusAsOf(t, eng, ctx, depID, probe)
		if !found {
			walkedOff = true
			break
		}
		stamps = append(stamps, createdAt)
		statuses = append(statuses, status)
		probe = createdAt.Add(-time.Microsecond)
	}
	require.True(t, walkedOff,
		"the walk must terminate by running out of history, not by exhausting its loop bound -- "+
			"otherwise it is asserting on a truncated prefix")

	require.Equal(t, []string{"succeeded", "in_progress", "pending"}, statuses,
		"reading at each version's own createdAt, then just before it, must walk the "+
			"transitions newest-to-oldest -- this IS #1872's \"history reconstructable\"")

	// And the positive direction: probing AT each recorded instant returns
	// that instant's state, which is the criterion stated literally.
	for i, ts := range stamps {
		status, _, found := deploymentStatusAsOf(t, eng, ctx, depID, ts)
		require.True(t, found, "asOf %s must find the deployment", ts)
		require.Equal(t, statuses[i], status,
			"asOf %s must return the state current at that instant, not the newest overall", ts)
	}

	// Before the first append there is nothing to see.
	//
	// This is NOT what catches a read that ignores asOf entirely -- the walk
	// above already does that on its own (an always-latest read yields
	// ["succeeded", "succeeded", ...], which fails the require.Equal). What
	// this adds is the narrower case where the candidate scan drops its
	// `createdAt <= T` filter while loadLatestNodes keeps its own: the walk
	// still succeeds there, and only a probe before the row existed reveals
	// that the bound is no longer being applied end to end.
	_, _, found := deploymentStatusAsOf(t, eng, ctx, depID, stamps[len(stamps)-1].Add(-time.Second))
	require.False(t, found,
		"a read before the deployment existed must return nothing; returning a row would mean "+
			"asOf is being ignored")
}

// TestDeploymentAllVersionsInOneReadIsStillAbsent pins the capability that is
// genuinely missing, so the distinction this file rests on stays honest. If a
// timeline read mode ever lands, this test should fail and be replaced -- it
// asserts a limitation, not a desirable property.
func TestDeploymentAllVersionsInOneReadIsStillAbsent(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-asofall-1872")
	depID := seedThreeVersions(t, eng, ctx, uniqueSuffix("all1872"))

	res, err := eng.Execute(ctx, fmt.Sprintf("deploymentById(deploymentId:%q)", depID))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotNil(t, res.Bundle)
	require.Len(t, res.Bundle.Nodes, 1,
		"got %d rows for a deployment with three versions on disk. If a timeline read mode "+
			"has landed, that is GOOD and this test has done its job: delete it, and update the "+
			"retirement note in dsl/deployment/queries.memql plus the sibling comment in "+
			"component/deploycontrol/deploy.go, which both say no such mode exists (memql#2880)",
		len(res.Bundle.Nodes))
	require.Equal(t, "succeeded", res.Bundle.Nodes[0].GetPayload().AsMap()["status"],
		"the surviving row is the newest append")
}

// TestDeploymentByIdRemainsWrappableInAsOf guards the property that makes
// point-in-time deployment reads reachable at all.
//
// A query that DECLARES its own asOf clause cannot be wrapped by a caller's
// asOf -- the parser rejects it with "multiple asOf() directives are not
// supported". deploymentById declares none, which is why the reads above
// work. Its siblings in the same file -- deploymentsForCluster,
// supersededDeployments and nodeSpecsForDeployment -- all declare
// `asOf latest`, so none of them can be time-travelled by a caller.
//
// Adding `asOf latest` to deploymentById would look like a harmless
// clarification -- it changes nothing about what the query returns -- while
// silently removing the ability to read a deployment's history at all.
func TestDeploymentByIdRemainsWrappableInAsOf(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-asofwrap-1872")
	sfx := uniqueSuffix("wrap1872")
	depID := fmt.Sprintf("wrap-%s", sfx)

	runMutation(t, ctx, eng, "createDeployment", map[string]any{
		"deploymentId": depID,
		"status":       "pending",
		"version":      "4.0.0",
	})

	q := fmt.Sprintf("asOf(deploymentById(deploymentId:%q), %q)",
		depID, time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano))
	_, err := eng.Execute(ctx, q)
	require.NoError(t, err,
		"deploymentById must stay wrappable in asOf. If this fails with \"multiple asOf() "+
			"directives are not supported\", an asOf clause was added to the query and every "+
			"point-in-time deployment read is now impossible (memql#1872, #2880)")
}
