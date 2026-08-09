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
// Forward-looking discovery: authorization_endpoint and
// registration_endpoint are advertised now even though the
// authorization-code + dynamic-client-registration handlers ship in
// later epic children (E3/E4). The document is a discovery contract, not
// a liveness assertion -- nothing consumes the advertised endpoints until
// the gated rollout wires them up.
//
// A struct (not a map) keeps the JSON field order stable.
type OAuthServerMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
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
func OAuthServerMetadataHandler(cfg Config) http.Handler {
	base := strings.TrimRight(cfg.BaseURL, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := OAuthServerMetadata{
			Issuer:                            cfg.BaseURL,
			AuthorizationEndpoint:             base + "/authorize",
			TokenEndpoint:                     base + "/oauth/token",
			RegistrationEndpoint:              base + "/register",
			DeviceAuthorizationEndpoint:       base + "/device/code",
			JWKSURI:                           base + "/.well-known/jwks.json",
			ResponseTypesSupported:            []string{"code"},
			GrantTypesSupported:               []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code"},
			CodeChallengeMethodsSupported:     []string{"S256"},
			TokenEndpointAuthMethodsSupported: []string{"none"},
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(doc)
	})
}
