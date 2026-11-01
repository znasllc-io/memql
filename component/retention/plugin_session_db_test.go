package retention

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// plugin_session_db_test.go -- the documented manual sweep, run by a person, over a
// concept no client reads (deployable access codes, re-review of Task 3).
//
// ===========================================================================
// WHAT WENT WRONG
// ===========================================================================
// conceptRetentionSweep is documented as callable by a cluster owner, and the handler
// admitted each row it was about to retire under the CALLER (AdmitSourceRow, the same
// per-row gate a read passes). The two access concepts are unreadable to a client by any
// path (clientUnreadableConcepts in component/memql), and a person at client origin is a
// client however senior: so the first session row past its thirty days refused the whole
// batch, "row admission refused; records preserved", and a person could not run the sweep
// the documentation tells them to run. The hourly automation was never affected, because
// it runs as the maintenance principal.
//
// The ruling is that, once the caller has proved to be a cluster owner, the pass runs as
// the maintenance principal -- the one the scheduled sweep already runs as -- so a manual
// run and the hourly run are the same pass. These tests drive the HANDLER, which is where
// the elevation is, against the real concept and the real admission.

// sessionSweepFixture is a fixture whose only retained concept is the real access
// session, with one row past its TTL and one fresh row, and the handler wired the way
// the plug-in wires it: the real AdmitSourceRow, a live concept registry, the fixture's
// database.
func sessionSweepFixture(t *testing.T) (*fixture, *Integration, string, string) {
	t.Helper()
	f := newFixture(t)
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	session, err := memorynodes.Get("v1:platform:siteAccessSession")
	require.NoError(t, err)
	require.NotNil(t, session.Retention, "the session concept declares no retention, so this test measures nothing")
	f.concept = session

	const expired, fresh = "v1:platform:siteAccessSession:expired", "v1:platform:siteAccessSession:fresh"
	row := `{"siteId":"v1:platform:site:s","ownerUserId":"v1:identity:user:the-site-owner","codeId":"v1:platform:siteAccessCode:c","generation":1,"tokenHash":"` +
		"0000000000000000000000000000000000000000000000000000000000000000" + `","issuedAt":"2026-09-01T00:00:00Z"}`
	f.insert(t, "MemoryNodes", expired, session.Retention.TTL+15*24*time.Hour, row)
	f.insert(t, "MemoryNodes", fresh, time.Hour, row)

	integration := &Integration{context: memql.PluginContext{
		Concepts:          memorynodes.NewRegistry(map[string]*memorynodes.Concept{session.Name: session}),
		BunDB:             func() *bun.DB { return f.db },
		AdmitSourceRow:    memql.AdmitSourceRow,
		InvalidateConcept: func(concept string) { f.invalidated = append(f.invalidated, concept) },
	}}
	return f, integration, expired, fresh
}

func sweepReport(t *testing.T, nodes []memorynodes.MemoryNode) Report {
	t.Helper()
	require.Len(t, nodes, 1)
	var report Report
	require.NoError(t, json.Unmarshal(nodes[0].Payload, &report))
	return report
}

func TestAPersonRunningTheDocumentedSweepRetiresAnExpiredAccessSession(t *testing.T) {
	f, integration, _, _ := sessionSweepFixture(t)
	person := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "an-operator", Role: auth.RoleOwner})

	// The person is a client as far as the read clause goes, and not the cluster: the
	// premise that makes this a test of the elevation and not of a bypass already there.
	access, _ := auth.AccessFromContext(person)
	require.True(t, access.IsClusterOwner() && !access.Synthetic)

	t.Run("a non-owner is still refused, and nothing is touched", func(t *testing.T) {
		for name, who := range map[string]context.Context{
			"a writer":    auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "w", Role: auth.RoleWriter}),
			"an admin":    auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "a", Role: auth.RoleAdmin}),
			"a developer": auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "d", Role: auth.RoleDeveloper}),
			"a reader":    auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "r", Role: auth.RoleReader}),
			"anonymous":   auth.ContextWithAnonymousActor(context.Background()),
			"no caller":   context.Background(),
		} {
			for _, dry := range []bool{true, false} {
				nodes, err := integration.sweep(who, map[string]any{"dryRun": dry}, 0)
				require.EqualError(t, err, "concept retention requires a cluster owner", "%s dryRun=%v", name, dry)
				require.Nil(t, nodes)
			}
		}
		require.Equal(t, 2, f.count(t, "MemoryNodes"), "a refused caller changed the table")
	})

	t.Run("a dry run reports the expired row as eligible and changes nothing", func(t *testing.T) {
		nodes, err := integration.sweep(person, map[string]any{"dryRun": true}, 0)
		require.NoError(t, err, "a person running the documented sweep was refused a session row")
		report := sweepReport(t, nodes)
		require.Equal(t, 1, report.Eligible, "the row past its TTL is not reported as eligible: %+v", report)
		require.Zero(t, report.Deleted)
		require.Equal(t, 2, f.count(t, "MemoryNodes"), "a dry run deleted a row")
	})

	t.Run("the real pass retires it, keeps the fresh one and invalidates the reads", func(t *testing.T) {
		nodes, err := integration.sweep(person, map[string]any{}, 0)
		require.NoError(t, err)
		report := sweepReport(t, nodes)
		require.Equal(t, 1, report.Deleted, "%+v", report)
		require.Equal(t, 1, f.count(t, "MemoryNodes"), "the expired row is still there, or the fresh one went with it")
		require.Contains(t, f.invalidated, "v1:platform:siteAccessSession")
	})
}

// The hourly automation runs as the maintenance principal already, and the elevation
// must not change what it is: the same pass, the same result. (The authority test in
// plugin_test.go proves the executor hands the handler the maintenance principal and the
// owner floor; this proves the pass over a session row works from it.)
func TestTheScheduledSweepStillRetiresAnExpiredAccessSession(t *testing.T) {
	f, integration, _, _ := sessionSweepFixture(t)
	scheduled := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("retireDeclaredConceptVersions"))

	nodes, err := integration.sweep(scheduled, map[string]any{}, 0)
	require.NoError(t, err)
	require.Equal(t, 1, sweepReport(t, nodes).Deleted)
	require.Equal(t, 1, f.count(t, "MemoryNodes"))
}
