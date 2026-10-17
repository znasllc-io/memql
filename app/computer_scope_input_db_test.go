//go:build agent

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/common"
	permission "github.com/znasllc-io/memql/core/computerpermission"
	"github.com/znasllc-io/memql/core/id"
	worker "github.com/znasllc-io/memql/integrations/agent/worker"
)

type scopeInputStore struct {
	worker.Store // Any unexpected dispatch/store operation fails the test.
}

func (*scopeInputStore) RoutingPolicyForOwner(context.Context, string) (*worker.Policy, error) {
	return nil, nil
}
func (*scopeInputStore) WorkersForOwner(context.Context, string) ([]worker.Candidate, error) {
	return []worker.Candidate{{RegistrationId: "scope-input-machine", MachineId: "machine", Name: "Test machine", ConnectedNodeId: "agent-peer", Capabilities: []string{workerservice.CapabilityHeadless}, ActionContracts: workerservice.ActionContracts{permission.Contract: permission.Version}}}, nil
}

type scopeInputAuthority struct {
	finds int
	err   error
}

func (*scopeInputAuthority) CheckExecutionDestination(context.Context, string, string) error {
	return nil
}

func (a *scopeInputAuthority) Find(context.Context, permission.Request) (*permission.Grant, error) {
	a.finds++
	return nil, a.err
}
func (*scopeInputAuthority) Claim(context.Context, string, permission.Request, string, string) (*permission.Scope, error) {
	panic("requesting an approval must never consume permission")
}

// Exercise the installed DSL tool, builtin, worker handler and actual cognition
// adapter. A plain error at the permission-input boundary fails the model-facing
// classification assertions; no model or machine call is needed.
func TestWorkDBComputerScopeInputIsModelCorrectable(t *testing.T) {
	e := workTemplateDBEngine(t)
	registry := workerservice.NewRegistry(e.Logger, nil)
	d, err := worker.NewDispatcher(worker.Options{Logger: e.Logger, Registry: registry, Store: &scopeInputStore{}})
	require.NoError(t, err)
	authority := &scopeInputAuthority{}
	d.SetComputerPermissions(authority)
	i := worker.NewIntegration(d, registry, e, e.Logger)
	approvalCalls := 0
	var subject map[string]any
	// A sentinel at the approval boundary tests propagation, without inventing
	// a durable approval or invoking a machine in this argument-validation test.
	wait := &work.HumanWait{ApprovalID: "scope-input-sentinel"}
	i.ScopeRequester = func(_ context.Context, _, _ string, s map[string]any) (string, error) {
		approvalCalls++
		subject = s
		return "", wait
	}
	require.NoError(t, e.RegisterIntegration(i))
	adapter := &CognitionEngineAdapter{Engine: e}
	owner, run := id.NewShortId(), id.NewShortId()
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	ctx = memql.WithActingAgentRole(ctx, "assistant")
	ctx = common.ContextWithRun(ctx, common.RunContext{RunId: run, GoalId: id.NewShortId(), OwnerUserId: owner, StepKey: "read"})
	ctx = common.ContextWithToolDefaults(ctx, map[string]any{"ownerUserId": owner, "agentId": id.NewShortId()})
	call := func(operation any) error {
		_, err := adapter.ExecuteToolByName(ctx, "requestComputerUseScope", map[string]any{
			"intent": "inspect", "requestedScope": "observe", "summary": "Read the selected file", "operation": operation,
		})
		return err
	}
	read := func(action, directory, path string) map[string]any {
		op := map[string]any{"tool": "workerHost", "action": action, "args": map[string]any{"path": path}}
		if directory != "" {
			op["directory"] = directory
		}
		return op
	}
	exec := func(args map[string]any) map[string]any {
		return map[string]any{"tool": "workerHost", "action": "exec", "args": args}
	}
	for _, tc := range []struct {
		name, field, schemaDetail string
		operation                 any
	}{
		{"read missing directory", "operation.directory", "", read("fs_read", "", "/docs/file")},
		{"list relative directory", "operation.directory", "", read("fs_list", "docs", "/docs/file")},
		{"stat outside directory", "operation.args.path", "", read("fs_stat", "/docs", "/elsewhere/file")},
		{"read relative path", "operation.args.path", "", read("fs_read", "/docs", "file")},
		{"command missing cwd", "operation.args", "", exec(map[string]any{"cmd": "pwd"})},
		{"command hidden option", "", "stdin", exec(map[string]any{"cmd": "pwd", "cwd": "/docs", "stdin": "data"})},
		{"command timeout", "operation.args", "", exec(map[string]any{"cmd": "pwd", "cwd": "/docs", "timeoutSec": 601})},
		{"unsupported operation", "", "/operation/action", read("fs_write", "/docs", "/docs/file")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := call(tc.operation)
			require.Error(t, err)
			classified := memql.ClassifyToolError(err)
			require.Equal(t, memql.ToolErrorValidation, classified.Type, "invalid tool input must not ask for a platform retry: %v", err)
			require.False(t, classified.Retryable)
			require.True(t, classified.UserFixable)
			if tc.schemaDetail != "" {
				require.ErrorContains(t, err, tc.schemaDetail, "the input schema must explain the rejected value")
			} else {
				require.Equal(t, tc.field, classified.Details["field"])
			}
			require.Zero(t, authority.finds, "input refusal must precede receipt lookup")
			require.Zero(t, approvalCalls, "input refusal must not ask for broader authority")
		})
	}

	t.Run("receipt failure remains its original error", func(t *testing.T) {
		original := errors.New("permission receipt store unavailable")
		authority.err = original
		err := call(read("fs_read", "/docs", "/docs/file"))
		require.ErrorIs(t, err, original)
		require.NotEqual(t, memql.ToolErrorValidation, memql.ClassifyToolError(err).Type)
		require.Equal(t, 1, authority.finds)
		require.Zero(t, approvalCalls)
		authority.err = nil
	})
	t.Run("verification unavailable is not input validation", func(t *testing.T) {
		d.SetComputerPermissions(nil)
		err := call(read("fs_read", "/docs", "/docs/file"))
		require.ErrorContains(t, err, "computer permission verification is unavailable")
		require.NotEqual(t, memql.ToolErrorValidation, memql.ClassifyToolError(err).Type)
		require.Zero(t, approvalCalls)
		d.SetComputerPermissions(authority)
	})
	t.Run("corrected input retains exact approval suspension", func(t *testing.T) {
		for _, operation := range []map[string]any{read("fs_read", "/docs", "/docs/file"), exec(map[string]any{"cmd": "pwd", "cwd": "/docs"})} {
			err := call(operation)
			require.ErrorIs(t, err, wait)
			require.NotContains(t, subject, "scope", "typed approval must never grant legacy broad access")
			scope, parseErr := permission.Parse(subject["computerPermission"])
			require.NoError(t, parseErr)
			require.Equal(t, operation["action"], scope.Action)
			require.Equal(t, "scope-input-machine", scope.RegistrationID)
		}
		require.Equal(t, 2, approvalCalls)
	})
}
