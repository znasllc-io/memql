package memql

import (
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/adminops"
)

// self_signin_policy_handler.go -- SetSignInPolicyMsg (memql#4319).
//
// The portal's Security tab renders the passkey-only switch, and this is how
// it writes. Everything that decides whether the write may happen lives in
// adminops.SetOwnSignInPolicy; this file is the transport, and deliberately
// holds no rule of its own -- a second copy of "you need a passkey first" is
// how the two surfaces come to disagree.
//
// WHY THIS IS NOT AN IdentityAdminMsg REQUEST. That message's every arm runs
// through adminops.authorize, the owner/admin gate. Adding a self-service arm
// to it would put a case inside a message whose contract is "owner or admin
// only", and the next person adding a gate would have to notice one member of
// the union is exempt. A separate message says what it is.

// handleSetSignInPolicy sets the caller's OWN sign-in policy.
//
// Refusals ride the result payload rather than a stream error, for the reason
// every handler on this multiplexed stream does: returning a Go error tears
// the connection down and takes every other in-flight request with it.
func (s *streamSession) handleSetSignInPolicy(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.SetSignInPolicyMsg) error {
	if msg == nil {
		return nil
	}
	requestId := s.normalizeRequestId(envelope, msg.GetRequestId())
	correlate := envelope.GetMessageId()

	send := func(result *memqlv1.SetSignInPolicyResult) error {
		result.RequestId = requestId
		return s.sendServerMessage(correlate, &memqlv1.MemqlServerMessage{
			Payload: &memqlv1.MemqlServerMessage_SetSignInPolicyResult{
				SetSignInPolicyResult: result,
			},
		})
	}

	svc := s.service.identityAdminHandler
	if svc == nil {
		return send(&memqlv1.SetSignInPolicyResult{
			ErrorCode:    "unavailable",
			ErrorMessage: "this node has no identity service wired",
		})
	}

	ctx := s.stream.Context()
	if ac := s.ensureAccess(ctx); ac != nil {
		ctx = auth.ContextWithAccess(ctx, ac)
	}

	policy := strings.TrimSpace(msg.GetPolicy())
	res := svc.SetOwnSignInPolicy(ctx, policy)
	if res.OK {
		return send(&memqlv1.SetSignInPolicyResult{
			Success: true,
			Policy:  policy,
		})
	}

	// The policy the caller still has. Reported on every refusal so a client
	// re-renders the truth rather than leaving a switch showing the state it
	// optimistically flipped to. Unreadable is reported as empty, which a
	// client shows as "unknown" rather than guessing.
	current, err := svc.OwnSignInPolicy(ctx)
	if err != nil {
		current = ""
	}

	return send(&memqlv1.SetSignInPolicyResult{
		Policy:       current,
		ErrorCode:    signInPolicyErrorCode(res),
		ErrorMessage: res.ErrorMessage,
	})
}

// signInPolicyErrorCode maps adminops' canonical gRPC codes onto the string
// codes this reply carries.
//
// Strings rather than the numeric code, matching the sibling session replies:
// a browser branches on "no_passkey" far more legibly than on 9, and the two
// FailedPrecondition refusals -- no passkey vs the count could not be read --
// need different words from a client, which one number cannot give it.
func signInPolicyErrorCode(res adminops.Result) string {
	switch res.Code {
	case adminops.CodeUnauthenticated:
		return "unauthenticated"
	case adminops.CodeInvalidArgument:
		return "invalid"
	case adminops.CodeFailedPrecondition:
		if res.ErrorMessage == identity.SignInPolicyPrecheckFailedMessage {
			return "precondition_unknown"
		}
		return "no_passkey"
	case adminops.CodeNotFound:
		return "not_found"
	default:
		return "unavailable"
	}
}
