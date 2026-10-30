package worker

// The cross-node half of Fleet's Sign in action. The person's call lands on a bff, and
// the machine's stream ends on one agent replica, so each thing they do crosses once.
//
// A sign-in is about ONE machine a person named, so there is nothing to fall through
// to, and each hop only answers whether the machine was told. The row is how anyone
// watches what happens next.
//
// A PASTED CODE CROSSES HERE AND IS WRITTEN NOWHERE. No log line in this file takes
// the action's Code, and neither do the refusals.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/id"
)

// appSignInForwardTimeout bounds one hop. The holder reads the fleet store and sends a
// message, so this is a ceiling on something quick.
const appSignInForwardTimeout = 15 * time.Second

// AppSignInRelay implements memqlengine.AppSignInRelay.
type AppSignInRelay struct {
	forward *ForwardRouter
	// registry is this replica's streams, nil on a node that holds none (a bff). A
	// machine this replica holds is told directly.
	registry *workerservice.Registry
	logger   *slog.Logger
}

// NewAppSignInRelay builds the relay for a node. Either half may be nil.
func NewAppSignInRelay(forward *ForwardRouter, registry *workerservice.Registry, logger *slog.Logger) *AppSignInRelay {
	if logger == nil {
		logger = slog.Default()
	}
	return &AppSignInRelay{forward: forward, registry: registry, logger: logger}
}

// RelayAppSignIn implements memqlengine.AppSignInRelay.
func (r *AppSignInRelay) RelayAppSignIn(ctx context.Context, holderNodeId string, action memqlengine.AppSignInAction) error {
	if r == nil {
		return errors.New("this node cannot reach a machine's stream")
	}
	if r.registry != nil {
		if w := r.registry.WorkerById(action.RegistrationId); w != nil {
			return tellWorkerAppSignIn(ctx, w, action.OwnerUserId, action)
		}
	}
	if r.forward == nil {
		return errors.New("this node has no route to the replica holding that machine")
	}
	return r.forward.ForwardAppSignIn(ctx, holderNodeId, action)
}

// tellWorkerAppSignIn tells a machine this replica holds. The stream is the owner's
// or this is refused: nothing but the machine's owner may start a sign-in.
func tellWorkerAppSignIn(ctx context.Context, w *workerservice.Worker, owner string, action memqlengine.AppSignInAction) error {
	if !sameSubject(w.OwnerUserId, owner) {
		return errRegistrationNotOwned
	}
	return w.SendAppSignIn(ctx, workerservice.AppSignInCommand{
		Action:   action.Action,
		SignInId: action.SignInId,
		App:      action.App,
		Code:     action.Code,
		Reason:   "the person who started it stopped it",
	})
}

// ForwardAppSignIn tells the replica holding a machine one thing a person did.
//
// THE MANDATORY ASSERTION (memql#3205), re-asserted from the context rather than
// rebuilt: only the machine's owner may start a sign-in, and the holder checks that
// against this and not the envelope.
func (r *ForwardRouter) ForwardAppSignIn(ctx context.Context, nodeId string, action memqlengine.AppSignInAction) error {
	if r == nil || r.sender == nil {
		return ErrNoPeerForNode
	}
	if strings.TrimSpace(nodeId) == "" {
		return errors.New("that machine is not connected to any replica right now")
	}
	authority, ok := auth.ForwardedAuthorityFromContext(ctx)
	if !ok {
		r.logger.Warn("app sign-in forward: no forwarded authority bound to the call context; refusing to forward",
			"registration_id", action.RegistrationId, "target_node_id", nodeId)
		return errors.New("forwarding a sign-in requires the assertion this node accepted; none is bound to the call")
	}
	requestId := id.NewShortId()
	answer := make(chan *nodev1.AppSignInForwardResponse, 1)
	r.appSignInMu.Lock()
	if r.appSignInInflight == nil {
		r.appSignInInflight = make(map[string]chan *nodev1.AppSignInForwardResponse)
	}
	r.appSignInInflight[requestId] = answer
	r.appSignInMu.Unlock()
	defer func() {
		r.appSignInMu.Lock()
		delete(r.appSignInInflight, requestId)
		r.appSignInMu.Unlock()
	}()

	if !r.sender.Send(nodeId, &nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeClientMessage_AppSignInForwardRequest{AppSignInForwardRequest: &nodev1.AppSignInForwardRequest{
			RequestId:      requestId,
			RegistrationId: action.RegistrationId,
			OwnerUserId:    action.OwnerUserId,
			SignInId:       action.SignInId,
			Action:         action.Action,
			App:            action.App,
			Code:           action.Code,
			Authority:      node.ForwardedAuthorityToProto(authority, r.SelfNodeId(), r.SelfNodeType()),
		}},
	}) {
		return fmt.Errorf("the replica holding that machine (%s) is not reachable from this node", nodeId)
	}

	ctx, cancel := context.WithTimeout(ctx, appSignInForwardTimeout)
	defer cancel()
	select {
	case resp := <-answer:
		if resp.GetErrorCode() != "" {
			return errors.New(resp.GetErrorMessage())
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the replica holding that machine (%s) did not answer", nodeId)
	}
}

