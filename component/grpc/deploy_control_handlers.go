package memql

import (
	"google.golang.org/grpc/codes"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/deploycontrol"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// deploy_control_handlers.go is the stream landing for the bridged
// DeployControlService surface (memql#3311).
//
// DeployControlService is a separate UNARY gRPC service mounted on the same
// listener. The Go SDK dials it natively, but a browser cannot dial gRPC and
// neither can any other WebSocket client -- so the entire deploy surface was
// unreachable from the VS Code extension (which speaks /memql/ws) and from
// the portal, which unlike the server-rendered identity /admin/* app has no
// fallback at all. Bridging it onto MemqlService.Stream is what makes the
// DevOps surface possible on both consumers.
//
// This handler is deliberately thin, and the thinness is the security
// property. It does NOT check roles, write audit events, or validate
// arguments -- every one of those lives inside the DeployControlService
// methods, and this handler reaches them through the SAME server interface
// the unary path is registered against. A bridged call is therefore the
// unary call with a different envelope around it; the owner/admin/developer
// matrix (epic #1871) and the v1:identity:auditEvent write cannot drift
// between the two surfaces, because there is only one of each. See
// component/deploycontrol/dispatch.go and the parity test in
// component/deploycontrol/stream_parity_test.go.

// SetDeployControlHandler installs the DeployControlService implementation
// the bridged stream surface dispatches to (memql#3311). app bootstrap wires
// it on the identity node with the SAME *deploycontrol.Service instance it
// registers for the unary service -- one object, so both surfaces share the
// gate, the audit logger, the executor, and the engine handle.
//
// Nil on every other node type; the handler then answers Unimplemented
// rather than pretending the surface exists. Mirrors
// SetNodeMaintenanceHandler: it updates both the Server field (picked up by
// the next service construction in Run) and the live service reference (so a
// post-Run install still takes effect).
func (s *Server) SetDeployControlHandler(h memqlv1.DeployControlServiceServer) {
	if s == nil {
		return
	}
	s.deployControlHandler = h
	if s.serviceRef != nil && s.serviceRef.svc != nil {
		s.serviceRef.svc.deployControlHandler = h
	}
}

// SetDeployControlForwarder installs the mesh route to the node that DOES have
// a deploy-control service (memql#3380). Wired on the bff, because the bff is
// the only node type that serves the portal bundle and a SPA dials the origin
// that served it -- so the deploy surface is always asked of a node that
// cannot answer it locally.
//
// Nil elsewhere, which is the whole no-op condition: a node with neither a
// local service nor a forwarder answers Unimplemented exactly as before.
// Mirrors SetDeployControlHandler in updating both the Server field and the
// live service reference so a post-Run install still takes effect.
func (s *Server) SetDeployControlForwarder(r *DeployControlForwardRouter) {
	if s == nil {
		return
	}
	s.deployControlForwarder = r
	if s.serviceRef != nil && s.serviceRef.svc != nil {
		s.serviceRef.svc.deployControlForwarder = r
	}
}

// handleDeployControl bridges one DeployControlMsg to the deploy-control
// service and returns the reply on the stream.
//
// The caller's identity is threaded the ordinary way: the session's resolved
// AccessContext is stamped onto the context handed to Dispatch, and
// deploycontrol.resolveActor reads it exactly as it reads the
// interceptor-stamped context on a unary call. That single line is the whole
// of the auth plumbing here -- everything downstream of it is shared code.
//
// Failures never return a Go error: an error out of a handler tears down the
// multiplexed stream, taking every other in-flight request with it. The gRPC
// status is carried inside DeployControlResult (error_code / error_message)
// so a bridged caller observes the same code a unary caller would.
func (s *streamSession) handleDeployControl(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.DeployControlMsg) error {
	ctx := s.stream.Context()

	// No local service, but a route to the node that has one (memql#3380).
	// This is the bff case, and it is the ONLY case an operator meets: the
	// portal is served by the bff, and a SPA dials the origin that served it.
	// The caller is asserted here, on the node that resolved them, and
	// re-verified on the receiving side -- see deploy_control_forward.go for
	// why that is the security content of the hop rather than a formality.
	if s.service.deployControlHandler == nil && s.service.deployControlForwarder != nil {
		return s.forwardDeployControl(envelope, msg)
	}

	// Stamp the session's resolved access context so the service's gate sees
	// the streamed caller exactly as the unary interceptor presents a dialed
	// one. ensureAccess resolves (and caches) it against the identity
	// resolver, and is the same call every other role-gated stream handler
	// makes. A nil access context is left alone -- the gate fails closed on a
	// caller it cannot resolve (Unauthenticated), which is the behaviour we
	// want and NOT something to paper over here.
	if ac := s.ensureAccess(ctx); ac != nil {
		ctx = auth.ContextWithAccess(ctx, ac)
	}

	result := deploycontrol.Dispatch(ctx, s.service.deployControlHandler, msg)

	if s.logger != nil && !result.GetOk() {
		// audit_event_id is empty unless this was a GATE refusal (memql#3334);
		// logging it here is what lets an operator's quoted id be found in the
		// node log as well as in v1:identity:auditEvent.
		s.logger.Warn("deploy control (streamed) rejected",
			"request_id", result.GetRequestId(),
			"error_code", result.GetErrorCode(),
			"error_message", result.GetErrorMessage(),
			"audit_event_id", result.GetAuditEventId())
	}

	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_DeployControlResult{
			DeployControlResult: result,
		},
	})
}

