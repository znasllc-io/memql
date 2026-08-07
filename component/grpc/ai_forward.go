package memql

import (
	"context"
	"errors"
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
	"github.com/znasllc-io/memql/component/metrics"
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
// `principal` is the mandatory authority assertion (memql#3205). It is ONE
// value carrying both the assertion and its derived claims, so a call site
// cannot ship claims without an authority -- which is what makes the defect
// this contract exists to fix inexpressible rather than merely discouraged.
func (r *AiForwardRouter) Forward(
	ctx context.Context,
	requestId string,
	targetType node.NodeType,
	principal auth.ForwardedPrincipal,
	envelope *memqlv1.MemqlClientMessage,
) (<-chan *memqlv1.MemqlServerMessage, error) {
	if r == nil {
		return nil, fmt.Errorf("ai forwarder not configured")
	}
	if strings.TrimSpace(requestId) == "" {
		return nil, fmt.Errorf("request_id is required for forwarding")
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
	fwd := &nodev1.AiForwardRequest{
		RequestId:     requestId,
		Auth:          principal.Claims,
		MemqlEnvelope: envBytes,
		Authority:     forwardedAuthorityToProto(principal.Authority, r.selfNodeId(), r.selfNodeType()),
		Continuation:  false,
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
	principal auth.ForwardedPrincipal,
	envelope *memqlv1.MemqlClientMessage,
) error {
	if r == nil {
		return fmt.Errorf("ai forwarder not configured")
	}
	if strings.TrimSpace(requestId) == "" {
		return fmt.Errorf("request_id is required for forwarding")
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

	entry.peer.Connection.Send(&nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeClientMessage_AiForwardRequest{
			AiForwardRequest: &nodev1.AiForwardRequest{
				RequestId:     requestId,
				Auth:          principal.Claims,
				MemqlEnvelope: envBytes,
				Authority:     forwardedAuthorityToProto(principal.Authority, r.selfNodeId(), r.selfNodeType()),
				// A continuation shares the PARENT's request_id, so the
				// receiver must never answer one on the refusal path -- any
				// AiForwardResponse on that id reaches the producer's Dispatch
				// and can close the parent turn's channel. See refuseForward.
				Continuation: true,
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

// selfNodeId / selfNodeType report the forwarding node's own identity, stamped
// onto every assertion by the ROUTER rather than by each producer -- audit
// provenance no call site can forget. Empty when the peer manager has not
// resolved an identity yet; the fields are audit-only and never inputs to a
// decision, so an empty value never changes a verdict.
func (r *AiForwardRouter) selfNodeId() string {
	if r == nil || r.peerMgr == nil {
		return ""
	}
	return r.peerMgr.SelfNodeId()
}

func (r *AiForwardRouter) selfNodeType() string {
	if r == nil || r.peerMgr == nil {
		return ""
	}
	return r.peerMgr.SelfNodeType()
}

// forwardedAuthorityToProto / forwardedAuthorityFromProto are the ONLY
// adaptation between the wire message and component/auth's plain-Go type.
// component/auth cannot import component/node (node -> identity/verifier ->
// auth already exists), so the grpc layer owns the mapping.
func forwardedAuthorityToProto(a auth.ForwardedAuthority, originNodeId, originNodeType string) *nodev1.ForwardedAuthority {
	out := &nodev1.ForwardedAuthority{
		ContractVersion: a.Version,
		Subject:         a.Subject,
		PrimaryEmail:    a.PrimaryEmail,
		Role:            string(a.Role),
		IdentityId:      a.IdentityId,
		CredentialClass: a.CredentialClass,
		RoleCeiling:     string(a.RoleCeiling),
		OriginNodeId:    originNodeId,
		OriginNodeType:  originNodeType,
	}
	switch a.Kind {
	case auth.ForwardedPrincipalUser:
		out.PrincipalKind = nodev1.ForwardedPrincipalKind_FORWARDED_PRINCIPAL_KIND_USER
	case auth.ForwardedPrincipalSystem:
		out.PrincipalKind = nodev1.ForwardedPrincipalKind_FORWARDED_PRINCIPAL_KIND_SYSTEM
	default:
		out.PrincipalKind = nodev1.ForwardedPrincipalKind_FORWARDED_PRINCIPAL_KIND_UNSPECIFIED
	}
	if !a.ExpiresAt.IsZero() {
		out.ExpiresAtUnix = a.ExpiresAt.Unix()
	}
	if !a.AssertedAt.IsZero() {
		out.AssertedAtUnix = a.AssertedAt.Unix()
	}
	return out
}

func forwardedAuthorityFromProto(p *nodev1.ForwardedAuthority) auth.ForwardedAuthority {
	if p == nil {
		// The zero value, which VerifyForwardedAuthority refuses. An ABSENT
		// assertion and a malformed one take the same path deliberately: the
		// contract's premise is that absence is never read as safe.
		return auth.ForwardedAuthority{}
	}
	a := auth.ForwardedAuthority{
		Version:         p.GetContractVersion(),
		Subject:         p.GetSubject(),
		PrimaryEmail:    p.GetPrimaryEmail(),
		Role:            auth.Role(p.GetRole()),
		IdentityId:      p.GetIdentityId(),
		CredentialClass: p.GetCredentialClass(),
		RoleCeiling:     auth.Role(p.GetRoleCeiling()),
		OriginNodeId:    p.GetOriginNodeId(),
		OriginNodeType:  p.GetOriginNodeType(),
	}
	switch p.GetPrincipalKind() {
	case nodev1.ForwardedPrincipalKind_FORWARDED_PRINCIPAL_KIND_USER:
		a.Kind = auth.ForwardedPrincipalUser
	case nodev1.ForwardedPrincipalKind_FORWARDED_PRINCIPAL_KIND_SYSTEM:
		a.Kind = auth.ForwardedPrincipalSystem
	}
	if v := p.GetExpiresAtUnix(); v > 0 {
		a.ExpiresAt = time.Unix(v, 0)
	}
	if v := p.GetAssertedAtUnix(); v > 0 {
		a.AssertedAt = time.Unix(v, 0)
	}
	return a
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

	// Unmarshal the embedded envelope FIRST, so the refusal shape below is
	// decided by this node's own payload-class table rather than by a
	// producer-supplied bool. The wire `continuation` flag is the fallback for
	// exactly the case where that is impossible -- an envelope that will not
	// parse -- and a cross-check otherwise. A mislabelled opener would
	// otherwise hang the caller forever; a mislabelled continuation would
	// re-create the parent-turn kill this contract exists to avoid.
	var envelope memqlv1.MemqlClientMessage
	envelopeOK := proto.Unmarshal(req.GetMemqlEnvelope(), &envelope) == nil
	isContinuation := req.GetContinuation()
	payloadClass, payloadKnown := forwardPayloadClass(envelope.GetPayload())
	if envelopeOK && payloadKnown {
		isContinuation = payloadClass == forwardPayloadContinuation
	}

	// THE GATE (memql#3205). Verify BEFORE anything else runs: the authority is
	// decidable without the envelope, and nothing -- not even a malformed-body
	// error -- should be answered on behalf of a caller whose authority we
	// cannot establish.
	authority := forwardedAuthorityFromProto(req.GetAuthority())
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	if err != nil {
		metrics.AuthReject(metrics.SurfaceNodeForward, forwardRefusalReason(err), codes.PermissionDenied.String())
		if s.logger != nil {
			s.logger.Warn("mesh forward refused: unprovable authority",
				"request_id", requestId,
				"reason", err,
				"origin_node_id", authority.OriginNodeId,
				"origin_node_type", authority.OriginNodeType,
				"continuation", isContinuation)
		}
		code := codes.PermissionDenied
		if errors.Is(err, auth.ErrForwardAuthorityExpired) {
			// Match the direct path's framing for an expired grant.
			code = codes.Unauthenticated
		}
		s.refuseForward(send, requestId, isContinuation, code, "forwarded_authority_refused: "+err.Error())
		return
	}

	if !envelopeOK {
		s.refuseForward(send, requestId, isContinuation, codes.InvalidArgument, "malformed ai forward envelope")
		return
	}
	if !payloadKnown {
		s.refuseForward(send, requestId, isContinuation, codes.Unimplemented, "unsupported ai forward payload type")
		return
	}

	// The two carriers must not drift. `auth` exists for createdBy attribution
	// and is derived from the same value as the assertion, so a disagreement
	// means one of them was populated independently -- exactly the shape that
	// lets a future edit reintroduce a claims-only forward.
	if sub, ok := req.GetAuth()["sub"]; ok && sub != authority.Subject {
		metrics.AuthReject(metrics.SurfaceNodeForward, metrics.ReasonForwardPrincipalMismatch, codes.PermissionDenied.String())
		s.refuseForward(send, requestId, isContinuation, codes.PermissionDenied,
			"forwarded_authority_refused: claims subject disagrees with the asserted principal")
		return
	}

	// Three stamps, three distinct jobs.
	//   - client origin: this work originated with a client request, not with
	//     a server-initiated sweep (#2889).
	//   - forwarded claims: TokenInfo for `createdBy` ATTRIBUTION only. Never
	//     an authorization input any more.
	//   - access: THE authorization decision, and it comes from the verified
	//     assertion alone -- no LoadFromClaims, no FallbackFromClaims, no
	//     userByIdSystem keyed by a wire-supplied subject, and therefore no
	//     per-message DB round trip either. The comment this replaced claimed
	//     "so worker-side ACLs work" while setting no AccessContext at all,
	//     which is memql#2876.
	ctx = auth.ContextWithClientOrigin(ctx)
	ctx = auth.ContextWithForwardedClaims(ctx, req.GetAuth())
	ctx = auth.ContextWithAccess(ctx, access)

	identity, _ := auth.UserIdentityFromContext(ctx)
	if identity.Subject == "" {
		// Unreachable: the assertion guarantees a subject and the claims are
		// derived from it. Kept as a belt-and-braces floor rather than the
		// former "forwarded" placeholder, which papered over a missing actor.
		identity.Subject = authority.Subject
	}

	// Build a forwardedStream that implements MemqlService_StreamServer
	// by wrapping each outbound MemqlServerMessage into an
	// AiForwardResponse delivered via `send`.
	fs := &forwardedStream{
		ctx:       ctx,
		requestId: requestId,
		send:      send,
	}

	// Build a minimal streamSession over the forwarded stream. The
	// normal newStreamSession() starts a forwardEvents goroutine to
	// relay bus subscriptions to the client; for a forwarded request
	// that loop is unused, so we construct the session literal and
	// close its shutdown channel up-front instead.
	sess := &streamSession{
		service:   s,
		stream:    fs,
		logger:    s.logger,
		identity:  identity,
		eventChan: make(chan events.Event, 1),
		closeChan: make(chan struct{}),
	}
	close(sess.closeChan)

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
			"unsupported ai forward payload type")
		return
	}

	// The handlers are async (spawn goroutines). We do NOT send a final
	// "done" marker here because the goroutines send their own final
	// MemqlServerMessage (AiChatResult, AiSpeechResult, etc.), and
	// forwardedStream.Send marks the corresponding AiForwardResponse
	// with done=true when it sees a terminal response type.
}

// forwardPayloadKind classifies a forwarded payload as one that OPENS a
// forwarded stream or one that CONTINUES an already-open one. The distinction
// decides the refusal shape, so it is derived from the payload type this node
// reads rather than trusted from the wire.
type forwardPayloadKind int

const (
	forwardPayloadOpener forwardPayloadKind = iota
	forwardPayloadContinuation
)

// forwardPayloadClass mirrors the dispatch switch in HandleForwardedRequest.
// TestForwardPayloadClassCoversTheDispatchSwitch pins the two together, so a
// payload added to one and not the other is a test failure rather than a
// silently mis-shaped refusal.
func forwardPayloadClass(payload any) (forwardPayloadKind, bool) {
	switch payload.(type) {
	case *memqlv1.MemqlClientMessage_AiTranscribe,
		*memqlv1.MemqlClientMessage_AiTranscribeStreamStart,
		*memqlv1.MemqlClientMessage_AiSpeech,
		*memqlv1.MemqlClientMessage_AiChat,
		*memqlv1.MemqlClientMessage_AiSuggest,
		*memqlv1.MemqlClientMessage_ListTools,
		*memqlv1.MemqlClientMessage_CallTool,
		*memqlv1.MemqlClientMessage_AgentGenerateTurn:
		return forwardPayloadOpener, true
	case *memqlv1.MemqlClientMessage_AiTranscribeStreamChunk,
		*memqlv1.MemqlClientMessage_AiTranscribeStreamEnd,
		*memqlv1.MemqlClientMessage_ClientToolResult,
		*memqlv1.MemqlClientMessage_AgentPreemptTurn:
		return forwardPayloadContinuation, true
	default:
		return forwardPayloadOpener, false
	}
}

// refuseForward delivers a refusal in the ONE shape that cannot damage the
// caller.
//
// A continuation shares the PARENT turn's request_id. Any AiForwardResponse on
// that id reaches the producer's AiForwardRouter.Dispatch, and:
//
//   - done=true calls cleanupInflight, which deletes the entry and CLOSES the
//     parent turn's response channel; and
//   - even done=false delivers a QueryError, which both consumers treat as
//     fatal for the turn.
//
// sendForwardError hardcodes done=true, so answering a continuation at all
// would kill the parent turn -- that is the live breakage the epic identified:
// pausing a Plan in cluster mode would end the turn while the agent kept
// running. Dropping is the only shape that leaves the parent alive.
//
// This is an availability trade, deliberately taken: a badge expiring mid
// transcription stops the transcript with no error frame rather than tearing
// down the turn. That matches badgeGate, which also only gates. The refusal is
// never silent to an OPERATOR -- it is logged and counted above.
func (s *service) refuseForward(
	send func(*nodev1.NodeServerMessage) error,
	requestId string,
	isContinuation bool,
	code codes.Code,
	message string,
) {
	if isContinuation {
		return
	}
	s.sendForwardError(send, requestId, code, message)
}

// forwardRefusalReason maps a verifier sentinel to a stable metric label.
func forwardRefusalReason(err error) string {
	switch {
	case errors.Is(err, auth.ErrForwardMissingClass), errors.Is(err, auth.ErrForwardUnknownClass):
		return metrics.ReasonForwardAuthorityMissing
	case errors.Is(err, auth.ErrForwardCeilingNotApplied),
		errors.Is(err, auth.ErrForwardBadgeMissingCeiling),
		errors.Is(err, auth.ErrForwardStrayCeiling):
		return metrics.ReasonForwardCeilingNotApplied
	case errors.Is(err, auth.ErrForwardAuthorityExpired), errors.Is(err, auth.ErrForwardBadgeMissingExpiry):
		return metrics.ReasonForwardExpired
	case errors.Is(err, auth.ErrForwardSystemRoleTooHigh), errors.Is(err, auth.ErrForwardSystemImpersonatesUser),
		errors.Is(err, auth.ErrForwardUserClaimsSystemClass):
		return metrics.ReasonForwardPrincipalMismatch
	case errors.Is(err, auth.ErrForwardUnsupportedContract):
		return metrics.ReasonForwardUnsupportedContract
	default:
		return metrics.ReasonForwardAuthorityMissing
	}
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

// sendForwardError delivers a QueryError-style error back to the BFF.
func (s *service) sendForwardError(
	send func(*nodev1.NodeServerMessage) error,
	requestId string,
	code codes.Code,
	message string,
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
				Done:           true,
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

	// Refuse LOCALLY rather than shipping a forward whose authority the
	// receiver would reject: the caller gets a real error instead of a hang,
	// and an unprovable assertion never reaches the wire (memql#3205).
	principal, err := s.forwardedPrincipal()
	if err != nil {
		return s.sendQueryError(requestId, correlate, codes.Internal,
			"cannot establish forwarded authority for this session: "+err.Error())
	}

	// Carry caller provenance across the hop so any rows the worker
	// writes (tool calls, agent-turn side effects) stamp the same
	// originating context. Receiver-side handlers re-hydrate via
	// contextWithEnvelopeProvenance at handler entry.
	stampEnvelopeProvenance(ctx, envelope)

	respCh, err := s.service.aiForwarder.Forward(ctx, requestId, target, principal, envelope)
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

// forwardedPrincipal builds the mesh authority assertion from the session's
// POST-ROTATE state (memql#3205).
//
// The stream context is deliberately NOT consulted for class / ceiling /
// expiry. A gRPC stream context is fixed at stream-open while badge grants
// arrive MID-STREAM via RotateAuth, which swaps s.access, s.identity and the
// credential stamp without touching the context because it cannot. Sourced from
// the context, a mid-stream badge session forwards no class and no ceiling, the
// receiver cannot distinguish that from "not a badge session", and it resolves
// the operator's UNCLAMPED stored role -- memql#2876's measured escalation.
//
// LOCK DISCIPLINE, load-bearing: ensureAccess acquires accessMu itself, so it
// MUST be called before we take the lock (sync.Mutex is not reentrant). The
// credential stamp is then read in ONE critical section together with
// s.access, so a concurrent handleRotateAuth -- which swaps all of them inside
// a single accessMu section -- can never be observed half-applied. Reading them
// separately could pair a pre-rotate role with a post-rotate ceiling, which the
// receiver's proof-of-clamp would then refuse: a self-inflicted outage.
func (s *streamSession) forwardedPrincipal() (auth.ForwardedPrincipal, error) {
	s.ensureAccess(s.stream.Context()) // acquires accessMu; never call it holding the lock

	s.accessMu.Lock()
	s.ensureAuthorityStampedLocked()
	access := s.access
	class := s.credentialClass
	ceiling := s.credentialCeiling
	expiresAt := s.badgeExpiresAt
	s.accessMu.Unlock()

	// The synthetic principals get NAMED classes rather than arriving
	// indistinguishable from an ordinary user session. local-dev and operator
	// both legitimately resolve to owner, so naming them is what keeps that
	// greppable and auditable on the receiving node; guest carries no `class`
	// claim at all, so without this it would be asserted as "user" -- an
	// actively wrong value in a field the contract declares is never empty.
	switch {
	case strings.EqualFold(s.identity.Subject, LocalDevSubject):
		class, ceiling, expiresAt = auth.ForwardedClassLocalDev, "", time.Time{}
	case strings.EqualFold(s.identity.Subject, OperatorSubject):
		class, ceiling, expiresAt = auth.ForwardedClassOperator, "", time.Time{}
	case strings.HasPrefix(s.identity.Subject, GuestSubjectPrefix):
		class, ceiling, expiresAt = auth.ForwardedClassGuest, "", time.Time{}
	}

	authority, err := auth.ForwardedAuthorityForUser(access, class, auth.Role(ceiling), expiresAt, time.Now())
	if err != nil {
		return auth.ForwardedPrincipal{}, err
	}
	return authority.Principal(), nil
}

// extractPartitionFromEnvelope is a no-op post-#56 phase 8. The
// partition dimension is gone from the wire; this stub stays for the
// handful of callers that still expect a string return until they're
// swept in a follow-up.
func extractPartitionFromEnvelope(envelope *memqlv1.MemqlClientMessage) string {
	_ = envelope
	return ""
}
