package genesis

import (
	"os"
	"strings"
	"testing"
)

func TestDomainDerivations(t *testing.T) {
	got := DomainDerivations("memql.localhost", "")

	want := map[string]string{
		"MEMQL_IDENTITY_BASE_URL":                 "https://identity.memql.localhost",
		"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER": "https://identity.memql.localhost",
		"MEMQL_IDENTITY_BOOTSTRAP_DOMAIN":         "memql.localhost",
		"MEMQL_DISCOVERY_GRPC_ENDPOINT":           "api.memql.localhost:443",
		"MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS":     "https://api.memql.localhost,https://app.memql.localhost,https://portal.memql.localhost",
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
		`"clientId":"portal"`,
		`"clientId":"app"`,
		"https://portal.memql.localhost/auth/callback",
		"https://app.memql.localhost/auth/callback",
		"http://127.0.0.1/cockpit/callback",
	} {
		if !strings.Contains(clients, fragment) {
			t.Errorf("registered clients %q missing %q", clients, fragment)
		}
	}

	// ONE URI for the portal, not two. Registering both the origin it is
	// served from now AND the one it moved off of is the accept-either
	// pattern the no-shims rule forbids. memql#3711 is the move -- the portal
	// is site #1, served at its own origin's root -- so the pre-move
	// api.<d>/portal/ shape (from the 7ec983a1 api.<domain> rename, back when
	// the bundle was still mounted on that front door) must be gone, not
	// merely superseded.
	if strings.Contains(clients, "https://api.memql.localhost/portal/auth/callback") {
		t.Errorf("registered clients still carry the pre-memql#3711 api.<d>/portal/ "+
			"redirect URI alongside the new portal.<d> one: %q", clients)
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
		if got := DomainDerivations(bad, ""); len(got) != 0 {
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
	got := DomainDerivations("memql.localhost", "")

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

// TestDomainDerivationsUnderAHostLabel is the environment axis (memql#3767).
//
// The label lands in TWO positions and they are not interchangeable: a ROLE
// host hyphenates so the cluster's existing `*.<domain>` wildcard certificate
// still covers it, and a SITE host nests because its own name already occupies
// that label. Getting either backwards is not a startup failure -- identity
// issues tokens naming an issuer nothing is served at, every verifier rejects
// them, and the symptom is "sign-in is broken" with both manifests looking
// correct.
func TestDomainDerivationsUnderAHostLabel(t *testing.T) {
	got := DomainDerivations("memql.localhost", "e2")

	want := map[string]string{
		// Roles hyphenate -- single label, covered by *.memql.localhost.
		"MEMQL_IDENTITY_BASE_URL":                 "https://identity-e2.memql.localhost",
		"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER": "https://identity-e2.memql.localhost",
		"MEMQL_DISCOVERY_GRPC_ENDPOINT":           "api-e2.memql.localhost:443",
		"MEMQL_MCP_PUBLIC_URL":                    "https://mcp-e2.memql.localhost",
		// Sites nest -- app. and portal. are site rows, not roles.
		"MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS": "https://api-e2.memql.localhost," +
			"https://app.e2.memql.localhost,https://portal.e2.memql.localhost",
		// The bootstrap domain is the CLUSTER's domain and carries no label:
		// it is the domain, not a host derived from it.
		"MEMQL_IDENTITY_BOOTSTRAP_DOMAIN": "memql.localhost",
	}
	for name, wantVal := range want {
		if got[name] != wantVal {
			t.Errorf("%s = %q, want %q", name, got[name], wantVal)
		}
	}

	if clients := got["MEMQL_IDENTITY_REGISTERED_CLIENTS"]; !strings.Contains(
		clients, "https://portal.e2.memql.localhost/auth/callback") {
		t.Errorf("the portal's redirect URI does not follow the label: %s", clients)
	}
}

// An unset label must produce byte-for-byte what the unprefixed environment
// produced before the label existed. That is every single-environment cluster,
// every local one, and production.
func TestDomainDerivationsWithoutALabelAreUnchanged(t *testing.T) {
	for name, v := range DomainDerivations("memql.localhost", "") {
		if strings.Contains(v, "-.") || strings.Contains(v, "..") {
			t.Errorf("%s = %q -- an empty label leaked a separator into the host", name, v)
		}
	}
	// Whitespace around a ConfigMap value is invisible in every UI that shows
	// it, and would otherwise be concatenated straight into a hostname.
	a := DomainDerivations("memql.localhost", "")
	b := DomainDerivations("memql.localhost", "  ")
	for name := range a {
		if a[name] != b[name] {
			t.Errorf("a whitespace-only label changed %s: %q vs %q", name, a[name], b[name])
		}
	}
}

// A label that cannot be concatenated safely derives NOTHING, for the same
// reason a malformed domain does: half a derivation is a cluster that boots and
// cannot be signed into. A dotted label is the dangerous member -- it silently
// makes a role host two labels, which the wildcard certificate does not cover.
func TestDomainDerivationsRejectsAMalformedLabel(t *testing.T) {
	for _, bad := range []string{"a.b", "Staging", "-lead", "trail-", "with space", "*"} {
		if got := DomainDerivations("memql.localhost", bad); len(got) != 0 {
			t.Errorf("DomainDerivations(_, %q) = %v, want empty", bad, got)
		}
	}
}

// The label reaches the process the same way the search path does: an env entry
// off the memql-environment ConfigMap.
func TestApplyDomainDerivationsReadsTheHostLabel(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "memql.localhost")
	t.Setenv("MEMQL_HOST_LABEL", "e2")
	t.Setenv("MEMQL_IDENTITY_BASE_URL", "")

	ApplyDomainDerivations(nil)

	if got, want := os.Getenv("MEMQL_IDENTITY_BASE_URL"), "https://identity-e2.memql.localhost"; got != want {
		t.Errorf("BASE_URL = %q, want %q", got, want)
	}
}
