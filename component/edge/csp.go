// component/edge/csp.go
package edge

import (
	"net/http"

	"github.com/znasllc-io/memql/component/frontdoor"
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
//
// scriptHashes is the `'sha256-...'` source list for the inline scripts of
// the DOCUMENT being served, or "" for every other response -- an asset, a
// 404, or a document with no inline scripts at all. See scripthash.go for
// why inline scripts need naming and why a hash rather than
// 'unsafe-inline'.
//
// An empty list reproduces this function's previous output byte for byte,
// which is what makes this change invisible to every bundle that has no
// inline scripts, MemQL OS included
// (TestPolicyForSiteWithNoHashesIsUnchanged). 'unsafe-inline' is never
// added under any condition: a browser IGNORES it whenever a hash is
// present, so it would be inert here and a silent hole on the overflow
// path, which deliberately falls back to NO hashes rather than to a weaker
// policy.
func policyForSite(r *http.Request, site *Site, env func(string) string, scriptHashes string) string {
	origin := siteOriginOf(site)
	connectSrc := "connect-src 'self' " + origin + " " + wsOriginOf(origin)
	if identity := identityOriginForSite(site, env); identity != "" {
		connectSrc += " " + identity
	}
	scriptSrc := "script-src 'self'"
	if scriptHashes != "" {
		scriptSrc += " " + scriptHashes
	}

	// THE STOREFRONT ARM, AND IT IS ADDITIVE ONLY (memql#5534). storefrontSources
	// returns its zero value for every kind but shopify_storefront, and the zero
	// value appends nothing to any directive -- so the spa and static policies
	// are byte-identical to what this cluster already serves, which is what
	// TestNonStorefrontPoliciesAreUnchangedByTheStorefrontArm pins. Why these
	// hosts, where they come from, and what is deliberately NOT widened are all
	// in csp_storefront.go.
	//
	// MEDIA-SRC APPEARS ONLY FOR A STOREFRONT. Every other kind has no such
	// directive and falls back to default-src 'self', exactly as before; adding
	// an empty one everywhere would change every policy in the cluster to say
	// the same thing in more bytes.
	sf := storefrontSources(site)
	connectSrc = sf.join(connectSrc, sf.connect)
	imgSrc := sf.join("img-src 'self' data: blob:", sf.img)
	mediaSrc := ""
	if len(sf.media) > 0 {
		mediaSrc = sf.join("media-src 'self'", sf.media) + "; "
	}

	return "default-src 'self'; " +
		connectSrc + "; " +
		imgSrc + "; " +
		mediaSrc +
		"style-src 'self' 'unsafe-inline'; " +
		scriptSrc + "; " +
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
	if site == nil {
		return ""
	}
	// THROUGH AN ACCOUNT'S FRONT DOOR THE SITE'S OWN HOSTNAME IS NOT THE
	// ORIGIN (epic memql#5168). The OS resolves through a door as the same
	// `os` site row it always is, so site.Hostname is still
	// `os.<cluster-domain>` -- but the page was served at
	// `app.<reservedName>`, and that is the origin this policy is about.
	//
	// Naming the wrong one is not merely untidy: wsOriginOf composes the
	// WebSocket origin from this value, so a door would advertise
	// `wss://os.<cluster-domain>` while the page opens
	// `wss://app.<reservedName>`. That survives only on the CSP3 reading of
	// 'self' covering same-origin ws:/wss: -- which is exactly the belt this
	// explicit origin exists to be the braces for.
	if site.Account != nil && site.Account.ReservedName != "" {
		host := frontdoor.AccountRoleHost(frontdoor.AccountRoleApp, site.Account.ReservedName)
		if validHost(host) {
			return "https://" + host
		}
		return ""
	}
	if !validHost(site.Hostname) {
		return ""
	}
	return "https://" + site.Hostname
}

// identityOriginForSite is the identity origin this page's own sign-in flow
// will reach: the cluster's, or the door's `id.` host when the page was served
// through an account's reserved front door.
//
// It must agree with RuntimeConfig.IdentityURL for the same site, and
// TestTheCspNamesTheSameIdentityOriginTheRuntimeConfigDoes asserts it does.
// The failure it prevents is the one csp.go's header paragraph already
// describes, one domain over: the top-level /authorize navigation succeeds
// (connect-src does not govern navigation), so sign-in appears to proceed and
// then fails silently at the callback fetch -- with a CSP violation naming an
// origin nobody configured anywhere.
func identityOriginForSite(site *Site, env func(string) string) string {
	if site != nil && site.Account != nil && site.Account.ReservedName != "" {
		return "https://" + frontdoor.AccountRoleHost(frontdoor.AccountRoleID, site.Account.ReservedName)
	}
	return identityOriginFromEnv(env)
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

// permissionsPolicy is the powerful-features policy every hosted site gets.
//
// MICROPHONE IS `self`, AND THE OTHER TWO ARE NOT (memql#4747). The set was
// `geolocation=(), microphone=(), camera=()` -- ported from the identity
// service, where nothing has ever needed any of the three. On the EDGE that
// is a different statement, because the edge serves MemQL OS, whose Ask
// surface takes dictation. `microphone=()` disables getUserMedia for the
// document outright: the promise rejects with NotAllowedError before the
// browser shows a permission prompt at all, which is character-for-character
// what a person DECLINING the microphone produces. So the feature would have
// looked implemented, passed every test, worked in `npm run dev` (vite sends
// no such header) and been dead in every deployed cluster, reporting itself
// as the user's own choice.
//
// The widening is narrow and the browser's own gate is untouched:
//
//   - `self` admits the site's own document and no cross-origin frame. The
//     edge already answers `frame-ancestors 'none'` and X-Frame-Options DENY,
//     so a hosted page is never embedded, and `self` never reaches a third
//     party's code.
//   - The per-origin permission PROMPT still stands. This header decides
//     whether a page may ASK; the person decides whether it may listen.
//   - Every hosted bundle arrives through the authenticated publish route
//     (POST /sites/{id}/bundles, a service-account JWT), so the code this
//     admits is code the cluster's own operators shipped.
//
// Geolocation and camera stay CLOSED, and that asymmetry is the point: a
// capability is opened when a first-party surface needs it, never
// speculatively. Anything reaching for the camera should have to change this
// line and say why, exactly as this change did.
const permissionsPolicy = "geolocation=(), microphone=(self), camera=()"

// securityHeaders writes the baseline hardening set, ported from
// component/portal/csp.go -- see permissionsPolicy for the one deliberate
// divergence.
//
// HSTS on TLS only: browsers cache it per host, so emitting it once over
// http://localhost poisons that host's STS store and forces every later
// localhost request to https, which then fails.
func securityHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Permissions-Policy", permissionsPolicy)
	if r != nil && r.TLS != nil {
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}
