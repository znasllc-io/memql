package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DiscoveryPath is where the identity service publishes its
// cluster-bootstrap document (the engine's identity.DiscoveryDocument,
// component/identity/discovery.go): the issuer, gRPC endpoint, client
// id and a default cluster name -- exactly the fields
// `memql cluster add` needs.
const DiscoveryPath = "/.well-known/memql-config.json"

// DiscoveryDocument mirrors the engine's shape. The JSON contract is
// additive; unknown fields are ignored.
type DiscoveryDocument struct {
	IdentityURL  string `json:"identityUrl"`
	GRPCEndpoint string `json:"grpcEndpoint"`
	ClientId     string `json:"clientId"`
	ClusterName  string `json:"clusterName"`
}

// FetchDiscovery GETs base+DiscoveryPath and decodes the document.
// base is an origin like https://identity.example.com; callers strip
// any path first.
func FetchDiscovery(ctx context.Context, base string) (*DiscoveryDocument, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil, fmt.Errorf("discovery: base URL is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+DiscoveryPath, nil)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery: %s answered %s", base+DiscoveryPath, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("discovery: read: %w", err)
	}
	var doc DiscoveryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("discovery: parse %s: %w", base+DiscoveryPath, err)
	}
	if strings.TrimSpace(doc.IdentityURL) == "" {
		return nil, fmt.Errorf("discovery: %s%s returned no identityUrl", base, DiscoveryPath)
	}
	return &doc, nil
}

// ComposeFromDomain builds a ClusterConfig from one domain by the
// front-door convention: api.<domain> is the gRPC front door,
// identity.<domain> the issuer. The client is the Cockpit's compiled
// DefaultClientId, which is deliberately NOT stored on the entry (see
// ClusterConfig.ClientId). The discovery document is authoritative when
// reachable; this is the fallback for clusters that predate the endpoint.
func ComposeFromDomain(domain string) ClusterConfig {
	domain = strings.TrimSpace(domain)
	return ClusterConfig{
		Name:        DomainToName(domain),
		DisplayName: domain,
		Domain:      domain,
		Endpoint:    "https://api." + domain,
		Issuer:      "https://identity." + domain,
	}
}

// DomainToName derives a clusters.yaml slot name from a domain:
// lowercase, dots become dashes, anything outside [a-z0-9-] is
// dropped, capped at 32 bytes ("staging.example.com" ->
// "staging-example-com").
func DomainToName(domain string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(domain)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == '.':
			b.WriteByte('-')
		}
	}
	s := b.String()
	if len(s) > 32 {
		s = s[:32]
	}
	return strings.Trim(s, "-")
}
