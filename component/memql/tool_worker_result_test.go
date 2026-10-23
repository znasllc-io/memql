package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

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

func TestWorkerToolExecutionFailureKeepsOnlyTheDeclaredReceiptInBand(t *testing.T) {
	payload := json.RawMessage(`{"ok":false,"errorCode":"exec_timeout","errorMessage":"The command outcome is uncertain","output":{"count":9007199254740993,"stdout":"partial output"}}`)
	nodes := []memorynodes.MemoryNode{{ID: "worker-call:1", Concept: "integration:worker:result", Payload: payload}}
	err := WorkerToolExecutionFailure("workerHost", "exec", nodes)
	require.Error(t, err, "a failed builtin must still fail an automation")
	require.Contains(t, err.Error(), "exec_timeout")
	wrapped := fmt.Errorf("function: %w", err)
	tool := &Tool{Name: "workerHost", Handler: &ToolHandler{Type: "function", FunctionName: "agentworkerDispatchHost"}}
	result := workerToolFailureResult(tool, wrapped)
	require.NotNil(t, result)
	require.True(t, result.IsError)
	require.Len(t, result.Content, 1)
	require.Contains(t, result.Content[0].Text, `"count":9007199254740993`)
	require.Contains(t, result.Content[0].Text, "partial output")
	require.False(t, ClassifyToolError(wrapped).Retryable)
	payload[0] = '!' // The recorded answer owns its bytes.
	require.Contains(t, workerToolFailureResult(tool, err).Content[0].Text, `"ok":false`)
	require.Nil(t, workerToolFailureResult(tool, fmt.Errorf("transport unavailable")))
	require.Nil(t, workerToolFailureResult(tool, ClassifyToolError(wrapped)), "an arbitrary structured error is not a worker receipt")
	other := &Tool{Name: "workerComputer", Handler: &ToolHandler{Type: "function", FunctionName: "agentworkerDispatchComputer"}}
	require.Nil(t, workerToolFailureResult(other, err), "a receipt cannot cross tool identities")
	tool.Handler.FunctionName = "unrelatedFunction"
	require.Nil(t, workerToolFailureResult(tool, err))
	tool.Handler.FunctionName = "agentworkerDispatchHost"
	for _, bad := range [][]memorynodes.MemoryNode{nil, nodes, {{Concept: "user:note", Payload: json.RawMessage(`{"ok":false}`)}}, {{Concept: "integration:worker:result", Payload: json.RawMessage(`{"ok":true}`)}}} {
		invalid := WorkerToolExecutionFailure("workerHost", "exec", bad)
		require.Nil(t, workerToolFailureResult(tool, invalid))
		require.False(t, ClassifyToolError(invalid).Retryable, "missing evidence cannot license another dispatch")
	}
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
