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

	// forwardedClaimClass and forwardedClaimRoleCeiling carry a badge
	// grant's authorization context (memql#2513) across the hop.
	//
	// These are NOT identity fields, which is exactly why they went
	// missing: UserIdentity carries Subject/Email/PhoneNumber/Role/
	// FirstName/LastName, so a projection through it drops them. But
	// applyBadgeRoleCeiling -- which LoadFromClaims runs on every DIRECT
	// request -- keys on precisely these two, so without them a receiving
	// node cannot reapply the ceiling and resolves the user's UNCLAMPED
	// stored role instead.
	//
	// Measured on a badge session with stored role `owner` and ceiling
	// `reader` (the analysis recorded on memql#2814):
	//
	//	DIRECT     role="reader" isClusterOwner=false
	//	FORWARDED  role="owner"  isClusterOwner=true
	//
	// The asymmetry is what makes the omission dangerous rather than
	// merely lossy: the PRINCIPAL fails closed -- an unresolvable subject
	// yields no actor -- while the CEILING fails open, and an absent
	// `class` is indistinguishable from "not a badge session", so the
	// receiver cannot detect the loss. See memql#2876.
	forwardedClaimClass       = "class"
	forwardedClaimRoleCeiling = "role_ceiling"
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

// WithForwardedAuthorityContext copies the authorization-context claims out of
// a claim map and into an already-packed forwarded map.
//
// Only an ALLOWLIST crosses. A blind copy of the claim set would put the raw
// token, arbitrary vendor claims and anything a future issuer adds onto a peer
// message; a receiving side needs exactly the inputs to the decisions it must
// reapply, and nothing else.
//
// # NOT WIRED, DELIBERATELY -- read this before calling it
//
// No producer calls this today, and wiring it naively REINTRODUCES the defect
// it exists to help fix. memql#2876 attempted exactly that and was reverted.
//
// The trap is the SOURCE. The obvious one -- the gRPC stream's context -- is
// wrong: a stream context is fixed at stream-open, while badge grants arrive
// MID-STREAM via RotateAuth (component/grpc/server.go:1090 says so outright,
// and handleRotateAuth swaps s.access / s.identity / the badge expiry stamp
// without touching the context, because it cannot). Sourced from there, a
// mid-stream badge session forwards NO class and NO role_ceiling, the receiver
// cannot distinguish that from "not a badge session", and it resolves the
// user's UNCLAMPED stored role. Measured, operator stored role `owner`,
// terminal ceiling `reader`:
//
//	DIRECT     role="reader" isClusterOwner=false
//	FORWARDED  role="owner"  isClusterOwner=true
//
// The reliable source is the SESSION's post-rotate state, not the context.
//
// The deeper point, which is why this is a signpost rather than a fix: two
// OPTIONAL claims whose absence is indistinguishable from "no badge" cannot
// carry a ceiling safely, however they are sourced. memql#2814's analysis names
// that failure mode as undetectable by the receiver. A correct contract makes
// the assertion mandatory and explicit -- "no badge" is a VALUE, not a missing
// key -- carries `exp` so expiry stays enforceable, covers every producer, and
// has the receiver REFUSE when it cannot prove the ceiling was applied.
//
// Kept in the tree because the allowlist discipline is right and reusable, and
// because the next attempt should start from this note rather than rediscover
// it. See memql#2876 and memql#2814.
func WithForwardedAuthorityContext(m map[string]string, claims map[string]any) map[string]string {
	if m == nil {
		m = make(map[string]string, 2)
	}
	for _, key := range []string{forwardedClaimClass, forwardedClaimRoleCeiling} {
		if v := stringClaim(claims, key); v != "" {
			m[key] = v
		}
	}
	return m
}

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
