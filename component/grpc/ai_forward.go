package memql

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/id"
)

// -----------------------------------------------------------------------------
// BFF side: outbound forwarding + response routing
// -----------------------------------------------------------------------------

// AiForwardRouter coordinates outbound AI/voice forwards from a BFF to
// worker peers (Voice / Agent) and routes responses back to the
// originating handler.
//
// One instance lives on the BFF (injected via Server.SetAiForwarder).
// The gRPC AI handlers call Forward() to dispatch a request; the router
// finds a healthy peer of the target node type, wraps the caller's
// original MemqlClientMessage into an AiForwardRequest, sends it, and
// returns a channel that emits each response MemqlServerMessage the
// worker produces. Responses arrive here via Dispatch(), which is hooked
// into the peer connection's inbound message handler.
type AiForwardRouter struct {
	peerMgr *node.PeerManager
	logger  *slog.Logger

	mu       sync.Mutex
	inflight map[string]*inflightEntry
}

// inflightEntry records the live state for a request the BFF has
// forwarded to a worker. `respCh` receives each MemqlServerMessage the
// worker emits; `peer` is the worker we're talking to, cached so that
// continuation messages in a multi-message stream (e.g. streaming
// transcription's Chunk / End envelopes) can Send to the same peer
// without re-running selectPeer.
type inflightEntry struct {
	respCh chan *memqlv1.MemqlServerMessage
	peer   *node.PeerEntry
}

// NewAiForwardRouter constructs an AiForwardRouter. It's a BFF-only
// component; non-BFF binaries leave the field nil on the service.
func NewAiForwardRouter(peerMgr *node.PeerManager, logger *slog.Logger) *AiForwardRouter {
	if logger == nil {
		logger = slog.Default()
	}
	return &AiForwardRouter{
		peerMgr:  peerMgr,
		logger:   logger,
		inflight: make(map[string]*inflightEntry),
	}
}

// Forward sends `envelope` to a healthy peer of `targetType` and returns
// a channel that emits each MemqlServerMessage the worker produces. The
// channel is closed once the worker signals done=true on its
// AiForwardResponse, or when ctx is cancelled.
//
// requestId correlates the response stream; it should match
// envelope.MessageId so the worker's response's correlate_to (and the
// client's expectation) match. On error (no peer available, failed to
// queue on peer connection) the channel is not returned.
func (r *AiForwardRouter) Forward(
	ctx context.Context,
	requestId string,
	targetType node.NodeType,
	authority *auth.ForwardedAuthority,
	partition string,
	envelope *memqlv1.MemqlClientMessage,
) (<-chan *memqlv1.MemqlServerMessage, error) {
	if r == nil {
		return nil, fmt.Errorf("ai forwarder not configured")
	}
	if strings.TrimSpace(requestId) == "" {
		return nil, fmt.Errorf("request_id is required for forwarding")
	}
	// Refuse to put an authority-less request on the wire. The receiver
	// refuses it too -- that is the contract -- but failing here gives the
	// caller a usable error instead of a remote refusal, and keeps a producer
	// that forgot its authority from looking like a mesh outage.
	if err := authority.Validate(time.Now()); err != nil {
		return nil, fmt.Errorf("forward to %s: %w", targetType, err)
	}

	peer, err := r.selectPeer(targetType)
	if err != nil {
		return nil, err
	}

	envBytes, err := proto.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("marshal envelope: %w", err)
	}

	// Allocate response channel BEFORE sending so responses that arrive
	// faster than we can return from this function still find a sink.
	// Capacity 16 balances streaming chat (many chunks) with memory.
	respCh := make(chan *memqlv1.MemqlServerMessage, 16)
	r.mu.Lock()
	if _, exists := r.inflight[requestId]; exists {
		r.mu.Unlock()
		return nil, fmt.Errorf("duplicate in-flight request_id %q", requestId)
	}
	r.inflight[requestId] = &inflightEntry{respCh: respCh, peer: peer}
	r.mu.Unlock()

	// Context watchdog: on cancel, remove the inflight entry and close
	// the response channel so the caller unblocks.
	go func() {
		<-ctx.Done()
		r.cleanupInflight(requestId)
		// Best-effort cancel notification to the peer. The worker will
		// stop producing chunks for this request_id.
		if peer.Connection != nil {
			peer.Connection.Send(&nodev1.NodeClientMessage{
				MessageId: id.NewShortId(),
				Payload: &nodev1.NodeClientMessage_AiForwardCancel{
					AiForwardCancel: &nodev1.AiForwardCancel{RequestId: requestId},
				},
			})
		}
	}()

	// Dispatch the request.
	_ = partition
	fwd := &nodev1.AiForwardRequest{
		RequestId:     requestId,
		Authority:     authorityToProto(authority),
		MemqlEnvelope: envBytes,
	}
	msg := &nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload:   &nodev1.NodeClientMessage_AiForwardRequest{AiForwardRequest: fwd},
	}
	if peer.Connection == nil {
		r.cleanupInflight(requestId)
		return nil, fmt.Errorf("selected peer %s has no active connection", peer.Info.GetNodeId())
	}
	peer.Connection.Send(msg)

	r.logger.Debug("ai forward dispatched",
		"request_id", requestId,
		"target_type", string(targetType),
		"peer_id", peer.Info.GetNodeId(),
	)

	return respCh, nil
}

