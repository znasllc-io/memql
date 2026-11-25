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
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// The evidence D6 waits on (network-aware fleet selection): a pipeline step one
// engine has sent to a machine is counted by another engine at once, from the
// activity row the sender wrote before the call left -- queued is not running,
// a finished step is gone, a model call is not a pipeline step, and another
// owner sees nothing.
func TestRunningPipelineStepsCountsAnotherReplicasRunningStep(t *testing.T) {
	reachable, err := dbtest.EnsureSchema(context.Background())
	require.NoError(t, err)
	if !reachable {
		dbtest.Unreachable(t, "running pipeline steps", dbtest.DSN(), nil)
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
	sender, planner := &EngineStore{Engine: engines[0]}, &EngineStore{Engine: engines[1]}
	count := func(who string) int {
		t.Helper()
		n, err := planner.RunningPipelineSteps(context.Background(), who, machine)
		require.NoError(t, err)
		return n
	}

	step := beginMachineActivity(ctx, sender, testLogger(), owner, machine, "tool", "workerHost."+PipelineStepAction, "run-1", time.Hour)
	model := beginMachineActivity(ctx, sender, testLogger(), owner, machine, "model", "local-model · chat", "", time.Hour)
	model.running()
	require.Equal(t, 0, count(owner), "a queued step and a running model call are not a running pipeline step")
	step.running()
	require.Equal(t, 1, count(owner), "the other engine's running step must be counted at once")
	require.Equal(t, 0, count("v1:identity:user:"+id.NewShortId()), "another owner must not see it")
	step.end("succeeded")
	require.Equal(t, 0, count(owner), "a finished step is no longer work in flight")
}
