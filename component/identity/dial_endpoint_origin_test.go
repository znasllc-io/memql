package identity

import "testing"

// TestDialEndpointFromOrigin_IsSeparableFromTheEnvLookup asserts the
// origin-to-dial mapping is usable WITHOUT the environment lookup
// (memql#3434): a caller that already holds an origin -- the worker-pairing
// redeem handler, whose origin is the one stamped on the pairing row -- maps
// it directly instead of going through DeriveGRPCEndpoint, whose first tier
// is the cluster-wide MEMQL_DISCOVERY_GRPC_ENDPOINT.
//
// Purity is structural: DialEndpointFromOrigin takes no env accessor, so
// there is nothing for an override to reach. What this test pins is that the
// extracted mapping is the SAME one DeriveGRPCEndpoint falls back to, so the
// split changed no published value.
func TestDialEndpointFromOrigin_IsSeparableFromTheEnvLookup(t *testing.T) {
	emptyEnv := func(string) string { return "" }

	cases := []struct {
		name   string
		origin string
		want   string
	}{
		{"https origin gets the front-door port", "https://app.acme.com", "app.acme.com:443"},
		{"the origin's own HTTP port is discarded", "https://app.acme.com:8443/path", "app.acme.com:443"},
		{"plaintext dev origin gets the plaintext gRPC port", "http://localhost:3000", "localhost:50050"},
		{"a bare host is readable", "localhost", "localhost:50050"},
		{"whitespace is not significant", "  http://identity:8081  ", "identity:50050"},
		{"empty in, empty out", "", ""},
		{"unreadable in, empty out", "://", ""},
		{"bracketed IPv6 keeps its brackets", "https://[::1]:8443", "[::1]:443"},
		{"unbracketed IPv6 is ambiguous and refused", "https://::1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DialEndpointFromOrigin(tc.origin); got != tc.want {
				t.Errorf("DialEndpointFromOrigin(%q) = %q, want %q", tc.origin, got, tc.want)
			}
			if got := deriveGRPCEndpoint(tc.origin, emptyEnv); got != tc.want {
				t.Errorf("deriveGRPCEndpoint(%q, empty) = %q, want %q -- extraction changed tier 2", tc.origin, got, tc.want)
			}
		})
	}

	// The wrapper still prefers the environment; only the mapping is pure.
	overridingEnv := func(k string) string {
		if k == "MEMQL_DISCOVERY_GRPC_ENDPOINT" {
			return "cockpit.local.znas.io:443"
		}
		return ""
	}
	if got := deriveGRPCEndpoint("https://app.acme.com", overridingEnv); got != "cockpit.local.znas.io:443" {
		t.Errorf("deriveGRPCEndpoint with override = %q, want the override", got)
	}
}