// Dispatch routes an inbound AiForwardResponse from a worker back to
// the caller's response channel. Installed as a hook on peer connections
// (see node.PeerManager's per-connection onMessage wiring in
// parent_connector.go / direct_peer.go).
func (r *AiForwardRouter) Dispatch(resp *nodev1.AiForwardResponse) {
	if r == nil || resp == nil {
		return
	}
	requestId := resp.GetRequestId()
	r.mu.Lock()
	entry, ok := r.inflight[requestId]
	r.mu.Unlock()
	if !ok {
		// Late response (e.g. worker is slow and we already cancelled)
		// or orphaned response for an unknown request. Drop silently;
		// log at debug so it shows up if we're chasing something.
		r.logger.Debug("ai forward response has no active receiver", "request_id", requestId)
		return
	}

	if len(resp.GetMemqlServerMsg()) > 0 {
		var serverMsg memqlv1.MemqlServerMessage
		if err := proto.Unmarshal(resp.GetMemqlServerMsg(), &serverMsg); err != nil {
			r.logger.Warn("ai forward response unmarshal failed",
				"request_id", requestId, "error", err)
		} else {
			select {
			case entry.respCh <- &serverMsg:
			default:
				// Channel full. Log and drop; indicates the caller is
				// slower than the worker.
				r.logger.Warn("ai forward response channel full, dropping",
					"request_id", requestId)
			}
		}
	}

	if resp.GetDone() {
		r.cleanupInflight(requestId)
	}
}

// cleanupInflight removes the request from the inflight table and
// closes its response channel so the caller unblocks.
func (r *AiForwardRouter) cleanupInflight(requestId string) {
	r.mu.Lock()
	entry, ok := r.inflight[requestId]
	if ok {
		delete(r.inflight, requestId)
	}
	r.mu.Unlock()
	if ok {
		close(entry.respCh)
	}
}

// ForwardContinuation sends an additional envelope on an already-open
// forwarded stream. Multi-message flows (streaming transcription's
// Chunk / End envelopes) use this so every envelope after the initial
// Start lands on the same worker without needing a fresh peer lookup
// or a new inflight entry.
//
// Errors if no inflight entry exists for `requestId` or if the peer's
// outbound connection has gone away since Start.
func (r *AiForwardRouter) ForwardContinuation(
	requestId string,
	authority *auth.ForwardedAuthority,
	partition string,
	envelope *memqlv1.MemqlClientMessage,
) error {
	if r == nil {
		return fmt.Errorf("ai forwarder not configured")
	}
	if strings.TrimSpace(requestId) == "" {
		return fmt.Errorf("request_id is required for forwarding")
	}
	// Same gate as Forward. Continuations used to pass nil claims outright
	// (the planner's pause signal, the client-tool relay); under the contract
	// they assert auth.InternalAuthority() -- "no principal" as a value.
	if err := authority.Validate(time.Now()); err != nil {
		return fmt.Errorf("forward continuation for %q: %w", requestId, err)
	}

	r.mu.Lock()
	entry, ok := r.inflight[requestId]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("no open forward stream for request_id %q", requestId)
	}
	if entry.peer == nil || entry.peer.Connection == nil {
		return fmt.Errorf("peer for request_id %q has no active connection", requestId)
	}

	envBytes, err := proto.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	_ = partition
	entry.peer.Connection.Send(&nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeClientMessage_AiForwardRequest{
			AiForwardRequest: &nodev1.AiForwardRequest{
				RequestId:     requestId,
				Authority:     authorityToProto(authority),
				MemqlEnvelope: envBytes,
			},
		},
	})

	r.logger.Debug("ai forward continuation dispatched",
		"request_id", requestId,
		"peer_id", entry.peer.Info.GetNodeId(),
	)
	return nil
}

