package node

import (
	"context"
	"log/slog"
	"sync"
)

// client_tool_rpc.go
// ==================
//
// Phase 2 of epic memql#1259 (memql#1265): migrate the genuine
// request/response forward -- the cluster client-tool round-trip's RETURN leg
// -- off the ad-hoc, churn-fragile peer-connection path onto the durable
// SubstrateRPC contract (correlation + reply-routing by logical key).
//
// The flow being fixed (see docs / client_tool_relay.go):
//
//  1. An agent's tool loop invokes a client-executed tool and PARKS a waiter
//     (clientToolWaiters[callId] on the agent's grpc service) for the result.
//  2. The ClientToolCall rides the streaming turn back to cognition, which
//     relays it to the browser via graph events.
//  3. The browser answers; cognition must deliver the ClientToolResult back to
//     the agent replica that parked the waiter.
//
// Step 3 was done with AgentForwarder.ForwardContinuation, which re-uses the
// ORIGINAL bff/cognition->agent peer *Connection cached on the in-flight
// SIForwardRouter entry. On agent-replica churn (rollout, restart, the #1245
// dead-peer skip) that cached connection is stale, the continuation Send fails,
// the parked waiter times out, and the agent gets 0 turns -- the exact
// memql#1245 symptom this epic exists to remove.
//
// The fix inverts the addressing from physical (the cached peer connection) to
// logical: the result is addressed to the agent TURN's logical key
// (`agentturn:<requestId>`) and travels over the durable DeliverySubstrate.
// Whichever agent replica is running that turn serves the key and fires its
// local waiter; cognition just publishes the result to the key and never has to
// hold (or even know) a live connection to that replica. Because the substrate's
// durable path is a shared-DB outbox+cursor (not the mesh), delivery survives
// the cognition<->agent connection being down entirely -- mesh-down still
// delivers, per the ADR. The streaming token leg (AgentGenerateTurnDelta) is
// untouched; that is memql#1266.
//
// ClientToolResultRPC is a thin, flow-specific wrapper over SubstrateRPC so the
// two call sites (agent Serve, cognition Call) share one correlation/reply
// contract and one envelope shape.

// ClientToolResultPayload is the application body carried on the RPC. It is the
// wire-neutral projection of the proto ClientToolResult -- the node package does
// not import the grpc/gen types, so the grpc + cognition layers marshal to/from
// this shape. contentJSON is the browser's tool-result content array as JSON
// (parsed back into []*ToolResultContent at the agent edge).
type ClientToolResultPayload struct {
	CallID       string
	ContentJSON  string
	IsError      bool
	ErrorMessage string
}

// ClientToolResultFirer delivers a decoded result to the locally-parked waiter
// for callId and reports whether a waiter was found+fired. The agent's grpc
// service implements this (DeliverClientToolResult) over its service-scoped
// clientToolWaiters map. "found" lets the RPC reply tell cognition whether the
// result actually landed on a live waiter (vs. a turn that already moved on).
type ClientToolResultFirer interface {
	DeliverClientToolResult(p ClientToolResultPayload) (found bool)
}

// agentTurnKey is the logical address of a single agent turn's client-tool
// return channel. Both the agent (Serve) and cognition (Call) derive it from
// the turn's requestId alone -- no node-id, so the result reaches whichever
// replica runs the turn (ADR 4.1).
func agentTurnKey(requestId string) RoutingKey {
	return RoutingKey{Kind: "agentturn", ID: requestId}
}

// rpc method name on the agent turn key.
const methodClientToolResult = "clientToolResult"

// rpc body field keys (kept local; the SubstrateRPC envelope reserves __rpc_*).
const (
	ctKeyCallID       = "callId"
	ctKeyContentJSON  = "contentJSON"
	ctKeyIsError      = "isError"
	ctKeyErrorMessage = "errorMessage"
	ctKeyFound        = "found"
)

// ClientToolResultServer is the agent-side endpoint. It runs ONE SubstrateRPC
// Serve loop per in-flight agent turn (keyed by requestId) so a ClientToolResult
// addressed to that turn's logical key fires the local waiter. The loop is
// started when a turn begins and stopped when it ends, so a replica only serves
// the turns it is actually running.
type ClientToolResultServer struct {
	substrate DeliverySubstrate
	firer     ClientToolResultFirer
	// consumerID is the durable cursor identity for this agent replica's serve
	// subscriptions (its node id). Per-replica so a different replica taking
	// over replays cleanly (ADR 4.4).
	consumerID string
	logger     *slog.Logger

	mu    sync.Mutex
	turns map[string]*servedTurn // requestId -> running serve loop
}

type servedTurn struct {
	cancel context.CancelFunc
	rpc    *SubstrateRPC
}

// NewClientToolResultServer builds the agent-side server. Returns nil when the
// substrate or firer is absent (non-agent binaries / single-node) so the caller
// can skip wiring without a nil-guard at every call site.
func NewClientToolResultServer(substrate DeliverySubstrate, firer ClientToolResultFirer, consumerID string, logger *slog.Logger) *ClientToolResultServer {
	if substrate == nil || firer == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &ClientToolResultServer{
		substrate:  substrate,
		firer:      firer,
		consumerID: consumerID,
		logger:     logger,
		turns:      make(map[string]*servedTurn),
	}
}

