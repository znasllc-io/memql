package memql

import (
	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity/adminops"
)

// identity_admin_handlers.go is the stream landing for the identity-admin
// write surface (memql#3324).
//
// This handler is deliberately thin, and the thinness is the security
// property -- the same argument deploy_control_handlers.go makes. It does NOT
// check roles, write audit events, or validate arguments. Every one of those
// lives in component/identity/adminops, so there is exactly one copy of the
// owner/admin rule and exactly one copy of the audit write, and a future
// second transport cannot disagree with this one.
//
// What it DOES do is stamp the session's resolved AccessContext onto the
// context it hands down. That single line is the whole of the auth plumbing:
// adminops.resolveActor reads the same AccessContext the deploy gate reads, so
// the caller the gate sees is the caller the stream interceptor verified. A
// nil access context is left alone on purpose -- the gate then fails closed
// with UNAUTHENTICATED, which is the behaviour we want and NOT something to
// paper over here.

// SetIdentityAdminHandler installs the gated identity-admin service the
// stream dispatches to. Nil leaves the surface answering UNIMPLEMENTED rather
// than pretending it exists.
//
// Mirrors SetDeployControlHandler: it updates both the Server field (picked up
// by the next service construction in Run) and the live service reference (so
// a post-Run install still takes effect).
func (s *Server) SetIdentityAdminHandler(h *adminops.Service) {
	if s == nil {
		return
	}
	s.identityAdminHandler = h
	if s.serviceRef != nil && s.serviceRef.svc != nil {
		s.serviceRef.svc.identityAdminHandler = h
	}
}

// handleIdentityAdmin dispatches one IdentityAdminMsg and returns its reply.
//
// Failures never return a Go error: an error out of a handler tears down the
// multiplexed stream, taking every other in-flight request with it. The gRPC
// status rides inside IdentityAdminResult (error_code / error_message), so a
// caller mapping the code back observes what a unary caller would.
func (s *streamSession) handleIdentityAdmin(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.IdentityAdminMsg) error {
	out := &memqlv1.IdentityAdminResult{RequestId: msg.GetRequestId()}

	svc := s.service.identityAdminHandler
	if svc == nil {
		out.ErrorCode = 12 // UNIMPLEMENTED
		out.ErrorMessage = "identity admin: this node has no identity-admin service wired"
		return s.replyIdentityAdmin(envelope, out)
	}

	ctx := s.stream.Context()
	if ac := s.ensureAccess(ctx); ac != nil {
		ctx = auth.ContextWithAccess(ctx, ac)
	}

	var res adminops.Result
	switch req := msg.GetRequest().(type) {
	case *memqlv1.IdentityAdminMsg_UpdateUserProfile:
		p := req.UpdateUserProfile
		res = svc.UpdateUserProfile(ctx, adminops.UserProfile{
			UserId:      p.GetUserId(),
			DisplayName: p.GetDisplayName(),
			FirstName:   p.GetFirstName(),
			LastName:    p.GetLastName(),
			Phone:       p.GetPhone(),
			PrimaryRole: p.GetPrimaryRole(),
			Gender:      p.GetGender(),
			Birthdate:   p.GetBirthdate(),
		})
	case *memqlv1.IdentityAdminMsg_SetUserRole:
		res = svc.SetUserRole(ctx, req.SetUserRole.GetUserId(), req.SetUserRole.GetRole())
	case *memqlv1.IdentityAdminMsg_SetUserSuspended:
		p := req.SetUserSuspended
		res = svc.SetUserSuspended(ctx, p.GetUserId(), p.GetSuspended(), p.GetReason())
	case *memqlv1.IdentityAdminMsg_RevokeUserToken:
		res = svc.RevokePersonalAccessToken(ctx, req.RevokeUserToken.GetIdentityId())
	case *memqlv1.IdentityAdminMsg_RevokeNodeToken:
		res = svc.RevokeNodeToken(ctx, req.RevokeNodeToken.GetIdentityId())
	case *memqlv1.IdentityAdminMsg_UpdateClusterSettings:
		p := req.UpdateClusterSettings
		res = svc.UpdateClusterSettings(ctx, adminops.ClusterSettings{
			BrandName:                 p.GetBrandName(),
			BrandPrimaryColor:         p.GetBrandPrimaryColor(),
			BrandLogoDataURI:          p.GetBrandLogoDataUri(),
			BrandIconDataURI:          p.GetBrandIconDataUri(),
			RegistrationMode:          p.GetRegistrationMode(),
			RegistrationDomains:       p.GetRegistrationDomains(),
			InternalDomains:           p.GetInternalDomains(),
			InternalDefaultRole:       p.GetInternalDefaultRole(),
			RegisteredClientsJSON:     p.GetRegisteredClientsJson(),
			AccessRequestNotifyEmails: p.GetAccessRequestNotifyEmails(),
			AccessTokenTTLSeconds:     int(p.GetAccessTokenTtlSeconds()),
			RefreshTokenTTLSeconds:    int(p.GetRefreshTokenTtlSeconds()),
			MagicLinkTTLSeconds:       int(p.GetMagicLinkTtlSeconds()),
			InvitationTTLDays:         int(p.GetInvitationTtlDays()),
			RefreshCookieSameSite:     p.GetRefreshCookieSameSite(),
		})
	default:
		out.ErrorCode = adminops.CodeInvalidArgument
		out.ErrorMessage = "identity admin: identity_admin message carries no request"
		return s.replyIdentityAdmin(envelope, out)
	}

	out.Ok = res.OK
	out.ErrorCode = res.Code
	out.ErrorMessage = res.ErrorMessage
	out.AuditEventId = res.AuditEventId
	out.Message = res.Message

	if s.logger != nil && !res.OK {
		s.logger.Warn("identity admin (streamed) rejected",
			"request_id", msg.GetRequestId(),
			"error_code", res.Code,
			"error_message", res.ErrorMessage,
			"audit_event_id", res.AuditEventId)
	}

	return s.replyIdentityAdmin(envelope, out)
}

func (s *streamSession) replyIdentityAdmin(envelope *memqlv1.MemqlClientMessage, out *memqlv1.IdentityAdminResult) error {
	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_IdentityAdminResult{IdentityAdminResult: out},
	})
}
