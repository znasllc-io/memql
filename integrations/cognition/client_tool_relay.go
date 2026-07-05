package cognition

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

// client_tool_relay.go
// =====================
//
// Cross-node bridge for client-executed tools.
//
// The agent-generate-turn pipeline runs on the agent node. When the
// agent's tool loop invokes a client-executed tool (e.g. `uiReadState`),
// the agent's streamSession emits a ClientToolCall envelope. That
// envelope rides the forwardedStream back to cognition as an
// AiForwardResponse (see cognition/agent_forward.go consumeAgentTurnStream).
//
// The browser is on a *different* node (BFF). So ClientToolCall cannot
// reach the browser by stream topology alone. This relay closes the
// gap by using the graph event bus:
//
//   1. Cognition intercepts ClientToolCall, inserts a
//      v1:cognition:client:tool:request node (via mutation) and
//      remembers (callId -> requestId) so it can reply.
//   2. The browser subscribes to client:tool:request events via the
//      normal MemqlStreamClient subscription (scoped to its active
//      space/participant), dispatches the tool via whatever client-
//      side registry the product ships, and inserts a matching
//      v1:cognition:client:tool:response.
//   3. Cognition subscribes to client:tool:response events, looks up
//      the pending entry by callId, wraps the payload in a
//      ClientToolResult MemqlClientMessage and hands it to
//      agentForwarder.ForwardContinuation, which delivers it back to
//      the agent node as a fresh AiForwardRequest. The agent's
//      service-scoped waiter (see component/grpc/server.go
//      clientToolWaiters) fires and the parked tool loop returns.
//
// The call_id is carried end-to-end so correlation is independent of
// which session / process touched the message at each hop.

const (
	// Pattern that cognition subscribes to for browser-originated tool
	// results. Path mirrors the product pack's other concept event
	// patterns so the events.Bus treats this identically.
	eventPatternClientToolResponse = "graph.node.created.v1:cognition:client:tool:response"

	// Default lifetime of a pending client-tool call when the call itself
	// didn't carry a timeout hint. The relay prefers `call.TimeoutMs +
	// pendingClientToolCallGrace` (see relayClientToolCall) so the
	// correlation stays alive for the full agent-side wait — for
	// uiAskUser that's 120s, for ordinary UI pokes it's 30s. We only
	// fall back to this constant when `call.TimeoutMs` is absent.
	pendingClientToolCallTTL = 60 * time.Second

	// Extra grace tacked onto the call's declared timeout before the
	// sweeper fires. Covers the round-trip between the browser's
	// resolveAsk() and cognition's handleClientToolResponse so a late-
	// but-legitimate response doesn't race the sweeper. Keep small; its
	// only job is absorbing network jitter on the response leg.
	pendingClientToolCallGrace = 15 * time.Second

	// Bound for the speculative substrate Deliver cognition makes when a browser
	// response has no pending entry on this replica (the voice/service relay path
	// parks its waiter on the emitting node, addressed by callId -- #1835). Kept
	// short: a live voice waiter replies in ms; a true orphan has no server, so
	// this just bounds how long the throwaway delivery goroutine lives.
	clientToolNoEntryDeliverTimeout = 8 * time.Second
)

// pendingClientToolCall records the correlation for a client-tool call
// that cognition relayed from agent -> browser and is awaiting the
// matching response. requestId refers to the in-flight AiForwardRouter
// entry so ForwardContinuation can address the same stream.
type pendingClientToolCall struct {
	requestId string
	createdAt time.Time
	// authClaims / partition are passed back to ForwardContinuation for
	// parity with the original AgentGenerateTurn forward. Internal
	// relay traffic runs without end-user claims.
	authClaims map[string]string
	partition  string
}

// startClientToolRelay subscribes cognition to clientToolResponse
// events. Called from Start(). Safe to call even when agentForwarder
// is nil -- responses without a pending entry are simply dropped.
func (c *CognitionIntegration) startClientToolRelay() {
	if c == nil || c.eventBus == nil {
		return
	}
	c.unsubscribes = append(c.unsubscribes, c.eventBus.Subscribe(
		eventPatternClientToolResponse,
		c.handleClientToolResponse,
		events.WithSubscriberName("cognition:client-tool-relay"),
	))
	c.Logger.Info("subscribed to client-tool-response events",
		"pattern", eventPatternClientToolResponse,
	)
}

