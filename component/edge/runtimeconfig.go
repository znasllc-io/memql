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
	// carries -- component/envregistry/domain.go's ApplyDomainDerivations sets
	// it once at boot -- never re-derived here.
	IdentityURL string `json:"identityUrl"`
	// IdentityAPIBaseURL is the base a client uses for the identity JSON
	// calls it makes with fetch() -- POST /oauth/token, /auth/refresh,
	// /auth/logout, GET /.well-known/jwks.json. Empty means same-origin:
	// the browser resolves those paths against the site that served the
	// page, and serveIdentityXHR (identity_proxy.go) forwards the exact
	// four paths to the identity binary. That is the topology
	// docs/public/operate/auth/identity-service.md requires -- sibling
	// hosts that share a wildcard cert + IP otherwise HTTP/2-coalesce
	// and a Mac browser's POST /oauth/token lands on this site's SPA
	// fallback (200 HTML, no access_token; memql#4154).
	//
	// IdentityURL stays the issuer for top-level /authorize navigation.
	// csp.go's connect-src still names the identity origin so a leftover
	// cross-origin fetch is not a CSP violation; the published base is
	// empty so the ordinary path never leaves the site origin.
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
	// Domain is the cluster's configured MEMQL_DOMAIN -- the value every
	// role host (api., identity., mcp.) is derived from (memql#3767). Served
	// so a client can compose an address that names THIS cluster (the
	// portal's "open in VS Code" link carries it as the cluster key) without
	// reverse-engineering it from identityUrl. Empty, never omitted, when
	// the node has no domain configured.
	Domain string `json:"domain"`
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
		IdentityAPIBaseURL: "",
		OAuthClientID:      clientIDForHostname(hostname, env("MEMQL_IDENTITY_REGISTERED_CLIENTS")),
		AuthEnabled:        authEnabled,
		Domain:             domainFromEnv(env),
	}
}

// registeredClient mirrors ONE entry of MEMQL_IDENTITY_REGISTERED_CLIENTS --
// the same wire shape component/envregistry/domain.go serializes. A read-only,
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
// component/envregistry/domain.go registered that site's own callback URI under
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
// from the same env component/envregistry/domain.go's ApplyDomainDerivations
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

// domainFromEnv reads MEMQL_DOMAIN through the injected env, trimmed. It
// deliberately does not import component/envregistry: that package derives
// OTHER variables from the domain and never exposes the domain itself, and
// this file's existing identityURLFromEnv already reads the derived values
// the same way.
func domainFromEnv(env func(string) string) string {
	return strings.TrimSpace(env("MEMQL_DOMAIN"))
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
