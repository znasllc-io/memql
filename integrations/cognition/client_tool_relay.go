package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/core/id"
)

// ClusterRPC is the narrow request/response-over-substrate surface the
// client-tool relay uses to route a ClientToolResult back to the agent that
// parked on the call (memql#1265). Satisfied by *node.SubstrateRPC; injected via
// SetClusterRPC on cognition binaries. When nil the relay falls back to the
// legacy ForwardContinuation reply leg (the in-flight forwarded gRPC stream),
// preserving behaviour on single-node / pre-cutover builds.
//
// The shape mirrors the RPC layer merged on main (component/node/substrate_rpc.go):
// RoutingKey-addressed, map[string]any payload, map reply.
type ClusterRPC interface {
	// Call issues a request to calleeKey and awaits the correlated reply,
	// bounded by ctx. The reply routes back by logical key, so it survives the
	// issuing replica restarting mid-call.
	Call(ctx context.Context, calleeKey node.RoutingKey, method string, payload map[string]any) (map[string]any, error)
}

// clientToolRPCMethod is the RPC method the agent's Serve handler dispatches a
// relayed ClientToolResult on. Must match component/grpc's clientToolRPCMethod.
const clientToolRPCMethod = "clientToolResult"

// rpcClientToolKind is the RoutingKey.Kind for a per-turn client-tool reply
// endpoint. Must match component/grpc's rpcClientToolKind so cognition's Call
// and the agent's Serve address the same logical key.
const rpcClientToolKind = "rpcClientTool"

// Payload keys carried on the client-tool RPC request body. Must match
// component/grpc's decode keys so the cross-binary wire contract round-trips.
const (
	clientToolRPCKeyCallID   = "callId"
	clientToolRPCKeyContent  = "content"
	clientToolRPCKeyIsError  = "isError"
	clientToolRPCKeyErrorMsg = "errorMessage"
)

// clientToolRPCEnv gates the substrate-RPC reply leg. Default OFF: the live,
// latency-sensitive client-tool round-trip keeps riding the proven
// ForwardContinuation path until the 2-replica cluster gate (memql#1261) proves
// the substrate reply leg non-regressive, per the issue's safety boundary.
// Set MEMQL_CLIENT_TOOL_RPC_SUBSTRATE=1 to route replies over the substrate.
const clientToolRPCEnv = "MEMQL_CLIENT_TOOL_RPC_SUBSTRATE"

// clientToolRPCKey builds the per-turn logical endpoint addressed by the reply
// leg. The id is the turn's requestId, shared by cognition + the owning agent.
func clientToolRPCKey(requestId string) node.RoutingKey {
	return node.RoutingKey{Kind: rpcClientToolKind, ID: requestId}
}

