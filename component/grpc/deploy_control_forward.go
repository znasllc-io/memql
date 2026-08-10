package memql

// deploy_control_forward.go carries a bridged DeployControlMsg across the mesh
// from the node a client is talking to, to the node that can serve it
// (memql#3380).
//
// WHY THE HOP EXISTS. DeployControlService shells out against an on-disk
// overlay checkout, so it is built `//go:build identity` and exists only on the
// identity node. The memQL portal is served ONLY by the bff
// (deploy/k8s/components/engine-bff/bff.yaml is the sole manifest that sets
// MEMQL_PORTAL_DIST) and a SPA dials the origin that served it, so the portal's
// stream always terminates on a bff. The result was a Deployments surface that
// rendered populated -- history is ordinary v1:cluster:deployment concept rows
// read over the normal query path -- and answered UNIMPLEMENTED the moment a
// button was pressed, because deploycontrol.Dispatch short-circuits when the
// node has no service.
//
// WHAT THIS FILE IS CAREFUL ABOUT. Deploy control's gate is a role matrix in
// which rollback is OWNER-ONLY -- not even admin (memql#1876; see
// docs/public/operate/deployment-console.md). A forward that lost the caller,
// or re-derived one on the receiving side, would widen rollback to anyone who
// can reach a bff: the bff dials identity with its own class="node" credential,
// so "resolve the actor from the connection" would resolve THE BFF, and a
// resolver keyed off a wire-supplied subject would take the caller's word for
// who they are. Neither happens here. The caller is asserted ONCE, by the
// producer, as a typed auth.ForwardedAuthority built from the session's
// post-rotate state (the memql#3205 contract), and the receiver's ONLY
// authorization input is that assertion:
//
//	bff                                     identity
//	----                                    --------
//	forwardedPrincipal()                    VerifyForwardedAuthority  <- fails closed
//	  session role, class, ceiling, exp       -> *auth.AccessContext
//	Forward(...)  ------ NodeService ---->  BindForwardedContext
//	                                        deploycontrol.Dispatch    <- the SAME
//	                                          resolveActor reads that      fan-out the
//	                                          AccessContext                local path runs
//
// Because the receiving side runs deploycontrol.Dispatch -- not a
// re-implementation of it -- the forwarded surface cannot drift from the local
// one: there is one gate, one audit write, and one argument-validation path,
// exactly as memql#3311 established for the stream bridge. The parity property
// this file adds is that a THIRD way in (over the mesh) still reaches that one
// gate with the ORIGINATING caller's role.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/deploycontrol"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/id"
)

// deployForwardCeiling bounds how long an originating handler waits for the
// identity node's reply.
//
// A ceiling is required rather than "wait on the caller's context": the
// caller's context is a WebSocket stream context that lives for the whole
// browser session, so a reply lost to a mid-flight peer restart would park the
// waiter -- and its goroutine and inflight entry -- for the life of that
// session. The value is generous because the slowest action behind this hop is
// a synchronous promote.sh run, not an RPC.
const deployForwardCeiling = 10 * time.Minute

// -----------------------------------------------------------------------------
// Originating side (the bff): outbound forward + reply routing
// -----------------------------------------------------------------------------

// DeployControlForwardRouter dispatches a bridged deploy-control envelope to
// the identity node and parks the calling handler until the reply lands.
//
// One instance lives on the bff (installed via Server.SetDeployControlForwarder).
// It implements node.DeployControlForwardResponseSink, so the WorkerDialer /
// ParentConnector inbound handlers route each DeployControlForwardResponse back
// to the waiting request.
//
// Single round-trip, unlike AiForwardRouter: every deploy RPC is unary, so
// Forward returns the one result rather than a channel of them.
type DeployControlForwardRouter struct {
	peerMgr *node.PeerManager
	logger  *slog.Logger

	mu       sync.Mutex
	inflight map[string]chan *nodev1.DeployControlForwardResponse
}

