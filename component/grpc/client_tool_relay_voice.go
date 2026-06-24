package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/core/id"
)

// client_tool_relay_voice.go
// ==========================
//
// Server-side entry into the cross-node client-tool relay for callers that
// have NO browser of their own on the calling stream -- chiefly the voice
// agent (#1420).
//
// The realtime voice model drives UI-control tools (uiClick / uiType /
// copresent_control, all ClientExecution=true). The voice agent runs them by
// sending a CallToolMsg on its locked VoiceAgent* gRPC stream; handleCallTool
// lands here. The PROBLEM the relay fixes: executeClientTool emits the
// ClientToolCall back on the CALLING stream (the voice-agent process), which
// has no browser, so the call can only time out. The browser for the same
// space lives on a DIFFERENT stream (and usually a different node), reachable
// only through the graph event bus.
//
// This file reuses the EXACT relay protocol cognition's agent-loop bridge uses
// (integrations/cognition/client_tool_relay.go) -- the same
// v1:cognition:client:tool:request / :response graph nodes, keyed by the same
// call_id -- but parks the waiter on THIS engine node's service-scoped
// clientToolWaiters instead of an agent turn:
//
//   1. relayClientToolToBrowser registers the service waiter by call_id, emits
//      a v1:cognition:client:tool:request graph node scoped to the voice space
//      (so the browser's space subscription picks it up), and blocks on the
//      waiter with the call's timeout.
//   2. The browser dispatches the tool via its client-tool relay bridge and
//      inserts a matching v1:cognition:client:tool:response.
//   3. The service subscribes ONCE to client:tool:response events (the grpc
//      Server already holds the cluster event bus) and fires the local waiter
//      via DeliverClientToolResult -- the same waiter-firing path the agent
//      relay's substrate return leg uses. Correlation stays on call_id; no
//      parallel mechanism is introduced.
//
// The agent relay (cognition) also subscribes to the same response events; a
// response for a call_id it never relayed simply finds no pending entry and is
// dropped there, while THIS node's waiter fires. The two relays coexist on
// disjoint call_id sets.

const (
	// eventPatternClientToolResponseVoice mirrors cognition's
	// eventPatternClientToolResponse so the voice relay observes the same
	// browser-originated response events.
	eventPatternClientToolResponseVoice = "graph.node.created.v1:cognition:client:tool:response"
)

// startClientToolResponseRelay subscribes the service to browser-originated
// client:tool:response events so a result lands on whichever engine node parked
// the waiter (the voice relay path, #1420). Idempotent and safe when the bus is
// nil (single-node / non-bus builds -- the voice relay then can't be reached
// and client-tool calls degrade to the executeClientTool timeout, same as
// before). Called once from prepareForRun after the service is built.
func (s *service) startClientToolResponseRelay() {
	if s == nil || s.eventBus == nil {
		return
	}
	s.clientToolRelayOnce.Do(func() {
		s.eventBus.Subscribe(
			eventPatternClientToolResponseVoice,
			s.handleClientToolResponseEvent,
			events.WithSubscriberName("grpc:voice-client-tool-relay"),
		)
		if s.logger != nil {
			s.logger.Info("subscribed to client-tool-response events (voice relay)",
				"component", ComponentName,
				"pattern", eventPatternClientToolResponseVoice)
		}
	})
}

// handleClientToolResponseEvent decodes a browser client:tool:response graph
// event and fires the local service waiter for its call_id (#1420). A response
// whose call_id has no pending waiter on THIS node (e.g. it belongs to the
// agent relay, or the waiter already timed out) is a no-op here. Reuses
// DeliverClientToolResult -- the exact path the agent relay's substrate return
// leg fires -- so both relays decode + deliver identically.
func (s *service) handleClientToolResponseEvent(event events.Event) {
	if s == nil {
		return
	}
	callId, contentJSON, errorMessage, isError := decodeClientToolResponsePayload(event.Payload)
	callId = strings.TrimSpace(callId)
	if callId == "" {
		return
	}
	// Only act when a waiter for this call_id is parked here; otherwise this
	// response belongs to another node / relay and we leave it alone.
	delivered := s.DeliverClientToolResult(node.ClientToolResultPayload{
		CallID:       callId,
		ContentJSON:  contentJSON,
		IsError:      isError,
		ErrorMessage: errorMessage,
	})
	if delivered && s.logger != nil {
		s.logger.Debug("voice client-tool relay: delivered browser response to parked waiter",
			"component", ComponentName, "call_id", callId, "is_error", isError)
	}
}