// relayClientToolCall is invoked by consumeAgentTurnStream when it
// sees a ClientToolCall MemqlServerMessage from the agent node. It
// records the correlation and inserts a clientToolRequest graph node
// so the browser receives the call via its space subscription.
//
// partitionId / participantId come from the cognition turn context; they
// tell the browser whether the request applies to its session. agentId
// is audit-only.
func (c *CognitionIntegration) relayClientToolCall(
	ctx context.Context,
	requestId string,
	partitionId string,
	participantId string,
	agentId string,
	call *memqlv1.ClientToolCall,
) {
	if c == nil || call == nil {
		return
	}
	relayStart := time.Now()

	callId := strings.TrimSpace(call.GetCallId())
	if callId == "" {
		return
	}

	// Diagnostic log so we can see exactly what the agent passed the
	// first time a call misbehaves. The agent node's streamSession
	// (guardClientToolArgs in component/grpc/server.go) owns the
	// retry-budget + soft-fallback policy for uiAskUser; by the time
	// a call reaches us here, either it has valid options or the
	// agent's budget was exhausted and defaults were injected. Either
	// way the right thing for the relay to do is pass it through and
	// log what it saw.
	c.Logger.Debug("client-tool relay: call args",
		"callId", callId,
		"toolName", call.GetToolName(),
		"argumentsJSON", call.GetArgumentsJson(),
		"partitionId", partitionId,
		"agentId", agentId,
	)

	// Capture the partition off the ctx before storing the correlation.
	// Without this, the entry is born with an empty partition and
	// ForwardContinuation later sends the ClientToolResult back to the
	// agent node with no partition stamped -- the agent's parked
	// waiter would resume under whatever fallback the agent node
	// defaults to instead of the original tenant. The relay is the
	// only thing that knows the original partition for this turn
	// (cognition received it in the envelope; the browser response
	// event flattens through the graph bus and arrives without
	// partition context), so it has to pin the value here.
	//
	// authClaims stays nil for now: cognition-originated forwards
	// don't carry an end-user principal -- the agent node already
	// authenticated the original request before the parked
	// clientToolWaiter was registered, and the continuation only
	// needs to find that waiter (keyed by requestId).
	partition := memqlengine.PartitionFromContext(ctx)
	c.pendingClientToolCalls.Store(callId, &pendingClientToolCall{
		requestId: requestId,
		createdAt: time.Now(),
		partition: partition,
	})

	timeoutMs := int64(call.GetTimeoutMs())
	if timeoutMs <= 0 {
		timeoutMs = int64(pendingClientToolCallTTL / time.Millisecond)
	}
	expiresAt := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond).UTC()

	// Deterministic-ish request-node ID keyed by callId so repeated
	// emits (in theory) would collide rather than stacking. Regardless,
	// insert() upserts, so this is defensive.
	requestNodeId := callId

	mutation := fmt.Sprintf(`mutation emitClientToolRequest(
		requestId: %s,
		callId: %s,
		toolName: %s,
		argumentsJSON: %s,
		partitionId: %s,
		participantId: %s,
		agentId: %s,
		expiresAt: %s
	)`,
		escapeJSONString(requestNodeId),
		escapeJSONString(callId),
		escapeJSONString(call.GetToolName()),
		escapeJSONString(call.GetArgumentsJson()),
		escapeJSONString(partitionId),
		escapeJSONString(participantId),
		escapeJSONString(agentId),
		escapeJSONString(expiresAt.Format(time.RFC3339Nano)),
	)
	if _, err := c.engine.Execute(ctx, mutation); err != nil {
		// Emit failed -- drop the correlation so we don't leak the map
		// entry. The agent's own 30s timeout will surface the error to
		// the user.
		c.pendingClientToolCalls.Delete(callId)
		c.Logger.Warn("client-tool relay: failed to emit clientToolRequest",
			"callId", callId, "error", err)
		return
	}

	// TTL sweeper: prefer the call's own deadline (+grace) over the
	// fallback constant. Every ClientToolCall carries a TimeoutMs — see
	// clientToolTimeoutMs in component/grpc/server.go (30s for ordinary
	// UI tools, 120s for uiAskUser because the user has to read + type
	// an answer). Sweeping at a fixed 60s would strand long-running asks:
	// user takes 90s to answer → correlation is already gone → response
	// lands with "response without pending entry" and the agent waiter
	// hits its own 120s timeout.
	sweepAfter := time.Duration(timeoutMs)*time.Millisecond + pendingClientToolCallGrace
	if sweepAfter < pendingClientToolCallTTL {
		sweepAfter = pendingClientToolCallTTL
	}
	go func(id string, after time.Duration) {
		t := time.NewTimer(after)
		defer t.Stop()
		<-t.C
		c.pendingClientToolCalls.Delete(id)
	}(callId, sweepAfter)

	c.Logger.Info("client-tool relay: emitted request",
		"callId", callId,
		"toolName", call.GetToolName(),
		"partitionId", partitionId,
		"requestId", requestId,
		"elapsed_ms", time.Since(relayStart).Milliseconds(),
	)
}

