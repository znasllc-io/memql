package identity

import (
	"strings"
	"testing"
)

// The scheme-STATING dial forms (memql#3437).
//
// The discovery document publishes a bare `host[:port]` and must keep doing
// so -- that form is pinned by test/fixtures/discovery-endpoint-contract.json
// and asserted from Go and TypeScript. The worker-pairing reply has a
// different consumer, sdk/go/worker.ParseClusterURL, which reads a bare
// address as useTLS=false; a bare "cockpit.local.znas.io:443" therefore told
// a cockpit to dial a TLS port in plaintext.
//
// So there are two renderings of ONE answer, and the property that keeps them
// honest is the invariant every test here asserts alongside its expected
// value: strip the scheme from the URL form and the bare form comes back
// EXACTLY. The dial target never moves; only the silence about TLS is
// removed.

// stripScheme removes a leading `<scheme>://` if there is one.
func stripScheme(v string) string {
	if i := strings.Index(v, "://"); i >= 0 {
		return v[i+len("://"):]
	}
	return v
}

func TestDialURLFromOrigin_StatesWhatTheBareFormDropped(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		want   string
	}{
		{"a TLS origin dials the front door over TLS", "https://app.acme.com", "https://app.acme.com:443"},
		{"the origin's own HTTP port is still discarded", "https://app.acme.com:8443/path", "https://app.acme.com:443"},
		{"a plaintext dev origin stays plaintext", "http://localhost:3000", "http://localhost:50050"},
		{"a bare host reads as plaintext, exactly as the bare form reads it", "localhost", "http://localhost:50050"},
		{"whitespace is not significant", "  http://identity:8081  ", "http://identity:50050"},
		{"empty in, empty out", "", ""},
		{"unreadable in, empty out", "://", ""},
		{"bracketed IPv6 keeps its brackets", "https://[::1]:8443", "https://[::1]:443"},
		{"unbracketed IPv6 is ambiguous and refused", "https://::1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DialURLFromOrigin(tc.origin)
			if got != tc.want {
				t.Errorf("DialURLFromOrigin(%q) = %q, want %q", tc.origin, got, tc.want)
			}
			if bare, wantBare := stripScheme(got), DialEndpointFromOrigin(tc.origin); bare != wantBare {
				t.Errorf("DialURLFromOrigin(%q) = %q strips to %q, but DialEndpointFromOrigin says %q -- "+
					"the two forms must name the same dial target", tc.origin, got, bare, wantBare)
			}
		})
	}
}

func TestDialURLFromEndpoint_HonoursASchemeAndInventsNone(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		want       string
	}{
		// Rule 1 -- a scheme is authoritative, including on a port that no
		// convention covers.
		{"https implies TLS on the front-door port", "https://cockpit.acme.com", "https://cockpit.acme.com:443"},
		{"https on a non-standard port keeps both", "https://cockpit.acme.com:8443", "https://cockpit.acme.com:8443"},
		{"wss is the same statement", "wss://cockpit.acme.com", "https://cockpit.acme.com:443"},
		{"http implies plaintext on the dev gRPC port", "http://localhost", "http://localhost:50050"},
		{"http keeps an explicit port", "http://localhost:50050", "http://localhost:50050"},
		{"ws is the same statement", "ws://agent.internal:9000", "http://agent.internal:9000"},

		// Rule 2 -- no scheme, no port: state the transport the port
		// inference already implies, and nothing more.
		{"a bare remote host is the TLS front door", "cockpit.acme.com", "https://cockpit.acme.com:443"},
		{"a bare loopback host is a plaintext dev listener", "localhost", "http://localhost:50050"},
		{"127.0.0.1 likewise", "127.0.0.1", "http://127.0.0.1:50050"},

		// Rule 3 -- a port without a transport is left bare. THIS IS THE
		// DELIBERATE ONE: reading `:443` as https is exactly as much of a
		// guess as reading `:8443` as http, and the server does not guess on
		// a credential-carrying hop.
		{"the deployed discovery value names a port, not a transport", "cockpit.local.znas.io:443", "cockpit.local.znas.io:443"},
		{"a non-standard port is equally silent", "agent.acme.com:8443", "agent.acme.com:8443"},

		// An unrecognised scheme falls into rules 2/3 -- the same vocabulary
		// normalizeDialEndpoint reads, so the two cannot disagree.
		{"grpcs is not in the vocabulary either function reads", "grpcs://agent.acme.com:443", "agent.acme.com:443"},

		{"empty in, empty out", "", ""},
		{"unreadable in, empty out", "://", ""},
		{"unbracketed IPv6 is ambiguous and refused", "::1", ""},
		{"bracketed IPv6 keeps its brackets", "https://[::1]", "https://[::1]:443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DialURLFromEndpoint(tc.configured)
			if got != tc.want {
				t.Errorf("DialURLFromEndpoint(%q) = %q, want %q", tc.configured, got, tc.want)
			}
			if bare, wantBare := stripScheme(got), normalizeDialEndpoint(tc.configured); bare != wantBare {
				t.Errorf("DialURLFromEndpoint(%q) = %q strips to %q, but normalizeDialEndpoint says %q -- "+
					"the two forms must name the same dial target", tc.configured, got, bare, wantBare)
			}
		})
	}
}