// HasInflight reports whether a forward stream is currently open for the
// given request_id. Useful for the BFF-side continuation handlers to
// distinguish "stream never started" from "stream already closed".
func (r *AiForwardRouter) HasInflight(requestId string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	_, ok := r.inflight[requestId]
	r.mu.Unlock()
	return ok
}

// selectPeer picks a healthy peer of the given type. Prefers HEALTHY
// over DEGRADED; errors if no peer is available.
func (r *AiForwardRouter) selectPeer(targetType node.NodeType) (*node.PeerEntry, error) {
	peers := r.peerMgr.ByType(targetType)
	if len(peers) == 0 {
		return nil, fmt.Errorf("no %s node available", targetType)
	}
	// Only peers with a LIVE outbound connection are dispatchable. Stale peers
	// -- dead pods left in the table after a rollout -- keep a HEALTHY status
	// from their last DB heartbeat but have Connection==nil; selecting one
	// fails downstream with "no active connection". Require Connection!=nil so
	// routing is robust against stale-peer accumulation; prefer HEALTHY, fall
	// back to DEGRADED/UNSPECIFIED among the connected set (#1056).
	var degraded *node.PeerEntry
	for _, p := range peers {
		if p == nil || p.Info == nil || p.Connection == nil {
			continue
		}
		switch p.Info.GetHealth() {
		case nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY:
			return p, nil
		case nodev1.NodeHealthStatus_NODE_HEALTH_DEGRADED, nodev1.NodeHealthStatus_NODE_HEALTH_UNSPECIFIED:
			if degraded == nil {
				degraded = p
			}
		}
	}
	if degraded != nil {
		return degraded, nil
	}
	return nil, fmt.Errorf("no connected %s node available", targetType)
}

// -----------------------------------------------------------------------------
// Worker side: inbound forwarding → local handler invocation
// -----------------------------------------------------------------------------

// HandleForwardedRequest is the entry point invoked by the NodeService
// stream_handler when it receives an AiForwardRequest. It unpacks the
// embedded MemqlClientMessage, reconstructs the caller's auth context,
// builds a thin session that sends responses back via AiForwardResponse,
// and dispatches to the same handler the client would have hit directly.
//
// send is the NodeService server's send function (sends NodeServerMessage
// back to the originating BFF).
func (s *service) HandleForwardedRequest(
	ctx context.Context,
	req *nodev1.AiForwardRequest,
	send func(*nodev1.NodeServerMessage) error,
) {
	if req == nil || send == nil {
		return
	}

	requestId := req.GetRequestId()

	// Unmarshal the embedded envelope. This happens before the authority
	// check because the payload TYPE decides whether a refusal may be
	// terminal -- see isContinuationPayload.
	var envelope memqlv1.MemqlClientMessage
	if err := proto.Unmarshal(req.GetMemqlEnvelope(), &envelope); err != nil {
		s.sendForwardError(send, requestId, codes.InvalidArgument,
			"malformed ai forward envelope: "+err.Error(), true)
		return
	}

	// THE REFUSAL. The mesh forwarded-auth contract (memql#3205) requires the
	// sender to assert, explicitly and mandatorily, the authorization decision
	// it already resolved. A request that cannot prove its ceiling was applied
	// is refused -- absence is never read as "not a badge session".
	//
	// This replaces auth.ContextWithForwardedClaims, which attached raw
	// sender-supplied claims and never built an AccessContext at all. Under
	// the deny-on-nil default (memql#2801) that left actor.userId as "" and
	// actor.isClusterOwner as false, so every actor-gated construct executed
	// here silently returned zero rows or wrote createdBy:"". The comment that
	// used to sit on this line claimed worker-side ACLs worked; the ACLs it
	// named were exactly what did not.
	authority := authorityFromProto(req.GetAuthority())
	if err := authority.Validate(time.Now()); err != nil {
		if s.logger != nil {
			s.logger.Warn("ai forward refused: unprovable authority",
				"request_id", requestId, "error", err)
		}
		s.sendForwardError(send, requestId, codes.PermissionDenied,
			"forwarded request refused: "+err.Error(),
			!isContinuationPayload(&envelope))
		return
	}

	sess := s.newForwardedSession(ctx, authority, requestId, send)
	ctx = sess.stream.Context()

	// Dispatch to the appropriate handler based on the envelope payload.
	switch payload := envelope.GetPayload().(type) {
	case *memqlv1.MemqlClientMessage_AiTranscribe:
		_ = sess.handleAiTranscribe(&envelope, payload.AiTranscribe)
	case *memqlv1.MemqlClientMessage_AiTranscribeStreamStart:
		_ = sess.handleAiTranscribeStreamStart(&envelope, payload.AiTranscribeStreamStart)
	case *memqlv1.MemqlClientMessage_AiTranscribeStreamChunk:
		_ = sess.handleAiTranscribeStreamChunk(&envelope, payload.AiTranscribeStreamChunk)
	case *memqlv1.MemqlClientMessage_AiTranscribeStreamEnd:
		_ = sess.handleAiTranscribeStreamEnd(&envelope, payload.AiTranscribeStreamEnd)
	case *memqlv1.MemqlClientMessage_AiSpeech:
		_ = sess.handleAiSpeech(&envelope, payload.AiSpeech)
	case *memqlv1.MemqlClientMessage_AiChat:
		_ = sess.handleAiChat(&envelope, payload.AiChat)
	case *memqlv1.MemqlClientMessage_AiSuggest:
		_ = sess.handleAiSuggest(&envelope, payload.AiSuggest)
	case *memqlv1.MemqlClientMessage_ListTools:
		_ = sess.handleListTools(&envelope, payload.ListTools)
	case *memqlv1.MemqlClientMessage_CallTool:
		_ = sess.handleCallTool(&envelope, payload.CallTool)
	case *memqlv1.MemqlClientMessage_AgentGenerateTurn:
		// Cognition -> agent forward: run the agent replier which streams
		// AgentGenerateTurnDelta + AgentGenerateTurnComplete through the
		// forwardedStream, each wrapped in AiForwardResponse.
		_ = sess.handleAgentGenerateTurn(&envelope, payload.AgentGenerateTurn)
	case *memqlv1.MemqlClientMessage_ClientToolResult:
		// Cluster relay: a client-executed tool ran in the browser and
		// its result was wrapped here (by cognition) so it reaches the
		// agent node's service-scoped waiter. The dispatch below looks
		// the waiter up by call_id and unblocks the original tool loop
		// regardless of which forwarded session delivered it.
		_ = sess.handleClientToolResult(&envelope, payload.ClientToolResult)
	case *memqlv1.MemqlClientMessage_AgentPreemptTurn:
		// Planner "pass" (epic memql#902 / #906): flag the in-flight
		// background turn (keyed by request_id) to stop at its next
		// checkpoint. Fire-and-forget; the turn ends with
		// AgentGenerateTurnComplete.paused=true.
		_ = sess.handleAgentPreemptTurn(&envelope, payload.AgentPreemptTurn)
	default:
		s.sendForwardError(send, requestId, codes.Unimplemented,
			"unsupported ai forward payload type", true)
		return
	}

	// The handlers are async (spawn goroutines). We do NOT send a final
	// "done" marker here because the goroutines send their own final
	// MemqlServerMessage (AiChatResult, AiSpeechResult, etc.), and
	// forwardedStream.Send marks the corresponding AiForwardResponse
	// with done=true when it sees a terminal response type.
}

