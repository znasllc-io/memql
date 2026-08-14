// component/edge/csp.go
package edge

import (
	"net/http"
	"net/url"
	"strings"
)

// policyForSite builds the CSP from the SITE's own origin, PLUS the
// cluster's one identity origin.
//
// component/portal/csp.go derives connect-src from MEMQL_IDENTITY_BASE_URL,
// which is correct when there is exactly one bundle on one origin. With many
// sites on many origins, one shared policy naming every site's origin
// everywhere would be uselessly permissive; naming only the SITE's own
// origin is what this settled on instead.
//
// THAT ORIGINAL REASONING WAS INCOMPLETE, NOT WRONG (memql#3711 fix round
// 2). It said "the site is same-origin with its own API through /_memql/*,
// so its own origin is the whole of what it needs" -- true for DATA, and
// silent about AUTHENTICATION. A site with signed-in users performs the
// OAuth token exchange (POST /oauth/token, plus /auth/refresh and
// /auth/logout) with fetch() directly against the identity service, which
// is a DIFFERENT origin from the site's own -- and no proxy path to
// identity exists: /_memql/* targets the bff, which does not itself serve
// those paths. Without the identity origin here, that fetch() is refused by
// THIS policy before it ever reaches the network: the top-level redirect to
// /authorize still works (a navigation, which connect-src does not govern),
// so sign-in appears to proceed and then fails silently at the callback --
// exactly the failure mode that makes the omission worth a paragraph
// instead of a one-line fix.
//
// Adding it keeps the policy TIGHT, not loose: identityOriginFromEnv reads
// the same domain-derived env runtimeconfig.go's identityUrl field does
// (identityURLFromEnv), so this is one more named origin, not a wildcard --
// and it is the SAME one for every site on this cluster, because there is
// one identity service per cluster. Every site gets it, not only the
// portal: any hosted SPA with its own sign-in needs precisely this, which
// is why naming it here is data-driven policy, not a branch for one site
// (TestPortalHasNoSpecialCaseInTheServingPath -- no site name appears
// anywhere in this file).
//
// The site's own origin comes from site.Hostname (the Resolver's
// already-validated result), forced to https, rather than from
// httpOriginOf(r). Two independent reasons converge on that, not just one:
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
func policyForSite(r *http.Request, site *Site, env func(string) string) string {
	origin := siteOriginOf(site)
	connectSrc := "connect-src 'self' " + origin + " " + wsOriginOf(origin)
	if identity := identityOriginFromEnv(env); identity != "" {
		connectSrc += " " + identity
	}
	return "default-src 'self'; " +
		connectSrc + "; " +
		"img-src 'self' data: blob:; " +
		"style-src 'self' 'unsafe-inline'; " +
		"script-src 'self'; " +
		"frame-ancestors 'none'; " +
		"base-uri 'self'; " +
		"form-action 'self'"
}

// identityOriginFromEnv resolves the cluster's identity-service origin for
// connect-src, from the SAME env runtimeconfig.go's identityURLFromEnv
// reads for the identityUrl field of the runtime-config document -- one
// function, two callers, so the policy and the document can never disagree
// about which origin the cluster's identity service is at.
//
// Reduced to scheme://host and validated before it can land in a response
// header -- the same posture component/portal/csp.go's originOf held, for
// the identical reason: this value is env-derived configuration, not
// literal code, and a malformed one must DROP the source rather than inject
// something unparseable into the policy.
func identityOriginFromEnv(env func(string) string) string {
	return originOf(identityURLFromEnv(env))
}

// originOf reduces an absolute http(s) URL to scheme://host[:port], or ""
// when it is not one. Ported from component/portal/csp.go unchanged.
func originOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	if !validHost(u.Host) {
		return ""
	}
	return u.Scheme + "://" + u.Host
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