// NewDeployControlForwardRouter constructs an idle router. peerMgr must be the
// originating node's PeerManager; logger may be nil.
func NewDeployControlForwardRouter(peerMgr *node.PeerManager, logger *slog.Logger) *DeployControlForwardRouter {
	if logger == nil {
		logger = slog.Default()
	}
	return &DeployControlForwardRouter{
		peerMgr:  peerMgr,
		logger:   logger,
		inflight: make(map[string]chan *nodev1.DeployControlForwardResponse),
	}
}

// Available reports whether a connected identity peer is reachable right now.
//
// The bridged handler consults this to choose between forwarding and answering
// locally. It is deliberately a live check rather than a boot-time flag: the
// identity peer appears in the table only after the dialer's first successful
// handshake, and disappears on a rollout.
func (r *DeployControlForwardRouter) Available() bool {
	return r.selectPeer() != nil
}

// Forward marshals `msg`, sends it to the identity node with the caller's
// authority assertion attached, and blocks until the reply arrives, the
// caller's context is cancelled, or the ceiling expires.
//
// The returned *DeployControlResult is the receiving node's verbatim envelope,
// including its error_code / error_message -- so a PermissionDenied refused by
// the owner-only rollback gate on identity reaches the browser as exactly that,
// not as a transport failure. A non-nil error means the request never reached
// the service.
func (r *DeployControlForwardRouter) Forward(
	ctx context.Context,
	principal auth.ForwardedPrincipal,
	msg *memqlv1.DeployControlMsg,
) (*memqlv1.DeployControlResult, error) {
	if r == nil {
		return nil, fmt.Errorf("deploy console: no deploy-control forwarder configured")
	}
	if msg == nil {
		return nil, fmt.Errorf("deploy console: nothing to forward")
	}
	peer := r.selectPeer()
	if peer == nil {
		return nil, fmt.Errorf("deploy console: no identity node available to serve the deploy surface")
	}

	// A fresh forward id per hop. It is NOT the caller's request_id: the reply
	// correlates the mesh round-trip, while the caller's request_id is echoed
	// back inside the DeployControlResult the receiver produces.
	requestId := id.NewShortId()
	fwd, err := buildDeployControlForwardRequest(requestId, principal, msg, r.selfNodeId(), r.selfNodeType())
	if err != nil {
		return nil, err
	}

	respCh := make(chan *nodev1.DeployControlForwardResponse, 1)
	r.mu.Lock()
	r.inflight[requestId] = respCh
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.inflight, requestId)
		r.mu.Unlock()
	}()

	peer.Connection.Send(&nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeClientMessage_DeployControlForwardRequest{
			DeployControlForwardRequest: fwd,
		},
	})

	r.logger.Debug("deploy control forwarded",
		"request_id", requestId,
		"peer_id", peer.Info.GetNodeId())

	select {
	case resp := <-respCh:
		return decodeDeployControlForwardResponse(resp)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(deployForwardCeiling):
		return nil, fmt.Errorf("deploy console: identity node did not reply within %s", deployForwardCeiling)
	}
}

// Dispatch implements node.DeployControlForwardResponseSink: route an inbound
// reply to the parked request. An unknown request_id is logged and dropped --
// it means the waiter already gave up.
func (r *DeployControlForwardRouter) Dispatch(resp *nodev1.DeployControlForwardResponse) {
	if r == nil || resp == nil {
		return
	}
	r.mu.Lock()
	ch, ok := r.inflight[resp.GetRequestId()]
	r.mu.Unlock()
	if !ok {
		r.logger.Warn("deploy control forward reply has no active receiver",
			"request_id", resp.GetRequestId())
		return
	}
	select {
	case ch <- resp:
	default:
		r.logger.Warn("deploy control forward reply dropped (channel full)",
			"request_id", resp.GetRequestId())
	}
}

// selectPeer returns a connected identity peer, or nil. Health is not consulted
// beyond requiring a live outbound connection: there is exactly one identity
// service in a cluster, so there is no healthier alternative to fall back to,
// and refusing to try a DEGRADED one would only turn a slow deploy console into
// an absent one.
func (r *DeployControlForwardRouter) selectPeer() *node.PeerEntry {
	if r == nil || r.peerMgr == nil {
		return nil
	}
	for _, p := range r.peerMgr.ByType(node.NodeTypeIdentity) {
		if p == nil || p.Info == nil || p.Connection == nil {
			continue
		}
		return p
	}
	return nil
}

