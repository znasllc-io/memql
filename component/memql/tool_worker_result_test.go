package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func TestWorkerToolErrorPreservesReportedFailureWithoutCompletingBuiltin(t *testing.T) {
	tool := &Tool{Name: "workerHost", Handler: &ToolHandler{Type: "function", FunctionName: "agentworkerDispatchHost"}}
	for _, code := range []string{"denied_no_per_task_approval", "exec_timeout"} {
		cause := fmt.Errorf("worker action failed (%s)", code)
		reported := &WorkerToolResultError{Cause: cause, Node: memorynodes.MemoryNode{
			ID: "worker-call:1", Concept: "integration:worker:result", Type: memorynodes.NodeTypeObject,
			Payload: json.RawMessage(fmt.Sprintf(`{"ok":false,"errorCode":%q,"output":{"stdout":"partial output"}}`, code)),
		}}
		wrapped := fmt.Errorf("builtin: %w", reported)
		require.ErrorIs(t, wrapped, cause, "ordinary execution must still fail")
		result := workerToolErrorResult(tool, wrapped)
		require.NotNil(t, result)
		require.True(t, result.IsError)
		require.Len(t, result.Content, 1)
		failure := WorkerToolResultFailure(result.Content[0].Text)
		require.NotNil(t, failure)
		require.Contains(t, failure.JSON(), code)
		require.Contains(t, failure.JSON(), "partial output")
		require.False(t, failure.Retryable)
		if code == "denied_no_per_task_approval" {
			require.Equal(t, ToolErrorPermission, failure.Type)
		}
		other := *tool
		other.Name = "unrelatedTool"
		require.Nil(t, workerToolErrorResult(&other, wrapped))
		require.Nil(t, workerToolErrorResult(tool, cause), "a transport error carries no reported receipt")
		reported.Node.Concept = "user:note"
		require.Nil(t, workerToolErrorResult(tool, wrapped))
		reported.Node.Concept = "integration:worker:result"
		reported.Node.Payload = json.RawMessage(`{"ok":true}`)
		require.Nil(t, workerToolErrorResult(tool, wrapped))
	}
}

func workerResultText(t *testing.T, concept string, payload any, graph bool) string {
	t.Helper()
	node := map[string]any{"id": "worker-call:1", "concept": concept, "payload": payload}
	envelope := map[string]any{"data": []any{map[string]any{"worker-call:1": node}}}
	if graph {
		envelope = map[string]any{"nodes": []any{node}}
	}
	raw, err := json.Marshal(envelope)
	require.NoError(t, err)
	return string(raw)
}

func TestWorkerToolResultUsesOnlyDeclaredContract(t *testing.T) {
	for _, graph := range []bool{false, true} {
		for _, code := range []string{"denied_no_per_task_approval", "exec_timeout"} {
			payload := map[string]any{"ok": false, "errorCode": code, "errorMessage": "Exact refusal or uncertain outcome", "output": map[string]any{"stdout": "partial output"}, "bytesOut": 14}
			failure := WorkerToolResultFailure(workerResultText(t, "integration:worker:result", payload, graph))
			require.NotNil(t, failure)
			require.False(t, failure.Retryable)
			require.False(t, failure.UserFixable)
			require.Contains(t, failure.JSON(), "partial output")
			require.Equal(t, failure, ClassifyToolError(fmt.Errorf("adapter: %w", failure)), "typed outcome must retain its replay policy and complete output")
			calls := 0
			_, reported, attempts := (ToolRetryPolicy{MaxAttempts: 3}).ExecuteWithRetry(context.Background(), func(context.Context) (string, error) {
				calls++
				return "", failure
			})
			require.Equal(t, 1, calls, "a reported denial or uncertain worker outcome must not be dispatched again")
			require.Equal(t, 1, attempts)
			require.Equal(t, failure, reported)
		}
		for _, payload := range []any{map[string]any{"ok": true, "output": map[string]any{"ok": false}}, map[string]any{"errorCode": "denied"}, map[string]any{"ok": "false"}, nil} {
			require.Nil(t, WorkerToolResultFailure(workerResultText(t, "integration:worker:result", payload, graph)))
		}
		require.Nil(t, WorkerToolResultFailure(workerResultText(t, "user:note", map[string]any{"ok": false}, graph)))
	}
	for _, text := range []string{`{"ok":false}`, `{"data":[{"ok":false}]}`, `{"nodes":[]}`, `{"nodes":[{"concept":"integration:worker:result","payload":{"ok":false}},{"concept":"integration:worker:result","payload":{"ok":true}}]}`, `null`, `invalid`} {
		require.Nil(t, WorkerToolResultFailure(text))
	}
	exact := WorkerToolResultFailure(`{"nodes":[{"concept":"integration:worker:result","payload":{"ok":false,"output":{"count":9007199254740993}}}]}`)
	require.NotNil(t, exact)
	require.Contains(t, exact.JSON(), `"count":9007199254740993`, "worker evidence must not round through float64")
	for _, name := range []string{"Host", "Computer"} {
		tool := &Tool{Name: "worker" + name, Handler: &ToolHandler{Type: "function", FunctionName: "agentworkerDispatch" + name}}
		require.True(t, declaredWorkerTool(tool))
		tool.Handler.FunctionName = "unrelatedFunction"
		require.False(t, declaredWorkerTool(tool))
		tool.Handler.FunctionName = "agentworkerDispatch" + name
		tool.Name = "unrelatedTool"
		require.False(t, declaredWorkerTool(tool))
	}
}
