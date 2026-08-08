// dispatch.go bridges the DeployControlService RPC set onto a single
// envelope so a WebSocket client can reach it (memql#3311).
//
// DeployControlService is a separate UNARY gRPC service on the same
// listener. The Go SDK dials it natively; a browser cannot dial gRPC at
// all, and neither can any other WebSocket client -- so the entire deploy
// surface was unreachable from the VS Code extension (which speaks
// /memql/ws) and from the portal. Bridging it onto MemqlService.Stream
// fixes that, and this file is the fan-out that makes the bridge safe.
//
// THE INVARIANT THIS FILE EXISTS TO HOLD. Dispatch takes the
// memqlv1.DeployControlServiceServer interface and calls its methods
// directly. It contains no role check, no audit write, and no argument
// validation of its own -- every one of those lives inside the Service
// methods, which is what the unary path invokes too. The gate therefore
// cannot drift between the two surfaces: there is one implementation, and
// a bridged call is the unary call with a different envelope around it. A
// bridge that re-implemented the checks would be a privilege-escalation
// hole the first time the two copies disagreed, which is precisely the
// failure the #3311 parity test (deploycontrol/stream_parity_test.go)
// exists to make impossible.
//
// The caller's identity travels the ordinary way: the stream handler
// stamps the session's auth.AccessContext onto the context it passes here,
// and resolveActor (service.go) reads it exactly as it reads the
// interceptor-stamped context on a unary call.
package deploycontrol

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// Dispatch routes one bridged DeployControlMsg to the matching
// DeployControlServiceServer method and folds the (result, error) pair
// into a DeployControlResult.
//
// A unary caller receives failure as a gRPC status error. A multiplexed
// stream has no per-message status channel -- returning an error there
// would tear down the whole stream and every other in-flight request with
// it -- so the status is carried IN the reply: error_code holds the
// canonical gRPC code and error_message its message. A caller mapping
// error_code back to a code observes exactly what the unary caller
// observes.
//
// srv is an interface rather than *Service so the bridge is provably
// incapable of reaching past the RPC surface into the service's internals
// (and so a test can substitute a recorder without touching the gate).
func Dispatch(ctx context.Context, srv memqlv1.DeployControlServiceServer, msg *memqlv1.DeployControlMsg) *memqlv1.DeployControlResult {
	out := &memqlv1.DeployControlResult{RequestId: msg.GetRequestId()}
	if srv == nil {
		return failResult(out, status.Error(codes.Unimplemented,
			"deploy console: this node has no deploy-control service (it runs on the identity node)"))
	}
	if msg == nil || msg.GetRequest() == nil {
		return failResult(out, status.Error(codes.InvalidArgument,
			"deploy console: deploy_control message carries no request"))
	}

	// One case per RPC. Reads land in deployment_status / next_version;
	// every action lands in action (the uniform ActionResult, which carries
	// the audit event id the caller needs).
	switch req := msg.GetRequest().(type) {
	case *memqlv1.DeployControlMsg_GetDeploymentStatus:
		res, err := srv.GetDeploymentStatus(ctx, req.GetDeploymentStatus)
		if err != nil {
			return failResult(out, err)
		}
		out.Result = &memqlv1.DeployControlResult_DeploymentStatus{DeploymentStatus: res}
	case *memqlv1.DeployControlMsg_SuggestNextVersion:
		res, err := srv.SuggestNextVersion(ctx, req.SuggestNextVersion)
		if err != nil {
			return failResult(out, err)
		}
		out.Result = &memqlv1.DeployControlResult_NextVersion{NextVersion: res}
	case *memqlv1.DeployControlMsg_DeployStaging:
		res, err := srv.DeployStaging(ctx, req.DeployStaging)
		if err != nil {
			return failResult(out, err)
		}
		out.Result = &memqlv1.DeployControlResult_Action{Action: res}
	case *memqlv1.DeployControlMsg_Promote:
		res, err := srv.Promote(ctx, req.Promote)
		if err != nil {
			return failResult(out, err)
		}
		out.Result = &memqlv1.DeployControlResult_Action{Action: res}
	case *memqlv1.DeployControlMsg_Rollback:
		res, err := srv.Rollback(ctx, req.Rollback)
		if err != nil {
			return failResult(out, err)
		}
		out.Result = &memqlv1.DeployControlResult_Action{Action: res}
	case *memqlv1.DeployControlMsg_RolloutAction:
		res, err := srv.RolloutAction(ctx, req.RolloutAction)
		if err != nil {
			return failResult(out, err)
		}
		out.Result = &memqlv1.DeployControlResult_Action{Action: res}
	case *memqlv1.DeployControlMsg_CutVersion:
		res, err := srv.CutVersion(ctx, req.CutVersion)
		if err != nil {
			return failResult(out, err)
		}
		out.Result = &memqlv1.DeployControlResult_Action{Action: res}
	case *memqlv1.DeployControlMsg_Deploy:
		res, err := srv.Deploy(ctx, req.Deploy)
		if err != nil {
			return failResult(out, err)
		}
		out.Result = &memqlv1.DeployControlResult_Action{Action: res}
	case *memqlv1.DeployControlMsg_RollbackDeployment:
		res, err := srv.RollbackDeployment(ctx, req.RollbackDeployment)
		if err != nil {
			return failResult(out, err)
		}
		out.Result = &memqlv1.DeployControlResult_Action{Action: res}
	default:
		// A request variant added to the proto but not wired here. Fail
		// loudly rather than returning an empty ok=true reply that a
		// client would read as "the action ran".
		return failResult(out, status.Errorf(codes.Unimplemented,
			"deploy console: bridged request %T is not routed", req))
	}

	out.Ok = true
	return out
}

// failResult stamps the canonical gRPC code + message of err onto the
// envelope. A non-status error is reported as Internal, matching how a
// unary gRPC server surfaces a bare error to its caller.
func failResult(out *memqlv1.DeployControlResult, err error) *memqlv1.DeployControlResult {
	st, _ := status.FromError(err)
	out.Ok = false
	out.ErrorCode = int32(st.Code())
	out.ErrorMessage = st.Message()
	return out
}
