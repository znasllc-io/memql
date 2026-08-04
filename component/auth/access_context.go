package auth

import (
	"context"
	"strings"
)

// AccessContext is the resolved authorization state for a single gRPC
// stream. It is built once when the middleware sees the first request
// on a connection (after the identity-issued JWT has been validated)
// and attached to every subsequent ctx on that stream.
//
//	UserId         v1:identity:user.id
//	PrimaryEmail   denormalized from the user record
//	Role           cluster-wide role (owner / admin / writer / reader)
//	IdentityId     which v1:identity:identity the caller authenticated with
//
// Per-row authorization (see docs/public/operate/auth/per-row-authz-audit.md) is the
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

// ContextWithUserActor stamps a synthetic user actor for work a server-side
// handler performs ON BEHALF OF a specific user -- an agent promoting a
// deliverable to its owner's library, a workbench writing back a file, a
// document edit attributed to the document's owner rather than to whoever
// triggered it.
//
// It sets all THREE surfaces, and that is the whole point of the helper
// (memql#2989):
//
//   - claims + TokenInfo, which is what `createdBy` and the mutation-actor
//     presence check read (component/memql/executor.go -> auth.ActorFromContext);
//   - the AccessContext, which is what `actor.userId` reads in a DSL
//     `stamp { }` / filter / spec (component/memql/mutation_templates.go's
//     resolveActorReference -> auth.AccessFromContext).
//
// Setting only the first two is the trap this helper exists to close. Five
// packages each carried a byte-identical copy that did exactly that, so a
// mutation stamping `ownerUserId: actor.userId` under one of them silently
// resolved to the INBOUND caller -- or to "" on a detached context, because
// ActorEnvelopeValue returns ("", true) for a nil AccessContext rather than an
// error. component/server/server.go records the same trap for
// contextWithSystemActor.
//
// Role is RoleWriter, matching the other synthetic-actor helpers in the tree
// (component/memql/authoring_session.go, authoring_promote_durable.go): these
// contexts exist to WRITE on the user's behalf and nothing more.
//
// A blank userId returns ctx UNCHANGED, so a caller that cannot resolve an
// owner must refuse before calling rather than relying on this to fail --
// otherwise the work is attributed to whoever the inbound caller happened to
// be. Every call site guards for this.
func ContextWithUserActor(ctx context.Context, userId string) context.Context {
	if strings.TrimSpace(userId) == "" {
		return ctx
	}
	claims := map[string]any{
		"sub":   userId,
		"email": userId,
		"role":  "user",
	}
	ctx = ContextWithClaims(ctx, claims)
	ctx = ContextWithToken(ctx, BuildTokenInfo(claims))
	return ContextWithAccess(ctx, &AccessContext{UserId: userId, Role: RoleWriter})
}

// IsClusterOwner returns true when the caller is a cluster-wide owner.
func (ac *AccessContext) IsClusterOwner() bool {
	return ac != nil && ac.Role == RoleOwner
}