// DispatchAppSignIn implements node.WorkerForwardResponseSink.
func (r *ForwardRouter) DispatchAppSignIn(resp *nodev1.AppSignInForwardResponse) {
	if r == nil || resp == nil {
		return
	}
	r.appSignInMu.Lock()
	answer, ok := r.appSignInInflight[resp.GetRequestId()]
	r.appSignInMu.Unlock()
	if !ok {
		return
	}
	select {
	case answer <- resp:
	default:
	}
}

// HandleForwardedAppSignIn serves an inbound AppSignInForwardRequest, on the replica
// holding the machine. As HandleForwardedModelPull does, it trusts the VERIFIED
// principal and never the envelope's owner hint: this is the last thing between a
// peer's envelope and a login on somebody's machine.
func (h *ForwardHandler) HandleForwardedAppSignIn(ctx context.Context, req *nodev1.AppSignInForwardRequest, send func(*nodev1.NodeServerMessage) error) {
	requestId := req.GetRequestId()
	reply := func(code, message string) {
		_ = send(&nodev1.NodeServerMessage{
			MessageId:   id.NewShortId(),
			CorrelateTo: requestId,
			Payload: &nodev1.NodeServerMessage_AppSignInForwardResponse{AppSignInForwardResponse: &nodev1.AppSignInForwardResponse{
				RequestId: requestId, ErrorCode: code, ErrorMessage: message,
			}},
		})
	}
	authority := node.ForwardedAuthorityFromProto(req.GetAuthority())
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	if err != nil {
		h.logger.Warn("app sign-in forward: refused an envelope whose authority did not verify",
			"request_id", requestId, "registration_id", req.GetRegistrationId(), "error", err)
		reply("forwarded_authority_refused", err.Error())
		return
	}
	ctx = auth.BindForwardedContext(ctx, authority.Principal().Claims, access, authority)
	owner := strings.TrimSpace(access.UserId)
	if owner == "" {
		reply("forwarded_authority_refused", "the verified authority names no subject, so there is no owner to check the machine against")
		return
	}
	if hint := strings.TrimSpace(req.GetOwnerUserId()); hint != "" && !sameSubject(hint, owner) {
		h.logger.Warn("app sign-in forward: envelope owner does not match the verified authority",
			"request_id", requestId, "envelope_owner", hint, "authority_subject", owner)
		reply("owner_mismatch", "the envelope's owner does not match the verified authority's subject")
		return
	}
	if err := h.verifyRegistration(ctx, owner, req.GetRegistrationId()); err != nil {
		reply("registration_refused", err.Error())
		return
	}
	w := h.registry.WorkerById(req.GetRegistrationId())
	if w == nil {
		reply("worker_disconnected", "That machine reconnected. Sign in again.")
		return
	}
	err = tellWorkerAppSignIn(ctx, w, owner, memqlengine.AppSignInAction{
		Action:         req.GetAction(),
		RegistrationId: req.GetRegistrationId(),
		OwnerUserId:    owner,
		SignInId:       req.GetSignInId(),
		App:            req.GetApp(),
		Code:           req.GetCode(),
	})
	if err != nil {
		reply("app_sign_in_refused", err.Error())
		return
	}
	reply("", "")
}

var _ memqlengine.AppSignInRelay = (*AppSignInRelay)(nil)
