package node

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/id"
)

// SubstrateRPC layers request/response RPC onto the durable DeliverySubstrate
// (memql#1265, Phase 2 of epic memql#1259). The substrate already provides the
// four delivery guarantees (logical addressing, at-least-once + idempotent
// dedup, per-key ordering, replay/catch-up); RPC adds exactly the two things
// the epic's decision 3 names on top of them -- a correlation id and
// reply-routing BY LOGICAL KEY -- and nothing else. No new wire format, no new
// broker: a request is a Deliverable published to the callee's logical key and
// a reply is a Deliverable published to the caller's reply key.
//
// The whole point is that BOTH legs are addressed by logical key, never by a
// physical node-id (epic decision 2). A caller does not say "send the reply to
// bff-pod-3"; it says "send the reply to <replyKey>", and whichever replica
// owns that key -- including a DIFFERENT replica than the one that issued the
// Call, if the caller restarted or the key moved -- replays the reply from its
// cursor and matches it back to the pending call by correlation id. This is the
// star-topology delivery bug removed by construction for the request/response
// pattern, the same way memql#1263 removed it for the event pattern.
//
// Contract realization (all inherited from the substrate, not reinvented):
//
//   - Logical addressing (ADR 4.1): the request rides calleeKey, the reply
//     rides replyKey; neither is a node-id.
//   - At-least-once + idempotent dedup (ADR 4.2): both legs are substrate
//     Publishes carrying a stable EventID, deduped per-subscription so a mesh
//     fast-path copy and the durable pull collapse to one delivery.
//   - Per-key ordering (ADR 4.3): inherited; RPC does not rely on cross-call
//     ordering, only on the substrate delivering each reply at least once.
//   - Replay/catch-up (ADR 4.4): a reply produced while the caller's reply-key
//     owner is mid-reconnect is replayed from the cursor when the owner
//     returns -- this is what makes a reply survive a caller-side restart.
//
// SubstrateRPC adds ONLY: the correlation id, the reply-key plumbing, and an
// in-process pending-waiter table keyed by correlation id. At-most-once
// semantics for the CALLER are achieved by the waiter firing exactly once (the
// first delivered reply for a correlation id wins; the substrate's per-sub
// dedup already collapses duplicate copies of the same reply EventID, and the
// waiter is removed on first match so a late genuinely-distinct replay is
// dropped as "no waiter").
type SubstrateRPC struct {
	substrate DeliverySubstrate
	// selfKey is this process's reply address: every Call routes its reply to
	// selfKey, and the caller side keeps exactly one Subscribe open on it for
	// the lifetime of the RPC client. Logical, never a node-id.
	selfKey RoutingKey
	// consumerID is the logical consumer identity used for the reply-key
	// subscription's cursor. It is stable per logical caller so a restart
	// resumes the same cursor and replays unacked replies (ADR 4.4); it is NOT
	// a physical node-id for addressing purposes (addressing is selfKey).
	consumerID string
	logger     *slog.Logger
	now        func() time.Time

	mu      sync.Mutex
	pending map[string]*pendingCall // correlationID -> waiter
	started bool
	stop    context.CancelFunc
}

// pendingCall is one in-flight Call awaiting its reply. respCh receives the
// single reply payload (buffered so the reply-pump never blocks on a caller
// that has already given up via ctx).
type pendingCall struct {
	respCh chan rpcReply
}

// rpcReply is the delivered outcome of a Call: the handler's response payload,
// or an error string the callee chose to surface.
type rpcReply struct {
	payload map[string]any
	errMsg  string
}

// RPCHandler processes one inbound request and returns the response payload, or
// an error. A non-nil error is surfaced to the caller as a typed RPC error
// reply (the request still gets a reply -- the caller never hangs on a handler
// failure). method + correlationID are passed through for routing/audit.
type RPCHandler func(ctx context.Context, req RPCRequest) (map[string]any, error)

// RPCRequest is the decoded inbound request a Serve handler sees.
type RPCRequest struct {
	Method        string         // logical method name (handler dispatch key)
	CorrelationID string         // echoed onto the reply
	ReplyKey      RoutingKey     // where the reply is published (caller's key)
	Payload       map[string]any // request arguments
	OriginNode    string         // producer node id; audit only
}

