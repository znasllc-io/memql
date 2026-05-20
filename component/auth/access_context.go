package auth

import "context"

// AccessContext is the resolved authorization state for a single gRPC
// stream. It is built once when the middleware sees the first request
// on a connection (after the identity-issued JWT has been validated)
// and attached to every subsequent ctx on that stream.
//
//   UserId         v1:identity:user.id
//   PrimaryEmail   denormalized from the user record
//   Role           cluster-wide role (owner / admin / writer / reader)
//   IdentityId     which v1:identity:identity the caller authenticated with
//
// Per-row authorization (see docs/auth/per-row-authz-audit.md) is the
// only gate post-#56; the partition-ACL dimension that previously
// lived here was retired in phase 4.
type AccessContext struct {
	UserId       string
	PrimaryEmail string
	Role         Role
	IdentityId   string
}

// AccessContextKey is the context key for AccessContext values.
const AccessContextKey contextKey = "accessContext"

// AccessFromContext extracts the AccessContext attached by the auth
// middleware. Returns nil/false if no context has been set -- callers
// should fail closed in that case.
func AccessFromContext(ctx context.Context) (*AccessContext, bool) {
	if ctx == nil {
		return nil, false
	}
	ac, ok := ctx.Value(AccessContextKey).(*AccessContext)
	return ac, ok && ac != nil
}

// ContextWithAccess returns a new context carrying the given
// AccessContext.
func ContextWithAccess(ctx context.Context, ac *AccessContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if ac == nil {
		return ctx
	}
	return context.WithValue(ctx, AccessContextKey, ac)
}

// IsClusterOwner returns true when the caller is a cluster-wide owner.
func (ac *AccessContext) IsClusterOwner() bool {
	return ac != nil && ac.Role == RoleOwner
}
