package memql

import (
	"encoding/json"
	"errors"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// WorkerToolResultError preserves a reported worker outcome while keeping the
// builtin failed: an automation must not journal a denied or partial effect as
// completed. Only the declared worker tool surface turns this typed receipt into
// an error-marked MCP result; ordinary execution still receives the error.
type WorkerToolResultError struct {
	Node  memorynodes.MemoryNode
	Cause error
}

func (e *WorkerToolResultError) Error() string { return e.Cause.Error() }
func (e *WorkerToolResultError) Unwrap() error { return e.Cause }

func workerToolErrorResult(tool *Tool, err error) *ToolCallResult {
	var reported *WorkerToolResultError
	if !declaredWorkerTool(tool) || !errors.As(err, &reported) || reported == nil {
		return nil
	}
	result := newExecuteResult(nil)
	result.setOutput(nodesToMap([]memorynodes.MemoryNode{reported.Node}))
	text, encodeErr := executeResultToToolJSON(result)
	if encodeErr != nil || WorkerToolResultFailure(text) == nil {
		return nil
	}
	return &ToolCallResult{IsError: true, Content: []ToolResultContent{{Type: "text", Text: text}}}
}

// A worker refusal is a completed protocol answer, not a failed transport.
// Only these declared native handlers return the worker action contract. Never
// interpret an arbitrary query, webhook, or tool's `ok:false` data as failure.
func declaredWorkerTool(tool *Tool) bool {
	if tool == nil || tool.Handler == nil || tool.Handler.Type != "function" {
		return false
	}
	return (tool.Name == "workerHost" && tool.Handler.FunctionName == "agentworkerDispatchHost") ||
		(tool.Name == "workerComputer" && tool.Handler.FunctionName == "agentworkerDispatchComputer")
}

// WorkerToolResultFailure decodes the declared worker result after the normal
// engine result codec. Callers must already have established a worker handler
// or an error-marked MCP result; this is not a generic JSON success detector.
// Preserve evidence in Details, including output after a possibly partial
// operation. A reported failure never licenses an automatic identical retry.
func WorkerToolResultFailure(text string) *StructuredToolError {
	type node struct {
		Concept string          `json:"concept"`
		Payload json.RawMessage `json:"payload"`
	}
	var envelope struct {
		Nodes []node                       `json:"nodes"`
		Data  []map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal([]byte(text), &envelope) != nil {
		return nil
	}
	nodes := envelope.Nodes
	for _, row := range envelope.Data {
		for _, raw := range row {
			var n node
			if json.Unmarshal(raw, &n) != nil {
				return nil
			}
			nodes = append(nodes, n)
		}
	}
	if len(nodes) != 1 || nodes[0].Concept != "integration:worker:result" {
		return nil
	}
	var result struct {
		OK           *bool  `json:"ok"`
		ErrorCode    string `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	}
	if json.Unmarshal(nodes[0].Payload, &result) != nil || result.OK == nil || *result.OK {
		return nil
	}
	kind := ToolErrorInternal
	if strings.HasPrefix(result.ErrorCode, "denied_") || result.ErrorCode == "kill_switch_engaged" {
		kind = ToolErrorPermission
	}
	message := result.ErrorMessage
	if message == "" {
		message = "Worker action failed"
	}
	return &StructuredToolError{Type: kind, Message: message, Retryable: false,
		UserFixable: false, Attempts: 1, Details: map[string]any{"workerResult": nodes[0].Payload}}
}
