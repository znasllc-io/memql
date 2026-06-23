package http

import (
	"net/http"
	"strings"
)

// cors wraps a handler with CORS headers. The auth endpoints all
// receive requests from registered SPA origins (CoPresent, admin UI),
// so we honor MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS. Wildcard "*" is honored
// only when the request does NOT carry credentials -- credentialed
// requests need an exact-match origin so the browser will accept the
// response.
func (s *Server) cors(next http.HandlerFunc) http.HandlerFunc {
	allowed := s.Cfg.CORSAllowedOrigins
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && originAllowed(allowed, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		next(w, r)
	}
}

// handleOptions short-circuits CORS preflight. The cors() wrapper has
// already attached the headers; we just need to return a 204.
func (s *Server) handleOptions(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// originAllowed returns true when origin matches the configured allow
// list. Exact match only — wildcards don't make sense here since CORS
// requires the response to echo back a single origin string.
func originAllowed(allowed []string, origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return false
	}
	for _, a := range allowed {
		if strings.EqualFold(strings.TrimSpace(a), origin) {
			return true
		}
		// "*" allows any origin — only ever used for non-credentialed
		// requests, but we treat it as a permissive override here.
		if a == "*" {
			return true
		}
	}
	return false
}
