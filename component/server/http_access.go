package server

import (
	"net/http"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity/verifier"
)

// http_access.go -- resolving the HTTP caller's AccessContext from verified
// claims, per handler family (memql#4843).
//
// # Why this exists
//
// The HTTP middleware chain attaches CLAIMS + TokenInfo and NOT an
// AccessContext: verifier.AttachToContext calls ContextWithClaims +
// ContextWithToken, and no HTTP middleware anywhere calls ContextWithAccess
// (callerIsOwnerOrAdmin in server.go records the same invariant). The gRPC
// side resolves that half separately -- streamSession.ensureAccess turns
// claims into an AccessContext when the stream opens -- so every engine
// surface that binds actor.userId from auth.AccessFromContext works on a
// stream and silently resolves to NOBODY over HTTP: the actor envelope treats
// a nil AccessContext as DENYING, so owned-tier reads answer empty and
// ownership gates refuse. That is how every Library HTTP route refused every
// caller, cluster owner included.
//
// # Why it is NOT a global HTTP middleware
//
// callerIsOwnerOrAdmin's "deliberately NOT put on ctx" note (server.go) is
// the reason: contextWithSystemActor (component/automations/executor.go)
// replaces claims + TokenInfo but does NOT clear an AccessContext, and
// ResumeFrom binds the actor envelope from auth.AccessFromContext -- so an
// AccessContext attached globally would leak the resuming operator's actor
// into the automation replay paths memql#2888 / #2890 hardened. Resolution is
// adopted per handler family instead, at the entry of handlers whose
// downstream reads and writes are meant to run under the CALLER's actor.
//
// # Why FallbackFromClaims, not LoadFromClaims
//
// No DB round-trip per request -- a chunked upload PUTs its file one bounded
// chunk at a time -- and the access token is short-lived, so the signed
// `role` claim is fresh enough for the tier checks these routes make. The
// same trade callerIsOwnerOrAdmin documents for the resume gate.

// requestWithResolvedAccess returns the request carrying an AccessContext
// resolved from the verified claims the HTTP auth middleware attached.
//
//   - An AccessContext already present (a server-side on-behalf-of context,
//     a test's ContextWithUserActor) wins untouched.
//   - No claims attaches NOTHING: a tokenless request keeps failing the
//     handlers' own gates exactly as before. FallbackFromClaims(nil) would
//     fabricate a blank-UserId reader envelope; the absence of claims is the
//     absence of a caller, and it stays one.
//   - A MACHINE-SUBJECT credential class attaches nothing either, and this
//     is a surface pin, not bookkeeping. service_account is read/query-pinned
//     and voice_agent is pinned to its gRPC message set -- their pins live in
//     the gRPC interceptors, which HTTP never runs. Resolving an actor for
//     them here would hand a machine credential a byte-storing write surface
//     its pin denies everywhere else (it 401'd before this file existed, by
//     accident; it keeps 401ing now, on purpose). Absent class means user,
//     the verifier's own backward-compat rule.
//
//   - app_session IS ADMITTED, and it is the one machine class that is
//     (memql#4857). The rule above is really about SUBJECTS rather than
//     classes: what makes resolving an actor for a service account wrong is
//     that its `sub` names a binary, so the resolution would invent a person
//     to own the bytes. An app-session credential's `sub` is a real user's
//     id -- that is the entire security story of the delegated-app
//     back-channel, which says row authz applies to the app exactly as it
//     applies to that person's browser. Refusing it made the cockpit's
//     Library pull and push 401 against the user's own rows.
//
//     It reaches no further than that person does. FallbackFromClaims reads
//     no role claim off this token (the mint stamps none), so the actor is
//     `reader` plus their real user id: enough for the byte routes, which
//     gate on the actor resolving to a USER and never on a role, and short
//     of every admin gate. The class stays read/query-pinned on gRPC, and
//     the site-bundle publish route still names service_account exactly, so
//     an app session cannot publish a site.
//
//     This was only expressible once app_session stopped being minted AS
//     service_account. One name for two subjects is why the rule could not
//     be stated for one and not the other.
// httpResolvableClass reports whether a verified credential class names a
// SUBJECT an HTTP actor may be resolved from. Closed on purpose: a class
// nobody has adjudicated resolves to nothing and the handler's own gate
// refuses it, which is the direction this file fails in.
func httpResolvableClass(class string) bool {
	switch class {
	case "", "user", "badge", verifier.ClassAppSession:
		return true
	default:
		return false
	}
}

func requestWithResolvedAccess(r *http.Request) *http.Request {
	ctx := r.Context()
	if _, ok := auth.AccessFromContext(ctx); ok {
		return r
	}
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return r
	}
	// badge (memql#2513) stays admitted: a badge session is a HUMAN at a
	// shared terminal whose actor FallbackFromClaims resolves with its role
	// ceiling applied -- refusing it here would 401 a badged operator's every
	// Library and attachment call while their gRPC surface works.
	if class, isString := claims["class"].(string); isString && !httpResolvableClass(class) {
		return r
	}
	return r.WithContext(auth.ContextWithAccess(ctx, auth.FallbackFromClaims(claims)))
}