// RPC envelope payload keys. The request/reply ride inside the substrate
// Deliverable's Payload map under these reserved keys, leaving the rest of the
// map for the caller's own arguments/response body. Reserved so an application
// payload can never collide with the RPC plumbing.
const (
	rpcKeyKind          = "__rpc_kind"     // "request" | "reply"
	rpcKeyCorrelationID = "__rpc_corr"     // correlation id (string)
	rpcKeyMethod        = "__rpc_method"   // method name (request leg)
	rpcKeyReplyKind     = "__rpc_reply_kk" // reply key Kind (request leg)
	rpcKeyReplyID       = "__rpc_reply_id" // reply key ID   (request leg)
	rpcKeyError         = "__rpc_error"    // error message (reply leg, optional)
	rpcKeyBody          = "__rpc_body"     // application payload (map)

	rpcKindRequest = "request"
	rpcKindReply   = "reply"

	// rpcTopic tags RPC deliverables on the substrate so they are
	// distinguishable from event-pattern deliverables on the same key in logs
	// and (future) routing. It is not used for addressing.
	rpcTopic = "mesh.rpc.v1"
)

// NewSubstrateRPC builds an RPC client/server over the substrate. selfKey is
// this process's logical reply address; consumerID is the stable logical
// consumer id used for the reply-key cursor (mirror the caller's stable
// identity so a restart replays unacked replies). The reply pump is started
// lazily on the first Call/Serve via ensureStarted, bound to the context passed
// there, so a never-used RPC client opens no subscription.
func NewSubstrateRPC(substrate DeliverySubstrate, selfKey RoutingKey, consumerID string, logger *slog.Logger) *SubstrateRPC {
	if logger == nil {
		logger = slog.Default()
	}
	return &SubstrateRPC{
		substrate:  substrate,
		selfKey:    selfKey,
		consumerID: consumerID,
		logger:     logger,
		now:        time.Now,
		pending:    make(map[string]*pendingCall),
	}
}

