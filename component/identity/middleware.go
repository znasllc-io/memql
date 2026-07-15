package identity

import (
	"context"
	"net/http"

	"github.com/znasllc-io/memql/component/auth"
)

// ContextWithSystemActor stamps the synthetic system actor onto a
// background context. Useful for non-HTTP entry points (the identity
// service's auto-bootstrap path on startup, cron jobs, etc.) where
// there's no incoming request to carry actor info but downstream
// engine mutations still need an actor for createdBy stamping.
//
// Mirrors what SystemActorMiddleware does for HTTP requests: same
// claims, same SystemActorSubject. Idempotent -- if a real actor is
// already on the context, this leaves it alone.
func ContextWithSystemActor(ctx context.Context) context.Context {
	if auth.TokenInfoFromContext(ctx) != nil {
		return ctx
	}
	claims := map[string]any{
		"sub":   SystemActorSubject,
		"email": "system@identity.memql.local",
		"role":  "owner",
		"name":  "Identity Service",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, token)
	return ctx
}

// SystemCredentialActorSubject is the synthetic actor attributed to
// machine-credential v1:identity:identity writes (badge / worker_token /
// node_token / voice_agent_token / service_account). It carries
// role="system" and a "system:"-prefixed subject so it satisfies the
// memql#2513 credential-actor guard (component/memql
// identity_credential_actor_validation.go's validateIdentityCredentialActorScope
// -> isSystemActor), which admits a machine-credential row write ONLY
// from a system actor.
//
// Distinct from SystemActorSubject on purpose. That actor
// ("system:identity-svc", role="owner", email="system@identity.memql.local")
// stands in for the eventual human owner on the deliberately-
// unauthenticated session-bootstrap surface (magic-link, oauth token
// exchange, first-run clusterSettings). Two properties keep it OUT of
// isSystemActor: role="owner" (not "system") and an email claim that
// ActorFromToken prefers over the subject (so the derived actor is
// "system@identity.memql.local", which lacks the "system:" prefix).
// Credential MINT/REVOKE is a different operation -- the caller is
// authorized at the edge (bootstrap secret / pair code / admin role /
// CLI exec) and the engine write runs as this dedicated system actor.
// memql#2549.
const SystemCredentialActorSubject = "system:identity-credential"

// ContextWithSystemCredentialActor forces the system credential actor
// onto ctx, OVERRIDING any actor already present. Unlike
// ContextWithSystemActor (idempotent -- it leaves an existing actor
// alone), the machine-credential write paths run under contexts that
// already carry a NON-system actor: the SystemActorMiddleware owner
// stamp on the /node/bootstrap + /pair/redeem HTTP surface, or the
// logged-in operator on the /admin/tokens revoke surface. That actor is
// exactly what the memql#2513 guard rejects, so it must be replaced, not
// preserved.
//
// This supplies only the engine-write attribution; it does NOT authorize
// anything. Callers MUST have authorized the operation before invoking
// it (bootstrap secret compare, pair-code redeem, requireAdmin, CLI
// exec). No email claim is stamped so the derived actor is the
// "system:"-prefixed subject rather than an email -- both the role and
// the actor-prefix arms of isSystemActor hold. memql#2549.
func ContextWithSystemCredentialActor(ctx context.Context) context.Context {
	claims := map[string]any{
		"sub":  SystemCredentialActorSubject,
		"role": "system",
		"name": "Identity Credential Service",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, token)
	return ctx
}

// SystemActorSubject is the synthetic actor stamped onto identity-
// side requests that have no other authenticated user. Identity's
// HTTP surface includes a deliberately-unauthenticated entry-point
// set (/setup, /login, POST /auth/magic-link, GET /auth/complete,
// POST /oauth/token, POST /auth/refresh, POST /auth/logout) -- those
// paths exist to BOOTSTRAP a session, so by definition there is no
// caller-supplied auth context. Mutations they trigger
// (clusterSettings on first run, magicLinkRequest credential rows,
// authSession rows) need an actor for createdBy stamping; without
// one the engine rejects with "no actor found in context".
//
// Using "system:identity-svc" instead of a real user keeps the audit
// trail honest: rows attributed to this subject were touched by the
// service-side bootstrap path, not by an authenticated end-user. The
// magic-link verifier later mints the real owner identity, and from
// that point on requests carry that user's TokenInfo through
// requireAdmin's stamping (admin paths) or the access-token bearer
// (API paths).
const SystemActorSubject = "system:identity-svc"

// SystemActorMiddleware stamps a synthetic system actor onto requests
// that arrive without any TokenInfo. Applied at the top of identity-
// side route mounting (web.Server.Mount + http.Server.Mount) so every
// unauthenticated handler downstream sees an actor.
//
// Idempotent: if a higher-level middleware (e.g. admin's requireAdmin
// once it stamps auth.TokenInfo) has already attached an actor, this
// middleware is a no-op. The synthetic actor is ONLY filled in when
// the request would otherwise have nothing.
//
// Scoped to identity binaries via the build-tag-conditional
// integrationsIdentity wiring -- bff/voice/cognition/agent/planner
// never see this middleware. On those binaries, an unauthenticated
// request fails closed at the verifier interceptor; there is no
// "fallback to system" path.
func SystemActorMiddleware(next http.Handler) http.Handler {
	if next == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if existing := auth.TokenInfoFromContext(ctx); existing != nil {
			next.ServeHTTP(w, r)
			return
		}
		claims := map[string]any{
			"sub":   SystemActorSubject,
			"email": "system@identity.memql.local",
			"role":  "owner",
			"name":  "Identity Service",
		}
		token := auth.BuildTokenInfo(claims)
		ctx = auth.ContextWithClaims(ctx, claims)
		ctx = auth.ContextWithToken(ctx, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// SystemActorHandlerFunc is the http.HandlerFunc-flavored adapter
// for SystemActorMiddleware. Many handlers in the identity package
// are wired with HandleFunc rather than Handle, so an HF-compatible
// shim is convenient.
func SystemActorHandlerFunc(next http.HandlerFunc) http.HandlerFunc {
	if next == nil {
		return next
	}
	wrapped := SystemActorMiddleware(next)
	return func(w http.ResponseWriter, r *http.Request) {
		wrapped.ServeHTTP(w, r)
	}
}
