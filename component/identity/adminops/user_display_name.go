package adminops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// SetUserDisplayName changes one directory field. Unlike UpdateUserProfile,
// omitted profile fields are not edits, and no caller-supplied payload can
// carry roles, sign-in policy or preferences through this capability.
func (s *Service) SetUserDisplayName(ctx context.Context, userID, displayName string) Result {
	userID, displayName = strings.TrimSpace(userID), strings.TrimSpace(displayName)
	detail := map[string]any{"userId": userID, "displayName": displayName}
	act, refusal, allowed := s.authorize(ctx, "changing a display name", detail)
	if !allowed {
		return refusal
	}
	const action = "user_display_name_changed"
	if userID == "" || displayName == "" {
		return fail(CodeInvalidArgument, s.emit(ctx, identity.AuditCategoryAdmin, action,
			act, userID, "", detail, identity.AuditOutcomeFailure, "missing_user_id_or_display_name"),
			"identity admin: userId and displayName are required")
	}

	user, err := s.userById(ctx, userID)
	if err != nil {
		return s.finish(ctx, identity.AuditCategoryAdmin, action, act, userID, "", detail, "", err)
	}
	if user == nil {
		return s.notFound(ctx, action, act, userID, detail, nil)
	}
	if user.CreatedAt.IsZero() {
		return fail(CodeFailedPrecondition, s.emit(ctx, identity.AuditCategoryAdmin, action,
			act, userID, user.PrimaryEmail, detail, identity.AuditOutcomeFailure, "missing_user_version"),
			"identity admin: cannot change a user without its current version")
	}

	// The fence compares the fresh version and serializes persistence across
	// replicas. A changed row is refused, never overwritten from a stale read.
	writeCtx := memqlengine.ContextWithRowVersionFence(ctx, "v1:identity:user", user.ID, user.CreatedAt)
	_, err = s.Engine.Execute(auth.ContextWithInternalOrigin(writeCtx),
		fmt.Sprintf(`mutation updateUser(userId: %s, payload: {displayName: %s})`, quote(user.ID), quote(displayName)))
	if errors.Is(err, memqlengine.ErrRowVersionChanged) {
		return fail(CodeFailedPrecondition, s.emit(ctx, identity.AuditCategoryAdmin, action,
			act, userID, user.PrimaryEmail, detail, identity.AuditOutcomeFailure, "user_version_changed"),
			"identity admin: the user changed; read the current user and try again")
	}
	// Audit with the original context: the successful write superseded the
	// witness, which must not fence the independent audit event.
	return s.finish(ctx, identity.AuditCategoryAdmin, action, act, userID, user.PrimaryEmail,
		detail, "Display name saved.", err)
}
