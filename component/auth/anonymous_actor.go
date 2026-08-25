package auth

import (
	"context"
	"os"
	"strconv"
	"strings"
)

// THE ANONYMOUS ACTOR (epic memql#4541, D4).
//
// A hosted site serving visitors who have not signed in needs a way to read
// the graph. Until now there was none: the WS bridge refuses a session with
// no identity, and the engine refuses a row-authz read with no actor. So the
// `public` row-authz tier -- which has parsed since the tier model was
// written -- described a capability nothing could exercise, and
// site-hosting.md recorded it as a gap.
//
// This is that capability, and the whole of it. An anonymous actor is a
// first-class named actor in the ConnectorActor mould rather than an absent
// one, for the same reason: absence decides nothing, and code that branches
// on absence has to guess.
//
// # The load-bearing rule
//
// An anonymous actor is admitted to concepts declaring @rowAuthz(public),
// and to NOTHING ELSE -- including the roughly 88 concepts that declare no
// tier at all and therefore admit every caller today.
//
// That exception is the entire point and it is easy to get backwards. An
// undeclared concept is not "safe by default"; it is UNMEASURED, in the
// undeclared gate's own words. Letting an anonymous visitor inherit
// undeclared-admits-everyone would mean shipping a public tier whose first
// act is to publish most of the graph, and it would do so silently, since
// every gate involved would report exactly what it was asked. So the
// anonymous branch answers FIRST and never falls through, in both
// directions -- the same shape, for the same reason, as connectorAdmission.
//
// # Reads only, and never a write
//
// There is no anonymous write, ever. `public` is a READ tier: it says who
// may see a row, and it says nothing about who may create one. The engine
// enforces that at the mutation actor check (a write needs a real actor)
// and at the surface pin on the bridge, which admits query and subscribe
// payloads and refuses every mutation. Two independent gates, because this
// is the one direction where a mistake is unrecoverable.
//
// # It is never minted from a request's credentials
//
// Nothing about the caller produces one of these. An anonymous actor exists
// because a bridge that was configured to accept unauthenticated sessions
// accepted one -- a cluster-level opt-in, default OFF -- and for no other
// reason. A malformed or absent credential does NOT degrade into anonymous:
// it is refused exactly as it is today.

// RoleAnonymous is the role an anonymous actor carries.
//
// It is NOT in ValidRoles(): nobody may be assigned it, so no identity can
// be issued with it and no delegation can be capped to it. It is also
// outside the rank model, so RoleLevel resolves it as the LEAST privileged
// tier anywhere roles are ranked -- if this value ever appeared on an
// ordinary request context it would grant less than a reader, not more.
//
// The power an anonymous actor has comes entirely from the targeted
// admission rule keyed on the concept's declared tier, never from the role.
const RoleAnonymous Role = "anonymous"

// AnonymousUserId is the synthetic UserId an anonymous actor carries.
//
// It is a CONSTANT, and that is load-bearing in two places:
//
//   - The result cache keys on the resolved actor identity
//     (actorCacheKeyComponent). One constant identity means every anonymous
//     visitor shares one cache key, so a public read is computed once and
//     served to everyone -- which is why the public tier belongs to the
//     caching push rather than beside it. Anything per-visitor here (a
//     session id, a request id) would silently give every visitor their own
//     cache entry and turn the best-cached data in the system into the
//     worst.
//   - Several engine surfaces treat a blank UserId as "no identity" and
//     refuse. An anonymous actor is not a caller who failed to
//     authenticate; it is a caller authorized by a different rule, and a
//     refusal is the wrong answer for it.
//
// It cannot collide with a real user id: ids are minted by the identity
// service and none of them carry a colon-prefixed literal like this.
const AnonymousUserId = "anonymous:public"

// AnonymousActor builds the AccessContext an anonymous public read runs
// under.
//
// No parameters, deliberately. Every anonymous visitor is the same actor --
// see AnonymousUserId. A variant carrying a session, an IP or a request id
// would be a per-visitor identity, which is both a cache-key hazard and an
// invitation to write a filter that branches on which stranger is asking.
func AnonymousActor() *AccessContext {
	return &AccessContext{
		UserId:      AnonymousUserId,
		Role:        RoleAnonymous,
		IsAnonymous: true,
	}
}

// ContextWithAnonymousActor stamps the anonymous actor onto ctx.
func ContextWithAnonymousActor(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return ContextWithAccess(ctx, AnonymousActor())
}

// IsAnonymousActor reports whether the caller on ctx is the anonymous
// actor.
//
// Reads the explicit flag rather than comparing the UserId string. The
// prefix exists so a log line is legible; it is not a protocol, and
// inferring an authorization decision from a string shape is how a value
// somebody can influence becomes a permission.
func IsAnonymousActor(ctx context.Context) bool {
	ac, ok := AccessFromContext(ctx)
	if !ok {
		return false
	}
	return ac.IsAnonymousActor()
}

// PublicReadsEnabledEnv is the cluster opt-in: whether this cluster has an
// anonymous read surface AT ALL.
const PublicReadsEnabledEnv = "MEMQL_PUBLIC_READS_ENABLED"

// PublicReadsEnabled reports whether the operator opted this cluster into
// accepting unauthenticated sessions.
//
// DEFAULT FALSE, and that default is the feature's whole safety story on
// every cluster that exists today: with it off, a dial with no credential is
// refused byte-for-byte as it always has been, no anonymous actor is ever
// constructed, and nothing in this file runs. Enabling it does not publish
// anything either -- it opens a door into a graph where no concept declares
// the public tier, so every read through it refuses until an author declares
// one.
//
// It is a VALUE, not an architecture branch: the same binary, the same
// topology, the same code path, configured differently (env-parity rules).
// A blank or unparseable value reads as false -- an anonymous surface is
// something an operator turns on deliberately, so a typo must not turn one
// on, and "0"/"no"/"off"/"" all resolve the same way strconv does.
func PublicReadsEnabled() bool {
	v := strings.TrimSpace(os.Getenv(PublicReadsEnabledEnv))
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

// IsAnonymousActor reports whether this actor is the anonymous one.
//
// BOTH halves must agree, matching ConnectorNameValue's contract: the flag
// without RoleAnonymous, or RoleAnonymous without the flag, is a malformed
// actor. It answers false -- which DENIES rather than admits, since the
// anonymous rule is the only thing that would have granted it anything, and
// every other tier refuses a caller whose UserId matches no row.
func (ac *AccessContext) IsAnonymousActor() bool {
	if ac == nil {
		return false
	}
	return ac.IsAnonymous && ac.Role == RoleAnonymous
}
