package memql

import (
	"context"
	"encoding/json"
	"time"

	"github.com/visionarys-io/memql/component/bus"
	busv1 "github.com/visionarys-io/memql/component/bus/gen"
	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	"google.golang.org/protobuf/types/known/structpb"
)

// SetWiring configures the bus wiring for channel-based communication.
// When set, the engine starts a goroutine that reads from EngineRequests
// and dispatches to the appropriate handler.
func (e *MemQLEngine) SetWiring(w *bus.Wiring) {
	e.wiring = w
}

// runBus reads from the EngineRequests channel and dispatches requests
// to the appropriate engine method. Runs as a goroutine alongside the
// main lifecycle run loop.
func (e *MemQLEngine) runBus(ctx context.Context) {
	if e.wiring == nil {
		return
	}

	e.Logger.Info("engine bus handler started")
	defer e.Logger.Info("engine bus handler stopped")

	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-e.wiring.EngineRequests.C:
			if !ok {
				return
			}
			e.handleBusRequest(ctx, req)
		}
	}
}

// handleBusRequest dispatches a single bus request to the appropriate handler.
func (e *MemQLEngine) handleBusRequest(ctx context.Context, req bus.Request) {
	if req.Msg == nil {
		return
	}

	// Use the correlation ID for request-scoped logging
	correlationId := req.Msg.CorrelationId

	switch req.Msg.Payload.(type) {
	case *busv1.InternalMessage_EngineExecute:
		e.handleExecuteRequest(ctx, req, correlationId)
	case *busv1.InternalMessage_EngineRenderPrompt:
		e.handleRenderPromptRequest(ctx, req, correlationId)
	case *busv1.InternalMessage_EngineToolExec:
		e.handleToolExecRequest(ctx, req, correlationId)
	case *busv1.InternalMessage_EngineVarResolve:
		e.handleVariableResolveRequest(ctx, req, correlationId)
	default:
		resp := bus.NewCorrelatedMessage(req.Msg)
		resp.Payload = &busv1.InternalMessage_EngineExecuteResponse{
			EngineExecuteResponse: &busv1.EngineExecuteResponse{
				Success: false,
				Error:   "unknown engine request type",
			},
		}
		req.Reply(resp)
	}
}

// handleExecuteRequest processes an EngineExecuteRequest.
func (e *MemQLEngine) handleExecuteRequest(ctx context.Context, req bus.Request, correlationId string) {
	execReq := req.Msg.GetEngineExecute()
	startTime := time.Now()

	result, err := e.Execute(ctx, execReq.Query)

	resp := bus.NewCorrelatedMessage(req.Msg)
	if err != nil {
		e.Logger.Error("bus execute failed",
			"correlation_id", correlationId,
			"query_prefix", truncate(execReq.Query, 80),
			"error", err,
		)
		resp.Payload = &busv1.InternalMessage_EngineExecuteResponse{
			EngineExecuteResponse: &busv1.EngineExecuteResponse{
				Success: false,
				Error:   err.Error(),
				TookMs:  time.Since(startTime).Milliseconds(),
			},
		}
		req.Reply(resp)
		return
	}

	// Serialize the result to protobuf Value
	resultValue, err := marshalExecuteResult(result)
	if err != nil {
		e.Logger.Error("bus execute result marshal failed",
			"correlation_id", correlationId,
			"error", err,
		)
		resp.Payload = &busv1.InternalMessage_EngineExecuteResponse{
			EngineExecuteResponse: &busv1.EngineExecuteResponse{
				Success: false,
				Error:   "failed to marshal result: " + err.Error(),
				TookMs:  time.Since(startTime).Milliseconds(),
			},
		}
		req.Reply(resp)
		return
	}

	resp.Payload = &busv1.InternalMessage_EngineExecuteResponse{
		EngineExecuteResponse: &busv1.EngineExecuteResponse{
			Success: true,
			Result:  resultValue,
			TookMs:  time.Since(startTime).Milliseconds(),
		},
	}
	req.Reply(resp)
}

