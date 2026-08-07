package auth

import (
	"context"
)

// Helpers for propagating the authenticated principal across inter-node RPC
// boundaries.
//
// The use case: a BFF receives a request from a client, authenticates it
// (identity-issued JWT, or the no-auth dev shim), and then forwards some
// part of the work to a worker node (Voice / Agent). The worker needs to
// know who the original caller was so it can enforce the same ACLs the BFF
// would have.
//
// The wire uses a proto map<string, string> (see AiForwardRequest.auth in
// component/node/node.proto). These helpers pack the BFF-side UserIdentity
// into that map and rebuild a TokenInfo on the worker side.

// Canonical claim keys used in the forwarded map. They mirror common JWT
// claim names so the existing BuildTokenInfo / UserIdentityFromContext
// paths pick them up without any special-case handling.
const (
	forwardedClaimSubject     = "sub"
	forwardedClaimEmail       = "email"
	forwardedClaimPhoneNumber = "phone_number"
	forwardedClaimRole        = "role"
	forwardedClaimFirstName   = "given_name"
	forwardedClaimLastName    = "family_name"
	// forwardedClaimLocalDev marks a synthetic no-auth identity so the
	// worker can log it distinctly if it wants to. The worker still
	// treats it as a valid principal; production clusters simply never
	// set this flag.
	forwardedClaimLocalDev = "memql_local_dev"

	// The `class` / `role_ceiling` claim keys that lived here are gone with
	// WithForwardedAuthorityContext (memql#3205). A badge grant's
	// authorization envelope now rides the TYPED, mandatory ForwardedAuthority
	// assertion rather than two optional string keys in this map -- see
	// forward_authority.go. This map is `createdBy` attribution only.
)

// ForwardedClaimsFromIdentity packs a UserIdentity into the
// map<string,string> shape used to propagate claims over an inter-node
// RPC. Empty fields are omitted so the map stays compact on the wire.
func ForwardedClaimsFromIdentity(id UserIdentity) map[string]string {
	out := make(map[string]string, 6)
	if id.Subject != "" {
		out[forwardedClaimSubject] = id.Subject
	}
	if id.Email != "" {
		out[forwardedClaimEmail] = id.Email
	}
	if id.PhoneNumber != "" {
		out[forwardedClaimPhoneNumber] = id.PhoneNumber
	}
	if id.Role != "" {
		out[forwardedClaimRole] = id.Role
	}
	if id.FirstName != "" {
		out[forwardedClaimFirstName] = id.FirstName
	}
	if id.LastName != "" {
		out[forwardedClaimLastName] = id.LastName
	}
	return out
}

// WithForwardedAuthorityContext stood here until memql#3205. It packed the
// badge grant's `class` / `role_ceiling` into an already-packed claims map as
// two OPTIONAL keys, and carried a long signpost explaining why wiring it
// naively would reintroduce the defect it existed to help fix.
//
// It is deleted rather than wired, because the signpost's own conclusion was
// that the SHAPE could not be made safe: two optional claims whose absence is
// indistinguishable from "no badge" cannot carry a ceiling however they are
// sourced. The replacement makes the assertion mandatory, explicit and typed --
// see ForwardedAuthority in forward_authority.go, where "no badge" is the VALUE
// credential_class="user". Leaving this in the tree beside it would have been a
// second, weaker way to say the same thing.

// ForwardedClaimsWithLocalDev is ForwardedClaimsFromIdentity plus a
// marker indicating the identity was synthesised by the no-auth dev
// shim. The worker sees `memql_local_dev=1` in its TokenInfo.Claims.
func ForwardedClaimsWithLocalDev(id UserIdentity) map[string]string {
	m := ForwardedClaimsFromIdentity(id)
	m[forwardedClaimLocalDev] = "1"
	return m
}

// TokenInfoFromForwardedClaims rebuilds a TokenInfo from the
// map<string,string> shape produced by ForwardedClaimsFromIdentity.
// Returns nil when the map is empty or lacks any recognised claim.
//
// The returned TokenInfo is suitable for ContextWithToken -- it will be
// picked up by TokenInfoFromContext and UserIdentityFromContext on the
// worker side just like a JWT-derived TokenInfo.
func TokenInfoFromForwardedClaims(m map[string]string) *TokenInfo {
	if len(m) == 0 {
		return nil
	}
	claims := make(map[string]any, len(m))
	for k, v := range m {
		claims[k] = v
	}
	return BuildTokenInfo(claims)
}