// pairingDialURL mirrors the tiering the worker-pairing redeem handler applies
// (component/identity/http/pair.go), minus the operator override, so the
// shared #3399 fixture can be replayed through the scheme-stating path.
func pairingDialURL(configured, identityURL string) string {
	if v := DialURLFromEndpoint(configured); v != "" {
		return v
	}
	return DialURLFromOrigin(identityURL)
}

// TestDialURLFormsAgreeWithTheDiscoveryContract replays every case in the
// cross-language contract fixture through the scheme-stating forms and
// asserts they resolve to the SAME dial target the discovery document
// advertises.
//
// This is what makes memql#3437 provably a fix and not a second answer: the
// fixture is memql#3399's statement of what a client must be told, pinned
// from Go and TypeScript. If a scheme-stating value ever named a different
// host or port than its bare twin, the pairing reply and the discovery
// document would be sending workers to two different places -- exactly the
// silent cross-half drift that fixture exists to prevent.
func TestDialURLFormsAgreeWithTheDiscoveryContract(t *testing.T) {
	contract := loadEndpointContract(t)
	for _, tc := range contract.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			got := pairingDialURL(tc.Configured, tc.IdentityURL)
			if bare := stripScheme(got); bare != tc.Advertised {
				t.Errorf("configured=%q identityUrl=%q: pairing form %q strips to %q, "+
					"but the contract advertises %q", tc.Configured, tc.IdentityURL, got, bare, tc.Advertised)
			}
			// A value that resolved from an origin or a scheme-ful endpoint
			// must SAY so; only the port-without-transport case may be silent.
			if !strings.Contains(got, "://") && !strings.Contains(tc.Configured, "://") &&
				strings.Contains(tc.Configured, ":") {
				return // rule 3, deliberately bare
			}
			if got != "" && !strings.Contains(got, "://") {
				t.Errorf("configured=%q identityUrl=%q: pairing form %q states no transport, "+
					"so ParseClusterURL will read it as plaintext", tc.Configured, tc.IdentityURL, got)
			}
		})
	}
}

// TestDiscoveryDocumentIsUnaffectedByTheSchemeStatingForms is the negative
// half: memql#3399's published form must not have moved. The emission
// contract test covers this from the fixture; this states it as a direct
// property of the refactor that gave both forms one parser.
func TestDiscoveryDocumentIsUnaffectedByTheSchemeStatingForms(t *testing.T) {
	inputs := []string{
		"", "://", "cockpit.local.znas.io:443", "https://bff.local.znas.io",
		"https://cockpit.example.com:8443", "http://localhost:50050",
		"https://cockpit.example.com/", "cockpit.example.com", "localhost",
		"grpc://cockpit.example.com:443", "user@cockpit.example.com",
		"[::1]:443", "::1", "https://[::1]:8443",
	}
	for _, in := range inputs {
		got := normalizeDialEndpoint(in)
		if strings.Contains(got, "://") {
			t.Errorf("normalizeDialEndpoint(%q) = %q -- the discovery document publishes a bare dial address", in, got)
		}
	}
}
