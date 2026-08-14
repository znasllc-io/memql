// component/edge/runtimeconfig.go
package edge

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/config"
)

// runtimeConfigPath is the well-known, root-relative path every hosted site
// answers with its own RuntimeConfig -- GET /runtime-config.json, dispatched
// in ServeHTTP ahead of the bundle lookup. Not a new entry in
// component/server.EdgePaths(): the edge's declared surface is exactly "/",
// and this path lives under it -- see that function's own doc comment.
const runtimeConfigPath = "/runtime-config.json"

// RuntimeConfig is what a browser needs before it can authenticate against
// this cluster -- for WHICHEVER site served the page. The identity service
// is one per cluster, so nothing here is derived from knowing which site is
// asking EXCEPT OAuthClientID, which is looked up from the site's own
// hostname against data the cluster already derived (clientIDForHostname).
// A site with no registered OAuth client still gets
// IdentityURL/IdentityAPIBaseURL/AuthEnabled; only OAuthClientID comes back
// empty, which is the honest answer for "this site has no client to
// present" rather than a fabricated one.
//
// Additive-only shape: an older cached bundle must keep working against a
// newer node, so a field is added, never a required one removed.
type RuntimeConfig struct {
	// IdentityURL is the browser-reachable identity-service origin (the OIDC
	// issuer). Read from the same env every verifier-consuming node already
	// carries -- component/genesis/domain.go's ApplyDomainDerivations sets
	// it once at boot -- never re-derived here.
	IdentityURL string `json:"identityUrl"`
	// IdentityAPIBaseURL is the base a client uses for the identity JSON
	// calls it makes with fetch() -- POST /oauth/token, /auth/refresh,
	// /auth/logout. Equal to IdentityURL: cross-origin against the
	// identity service, the zero-configuration default every deployment
	// gets without front-door rules. This WORKS: csp.go's connect-src
	// names the cluster's identity origin (identityOriginFromEnv, reading
	// the same env this field does) on every site's policy alike, not
	// only the portal's, which is what makes a cross-origin fetch() here
	// survive the browser's own CSP check rather than being silently
	// blocked before the request leaves the page.
	//
	// REJECTED FOR NOW: a same-origin path proxied through the site's own
	// /_memql/* surface to identity. It would need a SECOND proxy target
	// (today /_memql/* forwards only to the bff, which does not itself
	// serve /oauth/token et al.) and a second declared prefix to carry
	// it, which is a design change to the proxy Task 7 shipped -- not
	// this task's to make. Cross-origin is not a workaround; it is the
	// same shape every other OAuth 2.1 + PKCE public client on the web
	// uses, and it is what component/portal/config.go defaulted to
	// before this epic.
	IdentityAPIBaseURL string `json:"identityApiBaseUrl"`
	// OAuthClientID is the public OAuth client_id registered for THIS
	// site's own hostname -- see clientIDForHostname. Empty when no
	// registered client's redirect URI names this hostname.
	OAuthClientID string `json:"oauthClientId"`
	// AuthEnabled mirrors this cluster's own enforcement posture
	// (component/config.IdentityAuthEnabled). Not a permission -- every
	// read and write a site's own application performs is still gated
	// server-side on whatever stream it dials.
	AuthEnabled bool `json:"authEnabled"`
}

// serveRuntimeConfig answers GET /runtime-config.json for site. Every live
// site gets one -- this is cluster-wide identity discovery, not an opt-in
// capability, so there is no per-site flag gating it the way APIProxy gates
// serveAPI.
func (h *Handler) serveRuntimeConfig(w http.ResponseWriter, r *http.Request, site *Site) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	doc := runtimeConfigForSite(site, os.Getenv, config.IdentityAuthEnabled())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(doc)
}

// runtimeConfigForSite builds the document. env and authEnabled are
// injected so the derivation is testable without a listening server or a
// real process environment; authEnabled is a separate parameter rather than
// read through env because component/config.IdentityAuthEnabled reads
// os.Getenv directly and has its own tested false-value vocabulary
// (false/0/no/off) that this package must not reimplement.
func runtimeConfigForSite(site *Site, env func(string) string, authEnabled bool) RuntimeConfig {
	identityURL := identityURLFromEnv(env)
	hostname := ""
	if site != nil {
		hostname = site.Hostname
	}
	return RuntimeConfig{
		IdentityURL:        identityURL,
		IdentityAPIBaseURL: identityURL,
		OAuthClientID:      clientIDForHostname(hostname, env("MEMQL_IDENTITY_REGISTERED_CLIENTS")),
		AuthEnabled:        authEnabled,
	}
}

// registeredClient mirrors ONE entry of MEMQL_IDENTITY_REGISTERED_CLIENTS --
// the same wire shape component/genesis/domain.go serializes. A read-only,
// minimal reflection of that shape: this package never writes the variable
// and never imports the much heavier component/identity Config it belongs
// to, which the edge has no other reason to depend on.
type registeredClient struct {
	ClientId     string   `json:"clientId"`
	RedirectURIs []string `json:"redirectURIs"`
}

// clientIDForHostname finds the OAuth client registered for hostname's own
// callback -- https://<hostname>/auth/callback, forced https to match
// siteOriginOf's convention (csp.go) -- and returns its clientId, or "" when
// none matches or the input cannot be read.
//
// THIS IS WHAT MAKES OAuthClientID GENERIC. It is looked up from DATA --
// the requesting site's own hostname against the cluster's already-derived
// client registry -- never from a name this package recognises. Site #1's
// row happens to resolve to its seeded client id here only because
// component/genesis/domain.go registered that site's own callback URI under
// that same id; a customer site's row resolves through the exact same
// lookup once ITS hostname is registered the same way, and an unregistered
// site's row resolves to "" -- not a special case, the same code path
// answering honestly that it found nothing.
func clientIDForHostname(hostname string, registeredClientsJSON string) string {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" || strings.TrimSpace(registeredClientsJSON) == "" {
		return ""
	}
	want := "https://" + hostname + "/auth/callback"

	var clients []registeredClient
	if err := json.Unmarshal([]byte(registeredClientsJSON), &clients); err != nil {
		return ""
	}
	for _, c := range clients {
		for _, uri := range c.RedirectURIs {
			if uri == want {
				return c.ClientId
			}
		}
	}
	return ""
}

// identityURLFromEnv resolves the browser-reachable identity-service origin
// from the same env component/genesis/domain.go's ApplyDomainDerivations
// already sets at boot. Shared by runtimeConfigForSite (identityUrl /
// identityApiBaseUrl above) and csp.go's identityOriginFromEnv (connect-src)
// so the document and the policy can never name two different identity
// origins -- one function, two callers, not two derivations that could
// drift against each other.
func identityURLFromEnv(env func(string) string) string {
	return firstNonEmpty(
		env("MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER"),
		env("MEMQL_IDENTITY_BASE_URL"),
	)
}

// firstNonEmpty returns the first argument that is non-blank after
// trimming, or "" when every argument is blank.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}
