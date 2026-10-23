//go:build agent

package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

type refusedWorkerStore struct {
	worker.Store
	rows []workerservice.InvocationRow
}

func (*refusedWorkerStore) UserPreferences(context.Context, string) (worker.Preferences, error) {
	return worker.Preferences{}, nil
}
func (*refusedWorkerStore) AgentAuthorization(context.Context, string, string) (*worker.Authorization, error) {
	return &worker.Authorization{ComputerUseScope: "full"}, nil
}
func (*refusedWorkerStore) WorkComputerScope(context.Context, worker.Request, time.Time) (string, error) {
	return "", nil
}
func (s *refusedWorkerStore) WriteInvocation(_ context.Context, row workerservice.InvocationRow) error {
	s.rows = append(s.rows, row)
	return nil
}

// Two independent engines run the installed worker tool and its actual dispatch
// gate. No model, machine, permission or successful result is substituted.
func TestWorkDBWorkerRefusalReachesCanonicalToolAndAdapter(t *testing.T) {
	for range 2 {
		e := workTemplateDBEngine(t)
		registry := workerservice.NewRegistry(e.Logger, nil)
		store := &refusedWorkerStore{}
		d, err := worker.NewDispatcher(worker.Options{Logger: e.Logger, Registry: registry, Store: store})
		require.NoError(t, err)
		require.NoError(t, e.RegisterIntegration(worker.NewIntegration(d, registry, e, e.Logger)))
		ctx := auth.ContextWithUserActor(context.Background(), "worker-result-owner")
		ctx = memql.WithActingAgentRole(ctx, "assistant")
		ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "worker-result-run", GoalId: "worker-result-goal", OwnerUserId: "worker-result-owner", StepKey: "step"})
		ctx = common.ContextWithToolDefaults(ctx, map[string]any{"ownerUserId": "worker-result-owner", "agentId": "worker-result-agent", "runId": "worker-result-run"})
		args := map[string]any{"action": "exec", "args": map[string]any{"cmd": "printf read-only"}}
		raw, err := e.ExecuteToolByName(ctx, "workerHost", args)
		require.NoError(t, err, "reported refusal must remain in-band and avoid transport retry")
		var result memql.ToolCallResult
		require.NoError(t, json.Unmarshal([]byte(raw), &result))
		require.True(t, result.IsError, "installed worker denial was mislabeled as successful")
		require.Len(t, result.Content, 1)
		require.Contains(t, result.Content[0].Text, "denied_no_per_task_approval")
		adapter := &CognitionEngineAdapter{Engine: e}
		out, err := adapter.ExecuteToolByName(ctx, "workerHost", args)
		require.Error(t, err)
		require.Contains(t, out, "denied_no_per_task_approval", "the original worker evidence remains available alongside the typed failure")
		failure := memql.ClassifyToolError(err)
		require.Equal(t, memql.ToolErrorPermission, failure.Type)
		require.False(t, failure.Retryable)
		require.Contains(t, failure.JSON(), "denied_no_per_task_approval")
		_, err = e.Execute(ctx, `agentworkerDispatchHost(ownerUserId: "worker-result-owner", agentId: "worker-result-agent", runId: "worker-result-run", action: "exec", args: {"cmd":"printf read-only"})`)
		require.Error(t, err, "the native builtin must fail so an automation cannot record a successful effect")
		require.Contains(t, err.Error(), "denied_no_per_task_approval")
		require.False(t, memql.ClassifyToolError(err).Retryable)
		require.Len(t, store.rows, 3, "one gate answer per deliberate invocation, no retries")
		for _, row := range store.rows {
			require.Empty(t, row.WorkerId)
			require.Equal(t, "denied_by_policy", row.Outcome)
		}
	}
}
