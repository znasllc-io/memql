package identity

import (
	"net/url"
	"strings"
)

// portal.go -- memql#4144.
//
// #3324 retired the server-rendered /admin/* console. The post-setup
// and authenticated-/login happy paths still sent the browser there
// (410). The live console is the portal site on the cluster domain
// (portal.<clusterDomain>), not /admin/ and not identity's /portal/.

// PortalHomeURL is where a first-party sign-in should land when there
// is no relying-party callback and no post-login cookie.
//
// clusterDomain wins (the /setup wizard value). Otherwise identity.
// is rewritten to portal. on the identity BaseURL host. Empty means
// the caller cannot name the portal origin and must not fall back to
// /admin/.
func PortalHomeURL(clusterDomain, identityBaseURL string) string {
	if d := strings.TrimSpace(clusterDomain); d != "" {
		d = strings.TrimPrefix(d, ".")
		return "https://portal." + d + "/"
	}
	raw := strings.TrimSpace(identityBaseURL)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Host
	const prefix = "identity."
	if !strings.HasPrefix(host, prefix) {
		return ""
	}
	u.Host = "portal." + strings.TrimPrefix(host, prefix)
	u.Path = "/"
	u.RawQuery = ""
	u.Fragment = ""
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	return u.String()
}

// DefaultPostLoginLanding is the happy-path dest after setup or a
// bare /login revisit. Never /admin/. /me is same-origin fallback
// when the portal origin cannot be named.
func DefaultPostLoginLanding(clusterDomain, identityBaseURL string) string {
	if u := PortalHomeURL(clusterDomain, identityBaseURL); u != "" {
		return u
	}
	return "/me"
}
