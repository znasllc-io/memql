package server

import (
	"net/http"
	"strings"
	"time"
)

const (
	// authCookieName matches the cookie name set by the identity
	// service's magic-link / refresh flow and the verifier middleware.
	authCookieName       = "memql_auth"
	defaultCookieMaxAgeS = 3600
)

func readAuthCookie(r *http.Request) string {
	if r == nil {
		return ""
	}
	cookie, err := r.Cookie(authCookieName)
	if err != nil || cookie == nil {
		return ""
	}
	return strings.TrimSpace(cookie.Value)
}

func isSecureRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	return strings.EqualFold(proto, "https")
}

func buildAuthCookie(r *http.Request, token string, ttlSeconds int) *http.Cookie {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return nil
	}

	if ttlSeconds <= 0 {
		ttlSeconds = defaultCookieMaxAgeS
	}

	return &http.Cookie{
		Name:     authCookieName,
		Value:    trimmed,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecureRequest(r),
		MaxAge:   ttlSeconds,
		Expires:  time.Now().Add(time.Duration(ttlSeconds) * time.Second),
	}
}

func buildClearAuthCookie(r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecureRequest(r),
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	}
}
