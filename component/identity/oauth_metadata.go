package identity

import (
	"encoding/json"
	"net/http"
	"strings"
)

// OAuthServerMetadata is the RFC 8414 Authorization Server Metadata
// document. An MCP custom connector (and any other OAuth client) fetches
// it from /.well-known/oauth-authorization-server to discover the
// authorization/token/registration endpoints, the JWKS feed, and the
// supported flow parameters -- so the user doesn't hand-paste them.
//
// RegistrationEndpoint carries `omitempty` and is populated only when
// Cfg.OAuthDCREnabled is true (OAuthServerMetadataHandler leaves the field
// zero-valued otherwise, which `omitempty` then drops from the JSON
// entirely rather than serializing it as ""). RFC 8414 §2 makes the field
// OPTIONAL precisely so a server can signal non-support by omitting it: a
// DCR-disabled cluster that kept advertising /register unconditionally
// would tell every client "register here" and then answer 403
// registration_disabled.
//
// A struct (not a map) keeps the JSON field order stable.
type OAuthServerMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint,omitempty"`
	// DeviceAuthorizationEndpoint is the RFC 8628 §4 discovery field.
	// A device-flow client reads THIS rather than guessing a path, so
	// advertising it is what makes the fallback grant discoverable
	// (memql#3410).
	DeviceAuthorizationEndpoint       string   `json:"device_authorization_endpoint"`
	JWKSURI                           string   `json:"jwks_uri"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
}

// OAuthServerMetadataHandler returns an http.Handler that serves the
// RFC 8414 Authorization Server Metadata document. Mounted at
// /.well-known/oauth-authorization-server by Service.RegisterRoutes.
//
// Mirrors DiscoveryHandler's style: application/json, Cache-Control:
// no-store, json.NewEncoder. Absolute URLs are built from cfg.BaseURL
// with any trailing slash trimmed so we never emit a double slash.
//
// RegistrationEndpoint is set only when cfg.OAuthDCREnabled -- see the
// OAuthServerMetadata doc comment for why omitting it (rather than
// advertising a /register that answers 403) is the RFC 8414-conformant
// signal.
// # THE COPY SERVED FROM THE API HOST IS A POINTER, NOT A CONFORMANT DOCUMENT
// # FOR THAT HOST (memql#4624)
//
// `Issuer` is always cfg.BaseURL -- `https://identity.<domain>` -- including
// on the copy component/server mounts at `api.<domain>`. RFC 8414 §3.3 says
// the issuer value MUST be identical to the identifier the well-known URI was
// inserted into, so a client that fetches from the API host and requires a
// strict match will fail. That is correct behaviour on both sides, and the
// resolution is NOT to make the API host claim to be an issuer -- it is not
// one, nothing signs tokens there, and a client that believed it were would
// send credentials to the wrong host.
//
// The API-host copy exists so a client that knows only `api.<domain>` can
// LEARN where the issuer is. Having learned it, the client re-fetches from the
// issuer's own URL and validates strictly there. `discovery.ts` on the
// extension side does exactly that.
//
// # CORS
//
// Both well-known documents are public, unauthenticated, read-only and
// identical for every caller, so `*` gives away nothing that fetching the URL
// does not. It is here because a webview fetch is origin-checked while a Node
// fetch is not: without it, discovery works from the extension host and fails
// from any panel, which is the kind of difference that gets discovered late.
func OAuthServerMetadataHandler(cfg Config) http.Handler {
	base := strings.TrimRight(cfg.BaseURL, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setWellKnownCORS(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		doc := OAuthServerMetadata{
			Issuer:                            cfg.BaseURL,
			AuthorizationEndpoint:             base + "/authorize",
			TokenEndpoint:                     base + "/oauth/token",
			DeviceAuthorizationEndpoint:       base + "/device/code",
			JWKSURI:                           base + "/.well-known/jwks.json",
			ResponseTypesSupported:            []string{"code"},
			GrantTypesSupported:               []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code"},
			CodeChallengeMethodsSupported:     []string{"S256"},
			TokenEndpointAuthMethodsSupported: []string{"none"},
		}
		if cfg.OAuthDCREnabled {
			doc.RegistrationEndpoint = base + "/register"
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(doc)
	})
}

// setWellKnownCORS marks a public discovery document readable from any origin.
//
// Deliberately NOT the identity server's `cors` middleware, which reflects a
// configured allow-list because it guards endpoints that read cookies and
// mint credentials. These two documents carry neither: they are the same
// bytes for every caller, and an allow-list on them would mean a client can
// only DISCOVER a cluster it has already been configured for, which is
// backwards.
func setWellKnownCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type")
	w.Header().Set("Access-Control-Max-Age", "600")
}