// clientToolRPCEnabled reports whether the substrate reply leg is switched on
// AND wired (an RPC layer is installed). Both must hold; otherwise the relay
// uses ForwardContinuation.
func (c *CognitionIntegration) clientToolRPCEnabled() bool {
	if c == nil || c.clusterRPC == nil {
		return false
	}
	v := strings.TrimSpace(os.Getenv(clientToolRPCEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// client_tool_relay.go
// =====================
//
// Cross-node bridge for client-executed tools.
//
// The agent-generate-turn pipeline runs on the agent node. When the
// agent's tool loop invokes a client-executed tool (e.g. `uiReadState`),
// the agent's streamSession emits a ClientToolCall envelope. That
// envelope rides the forwardedStream back to cognition as an
// SIForwardResponse (see cognition/agent_forward.go consumeAgentTurnStream).
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
//      the agent node as a fresh SIForwardRequest. The agent's
//      service-scoped waiter (see component/grpc/server.go
//      clientToolWaiters) fires and the parked tool loop returns.
//
// The call_id is carried end-to-end so correlation is independent of
// which session / process touched the message at each hop.

const (
	// Pattern that cognition subscribes to for browser-originated tool
	// results. Path mirrors the other copresent concept event patterns
	// so the events.Bus treats this identically.
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
)

// pendingClientToolCall records the correlation for a client-tool call
// that cognition relayed from agent -> browser and is awaiting the
// matching response. requestId refers to the in-flight SIForwardRouter
// entry so ForwardContinuation can address the same stream.
type pendingClientToolCall struct {
	requestId string
	createdAt time.Time
	// authClaims / partition are passed back to ForwardContinuation for
	// parity with the original AgentGenerateTurn forward. Internal
	// relay traffic runs without end-user claims.
	authClaims map[string]string
	partition  string
	// agentId is the agent's canonical logical id (audit/diagnostic only; the
	// substrate reply leg addresses the turn's requestId endpoint, which both
	// cognition and the owning agent replica share).
	agentId string
}

// SetClusterRPC installs the request/response-over-substrate layer used by the
// client-tool relay's reply leg (memql#1265). Nil is tolerated: the relay then
// falls back to ForwardContinuation. Wired by app bootstrap on cognition
// binaries once the substrate + node topology are resolved (cluster phase).
func (c *CognitionIntegration) SetClusterRPC(rpc ClusterRPC) {
	if c == nil {
		return
	}
	c.clusterRPC = rpc
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
// spaceId / participantId come from the cognition turn context; they
// tell the browser whether the request applies to its session. agentId
// is audit-only.
func (c *CognitionIntegration) relayClientToolCall(
	ctx context.Context,
	requestId string,
	spaceId string,
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
		"spaceId", spaceId,
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
		agentId:   strings.TrimSpace(agentId),
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

	mutation := fmt.Sprintf(`mutationEmitClientToolRequest({
		"requestId": %s,
		"callId": %s,
		"toolName": %s,
		"argumentsJSON": %s,
		"spaceId": %s,
		"participantId": %s,
		"agentId": %s,
		"expiresAt": %s
	})`,
		escapeJSONString(requestNodeId),
		escapeJSONString(callId),
		escapeJSONString(call.GetToolName()),
		escapeJSONString(call.GetArgumentsJson()),
		escapeJSONString(spaceId),
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
		"spaceId", spaceId,
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
		// Response arrived after sweep or for an unknown call -- drop.
		c.Logger.Warn("client-tool relay: response without pending entry",
			"callId", callId)
		return
	}
	entry, ok := raw.(*pendingClientToolCall)
	if !ok || entry == nil {
		return
	}

	content := parseToolResultContent(contentJSON)

	// Reply leg (memql#1265): when the substrate RPC layer is wired AND enabled,
	// route the ClientToolResult back over the durable substrate keyed by the
	// agent's logical endpoint, so the reply reaches whoever owns that agent's
	// work even if THIS cognition replica (which issued the original forward) has
	// since restarted -- the exact churn that left the agent with 0 turns under
	// memql#1245. Otherwise fall back to the legacy in-flight forwarded-stream
	// continuation. The gate keeps the live path on the proven leg until the
	// 2-replica cluster gate (memql#1261) proves the substrate leg.
	if c.clientToolRPCEnabled() && entry.requestId != "" {
		if c.replyViaSubstrateRPC(callId, entry, content, isError, errorMessage) {
			return
		}
		// RPC reply failed; fall through to ForwardContinuation as a safety net
		// so a transient substrate error doesn't strand the parked agent waiter.
	}

	if c.agentForwarder == nil {
		c.Logger.Warn("client-tool relay: response dropped, no agentForwarder configured",
			"callId", callId)
		return
	}

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

// replyViaSubstrateRPC routes a ClientToolResult back to the agent over the
// request/response substrate (memql#1265). It builds the result payload and
// issues a Call to the turn's logical endpoint (clientToolRPCKey(requestId)),
// returning true on success. A failure returns false so the caller can fall
// back to the legacy ForwardContinuation leg. The Call is bounded so a
// missing/dead agent endpoint can't park the relay goroutine forever.
func (c *CognitionIntegration) replyViaSubstrateRPC(
	callId string,
	entry *pendingClientToolCall,
	content []*memqlv1.ToolResultContent,
	isError bool,
	errorMessage string,
) bool {
	// Build the request payload as a plain map -- the RPC layer carries it as
	// the substrate Deliverable's jsonb body, and the agent's Serve handler
	// decodes it back into a ClientToolResult (see component/grpc's
	// decodeClientToolResult). Content items are plain maps mirroring
	// memqlv1.ToolResultContent so the cross-binary contract stays explicit.
	items := make([]any, 0, len(content))
	for _, item := range content {
		if item == nil {
			continue
		}
		items = append(items, map[string]any{
			"type":     item.GetType(),
			"text":     item.GetText(),
			"mimeType": item.GetMimeType(),
			"data":     item.GetData(),
			"uri":      item.GetUri(),
		})
	}
	payload := map[string]any{
		clientToolRPCKeyCallID:   callId,
		clientToolRPCKeyIsError:  isError,
		clientToolRPCKeyErrorMsg: errorMessage,
		clientToolRPCKeyContent:  items,
	}

	ctx, cancel := context.WithTimeout(context.Background(), pendingClientToolCallTTL)
	defer cancel()
	// The agent's per-turn Serve handler (keyed by requestId -- the turn's
	// logical endpoint, known to both cognition and the agent) delivers the
	// result into its parked waiter and replies with an empty-body ack; we don't
	// need the reply payload, only that it round-tripped. The reply itself routes
	// back to THIS cognition replica's logical reply key. Addressing by requestId
	// makes the leg survive cognition-replica churn: whichever agent replica owns
	// the turn picks up the request, vs. ForwardContinuation which needs the
	// in-flight forwarded stream entry on the replica that issued the forward.
	if _, err := c.clusterRPC.Call(ctx, clientToolRPCKey(entry.requestId), clientToolRPCMethod, payload); err != nil {
		c.Logger.Warn("client-tool relay: substrate RPC reply failed; falling back",
			"callId", callId, "requestId", entry.requestId, "error", err)
		return false
	}
	c.Logger.Info("client-tool relay: routed response to agent via substrate RPC",
		"callId", callId,
		"requestId", entry.requestId,
		"isError", isError,
		"wait_ms", time.Since(entry.createdAt).Milliseconds(),
	)
	return true
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