// newForwardedSession is the ACCEPT-AND-BIND half of the mesh forwarded-auth
// contract: given an authority that has already passed Validate, it produces
// the worker-side session and the context every downstream handler reads.
//
// Extracted from HandleForwardedRequest so it is reachable from a test. It is
// the step that actually fixes memql#3205 -- and the refusal tests cannot
// cover it, because they all return before reaching it. Without direct
// coverage, deleting either the ctx binding or the access seeding below
// restores the original silent-zero-rows defect with every package still
// green. See TestForwardedSessionBindsTheCarriedDecision.
//
// The authority MUST have been validated by the caller; this function binds
// whatever it is handed.
func (s *service) newForwardedSession(
	ctx context.Context,
	authority *auth.ForwardedAuthority,
	requestId string,
	send func(*nodev1.NodeServerMessage) error,
) *streamSession {
	// Bind the actor from the carried DECISION. Nothing is re-derived here:
	// the role arrives post-ceiling, there is no `class` / `role_ceiling` to
	// re-clamp from, and auth.FallbackFromClaims -- which lifts `role`
	// straight off the wire, making IsClusterOwner() true for anyone who says
	// so -- is not reachable on this path at all.
	ctx = auth.ContextWithForwardedAuthority(ctx, authority)

	identity := authority.Identity()
	if identity.Subject == "" {
		// AuthorityKindInternal: accepted, binds no actor by design.
		identity.Subject = "forwarded"
	}

	// A forwardedStream implements MemqlService_StreamServer by wrapping each
	// outbound MemqlServerMessage into an AiForwardResponse delivered via
	// `send`. Its ctx is what handlers read as s.stream.Context().
	fs := &forwardedStream{
		ctx:       ctx,
		requestId: requestId,
		send:      send,
	}

	// The normal newStreamSession() starts a forwardEvents goroutine to relay
	// bus subscriptions to the client; for a forwarded request that loop is
	// unused, so construct the session literal and close its shutdown channel
	// up-front instead.
	sess := &streamSession{
		service:   s,
		stream:    fs,
		logger:    s.logger,
		identity:  identity,
		eventChan: make(chan events.Event, 1),
		closeChan: make(chan struct{}),
	}
	close(sess.closeChan)

	// Seed the resolved access so ensureAccess is a cache hit.
	//
	// This is what removes the per-message userByIdSystem round-trip. A
	// forwarded session is built PER MESSAGE, so it starts with an empty
	// access cache; before this, every forwarded envelope -- including every
	// audio chunk on the streaming-transcription path -- drove a fresh
	// LoadFromClaims -> userByIdSystem query. The direct path caches for the
	// life of the stream; the forward path structurally could not, because
	// there was no stream. Carrying the decision instead of the claims makes
	// the query unnecessary rather than merely cached.
	//
	// accessLoaded is set even when access is nil (AuthorityKindInternal) so
	// ensureAccess cannot fall through to the claims path and resurrect
	// FallbackFromClaims behind our back.
	//
	// badgeExpiresAt is stamped from the authority rather than lazily from a
	// stream context a forwarded session does not meaningfully have, so a
	// badge grant is gated here on the worker exactly as on the direct path.
	sess.accessMu.Lock()
	sess.access = authority.AccessContext()
	sess.accessLoaded = true
	sess.badgeStamped = true
	sess.badgeExpiresAt = authority.BadgeExpires
	sess.accessMu.Unlock()

	return sess
}

