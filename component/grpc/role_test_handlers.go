package memql

import (
	"context"
	"errors"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/identity/roletest"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

func roleTestPayloadGate(envelope *memqlv1.MemqlClientMessage) badgeGateVerdict {
	switch p := envelope.GetPayload().(type) {
	case *memqlv1.MemqlClientMessage_CreateWorkerToken,
		*memqlv1.MemqlClientMessage_CreateBadge,
		*memqlv1.MemqlClientMessage_CreateAccountToken,
		*memqlv1.MemqlClientMessage_SetSignInPolicy,
		*memqlv1.MemqlClientMessage_DeployControl,
		*memqlv1.MemqlClientMessage_NodeMaintenance:
		return badgeGateRestricted
	case *memqlv1.MemqlClientMessage_IdentityAdmin:
		switch r := p.IdentityAdmin.GetRequest().(type) {
		case *memqlv1.IdentityAdminMsg_RoleTest:
			if r.RoleTest.GetOperation() != "end" {
				return badgeGateRestricted
			}
		case *memqlv1.IdentityAdminMsg_IssueEnrolmentLink,
			*memqlv1.IdentityAdminMsg_IssueUserInvitation,
			*memqlv1.IdentityAdminMsg_RotateRecoveryKey,
			*memqlv1.IdentityAdminMsg_ResetSignInPolicy,
			*memqlv1.IdentityAdminMsg_UpdateClusterSettings:
			return badgeGateRestricted
		}
	}
	return badgeGateAllow
}

func (s *streamSession) roleTest(ctx context.Context, request *memqlv1.RoleTestRequest, out *memqlv1.IdentityAdminResult) {
	// The stream context predates RotateAuth. Minting a session must use
	// the current credential's subject, class, expiry and revocation epoch.
	s.accessMu.Lock()
	s.ensureAuthorityStampedLocked()
	claims := s.credentialClaims
	s.accessMu.Unlock()
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	svc := s.service.identityAdminHandler.RoleTests
	if svc == nil {
		out.ErrorCode = 12
		out.ErrorMessage = "Role testing is not configured"
		return
	}
	var sessions []roletest.Session
	var err error
	switch request.GetOperation() {
	case "start":
		var session roletest.Session
		session, err = svc.Start(ctx, request.GetRole())
		if err == nil {
			sessions = []roletest.Session{session}
			out.RoleTestToken = session.Token
		}
	case "end":
		err = svc.End(ctx, request.GetSessionId())
	case "list":
		sessions, err = svc.List(ctx)
	default:
		out.ErrorCode = 3
		out.ErrorMessage = "Unknown role-test operation"
		return
	}
	if err != nil {
		out.ErrorCode = 13
		out.ErrorMessage = "Role testing could not complete. Check the identity audit log."
		if errors.Is(err, roletest.ErrDenied) || errors.Is(err, roletest.ErrInvalid) {
			out.ErrorCode = 7
			out.ErrorMessage = err.Error()
		}
		return
	}
	out.Ok = true
	for _, session := range sessions {
		out.RoleTestSessions = append(out.RoleTestSessions, &memqlv1.RoleTestSession{Id: session.ID, UserId: memqlengine.BareShortId(session.UserID), Role: session.Role, ExpiresAt: session.ExpiresAt.Unix()})
	}
}
