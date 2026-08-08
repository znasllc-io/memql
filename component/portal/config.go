package portal

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/config"
)

// The portal's runtime configuration document (memql#3315).
//
// WHY THIS EXISTS AT ALL. The portal needs three facts before it can
// authenticate: which identity service to send the operator to, which OAuth
// client_id to present, and whether this cluster enforces auth in the first
// place. None of them can be compiled into the bundle. The engine image is
// PRODUCT-AGNOSTIC and environment-agnostic (see the platform-consolidation
// note in CLAUDE.md) -- the same bytes run against the local k3d cluster,
// staging, and a customer's install, each with a different identity host. A
// `VITE_MEMQL_IDENTITY_URL` baked at build time would make the bundle
// environment-specific, which is exactly the property the image model exists
// to avoid.
//
// WHY NOT /.well-known/memql-config.json. That document already carries
// identityUrl + clientId, but it is mounted by the IDENTITY binary
// (component/identity/identity.go), not the bff. A browser that has only
// loaded /portal/ does not yet know the identity origin, so it cannot fetch
// the discovery document -- that is the chicken-and-egg this endpoint breaks.
// The values here are deliberately the same shape, and a future consolidation
// could serve one from the other.
//
// WHY IT IS SAFE UNAUTHENTICATED. Every field is already public: the identity
// URL is the OIDC issuer printed in every JWT, the client id is a public
// OAuth client identifier (RFC 6749 §2.2 -- public clients have no secret,
// which is why the portal uses PKCE), and "is auth on" is observable by
// dialing the stream once. Nothing here is a credential, and the bundle it
// configures is public bytes on a public path (see server.PortalPaths).
//
// THE FUTURE-REGISTRY SEAM. #3315 resolved the cluster registry question in
// favour of "the cluster is the origin that served this page" (see
// clients/portal/src/cluster/endpoint.ts for the full reasoning). If that is
// ever revisited in favour of a SERVER-SIDE registry -- v1:cluster:* rows
// listing the clusters an operator may switch between -- this document is
// where the list would surface, because it is already the one thing the
// bundle reads before it has a connection to read anything else from. Adding
// a `clusters: [...]` field here and a picker in the endpoint resolver is the
// whole change; nothing else in the portal would move.

// runtimeConfigFile is the mount-relative path of the document, i.e.
// GET /portal/runtime-config.json.
//
// Named "runtime-config" rather than "config" on purpose: it must never
// collide with a file Vite might emit into the bundle root, because this
// handler answers the path BEFORE consulting the filesystem, and a silently
// shadowed asset is a worse failure than an ugly name.
const runtimeConfigFile = "runtime-config.json"

// DefaultOAuthClientID is the client_id the portal presents to the identity
// service when the operator has not registered a different one.
//
// The identity service resolves client ids against
// MEMQL_IDENTITY_REGISTERED_CLIENTS (or the DB-backed DCR store), and an
// unregistered id is refused at /authorize with "Unknown client" -- so this
// default is a CONVENTION, not a grant. The operator still has to register
// "portal" with the portal's callback URI; docs/public/operate/portal.md
// spells the entry out.
const DefaultOAuthClientID = "portal"

// OAuthClientIDEnvVar overrides DefaultOAuthClientID, for a deployment whose
// identity service already uses "portal" for something else.
const OAuthClientIDEnvVar = "MEMQL_PORTAL_OAUTH_CLIENT_ID"

// IdentityURLEnvVar overrides the derived identity URL. Rarely needed -- see
// identityURL for why the derivation is usually right.
const IdentityURLEnvVar = "MEMQL_PORTAL_IDENTITY_URL"

// IdentityAPIBaseURLEnvVar selects where the portal's identity XHR goes.
// SelfOriginSentinel is the one special value. See identityAPIBaseURL.
const IdentityAPIBaseURLEnvVar = "MEMQL_PORTAL_IDENTITY_API_BASE_URL"

// SelfOriginSentinel, as the value of IdentityAPIBaseURLEnvVar, means "the
// portal's own origin" -- the deployment has front-door rules proxying the
// identity JSON endpoints, so the browser should call them same-origin.
// Spelled as a word rather than an empty string because an empty env var is
// indistinguishable from an unset one to most deployment tooling, and this is
// a choice an operator makes deliberately.
const SelfOriginSentinel = "self"