// CancelForwardedRequest is the worker-side hook for AiForwardCancel
// messages. For now we just log; the handlers exit naturally when
// their context is cancelled (the NodeService stream's Context()
// cancels when the peer connection drops), and a future refinement
// can register per-request cancels for finer-grained teardown.
func (s *service) CancelForwardedRequest(_ context.Context, requestId string) {
	if s == nil || s.logger == nil {
		return
	}
	s.logger.Debug("ai forward cancel received (best-effort)", "request_id", requestId)
}

// aiForwardHandlerShim defers resolution of the worker-side handler
// until after Run() has populated the service. Registered with the
// NodeServer during bootstrap on worker binaries; the NodeServer reads
// the shim once per inbound AiForwardRequest.
type aiForwardHandlerShim struct {
	ref *serviceRef
}

func (h *aiForwardHandlerShim) HandleForwardedRequest(ctx context.Context, req *nodev1.AiForwardRequest, send func(*nodev1.NodeServerMessage) error) {
	if h == nil || h.ref == nil || h.ref.svc == nil {
		// Service not yet constructed (Run() hasn't fired) or binary
		// doesn't have the worker-side service configured. Surface a
		// clear error so the originating BFF unblocks its client.
		errBytes := encodeForwardErrorBytes(req.GetRequestId(), "ai forward handler not ready")
		_ = send(&nodev1.NodeServerMessage{
			MessageId:   id.NewShortId(),
			CorrelateTo: req.GetRequestId(),
			Payload: &nodev1.NodeServerMessage_AiForwardResponse{
				AiForwardResponse: &nodev1.AiForwardResponse{
					RequestId:      req.GetRequestId(),
					MemqlServerMsg: errBytes,
					Done:           true,
				},
			},
		})
		return
	}
	h.ref.svc.HandleForwardedRequest(ctx, req, send)
}

func (h *aiForwardHandlerShim) CancelForwardedRequest(ctx context.Context, requestId string) {
	if h == nil || h.ref == nil || h.ref.svc == nil {
		return
	}
	h.ref.svc.CancelForwardedRequest(ctx, requestId)
}

// encodeForwardErrorBytes produces a marshaled MemqlServerMessage that
// wraps a QueryError. Used by the shim's early-error path so the wire
// format stays consistent whether the failure originated in the worker's
// AI handler or its bootstrap layer.
func encodeForwardErrorBytes(requestId string, message string) []byte {
	qe := &memqlv1.QueryErrorMsg{
		RequestId: requestId,
		Error: &memqlv1.QueryError{
			Code:    codes.Unavailable.String(),
			Message: message,
		},
	}
	errMsg := &memqlv1.MemqlServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: requestId,
		Payload:     &memqlv1.MemqlServerMessage_QueryError{QueryError: qe},
	}
	b, _ := proto.Marshal(errMsg)
	return b
}

