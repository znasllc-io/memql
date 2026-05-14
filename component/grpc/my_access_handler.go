package memql

import (
	"google.golang.org/grpc/codes"

	"github.com/visionarys-io/memql/component/auth"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
)

// handleMyAccess returns the caller's own access record: cluster-wide
// role + the partitions they have grants for. Used by the Cockpit
// Settings tab's "My Access" section.
//
// The handler never reveals other users' grants -- it reads only from
// the caller's resolved AccessContext. The fuller "who has access to
// partition X" use case is served by the membersOfPartition query and
// is gated behind admin/owner roles at the query layer.
func (s *streamSession) handleMyAccess(envelope *memqlv1.MemqlClientMessage, msg *memqlv1.MyAccessMsg) error {
	requestId := ""
	if msg != nil {
		requestId = msg.GetRequestId()
	}

	// Ensure the ACL is loaded for this stream. Without it we have
	// nothing meaningful to return.
	ctx := s.stream.Context()
	ac := s.ensureAccess(ctx)
	if ac == nil {
		return s.sendQueryError(requestId, envelope.GetMessageId(), codes.Unauthenticated, "access context not available")
	}

	grants := make([]*memqlv1.PartitionGrant, 0, len(ac.PartitionACL))
	for partition, role := range ac.PartitionACL {
		grants = append(grants, &memqlv1.PartitionGrant{
			Partition: partition,
			Role:      roleToProto(role),
			// grantedBy / grantedAt / expiresAt / source / sourceRef are
			// not on the hot-path AccessContext today. Clients that need
			// them call accessForUser or membersOfPartition directly.
		})
	}

	result := &memqlv1.MyAccessResult{
		RequestId:    requestId,
		UserId:       ac.UserId,
		PrimaryEmail: ac.PrimaryEmail,
		ClusterRole:  roleToProto(ac.Role),
		Partitions:   grants,
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
	case auth.RoleWriter:
		return memqlv1.UserRole_USER_ROLE_WRITER
	case auth.RoleReader:
		return memqlv1.UserRole_USER_ROLE_READER
	default:
		return memqlv1.UserRole_USER_ROLE_UNSPECIFIED
	}
}
