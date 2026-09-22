package auth

import (
	"context"
	"strings"
)

// The SYSTEM ACTOR: the cluster reading and writing its OWN operational
// configuration (memql#5574).
//
// It is the third named internal actor beside ConnectorActor and
// MaintenanceActor, and the three answer three different questions:
//
//   - ConnectorActor says WHICH EXTERNAL SYSTEM is writing, and is admitted
//     by what a concept DECLARES about it -- a mirror's own filler reaching
//     rows a tier would refuse it, and reaching nothing else.
//   - MaintenanceActor says WHICH LISTED SWEEP is running, and is admitted by
//     membership of a closed list.
//   - SystemActor says THE DEPLOYMENT ITSELF is acting, over rows that belong
//     to the deployment rather than to any person or any origin: a site row,
//     a store configuration, a sync-health row, an audit line.
//
// # Why a connector actor is the wrong identity for configuration
//
// This existed three times before it existed once, and the copies had already
// drifted -- which is the whole argument for one definition. `component/edge`
// stamped all three surfaces and explained why; `component/datasync` stamped
// one. The gap is invisible until a WRITE runs under it: `createdBy` resolves
// through ActorFromContext, which reads TokenInfo and NOT AccessContext, so an
// actor carrying only the third surface is "no actor found in context" to
// every mutation.
//
// The failure that drove this out was a fourth site with no copy at all.
// `integrations/shopify` ran its own configuration reads and writes under a
// CONNECTOR actor, and `v1:shopify:store` is @origin("memql") with no
// @mirroredTo -- a NATIVE concept, MemQL's own record of how to reach a store,
// which Shopify has no corresponding object for. So the declaration named no
// connector, connectorAdmission DENIED, and the connector read zero stores:
// not an error, an empty list, which reads exactly like "no stores are
// configured". Declaring the connector on those concepts would have made the
// admission rule pass by writing down something untrue.
//
// # It is an IDENTITY, and deliberately not an origin
//
// ContextWithSystemActor stamps WHO is acting and nothing else. Internal
// origin -- what a @serverOnly construct requires -- is a separate axis, and
// a caller that needs both composes them:
//
//	ctx = auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(ctx, "shopify"))
//
// Bundling the two would hide the second stamp inside a helper named for the
// first, and `connectorContext`'s own comment already states why the tree
// keeps them apart: one answers "who", the other "is the engine doing this".
// Whose authority a call runs under has to be readable at the call site.

// systemUserIdPrefix is the prefix of the synthetic UserId a system actor
// carries.
//
// It cannot collide with a real user id -- those are minted by the identity
// service and none of them carry this prefix -- and it makes the value
// self-describing wherever it lands: a `createdBy` stamp or an audit line on a
// row the deployment wrote says so, by name, forever.
const systemUserIdPrefix = "system:"

// SystemActor builds the AccessContext the deployment's own work runs under.
//
// `name` says WHICH part of the deployment is acting ("edge", "datasync",
// "shopify"), and a blank name returns nil rather than an unnamed owner: a
// caller that cannot say who it is gets no actor at all -- and is then refused
// by every tier -- instead of a cluster-owner identity nobody can attribute.
func SystemActor(name string) *AccessContext {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	return &AccessContext{
		UserId: systemUserIdPrefix + name,
		// RoleOwner is what buys the cluster-owner escape, and it is what an
		// `actor.isClusterOwner == true` conjunct in a hand-written filter is
		// checking. This is precisely why ContextWithUserActor is not a
		// substitute: it hardcodes RoleWriter.
		Role: RoleOwner,
		// Unranked: not a principal, so the rank rules do not govern it (D4,
		// epic memql#4832). RoleOwner above is not a claim to rank 400, and
		// without this flag a rank-strict concept reads this actor as an
		// owner writing a PEER owner's row and refuses the work -- which for
		// a sweep or a reconcile looks exactly like there being nothing to do.
		Unranked: true,
		// Synthetic: this is the cluster acting, so it can never be a row's
		// owner. A mutation stamping `ownerUserId: actor.userId` under this
		// actor would write a row owned by a `system:` id no principal
		// matches -- readable by nobody, including the operator asking where
		// it went (memql#4817).
		Synthetic: true,
	}
}

// ContextWithSystemActor stamps a system actor onto ctx.
//
// All THREE surfaces, and that is the load-bearing part (memql#2989):
// `createdBy` resolves through ActorFromContext -> TokenInfoFromContext, while
// `actor.userId` in a filter or a mutation template reads the AccessContext.
// Stamping only the third is the trap -- reads work, writes fail with "no
// actor found in context", and the two are usually exercised by different
// tests.
//
// A blank name returns ctx UNCHANGED, matching ContextWithConnectorActor: a
// caller that names nobody is left with whatever authority it already had,
// rather than being silently promoted to a cluster owner.
func ContextWithSystemActor(ctx context.Context, name string) context.Context {
	ac := SystemActor(name)
	if ac == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	claims := map[string]any{"sub": ac.UserId, "role": string(RoleOwner)}
	ctx = ContextWithClaims(ctx, claims)
	ctx = ContextWithToken(ctx, BuildTokenInfo(claims))
	return ContextWithAccess(ctx, ac)
}

// IsSystemActorId reports whether an actor id was minted by SystemActor.
//
// For the places that have only the STRING -- an audit line, a `createdBy`
// column, a log field -- and need to say "the deployment did this" without
// reaching for the context it came from.
func IsSystemActorId(id string) bool {
	return strings.HasPrefix(strings.TrimSpace(id), systemUserIdPrefix)
}