// handleClientToolResponse is the subscriber for clientToolResponse
// graph events. It decodes the browser's payload, looks up the pending
// correlation and forwards a ClientToolResult MemqlClientMessage back
// to the agent node via ForwardContinuation.
func (c *CognitionIntegration) handleClientToolResponse(event events.Event) {
	if c == nil {
		return
	}
	c.Logger.Info("client-tool relay: response event received",
		"topic", event.Topic,
		"payloadKeys", mapKeysSorted(event.Payload))

	// Graph-node events flatten the concept payload into event.Payload
	// (callId, contentJSON, isError, ...) and ALSO mirror it under
	// event.Payload["payload"]. Read top-level first; fall back to nested
	// if the publisher used the non-flattened shape.
	callId, _ := event.Payload["callId"].(string)
	contentJSON, _ := event.Payload["contentJSON"].(string)
	errorMessage, _ := event.Payload["errorMessage"].(string)
	isError, _ := event.Payload["isError"].(bool)

	if strings.TrimSpace(callId) == "" {
		if nested, ok := event.Payload["payload"].(map[string]any); ok && nested != nil {
			callId, _ = nested["callId"].(string)
			contentJSON, _ = nested["contentJSON"].(string)
			errorMessage, _ = nested["errorMessage"].(string)
			if v, ok := nested["isError"].(bool); ok {
				isError = v
			}
		}
	}
	callId = strings.TrimSpace(callId)
	if callId == "" {
		c.Logger.Warn("client-tool relay: response missing callId",
			"payloadKeys", mapKeysSorted(event.Payload))
		return
	}

	raw, ok := c.pendingClientToolCalls.LoadAndDelete(callId)
	if !ok {
		// No pending entry on THIS cognition replica. This is the normal shape
		// for the VOICE/service relay path (#1420): the waiter lives on the node
		// that emitted the call, which sets requestId==callId and serves
		// agentTurnKey(callId) over the durable substrate. Best-effort mesh
		// delivery of the browser response to that node can be slow or dropped
		// (#1835: it reached cognition instantly but the waiter's node ~30s late,
		// after the 30s timeout fired), so don't just drop -- proactively deliver
		// over the durable, replica-addressed substrate keyed by callId. For a
		// true orphan (already swept, or a text turn whose key isn't callId) no
		// server is listening and Deliver returns found=false -- harmless. Run it
		// off the event-handler goroutine with a bounded context so a missing
		// server can't stall the subscriber.
		if c.clientToolResultClient != nil {
			payload := node.ClientToolResultPayload{
				CallID: callId, ContentJSON: contentJSON,
				IsError: isError, ErrorMessage: errorMessage,
			}
			go func(id string, p node.ClientToolResultPayload) {
				dctx, cancel := context.WithTimeout(context.Background(), clientToolNoEntryDeliverTimeout)
				defer cancel()
				found, err := c.clientToolResultClient.Deliver(dctx, id, p)
				if err != nil {
					c.Logger.Warn("client-tool relay: no-entry substrate Deliver failed",
						"callId", id, "error", err)
					return
				}
				c.Logger.Info("client-tool relay: no-entry substrate Deliver",
					"callId", id, "waiterFound", found)
			}(callId, payload)
			return
		}
		c.Logger.Warn("client-tool relay: response without pending entry",
			"callId", callId)
		return
	}
	entry, ok := raw.(*pendingClientToolCall)
	if !ok || entry == nil {
		return
	}

	// Preferred return leg (memql#1265): deliver the result to the agent turn
	// over the durable substrate, addressed by the turn's LOGICAL key
	// (agentturn:<requestId>). This reaches whichever agent replica runs the
	// turn regardless of cognition<->agent connection state, fixing the #1245
	// 0-turns drop where the legacy ForwardContinuation re-used a stale cached
	// peer connection. Falls back to ForwardContinuation only when the
	// substrate client isn't wired (single-node / non-substrate builds).
	if c.clientToolResultClient != nil {
		found, err := c.clientToolResultClient.Deliver(
			context.Background(),
			entry.requestId,
			node.ClientToolResultPayload{
				CallID:       callId,
				ContentJSON:  contentJSON,
				IsError:      isError,
				ErrorMessage: errorMessage,
			},
		)
		if err != nil {
			c.Logger.Warn("client-tool relay: substrate Deliver failed",
				"callId", callId, "requestId", entry.requestId, "error", err)
			return
		}
		c.Logger.Info("client-tool relay: delivered response to agent via substrate",
			"callId", callId,
			"requestId", entry.requestId,
			"isError", isError,
			"waiterFound", found,
			"wait_ms", time.Since(entry.createdAt).Milliseconds(),
		)
		return
	}

	// Legacy fallback: ForwardContinuation over the cached peer connection.
	if c.agentForwarder == nil {
		c.Logger.Warn("client-tool relay: response dropped, no agentForwarder configured",
			"callId", callId)
		return
	}

	content := parseToolResultContent(contentJSON)

	resultEnvelope := &memqlv1.MemqlClientMessage{
		MessageId: id.NewShortId(),
		Payload: &memqlv1.MemqlClientMessage_ClientToolResult{
			ClientToolResult: &memqlv1.ClientToolResult{
				CallId:       callId,
				Content:      content,
				IsError:      isError,
				ErrorMessage: errorMessage,
			},
		},
	}

	if err := c.agentForwarder.ForwardContinuation(
		entry.requestId,
		entry.authClaims,
		entry.partition,
		resultEnvelope,
	); err != nil {
		c.Logger.Warn("client-tool relay: ForwardContinuation failed",
			"callId", callId, "requestId", entry.requestId, "error", err)
		return
	}

	c.Logger.Info("client-tool relay: forwarded response to agent",
		"callId", callId,
		"requestId", entry.requestId,
		"isError", isError,
		"wait_ms", time.Since(entry.createdAt).Milliseconds(),
	)
}

// mapKeysSorted returns map keys for diagnostic logging (order stable).
func mapKeysSorted(m map[string]any) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// parseToolResultContent turns the browser's JSON-encoded content array
// back into []*ToolResultContent. Returns nil on parse failure so the
// agent sees an empty result rather than a malformed tool reply.
func parseToolResultContent(jsonStr string) []*memqlv1.ToolResultContent {
	jsonStr = strings.TrimSpace(jsonStr)
	if jsonStr == "" || jsonStr == "null" {
		return nil
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &items); err != nil {
		return nil
	}
	out := make([]*memqlv1.ToolResultContent, 0, len(items))
	for _, item := range items {
		typeStr, _ := item["type"].(string)
		text, _ := item["text"].(string)
		mimeType, _ := item["mimeType"].(string)
		data, _ := item["data"].(string)
		uri, _ := item["uri"].(string)
		out = append(out, &memqlv1.ToolResultContent{
			Type:     typeStr,
			Text:     text,
			MimeType: mimeType,
			Data:     data,
			Uri:      uri,
		})
	}
	return out
}
