package memql

import (
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
