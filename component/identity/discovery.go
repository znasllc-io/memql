package identity

import (
	"encoding/json"
	"net/http"
	"strings"
)

// DiscoveryDocument is the public-facing config the cockpit reads
// to bootstrap a cluster registration without requiring the user to
// hand-paste several flags.
//
// Served at /.well-known/memql-config.json. Stable JSON shape; new
// fields are additive.
//
// Operators that want to point CoPresent at a different gRPC
// endpoint than the identity origin set MEMQL_DISCOVERY_GRPC_ENDPOINT
// (or fall through to MEMQL_IDENTITY_BASE_URL's host as a sensible
// default).
type DiscoveryDocument struct {
	// IdentityURL is the OIDC issuer. Used by the cockpit's OAuth
	// flow as the issuer + as the base for /oauth/token, /authorize,
	// and /.well-known/jwks.json.
	IdentityURL string `json:"identityUrl"`
	// GRPCEndpoint is the host:port the cockpit should dial for
	// MemqlService.Stream and WorkerService.Stream. Defaults to
	// the host of IdentityURL when no separate value is configured.
	GRPCEndpoint string `json:"grpcEndpoint"`
	// ClientId is the OAuth2 client_id the cockpit should present
	// during the OIDC flow. The first registered client is the
	// canonical "cockpit" client; operators with multiple clients
	// can override via MEMQL_DISCOVERY_CLIENT_ID.
	ClientId string `json:"clientId"`
	// ClusterName is a human-readable default name for the cluster
	// when the cockpit registers it locally. Derived from the
	// identity URL host when unset; falls back to "default".
	ClusterName string `json:"clusterName"`
}

// DiscoveryHandler returns an http.Handler that serves the
// well-known discovery document. Mounted at
// /.well-known/memql-config.json by Service.RegisterRoutes.
//
// The handler reads from the supplied Config + env-var overrides on
// every request -- cheap enough that we don't need to cache, and
// it lets operators rotate config without restarting the binary.
func DiscoveryHandler(cfg Config, envLookup func(string) string) http.Handler {
	if envLookup == nil {
		envLookup = func(string) string { return "" }
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := DiscoveryDocument{
			IdentityURL:  cfg.BaseURL,
			GRPCEndpoint: deriveGRPCEndpoint(cfg.BaseURL, envLookup),
			ClientId:     deriveClientId(cfg, envLookup),
			ClusterName:  deriveClusterName(cfg.BaseURL, envLookup),
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(doc)
	})
}

// DeriveGRPCEndpoint resolves the gRPC host:port the cockpit should
// dial. Order of preference:
//
//  1. MEMQL_DISCOVERY_GRPC_ENDPOINT explicit env override -- always
//     trusted, used verbatim. This is the production posture: the
//     operator knows the public dial address (e.g.
//     bff.copresent.acme.com:443) and writes it into the env on the
//     identity binary.
//  2. Default: hostFromURL(identityURL) + a scheme-appropriate port.
//     HTTPS deployments default to 443 (gRPC over TLS, single ALB
//     entry point). HTTP localhost dev defaults to 50050 (the local
//     bff gRPC dial port; the k3d cluster port-forwards bff:50051).
//
// Exported so the worker-pairing redeem handler can translate the
// HTTP origin CoPresent passes (`window.location.origin`) into a
// dial address the cockpit can actually grpc.NewClient against.
//
// Note: MEMQL_GRPC_ADDRESS is INTENTIONALLY ignored. That env var
// configures the identity binary's OWN listen address (e.g.
// ":50051") -- it is not a dial address for external clients, and
// publishing it via discovery would emit a host-less ":50051" that
// cockpits couldn't actually dial. Operators who want a non-default
// dial address use MEMQL_DISCOVERY_GRPC_ENDPOINT.
func DeriveGRPCEndpoint(identityURL string, env func(string) string) string {
	return deriveGRPCEndpoint(identityURL, env)
}

func deriveGRPCEndpoint(identityURL string, env func(string) string) string {
	if v := strings.TrimSpace(env("MEMQL_DISCOVERY_GRPC_ENDPOINT")); v != "" {
		return v
	}
	host := hostFromURL(identityURL)
	if host == "" {
		return ""
	}
	if isHTTPS(identityURL) {
		return host + ":443"
	}
	if isLoopbackHost(host) {
		return host + ":50050"
	}
	return host + ":50050"
}

// deriveClientId picks the client id the cockpit should use. Order:
//  1. MEMQL_DISCOVERY_CLIENT_ID explicit override.
//  2. The first registered client's clientId. Operators register
//     the cockpit alongside the CoPresent SPA via
//     MEMQL_IDENTITY_REGISTERED_CLIENTS.
//  3. Empty -- cockpit will refuse to use the discovery shortcut
//     and prompt for explicit flags.
func deriveClientId(cfg Config, env func(string) string) string {
	if v := strings.TrimSpace(env("MEMQL_DISCOVERY_CLIENT_ID")); v != "" {
		return v
	}
	for _, c := range cfg.RegisteredClients {
		if id := strings.TrimSpace(c.ClientId); id != "" {
			return id
		}
	}
	return ""
}

// deriveClusterName picks a default name for the cluster row the
// cockpit will write to ~/.memql/clusters.yaml. Operators can
// override; callers can override via the cockpit's --name flag.
func deriveClusterName(identityURL string, env func(string) string) string {
	if v := strings.TrimSpace(env("MEMQL_DISCOVERY_CLUSTER_NAME")); v != "" {
		return v
	}
	host := hostFromURL(identityURL)
	if host == "" {
		return "default"
	}
	return host
}

func hostFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	stripped := strings.TrimPrefix(raw, "https://")
	stripped = strings.TrimPrefix(stripped, "http://")
	if i := strings.IndexByte(stripped, '/'); i >= 0 {
		stripped = stripped[:i]
	}
	if i := strings.IndexByte(stripped, ':'); i >= 0 {
		stripped = stripped[:i]
	}
	return stripped
}

// isHTTPS reports whether raw begins with "https://".
func isHTTPS(raw string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "https://")
}

// isLoopbackHost reports whether the given host is a loopback name.
// Used by AllowsRedirectURI / pickOAuthCtx for RFC 8252-style
// loopback-redirect-with-arbitrary-port matching.
func isLoopbackHost(h string) bool {
	switch strings.ToLower(strings.TrimSpace(h)) {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}
