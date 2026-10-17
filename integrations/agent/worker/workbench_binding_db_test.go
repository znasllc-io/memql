//go:build agent

package worker_test

import (
	"context"
	"database/sql"
	"sync"
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
	"github.com/znasllc-io/memql/core/id"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

func TestWorkbenchBindingDBChoosesOneMachineAcrossReplicas(t *testing.T) {
	reachable, err := dbtest.EnsureSchema(context.Background())
	require.NoError(t, err)
	if !reachable {
		dbtest.Unreachable(t, "workbench binding", dbtest.DSN(), nil)
		return
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	_, err = memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	var engines []*memql.MemQLEngine
	for range 2 {
		e, err := memql.New(db)
		require.NoError(t, err)
		require.NoError(t, e.Init(memorynodes.DefaultRegistry()))
		engines = append(engines, e)
	}
	owner := "v1:identity:user:" + id.NewShortId()
	ctx := worker.PermissionFlowPersonForTest(t, owner)
	write := auth.ContextWithInternalOrigin(ctx)
	call := func(e *memql.MemQLEngine, context context.Context, name string, args map[string]any) error {
		q, err := parser.RenderCall(name, args)
		if err != nil {
			return err
		}
		_, err = e.Execute(context, q)
		return err
	}
	goal, run := id.NewShortId(), id.NewShortId()
	require.NoError(t, call(engines[0], write, "createWorkGoal", map[string]any{"goalId": goal, "statement": "Use an isolated workbench", "origin": "user", "requestedVia": "ask"}))
	require.NoError(t, call(engines[0], write, "createWorkRun", map[string]any{"runId": run, "goalId": goal, "automationName": "workbench-binding-test", "templateFingerprint": "fixture", "mode": "live", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)}))
	stores := []*worker.EngineStore{{Engine: engines[0]}, {Engine: engines[1]}}
	results := make(chan string, 2)
	failures := make(chan error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index, store := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			candidate := worker.Candidate{RegistrationId: []string{"machine-a", "machine-b"}[index], MachineId: "physical", OwnerUserId: owner}
			binding, err := store.WorkbenchBinding(ctx, owner, run, &candidate)
			if err != nil {
				failures <- err
				return
			}
			results <- binding.RegistrationID
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	close(results)
	for err := range failures {
		require.NoError(t, err)
	}
	var chosen string
	for registration := range results {
		if chosen == "" {
			chosen = registration
		} else {
			require.Equal(t, chosen, registration, "both replicas must use one durable filesystem")
		}
	}
	require.NotEmpty(t, chosen)
	b, err := stores[1].WorkbenchBinding(ctx, owner, run, nil)
	require.NoError(t, err)
	require.Equal(t, chosen, b.RegistrationID)
	// A raw insert bypasses named mutations, so the concept must also guard it.
	_, err = engines[1].Execute(ctx, `insert("v1:workbench:executionBinding", id="forged-binding", payload={})`)
	require.ErrorContains(t, err, "@serverWritten requires internal origin")
	other := worker.PermissionFlowPersonForTest(t, "v1:identity:user:"+id.NewShortId())
	_, err = stores[1].WorkbenchBinding(other, owner, run, nil)
	require.Error(t, err, "a run ID is not authority")
	require.Error(t, call(engines[1], ctx, "bindWorkbenchExecution", map[string]any{"bindingId": id.NewShortId(), "runId": run, "resource": "fleet", "registrationId": "forged", "machineId": "physical", "machineOwnerUserId": owner, "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}))
}
