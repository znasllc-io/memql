package memql

// Deploy console bridge (memql#3311).
//
// DeployControlService is a separate UNARY gRPC service mounted on the same
// listener as MemqlService (component/grpc/deploy_control.proto). The Go SDK
// dials it natively; a browser cannot, and neither can anything else speaking
// the `/memql/ws` bridge, which tunnels MemqlService.Stream and nothing else.
// That left the whole deploy surface unreachable from the VS Code extension
// and the memQL portal.
//
// This file bridges the nine RPCs onto the stream. The bridge is deliberately
// thin: it unwraps DeployControlMsg's oneof, calls the MATCHING METHOD on the
// very same DeployControlServiceServer implementation the unary path is
// registered with, and re-wraps the reply. It contains no role check, no
// audit write, and no argument validation of its own -- all three live inside
// component/deploycontrol's methods, so there is exactly ONE gate and the two
// transports cannot diverge. The parity test
// (deploy_control_parity_test.go) is what holds that property down: it drives
// the real *deploycontrol.Service through both paths and asserts identical
// PermissionDenied verdicts for every gated RPC.
//
// Deployment HISTORY is intentionally not bridged: v1:cluster:deployment rows
// are ordinary concept rows already readable over ExecuteQueryMsg.

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// DeployControlHandler is the narrow port the streamed deploy bridge drives.
// It is deliberately the GENERATED server interface rather than a hand-written
// port: component/deploycontrol's *Service already satisfies it (it is what
// RegisterDeployControlServiceServer takes), so the bridge calls the identical
// methods the unary service exposes, gate and audit included. Declaring a
// bespoke interface here would invite a second, subtly different signature set
// -- the exact drift the bridge exists to avoid.
//
// component/grpc does NOT import component/deploycontrol: that package is
// linked into the identity binary only (app/integrations_deploy_control.go is
// //go:build identity), and every other node type would pay for it. The
// concrete service is injected from the wiring layer instead, the same shape
// SetNodeMaintenanceHandler / SetAgentReplier use.
type DeployControlHandler = memqlv1.DeployControlServiceServer

// SetDeployControlService installs the deploy-control implementation the
// streamed bridge forwards to. Wired on the identity node (the only binary
// that constructs a *deploycontrol.Service); left nil everywhere else, where
// the handler answers Unimplemented so a portal pointed at the wrong node gets
// a clear verdict rather than a silent no-op.
//
// Mirrors SetNodeMaintenanceHandler: updates both the Server field (picked up
// by the next service construction in Run) and the live service reference (so
// an install after Run still takes effect).
func (s *Server) SetDeployControlService(h DeployControlHandler) {
	if s == nil {
		return
	}
	s.deployControl = h
	if s.serviceRef != nil && s.serviceRef.svc != nil {
		s.serviceRef.svc.deployControl = h
	}
}

// handleDeployControl is the landing for DeployControlMsg. It resolves the
// caller's AccessContext onto the outgoing context -- which is the ONE piece
// of plumbing the bridge owes the gate, because deploycontrol's resolveActor
// reads auth.AccessFromContext and the stream interceptor only stamps raw
// claims -- then dispatches to the wired service.
//
// Errors (including the PermissionDenied a gated RPC raises for a caller below
// its role floor) travel on the ordinary QueryErrorMsg channel carrying the
// gRPC status code verbatim, so a streamed caller reads the same code a unary
// caller reads off the returned status error.
func (s *streamSession) handleDeployControl(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.DeployControlMsg) error {
	correlate := envelope.GetMessageId()
	requestId := msg.GetRequestId()

	if msg.GetRequest() == nil {
		return s.sendQueryError(requestId, correlate, codes.InvalidArgument,
			"deploy console: DeployControlMsg carries no request")
	}

	handler := s.service.deployControl
	if handler == nil {
		// Not the identity node (or deploy control was never wired). Say so
		// explicitly: a portal dialed at a bff would otherwise see a dropped
		// message and no explanation.
		return s.sendQueryError(requestId, correlate, codes.Unimplemented,
			"deploy console: this node does not host DeployControlService")
	}

	// The gate resolves the actor from the context, so hand it the same
	// AccessContext every other authenticated handler resolves. Without this
	// deploycontrol's resolveActor fails closed and EVERY call -- including an
	// owner's -- is rejected as unauthenticated.
	ctx := auth.ContextWithAccess(s.stream.Context(), s.ensureAccess(s.stream.Context()))

	result, err := dispatchDeployControl(ctx, handler, msg)
	if err != nil {
		st, _ := status.FromError(err)
		if s.logger != nil {
			s.logger.Warn("deploy control (streamed) rejected",
				"rpc", result.GetRpc(), "code", st.Code().String(), "request_id", requestId)
		}
		return s.sendQueryError(requestId, correlate, st.Code(), st.Message())
	}

	result.RequestId = requestId
	return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_DeployControlResult{DeployControlResult: result},
	})
}