// isContinuationPayload reports whether this envelope continues an
// already-open forwarded stream rather than starting one.
//
// It decides whether an error may be marked terminal. Continuations reuse the
// PARENT turn's request_id, and AiForwardRouter.Dispatch calls cleanupInflight
// on done=true -- which closes the parent's response channel. So a terminal
// error on a continuation kills the in-flight turn the user is watching while
// the agent keeps running: pausing a Plan in cluster mode would blank the
// reply mid-stream. These must fail soft.
func isContinuationPayload(envelope *memqlv1.MemqlClientMessage) bool {
	switch envelope.GetPayload().(type) {
	case *memqlv1.MemqlClientMessage_AiTranscribeStreamChunk,
		*memqlv1.MemqlClientMessage_AiTranscribeStreamEnd,
		*memqlv1.MemqlClientMessage_ClientToolResult,
		*memqlv1.MemqlClientMessage_AgentPreemptTurn:
		return true
	}
	return false
}

// sendForwardError delivers a QueryError-style error back to the BFF.
//
// terminal marks the AiForwardResponse done, which makes the BFF tear the
// inflight entry down and close the caller's response channel. Pass false for
// anything that continues an existing stream -- see isContinuationPayload.
func (s *service) sendForwardError(
	send func(*nodev1.NodeServerMessage) error,
	requestId string,
	code codes.Code,
	message string,
	terminal bool,
) {
	qe := &memqlv1.QueryErrorMsg{
		RequestId: requestId,
		Error: &memqlv1.QueryError{
			Code:    code.String(),
			Message: message,
		},
	}
	errMsg := &memqlv1.MemqlServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: requestId,
		Payload:     &memqlv1.MemqlServerMessage_QueryError{QueryError: qe},
	}
	errBytes, err := proto.Marshal(errMsg)
	if err != nil {
		return
	}
	_ = send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: requestId,
		Payload: &nodev1.NodeServerMessage_AiForwardResponse{
			AiForwardResponse: &nodev1.AiForwardResponse{
				RequestId:      requestId,
				MemqlServerMsg: errBytes,
				Done:           terminal,
			},
		},
	})
}

// -----------------------------------------------------------------------------
// forwardedStream: implements MemqlService_StreamServer for worker-side
// forwarded requests. Send wraps into AiForwardResponse; the other
// methods are no-ops appropriate to a one-shot server-push context.
// -----------------------------------------------------------------------------

type forwardedStream struct {
	ctx       context.Context
	requestId string
	send      func(*nodev1.NodeServerMessage) error
	mu        sync.Mutex
}

