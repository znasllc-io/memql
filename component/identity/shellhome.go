package identity

import (
	"net/url"
	"strings"
)

// shellhome.go -- memql#4144, repointed at MemQL OS by epic memql#4984.
//
// #3324 retired the server-rendered /admin/* console and the post-setup and
// authenticated-/login happy paths still sent the browser there (410). The
// live console was then the portal site on the cluster domain; memql#4984
// retired the portal in turn, and the console is now the MemQL OS shell at
// os.<clusterDomain>.
//
// This file was portal.go and the function was PortalHomeURL. Renaming both
// rather than leaving the old names pointing at a new host is the pre-release
// no-shims rule: a helper called PortalHomeURL that returns an os.<d> URL is a
// name that lies to the next reader, and the compiler is the only thing that
// can find every caller.

// ShellHomeURL is where a first-party sign-in should land when there is no
// relying-party callback and no post-login cookie.
//
// clusterDomain wins (the /setup wizard value). Otherwise identity. is
// rewritten to os. on the identity BaseURL host. Empty means the caller cannot
// name the shell origin and must not fall back to /admin/.
func ShellHomeURL(clusterDomain, identityBaseURL string) string {
	if d := strings.TrimSpace(clusterDomain); d != "" {
		d = strings.TrimPrefix(d, ".")
		return "https://os." + d + "/"
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
	u.Host = "os." + strings.TrimPrefix(host, prefix)
	u.Path = "/"
	u.RawQuery = ""
	u.Fragment = ""
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	return u.String()
}

// DefaultPostLoginLanding is the happy-path dest after setup or a bare /login
// revisit. Never /admin/. /me is the same-origin fallback when the shell origin
// cannot be named.
func DefaultPostLoginLanding(clusterDomain, identityBaseURL string) string {
	if u := ShellHomeURL(clusterDomain, identityBaseURL); u != "" {
		return u
	}
	return "/me"
}
