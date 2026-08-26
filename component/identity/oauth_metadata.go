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
func OAuthServerMetadataHandler(cfg Config) http.Handler {
	base := strings.TrimRight(cfg.BaseURL, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		setDiscoveryCORS(w)
		_ = json.NewEncoder(w).Encode(doc)
	})
}
