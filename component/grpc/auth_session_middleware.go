// Package memql session-revocation middleware.
//
// Two enforcement layers wrap the identity-verifier auth path so a
// revoked v1:identity:authSession row blocks the bearer token
// everywhere it might appear:
//
//   - NewSessionRevocationHTTPMiddleware: runs inside the verifier's
//     HTTPMiddleware (app.identityHTTPMiddleware builds the two as
//     one). For a request whose JWT has been signature-verified and
//     whose claims are attached, we hash the bearer token (Authorization
//     header, WebSocket subprotocol, memql_auth cookie, or `?token=`
//     query param) and reject the request if the matching session row
//     is revoked.
//
//   - NewSessionRevocationStreamInterceptor: does the same at stream
//     open, inside the base verifier that it hands the stream to.
//
// # A BEARER NOBODY VERIFIED COSTS NOTHING
//
// Both layers read a session only for a bearer the verifier accepted.
// The read is a database statement, and it used to run for any bearer
// a request carried. The verifier steps aside on a public or
// self-authenticated path, so `Authorization: Bearer x` on /healthz
// cost one read per request with no credential at all, and a `?token=`
// on an unsubscribe or email link counted as a bearer too. Before
// memory_nodes_auth_session_hash_lookup_idx that read was 1.1 s at
// 120,000 sessions. The index makes it cheap; waiting makes it
// unreachable by a stranger.
//
// A path the verifier skips is therefore not checked at all, a revoked
// bearer included. That gives nothing away: no verifier read that
// bearer, the handler behind it gets no claims from it, and the path
// answers a caller with no credential the same way. A later hop that
// does act on a credential verifies it for itself, and the stream
// interceptor below is on that hop.
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
// HTTP + WebSocket-upgrade requests. It MUST run inside the verifier
// auth middleware: it acts only on a request carrying the claims the
// verifier attached, so in front of the verifier it would enforce
// nothing and say nothing. app.identityHTTPMiddleware builds the two
// as one so the order cannot drift.
//
// Bypasses requests with no verified claims (public and
// self-authenticated paths the verifier stepped aside for), and
// verified requests with no extractable token -- failing closed there
// would break paths that legitimately have no session row.
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
			if _, verified := auth.ClaimsFromContext(r.Context()); !verified {
				next.ServeHTTP(w, r)
				return
			}
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
// at gRPC stream-open time. It hands the stream to base first and
// reads a session only from the handler base calls, so a stream base
// refuses costs no read.
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
		// Base runs first; it sets claims and rejects bad tokens, and
		// only a stream it accepted reaches this handler. The read
		// stays on the original ss.Context(), where the bearer token's
		// metadata lives: the context base hands on can carry a
		// delegation's narrowed authority, and a read refused under it
		// would fail open below.
		return base(srv, ss, info, func(srv interface{}, verified grpc.ServerStream) error {
			token := bearerTokenFromIncomingContext(ss.Context())
			if token == "" {
				// No bearer token visible (guest stream, no-auth dev,
				// some internal call). Nothing to look up.
				return handler(srv, verified)
			}
			session, err := LookupAuthSessionByTokenHash(ss.Context(), engine, HashBearerToken(token))
			if err != nil {
				if logger != nil {
					logger.Warn("session revocation: stream lookup failed -- failing open",
						"error", err)
				}
				return handler(srv, verified)
			}
			if session != nil && !session.RevokedAt.IsZero() {
				if logger != nil {
					logger.Info("session revocation: stream rejected",
						"session_id", session.ID,
						"reason", session.RevokedReason)
				}
				return status.Error(codes.Unauthenticated, "session revoked")
			}
			return handler(srv, verified)
		})
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
