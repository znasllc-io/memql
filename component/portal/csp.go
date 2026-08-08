package portal

import (
	"net/http"
	"strings"
)

// The portal's Content-Security-Policy and hardening headers.
//
// Modelled on component/identity/web/{csp,security_headers}.go -- same shape,
// same reasoning about TLS-only directives -- with one difference the portal
// forces and one it inherits.
//
// FORCED: connect-src. The portal's whole job is to hold a WebSocket open to
// /memql/ws on its own origin. CSP3 says 'self' matches ws:/wss: on the same
// origin, and Chrome and Firefox implement that; Safari's support has been
// inconsistent for long enough that relying on it means "the portal does not
// connect on Safari", discovered by a user. So the same-origin WebSocket
// origin is named EXPLICITLY alongside 'self'.
//
// INHERITED: style-src 'unsafe-inline'. view-kit ships its stylesheet as a
// string export rather than a .css file (sdk/ts-viewkit/src/styles.ts explains
// why: the package has no DOM and no bundler assumptions), so the host installs
// it as a runtime <style> element, which CSP counts as an inline style. The
// alternative -- a nonce -- would require the Go handler to rewrite index.html
// per request, which defeats the static-file serving this package exists to do.
// script-src stays strict: there is no inline script in the portal at all,
// which is the half of 'unsafe-inline' that actually carries risk.

const cspStatic = "default-src 'self'; " +
	"img-src 'self' data:; " +
	"style-src 'self' 'unsafe-inline'; " +
	"script-src 'self'; " +
	"font-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// upgradeDirective is appended on TLS responses only. On a plain-HTTP origin
// (a raw port-forward, a `vite preview`) it would force every subresource
// fetch to https, fail, and render the page unstyled -- the exact breakage
// component/identity/web/csp.go documents.
const upgradeDirective = "; upgrade-insecure-requests"

// policyFor builds the policy for one request. It is per-request rather than
// a constant because connect-src names the request's own host.
func policyFor(r *http.Request) string {
	var b strings.Builder
	b.WriteString(cspStatic)
	b.WriteString("; connect-src 'self'")
	if origin := webSocketOrigin(r); origin != "" {
		b.WriteString(" ")
		b.WriteString(origin)
	}
	if r != nil && r.TLS != nil {
		b.WriteString(upgradeDirective)
	}
	return b.String()
}

// webSocketOrigin renders the same-origin WebSocket origin (ws:// or wss://
// plus the request's host), or "" when the host is unusable.
//
// The host is VALIDATED before it reaches a response header. r.Host is
// attacker-controlled -- it is whatever the client sent, or whatever a proxy
// rewrote it to -- and interpolating it unchecked is how a CR/LF lands in a
// header, or how a stray space turns one CSP source into two. Only the
// characters a host:port can legitimately contain are admitted; anything else
// drops the source and leaves the policy at 'self', which fails CLOSED (the
// WebSocket may be blocked) rather than open.
func webSocketOrigin(r *http.Request) string {
	if r == nil || r.Host == "" || !validHost(r.Host) {
		return ""
	}
	if r.TLS != nil {
		return "wss://" + r.Host
	}
	return "ws://" + r.Host
}

// validHost admits the character set of a host[:port], including a bracketed
// IPv6 literal. Deliberately a whitelist: a blacklist of "\r\n" would miss the
// next character that turns out to matter.
func validHost(host string) bool {
	if len(host) > 253+6 { // hostname max + ":65535"
		return false
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == ':', c == '[', c == ']':
		default:
			return false
		}
	}
	return true
}

// securityHeaders writes the baseline hardening set.
//
// HSTS on TLS only, for the reason component/identity/web/security_headers.go
// records: browsers cache it per host, so emitting it once over
// http://localhost poisons that host's STS store and forces every later
// localhost request to https, which then fails.
func securityHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
	if r != nil && r.TLS != nil {
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}
