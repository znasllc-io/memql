// component/edge/csp.go
package edge

import (
	"net/http"
	"strings"
)

// policyForSite builds the CSP from the SITE's own origin.
//
// component/portal/csp.go derives connect-src from MEMQL_IDENTITY_BASE_URL,
// which is correct when there is exactly one bundle on one origin. With many
// sites on many origins, one shared policy is either uselessly permissive
// (every origin allowed everywhere) or wrong for most of them. The site is
// same-origin with its own API through /_memql/* (D9), so its own origin is
// the whole of what it needs.
//
// The origin comes from site.Hostname (the Resolver's already-validated
// result), forced to https, rather than from httpOriginOf(r). Two
// independent reasons converge on that, not just one:
//
//  1. site.Hostname is server-resolved; r.Host is the raw, client-supplied
//     Host header. A response header should name the value the server
//     trusts, not echo the client's own input back at it.
//  2. httpOriginOf's r.TLS check is right for the portal (whose own HTTP
//     server can terminate TLS directly, including a plain-http local-dev
//     port-forward) but wrong here: the edge sits behind the front door's
//     TLS-terminating ingress in every environment
//     (docs/public/operate/environment-parity.md), so r.TLS is nil on this
//     process even for a genuine https visitor. Keying the scheme off it
//     would silently downgrade every hosted site's policy to http://.
func policyForSite(r *http.Request, site *Site) string {
	origin := siteOriginOf(site)
	return "default-src 'self'; " +
		"connect-src 'self' " + origin + " " + wsOriginOf(origin) + "; " +
		"img-src 'self' data: blob:; " +
		"style-src 'self' 'unsafe-inline'; " +
		"script-src 'self'; " +
		"frame-ancestors 'none'; " +
		"base-uri 'self'; " +
		"form-action 'self'"
}

// siteOriginOf is the site's own https origin, or "" for a nil site or an
// unusable hostname. Always https -- see policyForSite's second reason.
func siteOriginOf(site *Site) string {
	if site == nil || !validHost(site.Hostname) {
		return ""
	}
	return "https://" + site.Hostname
}

// httpOriginOf is the request's own origin in http(s) form, ported unchanged
// from component/portal/csp.go -- it is what the portal's single, directly-
// TLS-terminating deployment needs, and a reverse proxy forwarding a request
// (Task 7's /_memql/* proxy) needs the same extraction. policyForSite does
// NOT use it for the per-site origin; see the r.TLS reasoning on
// policyForSite above.
func httpOriginOf(r *http.Request) string {
	if r == nil || r.Host == "" || !validHost(r.Host) {
		return ""
	}
	if r.TLS != nil {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

// wsOriginOf rewrites an http(s) origin to its ws(s) equivalent, or "" when
// origin is "" or not an http(s) origin.
//
// The reasoning carries over from component/portal/csp.go's webSocketOrigin
// unchanged: CSP3 says connect-src 'self' matches ws:/wss: on the same
// origin and Chrome/Firefox implement that, but Safari's support has been
// inconsistent for long enough that the same-origin WebSocket origin is
// named EXPLICITLY. What changed is the SHAPE, not the reasoning: the portal
// version re-derives scheme+host from an *http.Request (and re-validates the
// host) on its own; this one rewrites the scheme of an origin its caller
// already built and validated (siteOriginOf, or httpOriginOf), so the same
// value is never validated twice.
func wsOriginOf(origin string) string {
	switch {
	case strings.HasPrefix(origin, "https://"):
		return "wss://" + strings.TrimPrefix(origin, "https://")
	case strings.HasPrefix(origin, "http://"):
		return "ws://" + strings.TrimPrefix(origin, "http://")
	default:
		return ""
	}
}

// validHost admits the character set of a host[:port], including a bracketed
// IPv6 literal. Deliberately a whitelist: a blacklist of "\r\n" would miss the
// next character that turns out to matter. Ported unchanged from
// component/portal/csp.go.
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

// securityHeaders writes the baseline hardening set, ported unchanged from
// component/portal/csp.go.
//
// HSTS on TLS only: browsers cache it per host, so emitting it once over
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
