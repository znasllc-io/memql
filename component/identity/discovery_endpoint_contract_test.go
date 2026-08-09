package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Go half of the discovery-endpoint contract (memql#3399).
//
// The cluster advertises a dial address in GET /.well-known/memql-config.json.
// A client reads that field and makes a connection out of it -- for the VS Code
// extension, editors/vscode/src/connection/endpoint.ts lifts it to a /memql/ws
// URL and refuses anything carrying a scheme other than ws/wss. Nothing in the
// build links the emitter to that parser, so both sides stayed green while the
// live cluster published "https://bff.local.znas.io": a form the parser rejects
// outright, naming a host with no ingress.
//
// The fix for the string is a deploy value. The fix for the DRIFT is this pair
// of tests: one fixture, asserted from both sides. The TypeScript half is
// editors/vscode/test/discoveryEndpointContract.test.ts, and it reads the same
// file -- adding a case here obliges it there.

// contractFixturePath is the shared statement, relative to this package.
const contractFixturePath = "../../test/fixtures/discovery-endpoint-contract.json"

type endpointContractCase struct {
	Name         string `json:"name"`
	Configured   string `json:"configured"`
	IdentityURL  string `json:"identityUrl"`
	Advertised   string `json:"advertised"`
	WebSocketURL string `json:"webSocketUrl"`
}

type endpointContract struct {
	Cases    []endpointContractCase `json:"cases"`
	Rejected []string               `json:"rejected"`
}

func loadEndpointContract(t *testing.T) endpointContract {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(contractFixturePath))
	if err != nil {
		t.Fatalf("read shared contract fixture: %v", err)
	}
	var c endpointContract
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse shared contract fixture: %v", err)
	}
	if len(c.Cases) == 0 {
		t.Fatal("shared contract fixture carries no cases")
	}
	return c
}

// TestDiscoveryEndpointContract_Emission pins what the handler publishes for
// each configuration the fixture names.
func TestDiscoveryEndpointContract_Emission(t *testing.T) {
	contract := loadEndpointContract(t)
	for _, tc := range contract.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			env := func(k string) string {
				if k == "MEMQL_DISCOVERY_GRPC_ENDPOINT" {
					return tc.Configured
				}
				return ""
			}
			got := deriveGRPCEndpoint(tc.IdentityURL, env)
			if got != tc.Advertised {
				t.Errorf("configured=%q identityUrl=%q: advertised %q, want %q",
					tc.Configured, tc.IdentityURL, got, tc.Advertised)
			}
		})
	}
}

// TestDiscoveryEndpointContract_NeverPublishesARejectedForm is the property the
// emission table implies but does not state: whatever an operator configures,
// the document may not hand a client a value that client refuses. A URL is the
// specific shape memql#3399 shipped, so it is checked by name as well as by
// membership in the fixture's rejected list.
func TestDiscoveryEndpointContract_NeverPublishesARejectedForm(t *testing.T) {
	contract := loadEndpointContract(t)
	rejected := map[string]bool{}
	for _, r := range contract.Rejected {
		rejected[r] = true
	}

	// Every configuration the fixture names, plus every rejected spelling fed
	// back in AS the configuration -- an operator's stale value must not
	// survive to the wire.
	configured := make([]string, 0, len(contract.Cases)+len(contract.Rejected))
	for _, tc := range contract.Cases {
		configured = append(configured, tc.Configured)
	}
	configured = append(configured, contract.Rejected...)

	for _, in := range configured {
		env := func(k string) string {
			if k == "MEMQL_DISCOVERY_GRPC_ENDPOINT" {
				return in
			}
			return ""
		}
		for _, identityURL := range []string{"https://identity.acme.com", "http://localhost:8081"} {
			got := deriveGRPCEndpoint(identityURL, env)
			if rejected[got] {
				t.Errorf("configured=%q identityUrl=%q: advertised %q, which the client contract rejects",
					in, identityURL, got)
			}
			if strings.Contains(got, "://") {
				t.Errorf("configured=%q identityUrl=%q: advertised %q carries a URL scheme; the field is a dial address (host[:port])",
					in, identityURL, got)
			}
			if got != "" && !strings.Contains(got, ":") {
				t.Errorf("configured=%q identityUrl=%q: advertised %q carries no port; a client cannot dial it",
					in, identityURL, got)
			}
		}
	}
}