// handleRenderPromptRequest processes an EngineRenderPromptRequest.
func (e *MemQLEngine) handleRenderPromptRequest(_ context.Context, req bus.Request, correlationId string) {
	promptReq := req.Msg.GetEngineRenderPrompt()

	data := make(map[string]any)
	if promptReq.Data != nil {
		data = promptReq.Data.AsMap()
	}

	rendered, err := e.RenderPrompt(promptReq.TemplateId, data)

	resp := bus.NewCorrelatedMessage(req.Msg)
	if err != nil {
		e.Logger.Error("bus render prompt failed",
			"correlation_id", correlationId,
			"template_id", promptReq.TemplateId,
			"error", err,
		)
		resp.Payload = &busv1.InternalMessage_EngineRenderPromptResponse{
			EngineRenderPromptResponse: &busv1.EngineRenderPromptResponse{
				Success: false,
				Error:   err.Error(),
			},
		}
		req.Reply(resp)
		return
	}

	resp.Payload = &busv1.InternalMessage_EngineRenderPromptResponse{
		EngineRenderPromptResponse: &busv1.EngineRenderPromptResponse{
			Success:  true,
			Rendered: rendered,
		},
	}
	req.Reply(resp)
}

// handleToolExecRequest processes an EngineToolExecRequest.
func (e *MemQLEngine) handleToolExecRequest(ctx context.Context, req bus.Request, correlationId string) {
	toolReq := req.Msg.GetEngineToolExec()

	args := make(map[string]any)
	if toolReq.Arguments != nil {
		args = toolReq.Arguments.AsMap()
	}

	result, err := e.ExecuteToolByName(ctx, toolReq.ToolName, args)

	resp := bus.NewCorrelatedMessage(req.Msg)
	if err != nil {
		e.Logger.Error("bus tool exec failed",
			"correlation_id", correlationId,
			"tool_name", toolReq.ToolName,
			"error", err,
		)
		resp.Payload = &busv1.InternalMessage_EngineToolExecResponse{
			EngineToolExecResponse: &busv1.EngineToolExecResponse{
				Success: false,
				Error:   err.Error(),
			},
		}
		req.Reply(resp)
		return
	}

	resp.Payload = &busv1.InternalMessage_EngineToolExecResponse{
		EngineToolExecResponse: &busv1.EngineToolExecResponse{
			Success: true,
			Result:  result,
		},
	}
	req.Reply(resp)
}

// handleVariableResolveRequest processes an EngineVariableResolveRequest.
func (e *MemQLEngine) handleVariableResolveRequest(ctx context.Context, req bus.Request, correlationId string) {
	varReq := req.Msg.GetEngineVarResolve()

	var value string
	var err error

	if varReq.DefaultValue != "" {
		value = e.ResolveVariableWithDefault(ctx, varReq.VariableName, varReq.DefaultValue)
	} else {
		value, err = e.ResolveVariable(ctx, varReq.VariableName)
	}

	resp := bus.NewCorrelatedMessage(req.Msg)
	if err != nil {
		resp.Payload = &busv1.InternalMessage_EngineVarResolveResponse{
			EngineVarResolveResponse: &busv1.EngineVariableResolveResponse{
				Success: false,
				Error:   err.Error(),
			},
		}
		req.Reply(resp)
		return
	}

	resp.Payload = &busv1.InternalMessage_EngineVarResolveResponse{
		EngineVarResolveResponse: &busv1.EngineVariableResolveResponse{
			Success: true,
			Value:   value,
		},
	}
	req.Reply(resp)
}

// marshalExecuteResult converts an ExecuteResult to a protobuf Value.
func marshalExecuteResult(result *ExecuteResult) (*structpb.Value, error) {
	if result == nil {
		return structpb.NewNullValue(), nil
	}

	// Convert result to a JSON-friendly map and then to structpb.Value
	resultMap := make(map[string]any)

	if result.Bundle != nil {
		resultMap["bundle"] = result.Bundle
	}
	if result.Meta != nil {
		resultMap["meta"] = map[string]any{
			"count":   result.Meta.Count,
			"tookMs":  result.Meta.TookMs,
			"version": result.Meta.Version,
		}
	}

	// Use JSON as intermediary for complex struct conversion
	jsonBytes, err := json.Marshal(resultMap)
	if err != nil {
		return nil, err
	}

	var generic any
	if err := json.Unmarshal(jsonBytes, &generic); err != nil {
		return nil, err
	}

	return structpb.NewValue(generic)
}

