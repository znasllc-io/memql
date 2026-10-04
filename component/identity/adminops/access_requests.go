package adminops

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
)

type AccessRequestReview struct{ RequestID, Decision, Role, Note string }

func (s *Service) ReviewAccessRequest(ctx context.Context, in AccessRequestReview) Result {
	detail := map[string]any{"requestId": in.RequestID, "decision": in.Decision}
	actor, refusal, allowed := s.authorize(ctx, "reviewing an access request", detail)
	if !allowed {
		return refusal
	}
	if strings.TrimSpace(in.RequestID) == "" || (in.Decision != "approve" && in.Decision != "reject") || (in.Decision == "reject" && strings.TrimSpace(in.Note) == "") {
		return fail(CodeInvalidArgument, "", "Choose a request and approve or reject it. Rejection requires a reason.")
	}
	var result Result
	err := s.AccessRequests.WithAccessRequestGate(ctx, in.RequestID, func(ctx context.Context) error {
		row, err := s.AccessRequests.ReadAccessRequest(ctx, in.RequestID)
		if err != nil {
			return err
		}
		if row == nil {
			result = fail(CodeNotFound, "", "Access request not found.")
			return nil
		}
		if row.Status != "pending" {
			result = fail(CodeFailedPrecondition, "", "This request has already been reviewed.")
			return nil
		}
		if in.Decision == "reject" {
			_, err = s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), fmt.Sprintf(`mutation rejectAccessRequest(requestId: %s, reviewedBy: %s, reviewerNote: %s)`, quote(row.ID), quote(actor.userID), quote(strings.TrimSpace(in.Note))))
			result = s.finish(ctx, identity.AuditCategoryAdmin, "access_request_rejected", actor, row.ID, row.Email, detail, "Request rejected.", err)
			return nil
		}
		result = s.IssueUserInvitation(ctx, UserInvitation{Email: row.Email, Role: in.Role,
			beforeDelivery: func(invitationID string) error {
				_, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), fmt.Sprintf(`mutation approveAccessRequest(requestId: %s, reviewedBy: %s, invitationId: %s, reviewerNote: %s)`, quote(row.ID), quote(actor.userID), quote(invitationID), quote(strings.TrimSpace(in.Note))))
				return err
			},
		})
		if result.OK {
			s.emit(ctx, identity.AuditCategoryAdmin, "access_request_approved", actor, row.ID, row.Email, detail, identity.AuditOutcomeSuccess, "")
		}
		return nil
	})
	if err != nil {
		return s.finish(ctx, identity.AuditCategoryAdmin, "access_request_review_failed", actor, in.RequestID, "", detail, "", err)
	}
	return result
}