// Call issues a request to calleeKey and blocks until the matching reply lands
// on selfKey (matched by correlation id) or ctx is done. The reply is routed by
// LOGICAL KEY: Call publishes the request carrying selfKey as the reply key, so
// the callee's reply finds whichever replica owns selfKey -- which may differ
// from this one if the key moves. Returns the response payload, or an error
// (ctx cancellation/timeout, a handler error surfaced by the callee, or a
// publish failure).
//
// At-least-once on the wire, at-most-once for the caller: the substrate may
// deliver the reply more than once (durable pull + mesh fast-path), but the
// per-subscription dedup collapses copies of the same EventID and the waiter
// fires exactly once on the first match, after which it is removed.
func (r *SubstrateRPC) Call(ctx context.Context, calleeKey RoutingKey, method string, payload map[string]any) (map[string]any, error) {
	if r == nil || r.substrate == nil {
		return nil, fmt.Errorf("substrate rpc: not configured")
	}
	if !calleeKey.Valid() {
		return nil, fmt.Errorf("substrate rpc: invalid callee key %q", calleeKey.String())
	}
	if !r.selfKey.Valid() {
		return nil, fmt.Errorf("substrate rpc: invalid self/reply key %q", r.selfKey.String())
	}

	if err := r.ensureStarted(ctx); err != nil {
		return nil, err
	}

	correlationID := id.NewShortId()
	respCh := make(chan rpcReply, 1)
	r.mu.Lock()
	r.pending[correlationID] = &pendingCall{respCh: respCh}
	r.mu.Unlock()
	// Always clear the waiter on the way out so a ctx-cancelled call can't leak
	// its entry; a reply arriving after this is harmlessly dropped as "no
	// waiter".
	defer r.clearPending(correlationID)

	req := Deliverable{
		EventID:    "rpc-req-" + correlationID,
		Key:        calleeKey,
		Topic:      rpcTopic,
		Kind:       events.KindMessage,
		Payload:    r.encodeRequest(correlationID, method, payload),
		OriginNode: r.consumerID,
	}
	if _, err := r.substrate.Publish(ctx, req); err != nil {
		return nil, fmt.Errorf("substrate rpc: publish request to %s: %w", calleeKey.String(), err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case reply := <-respCh:
		if reply.errMsg != "" {
			return reply.payload, fmt.Errorf("substrate rpc: remote error: %s", reply.errMsg)
		}
		return reply.payload, nil
	}
}

// Serve subscribes to ownKey and dispatches every inbound request to handler,
// publishing the handler's result (or error) back to the request's reply key
// with the same correlation id. It runs until ctx is cancelled. ownKey is the
// logical address callers target with Call; consumerID for the request-side
// subscription is r.consumerID, so a Serve restart replays unacked requests
// from its cursor (a request produced while the handler replica was mid-restart
// is not lost -- ADR 4.4). Blocking; run it in a goroutine.
func (r *SubstrateRPC) Serve(ctx context.Context, ownKey RoutingKey, handler RPCHandler) error {
	if r == nil || r.substrate == nil {
		return fmt.Errorf("substrate rpc: not configured")
	}
	if !ownKey.Valid() {
		return fmt.Errorf("substrate rpc: invalid serve key %q", ownKey.String())
	}
	if handler == nil {
		return fmt.Errorf("substrate rpc: nil handler")
	}

	stream, err := r.substrate.Subscribe(ctx, ownKey, r.consumerID)
	if err != nil {
		return fmt.Errorf("substrate rpc: subscribe %s: %w", ownKey.String(), err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-stream:
			if !ok {
				return ctx.Err()
			}
			// Only act on REQUEST deliverables; a reply that happens to share
			// this key (a process that both serves and calls on the same key)
			// is not ours to dispatch.
			if kindOf(d.Payload) != rpcKindRequest {
				// Ack so the request-side cursor still advances past a foreign
				// row; otherwise a non-request row on this key would wedge
				// replay.
				r.ackBestEffort(ctx, ownKey, d.Seq)
				continue
			}
			r.dispatchRequest(ctx, ownKey, d, handler)
		}
	}
}

// dispatchRequest decodes one request, runs the handler, publishes the reply to
// the request's reply key, and advances the request-side cursor. A handler
// panic or error becomes a typed error reply so the caller always unblocks.
func (r *SubstrateRPC) dispatchRequest(ctx context.Context, ownKey RoutingKey, d Deliverable, handler RPCHandler) {
	req, err := r.decodeRequest(d)
	if err != nil {
		r.logger.Warn("substrate rpc: undecodable request dropped",
			"key", ownKey.String(), "seq", d.Seq, "error", err)
		r.ackBestEffort(ctx, ownKey, d.Seq)
		return
	}

	resp, herr := r.runHandler(ctx, req, handler)

	// Publish the reply to the CALLER's reply key (logical addressing of the
	// return leg). The reply EventID is derived from the correlation id so a
	// re-dispatched request (at-least-once on the request leg) produces an
	// IDEMPOTENT reply -- the substrate's unique-event-id guard collapses the
	// duplicate reply rather than double-delivering it to the caller.
	reply := Deliverable{
		EventID:    "rpc-rep-" + req.CorrelationID,
		Key:        req.ReplyKey,
		Topic:      rpcTopic,
		Kind:       events.KindMessage,
		Payload:    r.encodeReply(req.CorrelationID, resp, herr),
		OriginNode: r.consumerID,
	}
	if !req.ReplyKey.Valid() {
		r.logger.Warn("substrate rpc: request has no reply key; dropping reply",
			"key", ownKey.String(), "corr", req.CorrelationID)
	} else if _, perr := r.substrate.Publish(ctx, reply); perr != nil {
		r.logger.Warn("substrate rpc: publish reply failed",
			"reply_key", req.ReplyKey.String(), "corr", req.CorrelationID, "error", perr)
	}

	// Advance the request-side cursor past this request so a Serve restart does
	// not re-run it. The reply is idempotent (same EventID) even if a crash
	// between publish and Ack causes a re-dispatch, so this Ack is safe to do
	// after the reply publish.
	r.ackBestEffort(ctx, ownKey, d.Seq)
}

// runHandler invokes the handler with panic-safety so a buggy handler can never
// take down the Serve loop or leave the caller hanging.
func (r *SubstrateRPC) runHandler(ctx context.Context, req RPCRequest, handler RPCHandler) (resp map[string]any, err error) {
	defer func() {
		if p := recover(); p != nil {
			resp = nil
			err = fmt.Errorf("substrate rpc: handler panic: %v", p)
			r.logger.Error("substrate rpc: handler panic recovered",
				"method", req.Method, "corr", req.CorrelationID, "panic", p)
		}
	}()
	return handler(ctx, req)
}

// ensureStarted lazily opens the single reply-key subscription that the caller
// side drains for the life of the client. It is idempotent. The reply pump runs
// on its OWN background context until Close() is called -- deliberately NOT tied
// to the first Call's context, because the pump is shared across every
// concurrent and subsequent Call on this client; binding it to one Call's
// context would tear the pump down (and silently drop other callers' replies)
// the moment that one Call returned. Lifetime is the client's, ended explicitly
// via Close() at app shutdown.
//
// The passed ctx is used only to short-circuit the initial Subscribe if the
// first caller has already given up; the resulting pump does not inherit it.
func (r *SubstrateRPC) ensureStarted(ctx context.Context) error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	pumpCtx, cancel := context.WithCancel(context.Background())
	stream, err := r.substrate.Subscribe(pumpCtx, r.selfKey, r.consumerID)
	if err != nil {
		cancel()
		return fmt.Errorf("substrate rpc: subscribe reply key %s: %w", r.selfKey.String(), err)
	}

	r.mu.Lock()
	if r.started {
		// Lost the race; another Call started the pump first. Tear down ours.
		r.mu.Unlock()
		cancel()
		return nil
	}
	r.started = true
	r.stop = cancel
	r.mu.Unlock()

	go r.pumpReplies(pumpCtx, stream)
	_ = ctx // intentionally not propagated to the shared pump (see doc above)
	return nil
}

