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
// Operators that want to point the product SPA at a different gRPC
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

// DeriveGRPCEndpoint resolves the gRPC host:port a client should dial.
// Order of preference:
//
//  1. MEMQL_DISCOVERY_GRPC_ENDPOINT explicit env override, NORMALIZED
//     to a dial address. This is the deployment posture: the operator
//     knows the public dial address -- the FRONT DOOR,
//     cockpit.<domain>:443 -- and deploy/k8s/base/identity.yaml writes
//     it into the env, per overlay. An override that cannot be read as
//     a host falls through to (2) rather than being published as-is.
//
//  2. The origin's host + the port its scheme implies. HTTPS gets 443
//     (gRPC over TLS through the single ingress); plaintext gets the
//     local bff gRPC port. The origin's OWN port is deliberately
//     discarded -- 8081/8085/3000 is where something serves HTTP, never
//     where a client dials MemqlService.
//
//     As the DISCOVERY fallback this is a last resort and a poor one:
//     the identity host serves HTTP and no MemqlService. It exists so
//     the field is never empty, not because it is right -- which is why
//     (1) is declared in base rather than left to each operator.
//
// A caller that already HOLDS the origin it wants mapped -- rather than
// wanting the cluster's advertised one -- must call DialEndpointFromOrigin
// instead: the env tier here is unconditional and would silently outrank the
// argument (memql#3434).
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
	if v := normalizeDialEndpoint(env("MEMQL_DISCOVERY_GRPC_ENDPOINT")); v != "" {
		return v
	}
	return DialEndpointFromOrigin(identityURL)
}

// DialEndpointFromOrigin maps ONE HTTP origin to the gRPC dial address a
// client should use for it. It is tier (2) of DeriveGRPCEndpoint with the
// environment lookup lifted off -- pure, reading only its argument.
//
// WHY IT IS SEPARATE (memql#3434). Two callers want different things from
// the same mapping. The discovery document wants "what should this CLUSTER
// advertise", so the operator's MEMQL_DISCOVERY_GRPC_ENDPOINT rightly
// outranks any derivation. The worker-pairing redeem handler wants "where
// does THIS pairing's origin live" -- the origin the product SPA stamped on
// the row (`window.location.origin`) -- and for it the cluster-wide
// advertised value is not a better answer, it is a different question.
// Routing that caller through DeriveGRPCEndpoint meant the env tier answered
// every time it was set, which in a deployed cluster is always: the stored
// origin was never read at all.
//
// The origin's OWN port is deliberately discarded. 8081/8085/3000 is where
// something serves HTTP; the scheme is what names the gRPC port -- HTTPS
// dials the single front-door 443, plaintext dials the local bff gRPC port.
//
// Host extraction agrees with normalizeDialEndpoint (and so with the TS
// parser in editors/vscode/src/connection/endpoint.ts): scheme, path, query
// and userinfo are stripped, a bracketed IPv6 literal keeps its brackets, and
// an unbracketed one is refused as ambiguous. Anything unreadable returns ""
// so the caller can fall through rather than dial garbage.
func DialEndpointFromOrigin(origin string) string {
	host := dialHostFromOrigin(origin)
	if host == "" {
		return ""
	}
	if isHTTPS(origin) {
		return host + ":443"
	}
	return host + ":" + plaintextDialPort
}

// dialHostFromOrigin returns just the host of an origin, brackets intact for
// an IPv6 literal, or "" when the authority cannot be read unambiguously.
func dialHostFromOrigin(raw string) string {
	v := strings.TrimSpace(raw)
	if i := strings.Index(v, "://"); i >= 0 {
		v = v[i+len("://"):]
	}
	if i := strings.IndexAny(v, "/?#"); i >= 0 {
		v = v[:i]
	}
	if i := strings.LastIndex(v, "@"); i >= 0 {
		v = v[i+1:]
	}
	host, _ := splitDialHostPort(strings.TrimSpace(v))
	return host
}

// plaintextDialPort is the gRPC port a non-TLS deployment dials. The
// TLS counterpart is 443 -- the single front-door port every ingress
// terminates on, which is why it needs no name.
const plaintextDialPort = "50050"

// normalizeDialEndpoint turns whatever an operator configured into the
// ONE form the discovery document is allowed to publish: a bare
// `host[:port]` dial address.
//
// WHY THIS EXISTS (memql#3399). The override used to be trusted
// verbatim, so the value an operator happened to write -- in the local
// cluster's case "https://bff.local.znas.io", carried in the genesis
// envelope -- went straight onto the wire. That is a URL, and a URL is
// not a dial address: `grpc.NewClient` cannot use it, and the VS Code
// extension's parser (editors/vscode/src/connection/endpoint.ts)
// refuses any scheme that is not ws/wss, by name, before it dials. A
// discovery document exists so a client does not have to guess; one
// that publishes an unusable spelling is worse than no document,
// because the client trusts it.
//
// So the scheme is READ (it names the port when none is given) and then
// DROPPED. Anything that cannot be read as a host returns "" and the
// caller falls back to its derived default -- publishing nothing usable
// is strictly better than publishing something wrong.
//
// This does NOT validate that the host exists or serves gRPC. Naming
// the right host is a deployment fact and lives in the overlay; this
// only guarantees the FORM.
func normalizeDialEndpoint(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}

	scheme := ""
	if i := strings.Index(v, "://"); i >= 0 {
		scheme = strings.ToLower(v[:i])
		v = v[i+len("://"):]
	}
	// A path, query or fragment is not part of an authority.
	if i := strings.IndexAny(v, "/?#"); i >= 0 {
		v = v[:i]
	}
	// Userinfo is a URL affordance with no meaning to a dialer.
	if i := strings.LastIndex(v, "@"); i >= 0 {
		v = v[i+1:]
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}

	host, port := splitDialHostPort(v)
	if host == "" {
		return ""
	}
	if port != "" {
		return host + ":" + port
	}
	switch scheme {
	case "https", "wss":
		return host + ":443"
	case "http", "ws":
		return host + ":" + plaintextDialPort
	}
	if isLoopbackHost(strings.Trim(host, "[]")) {
		return host + ":" + plaintextDialPort
	}
	return host + ":443"
}

// splitDialHostPort splits an authority into host and (optional) port,
// keeping an IPv6 literal's brackets. Returns an empty host for input
// it cannot read unambiguously -- notably an UNBRACKETED IPv6 address,
// whose own colons make "where does the host end" unanswerable. The
// client parser draws the same line, so agreeing with it here is the
// point.
func splitDialHostPort(authority string) (host, port string) {
	if strings.HasPrefix(authority, "[") {
		end := strings.Index(authority, "]")
		if end < 0 {
			return "", ""
		}
		host = authority[:end+1]
		rest := authority[end+1:]
		if strings.HasPrefix(rest, ":") && isAllDigits(rest[1:]) {
			return host, rest[1:]
		}
		if rest != "" {
			return "", ""
		}
		return host, ""
	}
	if i := strings.LastIndex(authority, ":"); i > 0 {
		if isAllDigits(authority[i+1:]) && !strings.Contains(authority[:i], ":") {
			return authority[:i], authority[i+1:]
		}
		return "", ""
	}
	if strings.Contains(authority, ":") {
		return "", ""
	}
	return authority, ""
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// deriveClientId picks the client id the cockpit should use. Order:
//  1. MEMQL_DISCOVERY_CLIENT_ID explicit override.
//  2. The first registered client's clientId. Operators register
//     the cockpit alongside the product SPA via
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
