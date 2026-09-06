package envregistry

import (
	"os"
	"strings"
	"testing"
)

func TestDomainDerivations(t *testing.T) {
	got := DomainDerivations("memql.localhost")

	want := map[string]string{
		"MEMQL_IDENTITY_BASE_URL":                 "https://identity.memql.localhost",
		"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER": "https://identity.memql.localhost",
		"MEMQL_IDENTITY_BOOTSTRAP_DOMAIN":         "memql.localhost",
		"MEMQL_DISCOVERY_GRPC_ENDPOINT":           "api.memql.localhost:443",
		"MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS":     "https://api.memql.localhost,https://app.memql.localhost,https://os.memql.localhost",
		"MEMQL_MCP_PUBLIC_URL":                    "https://mcp.memql.localhost",
	}
	for name, wantVal := range want {
		if got[name] != wantVal {
			t.Errorf("%s = %q, want %q", name, got[name], wantVal)
		}
	}

	clients := got["MEMQL_IDENTITY_REGISTERED_CLIENTS"]
	for _, fragment := range []string{
		`"clientId":"cockpit"`,
		`"clientId":"os"`,
		`"clientId":"app"`,
		"https://os.memql.localhost/auth/callback",
		"https://app.memql.localhost/auth/callback",
		"http://127.0.0.1/cockpit/callback",
	} {
		if !strings.Contains(clients, fragment) {
			t.Errorf("registered clients %q missing %q", clients, fragment)
		}
	}

	// ONE URI for the OS shell, not two. Registering both the origin it is
	// served from now AND one it moved off of is the accept-either pattern
	// the no-shims rule forbids -- and this derivation has moved a redirect
	// URI three times now, so the negative half is the half that keeps it
	// honest. The client was called `portal` and carried BOTH the portal's
	// callback and the OS's until epic memql#4984 retired the portal; neither
	// the old client id nor any portal.<d> URI may come back.
	for _, gone := range []string{
		"https://api.memql.localhost/portal/auth/callback",
		"https://portal.memql.localhost/auth/callback",
		`"clientId":"portal"`,
	} {
		if strings.Contains(clients, gone) {
			t.Errorf("registered clients still carry the retired %q: %q", gone, clients)
		}
	}
}

// A domain that is not a domain derives NOTHING. Deriving from garbage would
// mint an issuer no client can reach and fail later, at sign-in, as an
// unrelated-looking error.
func TestDomainDerivationsRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"", "   ", "localhost", "https://memql.localhost",
		"memql.localhost:443", "MEMQL.localhost", "memql.localhost.",
		"*.memql.localhost", "127.0.0.1", "memql..localhost",
	} {
		if got := DomainDerivations(bad); len(got) != 0 {
			t.Errorf("DomainDerivations(%q) = %v, want empty", bad, got)
		}
	}
}

// Set-if-absent. An explicitly configured value is a statement of intent and
// always wins -- this is what keeps staging and prod untouched.
func TestApplyDomainDerivationsExplicitWins(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "memql.localhost")
	t.Setenv("MEMQL_IDENTITY_BASE_URL", "https://auth.example.com")
	t.Setenv("MEMQL_IDENTITY_BOOTSTRAP_DOMAIN", "")

	ApplyDomainDerivations(nil)

	if got := os.Getenv("MEMQL_IDENTITY_BASE_URL"); got != "https://auth.example.com" {
		t.Errorf("BASE_URL = %q, want the explicit value untouched", got)
	}
	if got := os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_DOMAIN"); got != "memql.localhost" {
		t.Errorf("BOOTSTRAP_DOMAIN = %q, want it derived", got)
	}
}

// No domain, no derivation. A node configured entirely by explicit env keeps
// behaving exactly as it does today.
func TestApplyDomainDerivationsNoopWithoutDomain(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "")
	t.Setenv("MEMQL_IDENTITY_BOOTSTRAP_DOMAIN", "")

	ApplyDomainDerivations(nil)

	if got := os.Getenv("MEMQL_IDENTITY_BOOTSTRAP_DOMAIN"); got != "" {
		t.Errorf("BOOTSTRAP_DOMAIN = %q, want empty", got)
	}
}

// D4: the API edge is named for its role. Six consumers dial this endpoint --
// the Cockpit, the VS Code extension, sdk/go, sdk/ts, workers, the portal --
// and a seventh reads it out of a List-Unsubscribe header we send.
func TestDomainDerivationsUseApiHost(t *testing.T) {
	got := DomainDerivations("memql.localhost")

	if want := "api.memql.localhost:443"; got["MEMQL_DISCOVERY_GRPC_ENDPOINT"] != want {
		t.Errorf("MEMQL_DISCOVERY_GRPC_ENDPOINT = %q, want %q",
			got["MEMQL_DISCOVERY_GRPC_ENDPOINT"], want)
	}
	for name, v := range got {
		if strings.Contains(v, "cockpit.") {
			t.Errorf("%s still derives a cockpit. host: %q", name, v)
		}
	}
}

// Idempotent, like ApplyLegacyEnvAliases: a second call finds every name
// populated and changes nothing.
func TestApplyDomainDerivationsIdempotent(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "memql.localhost")
	t.Setenv("MEMQL_IDENTITY_BASE_URL", "")

	ApplyDomainDerivations(nil)
	first := os.Getenv("MEMQL_IDENTITY_BASE_URL")
	ApplyDomainDerivations(nil)

	if got := os.Getenv("MEMQL_IDENTITY_BASE_URL"); got != first {
		t.Errorf("second call changed BASE_URL: %q -> %q", first, got)
	}
}
