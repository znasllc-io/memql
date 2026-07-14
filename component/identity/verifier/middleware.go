package verifier

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/metrics"
)

// MiddlewareOptions configures HTTPMiddleware.
type MiddlewareOptions struct {
	Logger              *slog.Logger
	PublicPaths         []string
	UnauthorizedHandler func(http.ResponseWriter, *http.Request)
	DelegationResolver  auth.DelegationResolver
}

// HTTPMiddleware wraps an HTTP handler with bearer-token verification.
// Public paths bypass; everything else requires a valid bearer token
// (Authorization header; the bearer Sec-WebSocket-Protocol entry for
// browser WebSocket upgrades, #2511; the memql_auth cookie; or the
// deprecated `?bearer_token=` / `?token=` query params).
func HTTPMiddleware(v *Verifier, opts MiddlewareOptions) func(http.Handler) http.Handler {
	if v == nil {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "verifier not configured", http.StatusInternalServerError)
			})
		}
	}
	logger := opts.Logger
	publicPaths := normalizePublicPathSet(opts.PublicPaths)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if shouldBypassAuth(r, publicPaths) {
				next.ServeHTTP(w, r)
				return
			}
			token, source, err := extractTokenHTTP(r)
			if err != nil {
				logAuthFailure(logger, err, r, source)
				metrics.AuthReject(metrics.SurfaceHTTP, metrics.ReasonMissingToken, metrics.CodeUnauthenticated)
				respondUnauthorized(w, r, opts.UnauthorizedHandler)
				return
			}
			vc, err := v.VerifyBearer(r.Context(), token)
			if err != nil {
				logAuthFailure(logger, err, r, source)
				metrics.AuthReject(metrics.SurfaceHTTP, rejectReason(err), metrics.CodeUnauthenticated)
				respondUnauthorized(w, r, opts.UnauthorizedHandler)
				return
			}
			if logger != nil {
				logger.Debug("http auth: success",
					"subject", vc.UserId,
					"role", vc.Role,
					"source", string(vc.Source),
					"path", r.URL.Path)
			}
			ctx := AttachToContext(r.Context(), vc)
			if opts.DelegationResolver != nil {
				if dc, derr := opts.DelegationResolver.ResolveActiveDelegation(ctx, vc.UserId); derr == nil && dc != nil {
					ctx = auth.ContextWithDelegation(ctx, dc)
					if logger != nil {
						logger.Debug("http auth: delegation attached",
							"subject", vc.UserId,
							"agent_id", dc.AgentId,
							"role_ceiling", dc.RoleCeiling,
							"path", r.URL.Path)
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// authCookieName matches the cookie set by the legacy auth path. The
// identity-issued JWTs ride the same cookie name so existing browser
// sessions continue to work after the cutover.
const authCookieName = "memql_auth"

func extractTokenHTTP(r *http.Request) (string, string, error) {
	if header := r.Header.Get("Authorization"); header != "" {
		tok, err := extractBearer(header)
		if err == nil {
			return tok, "header", nil
		}
	}
	// Browsers can't set headers on the WebSocket upgrade; the SDK carries
	// the bearer JWT as the second Sec-WebSocket-Protocol entry
	// (`new WebSocket(url, ["bearer", token])`, #2511). Beats the cookie:
	// an explicit dial credential wins over an ambient (possibly stale)
	// browser session cookie.
	if scheme, cred := auth.WebSocketSubprotocolCredential(r); scheme == auth.WSCredentialSchemeBearer && cred != "" {
		return cred, "ws-subprotocol", nil
	}
	if c, err := r.Cookie(authCookieName); err == nil && c != nil && strings.TrimSpace(c.Value) != "" {
		return strings.TrimSpace(c.Value), "cookie", nil
	}
	if r.URL != nil {
		// DEPRECATED (#2511): query-param token carry predates the
		// subprotocol channel and leaks the credential into request-line
		// access logs. Kept working until downstream clients migrate
		// (memql-project#16); ?token= is the older legacy alias.
		if v := strings.TrimSpace(r.URL.Query().Get("bearer_token")); v != "" {
			return v, "query", nil
		}
		if v := strings.TrimSpace(r.URL.Query().Get("token")); v != "" {
			return v, "query", nil
		}
	}
	return "", "", errors.New("no authentication token found")
}

func extractBearer(header string) (string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", errors.New("authorization header empty")
	}
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", errors.New("invalid authorization header format")
	}
	tok := strings.TrimSpace(parts[1])
	if tok == "" {
		return "", errors.New("bearer token empty")
	}
	return tok, nil
}

func shouldBypassAuth(r *http.Request, publicPaths map[string]struct{}) bool {
	if r == nil || r.URL == nil {
		return false
	}
	path := normalizePath(r.URL.Path)
	if _, ok := publicPaths[path]; ok {
		return true
	}
	for allowed := range publicPaths {
		if allowed == "" || allowed == "/" || allowed == path {
			continue
		}
		if strings.HasPrefix(path, allowed+"/") {
			return true
		}
	}
	return false
}

func normalizePublicPathSet(paths []string) map[string]struct{} {
	set := make(map[string]struct{}, len(paths)+2)
	for _, p := range paths {
		if n := normalizePath(p); n != "" {
			set[n] = struct{}{}
		}
	}
	set["/health"] = struct{}{}
	set["/healthz"] = struct{}{}
	set["/readyz"] = struct{}{} // schema-assertion readiness probe (#657), unauthenticated
	return set
}

func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if path != "/" {
		path = strings.TrimSuffix(path, "/")
	}
	return path
}

func respondUnauthorized(w http.ResponseWriter, r *http.Request, handler func(http.ResponseWriter, *http.Request)) {
	if handler != nil {
		handler(w, r)
		return
	}
	http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
}

func logAuthFailure(logger *slog.Logger, err error, r *http.Request, tokenSource string) {
	if logger == nil {
		return
	}
	logger.Warn("http auth failed",
		"error", err,
		"path", r.URL.Path,
		"token_source", tokenSource)
}