func (r *DeployControlForwardRouter) selfNodeId() string {
	if r == nil || r.peerMgr == nil {
		return ""
	}
	return r.peerMgr.SelfNodeId()
}

func (r *DeployControlForwardRouter) selfNodeType() string {
	if r == nil || r.peerMgr == nil {
		return ""
	}
	return r.peerMgr.SelfNodeType()
}

// buildDeployControlForwardRequest is the producer's ENCODER, factored out of
// Forward so a test can drive the real thing.
//
// The two halves of this hop are only meaningfully tested together -- an
// encoder test that asserts against a hand-built envelope proves the test
// agrees with itself. Extracting the encode and the decode lets a test run
// producer -> receiver -> consumer through the same code the mesh runs, with
// only peer.Connection.Send (which needs a live gRPC stream) simulated.
func buildDeployControlForwardRequest(
	requestId string,
	principal auth.ForwardedPrincipal,
	msg *memqlv1.DeployControlMsg,
	selfNodeId, selfNodeType string,
) (*nodev1.DeployControlForwardRequest, error) {
	payload, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("deploy console: marshal request: %w", err)
	}
	return &nodev1.DeployControlForwardRequest{
		RequestId:        requestId,
		DeployControlMsg: payload,
		Authority:        node.ForwardedAuthorityToProto(principal.Authority, selfNodeId, selfNodeType),
	}, nil
}

// decodeDeployControlForwardResponse is the consumer's DECODER, the mirror of
// buildDeployControlForwardRequest.
//
// A populated error_code is a TRANSPORT failure -- the envelope never reached
// the service -- and is returned as an error rather than folded into a result,
// so "could not reach the deploy node" never reads to an operator as "the
// deploy node refused you". A service refusal travels the other way: inside
// the DeployControlResult, carrying the same gRPC code the local path
// produces.
func decodeDeployControlForwardResponse(resp *nodev1.DeployControlForwardResponse) (*memqlv1.DeployControlResult, error) {
	if resp == nil {
		return nil, fmt.Errorf("deploy console: empty forwarded reply")
	}
	if code := strings.TrimSpace(resp.GetErrorCode()); code != "" {
		return nil, fmt.Errorf("deploy console: forward refused (%s): %s", code, resp.GetErrorMessage())
	}
	var out memqlv1.DeployControlResult
	if err := proto.Unmarshal(resp.GetDeployControlResult(), &out); err != nil {
		return nil, fmt.Errorf("deploy console: decode forwarded reply: %w", err)
	}
	return &out, nil
}

// -----------------------------------------------------------------------------
// Receiving side (the identity node): verify the caller, then dispatch locally
// -----------------------------------------------------------------------------

// DeployControlForwardHandler serves inbound deploy-control forwards on the
// identity node. Installed on the NodeServer by app bootstrap with the SAME
// *deploycontrol.Service instance registered for the unary service and bridged
// onto MemqlService.Stream, so all three surfaces share one gate, one audit
// logger and one executor.
type DeployControlForwardHandler struct {
	srv    memqlv1.DeployControlServiceServer
	logger *slog.Logger
}

// NewDeployControlForwardHandler wraps a deploy-control service for the mesh.
// Returns nil when srv is nil, so the caller can install unconditionally and
// a node without the service keeps answering "not_configured".
func NewDeployControlForwardHandler(srv memqlv1.DeployControlServiceServer, logger *slog.Logger) *DeployControlForwardHandler {
	if srv == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &DeployControlForwardHandler{srv: srv, logger: logger}
}

