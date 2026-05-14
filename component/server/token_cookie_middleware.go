package server

import (
	"fmt"
	"net/http"
	"strings"
)

// TokenCookieMiddleware injects the bearer token stored in the MemQL auth cookie
// into the Authorization header so downstream middleware can validate it.
func TokenCookieMiddleware() MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		if next == nil {
			next = http.DefaultServeMux
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r == nil {
				next.ServeHTTP(w, r)
				return
			}

			if hasAuthorizationHeader(r) {
				next.ServeHTTP(w, r)
				return
			}

			token := readAuthCookie(r)
			if token == "" {
				next.ServeHTTP(w, r)
				return
			}

			cloned := r.Clone(r.Context())
			if cloned.Header == nil {
				cloned.Header = make(http.Header)
			}
			cloned.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
			next.ServeHTTP(w, cloned)
		})
	}
}

func hasAuthorizationHeader(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.TrimSpace(r.Header.Get("Authorization")) != ""
}