func (f *forwardedStream) Send(msg *memqlv1.MemqlServerMessage) error {
	if msg == nil {
		return nil
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	// Mark the AiForwardResponse as terminal when the payload is itself
	// a terminal response type. Streaming chat sends AiChunk for each
	// delta; the terminal is AiChatResult. For non-streaming types the
	// single response is the terminal.
	done := isTerminalServerPayload(msg.GetPayload())

	wrapped := &nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: f.requestId,
		Payload: &nodev1.NodeServerMessage_AiForwardResponse{
			AiForwardResponse: &nodev1.AiForwardResponse{
				RequestId:      f.requestId,
				MemqlServerMsg: b,
				Done:           done,
			},
		},
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.send(wrapped)
}

// Recv on a forwardedStream is undefined; the forward path is server-push
// only. Return EOF immediately so any accidental reader terminates.
func (f *forwardedStream) Recv() (*memqlv1.MemqlClientMessage, error) {
	return nil, io.EOF
}

func (f *forwardedStream) Context() context.Context { return f.ctx }

// The remaining ServerStream methods are satisfied with no-op /
// reasonable defaults. We don't send headers or trailers through the
// inter-node path.
func (f *forwardedStream) SetHeader(_ metadata.MD) error  { return nil }
func (f *forwardedStream) SendHeader(_ metadata.MD) error { return nil }
func (f *forwardedStream) SetTrailer(_ metadata.MD)       {}
func (f *forwardedStream) SendMsg(m any) error {
	msg, ok := m.(*memqlv1.MemqlServerMessage)
	if !ok {
		return status.Errorf(codes.Internal, "forwardedStream expects *MemqlServerMessage, got %T", m)
	}
	return f.Send(msg)
}
func (f *forwardedStream) RecvMsg(_ any) error { return io.EOF }

// isTerminalServerPayload reports whether a MemqlServerMessage payload
// represents the last message for its request (so the enclosing
// AiForwardResponse can carry done=true). Stream chunks are NOT
// terminal; their following AiChatResult is.
func isTerminalServerPayload(p any) bool {
	switch p.(type) {
	case *memqlv1.MemqlServerMessage_AiChatResult,
		*memqlv1.MemqlServerMessage_AiSpeechResult,
		*memqlv1.MemqlServerMessage_AiTranscribeResult,
		*memqlv1.MemqlServerMessage_AiTranscribeStreamComplete,
		*memqlv1.MemqlServerMessage_AiSuggestResult,
		*memqlv1.MemqlServerMessage_ListToolsResult,
		*memqlv1.MemqlServerMessage_CallToolResult,
		// Agent-turn forwarding: Delta is streamed mid-turn (not
		// terminal); Complete is the one-per-turn terminal message
		// that closes out the respCh on the cognition side.
		*memqlv1.MemqlServerMessage_AgentGenerateTurnComplete,
		*memqlv1.MemqlServerMessage_QueryError:
		return true
	}
	return false
}

// Guards — these type assertions run at compile time to ensure
// forwardedStream stays compatible with the gRPC ServerStream interface
// and the MemqlService_StreamServer contract.
var _ memqlv1.MemqlService_StreamServer = (*forwardedStream)(nil)
var _ = status.Code // keep status import if future error paths need it

// -----------------------------------------------------------------------------
// Routing table + proxy entry points invoked by the AI handlers.
// -----------------------------------------------------------------------------

// nodeTargetForChat returns the node type that owns chat completion
// providers. Hardcoded for now; future capability-discovery rework.
func nodeTargetForChat() node.NodeType       { return node.NodeTypeAgent }
func nodeTargetForSuggest() node.NodeType    { return node.NodeTypeAgent }
func nodeTargetForListTools() node.NodeType  { return node.NodeTypeAgent }
func nodeTargetForCallTool() node.NodeType   { return node.NodeTypeAgent }
func nodeTargetForSpeech() node.NodeType     { return node.NodeTypeVoice }
func nodeTargetForTranscribe() node.NodeType { return node.NodeTypeVoice }

// shouldProxyAI reports whether an AI handler should short-circuit
// to the forwarder rather than executing locally. True when:
//   - an AiForwardRouter is installed (BFF binary with at least one
//     worker peer),
//   - a healthy peer of the target type is available, AND
//   - this binary is compiled as a BFF (the compile-time node type
//     short-circuit keeps workers from accidentally forwarding to
//     themselves).
func (s *streamSession) shouldProxyAI(target node.NodeType) bool {
	if s == nil || s.service == nil || s.service.aiForwarder == nil {
		return false
	}
	if node.CompiledNodeType() != node.NodeTypeBFF {
		return false
	}
	// Don't engage the forwarder if no peer of the target type is
	// visible yet. Let the local handler produce its usual
	// "provider unavailable" error instead of a less-friendly
	// "no X node available" proxy error.
	return len(s.service.aiForwarder.peerMgr.ByType(target)) > 0
}

// proxyAI forwards the original envelope to a worker peer and streams
// the worker's responses back to the client on the same stream the
// client is already reading from. Returns nil so the dispatcher moves
// on; any errors are delivered inline as QueryError responses.
func (s *streamSession) proxyAI(envelope *memqlv1.MemqlClientMessage, requestId string, target node.NodeType) error {
	ctx := s.stream.Context()
	correlate := envelope.GetMessageId()

	authority, err := s.forwardedAuthority()
	if err != nil {
		// No provable principal to forward. Fail here rather than ship an
		// authority-less envelope the worker would refuse anyway.
		return s.sendQueryError(requestId, correlate, codes.PermissionDenied, err.Error())
	}
	partition := extractPartitionFromEnvelope(envelope)

	// Carry caller provenance across the hop so any rows the worker
	// writes (tool calls, agent-turn side effects) stamp the same
	// originating context. Receiver-side handlers re-hydrate via
	// contextWithEnvelopeProvenance at handler entry.
	stampEnvelopeProvenance(ctx, envelope)

	respCh, err := s.service.aiForwarder.Forward(ctx, requestId, target, authority, partition, envelope)
	if err != nil {
		return s.sendQueryError(requestId, correlate, codes.Unavailable, err.Error())
	}

	go s.relayForwardedResponses(correlate, requestId, respCh)
	return nil
}

// relayForwardedResponses pumps each MemqlServerMessage from the
// forwarder's channel to the originating client, rewriting the
// CorrelateTo so the message looks like it was produced locally.
func (s *streamSession) relayForwardedResponses(
	correlate string,
	requestId string,
	respCh <-chan *memqlv1.MemqlServerMessage,
) {
	for msg := range respCh {
		if msg == nil {
			continue
		}
		// Rewrite CorrelateTo so the client matches against its
		// original envelope's message_id, not the worker's view.
		msg.CorrelateTo = s.safeCorrelate(correlate)
		if msg.MessageId == "" {
			msg.MessageId = id.NewShortId()
		}
		s.sendMu.Lock()
		err := s.stream.Send(msg)
		s.sendMu.Unlock()
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("ai forward relay send failed",
					"request_id", requestId, "error", err)
			}
			return
		}
	}
	// Channel closed without a terminal response. Surface a clear error
	// so the client's QueryError-listener unblocks instead of hanging.
	if _ = requestId; false {
		// kept for symmetry; no-op (we rely on the forwarder's cleanup
		// to close the channel either on done=true or on ctx cancel).
	}
}

