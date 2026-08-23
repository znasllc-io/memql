package memql

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// handleMyAccess returns the caller's own identity record: userId,
// email, cluster-wide role. Used by the Cockpit Settings tab's
// "My Access" section.
//
// The PartitionGrants field on the wire is kept as an empty list for
// proto compatibility until the partition wire dimension is dropped
// in #56 phase 8.
func (s *streamSession) handleMyAccess(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.MyAccessMsg) error {
	requestId := ""
	if msg != nil {
		requestId = msg.GetRequestId()
	}

	ctx := s.stream.Context()
	ac := s.ensureAccess(ctx)
	if ac == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unauthenticated, "access context not available")
	}

	result := &memqlv1.MyAccessResult{
		RequestId:    requestId,
		UserId:       ac.UserId,
		PrimaryEmail: ac.PrimaryEmail,
		ClusterRole:  roleToProto(ac.Role),
		SessionId:    sessionIdFromClaims(ctx),
	}

	return s.sendServerMessage(envelope.GetMessageId(), &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_MyAccessResult{
			MyAccessResult: result,
		},
	})
}

// roleToProto maps the auth.Role string constants to the UserRole
// proto enum.
func roleToProto(r auth.Role) memqlv1.UserRole {
	switch r {
	case auth.RoleOwner:
		return memqlv1.UserRole_USER_ROLE_OWNER
	case auth.RoleAdmin:
		return memqlv1.UserRole_USER_ROLE_ADMIN
	case auth.RoleDeveloper:
		return memqlv1.UserRole_USER_ROLE_DEVELOPER
	case auth.RoleWriter:
		return memqlv1.UserRole_USER_ROLE_WRITER
	case auth.RoleReader:
		return memqlv1.UserRole_USER_ROLE_READER
	default:
		return memqlv1.UserRole_USER_ROLE_UNSPECIFIED
	}
}

// sessionIdFromClaims reads the `sid` claim off the VERIFIED token
// (memql#4306).
//
// Verified is the operative word: these claims were attached by the identity
// verifier after checking the signature, so this is the server reporting what
// it already believes rather than trusting anything the client said. That is
// the whole reason the field exists -- the portal refuses to decode JWTs by
// standing rule, and a client that parsed its own bearer to find the session
// id would be making decisions from claims nobody promised it.
//
// Empty for a credential that carries no session -- a PAT, an operator key, a
// service-account token. Not an error: those bearers have no session row to
// name, and a client marking "this device" simply marks nothing.
func sessionIdFromClaims(ctx context.Context) string {
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return ""
	}
	sid, _ := claims["sid"].(string)
	return strings.TrimSpace(sid)
}
