package retention

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// status_rows_db_test.go -- the hourly sweep over the two status rows whose
// superseded versions expire after a day (v1:pipelines:run, v1:worker:appSession).
//
// On production on 2026-11-06 a pipeline run row was rewritten ~127 times per run,
// every version carrying the run's ~14 KB policySnapshot, and an app session every two
// seconds at ~10 KB a version: together 1.9 GB in one week, none of it ever read
// again. The declarations live on the concepts (dsl/pipelines/concepts.memql,
// dsl/worker/concepts.memql) and are pinned by
// component/memql/status_row_retention_test.go. These tests drive the HANDLER the
// hourly automation calls, under the maintenance principal it runs as, against the
// real concepts and the real per-row admission: a tier that did not admit the sweeper
// would refuse the whole batch ("row admission refused; records preserved") and keep
// every version for good, which is the failure a declaration-only test cannot see.

func statusRowSweep(t *testing.T, concept string) (*fixture, *Integration) {
	t.Helper()
	f := newFixture(t)
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	c, err := memorynodes.Get(concept)
	require.NoError(t, err)
	require.NotNil(t, c.Retention, "%s declares no retention, so this test measures nothing", concept)
	f.concept = c
	integration := &Integration{context: memql.PluginContext{
		Concepts:          memorynodes.NewRegistry(map[string]*memorynodes.Concept{c.Name: c}),
		BunDB:             func() *bun.DB { return f.db },
		AdmitSourceRow:    memql.AdmitSourceRow,
		InvalidateConcept: func(concept string) { f.invalidated = append(f.invalidated, concept) },
	}}
	return f, integration
}

func (f *fixture) versionsOf(t *testing.T, id string) []time.Time {
	t.Helper()
	var at []time.Time
	if err := f.db.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr(`"createdAt"`).Where("id = ?", id).OrderExpr(`"createdAt"`).Scan(context.Background(), &at); err != nil {
		t.Fatal(err)
	}
	return at
}

func TestTheSweepKeepsOnlyTheNewestVersionOfAStatusRowPastADay(t *testing.T) {
	for concept, payload := range map[string]func(n int) string{
		"v1:pipelines:run": func(n int) string {
			return fmt.Sprintf(`{"ownerUserId":"v1:identity:user:the-owner","accountId":"v1:accounts:account:a","pipelineId":"v1:pipelines:pipeline:p","repository":"o/r","sha":"abc","status":"in_progress","driverHeartbeatAt":"2026-11-0%dT00:00:00Z","policySnapshot":{"sources":"unchanged"}}`, n)
		},
		"v1:worker:appSession": func(n int) string {
			return fmt.Sprintf(`{"ownerUserId":"v1:identity:user:the-owner","workerId":"v1:worker:registration:w","status":"running","recordedSteps":%d}`, n)
		},
	} {
		t.Run(concept, func(t *testing.T) {
			f, integration := statusRowSweep(t, concept)
			busy, quiet := concept+":busy", concept+":quiet"

			// A row still being driven: two versions past a day, two younger.
			f.insert(t, "MemoryNodes", busy, 50*time.Hour, payload(1))
			f.insert(t, "MemoryNodes", busy, 30*time.Hour, payload(2))
			f.insert(t, "MemoryNodes", busy, 2*time.Hour, payload(3))
			f.insert(t, "MemoryNodes", busy, time.Hour, payload(4))
			// A finished row whose only version is a week old.
			f.insert(t, "MemoryNodes", quiet, 7*24*time.Hour, payload(5))

			scheduled := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor(auth.AutomationOriginEngine, "retireDeclaredConceptVersions"))
			nodes, err := integration.sweep(scheduled, map[string]any{}, 0)
			require.NoError(t, err, "the scheduled sweep was refused a %s row: its tier does not admit the maintenance principal", concept)
			report := sweepReport(t, nodes)

			require.Equal(t, 2, report.Deleted, "only the two superseded versions older than a day may go: %+v", report)
			require.Zero(t, report.Archived, "archive=\"none\": a superseded status version is not evidence worth a verified copy")
			require.Len(t, f.versionsOf(t, busy), 2, "the busy row lost a version younger than a day, or kept an expired one")
			require.Len(t, f.versionsOf(t, quiet), 1, "keepLatest: the newest version of a row never expires, however old")
			require.Contains(t, f.invalidated, concept)

			// The second pass finds nothing more to do: the two younger versions wait for
			// their day, and the newest of each row waits forever.
			nodes, err = integration.sweep(scheduled, map[string]any{}, 0)
			require.NoError(t, err)
			require.Zero(t, sweepReport(t, nodes).Deleted)
		})
	}
}