// forwardedAuthority builds this session's assertion for the mesh
// forwarded-auth contract (memql#3205).
//
// THE SOURCE IS THE SESSION, NOT THE STREAM CONTEXT. This is the single
// load-bearing line of the producer side. A gRPC stream's context is fixed at
// stream-open, while badge grants arrive MID-STREAM via RotateAuth --
// handleRotateAuth swaps s.access / s.identity / s.badgeExpiresAt and cannot
// touch the context. Read from the context, a rotated-in badge session would
// forward its PRE-rotation, unclamped role:
//
//	DIRECT     role="reader" isClusterOwner=false
//	FORWARDED  role="owner"  isClusterOwner=true   <- context-sourced
//
// ensureAccess rather than currentAccess: once loaded they return the same
// pointer (handleRotateAuth sets both s.access and s.accessLoaded), but
// currentAccess alone returns nil on a stream whose first message is the
// forwarded one, and forwarding an empty principal is exactly the silent
// zero-rows failure this contract exists to remove.
//
// badgeExpiresAt rides along so the WORKER can enforce expiry. The direct path
// gates every envelope through badgeGate; without the expiry on the wire a
// walked-away kiosk's grant would be rejected on the direct stream and honored
// on every forwarded AiChat / CallTool.
//
// In no-auth dev mode the local-dev stream interceptor synthesises a
// "local-dev" principal; the marker is carried so the provenance is obvious in
// worker logs. It has no authorization meaning.
func (s *streamSession) forwardedAuthority() (*auth.ForwardedAuthority, error) {
	access := s.ensureAccess(s.stream.Context())

	s.accessMu.Lock()
	badgeExpires := s.badgeExpiresAt
	s.accessMu.Unlock()

	localDev := strings.EqualFold(s.identity.Subject, "local-dev")
	authority, err := auth.PrincipalAuthority(access, badgeExpires, localDev)
	if err != nil {
		return nil, err
	}
	// Names come off the session identity, not the AccessContext (which has
	// none). Provenance only -- they feed identity.displayName on rows the
	// worker writes, and no authorization decision reads them.
	return authority.WithDisplayName(s.identity.FirstName, s.identity.LastName), nil
}

// authorityToProto / authorityFromProto convert between the transport-agnostic
// contract type in component/auth and its wire form. The conversion lives here
// so component/auth carries no dependency on the node protos.
func authorityToProto(a *auth.ForwardedAuthority) *nodev1.ForwardedAuthority {
	if a == nil {
		// Deliberately nil, not an empty message: nil is the state the
		// receiver refuses on, and a producer reaching here has already
		// failed Validate.
		return nil
	}
	out := &nodev1.ForwardedAuthority{
		Kind:         string(a.Kind),
		UserId:       a.UserId,
		PrimaryEmail: a.PrimaryEmail,
		Role:         string(a.Role),
		LocalDev:     a.LocalDev,
		FirstName:    a.FirstName,
		LastName:     a.LastName,
	}
	if !a.BadgeExpires.IsZero() {
		out.BadgeExpUnix = a.BadgeExpires.Unix()
	}
	return out
}

func authorityFromProto(p *nodev1.ForwardedAuthority) *auth.ForwardedAuthority {
	if p == nil {
		return nil
	}
	out := &auth.ForwardedAuthority{
		Kind:         auth.AuthorityKind(p.GetKind()),
		UserId:       p.GetUserId(),
		PrimaryEmail: p.GetPrimaryEmail(),
		Role:         auth.Role(p.GetRole()),
		LocalDev:     p.GetLocalDev(),
		FirstName:    p.GetFirstName(),
		LastName:     p.GetLastName(),
	}
	if exp := p.GetBadgeExpUnix(); exp != 0 {
		out.BadgeExpires = time.Unix(exp, 0)
	}
	return out
}

// extractPartitionFromEnvelope is a no-op post-#56 phase 8. The
// partition dimension is gone from the wire; this stub stays for the
// handful of callers that still expect a string return until they're
// swept in a follow-up.
func extractPartitionFromEnvelope(envelope *memqlv1.MemqlClientMessage) string {
	_ = envelope
	return ""
}
