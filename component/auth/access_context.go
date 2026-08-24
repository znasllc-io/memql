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
//	DisplayName    denormalized from the user record, alongside the email
//	Role           cluster-wide role (owner / admin / developer / writer / reader)
//	IdentityId     which v1:identity:identity the caller authenticated with
//
// Per-row authorization (see docs/public/operate/auth/per-row-authz-audit.md) is the
// only gate post-#56; the partition-ACL dimension that previously
// lived here was retired in phase 4.
//
// DISPLAYNAME IS NOT AN AUTHORIZATION INPUT and nothing may branch on it.
// It rides here because it comes off the same user row PrimaryEmail does,
// on a read that has already happened -- fetching it separately would be a
// second query for a field the first one returned (memql#4317). It is not
// projected by the actor envelope (`actor.*` in the DSL) for that reason:
// a name is presentation, and adding it to the envelope would invite a
// filter to key on one.
type AccessContext struct {
	UserId       string
	PrimaryEmail string
	DisplayName  string
	Role         Role
	IdentityId   string

	// ConnectorName is set ONLY on a connector actor (D4,
	// connector_actor.go): the name a mirror's @origin or an origin's
	// @mirroredTo has to match for row admission to let this writer
	// through. Empty on every actor built from a request, and there is
	// no request shape that sets it -- ConnectorActor is the only
	// constructor.
	//
	// It is a Go field and NOT part of the DSL actor envelope. The
	// envelope's field set is closed (actor.userId / role / identityId /
	// isClusterOwner / primaryEmail / now) and is an authoring surface;
	// widening it would let a filter or a spec branch on which
	// connector is writing, which is a rule that belongs in one place
	// -- the concept's own declaration -- rather than restated per
	// construct.
	ConnectorName string
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