// dispatchDeployControl maps one DeployControlMsg request arm onto the
// matching DeployControlServiceServer method and wraps its reply in the
// response oneof. The returned DeployControlResult always carries the RPC's
// method name -- even on the error return -- so both the reply envelope and
// the rejection log can name it.
//
// Split out of the handler so the parity test drives the SAME dispatch the
// stream drives without standing up a stream, and so a newly added RPC left
// out of this switch is caught by the test's completeness assertion against
// the generated DeployControlServiceServer interface.
func dispatchDeployControl(
	ctx context.Context,
	h DeployControlHandler,
	msg *memqlv1.DeployControlMsg,
) (*memqlv1.DeployControlResult, error) {
	switch req := msg.GetRequest().(type) {
	// Reads.
	case *memqlv1.DeployControlMsg_GetDeploymentStatus:
		res := &memqlv1.DeployControlResult{Rpc: "GetDeploymentStatus"}
		out, err := h.GetDeploymentStatus(ctx, req.GetDeploymentStatus)
		if err != nil {
			return res, err
		}
		res.Response = &memqlv1.DeployControlResult_DeploymentStatus{DeploymentStatus: out}
		return res, nil
	case *memqlv1.DeployControlMsg_SuggestNextVersion:
		res := &memqlv1.DeployControlResult{Rpc: "SuggestNextVersion"}
		out, err := h.SuggestNextVersion(ctx, req.SuggestNextVersion)
		if err != nil {
			return res, err
		}
		res.Response = &memqlv1.DeployControlResult_NextVersion{NextVersion: out}
		return res, nil

	// Actions. All seven share ActionResult, so they share one wrapper.
	case *memqlv1.DeployControlMsg_DeployStaging:
		out, err := h.DeployStaging(ctx, req.DeployStaging)
		return actionResult("DeployStaging", out), err
	case *memqlv1.DeployControlMsg_Promote:
		out, err := h.Promote(ctx, req.Promote)
		return actionResult("Promote", out), err
	case *memqlv1.DeployControlMsg_Rollback:
		out, err := h.Rollback(ctx, req.Rollback)
		return actionResult("Rollback", out), err
	case *memqlv1.DeployControlMsg_RolloutAction:
		out, err := h.RolloutAction(ctx, req.RolloutAction)
		return actionResult("RolloutAction", out), err
	case *memqlv1.DeployControlMsg_CutVersion:
		out, err := h.CutVersion(ctx, req.CutVersion)
		return actionResult("CutVersion", out), err
	case *memqlv1.DeployControlMsg_Deploy:
		out, err := h.Deploy(ctx, req.Deploy)
		return actionResult("Deploy", out), err
	case *memqlv1.DeployControlMsg_RollbackDeployment:
		out, err := h.RollbackDeployment(ctx, req.RollbackDeployment)
		return actionResult("RollbackDeployment", out), err
	}
	return &memqlv1.DeployControlResult{}, status.Error(codes.InvalidArgument,
		"deploy console: unrecognized DeployControlMsg request")
}

// actionResult builds the reply for one of the seven ActionResult-returning
// RPCs. A nil ActionResult (which only happens alongside an error) leaves the
// response oneof unset so the caller's error branch wins.
func actionResult(rpc string, out *memqlv1.ActionResult) *memqlv1.DeployControlResult {
	res := &memqlv1.DeployControlResult{Rpc: rpc}
	if out != nil {
		res.Response = &memqlv1.DeployControlResult_Action{Action: out}
	}
	return res
}