// decodeClientToolResponsePayload pulls the relay fields off a graph-node event
// payload. Graph-node events flatten the concept payload top-level and ALSO
// mirror it under payload["payload"]; read top-level first, fall back to
// nested -- identical to cognition's handleClientToolResponse so both relays
// observe the same shape.
func decodeClientToolResponsePayload(payload map[string]any) (callId, contentJSON, errorMessage string, isError bool) {
	callId, _ = payload["callId"].(string)
	contentJSON, _ = payload["contentJSON"].(string)
	errorMessage, _ = payload["errorMessage"].(string)
	isError, _ = payload["isError"].(bool)
	if strings.TrimSpace(callId) == "" {
		if nested, ok := payload["payload"].(map[string]any); ok && nested != nil {
			callId, _ = nested["callId"].(string)
			contentJSON, _ = nested["contentJSON"].(string)
			errorMessage, _ = nested["errorMessage"].(string)
			if v, ok := nested["isError"].(bool); ok {
				isError = v
			}
		}
	}
	return callId, contentJSON, errorMessage, isError
}

// relayClientToolToBrowser runs a ClientExecution tool for a caller with no
// browser on its own stream (the voice agent, #1420) by routing it through the
// cross-node relay: it registers the service waiter by call_id, emits a
// v1:cognition:client:tool:request graph node scoped to partitionId so the
// browser's space subscription dispatches it, and blocks until the matching
// client:tool:response lands (fired by handleClientToolResponseEvent) or the
// timeout expires. Returns the tool result content, or an error (timeout /
// browser failure / emit failure) the caller surfaces to the model as a tool
// error so it can tell the user.
func (s *streamSession) relayClientToolToBrowser(
	ctx context.Context,
	toolName string,
	argsJSON string,
	partitionId string,
) ([]*memqlv1.ToolResultContent, error) {
	if s == nil || s.service == nil || s.service.engine == nil {
		return nil, fmt.Errorf("client-tool relay unavailable")
	}
	if strings.TrimSpace(partitionId) == "" {
		// No space to scope the request to -- the browser would never see it.
		return nil, fmt.Errorf("client tool %q has no space to relay to", toolName)
	}
	// The grpc service must be subscribed to response events or the waiter can
	// never fire. Refuse fast rather than parking for the full timeout.
	if s.service.eventBus == nil {
		return nil, fmt.Errorf("client tool %q cannot be relayed (no event bus)", toolName)
	}

	callId := id.NewShortId()
	if strings.TrimSpace(argsJSON) == "" {
		argsJSON = "{}"
	}

	// Register the waiter BEFORE emitting so a fast browser response is never
	// missed. The response leg (handleClientToolResponseEvent) does the
	// LoadAndDelete-equivalent via DeliverClientToolResult.
	ch := s.registerClientToolWaiter(callId)
	defer s.unregisterClientToolWaiter(callId)

	// Also serve the DURABLE-SUBSTRATE return channel for this call so the
	// browser response reaches THIS node reliably, independent of best-effort
	// mesh delivery. Root cause of the #1835 voice strand: the response was
	// broadcast over the mesh and reached cognition + the sibling agent replica
	// instantly but this waiter's node ~30s late (after the 30s timeout had
	// already fired) -- the agent node had no return path the response could be
	// ADDRESSED to. The voice relay sets requestId==callId, so cognition's
	// no-pending-entry Deliver(callId) targets agentTurnKey(callId); serving it
	// here fires this node's clientToolWaiters[callId] over the durable,
	// replica-addressed substrate (the same return leg the text/agent path
	// already uses, #1265). The event-bus path stays as a best-effort fast-path;
	// whichever lands first wins, the other is an idempotent no-op.
	if srv := s.service.clientToolResultServer; srv != nil {
		srv.BeginTurn(ctx, callId)
		defer srv.EndTurn(callId)
	}

	// Bound the browser round-trip by the per-tool ceiling: the fast
	// interactive ceiling for programmatic UI tools (uiClick / uiRequestControl
	// / ...) so an unreachable browser fails fast and legibly (#1834), the
	// longer human-input ceiling for uiAskUser.
	timeout := clientToolWaitTimeout(toolName)
	expiresAt := time.Now().Add(timeout).UTC()
	if s.logger != nil {
		s.logger.Info("client-tool waiter registered",
			"component", ComponentName, "path", "voice",
			"call_id", callId, "tool", toolName, "partition_id", partitionId,
			"timeout_ms", timeout.Milliseconds())
	}

	// Reuse the SAME request mutation + node shape the agent relay uses
	// (emitClientToolRequest); the request node id is keyed by callId so
	// repeated emits upsert rather than stack. agentId is audit-only; the voice
	// agent attributes via the acting agent on context.
	agentId := memqlengine.ActingAgentIdFromContext(ctx)
	mutation := fmt.Sprintf(`emitClientToolRequest({
		requestId: %s,
		callId: %s,
		toolName: %s,
		argumentsJSON: %s,
		partitionId: %s,
		participantId: %s,
		agentId: %s,
		expiresAt: %s
	})`,
		escapeGraphJSONString(callId),
		escapeGraphJSONString(callId),
		escapeGraphJSONString(toolName),
		escapeGraphJSONString(argsJSON),
		escapeGraphJSONString(partitionId),
		escapeGraphJSONString(""),
		escapeGraphJSONString(agentId),
		escapeGraphJSONString(expiresAt.Format(time.RFC3339Nano)),
	)
	if _, err := s.service.engine.Execute(ctx, mutation); err != nil {
		return nil, fmt.Errorf("emit client tool request: %w", err)
	}
	if s.logger != nil {
		s.logger.Info("voice client-tool relay: emitted request",
			"component", ComponentName, "call_id", callId, "tool", toolName, "partition_id", partitionId)
	}

	result, outcome, elapsed := awaitClientToolResult(ctx, ch, toolName)
	if s.logger != nil {
		s.logger.Info("client-tool waiter resolved",
			"component", ComponentName, "path", "voice",
			"call_id", callId, "tool", toolName,
			"outcome", string(outcome), "elapsed_ms", elapsed.Milliseconds())
	}
	switch outcome {
	case clientToolCancelled:
		return nil, ctx.Err()
	case clientToolTimedOut:
		if toolName == "uiAskUser" {
			// uiAskUser blocks on a human; keep a plain timeout error rather
			// than the browser-unreachable fast-fail message.
			return nil, fmt.Errorf("client tool %q timed out after %v (no user response)", toolName, elapsed.Round(time.Millisecond))
		}
		// Browser gone / unreachable: fail fast with the typed, user-legible
		// error so the voice model tells the user instead of speaking a bare
		// "it timed out" (#1834).
		return nil, clientToolUnreachableError(toolName, elapsed)
	}
	if result == nil {
		return nil, fmt.Errorf("client tool %q relay closed without a result", toolName)
	}
	if result.GetIsError() {
		errMsg := result.GetErrorMessage()
		if errMsg == "" {
			errMsg = "client tool execution failed in the browser"
		}
		return nil, fmt.Errorf("%s", errMsg)
	}
	return result.GetContent(), nil
}

// escapeGraphJSONString renders a Go string as a safely-quoted JSON string
// literal for embedding into a memQL mutation argument. It delegates to
// encoding/json, which produces a fully-escaped, balanced-quote literal so a
// value containing a double quote (e.g. a tool argument) can never break out of
// the enclosing quotes. A marshal error (not reachable for a Go string) falls
// back to the empty JSON string. component/grpc must not import the cognition
// package, so this is a local helper rather than a reuse of escapeJSONString.
func escapeGraphJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