// forwardDeployControl carries one bridged DeployControlMsg to the identity
// node and returns its reply on this stream (memql#3380).
//
// ASYNC, deliberately. handleMessage runs on the stream's Recv loop, so a
// synchronous wait here would stall every other in-flight request on the
// browser's single multiplexed stream for as long as the remote action takes
// -- and the slowest action behind this hop is a synchronous promote.sh run,
// not an RPC. The reply is sent from the goroutine through sendServerMessage,
// which serializes on the session's send mutex.
//
// Failures never return a Go error: an error out of a handler tears down the
// whole stream. A transport failure is folded into the SAME DeployControlResult
// shape the local path produces, so a caller mapping error_code back to a
// gRPC code observes one contract regardless of which node served it.
func (s *streamSession) forwardDeployControl(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.DeployControlMsg) error {
	// Build the authority assertion BEFORE leaving the handler: it is derived
	// from the session's post-rotate state, which is this node's knowledge and
	// nobody else's. Refusing locally on a session whose authority cannot be
	// established means an unprovable assertion never reaches the wire.
	principal, err := s.forwardedPrincipal()
	if err != nil {
		return s.sendDeployControlResult(envelope, failedDeployControlResult(msg, codes.Internal,
			"deploy console: cannot establish forwarded authority for this session: "+err.Error()))
	}

	ctx := s.stream.Context()
	go func() {
		result, ferr := s.service.deployControlForwarder.Forward(ctx, principal, msg)
		if ferr != nil {
			// Unavailable, not Internal: the deploy surface exists, this node
			// just could not reach it right now, and the portal renders a
			// retryable failure rather than a bug report.
			result = failedDeployControlResult(msg, codes.Unavailable, ferr.Error())
		}
		if s.logger != nil && !result.GetOk() {
			s.logger.Warn("deploy control (forwarded) rejected",
				"request_id", result.GetRequestId(),
				"error_code", result.GetErrorCode(),
				"error_message", result.GetErrorMessage(),
				"audit_event_id", result.GetAuditEventId())
		}
		if serr := s.sendDeployControlResult(envelope, result); serr != nil && s.logger != nil {
			s.logger.Warn("deploy control forwarded reply send failed",
				"request_id", result.GetRequestId(), "error", serr)
		}
	}()
	return nil
}

// sendDeployControlResult writes one DeployControlResult back on the stream,
// correlated to the caller's envelope.
func (s *streamSession) sendDeployControlResult(envelope *memqlv1.MemqlClientMessage, result *memqlv1.DeployControlResult) error {
	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_DeployControlResult{
			DeployControlResult: result,
		},
	})
}

// failedDeployControlResult mirrors deploycontrol's own failure envelope for
// the failures that happen on THIS side of the hop, so a transport problem and
// a service refusal reach the client in one shape.
func failedDeployControlResult(msg *memqlv1.DeployControlMsg, code codes.Code, message string) *memqlv1.DeployControlResult {
	return &memqlv1.DeployControlResult{
		RequestId:    msg.GetRequestId(),
		Ok:           false,
		ErrorCode:    int32(code),
		ErrorMessage: message,
	}
}
