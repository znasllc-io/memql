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

	// IsAnonymous is set ONLY on the anonymous actor (D4,
	// anonymous_actor.go): the caller with no identity that a
	// public-reads-enabled bridge admits, and that row admission lets
	// through to @rowAuthz(public) concepts and to nothing else.
	//
	// It is a distinct FLAG rather than a role comparison because both
	// halves have to agree for the actor to count as anonymous -- a
	// malformed one must deny, and a single field cannot express that.
	// AnonymousActor is the only constructor, and no request shape sets it:
	// a missing or malformed credential is REFUSED, never degraded into
	// this.
	//
	// Like ConnectorName it is a Go field and NOT part of the DSL actor
	// envelope. The envelope's field set is closed and is an authoring
	// surface; a filter that branched on "is the caller anonymous" would be
	// restating, per construct, the rule the concept's own @rowAuthz(public)
	// declaration already makes once.
	IsAnonymous bool

	// Unranked marks an actor the RANK RULES DO NOT GOVERN (epic
	// memql#4832, D4). Rank-visible reads and rank-strict writes are
	// statements about PRINCIPALS -- people holding a rung on the role
	// ladder -- and the cluster's own characterised identities hold no
	// rung: MaintenanceActor, the seed materializer, an automation's
	// system actor, and borrowed authority via ContextWithUserActor.
	//
	// WITHOUT THIS, EVERY RETENTION SWEEP AND BOOT SEED BECOMES A
	// PEER-WRITE AND STOPS -- and a sweep that retires nothing looks
	// exactly like a sweep with nothing to retire, which is why D4 is
	// stated in the design rather than left to be discovered.
	//
	// IT IS AN EXPLICIT FLAG BECAUSE THERE IS NOTHING ELSE HONEST TO READ.
	// The maintenance actor and the seed materializer both carry
	// RoleOwner, so a role check cannot tell them from a real operator;
	// their synthetic `system:` UserId prefix CAN, and this tree forbids
	// that in as many words -- "the prefix exists so a log line is
	// legible; it is not a protocol, and inferring an authorization
	// decision from a string shape is how a value somebody can influence
	// becomes a permission" (anonymous_actor.go). So the constructors say
	// it, once, where the actor is built.
	//
	// It does NOT grant anything. It is read only where the rank rules
	// would otherwise apply, and every other gate -- the owned tier, the
	// cluster-owner escape, internal origin -- answers exactly as before.
	//
	// Like ConnectorName and IsAnonymous it is a Go field and NOT part of
	// the DSL actor envelope, for the same reason: the envelope is a
	// closed authoring surface, and a filter branching on "is the caller a
	// system identity" would restate per construct a rule the engine makes
	// once.
	Unranked bool

	// Synthetic marks an actor that is THE CLUSTER ACTING RATHER THAN A
	// PERSON, and it is a narrower statement than Unranked above.
	//
	// THE TWO ARE NOT THE SAME PROPERTY, and conflating them is a mistake
	// this field exists because of. Unranked says "the rank rules do not
	// govern this actor". Synthetic says "this actor can never be a row's
	// OWNER". All three of MaintenanceActor, the seed materializer and an
	// automation's system actor are both. BORROWED AUTHORITY
	// (ContextWithUserActor) is only the first: its synthetic RoleWriter must
	// not be read as a rung, but its UserId is a real person's, and rows it
	// creates are genuinely theirs.
	//
	// The failure that separated them: undoNonPrincipalOwnerStamp keyed on
	// Unranked and blanked `ownerUserId` on every row written through
	// borrowed authority -- the worker's delegation policy, an app session --
	// leaving them owned by nobody. Three db-gated tests caught it; no unit
	// test could, because the stamp only happens on a real write.
	//
	// Set by the three synthetic constructors and by nothing a request can
	// reach, exactly like ConnectorName and IsAnonymous above.
	Synthetic bool
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
	// Unranked: this is BORROWED authority (D4). The context exists so
	// server-side Go can act on one user's behalf, and the rank rules must
	// not read the synthetic RoleWriter above as "this principal sits at
	// rank 100" -- the campaigns drain worker borrows a campaign owner who
	// may be an admin, and a rank comparison against the borrowed role
	// would refuse them their own rows.
	// Unranked but NOT Synthetic. The synthetic RoleWriter above must not be
	// read as a rung -- the campaigns worker borrows a campaign owner who may
	// be an admin -- but the UserId is a real person's, and a row created here
	// is genuinely theirs. See AccessContext.Synthetic for the failure that
	// separated the two.
	return ContextWithAccess(ctx, &AccessContext{UserId: userId, Role: RoleWriter, Unranked: true})
}

// IsClusterOwner returns true when the caller is a cluster-wide owner.
func (ac *AccessContext) IsClusterOwner() bool {
	return ac != nil && ac.Role == RoleOwner
}
