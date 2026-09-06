package memql

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/grpc/metadata"

	"github.com/znasllc-io/memql/component/auth"
)

// Small helpers the stream surface shares.
//
// They were spread across the voice-agent interceptor, the Polyphon handlers
// and the guest interceptor -- the three files that happened to need one
// first. All three went with the conversational product (epic memql#4988) and
// none of these had anything to do with it.

// payloadTypeName returns a short log-friendly name for a oneof payload type.
// Used only on rejection paths, so the reflection cost is not on the happy
// path.
func payloadTypeName(payload any) string {
	if payload == nil {
		return "<nil>"
	}
	full := fmt.Sprintf("%T", payload)
	return strings.TrimPrefix(full, "*memqlv1.MemqlClientMessage_")
}

// schemeAndTokenFromMetadata splits an inbound `authorization` header into its
// scheme and token. Returns two empty strings for anything that is not exactly
// two whitespace-separated fields, so a malformed header is indistinguishable
// from an absent one at every call site.
func schemeAndTokenFromMetadata(ctx context.Context) (scheme, token string) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", ""
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		values = md.Get("Authorization")
	}
	if len(values) == 0 {
		return "", ""
	}
	parts := strings.Fields(strings.TrimSpace(values[0]))
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

// systemActorSubject is the subject stamped by contextWithSystemActor.
//
// It read "polyphon-bridge-agent" until the voice pipeline was removed, which
// meant every badge, worker-token and auth-session row this helper wrote was
// attributed to a subsystem that had nothing to do with it. The name is now
// what it always described: the gRPC surface acting on its own behalf.
const systemActorSubject = "grpc-system-actor"

// contextWithSystemActor replaces the context's claims and token with a
// system-actor identity, for the handful of handlers that must read or write
// rows the CALLER may not reach -- minting a badge, rotating a worker token,
// listing a user's own sessions.
//
// It replaces claims and TokenInfo only, and deliberately leaves the
// AccessContext alone: a handler that also needs the caller's own identity
// still has it.
func contextWithSystemActor(ctx context.Context) context.Context {
	claims := map[string]any{
		"sub":   systemActorSubject,
		"email": systemActorSubject + "@memql.internal",
		"role":  "system",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}

// hasReservedTokenPrefix reports whether a bearer carries one of the
// non-JWT credential prefixes, so an interceptor can short-circuit rather
// than feed it to the JWT parser -- which would answer with a parse error
// saying nothing about what was actually presented.
//
// RESERVATION IS NOT ADMISSION. mql_acct_ (memql#3322) is listed so an
// account token presented as a Bearer is recognised as not-a-JWT. NOTHING in
// the tree resolves an mql_acct_ bearer into an identity: no verifier branch,
// no interceptor, and no by-keyHash query. That absence is the design
// (docs/public/operate/auth/account-tokens.md) and is asserted by
// component/identity/accounttoken's own tests.
func hasReservedTokenPrefix(token string) bool {
	for _, p := range []string{"mql_pat_", "mql_wkr_", "mql_acct_"} {
		if strings.HasPrefix(token, p) {
			return true
		}
	}
	return false
}
