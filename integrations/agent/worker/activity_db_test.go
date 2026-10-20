//go:build agent

package worker

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/id"
)

// Persist on one engine and read through a different engine, without either
// engine's local result cache. This also exercises DSL validation and owner
// scoping, which a recording activityStore cannot establish.
func TestMachineActivityPersistedAcrossEngines(t *testing.T) {
	reachable, err := dbtest.EnsureSchema(context.Background())
	require.NoError(t, err)
	if !reachable {
		dbtest.Unreachable(t, "machine activity", dbtest.DSN(), nil)
		return
	}
	_, err = memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	engines := make([]*memql.MemQLEngine, 2)
	for i := range engines {
		engines[i], err = memql.New(db)
		require.NoError(t, err)
		require.NoError(t, engines[i].Init(memorynodes.DefaultRegistry()))
	}
	owner, machine := "v1:identity:user:"+id.NewShortId(), id.NewShortId()
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	activity := beginMachineActivity(ctx, &EngineStore{Engine: engines[0]}, testLogger(), owner, machine, "model", "local-model · chat", "", time.Minute)
	read := func(ctx context.Context, query string, args map[string]any) []map[string]any {
		call, e := parser.RenderCall(query, args)
		require.NoError(t, e)
		result, e := engines[1].Execute(memql.ContextWithFreshRead(ctx), "query "+call)
		require.NoError(t, e)
		return memql.MaterializeRows(result.OutputPayload())
	}
	for _, state := range []string{"queued", "running", "succeeded"} {
		if state == "running" {
			activity.running()
		}
		if state == "succeeded" {
			activity.end(state)
		}
		rows := read(ctx, "machineActivity", map[string]any{"workerId": machine})
		require.Len(t, rows, 1)
		require.Equal(t, state, rows[0]["status"])
		active := read(ctx, "activeMachineActivity", map[string]any{"workerId": machine})
		if state == "succeeded" {
			require.Empty(t, active)
		} else {
			require.Len(t, active, 1)
		}
	}
	// Router receipts store the machine as a canonical execution surface;
	// the browser sends only a bare registration id.
	call, err := parser.RenderCall("recordRouterCall", map[string]any{
		"callId": id.NewShortId(), "requestId": id.NewShortId(), "userId": owner,
		"vendor": "local", "model": "earlier-model", "providerName": "fleet:earlier-model",
		"totalDurationMs": 4, "outcome": "ok", "executionSurface": "fleet:v1:worker:registration:" + machine,
	})
	require.NoError(t, err)
	_, err = engines[0].Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
	require.NoError(t, err)
	history := read(ctx, "myModelCallsOnMachine", map[string]any{"workerId": machine})
	require.Len(t, history, 1)
	require.Equal(t, "earlier-model", history[0]["model"])

	other := auth.ContextWithUserActor(context.Background(), "v1:identity:user:"+id.NewShortId())
	require.Empty(t, read(other, "myModelCallsOnMachine", map[string]any{"workerId": machine}))
	require.Empty(t, read(other, "machineActivity", map[string]any{"workerId": machine}))
	require.Empty(t, read(other, "machineActivityById", map[string]any{"activityId": activity.id}))

	// The isolated Fleet command succeeds before its receipt is persisted.
	// Persist the real writer's payload, then read it on the other engine to
	// cover the schema and owner boundary that in-memory dispatcher tests miss.
	runID := id.NewShortId()
	invocation := workerservice.InvocationRow{
		ID: id.NewShortId(), OwnerUserId: owner, WorkerId: machine,
		AgentId: id.NewShortId(), RunId: runID, CorrelationId: id.NewShortId(),
		Tool: WorkbenchTool, Action: "exec", Outcome: "success",
		StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(),
		OutputPreview: "fleet activity smoke\n65532\n",
	}
	require.NoError(t, (&EngineStore{Engine: engines[0]}).WriteInvocation(ctx, invocation))
	receipts := read(ctx, "invocationsForRun", map[string]any{"runId": runID})
	require.Len(t, receipts, 1)
	require.Equal(t, WorkbenchTool, receipts[0]["tool"])
	require.Equal(t, "success", receipts[0]["outcome"])
	require.Equal(t, invocation.OutputPreview, receipts[0]["outputPreview"])
	require.Empty(t, read(other, "invocationsForRun", map[string]any{"runId": runID}))
}