// pumpReplies drains the reply-key subscription, matching each reply to its
// pending Call by correlation id and advancing the reply-side cursor. A reply
// with no waiter (the caller already returned, or this is a duplicate/late
// replay the waiter already consumed) is dropped after acking so the cursor
// still advances.
func (r *SubstrateRPC) pumpReplies(ctx context.Context, stream <-chan Deliverable) {
	for {
		select {
		case <-ctx.Done():
			return
		case d, ok := <-stream:
			if !ok {
				return
			}
			if kindOf(d.Payload) != rpcKindReply {
				r.ackBestEffort(ctx, r.selfKey, d.Seq)
				continue
			}
			corr := stringOf(d.Payload, rpcKeyCorrelationID)
			r.mu.Lock()
			waiter, ok := r.pending[corr]
			if ok {
				delete(r.pending, corr)
			}
			r.mu.Unlock()
			if ok {
				// Non-blocking: respCh is buffered(1) and the waiter is unique,
				// so this never blocks; the select guards against a torn-down
				// pump.
				select {
				case waiter.respCh <- rpcReply{
					payload: bodyOf(d.Payload),
					errMsg:  stringOf(d.Payload, rpcKeyError),
				}:
				default:
				}
			}
			// Advance the reply cursor whether or not a waiter matched: a reply
			// is single-shot per correlation id, so once consumed it must not
			// be re-delivered to a future, unrelated waiter on the same key.
			r.ackBestEffort(ctx, r.selfKey, d.Seq)
		}
	}
}

// Close tears down the reply pump. Idempotent. Safe to call from app shutdown.
func (r *SubstrateRPC) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	stop := r.stop
	r.stop = nil
	r.started = false
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (r *SubstrateRPC) clearPending(correlationID string) {
	r.mu.Lock()
	delete(r.pending, correlationID)
	r.mu.Unlock()
}

func (r *SubstrateRPC) ackBestEffort(ctx context.Context, key RoutingKey, seq int64) {
	if err := r.substrate.Ack(ctx, key, r.consumerID, seq); err != nil && ctx.Err() == nil {
		r.logger.Warn("substrate rpc: ack failed",
			"key", key.String(), "seq", seq, "error", err)
	}
}

// --- envelope encode/decode -------------------------------------------------

func (r *SubstrateRPC) encodeRequest(correlationID, method string, body map[string]any) map[string]any {
	return map[string]any{
		rpcKeyKind:          rpcKindRequest,
		rpcKeyCorrelationID: correlationID,
		rpcKeyMethod:        method,
		rpcKeyReplyKind:     r.selfKey.Kind,
		rpcKeyReplyID:       r.selfKey.ID,
		rpcKeyBody:          body,
	}
}

func (r *SubstrateRPC) encodeReply(correlationID string, body map[string]any, herr error) map[string]any {
	out := map[string]any{
		rpcKeyKind:          rpcKindReply,
		rpcKeyCorrelationID: correlationID,
		rpcKeyBody:          body,
	}
	if herr != nil {
		out[rpcKeyError] = herr.Error()
	}
	return out
}

func (r *SubstrateRPC) decodeRequest(d Deliverable) (RPCRequest, error) {
	corr := stringOf(d.Payload, rpcKeyCorrelationID)
	if corr == "" {
		return RPCRequest{}, fmt.Errorf("missing correlation id")
	}
	replyKey := RoutingKey{
		Kind: stringOf(d.Payload, rpcKeyReplyKind),
		ID:   stringOf(d.Payload, rpcKeyReplyID),
	}
	return RPCRequest{
		Method:        stringOf(d.Payload, rpcKeyMethod),
		CorrelationID: corr,
		ReplyKey:      replyKey,
		Payload:       bodyOf(d.Payload),
		OriginNode:    d.OriginNode,
	}, nil
}

// kindOf reads the RPC envelope discriminator. A payload without it is foreign
// (an event-pattern deliverable sharing the key) and reported as empty.
func kindOf(payload map[string]any) string { return stringOf(payload, rpcKeyKind) }

func stringOf(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// bodyOf extracts the application payload from an RPC envelope. Returns nil when
// absent so callers see an empty result rather than a malformed map.
func bodyOf(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	if v, ok := m[rpcKeyBody].(map[string]any); ok {
		return v
	}
	return nil
}
