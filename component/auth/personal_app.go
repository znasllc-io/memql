package auth

import "context"

// CanManageOwnAppData admits a signed-in person's personal Files/Ask actions.
// App access includes managing their own content even without shared-data write
// authority. This is never a row grant: callers must still enforce ownership,
// server-only writes, token class and action-specific validation.
func CanManageOwnAppData(ctx context.Context, app string) bool {
	if app != "files" && app != "ask" {
		return false
	}
	access, ok := AccessFromContext(ctx)
	if !ok || access == nil || access.Synthetic || access.Unranked || access.IsAnonymousActor() || access.IsConnector() {
		return false
	}
	subject, ok := SubjectFromContext(ctx)
	return ok && subject.UserId != "" && RoleRank(subject.Role) > 0 &&
		CapableFor(ctx, subject, VerbRead, ResourceData) &&
		CapableFor(ctx, subject, VerbRead, "app:"+app)
}
