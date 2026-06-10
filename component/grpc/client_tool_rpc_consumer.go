package memql

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
)

// Agent-side consumer for the client-tool reply leg over the request/response
// substrate (memql#1265). The cognition relay routes a ClientToolResult back to
// the agent via SubstrateRPC.Call addressed to the turn's logical endpoint
// (RoutingKey{rpcClientToolKind, requestId}); this runs the matching Serve loop
// on the agent node, decodes the result from the request payload, and hands it
// to the gRPC Server so the parked client-tool waiter (keyed by call_id)
// returns. Because the request is addressed to the turn's LOGICAL key, the reply
// leg no longer depends on the in-flight forwarded gRPC stream owned by a
// specific cognition replica -- the leg that broke under churn (memql#1245).
//
// The RPC layer here is the one merged on main (component/node/substrate_rpc.go):
// RoutingKey-addressed, map[string]any payloads, RPCHandler returning a
// (map[string]any, error). This consumer maps the client-tool result onto that
// shape.

const (
	// rpcClientToolKind is the RoutingKey.Kind for a per-turn client-tool reply
	// endpoint. Distinct from the event kinds (space/session/user) and the
	// stream kind so the per-key seq space + cursor never collide.
	rpcClientToolKind = "rpcClientTool"

	// clientToolRPCMethod is the RPC method cognition's relay dispatches a
	// relayed ClientToolResult on. Must match integrations/cognition's
	// clientToolRPCMethod.
	clientToolRPCMethod = "clientToolResult"

	// clientToolRPCSubstrateEnv gates the agent-side per-turn Serve loop. Must
	// match integrations/cognition's clientToolRPCEnv so both legs flip together.
	clientToolRPCSubstrateEnv = "MEMQL_CLIENT_TOOL_RPC_SUBSTRATE"

	// Payload keys carried on the client-tool RPC request body. Kept small +
	// explicit so the durable row's jsonb stays a stable cross-binary contract.
	clientToolRPCKeyCallID   = "callId"
	clientToolRPCKeyContent  = "content" // []map[string]any (ToolResultContent)
	clientToolRPCKeyIsError  = "isError" // bool
	clientToolRPCKeyErrorMsg = "errorMessage"
)

// ClientToolRPCKey builds the logical endpoint a relayed ClientToolResult is
// addressed to for a given agent turn. The id is the turn's requestId, known to
// both cognition (which issued the forward) and the agent (which stamped it on
// the turn), so the reply routes without naming a physical node.
func ClientToolRPCKey(requestId string) node.RoutingKey {
	return node.RoutingKey{Kind: rpcClientToolKind, ID: requestId}
}

// clientToolRPCSubstrateEnabled reports whether the substrate reply leg is
// switched on. Default OFF -- the live path stays on ForwardContinuation until
// the 2-replica cluster gate (memql#1261) proves the substrate leg.
func clientToolRPCSubstrateEnabled() bool {
	v := strings.TrimSpace(os.Getenv(clientToolRPCSubstrateEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// ServeClientToolRPC runs the agent-side Serve loop for the client-tool reply
// leg, blocking until ctx is cancelled. requestId is the agent turn's id (the
// logical endpoint cognition Calls); rpc is the SubstrateRPC layer; server is
// the gRPC Server holding the parked waiters. Any of rpc / server being nil is a
// no-op (single-node / non-agent builds), so the caller can wire this
// unconditionally and let it stand down when the substrate isn't present.
func ServeClientToolRPC(
	ctx context.Context,
	rpc *node.SubstrateRPC,
	server *Server,
	requestId string,
	logger *slog.Logger,
) {
	if rpc == nil || server == nil || requestId == "" {
		return
	}
	handler := func(_ context.Context, req node.RPCRequest) (map[string]any, error) {
		if req.Method != clientToolRPCMethod {
			// Unknown method on this endpoint: the RPC layer surfaces the error
			// to the caller rather than parking it forever.
			return nil, fmt.Errorf("client-tool rpc: unexpected method %q", req.Method)
		}
		result := decodeClientToolResult(req.Payload)
		if result != nil {
			server.DeliverClientToolResult(result)
		}
		// Empty reply: the caller only needs the round-trip ack, not a payload
		// (the result was delivered to the parked waiter).
		return nil, nil
	}
	if err := rpc.Serve(ctx, ClientToolRPCKey(requestId), handler); err != nil && ctx.Err() == nil {
		if logger != nil {
			logger.Warn("client-tool rpc serve loop exited", "request_id", requestId, "error", err)
		}
	}
}

// decodeClientToolResult reconstructs a ClientToolResult from the RPC request
// payload map. Total: a malformed/empty payload yields a result with whatever
// fields it could read (an empty callId result is harmlessly dropped by the
// waiter table).
func decodeClientToolResult(payload map[string]any) *memqlv1.ClientToolResult {
	if payload == nil {
		return nil
	}
	callId, _ := payload[clientToolRPCKeyCallID].(string)
	isError, _ := payload[clientToolRPCKeyIsError].(bool)
	errMsg, _ := payload[clientToolRPCKeyErrorMsg].(string)
	result := &memqlv1.ClientToolResult{
		CallId:       callId,
		IsError:      isError,
		ErrorMessage: errMsg,
	}
	if items, ok := payload[clientToolRPCKeyContent].([]any); ok {
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			content := &memqlv1.ToolResultContent{}
			content.Type, _ = item["type"].(string)
			content.Text, _ = item["text"].(string)
			content.MimeType, _ = item["mimeType"].(string)
			content.Data, _ = item["data"].(string)
			content.Uri, _ = item["uri"].(string)
			result.Content = append(result.Content, content)
		}
	}
	return result
}