// runIntegrationDispatcher reads from the IntegrationRequests channel and
// routes each request to the registered capability handler.
// Runs as a goroutine alongside the bus handler.
func (e *MemQLEngine) runIntegrationDispatcher(ctx context.Context) {
	if e.wiring == nil {
		return
	}

	e.Logger.Info("integration dispatcher started")
	defer e.Logger.Info("integration dispatcher stopped")

	for {
		select {
		case <-ctx.Done():
			return
		case req, ok := <-e.wiring.IntegrationRequests.C:
			if !ok {
				return
			}
			e.handleIntegrationRequest(ctx, req)
		}
	}
}

// handleIntegrationRequest dispatches an IntegrationDispatchRequest to the
// registered capability handler.
func (e *MemQLEngine) handleIntegrationRequest(ctx context.Context, req bus.Request) {
	if req.Msg == nil {
		return
	}

	dispatchReq, ok := req.Msg.Payload.(*busv1.InternalMessage_IntegrationDispatch)
	if !ok || dispatchReq.IntegrationDispatch == nil {
		resp := bus.NewCorrelatedMessage(req.Msg)
		resp.Payload = &busv1.InternalMessage_IntegrationDispatchResponse{
			IntegrationDispatchResponse: &busv1.IntegrationDispatchResponse{
				Success: false,
				Error:   "invalid integration dispatch request",
			},
		}
		req.Reply(resp)
		return
	}

	ir := dispatchReq.IntegrationDispatch
	correlationId := req.Msg.CorrelationId

	// Look up the capability handler
	handler, found := e.integrations.Get(ir.CapabilityFqn)
	if !found {
		resp := bus.NewCorrelatedMessage(req.Msg)
		resp.Payload = &busv1.InternalMessage_IntegrationDispatchResponse{
			IntegrationDispatchResponse: &busv1.IntegrationDispatchResponse{
				Success: false,
				Error:   "capability not found: " + ir.CapabilityFqn,
			},
		}
		req.Reply(resp)
		return
	}

	// Convert proto args to map[string]any
	args := make(map[string]any)
	if ir.Args != nil {
		args = ir.Args.AsMap()
	}

	// Execute the capability handler
	nodes, err := handler(ctx, args, int(ir.Target))

	resp := bus.NewCorrelatedMessage(req.Msg)
	if err != nil {
		e.Logger.Error("integration dispatch failed",
			"correlation_id", correlationId,
			"capability", ir.CapabilityFqn,
			"error", err,
		)
		resp.Payload = &busv1.InternalMessage_IntegrationDispatchResponse{
			IntegrationDispatchResponse: &busv1.IntegrationDispatchResponse{
				Success: false,
				Error:   err.Error(),
			},
		}
		req.Reply(resp)
		return
	}

	// Serialize result nodes
	resultValue, err := marshalNodes(nodes)
	if err != nil {
		resp.Payload = &busv1.InternalMessage_IntegrationDispatchResponse{
			IntegrationDispatchResponse: &busv1.IntegrationDispatchResponse{
				Success: false,
				Error:   "failed to marshal result: " + err.Error(),
			},
		}
		req.Reply(resp)
		return
	}

	resp.Payload = &busv1.InternalMessage_IntegrationDispatchResponse{
		IntegrationDispatchResponse: &busv1.IntegrationDispatchResponse{
			Success: true,
			Result:  resultValue,
		},
	}
	req.Reply(resp)
}

// marshalNodes converts a slice of MemoryNodes to a protobuf Value.
func marshalNodes(nodes []memorynodes.MemoryNode) (*structpb.Value, error) {
	if len(nodes) == 0 {
		return structpb.NewListValue(&structpb.ListValue{}), nil
	}

	jsonBytes, err := json.Marshal(nodes)
	if err != nil {
		return nil, err
	}

	var generic any
	if err := json.Unmarshal(jsonBytes, &generic); err != nil {
		return nil, err
	}

	return structpb.NewValue(generic)
}

// truncate returns the first n characters of s, appending "..." if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