// RuntimeConfig is the document's stable JSON shape. Additive only: the
// bundle and the node roll independently (a cached index.html can outlive a
// node restart), so an older bundle must keep working against a newer node.
type RuntimeConfig struct {
	// IdentityURL is the browser-reachable origin of the identity service --
	// the OIDC issuer. Used for TOP-LEVEL NAVIGATIONS: GET /authorize, which
	// is an HTML page in component/identity/web and has no CORS and no
	// same-origin alternative. Empty when the node cannot derive one, which
	// the portal reports as a configuration error rather than guessing.
	IdentityURL string `json:"identityUrl"`
	// IdentityAPIBaseURL is the base for the identity JSON calls the portal
	// makes with fetch(): POST /oauth/token, /auth/refresh, /auth/logout.
	//
	// SEPARATE FROM IdentityURL ON PURPOSE, and it mirrors the topology
	// docs/public/operate/auth/identity-service.md already prescribes: browsers
	// should reach those three SAME-ORIGIN through the front door, which
	// proxies them to identity, while only the top-level magic-link
	// navigations go to identity.<domain> directly. The stated reason is a
	// Safari HTTP/2 connection-coalescing bug that intermittently fails
	// cross-origin credentialed XHR to a sibling host sharing a wildcard
	// certificate and IP -- it surfaces as "TypeError: Load failed" with no
	// server-side trace.
	//
	// Empty means SAME-ORIGIN (the front door proxies; nothing to configure in
	// the browser). Otherwise an absolute origin, which additionally requires
	// the portal's origin in MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS.
	//
	// Defaults to IdentityURL -- cross-origin, which works out of the box on a
	// deployment with no proxy rules, at the cost of the Safari caveat above.
	// An operator who adds the front-door rules sets the env var to "self".
	IdentityAPIBaseURL string `json:"identityApiBaseUrl"`
	// OAuthClientID is the public OAuth client_id the portal presents.
	OAuthClientID string `json:"oauthClientId"`
	// AuthEnabled mirrors this node's own enforcement posture. False only
	// where MEMQL_IDENTITY_ENABLED=false (auth disabled for troubleshooting;
	// never in staging/production -- see CLAUDE.md), where the stream admits
	// every dial as the synthetic local-dev cluster owner.
	//
	// THIS IS NOT A PERMISSION. It tells the portal whether a sign-in flow
	// can succeed at all, so a cluster with no identity service shows the
	// concept browser instead of a sign-in wall nothing can satisfy. Every
	// read and write is still gated server-side on the stream the bundle
	// dials -- a browser that lies to itself about this field gains nothing.
	AuthEnabled bool `json:"authEnabled"`
}

// serveRuntimeConfig writes the document. Always no-store: it is small, it is
// read once per page load, and a stale copy would point a browser at the
// wrong identity service after a reconfiguration -- which presents as a login
// loop with no error anywhere.
func (h *Handler) serveRuntimeConfig(w http.ResponseWriter, r *http.Request) {
	if r != nil && r.Method != "" && r.Method != http.MethodGet && r.Method != http.MethodHead {
		noCache(w)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(runtimeConfigFromEnv(os.Getenv))
}

// runtimeConfigFromEnv builds the document from the node's own environment.
// env is injected so the test can drive it without mutating the process.
func runtimeConfigFromEnv(env func(string) string) RuntimeConfig {
	base := identityURL(env)
	return RuntimeConfig{
		IdentityURL:        base,
		IdentityAPIBaseURL: identityAPIBaseURL(env, base),
		OAuthClientID:      oauthClientID(env),
		AuthEnabled:        config.IdentityAuthEnabled(),
	}
}

// identityAPIBaseURL resolves the XHR base. See RuntimeConfig.IdentityAPIBaseURL.
func identityAPIBaseURL(env func(string) string, identityBase string) string {
	v := strings.TrimSpace(env(IdentityAPIBaseURLEnvVar))
	if strings.EqualFold(v, SelfOriginSentinel) {
		return ""
	}
	if v != "" {
		return strings.TrimRight(v, "/")
	}
	return identityBase
}

// identityURL resolves the BROWSER-REACHABLE identity origin.
//
// The obvious candidate -- MEMQL_IDENTITY_VERIFIER_BASE_URL -- is the WRONG
// one, and quietly so. That is the in-cluster address a node fetches JWKS
// from ("https://identity:8085" in every deploy/k8s manifest); handing it to
// a browser produces a DNS failure on a name that only resolves inside the
// pod network. Using it would work on a laptop running everything on
// localhost and fail on every real deployment, which is the worst possible
// distribution of outcomes.
//
// The right value is the ISSUER: MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER is
// already set on every engine node and must be byte-identical to the identity
// binary's MEMQL_IDENTITY_BASE_URL (the overlays enforce this -- see the
// comment in deploy/k8s/overlays/local/kustomization.yaml), and an issuer is
// by construction the public origin operators reach identity at. So the
// portal gets a correct, browser-reachable URL on every existing deployment
// with no new configuration at all.
//
// Order: explicit override, then the issuer, then MEMQL_IDENTITY_BASE_URL
// (set on the identity binary itself, which also serves a portal in the
// single-binary build).
func identityURL(env func(string) string) string {
	for _, key := range []string{
		IdentityURLEnvVar,
		"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER",
		"MEMQL_IDENTITY_BASE_URL",
	} {
		if v := strings.TrimRight(strings.TrimSpace(env(key)), "/"); v != "" {
			return v
		}
	}
	return ""
}

func oauthClientID(env func(string) string) string {
	if v := strings.TrimSpace(env(OAuthClientIDEnvVar)); v != "" {
		return v
	}
	return DefaultOAuthClientID
}
