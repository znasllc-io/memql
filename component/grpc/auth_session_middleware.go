// Package memql session-revocation middleware.
//
// Two enforcement layers wrap the identity-verifier auth path so a
// revoked v1:identity:authSession row blocks the bearer token
// everywhere it might appear:
//
//   - NewSessionRevocationHTTPMiddleware: wraps the HTTP handler
//     chain. Runs after the verifier's HTTPMiddleware -- by the time
//     we execute, the JWT has already been signature-verified and the
//     claims are attached. We hash the bearer token (Authorization
//     header, memql_auth cookie, or `?token=` query param for
//     WebSocket upgrades) and reject the request if the matching
//     session row is revoked.
//
//   - NewSessionRevocationStreamInterceptor: wraps the gRPC stream
//     chain similarly, after the base verifier / no-auth interceptor
//     has set claims and before the guest-aware wrapper.
//
// The HTTP middleware also runs on the WebSocket upgrade -- the
// revocation check happens at connect time. Already-established
// streams keep their socket open until the JWT naturally expires or
// the client tears the connection down. That's an intentional
// tradeoff: per-message stream revocation would require a hot DB
// hit on every envelope.

package memql

import (
	"log/slog"
	"net/http"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// authCookieName mirrors the cookie name shared between the identity
// service's web flow and the per-node verifier middleware. Duplicated
// here as a literal string so this file stays decoupled from the
// verifier package.
const authCookieName = "memql_auth"

// NewSessionRevocationHTTPMiddleware enforces session revocation on
// HTTP + WebSocket-upgrade requests. Wrap AFTER the verifier auth
// middleware so the request only reaches us when the JWT has
// already been validated.
//
// Bypasses requests with no extractable token (public paths the
// verifier middleware let through, dev/no-auth flows, etc.) --
// failing closed there would break paths that legitimately have no
// session row.
func NewSessionRevocationHTTPMiddleware(engine *memqlengine.MemQLEngine, logger *slog.Logger) func(http.Handler) http.Handler {
	if engine == nil {
		// Fail-open in dev / cluster bootstrap before the engine is
		// ready: no enforcement, but log loudly so the operator
		// notices it isn't installed.
		if logger != nil {
			logger.Warn("session revocation middleware: engine not configured, all bearer tokens will be accepted")
		}
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractBearerTokenHTTP(r)
			if token == "" {
				next.ServeHTTP(w, r)
				return
			}
			session, err := LookupAuthSessionByTokenHash(r.Context(), engine, HashBearerToken(token))
			if err != nil {
				if logger != nil {
					logger.Warn("session revocation: lookup failed -- failing open",
						"error", err, "path", r.URL.Path)
				}
				next.ServeHTTP(w, r)
				return
			}
			if session != nil && !session.RevokedAt.IsZero() {
				if logger != nil {
					logger.Info("session revocation: token blocked",
						"session_id", session.ID,
						"reason", session.RevokedReason,
						"path", r.URL.Path)
				}
				http.Error(w, "session revoked", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// NewSessionRevocationStreamInterceptor enforces session revocation
// at gRPC stream-open time. Wrap AFTER the base verifier / no-auth
// interceptor (so claims are set), BEFORE the guest-aware wrapper
// (guest streams skip this check entirely -- they authenticate via
// invitation token, not bearer token).
func NewSessionRevocationStreamInterceptor(base grpc.StreamServerInterceptor, engine *memqlengine.MemQLEngine, logger *slog.Logger) grpc.StreamServerInterceptor {
	if base == nil {
		return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			return status.Error(codes.Internal, "auth not configured")
		}
	}
	if engine == nil {
		if logger != nil {
			logger.Warn("session revocation stream interceptor: engine not configured, all bearer tokens will be accepted")
		}
		return base
	}
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		// Run base auth first; it sets claims and rejects bad tokens.
		// We layer revocation enforcement on top by inspecting the
		// metadata on the original stream context (claims propagate
		// onto a wrapped context that base hands to handler -- but
		// we need the bearer token, which lives on the metadata of
		// the original ss.Context()).
		token := bearerTokenFromIncomingContext(ss.Context())
		if token == "" {
			// No bearer token visible (guest stream, no-auth dev,
			// some internal call). Defer to base.
			return base(srv, ss, info, handler)
		}
		session, err := LookupAuthSessionByTokenHash(ss.Context(), engine, HashBearerToken(token))
		if err != nil {
			if logger != nil {
				logger.Warn("session revocation: stream lookup failed -- failing open",
					"error", err)
			}
			return base(srv, ss, info, handler)
		}
		if session != nil && !session.RevokedAt.IsZero() {
			if logger != nil {
				logger.Info("session revocation: stream rejected",
					"session_id", session.ID,
					"reason", session.RevokedReason)
			}
			return status.Error(codes.Unauthenticated, "session revoked")
		}
		return base(srv, ss, info, handler)
	}
}

// extractBearerTokenHTTP walks the same fallback chain as the
// verifier HTTP middleware: Authorization header, then the bearer
// Sec-WebSocket-Protocol entry (browser WS upgrades, #2511), then
// auth cookie, then the deprecated `?bearer_token=` / `?token=`
// query parameters. The order MUST match the verifier's so the
// revocation check hashes the same token the verifier validated.
func extractBearerTokenHTTP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		parts := strings.Fields(header)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
	}
	if scheme, cred := auth.WebSocketSubprotocolCredential(r); scheme == auth.WSCredentialSchemeBearer && cred != "" {
		return cred
	}
	if cookie, err := r.Cookie(authCookieName); err == nil && cookie != nil {
		if v := strings.TrimSpace(cookie.Value); v != "" {
			return v
		}
	}
	if r.URL != nil {
		// DEPRECATED (#2511): kept until downstream clients migrate to
		// the subprotocol carry (memql-project#16).
		if v := strings.TrimSpace(r.URL.Query().Get("bearer_token")); v != "" {
			return v
		}
		if v := strings.TrimSpace(r.URL.Query().Get("token")); v != "" {
			return v
		}
	}
	return ""
}
