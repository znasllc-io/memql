package memql

import (
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
		s.logger.Warn("deploy control (streamed) rejected",
			"request_id", result.GetRequestId(),
			"error_code", result.GetErrorCode(),
			"error_message", result.GetErrorMessage())
	}

	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_DeployControlResult{
			DeployControlResult: result,
		},
	})
}
