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
// The wire uses a proto map<string, string> (see SIForwardRequest.auth in
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
