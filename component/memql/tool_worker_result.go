package memql

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// WorkerToolExecutionFailure keeps a completed worker receipt attached to a
// failed builtin. Automations still receive an error; the declared worker tool
// surface can return the same refusal in-band without licensing a transport retry.
func WorkerToolExecutionFailure(tool, action string, nodes []memorynodes.MemoryNode) error {
	if len(nodes) != 1 || nodes[0].Concept != "integration:worker:result" {
		return invalidWorkerReceipt(fmt.Sprintf("worker %s %s failed without a valid receipt", tool, action))
	}
	text, err := json.Marshal(map[string]any{"nodes": []map[string]any{{
		"id": BareShortId(nodes[0].ID), "concept": nodes[0].Concept, "payload": json.RawMessage(nodes[0].Payload),
	}}})
	if err != nil {
		return invalidWorkerReceipt(fmt.Sprintf("worker %s %s failed: encode receipt: %v", tool, action, err))
	}
	failure := WorkerToolResultFailure(string(text))
	if failure == nil {
		return invalidWorkerReceipt(fmt.Sprintf("worker %s %s failed without a failed receipt", tool, action))
	}
	var result struct {
		ErrorCode    string `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
	}
	_ = json.Unmarshal(nodes[0].Payload, &result) // The receipt was validated above.
	return &workerToolExecutionFailure{tool: tool, text: string(text), failure: failure,
		message: fmt.Sprintf("worker %s %s failed (%s): %s", tool, action, result.ErrorCode, result.ErrorMessage)}
}

func invalidWorkerReceipt(message string) error {
	// Missing evidence cannot authorize another potentially partial operation.
	return &StructuredToolError{Type: ToolErrorInternal, Message: message, Retryable: false, UserFixable: false, Attempts: 1}
}

type workerToolExecutionFailure struct {
	tool, text, message string
	failure             *StructuredToolError
}

func (e *workerToolExecutionFailure) Error() string { return e.message }

func (e *workerToolExecutionFailure) Unwrap() error { return e.failure }

func workerToolFailureResult(tool *Tool, err error) *ToolCallResult {
	var failure *workerToolExecutionFailure
	if !declaredWorkerTool(tool) || !errors.As(err, &failure) || failure.tool != tool.Name {
		return nil
	}
	return &ToolCallResult{IsError: true, Content: []ToolResultContent{{Type: "text", Text: failure.text}}}
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