// ContextWithForwardedClaims applies TokenInfoFromForwardedClaims and
// attaches the resulting TokenInfo to the context. Returns ctx unchanged
// when the map is empty (no principal to propagate).
func ContextWithForwardedClaims(ctx context.Context, m map[string]string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	tok := TokenInfoFromForwardedClaims(m)
	if tok == nil {
		return ctx
	}
	ctx = ContextWithToken(ctx, tok)
	// Also populate ClaimsContextKey so downstream code that reads
	// claims directly (rather than through TokenInfo) still works.
	ctx = ContextWithClaims(ctx, tok.Claims)
	return ctx
}

// forwardedAuthorityContextKey carries the VERIFIED assertion a worker accepted,
// so a second hop out of that worker can re-assert it rather than rebuild one.
const forwardedAuthorityContextKey contextKey = "forwardedAuthority"

// ContextWithForwardedAuthority records the assertion a receiver verified and
// bound. It is chain-of-custody state, not an authorization input: nothing
// reads it to make a decision. The one consumer is a producer -- a node that
// received a forward and is about to make one of its own (memql#3219).
//
// The alternative is what makes it necessary. A second-hop producer holding
// only an AccessContext cannot rebuild a faithful assertion: an AccessContext
// carries no credential class and no role ceiling, so the rebuilt one would say
// class="user" with no ceiling for a BADGE session -- and "no badge" being
// indistinguishable from "not stated" is precisely the defect memql#3205
// removed. Re-asserting the received value keeps the ceiling provable at every
// hop instead of only the first.
func ContextWithForwardedAuthority(ctx context.Context, a ForwardedAuthority) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, forwardedAuthorityContextKey, a)
}

// ForwardedAuthorityFromContext returns the assertion this node accepted, and
// ok=false when the work did not arrive over a mesh forward.
//
// A false here is not an error: single-node dispatch and node-local startup work
// legitimately have none. It IS a refusal for anything about to forward -- see
// the workbench producer, which fails closed rather than sending an envelope it
// cannot make an assertion for.
func ForwardedAuthorityFromContext(ctx context.Context) (ForwardedAuthority, bool) {
	if ctx == nil {
		return ForwardedAuthority{}, false
	}
	a, ok := ctx.Value(forwardedAuthorityContextKey).(ForwardedAuthority)
	return a, ok
}

// BindForwardedContext is the ACCEPT-AND-BIND step of the mesh forwarded-auth
// contract: given a VERIFIED assertion and the attribution claims derived from
// it, it produces the context every worker-side handler reads. Both mesh
// receivers go through it -- the AI forward (component/grpc) and the workbench
// forward (integrations/workbench).
//
// Four stamps, four distinct jobs.
//   - client origin: this work originated with a client request, not with a
//     server-initiated sweep (#2889).
//   - forwarded claims: TokenInfo for `createdBy` ATTRIBUTION only. Never an
//     authorization input any more. This is also what carries the
//     provenance-only display name that component/metadata stamps as
//     identity.displayName on every row a mutation writes (memql#3221).
//   - access: THE authorization decision, and it comes from the verified
//     assertion alone -- no LoadFromClaims, no FallbackFromClaims, no
//     userByIdSystem keyed by a wire-supplied subject, and therefore no
//     per-message DB round trip either.
//   - the assertion itself: chain of custody for a SECOND hop out of this node,
//     which is the only thing that reads it. See ContextWithForwardedAuthority.
//
// `access` MUST already have come from VerifyForwardedAuthority; this binds what
// it is handed and verifies nothing itself.
func BindForwardedContext(ctx context.Context, claims map[string]string, access *AccessContext, authority ForwardedAuthority) context.Context {
	ctx = ContextWithClientOrigin(ctx)
	ctx = ContextWithForwardedClaims(ctx, claims)
	ctx = ContextWithAccess(ctx, access)
	return ContextWithForwardedAuthority(ctx, authority)
}
