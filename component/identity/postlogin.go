package identity

import (
	"net/http"
	"net/url"
	"strings"
)

// Post-login return-to, for pages that need the visitor signed in
// before they can do anything (memql#3410).
//
// The identity web surface already has a return_to convention, but it
// means something else: return_to names a RELYING PARTY, is matched
// against a registered client's redirect URIs, and drives the OAuth
// code bounce. A visitor who lands on an identity-owned page while
// signed out is a different case -- there is no client to match, so
// pickOAuthCtx classifies the sign-in as an admin session and
// /auth/complete sends them to /admin/, which is not where they were
// going.
//
// This is the narrow fix for that: the page stashes its OWN path in a
// short-lived cookie before bouncing to /login, and /auth/complete
// consumes the cookie on the admin-session branch instead of hardcoding
// /admin/. It is deliberately not a query parameter -- a parameter
// would have to survive /login -> magic link -> email client ->
// /auth/complete, and the magic-link row has no slot for a
// non-relying-party destination.
//
// The value is SAME-ORIGIN ONLY and enforced as such at both ends. An
// open redirect off the identity origin is worth more to an attacker
// here than almost anywhere else in the tree, because the page it
// bounces from is one the user just authenticated on.

// PostLoginCookieName carries the intended destination across the
// magic-link round trip. Mirrors the memql_auth / memql_admin /
// memql_csrf naming.
const PostLoginCookieName = "memql_post_login"

// postLoginCookieMaxAge bounds the window. A magic link is a
// ten-minute credential and the email hop is the slow part; fifteen
// minutes covers it without leaving a stale destination attached to
// the browser for the rest of the day.
const postLoginCookieMaxAge = 15 * 60

// SetPostLoginRedirect stashes dest for /auth/complete to pick up.
// A dest that is not a safe same-origin path is dropped rather than
// stored, so a caller cannot accidentally launder an attacker-supplied
// value through this helper.
func SetPostLoginRedirect(w http.ResponseWriter, dest string, secure bool) {
	safe := SafeRelativeRedirect(dest)
	if safe == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     PostLoginCookieName,
		Value:    safe,
		Path:     "/",
		MaxAge:   postLoginCookieMaxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// TakePostLoginRedirect returns the stashed destination and clears the
// cookie. Returns "" when nothing was stashed or when what was stashed
// no longer validates -- re-checking on read rather than trusting the
// write is the point: the cookie is client-held and a client can
// rewrite it.
func TakePostLoginRedirect(w http.ResponseWriter, r *http.Request) string {
	if r == nil {
		return ""
	}
	c, err := r.Cookie(PostLoginCookieName)
	if err != nil || c == nil {
		return ""
	}
	clearPostLoginCookie(w, r)
	return SafeRelativeRedirect(c.Value)
}

func clearPostLoginCookie(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		return
	}
	secure := false
	if r != nil && r.TLS != nil {
		secure = true
	}
	http.SetCookie(w, &http.Cookie{
		Name:     PostLoginCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// SafeRelativeRedirect returns raw when it is a same-origin path we are
// willing to bounce a freshly-authenticated browser to, and "" for
// everything else.
//
// The rules, and what each one is actually stopping:
//
//   - Must begin with a single "/". A value like "https://evil.test"
//     or "//evil.test" is an absolute destination wearing a relative
//     costume; the second is the classic protocol-relative bypass of a
//     naive `strings.HasPrefix(dest, "/")` check.
//   - Must parse as a URL with no scheme and no host. Belt to the
//     prefix check's braces, and it also rejects the "/\evil.test"
//     form some browsers normalize into a host.
//   - Must contain no control characters. A CR or LF in a Location
//     header is response splitting.
//
// The query string is preserved -- it is how a verification page
// carries the code the user is mid-way through approving.
func SafeRelativeRedirect(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.ContainsAny(raw, "\r\n\x00") {
		return ""
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return ""
	}
	return raw
}