// HandleForwardedRequest implements node.DeployControlForwardHandler.
//
// Order is load-bearing. THE GATE runs first: the authority is decidable
// without the payload, and nothing -- not even a malformed-envelope error --
// should be answered on behalf of a caller whose authority cannot be
// established. Only then is the envelope decoded and handed to
// deploycontrol.Dispatch under a context carrying the VERIFIED caller.
//
// Nothing about the calling NODE reaches the gate. The context this runs on is
// the NodeService stream's, which carries the bff's class="node" credential;
// binding the verified assertion over it is what makes deploycontrol's
// resolveActor read the human who pressed the button rather than the machine
// that relayed the press. That is the whole security content of the hop, and
// TestDeployControlForwardIgnoresTheForwardingNodesOwnCredential pins it.
func (h *DeployControlForwardHandler) HandleForwardedRequest(
	ctx context.Context,
	req *nodev1.DeployControlForwardRequest,
	send func(*nodev1.NodeServerMessage) error,
) {
	if h == nil || req == nil || send == nil {
		return
	}
	requestId := req.GetRequestId()

	authority := node.ForwardedAuthorityFromProto(req.GetAuthority())
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	if err != nil {
		metrics.AuthReject(metrics.SurfaceNodeForward, forwardRefusalReason(err), codes.PermissionDenied.String())
		h.logger.Warn("deploy control forward refused: unprovable authority",
			"request_id", requestId,
			"reason", err,
			"origin_node_id", authority.OriginNodeId,
			"origin_node_type", authority.OriginNodeType)
		h.sendTransportError(send, requestId, "forwarded_authority_refused", err.Error())
		return
	}

	var msg memqlv1.DeployControlMsg
	if err := proto.Unmarshal(req.GetDeployControlMsg(), &msg); err != nil {
		h.sendTransportError(send, requestId, "malformed_envelope",
			"deploy console: forwarded deploy_control envelope would not parse")
		return
	}

	// The claims are DERIVED from the assertion, never carried beside it --
	// there is no claims map on this message for one to arrive in, which is
	// what makes "claims without an authority" inexpressible on this hop.
	ctx = auth.BindForwardedContext(ctx, authority.Principal().Claims, access, authority)

	// The SAME fan-out the local bridge runs. Not a copy of it: an RPC that is
	// gated on one surface and ungated on another is precisely the drift the
	// #3311 parity test exists to prevent, and a second Dispatch would be the
	// way to reintroduce it.
	result := deploycontrol.Dispatch(ctx, h.srv, &msg)

	if !result.GetOk() {
		h.logger.Warn("deploy control (forwarded) rejected",
			"request_id", requestId,
			"actor", access.UserId,
			"actor_role", string(access.Role),
			"error_code", result.GetErrorCode(),
			"error_message", result.GetErrorMessage(),
			// Set on a GATE refusal only (memql#3334). This is the node that
			// WROTE the blocked audit event, so logging the id here is where
			// the trail and the log line meet.
			"audit_event_id", result.GetAuditEventId())
	}

	encoded, err := proto.Marshal(result)
	if err != nil {
		h.sendTransportError(send, requestId, "encode_reply", err.Error())
		return
	}
	if err := send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: requestId,
		Payload: &nodev1.NodeServerMessage_DeployControlForwardResponse{
			DeployControlForwardResponse: &nodev1.DeployControlForwardResponse{
				RequestId:           requestId,
				DeployControlResult: encoded,
			},
		},
	}); err != nil {
		h.logger.Warn("deploy control forward reply send failed",
			"request_id", requestId, "error", err)
	}
}

// sendTransportError answers a forward that never reached the service. It is
// always answered rather than dropped: the originating handler is parked on
// this reply, so a dropped message turns a refusal into a ten-minute hang.
func (h *DeployControlForwardHandler) sendTransportError(
	send func(*nodev1.NodeServerMessage) error,
	requestId, code, message string,
) {
	if err := send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: requestId,
		Payload: &nodev1.NodeServerMessage_DeployControlForwardResponse{
			DeployControlForwardResponse: &nodev1.DeployControlForwardResponse{
				RequestId:    requestId,
				ErrorCode:    code,
				ErrorMessage: message,
			},
		},
	}); err != nil {
		h.logger.Warn("deploy control forward error reply send failed",
			"request_id", requestId, "error_code", code, "error", err)
	}
}

// Compile-time guards: the two halves must keep satisfying the node-package
// ports they are installed through.
var (
	_ node.DeployControlForwardResponseSink = (*DeployControlForwardRouter)(nil)
	_ node.DeployControlForwardHandler      = (*DeployControlForwardHandler)(nil)
	_                                       = status.Code
)