// BeginTurn starts serving the client-tool return channel for one agent turn.
// Idempotent per requestId. Safe to call at turn start even if the turn never
// invokes a client tool -- an unused serve loop just polls an empty key and is
// torn down by EndTurn.
func (s *ClientToolResultServer) BeginTurn(ctx context.Context, requestId string) {
	if s == nil || requestId == "" {
		return
	}
	s.mu.Lock()
	if _, ok := s.turns[requestId]; ok {
		s.mu.Unlock()
		return
	}
	serveCtx, cancel := context.WithCancel(ctx)
	// The serve loop's reply key is this turn's key too (it only ever replies,
	// never calls), but SubstrateRPC needs a valid selfKey; reuse the turn key.
	rpc := NewSubstrateRPC(s.substrate, agentTurnKey(requestId), s.consumerID, s.logger)
	st := &servedTurn{cancel: cancel, rpc: rpc}
	s.turns[requestId] = st
	s.mu.Unlock()

	go func() {
		err := rpc.Serve(serveCtx, agentTurnKey(requestId), func(_ context.Context, req RPCRequest) (map[string]any, error) {
			if req.Method != methodClientToolResult {
				return map[string]any{ctKeyFound: false}, nil
			}
			found := s.firer.DeliverClientToolResult(decodeClientToolResult(req.Payload))
			return map[string]any{ctKeyFound: found}, nil
		})
		if err != nil && serveCtx.Err() == nil {
			s.logger.Warn("client-tool result server: serve loop exited",
				"request_id", requestId, "error", err)
		}
	}()
}

// EndTurn stops serving requestId's return channel. Called when the agent turn
// finishes. Idempotent.
func (s *ClientToolResultServer) EndTurn(requestId string) {
	if s == nil || requestId == "" {
		return
	}
	s.mu.Lock()
	st, ok := s.turns[requestId]
	if ok {
		delete(s.turns, requestId)
	}
	s.mu.Unlock()
	if ok {
		st.cancel()
		st.rpc.Close()
	}
}

// ClientToolResultClient is the cognition-side caller. It publishes a
// ClientToolResult to the agent turn's logical key and waits for the agent's
// reply (found/not-found). One client per cognition process; its reply key is
// derived from the cognition node id so replies route back to it across churn.
type ClientToolResultClient struct {
	rpc *SubstrateRPC
}

// NewClientToolResultClient builds the cognition-side client. replyConsumerID
// is the cognition node id (stable per replica so a restart replays unacked
// replies). Returns nil when the substrate is absent.
func NewClientToolResultClient(substrate DeliverySubstrate, replyConsumerID string, logger *slog.Logger) *ClientToolResultClient {
	if substrate == nil {
		return nil
	}
	// The client's reply key is a per-consumer logical key; cognition only ever
	// calls, so it just needs a stable, unique self key to receive replies on.
	selfKey := RoutingKey{Kind: "ctclient", ID: replyConsumerID}
	return &ClientToolResultClient{
		rpc: NewSubstrateRPC(substrate, selfKey, replyConsumerID, logger),
	}
}

// Deliver publishes the ClientToolResult to the agent turn (requestId) and
// blocks until the agent replies or ctx is done. Returns whether a live waiter
// fired, plus any transport error. A nil error with found=false means the
// result reached the agent but the turn no longer had a parked waiter (timed
// out / already resumed) -- a benign, observable outcome, not a delivery
// failure.
func (c *ClientToolResultClient) Deliver(ctx context.Context, requestId string, p ClientToolResultPayload) (found bool, err error) {
	if c == nil || c.rpc == nil {
		return false, nil
	}
	resp, err := c.rpc.Call(ctx, agentTurnKey(requestId), methodClientToolResult, encodeClientToolResult(p))
	if err != nil {
		return false, err
	}
	if resp != nil {
		if v, ok := resp[ctKeyFound].(bool); ok {
			found = v
		}
	}
	return found, nil
}

// Close tears down the cognition-side reply pump. Idempotent.
func (c *ClientToolResultClient) Close() {
	if c != nil && c.rpc != nil {
		c.rpc.Close()
	}
}

// --- envelope (de)serialization between the typed payload and the RPC body ---

func encodeClientToolResult(p ClientToolResultPayload) map[string]any {
	return map[string]any{
		ctKeyCallID:       p.CallID,
		ctKeyContentJSON:  p.ContentJSON,
		ctKeyIsError:      p.IsError,
		ctKeyErrorMessage: p.ErrorMessage,
	}
}

func decodeClientToolResult(body map[string]any) ClientToolResultPayload {
	p := ClientToolResultPayload{}
	if body == nil {
		return p
	}
	if v, ok := body[ctKeyCallID].(string); ok {
		p.CallID = v
	}
	if v, ok := body[ctKeyContentJSON].(string); ok {
		p.ContentJSON = v
	}
	if v, ok := body[ctKeyIsError].(bool); ok {
		p.IsError = v
	}
	if v, ok := body[ctKeyErrorMessage].(string); ok {
		p.ErrorMessage = v
	}
	return p
}
