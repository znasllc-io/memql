package compose

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	work "github.com/znasllc-io/memql/integrations/work"
)

// A surviving replica abandons a real saved Materializer run with no state from
// the accepting node. These are the exact owner reads the Materializer joins;
// the frontend regression exercises their retained graph-event path separately.
func TestMaterializeDB_AbandonedRunIsReadableOnAnotherReplica(t *testing.T) {
	accepting := materializeDBEngine(t)
	surviving := materializeDBEngine(t)
	accept := New(accepting, accepting.Logger)
	accept.SetGoalOpener(gatedWork(t, accepting))
	owner := fmt.Sprintf("materializer-abandoned-%d", time.Now().UnixNano())
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	accepted, err := accept.materialize(ctx, owner, "", materializeDraft())
	if err != nil {
		t.Fatal(err)
	}
	compositionID, runID := stringOf(accepted["compositionId"]), stringOf(accepted["runId"])
	if err := accept.store().updateCompositionState(ctx, map[string]any{"compositionId": compositionID, "status": "composing"}); err != nil {
		t.Fatal(err)
	}
	if err := accept.store().writeInternal(ctx, "mutation "+call("updateWorkRun", map[string]any{
		"runId": runID, "status": "running", "nodeId": "agent-that-stopped",
		"heartbeatAt": time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano),
	})); err != nil {
		t.Fatal(err)
	}

	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	var sweeper *work.Integration
	for _, registration := range memql.RegisteredPlugins() {
		if registration.Name != "work" {
			continue
		}
		provider, err := registration.Factory(memql.PluginContext{
			Engine: surviving, Logger: surviving.Logger,
			BunDB: func() *bun.DB { return db }, GateDB: composeGatePool(t), AdmitSourceRow: memql.AdmitSourceRow,
		})
		if err != nil {
			t.Fatal(err)
		}
		sweeper = provider.(*work.Integration)
	}
	if sweeper == nil {
		t.Fatal("work plugin is not registered")
	}
	// The real admission gate scopes the shared test database to this unique
	// owner. No other test's in-flight runs may be changed by this sweep.
	result, err := sweeper.SweepWaiting(ctx, time.Minute)
	if err != nil || result.Abandoned != 1 {
		t.Fatalf("sweep result %+v: %v", result, err)
	}
	reader := New(surviving, surviving.Logger)
	run, err := one(reader.store().query(ctx, "query "+call("workRunForOwner", map[string]any{"runId": runID})))
	if err != nil || run == nil || run["status"] != "abandoned" || !strings.Contains(stringOf(run["errorMessage"]), "agent-that-stopped") {
		t.Fatalf("owner cannot read the surviving replica's abandonment: %+v %v", run, err)
	}
	composition, err := reader.store().compositionById(ctx, compositionID)
	if err != nil || composition["status"] != "composing" {
		t.Fatalf("interrupted pipeline's last persisted progress: %+v %v", composition, err)
	}
	// Both shaped wire rows expose bare matching identities, including owner
	// and goal, so the client need not parse canonical IDs to join the records.
	for _, pair := range [][2]string{{"runId", "id"}, {"goalId", "goalId"}, {"ownerUserId", "ownerUserId"}} {
		if composition[pair[0]] == "" || composition[pair[0]] != run[pair[1]] {
			t.Fatalf("wire binding %v differs: composition=%+v run=%+v", pair, composition, run)
		}
	}
	other := auth.ContextWithUserActor(context.Background(), owner+"-other")
	hidden, err := reader.store().query(other, "query "+call("workRunForOwner", map[string]any{"runId": runID}))
	if err != nil || len(hidden) != 0 {
		t.Fatalf("another owner can read the interrupted run: %+v %v", hidden, err)
	}
}
