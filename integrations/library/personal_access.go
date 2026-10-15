package library

import (
	"context"

	"github.com/znasllc-io/memql/component/auth"
)

// This only admits the action. Every document handler subsequently resolves its
// artifact and backing row under the unchanged caller's owner-scoped context.
func canManageDocument(ctx context.Context, verb string) bool {
	subject, ok := auth.SubjectFromContext(ctx)
	return ok && (auth.CapableFor(ctx, subject, verb, auth.ResourceData) || auth.CanManageOwnAppData(ctx, "files"))
}
